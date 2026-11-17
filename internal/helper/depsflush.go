package helper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Restoring the dependency cache and copying it into a slot write
// gigabytes in seconds, into the page cache, which the disk then writes
// at its own pace: a fresh VM's gp3 root volume takes a minute or more for
// a few slots' worth (#253). Docker syncs the filesystem under a
// container's overlay when it unmounts it (overlayfs syncs its upper
// filesystem), which every docker create and run does, so the run's
// container start waits for all of it, in the setup phase, with nothing
// running. flushDeps waits for it in the dependency phase instead, and
// says what it is waiting on.

// flushMin is the page cache waiting to be written below which a run
// doesn't wait for it: the disk writes that in a few seconds.
var flushMin int64 = 256 << 20

// flushBudget bounds the wait. Past it the run goes on, and its
// container's start waits for the rest, as it did before.
var flushBudget = 10 * time.Minute

// flushTick is how often the wait checks that the client is still there.
var flushTick = 500 * time.Millisecond

// pendingBytes is the machine's page cache waiting to be written to disk
// (0 when unknown: nothing is waited for).
var pendingBytes = pendingWrites

// startSync starts writing everything on dir's filesystem to disk, in a
// child process (syncfs can't be interrupted: the helper must stay free
// to give up on it), and reports its end on the returned channel.
var startSync = func(dir string) (<-chan error, error) {
	cmd := exec.Command("sync", "-f", dir)
	cmd.SysProcAttr = childSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}

// dockerSharesFS reports whether dir is on the filesystem Docker keeps its
// containers on: only writes there hold up a container's start. When that
// can't be told, it says yes.
var dockerSharesFS = func(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := probeDocker(ctx)
	if err != nil {
		return true
	}
	root := dockerRootDir(d.info)
	if root == "" {
		return true
	}
	same, ok := sameDevice(dir, root)
	return same || !ok
}

// dockerRootDir is the "Docker Root Dir" `docker info` printed, or "".
func dockerRootDir(info string) string {
	for _, l := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "Docker Root Dir:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// sameDevice reports whether a and b are on the same filesystem; ok is
// false when either can't be looked at.
func sameDevice(a, b string) (same, ok bool) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, false
	}
	sa, oka := fa.Sys().(*syscall.Stat_t)
	sb, okb := fb.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return false, false
	}
	return uint64(sa.Dev) == uint64(sb.Dev), true
}

// flushDeps waits, before a containerised run starts, for what the
// dependency phase (and anything else) wrote on the work root's filesystem
// to reach the disk, when there is enough of it to hold up the container's
// start. It returns how long it waited. Only the client going, or the
// runner shutting down, ends it with an error: a flush that fails or runs
// past its budget leaves the rest to the container's start.
func (s *Session) flushDeps() (time.Duration, error) {
	pending := pendingBytes()
	if pending < flushMin || !dockerSharesFS(s.layout.Root) {
		return 0, nil
	}
	start := time.Now()
	done, err := startSync(s.layout.Root)
	if err != nil {
		return 0, nil
	}
	_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_flush", Message: sizeText(pending)})
	deadline := time.NewTimer(flushBudget)
	defer deadline.Stop()
	tick := time.NewTicker(flushTick)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return time.Since(start), nil
		case <-deadline.C:
			return time.Since(start), nil
		case <-tick.C:
			if s.stopping.Load() {
				return 0, perr(protocol.CodeShutdown, "the runner is shutting down")
			}
			if s.clientGone() {
				return 0, perr(protocol.CodeDepsFailed, "client went away while dependencies were written to disk")
			}
		}
	}
}

// sizeText is n bytes for a person: "8.1 GB", "300 MB".
func sizeText(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MB", n>>20)
}
