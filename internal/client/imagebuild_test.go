package client

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

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
