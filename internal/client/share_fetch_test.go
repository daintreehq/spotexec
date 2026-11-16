package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// scriptRPC is a helper session answering from a script: reply gets each
// request and sends its answer on c.
type scriptRPC struct {
	t     *testing.T
	reply func(c *protocol.Conn, typ string, msg json.RawMessage)
	sent  []string // "type" or "type:probe" / "type:kind"
	buf   bytes.Buffer
	w, r  *protocol.Conn
}

func newScript(t *testing.T, reply func(c *protocol.Conn, typ string, msg json.RawMessage)) *scriptRPC {
	s := &scriptRPC{t: t, reply: reply}
	s.w, s.r = protocol.NewConn(nil, &s.buf), protocol.NewConn(&s.buf, nil)
	return s
}

func (s *scriptRPC) send(typ string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var probe struct {
		Probe     bool   `json:"probe"`
		Kind      string `json:"kind"`
		TimeoutMS int64  `json:"timeout_ms"`
	}
	_ = json.Unmarshal(b, &probe)
	what := typ
	switch {
	case probe.Probe:
		what += ":probe"
	case typ == protocol.TypeShareWait:
		what += ":" + probe.Kind
		if probe.TimeoutMS != shareTransfer.Milliseconds() {
			s.t.Errorf("share_wait timeout %dms", probe.TimeoutMS)
		}
	}
	s.sent = append(s.sent, what)
	s.reply(s.w, typ, b)
	return nil
}

func (s *scriptRPC) next(context.Context) (protocol.Frame, error) {
	f, err := s.r.ReadFrame()
	if err == io.EOF {
		return f, fmt.Errorf("script: no reply")
	}
	return f, err
}

func (s *scriptRPC) event(protocol.Event)  {}
func (s *scriptRPC) lost() <-chan struct{} { return nil }
func (s *scriptRPC) lostErr() error        { return nil }

type recRep struct {
	nopRep
	mu     sync.Mutex
	events []string
	fields []map[string]any
	warns  []string
}

func (r *recRep) Event(name string, f map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, name)
	r.fields = append(r.fields, f)
}

func (r *recRep) Warn(code, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warns = append(r.warns, code+": "+fmt.Sprintf(format, args...))
}

func (r *recRep) event(name string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, n := range r.events {
		if n == name {
			return r.fields[i]
		}
	}
	return nil
}

func sharedDeps() protocol.Deps {
	return protocol.Deps{Lockfiles: []string{"package-lock.json"}, Install: "npm ci", CacheDirs: []string{"node_modules"},
		CacheKey: "k1", CacheGet: "https://b.s3.eu-north-1.amazonaws.com/r/k1.tar?sig=get", CachePut: "https://b.s3.eu-north-1.amazonaws.com/r/k1.tar?sig=put"}
}

func depsFetch(ctx context.Context, t *testing.T, c rpc, rep *recRep, req protocol.Deps) (protocol.DepsResult, shareOutcome, *protocol.ShareDone, error) {
	t.Helper()
	return depsFetchSigned(ctx, t, c, rep, req, nil)
}

func depsFetchSigned(ctx context.Context, t *testing.T, c rpc, rep *recRep, req protocol.Deps, resign func(context.Context) (string, string, error)) (protocol.DepsResult, shareOutcome, *protocol.ShareDone, error) {
	t.Helper()
	log := newDepsLog(t.TempDir(), "aws#1")
	defer log.Close()
	return fetchDeps(ctx, c, func(protocol.Event) {}, Options{Rep: rep}, "aws#1", log, req, resign, time.Minute)
}

// URLs signed before a long wait are signed afresh for the request that
// restores (or installs and uploads); the lock stays the same object's.
func TestFetchDepsResignsStaleURLs(t *testing.T) {
	ctx := fastShare(t)
	old := resignAfter
	resignAfter = 0
	t.Cleanup(func() { resignAfter = old })
	var gets []string
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var d protocol.Deps
		_ = json.Unmarshal(msg, &d)
		gets = append(gets, d.CacheGet)
		if d.Probe {
			sendDeps(c, protocol.DepsResult{Missing: true})
			return
		}
		sendDeps(c, protocol.DepsResult{Built: true})
	})
	fresh := "https://b.s3.eu-north-1.amazonaws.com/r/k1.tar?sig=fresh"
	_, _, _, err := depsFetchSigned(ctx, t, c, &recRep{}, sharedDeps(), func(context.Context) (string, string, error) {
		return fresh, fresh + "-put", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gets) != 2 || gets[0] != sharedDeps().CacheGet || gets[1] != fresh {
		t.Fatalf("cache_get sent %q", gets)
	}
}

// The helper giving up on this client while it waits for the upload is a
// lost session: an error, and no verdict, so a waiting run takes over.
func TestFetchDepsSessionErrorDuringUpload(t *testing.T) {
	ctx := fastShare(t)
	req := sharedDeps()
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var d protocol.Deps
		_ = json.Unmarshal(msg, &d)
		switch {
		case typ == protocol.TypeDeps && d.Probe:
			sendDeps(c, protocol.DepsResult{Missing: true})
		case typ == protocol.TypeDeps:
			sendDeps(c, protocol.DepsResult{Built: true, Uploaded: true})
		default:
			_ = c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeInternal, Message: "client went away while waiting for the deps transfer"})
		}
	})
	if _, _, _, err := depsFetch(ctx, t, c, &recRep{}, req); err == nil {
		t.Fatal("want the session's error")
	}
	if v := shareLockFor("deps", stripQuery(req.CacheGet)).verdict(); v != "" {
		t.Fatalf("verdict %q left", v)
	}
}

func sendDeps(c *protocol.Conn, r protocol.DepsResult) { _ = c.Send(protocol.TypeDepsResult, r) }

// A cold key: probe (restore only), miss, install while holding the lock,
// then wait for the upload so other runs can restore it.
func TestFetchDepsColdInstallsAndWaitsForItsUpload(t *testing.T) {
	ctx := fastShare(t)
	rep := &recRep{}
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var d protocol.Deps
		_ = json.Unmarshal(msg, &d)
		switch {
		case typ == protocol.TypeDeps && d.Probe:
			sendDeps(c, protocol.DepsResult{Key: "k1", Missing: true})
		case typ == protocol.TypeDeps:
			sendDeps(c, protocol.DepsResult{Key: "k1", Built: true, Uploaded: true, Hydrated: false})
		case typ == protocol.TypeShareWait:
			_ = c.Send(protocol.TypeShareDone, protocol.ShareDone{OK: true, MS: 1234})
		}
	})
	dr, shared, upload, err := depsFetch(ctx, t, c, rep, sharedDeps())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(c.sent); got != "[deps:probe deps share_wait:deps]" {
		t.Fatalf("sent %s", got)
	}
	if !dr.Built || !shared.led || shared.waited != 0 || upload == nil || !upload.OK || upload.MS != 1234 {
		t.Fatalf("dr %+v, shared %+v, upload %+v", dr, shared, upload)
	}
	if len(rep.warns) != 0 || rep.event("deps_wait") != nil {
		t.Fatalf("warns %q, events %q", rep.warns, rep.events)
	}
}

// A failed upload is a warning, not a failure of this run.
func TestFetchDepsUploadFailureWarns(t *testing.T) {
	ctx := fastShare(t)
	rep := &recRep{}
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var d protocol.Deps
		_ = json.Unmarshal(msg, &d)
		switch {
		case typ == protocol.TypeDeps && d.Probe:
			sendDeps(c, protocol.DepsResult{Missing: true})
		case typ == protocol.TypeDeps:
			sendDeps(c, protocol.DepsResult{Built: true, Uploaded: true})
		case typ == protocol.TypeShareWait:
			_ = c.Send(protocol.TypeShareDone, protocol.ShareDone{Message: "exit status 1"})
		}
	})
	_, _, upload, err := depsFetch(ctx, t, c, rep, sharedDeps())
	if err != nil || upload == nil || upload.OK {
		t.Fatalf("upload %+v, %v", upload, err)
	}
	if len(rep.warns) != 1 || rep.warns[0] != "DEPS_CACHE: uploading the dependency cache failed (exit status 1): runs waiting for it install the dependencies themselves" {
		t.Fatalf("warns %q", rep.warns)
	}
}

// A hit needs only the probe; without a shared cache there is no probe.
func TestFetchDepsHitAndUnsharedSendOneRequest(t *testing.T) {
	ctx := fastShare(t)
	for name, req := range map[string]protocol.Deps{"hit": sharedDeps(), "no shared cache": {Install: "npm ci"}} {
		t.Run(name, func(t *testing.T) {
			c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
				sendDeps(c, protocol.DepsResult{Restored: true, Hydrated: true})
			})
			dr, shared, upload, err := depsFetch(ctx, t, c, &recRep{}, req)
			if err != nil || !dr.Restored || shared.led || upload != nil {
				t.Fatalf("dr %+v, shared %+v, upload %+v, %v", dr, shared, upload, err)
			}
			want := "[deps:probe]"
			if name != "hit" {
				want = "[deps]"
			}
			if got := fmt.Sprint(c.sent); got != want {
				t.Fatalf("sent %s, want %s", got, want)
			}
		})
	}
}

// A run that misses while another installs the same key says so, names
// it, waits, then restores what it uploaded.
func TestFetchDepsWaitsForTheInstallingRun(t *testing.T) {
	ctx := fastShare(t)
	req := sharedDeps()
	lock := shareLockFor("deps", stripQuery(req.CacheGet))
	held, err := lock.take(ctx, lock.verdict(), "test 2/8 on aws#4", time.Minute, nil)
	if err != nil || !held.lead {
		t.Fatalf("take %+v, %v", held, err)
	}
	rep := &recRep{}
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var d protocol.Deps
		_ = json.Unmarshal(msg, &d)
		if d.Probe {
			sendDeps(c, protocol.DepsResult{Missing: true})
			return
		}
		sendDeps(c, protocol.DepsResult{Restored: true, Hydrated: true})
	})
	go func() {
		for rep.event("deps_wait") == nil {
			time.Sleep(time.Millisecond)
		}
		held.release(shareDone)
	}()
	dr, shared, upload, err := depsFetch(ctx, t, c, rep, req)
	if err != nil || !dr.Restored || shared.led || shared.waited <= 0 || upload != nil {
		t.Fatalf("dr %+v, shared %+v, upload %+v, %v", dr, shared, upload, err)
	}
	if got := fmt.Sprint(c.sent); got != "[deps:probe deps]" {
		t.Fatalf("sent %s", got)
	}
	ev := rep.event("deps_wait")
	if ev["holder"] != "another part of this run (test 2/8 on aws#4)" || ev["key"] != "k1" || ev["message"] == "" {
		t.Fatalf("deps_wait %v", ev)
	}
}

func sharedImage() protocol.Image {
	return protocol.Image{Dockerfile: ".spotexec/Dockerfile", Tag: "abcdef0123456789",
		Registry: &protocol.RegistryAuth{Repo: "1.dkr.ecr.eu-north-1.amazonaws.com/spotexec/r"}}
}

// A missing image: one run builds it (the probe having installed Docker,
// which the result keeps) and waits for its push.
func TestFetchImageColdBuildsAndWaitsForItsPush(t *testing.T) {
	ctx := fastShare(t)
	rep := &recRep{}
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var m protocol.Image
		_ = json.Unmarshal(msg, &m)
		switch {
		case typ == protocol.TypeImage && m.Probe:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Missing: true, InstalledDocker: true})
		case typ == protocol.TypeImage:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "spotexec-local/r:abc", ImageID: "sha256:1", Built: true, Pushing: true})
		case typ == protocol.TypeShareWait:
			_ = c.Send(protocol.TypeShareDone, protocol.ShareDone{OK: true, MS: 99})
		}
	})
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, sharedImage(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe image share_wait:image]" {
		t.Fatalf("sent %s", got)
	}
	if !r.Built || !r.InstalledDocker || !shared.led || push == nil || !push.OK || push.MS != 99 {
		t.Fatalf("r %+v, shared %+v, push %+v", r, shared, push)
	}
}

// image = (a public reference) or no registry: one plain request.
func TestFetchImageWithoutRegistrySendsOneRequest(t *testing.T) {
	ctx := fastShare(t)
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "node:22", Pulled: true})
	})
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: &recRep{}}, "aws#1", io.Discard, nil, imageMemo{}, protocol.Image{Ref: "node:22"}, time.Minute)
	if err != nil || r.Image != "node:22" || shared.led || push != nil {
		t.Fatalf("r %+v, shared %+v, push %+v, %v", r, shared, push, err)
	}
	if got := fmt.Sprint(c.sent); got != "[image]" {
		t.Fatalf("sent %s", got)
	}
}
