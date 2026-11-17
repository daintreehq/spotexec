package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/tailbuf"
)

// setupOutputMax is how much of a failed [setup]'s output is shown: its end.
const setupOutputMax = 64 << 10

func shellPath() (string, []string) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-c"}
	}
	return "/bin/sh", []string{"-c"}
}

// Login profiles load toolchains (mise, nvm, rbenv, conda), so they are
// sourced like a login shell would, but silently (a banner must not end up
// in the command's output), and before spotexec's own environment and
// working directory are applied (Ubuntu's /etc/profile resets PATH).
//
// A profile that exits would otherwise end the run with "success" before the
// command ran: the EXIT trap turns that into a failure. Options a profile
// sets (-e, -u, -x) and positional parameters are reset afterwards.
const profilePrelude = `trap 'echo "spotexec: a login profile exited the shell before the command ran" >&2; exit 125' EXIT
{ [ -r /etc/profile ] && . /etc/profile
if [ -n "${BASH_VERSION:-}" ]; then
  if [ -r "${HOME:-/nonexistent}/.bash_profile" ]; then . "$HOME/.bash_profile"
  elif [ -r "${HOME:-/nonexistent}/.bash_login" ]; then . "$HOME/.bash_login"
  elif [ -r "${HOME:-/nonexistent}/.profile" ]; then . "$HOME/.profile"; fi
elif [ -r "${HOME:-/nonexistent}/.profile" ]; then . "$HOME/.profile"; fi
} </dev/null >/dev/null 2>&1
trap - EXIT
set +e +u +x
set --
`

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// wrapScript applies the profile prelude, then the job's environment (from
// envFile: values never go on a command line, where ps would show them),
// then changes to dir and runs script.
func wrapScript(script, dir, envFile string) string {
	var b strings.Builder
	if oomLowered.Load() {
		b.WriteString(childOOMReset)
	}
	b.WriteString(profilePrelude)
	if envFile != "" {
		b.WriteString(". " + shq(envFile) + "\n")
	}
	b.WriteString("cd " + shq(dir) + " || exit 1\n")
	b.WriteString(script)
	return b.String()
}

// writeEnvFile writes env as shell exports to a private temp file.
func writeEnvFile(dir string, env map[string]string) (string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if envKeyRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + shq(env[k]) + "\n")
	}
	// In the private work root, not $TMPDIR (which may be missing,
	// unwritable, relative, or shared).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "env-*") // 0600
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// runProc spawns script in dir as its own process group, streams its output
// as frames, and enforces timeout/cancel/client loss. It returns the exit code,
// the terminating signal name, and flags.
type procResult struct {
	code     int
	signal   string
	timedOut bool
	canceled bool
	spotIntr bool
	startErr error
}

// teeOut, passed to runProc as out, sees the output as it goes to the
// client as frames (kind is the frame's kind).
type teeOut func(kind byte, p []byte)

// Write is never called: runProc hands teeOut each chunk with its kind.
func (t teeOut) Write(p []byte) (int, error) { t(protocol.KindStdout, p); return len(p), nil }

// Output goes to the client as frames, or into out when it is set (and
// nothing reaches the client), unless out is a teeOut: then it goes to both.
func (s *Session) runProc(script, dir string, env map[string]string, timeout, grace time.Duration, spotWatch bool, out io.Writer) procResult {
	pr := s.runProc1(script, dir, env, timeout, grace, spotWatch, out)
	if s.stopping.Load() {
		// Shut down under us (SIGTERM/SIGHUP, the VM going away): what ran
		// was killed by us. Report nothing (no result, no error, no cache
		// built from a half-finished install) so the client sees the runner
		// as lost; Shutdown exits the process.
		select {}
	}
	return pr
}

func (s *Session) runProc1(script, dir string, env map[string]string, timeout, grace time.Duration, spotWatch bool, out io.Writer) procResult {
	waitProtected()
	// Starting a process (and its env file) is atomic with respect to
	// Shutdown: it either sees the child and kills it, or we see it
	// stopping and never start.
	s.procMu.Lock()
	if s.stopping.Load() {
		s.procMu.Unlock()
		select {}
	}
	locked := true
	unlockProc := func() {
		if locked {
			locked = false
			s.procMu.Unlock()
		}
	}
	defer unlockProc()
	shell, args := shellPath()
	envDir := filepath.Join(s.layout.Root, "tmp")
	if s.layout.Root == "" {
		envDir = os.TempDir()
	}
	envFile, err := writeEnvFile(envDir, env)
	if err != nil {
		return procResult{code: 127, startErr: err}
	}
	if envFile != "" {
		defer os.Remove(envFile)
	}
	cmd := exec.Command(shell, append(args, wrapScript(script, dir, envFile))...)
	cmd.Dir = dir
	// The helper's own environment: the job's is applied after the login
	// profiles (a [env] HOME must not redirect where profiles are read).
	// SPOTEXEC_WORKSPACE is there from the start, so every process the run
	// leaves behind carries it, a forked subshell included (/proc shows the
	// environment a process was exec'd with): see reapWorkspace.
	cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE="+s.workspace)
	cmd.SysProcAttr = childSysProcAttr()
	devnull, _ := os.Open(os.DevNull)
	if devnull != nil {
		defer devnull.Close()
		cmd.Stdin = devnull
	}
	or, ow, err := os.Pipe()
	if err != nil {
		return procResult{code: 127, startErr: err}
	}
	er, ew, err := os.Pipe()
	if err != nil {
		return procResult{code: 127, startErr: err}
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	if err := cmd.Start(); err != nil {
		or.Close()
		ow.Close()
		er.Close()
		ew.Close()
		return procResult{code: 127, startErr: err}
	}
	ow.Close()
	ew.Close()
	pgid := cmd.Process.Pid
	s.setChildPGID(pgid)
	s.child.Store(int64(pgid))
	unlockProc()
	defer s.child.Store(0)
	_ = s.c.Send(protocol.TypeStarted, protocol.Started{PID: pgid})

	var wg sync.WaitGroup
	tee, teeing := out.(teeOut)
	pump := func(r *os.File, kind byte) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 && teeing {
				tee(kind, buf[:n])
			}
			if n > 0 && !teeing && out != nil {
				_, _ = out.Write(buf[:n])
			} else if n > 0 {
				if werr := s.c.WriteFrame(kind, append([]byte(nil), buf[:n]...)); werr != nil {
					// Client gone: keep draining so the child never blocks.
					_, _ = io.Copy(io.Discard, r)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(or, protocol.KindStdout)
	go pump(er, protocol.KindStderr)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var res procResult
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	terminate := func() { go killGroup(pgid, grace) }
	frames := s.frames
	liveness := time.NewTicker(5 * time.Second)
	defer liveness.Stop()
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-exited:
			break loop
		case <-timer:
			res.timedOut = true
			terminate()
			timer = nil
		case f, ok := <-frames:
			if !ok {
				res.canceled = true
				terminate()
				frames = nil
				continue
			}
			if env, err := protocol.Decode(f); err == nil && env.Type == protocol.TypeCancel {
				if !res.canceled {
					res.canceled = true
					terminate()
				} else {
					go func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }() // second ^C
				}
			}
		case <-liveness.C:
			touchActivity(s.layout.Root)
			if s.clientGone() && !res.canceled {
				res.canceled = true
				terminate()
			}
		}
	}
	// The main process is gone; reap anything it left running in its group
	// (watchers, servers) so pipes close and the slot is clean.
	if groupAlive(pgid) {
		killGroup(pgid, grace)
	}
	// Descendants that escaped the group (setsid, double fork) but still run
	// inside the workspace must not outlive the run and touch the next one.
	reapWorkspace(s.workspace, grace)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace + 2*time.Second):
		// A process escaped the group (setsid) but holds our pipes.
		or.Close()
		er.Close()
		<-done
	}
	or.Close()
	er.Close()
	s.setChildPGID(0)

	var ee *exec.ExitError
	switch {
	case waitErr == nil:
		res.code = 0
	case errors.As(waitErr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.signal = ws.Signal().String()
			res.code = 128 + int(ws.Signal())
		} else {
			res.code = ee.ExitCode()
		}
	default:
		res.code = 1
	}
	if res.timedOut {
		res.code = 124
	}
	return res
}

// watchCancel returns a context that the client's cancel (or its going
// away) ends, for work before a run's process starts, and stop, which ends
// the watch and reports whether that happened. Other frames are dropped,
// as runProc drops them; a cancel seen is never lost, since stop reports it
// even when the work finished first.
func (s *Session) watchCancel() (context.Context, func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	quit, exited := make(chan struct{}), make(chan struct{})
	var canceled atomic.Bool
	go func() {
		defer close(exited)
		liveness := time.NewTicker(time.Second)
		defer liveness.Stop()
		for {
			select {
			case <-quit:
				return
			case f, ok := <-s.frames:
				if env, err := protocol.Decode(f); !ok || err == nil && env.Type == protocol.TypeCancel {
					canceled.Store(true)
					cancel()
					return
				}
			case <-liveness.C:
				if s.clientGone() {
					canceled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() bool {
		close(quit)
		<-exited
		cancel()
		return canceled.Load()
	}
}

// cancelQueued reports, without waiting, whether the client canceled the
// run (or went away) since the last process ended. A withdraw read here
// comes too late to honor and is dropped.
func (s *Session) cancelQueued() bool {
	canceled, _ := s.stopQueued()
	return canceled
}

// stopQueued reports, without waiting, whether the client canceled the run
// (or went away), and whether it withdrew the job (protocol.TypeWithdraw),
// since the last look. Both are read: a cancel wins over a withdraw.
func (s *Session) stopQueued() (canceled, withdrawn bool) {
	if s.clientGone() {
		return true, false
	}
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				return true, withdrawn
			}
			if env, err := protocol.Decode(f); err == nil {
				canceled = canceled || env.Type == protocol.TypeCancel
				withdrawn = withdrawn || env.Type == protocol.TypeWithdraw
			}
		default:
			return canceled, withdrawn
		}
	}
}

// shutdownExit: the exit statuses a shutdown produces (SIGHUP, SIGKILL,
// SIGTERM). In a container, `docker run` also exits 125 when dockerd stops
// under it.
func shutdownExit(code int, container bool) bool {
	switch code {
	case 128 + int(syscall.SIGHUP), 128 + int(syscall.SIGKILL), 128 + int(syscall.SIGTERM):
		return true
	case 125:
		return container
	}
	return false
}

func (s *Session) exec(e protocol.Exec) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before exec")
	}
	// Before anything marks the slot dirty: a job that never gets to run
	// leaves its workspace as it was.
	if canceled, err := s.takeRun(); errors.Is(err, errWithdrawn) {
		return s.c.Send(protocol.TypeResult, protocol.Result{Withdrawn: true})
	} else if err != nil {
		return err
	} else if canceled {
		return s.c.Send(protocol.TypeResult, protocol.Result{ExitCode: 130, Canceled: true})
	}
	// A command whose processes or container couldn't be cleaned up keeps
	// its permit until the session ends: it may still be running.
	stuck := false
	defer func() {
		if !stuck {
			s.releaseRun()
		}
	}()
	s.loadStored()
	dir := s.workspace
	if e.Cwd != "" && e.Cwd != "." {
		rel, err := fsid.CleanRel(e.Cwd)
		if err != nil {
			return perr(protocol.CodeBadRequest, "bad cwd: %v", err)
		}
		dir = filepath.Join(s.workspace, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var pre snapshot
	if e.Snapshot {
		pre = s.takeSnapshot(e.Skip)
	}
	// Any interrupted run leaves the workspace in an unknown state.
	fp := s.state.Fingerprint
	s.state.Fingerprint = ""
	// Until the run's created files are collected, a helper that dies
	// leaves them unknown: the next acquire sweeps the workspace for them.
	s.state.Sweep, s.state.SweepSkip = e.Snapshot, e.Skip
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return fmt.Errorf("marking slot dirty before exec: %w", err)
	}

	env := map[string]string{}
	for k, v := range e.Env {
		env[k] = v
	}
	env["SPOTEXEC_WORKSPACE"] = s.workspace
	if _, set := env["SPOTEXEC_MEMORY"]; !set {
		if mb := s.memoryShareMB(); mb > 0 {
			env["SPOTEXEC_MEMORY"] = strconv.FormatInt(mb, 10)
		}
	}
	start := time.Now()
	timeout := time.Duration(e.TimeoutMS) * time.Millisecond
	grace := time.Duration(e.GraceMS) * time.Millisecond
	if grace <= 0 {
		grace = 5 * time.Second
	}
	// runPhase runs one script (the setup or the command) in dir, on the
	// runner or in the project container, and removes its container before
	// returning: one that can't be removed could still be writing into the
	// workspace. The VM's shutdown can kill the process (or dockerd, under a
	// container) before this helper is signalled itself: that is a lost
	// runner, not the script's result. One that finished on its own is
	// never one.
	runPhase := func(shell, dir string, timeout time.Duration, out io.Writer) (pr procResult, shutdown, oom bool, err error) {
		script, runEnv, cleanup := shell, env, func() error { return nil }
		if strings.TrimSpace(e.Setup) != "" {
			script = e.Setup + "\n" + script
		}
		if e.Container != nil {
			// Inside the project container: runner PATH setup doesn't apply.
			// Preparing the container can take minutes (reading the
			// image's users): a cancel meanwhile ends it, and the run.
			ctx, stop := s.watchCancel()
			host, hostEnv, c, err := s.containerScript(ctx, e.Container, shell, dir, env)
			if stop() {
				if err == nil {
					if err := c(); err != nil {
						return pr, false, false, perr(protocol.CodeImageFailed, "%v", err)
					}
				}
				pr.canceled, pr.code = true, 130
				return pr, false, false, nil
			}
			if err != nil {
				return pr, false, false, imageFailed(err)
			}
			script, runEnv, cleanup = host, hostEnv, c
		}
		pr = s.runProc(script, dir, runEnv, timeout, grace, e.SpotWatch, out)
		shutdown = pr.startErr == nil && !pr.timedOut && shutdownExit(pr.code, e.Container != nil) && systemStopping()
		if err := cleanup(); err != nil {
			if shutdown {
				// dockerd went away with the VM: the run was cut short, not failed.
				return pr, true, false, perr(protocol.CodeShutdown, "the runner is shutting down: %v", err)
			}
			if errors.Is(err, errFilesNotGivenBack) {
				// Nothing is still running, but files the sync can't read
				// would make the results incomplete.
				return pr, false, false, perr(protocol.CodeImageFailed, "%v; refusing to collect results from the workspace", err)
			}
			return pr, false, false, perr(protocol.CodeImageFailed, "%v; refusing to collect results from a workspace it may still be modifying", err)
		}
		if e.Container != nil && pr.startErr == nil && pr.code != 0 && !pr.timedOut && !pr.canceled && !shutdown {
			oom = s.containerOOMKilled()
		}
		return pr, shutdown, oom, nil
	}
	var (
		pr            procResult
		shutdown, oom bool
		setupFailed   bool
		setupMS       int64
		err           error
	)
	// The setup runs from the workspace root, inside the same snapshot as
	// the command, so what it changes is collected like the command's
	// changes. Its output is kept back unless it fails.
	if strings.TrimSpace(e.SetupRun) != "" {
		out := tailbuf.New(setupOutputMax)
		t := time.Now()
		pr, shutdown, oom, err = runPhase(e.SetupRun, s.workspace, time.Duration(e.SetupTimeoutMS)*time.Millisecond, out)
		if err != nil {
			stuck = true
			return err
		}
		setupMS = time.Since(t).Milliseconds()
		setupFailed = pr.startErr != nil || pr.code != 0 || pr.timedOut || pr.canceled || shutdown
		if setupFailed && !pr.canceled && !shutdown {
			for b := out.Bytes(); len(b) > 0; {
				n := min(len(b), 32<<10)
				if err := s.c.WriteFrame(protocol.KindStderr, b[:n]); err != nil {
					break
				}
				b = b[n:]
			}
		}
		// A cancel (or the client going) after the setup ended, while its
		// processes were reaped, must not let the command start at all.
		if !setupFailed && s.cancelQueued() {
			pr.canceled, setupFailed = true, true
		}
		if !setupFailed {
			_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "setup_done", Message: fmt.Sprintf("%dms", setupMS)})
		}
	}
	if !setupFailed {
		pr, shutdown, oom, err = runPhase(e.Shell, dir, timeout, nil)
		if err != nil {
			stuck = true
			return err
		}
	}
	// The command and its container are gone: collecting and writing back
	// its results needs no command capacity.
	s.releaseRun()
	res := protocol.Result{
		ExitCode: pr.code, Signal: pr.signal, TimedOut: pr.timedOut, Canceled: pr.canceled,
		Interrupted: pr.spotIntr || s.interrupted.Load(), DurationMS: time.Since(start).Milliseconds(),
		Shutdown: shutdown, SetupFailed: setupFailed, SetupMS: setupMS,
	}
	if pr.startErr != nil {
		res.StartError = pr.startErr.Error()
	}
	if oom {
		res.OOMKilled, res.MemoryLimitMB = true, s.memLimit>>20
	}
	// Nothing of a run the shutdown killed is collected (and the slot stays
	// marked dirty): report it before anything else can fail.
	if e.Snapshot && !shutdown {
		t := time.Now()
		post := s.takeSnapshot(e.Skip)
		var unreported []string
		res.Changed, res.Deleted, res.Created, res.Truncated, res.Unverified, unreported = s.diff(pre, post)
		res.GitDirs = newGitDirs(pre, post)
		res.SnapshotMS = time.Since(t).Milliseconds()
		s.state.Pending = s.pendingAfter(res.Created, res.Unverified, append(unreported, res.GitDirs...))
		s.state.Sweep, s.state.SweepSkip = false, nil
		if err := s.saveStored(); err != nil {
			return err
		}
		if len(res.Changed) == 0 && len(res.Deleted) == 0 && len(res.Unverified) == 0 {
			s.state.Fingerprint = fp // created files are outside the manifest
		}
	}
	s.state.LastUsed = time.Now().Unix()
	_ = saveSlotState(s.slotDir, s.state)
	touchActivity(s.layout.Root)
	return s.c.Send(protocol.TypeResult, res)
}
