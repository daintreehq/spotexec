package helper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/fsid"
	"github.com/spotexec/spotexec/internal/protocol"
)

func shellPath() (string, []string) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-lc"}
	}
	return "/bin/sh", []string{"-lc"}
}

// buildEnv returns the helper's environment overlaid with extra.
func buildEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
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

func (s *Session) runProc(script, dir string, env map[string]string, timeout, grace time.Duration, spotWatch bool) procResult {
	shell, args := shellPath()
	cmd := exec.Command(shell, append(args, script)...)
	cmd.Dir = dir
	cmd.Env = buildEnv(env)
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
	defer s.child.Store(0)
	_ = s.c.Send(protocol.TypeStarted, protocol.Started{PID: pgid})

	var wg sync.WaitGroup
	pump := func(r *os.File, kind byte) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
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

func (s *Session) exec(e protocol.Exec) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before exec")
	}
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
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return fmt.Errorf("marking slot dirty before exec: %w", err)
	}

	env := map[string]string{}
	for k, v := range e.Env {
		env[k] = v
	}
	env["SPOTEXEC_WORKSPACE"] = s.workspace
	script := e.Shell
	if strings.TrimSpace(e.Setup) != "" {
		script = e.Setup + "\n" + script
	}
	start := time.Now()
	timeout := time.Duration(e.TimeoutMS) * time.Millisecond
	grace := time.Duration(e.GraceMS) * time.Millisecond
	if grace <= 0 {
		grace = 5 * time.Second
	}
	runEnv := env
	cleanup := func() error { return nil }
	if e.Container != nil {
		// Inside the project container: runner PATH setup doesn't apply.
		host, hostEnv, c, err := s.containerScript(e.Container, e.Shell, dir, env)
		if err != nil {
			return perr(protocol.CodeImageFailed, "%v", err)
		}
		script, runEnv, cleanup = host, hostEnv, c
	}
	pr := s.runProc(script, dir, runEnv, timeout, grace, e.SpotWatch)
	// The container must be gone before we snapshot the workspace: a
	// container we can't remove could still be writing into it.
	if err := cleanup(); err != nil {
		return perr(protocol.CodeImageFailed, "%v; refusing to collect results from a workspace it may still be modifying", err)
	}
	res := protocol.Result{
		ExitCode: pr.code, Signal: pr.signal, TimedOut: pr.timedOut, Canceled: pr.canceled,
		Interrupted: pr.spotIntr || s.interrupted.Load(), DurationMS: time.Since(start).Milliseconds(),
	}
	if pr.startErr != nil {
		res.StartError = pr.startErr.Error()
	}
	if e.Snapshot {
		t := time.Now()
		post := s.takeSnapshot(e.Skip)
		res.Changed, res.Deleted, res.Created, res.Truncated, res.Unverified = s.diff(pre, post)
		res.SnapshotMS = time.Since(t).Milliseconds()
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
