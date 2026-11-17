package helper

import (
	"errors"
	"fmt"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// #162: an install or image build that fails as the VM shuts down is the
// runner going away, not the build failing; anything else, or any failure
// while the VM runs on, keeps its code.
func TestCutShortOnlyWhileStopping(t *testing.T) {
	old := stoppingNow
	t.Cleanup(func() { stoppingNow = old })
	for _, tc := range []struct {
		err      error
		stopping bool
		want     string
	}{
		{perr(protocol.CodeDepsFailed, "install command exited 143"), true, protocol.CodeShutdown},
		{perr(protocol.CodeImageFailed, "docker build: exit 1"), true, protocol.CodeShutdown},
		{perr(protocol.CodeDepsFailed, "install command exited 1"), false, protocol.CodeDepsFailed},
		{perr(protocol.CodeImageFailed, "docker build: exit 1"), false, protocol.CodeImageFailed},
		{perr(protocol.CodeImageBuildFailed, "a RUN step exited 137"), true, protocol.CodeShutdown},
		{perr(protocol.CodeImageBuildFailed, "a RUN step exited 1"), false, protocol.CodeImageBuildFailed},
		{perr(protocol.CodeBadRequest, "no"), true, protocol.CodeBadRequest},
	} {
		stoppingNow = func() bool { return tc.stopping }
		var pe *protoError
		if !errors.As(cutShort(tc.err), &pe) || pe.code != tc.want {
			t.Errorf("%v while stopping=%v: got %v, want %s", tc.err, tc.stopping, cutShort(tc.err), tc.want)
		}
	}
	// A build cut short is no failure to remember (#194); one that failed
	// while the VM runs on is.
	for _, stopping := range []bool{true, false} {
		stoppingNow = func() bool { return stopping }
		var pe *protoError
		if !errors.As(cutShort(&protoError{code: protocol.CodeImageFailed, msg: "docker build: exit 1", buildFailed: true}), &pe) || pe.buildFailed == stopping || pe.wire().BuildFailed == stopping {
			t.Errorf("stopping=%v: marked %+v", stopping, pe)
		}
	}
	stoppingNow = func() bool { return true }
	if err := cutShort(nil); err != nil {
		t.Errorf("success became %v", err)
	}
	if plain := fmt.Errorf("plain"); cutShort(plain) != plain {
		t.Error("a non-protocol error changed")
	}
}

// #250: an error that already carries the helper's code keeps it once;
// anything else becomes IMAGE_FAILED.
func TestImageFailedKeepsOneCode(t *testing.T) {
	inner := perr(protocol.CodeImageFailed, "can't read the users of image x: docker create: signal: killed")
	if got := imageFailed(inner); got != inner {
		t.Fatalf("re-wrapped a coded error: %v", got)
	}
	var pe *protoError
	if got := imageFailed(inner); !errors.As(got, &pe) || pe.wire().Message != "can't read the users of image x: docker create: signal: killed" {
		t.Fatalf("got %v", got)
	}
	if got := imageFailed(fmt.Errorf("invalid image reference %q", "x y")); !errors.As(got, &pe) || pe.code != protocol.CodeImageFailed || pe.msg != `invalid image reference "x y"` {
		t.Fatalf("plain error: %v", got)
	}
}
