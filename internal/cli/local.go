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
	"github.com/daintreehq/spotexec/internal/tailbuf"
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
// own SPOTEXEC_SLOT, port range and CPU share. The project's [setup] runs
// first, from the worktree root, the same way: its output is shown only if
// it fails, and then the command doesn't run.
func runLocal(ctx context.Context, intr <-chan struct{}, l *loaded, o client.Options, rep ui.Reporter, slot, slots int) int {
	// The command was written for the runner: worktree paths in it were
	// rewritten to ${SPOTEXEC_WORKSPACE}, which here is the worktree itself
	// (unset, "rm -rf ${SPOTEXEC_WORKSPACE}/tmp" would mean /tmp).
	// Defaults, then the project's [env] (which may override them, as on a
	// runner), then what must hold here.
	env := localEnv(l, o, slot, slots)
	grace := o.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	if run, timeout := client.SetupFor(l.cfg.Project, o); run != "" {
		out := tailbuf.New(64 << 10)
		r := runLocalProc(ctx, intr, run, l.ws.Root, env, out, out, timeout, grace)
		switch {
		case r.err != nil:
			fmt.Fprintf(stderrOf(o), "spotexec: running [setup] locally: %v\n", r.err)
			return client.ExitInfra
		case r.canceled:
			return client.ExitInterrupted
		case r.timedOut:
			_, _ = stderrOf(o).Write(out.Bytes())
			rep.Warn("SETUP_FAILED", "[setup] run exceeded %s and was terminated (raise setup.timeout); the command did not run", timeout)
			return client.ExitTimeout
		case r.code != 0:
			_, _ = stderrOf(o).Write(out.Bytes())
			rep.Warn("SETUP_FAILED", "[setup] run failed (exit %d); the command did not run. Its output is above; skip the setup with --no-setup or setup = false on a task", r.code)
			return 1
		}
		if ctx.Err() != nil {
			return client.ExitInterrupted
		}
	}
	r := runLocalProc(ctx, intr, o.Command, localDir(l), env, o.Stdout, o.Stderr, o.Timeout, grace)
	switch {
	case r.err != nil:
		fmt.Fprintf(stderrOf(o), "spotexec: running locally: %v\n", r.err)
		return client.ExitInfra
	case r.timedOut:
		rep.Warn("TIMEOUT", "command exceeded %s and was terminated (raise with --timeout)", o.Timeout)
		return client.ExitTimeout
	case r.canceled:
		return client.ExitInterrupted
	}
	return r.code
}

// localEnv is the environment a local run gets: defaults, then the
// project's [env] (which may override them, as on a runner), then what must
// hold here.
func localEnv(l *loaded, o client.Options, slot, slots int) []string {
	slots = max(slots, 1)
	env := append(os.Environ(), "CI=true", "SPOTEXEC_CPUS="+strconv.Itoa(max(runtime.NumCPU()/slots, 1)), "SPOTEXEC_SLOTS="+strconv.Itoa(slots),
		"SPOTEXEC_SLOT="+strconv.Itoa(slot), "SPOTEXEC_PORT_BASE="+strconv.Itoa(20000+slot*1000))
	for k, v := range l.cfg.Project.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range o.Env {
		env = append(env, k+"="+v)
	}
	return append(env, "SPOTEXEC=1", "SPOTEXEC_WORKSPACE="+l.ws.Root, "SPOTEXEC_LOCAL=1")
}

// localDir is where a local run's command runs: the worktree's current
// directory.
func localDir(l *loaded) string { return filepath.Join(l.ws.Root, filepath.FromSlash(l.ws.Cwd)) }

// localShell is the shell a local run uses: bash, else sh.
func localShell() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

func stderrOf(o client.Options) io.Writer {
	if o.Stderr == nil {
		return os.Stderr
	}
	return o.Stderr
}

type localResult struct {
	code               int
	timedOut, canceled bool
	err                error // it could not be started, or its status not read
}

// runLocalProc runs script under bash in dir, in its own process group,
// with the timeout and interrupt handling runLocal describes.
func runLocalProc(ctx context.Context, intr <-chan struct{}, script, dir string, env []string, stdout, stderr io.Writer, timeout, grace time.Duration) localResult {
	cmd := exec.Command(localShell(), "-c", script)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	if cmd.Stdout, err = attach(stdout); err == nil {
		cmd.Stderr, err = attach(stderr)
	}
	if err == nil {
		err = cmd.Start()
	}
	closeAll(childEnds)
	if err != nil {
		closeAll(readers)
		copies.Wait()
		return localResult{err: err}
	}
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
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
	if timedOut || canceled {
		return localResult{code: 1, timedOut: timedOut, canceled: canceled}
	}
	var ee *exec.ExitError
	switch {
	case werr == nil:
		return localResult{}
	case errors.As(werr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return localResult{code: 128 + int(ws.Signal())}
		}
		return localResult{code: ee.ExitCode()}
	}
	return localResult{err: werr}
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
