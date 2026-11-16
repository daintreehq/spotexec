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
		{perr(protocol.CodeBadRequest, "no"), true, protocol.CodeBadRequest},
	} {
		stoppingNow = func() bool { return tc.stopping }
		var pe *protoError
		if !errors.As(cutShort(tc.err), &pe) || pe.code != tc.want {
			t.Errorf("%v while stopping=%v: got %v, want %s", tc.err, tc.stopping, cutShort(tc.err), tc.want)
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
