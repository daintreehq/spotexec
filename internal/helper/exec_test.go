package helper

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

type execOutput struct {
	stdout, stderr string
	events         []string
	res            protocol.Result
}

// runExec runs e in a freshly acquired slot (cancel, when set, is sent
// to the session after that long) and collects what reached the client.
func runExec(t *testing.T, e protocol.Exec, cancel time.Duration) (execOutput, string) {
	t.Helper()
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	s.c = protocol.NewConn(strings.NewReader(""), out)
	if cancel > 0 {
		var b bytes.Buffer
		if err := protocol.NewConn(nil, &b).Send(protocol.TypeCancel, protocol.Cancel{Reason: "interrupt"}); err != nil {
			t.Fatal(err)
		}
		f, err := protocol.NewConn(&b, io.Discard).ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(cancel, func() { s.frames <- f })
	}
	if err := s.exec(e); err != nil {
		t.Fatal(err)
	}
	var o execOutput
	in := protocol.NewConn(bytes.NewReader(out.b.Bytes()), io.Discard)
	for {
		f, err := in.ReadFrame()
		if err != nil {
			break
		}
		switch f.Kind {
		case protocol.KindStdout:
			o.stdout += string(f.Data)
			continue
		case protocol.KindStderr:
			o.stderr += string(f.Data)
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		switch env.Type {
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			o.events = append(o.events, ev.Name)
		case protocol.TypeResult:
			if err := env.Unmarshal(&o.res); err != nil {
				t.Fatal(err)
			}
		}
	}
	return o, s.workspace
}

// The setup runs from the workspace root before the command, inside the
// same snapshot, and its output stays out of the command's.
func TestExecSetupRunsFirstAndQuietly(t *testing.T) {
	o, ws := runExec(t, protocol.Exec{
		SetupRun: `echo setup-out; echo setup-err >&2; mkdir -p dist && echo built > dist/x; pwd > setup.pwd`,
		Shell:    `cat ../dist/x; pwd`, Cwd: "sub", Snapshot: true,
	}, 0)
	if o.res.SetupFailed || o.res.ExitCode != 0 {
		t.Fatalf("result %+v, stderr %q", o.res, o.stderr)
	}
	if strings.Contains(o.stdout+o.stderr, "setup-") {
		t.Errorf("setup output reached the client: stdout %q, stderr %q", o.stdout, o.stderr)
	}
	if !strings.HasPrefix(o.stdout, "built\n") || !strings.HasSuffix(strings.TrimSpace(o.stdout), "/sub") {
		t.Errorf("command output %q", o.stdout)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "setup.pwd")); mustEval(t, strings.TrimSpace(string(b))) != mustEval(t, ws) {
		t.Errorf("setup ran in %q, want the workspace root %q", b, ws)
	}
	var created []string
	for _, c := range o.res.Created {
		created = append(created, c.Path)
	}
	if !strings.Contains(strings.Join(created, " "), "dist/x") {
		t.Errorf("the setup's files are not in the run's snapshot: %v", created)
	}
	if strings.Join(o.events, ",") != "setup_done" {
		t.Errorf("events %v", o.events)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A failed setup shows its output, reports its exit code and never starts
// the command.
func TestExecSetupFailureStopsTheCommand(t *testing.T) {
	o, ws := runExec(t, protocol.Exec{SetupRun: `echo why-it-failed; exit 7`, Shell: `touch ran`, Snapshot: true}, 0)
	if !o.res.SetupFailed || o.res.ExitCode != 7 {
		t.Fatalf("result %+v", o.res)
	}
	if !strings.Contains(o.stderr, "why-it-failed") || o.stdout != "" {
		t.Errorf("stdout %q, stderr %q", o.stdout, o.stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err == nil {
		t.Error("the command ran after a failed setup")
	}
}

func TestExecSetupTimeout(t *testing.T) {
	start := time.Now()
	o, ws := runExec(t, protocol.Exec{SetupRun: `echo slow; sleep 30`, SetupTimeoutMS: 300, GraceMS: 300, Shell: `touch ran`}, 0)
	if !o.res.SetupFailed || !o.res.TimedOut || o.res.ExitCode != 124 || time.Since(start) > 10*time.Second {
		t.Fatalf("result %+v after %s", o.res, time.Since(start))
	}
	if !strings.Contains(o.stderr, "slow") {
		t.Errorf("stderr %q", o.stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err == nil {
		t.Error("the command ran after the setup timed out")
	}
}

// A cancel during the setup ends the run there, even when the setup
// handles the signal and exits 0.
func TestExecSetupCanceled(t *testing.T) {
	o, ws := runExec(t, protocol.Exec{SetupRun: `trap 'exit 0' TERM; sleep 30 & wait`, GraceMS: 300, Shell: `touch ran`}, 300*time.Millisecond)
	if !o.res.SetupFailed || !o.res.Canceled {
		t.Fatalf("result %+v", o.res)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err == nil {
		t.Error("the command ran after the setup was canceled")
	}
}

// Without a setup, exec is unchanged.
func TestExecWithoutSetup(t *testing.T) {
	o, _ := runExec(t, protocol.Exec{Shell: `echo out; echo err >&2; exit 3`}, 0)
	if o.res.SetupFailed || o.res.ExitCode != 3 || o.stdout != "out\n" || o.stderr != "err\n" || len(o.events) != 0 {
		t.Fatalf("result %+v, stdout %q, stderr %q, events %v", o.res, o.stdout, o.stderr, o.events)
	}
}
