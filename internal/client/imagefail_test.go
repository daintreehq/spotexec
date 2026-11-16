package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

func TestImageMemoRemembersPerRepoAndTagUntilItAgesOut(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	m := imageMemo{dir: dir, repo: "r1", now: func() time.Time { return now }}
	m.remember(imageFailure{Tag: "t1", Message: "docker build Dockerfile: exit 1", Who: "aws#1", At: now})
	if f := m.recall("t1"); f == nil || f.Message != "docker build Dockerfile: exit 1" || f.Who != "aws#1" {
		t.Fatalf("recall %+v", f)
	}
	if f := m.recall("t2"); f != nil {
		t.Fatalf("another tag: %+v", f)
	}
	if f := (imageMemo{dir: dir, repo: "r2", now: m.now}).recall("t1"); f != nil {
		t.Fatalf("another repository: %+v", f)
	}
	now = now.Add(imageFailAge - time.Second)
	if m.recall("t1") == nil {
		t.Fatal("forgotten before it aged out")
	}
	now = now.Add(time.Second)
	if f := m.recall("t1"); f != nil {
		t.Fatalf("aged out but recalled: %+v", f)
	}
	now = now.Add(-time.Minute)
	m.forget("t1")
	if f := m.recall("t1"); f != nil {
		t.Fatalf("forgotten but recalled: %+v", f)
	}
	// Recording another failure drops the ones that aged out.
	m.remember(imageFailure{Tag: "t3", At: now.Add(-imageFailAge)})
	m.remember(imageFailure{Tag: "t4", At: now})
	if ents, _ := os.ReadDir(dir); len(ents) != 1 || m.recall("t4") == nil {
		t.Fatalf("left %v", ents)
	}
	if (imageMemo{}).recall("t1") != nil {
		t.Fatal("an unset memo recalled a failure")
	}
}

// imageScript answers image requests: a probe with probe, a build with
// build (nil: the helper fails the build itself).
type imageScript struct {
	probe protocol.ImageResult
	build *protocol.ImageResult
	fail  protocol.ErrorMsg
}

func (s imageScript) reply(c *protocol.Conn, typ string, msg json.RawMessage) {
	var m protocol.Image
	_ = json.Unmarshal(msg, &m)
	switch {
	case typ == protocol.TypeImage && m.Probe:
		_ = c.Send(protocol.TypeImageResult, s.probe)
	case typ == protocol.TypeImage && s.build != nil:
		_ = c.Send(protocol.TypeImageResult, *s.build)
	case typ == protocol.TypeImage:
		_ = c.WriteFrame(protocol.KindStderr, []byte("#1 resolving\n"))
		_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "image_build", Message: m.Dockerfile})
		_ = c.WriteFrame(protocol.KindStderr, []byte("#5 RUN apt-get install nope\nE: Unable to locate package nope\n"))
		_ = c.Send(protocol.TypeError, s.fail)
	case typ == protocol.TypeShareWait:
		_ = c.Send(protocol.TypeShareDone, protocol.ShareDone{OK: true})
	}
}

var buildFails = imageScript{probe: protocol.ImageResult{Missing: true},
	fail: protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "docker build .spotexec/Dockerfile: exit 100", BuildFailed: true}}

var builds = imageScript{probe: protocol.ImageResult{Missing: true},
	build: &protocol.ImageResult{Image: "spotexec-local/r:abc", Built: true}}

func imageRun(ctx context.Context, t *testing.T, s imageScript, o Options, req protocol.Image) (*scriptRPC, protocol.ImageResult, error) {
	t.Helper()
	c := newScript(t, s.reply)
	log := newDepsLog(t.TempDir(), "aws#1")
	defer log.Close()
	if o.Rep == nil {
		o.Rep = &recRep{}
	}
	r, _, _, err := fetchImage(ctx, c, o, "aws#1", io.Discard, log, newImageMemo("repo"), req, time.Minute)
	return c, r, err
}

// #194: a build that fails is remembered with its output, and the next run
// needing the same image fails at once with that error instead of
// building it again.
func TestFetchImageRemembersAFailedBuild(t *testing.T) {
	ctx := fastShare(t)
	c, _, err := imageRun(ctx, t, buildFails, Options{}, sharedImage())
	var be *ImageBuildError
	if !errors.As(err, &be) || be.Cached || be.Who != "aws#1" || !strings.Contains(be.Tail, "Unable to locate package nope") || strings.Contains(be.Tail, "resolving") {
		t.Fatalf("first build: %#v", err)
	}
	if b, rerr := os.ReadFile(be.Log); rerr != nil || !strings.Contains(string(b), "Unable to locate package nope") {
		t.Fatalf("log %q: %q, %v", be.Log, b, rerr)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe image]" {
		t.Fatalf("first run sent %s", got)
	}

	for _, req := range []protocol.Image{sharedImage(), withRegistry(sharedImage(), "2.dkr.ecr.us-east-1.amazonaws.com/spotexec/r"), withRegistry(sharedImage(), "")} {
		c, _, err := imageRun(ctx, t, builds, Options{}, req)
		var cached *ImageBuildError
		if !errors.As(err, &cached) || !cached.Cached || cached.Tail != be.Tail || cached.Log != be.Log {
			t.Fatalf("registry %v: %#v", req.Registry, err)
		}
		var re *transport.RemoteError
		if !errors.As(err, &re) || re.Code != protocol.CodeImageFailed || !strings.Contains(re.Message, "exit 100") || !strings.Contains(re.Message, "not building it again") {
			t.Fatalf("registry %v: %v", req.Registry, err)
		}
		if got := fmt.Sprint(c.sent); got != "[image:probe]" {
			t.Fatalf("registry %v: sent %s", req.Registry, got)
		}
		var b bytes.Buffer
		for _, l := range cached.Detail() {
			b.WriteString(l + "\n")
		}
		if d := b.String(); !strings.Contains(d, "E: Unable to locate package nope") || !strings.Contains(d, "full log: "+be.Log) || !strings.Contains(d, "spotexec container build") {
			t.Fatalf("detail %s", d)
		}
	}
}

func withRegistry(req protocol.Image, repo string) protocol.Image {
	req.Registry = nil
	if repo != "" {
		req.Registry = &protocol.RegistryAuth{Repo: repo}
	}
	return req
}

// A failure the helper doesn't mark as the build's own (a push, a login,
// an older helper) is not remembered: the next run builds.
func TestFetchImageDoesNotRememberAnUnmarkedFailure(t *testing.T) {
	ctx := fastShare(t)
	s := buildFails
	s.fail.BuildFailed = false
	_, _, err := imageRun(ctx, t, s, Options{}, sharedImage())
	var be *ImageBuildError
	if err == nil || errors.As(err, &be) {
		t.Fatalf("got %#v", err)
	}
	c, r, err := imageRun(ctx, t, builds, Options{}, sharedImage())
	if err != nil || !r.Built || fmt.Sprint(c.sent) != "[image:probe image]" {
		t.Fatalf("r %+v, sent %v, %v", r, c.sent, err)
	}
}

// The image had after all (from the registry, already on the runner, or
// built by `container build`) forgets the failure.
func TestFetchImageForgetsTheFailureOnceTheImageIsHad(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    imageScript
		o    Options
		req  protocol.Image
		sent string
	}{
		{"pulled from the registry", imageScript{probe: protocol.ImageResult{Image: "spotexec-local/r:abc", Pulled: true}}, Options{}, sharedImage(), "[image:probe]"},
		{"on the runner", imageScript{probe: protocol.ImageResult{Image: "spotexec-local/r:abc"}}, Options{}, withRegistry(sharedImage(), ""), "[image:probe]"},
		{"container build", builds, Options{WaitPush: true}, sharedImage(), "[image:probe image]"},
		{"container build without a registry", builds, Options{WaitPush: true}, withRegistry(sharedImage(), ""), "[image]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := fastShare(t)
			if _, _, err := imageRun(ctx, t, buildFails, Options{}, sharedImage()); err == nil {
				t.Fatal("the build didn't fail")
			}
			c, r, err := imageRun(ctx, t, tc.s, tc.o, tc.req)
			if err != nil || r.Image == "" || fmt.Sprint(c.sent) != tc.sent {
				t.Fatalf("r %+v, sent %v, %v", r, c.sent, err)
			}
			if f := newImageMemo("repo").recall(sharedImage().Tag); f != nil {
				t.Fatalf("still remembered: %+v", f)
			}
		})
	}
}

// `container build` that builds the image but can't push it forgets the
// failure: the build works now. One cancelled, or failing before it has the
// image, leaves it remembered.
func TestFetchImageForcedBuildForgetsTheFailureOnlyOnceItHasTheImage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		push      bool // the helper got as far as pushing
		cancel    bool
		forgotten bool
	}{
		{"built, push failed", true, false, true},
		{"on the runner, push failed", true, false, true},
		{"cancelled", false, true, false},
		{"failed before building", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := fastShare(t)
			if _, _, err := imageRun(ctx, t, buildFails, Options{}, sharedImage()); err == nil {
				t.Fatal("the build didn't fail")
			}
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
				var m protocol.Image
				_ = json.Unmarshal(msg, &m)
				onRunner := strings.HasPrefix(tc.name, "on the runner")
				if m.Probe && !onRunner {
					_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Missing: true})
					return
				}
				if tc.push {
					_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "image_build"})
					_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "image_push"})
				}
				if tc.cancel {
					cancel()
				}
				_ = c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "registry login: denied"})
			})
			_, _, _, err := fetchImage(cctx, c, Options{Rep: &recRep{}, WaitPush: true}, "aws#1", io.Discard, nil, newImageMemo("repo"), sharedImage(), time.Minute)
			want := "[image:probe image]"
			if strings.HasPrefix(tc.name, "on the runner") {
				want = "[image:probe]" // the probe finds it and pushes it
			}
			if err == nil || fmt.Sprint(c.sent) != want {
				t.Fatalf("sent %v, %v", c.sent, err)
			}
			if f := newImageMemo("repo").recall(sharedImage().Tag); (f == nil) != tc.forgotten {
				t.Fatalf("remembered %+v, want forgotten %v", f, tc.forgotten)
			}
		})
	}
}

// A run that finds the image missing and its build failed doesn't wait
// behind another run building it: it fails at once.
func TestFetchImageRememberedFailureDoesNotWait(t *testing.T) {
	ctx := fastShare(t)
	req := sharedImage()
	newImageMemo("repo").remember(imageFailure{Tag: req.Tag, Message: "docker build .spotexec/Dockerfile: exit 1", Who: "aws#2", At: time.Now()})
	lock := shareLockFor("image", req.Registry.Repo+":"+req.Tag)
	held, err := lock.take(ctx, lock.verdict(), "test 2/8 on aws#4", time.Minute, nil)
	if err != nil || !held.lead {
		t.Fatalf("take %+v, %v", held, err)
	}
	defer held.release("")
	rep := &recRep{}
	c, _, err := imageRun(ctx, t, builds, Options{Rep: rep}, req)
	var be *ImageBuildError
	if !errors.As(err, &be) || !be.Cached || fmt.Sprint(c.sent) != "[image:probe]" || rep.event("image_wait") != nil {
		t.Fatalf("sent %v, waited %v, %#v", c.sent, rep.event("image_wait"), err)
	}
}

// Runs waiting on a build that fails don't build it again: they fail with
// its error as soon as it is released (#194). A leader lost with its runner
// leaves nothing remembered, so the next waiting run builds (#162).
func TestFetchImageWaitersOfAFailedBuildFailAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed bool
	}{{"failed", true}, {"lost", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := fastShare(t)
			req := sharedImage()
			lock := shareLockFor("image", req.Registry.Repo+":"+req.Tag)
			held, err := lock.take(ctx, lock.verdict(), "test 2/8 on aws#4", time.Minute, nil)
			if err != nil || !held.lead {
				t.Fatalf("take %+v, %v", held, err)
			}
			rep := &recRep{}
			go func() {
				for rep.event("image_wait") == nil {
					time.Sleep(time.Millisecond)
				}
				if !tc.failed {
					held.release("")
					return
				}
				newImageMemo("repo").remember(imageFailure{Tag: req.Tag, Message: "docker build .spotexec/Dockerfile: exit 1", Who: "test 2/8 on aws#4", At: time.Now()})
				held.release(shareDone)
			}()
			c, r, err := imageRun(ctx, t, builds, Options{Rep: rep}, req)
			var be *ImageBuildError
			if tc.failed {
				if !errors.As(err, &be) || !be.Cached || be.Who != "test 2/8 on aws#4" || fmt.Sprint(c.sent) != "[image:probe]" {
					t.Fatalf("sent %v, %#v", c.sent, err)
				}
				return
			}
			if err != nil || !r.Built || fmt.Sprint(c.sent) != "[image:probe image]" {
				t.Fatalf("r %+v, sent %v, %v", r, c.sent, err)
			}
		})
	}
}

// A build cut short by this run being cancelled is not remembered.
func TestFetchImageCancelledBuildIsNotRemembered(t *testing.T) {
	ctx := fastShare(t)
	cctx, cancel := context.WithCancel(ctx)
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var m protocol.Image
		_ = json.Unmarshal(msg, &m)
		if !m.Probe {
			cancel()
		}
		buildFails.reply(c, typ, msg)
	})
	_, _, _, err := fetchImage(cctx, c, Options{Rep: &recRep{}}, "aws#1", io.Discard, nil, newImageMemo("repo"), withRegistry(sharedImage(), ""), time.Minute)
	var be *ImageBuildError
	if err == nil || errors.As(err, &be) {
		t.Fatalf("got %#v", err)
	}
	if f := newImageMemo("repo").recall(sharedImage().Tag); f != nil {
		t.Fatalf("remembered %+v", f)
	}
}
