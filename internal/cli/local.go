package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/ui"
)

// fallsBack reports whether a failed run may run locally instead: only when
// fallback is on, no runner was ever reached (so the command never ran
// remotely), and only for real commands (not exec, --same-slot or sync).
func (f *runFlags) fallsBack(l *loaded, o client.Options, res client.Result, err error) bool {
	var ie *client.InfraError
	return errors.As(err, &ie) && (f.fallbackLocal || l.cfg.Global.LocalFallback) && !res.Reached &&
		!o.PreferLast && !o.SameSlot && !o.SyncOnly && o.Command != ""
}

// runLocal runs a fallback command on this machine the way a runner would:
// under bash, in its own process group, with stdin from /dev/null, and with
// the run's timeout (exit 124). An interrupt or SIGTERM/SIGHUP to spotexec
// terminates the group (SIGTERM, then SIGKILL after the grace period; a
// second interrupt kills it at once) and exits 130. Whatever the command
// leaves running in its group is stopped the same way once it exits.
// Parts of a parallel task run as slot `slot` of `slots`, so each gets its
// own SPOTEXEC_SLOT, port range and CPU share.
func runLocal(ctx context.Context, intr <-chan struct{}, l *loaded, o client.Options, rep ui.Reporter, slot, slots int) int {
	shell := "/bin/sh"
	if p, err := exec.LookPath("bash"); err == nil {
		shell = p
	}
	cmd := exec.Command(shell, "-c", o.Command)
	cmd.Dir = filepath.Join(l.ws.Root, filepath.FromSlash(l.ws.Cwd))
	// The command was written for the runner: worktree paths in it were
	// rewritten to ${SPOTEXEC_WORKSPACE}, which here is the worktree itself
	// (unset, "rm -rf ${SPOTEXEC_WORKSPACE}/tmp" would mean /tmp).
	// Defaults, then the project's [env] (which may override them, as on a
	// runner), then what must hold here.
	slots = max(slots, 1)
	cmd.Env = append(os.Environ(), "CI=true", "SPOTEXEC_CPUS="+strconv.Itoa(max(runtime.NumCPU()/slots, 1)), "SPOTEXEC_SLOTS="+strconv.Itoa(slots),
		"SPOTEXEC_SLOT="+strconv.Itoa(slot), "SPOTEXEC_PORT_BASE="+strconv.Itoa(20000+slot*1000))
	for k, v := range l.cfg.Project.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "SPOTEXEC=1", "SPOTEXEC_WORKSPACE="+l.ws.Root, "SPOTEXEC_LOCAL=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	grace := o.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	// Output goes through pipes of our own (unless it's a file already), so
	// the command's exit is seen at once, not when a background process it
	// left behind lets go of its output.
	var copies sync.WaitGroup
	var readers, childEnds []*os.File
	attach := func(w io.Writer) (io.Writer, error) {
		if _, ok := w.(*os.File); ok || w == nil {
			return w, nil
		}
		r, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		readers, childEnds = append(readers, r), append(childEnds, pw)
		copies.Add(1)
		go func() { defer copies.Done(); _, _ = io.Copy(w, r) }()
		return pw, nil
	}
	closeAll := func(fs []*os.File) {
		for _, f := range fs {
			f.Close()
		}
	}
	var err error
	if cmd.Stdout, err = attach(o.Stdout); err == nil {
		cmd.Stderr, err = attach(o.Stderr)
	}
	if err == nil {
		err = cmd.Start()
	}
	closeAll(childEnds)
	if err != nil {
		closeAll(readers)
		copies.Wait()
		fmt.Fprintf(os.Stderr, "spotexec: running locally: %v\n", err)
		return client.ExitInfra
	}
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var timer <-chan time.Time
	if o.Timeout > 0 {
		t := time.NewTimer(o.Timeout)
		defer t.Stop()
		timer = t.C
	}
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	done := ctx.Done()
	var (
		werr                       error
		exitedNow                  bool
		timedOut, canceled, killed bool
		intrs                      int
		killAt, killedAt           time.Time
	)
	// terminate starts the one graceful stop: SIGTERM, SIGKILL at killAt.
	terminate := func() {
		if killAt.IsZero() {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			killAt = time.Now().Add(grace)
		}
	}
	kill := func() {
		if !killed {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			killed, killedAt = true, time.Now()
		}
	}
	for {
		select {
		case werr = <-exited:
			exited, exitedNow, timer = nil, true, nil
		case <-timer:
			timer = nil
			timedOut = !canceled
			terminate()
		case <-done:
			// Comes with the first interrupt: one termination for both.
			done = nil
			canceled = canceled || !timedOut
			terminate()
		case _, ok := <-intr:
			if !ok {
				intr = nil
				continue
			}
			intrs++
			canceled = canceled || !timedOut
			if intrs > 1 {
				kill()
			}
			terminate()
		case <-poll.C:
		}
		if !killAt.IsZero() && time.Now().After(killAt) {
			kill()
		}
		if exitedNow {
			// Done when the group is gone, or soon after SIGKILL even if it
			// isn't (an unreaped zombie can't be killed).
			if !groupAlive(pgid) || killed && time.Since(killedAt) > 2*time.Second {
				break
			}
			terminate() // what the command left running in its group
		}
	}
	// Output pipes close once the group is gone; a process that escaped it
	// (setsid) must not hold the run open.
	drained := make(chan struct{})
	go func() { copies.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		closeAll(readers)
		// A destination that blocks (a full pipe nobody reads) must not
		// hold the run either: its copy is abandoned.
		select {
		case <-drained:
		case <-time.After(time.Second):
		}
	}
	closeAll(readers)
	switch {
	case timedOut:
		rep.Warn("TIMEOUT", "command exceeded %s and was terminated (raise with --timeout)", o.Timeout)
		return client.ExitTimeout
	case canceled:
		return 130
	}
	var ee *exec.ExitError
	switch {
	case werr == nil:
		return 0
	case errors.As(werr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "spotexec: running locally: %v\n", werr)
	return client.ExitInfra
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
