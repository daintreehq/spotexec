package cli

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
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
