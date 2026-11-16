package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// regionsProvider is a multi-region runner whose instance's region answers
// registry lookups with registry, and whose other regions hold the image
// at source (nil: none).
type regionsProvider struct {
	provider.Provider
	registry func(ctx context.Context) (*protocol.RegistryAuth, error)
	source   *protocol.RegistryAuth
	looked   int
}

func (p *regionsProvider) RegistryFor(ctx context.Context, repoID string) (*protocol.RegistryAuth, error) {
	return p.registry(ctx)
}

func (p *regionsProvider) ImageSource(ctx context.Context, instanceID, repoID, tag string) (*protocol.RegistryAuth, error) {
	p.looked++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if instanceID != "i-1" || repoID != "repo" || tag != "abcdef0123456789" {
		return nil, fmt.Errorf("looked up %s %s %s", instanceID, repoID, tag)
	}
	return p.source, nil
}

// runner is how a runner without a registry of its own answers image
// requests: from the image it has, else by copying from Source when
// inSource (only once probing is over, when lateCopy: the first copy
// failed), else (not probing) by building it, which fails when buildFails.
type runner struct {
	onRunner, inSource, lateCopy, buildFails bool
}

// localScript answers image requests as rn says. The source each request
// named is recorded.
func localScript(t *testing.T, rn runner, sources *[]string) *scriptRPC {
	return newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		var m protocol.Image
		_ = json.Unmarshal(msg, &m)
		if m.Registry != nil {
			t.Errorf("request with a registry %+v", m.Registry)
		}
		src := ""
		if m.Source != nil {
			src = m.Source.Repo
		}
		*sources = append(*sources, src)
		switch {
		case rn.onRunner:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "spotexec-local/r:abc", ImageID: "sha256:0"})
		case m.Source != nil && rn.inSource && !(rn.lateCopy && m.Probe):
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "spotexec-local/r:abc", ImageID: "sha256:1", Pulled: true, Copied: true})
		case m.Probe:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Missing: true, InstalledDocker: len(*sources) == 1})
		case rn.buildFails:
			_ = c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "docker build .spotexec/Dockerfile: exit 100", BuildFailed: true})
		default:
			_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Image: "spotexec-local/r:abc", ImageID: "sha256:2", Built: true})
		}
	})
}

func localImage() protocol.Image {
	req := sharedImage()
	req.Registry = nil
	return req
}

// #218: a VM whose region's registry lookup timed out still copies the
// image from another of the runner's regions, rather than building it.
func TestImageRegistriesLookupTimesOut(t *testing.T) {
	ctx := fastShare(t)
	old := registryTimeout
	registryTimeout = 200 * time.Millisecond
	t.Cleanup(func() { registryTimeout = old })
	src := &protocol.RegistryAuth{Server: "1.dkr.ecr.us-east-2.amazonaws.com", Repo: "1.dkr.ecr.us-east-2.amazonaws.com/spotexec/r"}
	p := &regionsProvider{source: src, registry: func(ctx context.Context) (*protocol.RegistryAuth, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	rep := &recRep{}
	reg, locate := imageRegistries(ctx, Options{Rep: rep}, p, "i-1", "repo", "abcdef0123456789")
	if reg != nil || locate == nil {
		t.Fatalf("registry %+v, locate %v", reg, locate != nil)
	}
	if len(rep.warns) != 1 || !strings.Contains(rep.warns[0], "IMAGE_REGISTRY") || !strings.Contains(rep.warns[0], "another of the runner's regions") ||
		!strings.Contains(rep.warns[0], context.DeadlineExceeded.Error()) {
		t.Fatalf("warns %v", rep.warns)
	}
	var sources []string
	c := localScript(t, runner{inSource: true}, &sources)
	req := localImage()
	req.Registry = reg
	r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, req, locate, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Copied || r.Built || !r.InstalledDocker || r.ImageID != "sha256:1" || shared.led || push != nil || p.looked != 1 {
		t.Fatalf("r %+v, shared %+v, push %+v, looked %d", r, shared, push, p.looked)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe image:probe]" {
		t.Fatalf("sent %s", got)
	}
	if got := fmt.Sprint(sources); got != "[ "+src.Repo+"]" {
		t.Fatalf("sources %s", got)
	}
}

// The region's registry, when it answers, is used as before; a runner in
// one region gets no lookup of others, and building is all that is left
// when its registry fails.
func TestImageRegistries(t *testing.T) {
	ctx := context.Background()
	home := &protocol.RegistryAuth{Server: "1.dkr.ecr.ap-south-1.amazonaws.com", Repo: "1.dkr.ecr.ap-south-1.amazonaws.com/spotexec/r"}
	p := &regionsProvider{registry: func(context.Context) (*protocol.RegistryAuth, error) { return home, nil }}
	rep := &recRep{}
	if reg, locate := imageRegistries(ctx, Options{Rep: rep}, p, "i-1", "repo", "abcdef0123456789"); reg != home || locate == nil || len(rep.warns) != 0 {
		t.Fatalf("registry %+v, locate %v, warns %v", reg, locate != nil, rep.warns)
	}
	one := &oneRegion{registry: func(context.Context) (*protocol.RegistryAuth, error) { return nil, errors.New("denied") }}
	reg, locate := imageRegistries(ctx, Options{Rep: rep}, one, "i-1", "repo", "abcdef0123456789")
	if reg != nil || locate != nil || len(rep.warns) != 1 || !strings.Contains(rep.warns[0], "building on this runner only") {
		t.Fatalf("registry %+v, locate %v, warns %v", reg, locate != nil, rep.warns)
	}
}

// oneRegion is a single-region runner: a registry, no other regions.
type oneRegion struct {
	provider.Provider
	registry func(ctx context.Context) (*protocol.RegistryAuth, error)
}

func (p *oneRegion) RegistryFor(ctx context.Context, repoID string) (*protocol.RegistryAuth, error) {
	return p.registry(ctx)
}

// #218: without a registry of its own, a run uses the image on the runner,
// else copies it from another region holding it, else builds it; a
// remembered build failure rules out only the build.
func TestFetchImageWithoutRegistryCopiesFromAnotherRegion(t *testing.T) {
	src := &protocol.RegistryAuth{Server: "1.dkr.ecr.us-east-1.amazonaws.com", Repo: "1.dkr.ecr.us-east-1.amazonaws.com/spotexec/r"}
	for _, tc := range []struct {
		name       string
		onRunner   bool
		lateCopy   bool // the source probe's copy fails, the request's works
		found      *protocol.RegistryAuth
		lookupErr  error
		failed     bool // a build failure is remembered
		sent       string
		sources    string
		looked     int
		warns      int
		copied     bool
		built      bool
		cachedFail bool
	}{
		{name: "on the runner already", onRunner: true, sent: "[image:probe]", sources: "[]"},
		{name: "another region holds it", found: src, sent: "[image:probe image:probe]", sources: "[ " + src.Repo + "]", looked: 1, copied: true},
		{name: "no region holds it", sent: "[image:probe image]", sources: "[ ]", looked: 1, built: true},
		{name: "the lookup fails", lookupErr: errors.New("timed out"), sent: "[image:probe image]", sources: "[ ]", looked: 1, warns: 1, built: true},
		{name: "copied despite a remembered failure", found: src, failed: true, sent: "[image:probe image:probe]", sources: "[ " + src.Repo + "]", looked: 1, copied: true},
		{name: "a remembered failure with no region holding it", failed: true, sent: "[image:probe]", sources: "[]", looked: 1, cachedFail: true},
		// The request keeps the source: copying is tried again before
		// building.
		{name: "a copy that failed once is tried again", found: src, lateCopy: true, sent: "[image:probe image:probe image]", sources: "[ " + src.Repo + " " + src.Repo + "]", looked: 1, copied: true},
		{name: "a copy failing again with a failure remembered", found: src, lateCopy: true, failed: true, sent: "[image:probe image:probe]", sources: "[ " + src.Repo + "]", looked: 1, cachedFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := fastShare(t)
			rep := &recRep{}
			var sources []string
			rn := runner{onRunner: tc.onRunner, inSource: tc.found != nil, lateCopy: tc.lateCopy}
			if tc.lateCopy && tc.failed {
				rn.inSource = false // the copy fails every time
			}
			c := localScript(t, rn, &sources)
			looked := 0
			locate := func(context.Context) (*protocol.RegistryAuth, error) {
				looked++
				return tc.found, tc.lookupErr
			}
			req := localImage()
			memo := imageMemo{}
			if tc.failed {
				memo = newImageMemo("repo")
				memo.remember(imageFailure{Tag: req.Tag, Message: "docker build .spotexec/Dockerfile: exit 1", Who: "aws#2", At: time.Now()})
			}
			r, shared, push, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, memo, req, locate, time.Minute)
			if got := fmt.Sprint(c.sent); got != tc.sent {
				t.Fatalf("sent %s, %v", got, err)
			}
			if got := fmt.Sprint(sources); got != tc.sources {
				t.Fatalf("sources %s", got)
			}
			if looked != tc.looked || len(rep.warns) != tc.warns || shared.led || push != nil {
				t.Fatalf("looked %d, warns %v, shared %+v, push %+v", looked, rep.warns, shared, push)
			}
			if tc.cachedFail {
				var be *ImageBuildError
				if !errors.As(err, &be) || !be.Cached || !r.InstalledDocker {
					t.Fatalf("r %+v, err %#v", r, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Copied != tc.copied || r.Built != tc.built || r.Image == "" || r.InstalledDocker == tc.onRunner {
				t.Fatalf("r %+v", r)
			}
			// An image had forgets an earlier failure.
			if tc.failed && memo.recall(req.Tag) != nil {
				t.Fatal("the failure is still remembered")
			}
		})
	}
}

// #218: a build that fails on a runner without a registry of its own is
// remembered, so the next run needing the image fails at once rather than
// building it again.
func TestFetchImageWithoutRegistryRemembersAFailedBuild(t *testing.T) {
	ctx := fastShare(t)
	memo := newImageMemo("repo")
	locate := func(context.Context) (*protocol.RegistryAuth, error) { return nil, nil }
	var sources []string
	c := localScript(t, runner{buildFails: true}, &sources)
	_, _, _, err := fetchImage(ctx, c, Options{Rep: &recRep{}}, "aws#1", io.Discard, nil, memo, localImage(), locate, time.Minute)
	var be *ImageBuildError
	if !errors.As(err, &be) || be.Cached || memo.recall(localImage().Tag) == nil {
		t.Fatalf("err %#v", err)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe image]" {
		t.Fatalf("sent %s", got)
	}
	c = localScript(t, runner{buildFails: true}, &sources)
	_, _, _, err = fetchImage(ctx, c, Options{Rep: &recRep{}}, "aws#2", io.Discard, nil, memo, localImage(), locate, time.Minute)
	if !errors.As(err, &be) || !be.Cached {
		t.Fatalf("err %#v", err)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe]" {
		t.Fatalf("sent %s", got)
	}
}
