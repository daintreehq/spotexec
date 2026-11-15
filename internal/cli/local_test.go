package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/ui"
)

func localRun(t *testing.T, command string, timeout time.Duration, intr chan struct{}, ctx context.Context) (int, string, time.Duration) {
	return localRunGrace(t, command, timeout, 300*time.Millisecond, intr, ctx)
}

func localRunGrace(t *testing.T, command string, timeout, grace time.Duration, intr chan struct{}, ctx context.Context) (int, string, time.Duration) {
	t.Helper()
	l := testLoaded()
	l.ws.Root = t.TempDir()
	rep, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	var out lockedBuf
	o := client.Options{Command: command, Timeout: timeout, Grace: grace, Stdout: &out, Stderr: &out}
	start := time.Now()
	code := runLocal(ctx, intr, l, o, rep, 0, 1)
	return code, out.String(), time.Since(start)
}

func TestRunLocalLikeARunner(t *testing.T) {
	// bash, in a process group of its own, stdin from /dev/null.
	code, out, _ := localRun(t, `[[ -n $BASH_VERSION ]] && echo bash; ps -o pgid= -p $$; read -r x || echo eof`, 0, nil, context.Background())
	lines := strings.Fields(out)
	if code != 0 || len(lines) != 3 || lines[0] != "bash" || lines[2] != "eof" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if pg, _ := strconv.Atoi(lines[1]); pg == syscall.Getpgrp() {
		t.Error("local command shares spotexec's process group")
	}
	if code, _, _ := localRun(t, "exit 3", 0, nil, context.Background()); code != 3 {
		t.Errorf("exit code: %d", code)
	}
}

func TestRunLocalTimeout(t *testing.T) {
	code, _, took := localRun(t, "sleep 30", 300*time.Millisecond, nil, context.Background())
	if code != client.ExitTimeout || took > 5*time.Second {
		t.Fatalf("exit %d after %s", code, took)
	}
	// A command that ignores SIGTERM is killed after the grace period.
	code, _, took = localRun(t, "trap '' TERM; sleep 30", 300*time.Millisecond, nil, context.Background())
	if code != client.ExitTimeout || took > 5*time.Second {
		t.Fatalf("TERM-ignoring command: exit %d after %s", code, took)
	}
}

func TestRunLocalInterrupt(t *testing.T) {
	// The first interrupt (which also cancels the context) terminates the
	// group gracefully, so the command's own cleanup runs.
	ctx, cancel := context.WithCancel(context.Background())
	intr := make(chan struct{}, 4)
	time.AfterFunc(300*time.Millisecond, func() { cancel(); intr <- struct{}{} })
	code, out, took := localRun(t, `trap 'echo cleaned; exit 0' TERM; while :; do sleep 0.05; done`, 0, intr, ctx)
	if code != 130 || !strings.Contains(out, "cleaned") || took > 5*time.Second {
		t.Fatalf("exit %d after %s, output %q", code, took, out)
	}
	// A second interrupt kills it without waiting out the (long) grace
	// period, also when only a TERM-ignoring child is left.
	for _, c := range []string{"trap '' TERM; sleep 30", "(trap '' TERM; sleep 30) & wait"} {
		intr = make(chan struct{}, 4)
		time.AfterFunc(300*time.Millisecond, func() { intr <- struct{}{} })
		time.AfterFunc(600*time.Millisecond, func() { intr <- struct{}{} })
		code, _, took = localRunGrace(t, c, 0, 20*time.Second, intr, context.Background())
		if code != 130 || took > 3*time.Second {
			t.Fatalf("%q, second interrupt: exit %d after %s", c, code, took)
		}
	}
}

func TestRunLocalBlockedOutputDoesntHoldTheRun(t *testing.T) {
	l := testLoaded()
	l.ws.Root = t.TempDir()
	rep, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	block := make(chan struct{})
	defer close(block)
	w := writerFunc(func(p []byte) (int, error) { <-block; return len(p), nil })
	o := client.Options{Command: "yes", Timeout: 300 * time.Millisecond, Grace: 300 * time.Millisecond, Stdout: w, Stderr: w}
	start := time.Now()
	if code := runLocal(context.Background(), nil, l, o, rep, 0, 1); code != client.ExitTimeout || time.Since(start) > 6*time.Second {
		t.Fatalf("exit %d after %s", code, time.Since(start))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestRunLocalBackgroundChildDoesntHoldTheRun(t *testing.T) {
	// The command exits at once; the child it leaves holding the output is
	// stopped, and the run is neither held open nor counted as timed out.
	code, out, took := localRunGrace(t, "echo hi; sleep 30 &", time.Second, 5*time.Second, nil, context.Background())
	if code != 0 || out != "hi\n" || took > 900*time.Millisecond {
		t.Fatalf("exit %d after %s, output %q", code, took, out)
	}
}

func TestParallelTasksFallBackLocally(t *testing.T) {
	dir := project(t, "[tasks.l]\nrun = \"echo $SPOTEXEC_PORT_BASE > l.out\"\n[tasks.t]\nrun = \"[[ -n $BASH_VERSION ]] && echo $SPOTEXEC_PORT_BASE > t.out\"\n[tasks.v]\nparallel = [\"l\", \"t\"]\n")
	// A runner that refuses connections at once.
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	code, stderr := runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "v")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	ports := map[string]bool{}
	for _, f := range []string{"l.out", "t.out"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s didn't run locally: %v (stderr %q)", f, err, stderr)
		}
		ports[strings.TrimSpace(string(b))] = true
	}
	if len(ports) != 2 {
		t.Errorf("parallel local parts share a port range: %v", ports)
	}
	// Each part's result comes through the plain reporter: no ANSI.
	for _, want := range []string{"sx: l · ", "sx: t · ", " locally\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, stderr)
		}
	}
	if strings.Contains(stderr, "\x1b") {
		t.Errorf("ANSI in plain output: %q", stderr)
	}
	if _, stderr := runMain(t, "sx", "-C", dir, "-q", "--fallback-local", "--queue-timeout", "5s", "v"); strings.Contains(stderr, "sx: l · ") || strings.Contains(stderr, "sx: t · ") {
		t.Errorf("quiet mode printed results: %q", stderr)
	} else if !strings.Contains(stderr, "sx: warning: ") {
		t.Errorf("quiet mode dropped the fallback warning: %q", stderr)
	}
	// Without fallback, the same task fails with 125, and parts that never
	// reached a runner don't claim one.
	code, stderr = runMain(t, "sx", "-C", dir, "--queue-timeout", "5s", "v")
	if code != client.ExitInfra || !strings.Contains(stderr, "sx: l · ") || !strings.Contains(stderr, "exit 125") || strings.Contains(stderr, "#0") {
		t.Errorf("without fallback: exit %d, stderr %q", code, stderr)
	}
}

func TestShardedTaskFansOut(t *testing.T) {
	dir := project(t, `[tasks.unit]
run = "echo $SPOTEXEC_SHARD/$SPOTEXEC_SHARDS {args} > s{shard}of{shards}.out; [ {shard} != 2 ] || exit 3"
shards = 3
[tasks.lint]
run = "echo $SPOTEXEC_SHARD > lint.out"
[tasks.all]
parallel = ["unit", "lint"]
`)
	// A runner that refuses connections at once.
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	// Every shard runs, with its number and the extra arguments; the
	// failing one sets the exit code.
	code, stderr := runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "unit", "a b")
	if code != 3 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for i := 1; i <= 3; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "s"+strconv.Itoa(i)+"of3.out"))
		if want := strconv.Itoa(i) + "/3 a b\n"; err != nil || string(b) != want {
			t.Errorf("shard %d: %q, %v (stderr %q)", i, b, err, stderr)
		}
	}
	for _, want := range []string{"sx: unit 1/3 · ", "sx: unit 2/3 · ", "sx: unit 3/3 · ", "locally · exit 3\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, stderr)
		}
	}
	// NDJSON tells the shards apart, and the result names the shard count.
	code, stderr = runMain(t, "sx", "-C", dir, "--json", "--fallback-local", "--queue-timeout", "5s", "unit")
	if code != 3 {
		t.Fatalf("--json: exit %d, stderr %q", code, stderr)
	}
	shards := map[float64]bool{}
	var result map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("not NDJSON: %q", line)
		}
		if ev["event"] == "result" && ev["shard"] == nil {
			result = ev
		} else if ev["task"] == "unit" && ev["shards"] == float64(3) {
			shards[ev["shard"].(float64)] = true
		}
	}
	if len(shards) != 3 || result == nil || result["task"] != "unit" || result["shards"] != float64(3) || result["exit_code"] != float64(3) {
		t.Errorf("shards %v, result %v (stderr %q)", shards, result, stderr)
	}
	// Named in a parallel task, a sharded task fans out in place.
	os.Remove(filepath.Join(dir, "s1of3.out"))
	code, stderr = runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "all")
	if code != 3 || !strings.Contains(stderr, "sx: lint · ") || !strings.Contains(stderr, "sx: unit 3/3 · ") {
		t.Errorf("parallel: exit %d, stderr %q", code, stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "s1of3.out")); err != nil || string(b) != "1/3\n" {
		t.Errorf("parallel shard 1: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "lint.out")); err != nil || string(b) != "\n" {
		t.Errorf("an unsharded part got a shard number: %q, %v", b, err)
	}
	// Parts run on several slots at once: --same-slot can't apply.
	code, stderr = runMain(t, "sx", "-C", dir, "--same-slot", "unit")
	if code != client.ExitInfra || !strings.Contains(stderr, "unit: it's a sharded task, whose 3 shards") {
		t.Errorf("--same-slot: exit %d, stderr %q", code, stderr)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestSameSlotAfterParallelTaskUsesTheFirstPart(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	ws := &client.Workspace{WorktreeKey: "k"}
	saveFirstLastRun(ws, []*client.LastRun{nil, {Runner: "b", Slot: 3}, {Runner: "c", Slot: 1}})
	if lr, err := client.LoadLastRun(ws); err != nil || lr.Runner != "b" || lr.Slot != 3 {
		t.Fatalf("got %+v, %v", lr, err)
	}
	// No part synced: the previous record stays.
	saveFirstLastRun(ws, []*client.LastRun{nil, nil})
	if lr, _ := client.LoadLastRun(ws); lr.Runner != "b" {
		t.Fatalf("record replaced: %+v", lr)
	}
}

// The project's [setup] runs first, from the worktree root, quietly; a
// failed one shows its output and the command never runs.
func TestRunLocalSetup(t *testing.T) {
	run := func(setup, command string, o client.Options) (int, string, string) {
		t.Helper()
		l := testLoaded()
		l.ws.Root = t.TempDir()
		l.ws.Cwd = "sub"
		os.Mkdir(filepath.Join(l.ws.Root, "sub"), 0o755)
		l.cfg.Project.Setup.Run = setup
		rep, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
		var out lockedBuf
		o.Command, o.Stdout, o.Stderr, o.Grace = command, &out, &out, 300*time.Millisecond
		code := runLocal(context.Background(), nil, l, o, rep, 0, 1)
		return code, out.String(), l.ws.Root
	}
	code, out, root := run("echo noisy; mkdir -p dist && echo built > dist/x", "cat ../dist/x", client.Options{})
	if code != 0 || out != "built\n" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	code, out, root = run("echo why-it-failed; exit 125", "touch ran", client.Options{})
	if _, err := os.Stat(filepath.Join(root, "sub", "ran")); code != 1 || !strings.Contains(out, "why-it-failed") || err == nil {
		t.Fatalf("failed setup: exit %d, output %q, command ran: %v", code, out, err == nil)
	}
	code, out, _ = run("touch ran-setup; exit 3", "echo command", client.Options{NoSetup: true})
	if code != 0 || out != "command\n" {
		t.Fatalf("--no-setup: exit %d, output %q", code, out)
	}
	start := time.Now()
	code, _, root = run("sleep 30", "touch ran", client.Options{Timeout: 300 * time.Millisecond})
	if _, err := os.Stat(filepath.Join(root, "sub", "ran")); code != client.ExitTimeout || err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("setup timeout: exit %d after %s, command ran: %v", code, time.Since(start), err == nil)
	}
}

// Ctrl-C during the setup ends the run there (130), even when the setup
// handles the signal and exits 0.
func TestRunLocalSetupInterrupted(t *testing.T) {
	l := testLoaded()
	l.ws.Root = t.TempDir()
	l.cfg.Project.Setup.Run = `trap 'exit 0' TERM; sleep 30 & touch ready; wait`
	rep, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(l.ws.Root, "ready")); err == nil {
				break
			}
		}
		cancel()
	}()
	var out lockedBuf
	o := client.Options{Command: "touch ran", Grace: 300 * time.Millisecond, Stdout: &out, Stderr: &out}
	code := runLocal(ctx, nil, l, o, rep, 0, 1)
	if _, err := os.Stat(filepath.Join(l.ws.Root, "ran")); code != client.ExitInterrupted || err == nil {
		t.Fatalf("exit %d, command ran: %v, output %q", code, err == nil, out.String())
	}
}
