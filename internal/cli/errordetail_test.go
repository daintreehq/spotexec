package cli

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestDepsDetailPrefixesEveryLine(t *testing.T) {
	de := &client.DepsError{Err: errors.New("dependencies: DEPS_FAILED: install command exited 1"), Tail: "npm ERR! a\nnpm ERR! b", Log: "/l/a.log"}
	var b bytes.Buffer
	errDetail(&b, "[test 2/4] ", fmt.Errorf("wrapped: %w", de))
	want := "[test 2/4]   the install's output:\n[test 2/4]     npm ERR! a\n[test 2/4]     npm ERR! b\n[test 2/4]   full log: /l/a.log\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
	b.Reset()
	errDetail(&b, "", errors.New("something else"))
	if b.Len() != 0 {
		t.Fatalf("printed %q for another error", b.String())
	}
}

// #193: a broken Dockerfile exits 123 with the end of the build's output
// under its error; every other failure of a run is spotexec's (125).
func TestImageBuildFailureExitAndDetail(t *testing.T) {
	be := &client.ImageBuildError{Err: errors.New("container image: IMAGE_BUILD_FAILED: the project's Dockerfile Dockerfile failed to build: a RUN step exited 2")}
	be.Code, be.Tail = protocol.CodeImageBuildFailed, "#5 0.2 make: *** Stop.\nERROR: failed to solve"
	if got := failExit(fmt.Errorf("wrapped: %w", be)); got != client.ExitImageBuild {
		t.Fatalf("exit %d, want %d", got, client.ExitImageBuild)
	}
	// A remembered failed build that isn't the Dockerfile's fault (#194)
	// stays spotexec's own failure.
	other := &client.ImageBuildError{Err: errors.New("container image: IMAGE_FAILED: docker build Dockerfile: exit 1")}
	other.Code = protocol.CodeImageFailed
	for _, err := range []error{&client.InfraError{Err: errors.New("container image: IMAGE_FAILED: docker build Dockerfile: exit 1")}, &client.InfraError{Err: other}, errors.New("ssh: lost")} {
		if got := failExit(err); got != client.ExitInfra {
			t.Errorf("%v: exit %d, want %d", err, got, client.ExitInfra)
		}
	}
	var b bytes.Buffer
	errDetail(&b, "[img] ", be)
	want := "[img]   end of the build's output:\n[img]     #5 0.2 make: *** Stop.\n[img]     ERROR: failed to solve\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

// A broken Dockerfile never falls back to running locally, where the
// command would run without the image and hide the breakage.
func TestImageBuildFailureDoesNotFallBack(t *testing.T) {
	f := &runFlags{fallbackLocal: true}
	l := &loaded{cfg: &config.Config{}}
	o := client.Options{Command: "make test"}
	if !f.fallsBack(l, o, client.Result{}, &client.InfraError{Err: errors.New("no runner")}) {
		t.Fatal("the control case doesn't fall back")
	}
	be := &client.ImageBuildError{Err: errors.New("container image: IMAGE_BUILD_FAILED: …")}
	be.Code = protocol.CodeImageBuildFailed
	if f.fallsBack(l, o, client.Result{}, be) {
		t.Fatal("a broken Dockerfile fell back to a local run")
	}
}
