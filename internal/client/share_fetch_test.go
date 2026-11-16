package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, sharedImage(), nil, time.Minute)
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
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: &recRep{}}, "aws#1", io.Discard, nil, imageMemo{}, protocol.Image{Ref: "node:22"}, nil, time.Minute)
	if err != nil || r.Image != "node:22" || shared.led || push != nil {
		t.Fatalf("r %+v, shared %+v, push %+v, %v", r, shared, push, err)
	}
	if got := fmt.Sprint(c.sent); got != "[image]" {
		t.Fatalf("sent %s", got)
	}
}

// regionImage is sharedImage's request in region rg of account 1.
func regionImage(rg string) protocol.Image {
	req := sharedImage()
	server := "1.dkr.ecr." + rg + ".amazonaws.com"
	req.Registry = &protocol.RegistryAuth{Server: server, Repo: server + "/spotexec/r"}
	return req
}

// regionScript answers an image request from where the image is: on the
// runner's own registry never, in Source when has says so. The source each
// request named is recorded.
func regionScript(t *testing.T, has bool, sources *[]string) *scriptRPC {
	return newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var m protocol.Image
		_ = json.Unmarshal(msg, &m)
		src := ""
		if m.Source != nil {
			src = m.Source.Repo
		}
		*sources = append(*sources, src)
		switch {
		case typ == protocol.TypeShareWait:
			_ = c.Send(protocol.TypeShareDone, protocol.ShareDone{OK: true})
		case m.Source != nil && has:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "spotexec-local/r:abc", ImageID: "sha256:1", Pulled: true, Copied: true, Pushing: true})
		case m.Probe:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Missing: true, InstalledDocker: len(*sources) == 1})
		default:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "spotexec-local/r:abc", ImageID: "sha256:2", Built: true, Pushing: true})
		}
	})
}

// #192: an image the runner's region lacks but another region holds is
// copied from there, never built; a lookup that finds none, or fails,
// leaves the build to this run as before.
func TestFetchImageCopiesFromAnotherRegion(t *testing.T) {
	src := &protocol.RegistryAuth{Server: "1.dkr.ecr.us-east-1.amazonaws.com", Repo: "1.dkr.ecr.us-east-1.amazonaws.com/spotexec/r"}
	for _, tc := range []struct {
		name          string
		found         *protocol.RegistryAuth
		err           error
		sent          string
		sources       string
		copied        bool
		looked, warns int
	}{
		{"another region holds it", src, nil, "[image:probe image:probe]", "[ " + src.Repo + "]", true, 1, 0},
		// Looked for again before building: another run may have pushed it.
		{"no region holds it", nil, nil, "[image:probe image share_wait]", "[  ]", false, 2, 0},
		{"the lookup fails", nil, fmt.Errorf("denied"), "[image:probe image share_wait]", "[  ]", false, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := fastShare(t)
			rep := &recRep{}
			var sources []string
			c := regionScript(t, true, &sources)
			looked := 0
			locate := func(context.Context) (*protocol.RegistryAuth, error) {
				looked++
				return tc.found, tc.err
			}
			r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, regionImage("ap-south-1"), locate, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(c.sent); got != strings.Replace(tc.sent, "share_wait", "share_wait:image", 1) {
				t.Fatalf("sent %s", got)
			}
			if got := fmt.Sprint(sources); got != tc.sources {
				t.Fatalf("sources %s", got)
			}
			if looked != tc.looked || r.Copied != tc.copied || r.Built == tc.copied || !r.InstalledDocker || len(rep.warns) != tc.warns {
				t.Fatalf("looked %d, r %+v, warns %v", looked, r, rep.warns)
			}
			// A run that copied it never led a build; one that built it
			// waited for its push.
			if shared.led == tc.copied || (push != nil) == tc.copied {
				t.Fatalf("shared %+v, push %+v", shared, push)
			}
		})
	}
}

// #192: a run waiting for another run on this machine that builds the same
// image in another region copies it from there once it is pushed, rather
// than building it again.
func TestFetchImageWaitsForABuildInAnotherRegion(t *testing.T) {
	ctx := fastShare(t)
	other := regionImage("us-east-1")
	lock := shareLockFor("image", imageLockID(other.Registry, other.Tag, true))
	held, err := lock.take(ctx, lock.verdict(), "test 2/8 on aws#4", time.Minute, nil)
	if err != nil || !held.lead {
		t.Fatalf("take %+v, %v", held, err)
	}
	rep := &recRep{}
	var sources []string
	c := regionScript(t, true, &sources)
	var pushed atomic.Bool
	locate := func(context.Context) (*protocol.RegistryAuth, error) {
		if !pushed.Load() {
			return nil, nil
		}
		return other.Registry, nil
	}
	go func() {
		for rep.event("image_wait") == nil {
			time.Sleep(time.Millisecond)
		}
		pushed.Store(true)
		held.release(shareDone)
	}()
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, regionImage("ap-south-1"), locate, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Copied || r.Built || shared.led || shared.waited <= 0 || push != nil {
		t.Fatalf("r %+v, shared %+v, push %+v", r, shared, push)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe image]" {
		t.Fatalf("sent %s", got)
	}
	if got := fmt.Sprint(sources); got != "[ "+other.Registry.Repo+"]" {
		t.Fatalf("sources %s", got)
	}
}

// #192: a region found holding the image only once this run leads (the
// other run's push ended between the probe and the lock, without a wait)
// is copied from, and the leader waits for the copy's push to its own
// region, as for a build.
func TestFetchImageLeaderCopiesAndWaitsForThePush(t *testing.T) {
	ctx := fastShare(t)
	src := regionImage("us-east-1").Registry
	rep := &recRep{}
	var sources []string
	c := regionScript(t, true, &sources)
	looked := 0
	locate := func(context.Context) (*protocol.RegistryAuth, error) {
		looked++
		if looked == 1 {
			return nil, nil
		}
		return src, nil
	}
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, regionImage("ap-south-1"), locate, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Copied || r.Built || !shared.led || shared.waited != 0 || push == nil || !push.OK {
		t.Fatalf("r %+v, shared %+v, push %+v", r, shared, push)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe image share_wait:image]" {
		t.Fatalf("sent %s", got)
	}
	if got := fmt.Sprint(sources); got != "[ "+src.Repo+" ]" {
		t.Fatalf("sources %s", got)
	}
}

// The image lock names the image, not the region holding it, for runs that
// can copy it from another region; for the rest, the region's repository.
func TestImageLockID(t *testing.T) {
	a, b := regionImage("us-east-1"), regionImage("ap-south-1")
	if imageLockID(a.Registry, a.Tag, true) != imageLockID(b.Registry, b.Tag, true) {
		t.Fatalf("%s != %s", imageLockID(a.Registry, a.Tag, true), imageLockID(b.Registry, b.Tag, true))
	}
	if imageLockID(a.Registry, a.Tag, false) == imageLockID(b.Registry, b.Tag, false) {
		t.Fatal("runs that can't copy across regions wait on each other")
	}
	if got := imageLockID(a.Registry, a.Tag, false); got != a.Registry.Repo+":"+a.Tag {
		t.Fatalf("single-region lock %s", got)
	}
	c := regionImage("us-east-1")
	c.Registry.Server = "2.dkr.ecr.us-east-1.amazonaws.com"
	c.Registry.Repo = c.Registry.Server + "/spotexec/r"
	if imageLockID(a.Registry, a.Tag, true) == imageLockID(c.Registry, c.Tag, true) || imageLockID(a.Registry, a.Tag, true) == imageLockID(a.Registry, "other", true) {
		t.Fatal("other accounts or tags share the lock")
	}
}
