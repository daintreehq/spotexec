package helper

import (
	"errors"
	"fmt"
	"strings"
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

// #250: an error that already carries the helper's code keeps it, whole and
// once; anything else becomes IMAGE_FAILED.
func TestImageFailedKeepsOneCode(t *testing.T) {
	inner := &protoError{code: protocol.CodeImageBuildFailed, msg: "docker build Dockerfile: exit 1", detail: "step 3/9", buildFailed: true}
	want := inner.wire()
	var pe *protoError
	if got := imageFailed(inner); !errors.As(got, &pe) || pe.wire() != want {
		t.Fatalf("re-wrapped a coded error: %v", got)
	}
	if got := imageFailed(fmt.Errorf("invalid image reference %q", "x y")); !errors.As(got, &pe) || pe.code != protocol.CodeImageFailed || pe.msg != `invalid image reference "x y"` {
		t.Fatalf("plain error: %v", got)
	}
	if got := uncoded(inner); got != "docker build Dockerfile: exit 1" {
		t.Fatalf("uncoded: %q", got)
	}
	if got := uncoded(errors.New("plain")); got != "plain" {
		t.Fatalf("uncoded plain: %q", got)
	}
}

// #250: a container the run or the dependency install can't start fails
// with containerScript's error under one code, not that code twice.
func TestContainerFailureCodedOnce(t *testing.T) {
	bin, _ := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	forbidden := &protocol.ContainerRun{Image: "node:22", Options: []string{"-u", "0"}}
	check := func(what, code string, err error) {
		t.Helper()
		var pe *protoError
		if !errors.As(err, &pe) || pe.code != code || !strings.Contains(pe.msg, "container.options may not set -u") || strings.Contains(pe.msg, protocol.CodeImageFailed) {
			t.Fatalf("%s: got %v", what, err)
		}
	}

	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	s.c = protocol.NewConn(strings.NewReader(""), &syncBuf{})
	check("run", protocol.CodeImageFailed, s.exec(protocol.Exec{Shell: "true", Container: forbidden}))

	d, _ := depsSession(t)
	check("deps", protocol.CodeDepsFailed, d.deps(protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "true", CacheDirs: []string{"vendor"}, Container: forbidden}))
}
