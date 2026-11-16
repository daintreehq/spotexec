package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// #193: only the runner's IMAGE_BUILD_FAILED is the project's Dockerfile
// failing; an IMAGE_FAILED (an older runner's included) stays spotexec's.
func TestImageBuildError(t *testing.T) {
	remote := func(code, detail string) error {
		return fmt.Errorf("container image: %w", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: code, Message: "m", Detail: detail}})
	}
	broken := remote(protocol.CodeImageBuildFailed, "#5 0.2 boom\x1b[31m!\x1b[0m\nERROR: failed to solve")
	err := imageBuildError(broken)
	var be *ImageBuildError
	if !errors.As(err, &be) || err.Error() != broken.Error() {
		t.Fatalf("got %T %v", err, err)
	}
	var ie *InfraError
	if errors.As(err, &ie) {
		t.Fatal("a broken Dockerfile is spotexec's failure")
	}
	var re *transport.RemoteError
	if !errors.As(err, &re) || re.Code != protocol.CodeImageBuildFailed {
		t.Fatal("the runner's error is no longer reachable")
	}
	want := []string{"end of the build's output:", "  #5 0.2 boom!", "  ERROR: failed to solve"}
	if got := be.Detail(); !reflect.DeepEqual(got, want) {
		t.Fatalf("detail %q, want %q", got, want)
	}
	if got := (&ImageBuildError{Err: broken}).Detail(); got != nil {
		t.Fatalf("detail without output: %q", got)
	}
	for _, other := range []error{remote(protocol.CodeImageFailed, ""), errors.New("ssh: lost"), nil} {
		if got := imageBuildError(other); got != other {
			t.Errorf("%v became %v", other, got)
		}
	}
}

// The runner's IMAGE_BUILD_FAILED reaches the run as an ImageBuildError,
// with the build's tail and an image_build_failed event, whether the run
// builds alone or after finding the shared image missing.
func TestFetchImageBuildFailure(t *testing.T) {
	broken := protocol.ErrorMsg{Code: protocol.CodeImageBuildFailed, Message: "the project's Dockerfile .spotexec/Dockerfile failed to build: a RUN step exited 2", Detail: "ERROR: failed to solve"}
	for name, req := range map[string]protocol.Image{
		"alone":  {Dockerfile: ".spotexec/Dockerfile", Tag: "abcdef0123456789"},
		"shared": sharedImage(),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := fastShare(t)
			rep := &recRep{}
			c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
				var m protocol.Image
				_ = json.Unmarshal(msg, &m)
				if m.Probe {
					_ = c.Send(protocol.TypeImageResult, protocol.ImageResult{Missing: true})
					return
				}
				_ = c.Send(protocol.TypeError, broken)
			})
			_, _, _, err := fetchImage(ctx, c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, req, nil, time.Minute)
			var be *ImageBuildError
			if !errors.As(err, &be) || !be.Project() || be.Tail != broken.Detail || !be.Until.IsZero() {
				t.Fatalf("got %T %v", err, err)
			}
			ev := rep.event("image_build_failed")
			if ev["code"] != protocol.CodeImageBuildFailed || ev["dockerfile"] != ".spotexec/Dockerfile" || ev["output_tail"] != broken.Detail || ev["message"] != broken.Message || ev["cached"] != false {
				t.Fatalf("event %v", ev)
			}
			lease := &Lease{Runner: &config.Runner{Type: config.TypeAWSSpot}, InstanceID: "i-1"}
			if got, lost := imageErr(lease, err); got != err || lost != "" {
				t.Fatalf("imageErr = %v, %q; want the error as it is, no runner lost", got, lost)
			}
		})
	}
	// Any other image failure is spotexec's own, with no such event.
	rep := &recRep{}
	c := newScript(t, func(c *protocol.Conn, typ string, msg json.RawMessage) {
		_ = c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "docker build .spotexec/Dockerfile: exit 1"})
	})
	_, _, _, err := fetchImage(fastShare(t), c, Options{Rep: rep}, "aws#1", io.Discard, nil, imageMemo{}, protocol.Image{Dockerfile: ".spotexec/Dockerfile", Tag: "abcdef0123456789"}, nil, time.Minute)
	var be *ImageBuildError
	if err == nil || errors.As(err, &be) || rep.event("image_build_failed") != nil {
		t.Fatalf("got %T %v", err, err)
	}
	var ie *InfraError
	if got, _ := imageErr(&Lease{Runner: &config.Runner{Type: config.TypeSSH}}, err); !errors.As(got, &ie) {
		t.Fatalf("imageErr = %T %v, want spotexec's failure", got, got)
	}
}

// #193 with #194: a broken Dockerfile's build is remembered like any failed
// build, and the runs that need the same image fail at once with it, still
// as the project's failure (exit 123), not spotexec's.
func TestFetchImageRemembersABrokenDockerfile(t *testing.T) {
	ctx := fastShare(t)
	broken := imageScript{probe: protocol.ImageResult{Missing: true}, fail: protocol.ErrorMsg{Code: protocol.CodeImageBuildFailed,
		Message: "the project's Dockerfile .spotexec/Dockerfile failed to build: a RUN step exited 100", Detail: "E: Unable to locate package nope", BuildFailed: true}}
	rep := &recRep{}
	_, _, err := imageRun(ctx, t, broken, Options{Rep: rep}, sharedImage())
	var be *ImageBuildError
	if !errors.As(err, &be) || !be.Project() || be.Cached || be.Until.IsZero() || !strings.Contains(be.Tail, "Unable to locate package nope") {
		t.Fatalf("first build: %#v", err)
	}
	if ev := rep.event("image_build_failed"); ev == nil || ev["retry_after"] == nil || rep.event("image_failed") != nil {
		t.Fatalf("first build's event %v", ev)
	}
	lease := &Lease{Runner: &config.Runner{Type: config.TypeAWSSpot}, InstanceID: "i-1"}
	rep = &recRep{}
	c, _, err := imageRun(ctx, t, builds, Options{Rep: rep}, sharedImage())
	var cached *ImageBuildError
	if !errors.As(err, &cached) || !cached.Cached || !cached.Project() {
		t.Fatalf("second run: %#v", err)
	}
	var re *transport.RemoteError
	if !errors.As(err, &re) || re.Code != protocol.CodeImageBuildFailed || !strings.Contains(re.Message, "not building it again") {
		t.Fatalf("second run: %v", err)
	}
	if got := fmt.Sprint(c.sent); got != "[image:probe]" {
		t.Fatalf("second run sent %s", got)
	}
	if ev := rep.event("image_build_failed"); ev == nil || ev["cached"] != true || ev["code"] != protocol.CodeImageBuildFailed {
		t.Fatalf("second run's event %v", ev)
	}
	if got, lost := imageErr(lease, err); got != err || lost != "" {
		t.Fatalf("imageErr = %v, %q; want the error as it is, no runner lost", got, lost)
	}
}
