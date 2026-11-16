package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	code, stderr = runMain(t, "sx", "-C", dir, "-q", "--fallback-local", "--queue-timeout", "5s", "v")
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "sx: ") && !strings.HasPrefix(line, "sx: warning: ") {
			t.Errorf("quiet mode printed %q", line)
		}
	}
	if code != 0 || !strings.Contains(stderr, "sx: warning: ") {
		t.Errorf("quiet mode: exit %d, stderr %q", code, stderr)
	}
	// Without fallback, the same task fails with 125, and parts that never
	// reached a runner don't claim one.
	code, stderr = runMain(t, "sx", "-C", dir, "--queue-timeout", "5s", "v")
	if code != client.ExitInfra || !strings.Contains(stderr, "sx: l · ") || !strings.Contains(stderr, "exit 125") || strings.Contains(stderr, "#0") {
		t.Errorf("without fallback: exit %d, stderr %q", code, stderr)
	}
	assertTail(t, stderr,
		`^sx: l · failed · \S+ · exit 125$`,
		`^sx: t · failed · \S+ · exit 125$`,
		`^sx: v · 2 of 2 parts failed \(l, t\) · \S+ · exit 125$`)
	code, stderr = runMain(t, "sx", "-C", dir, "--json", "--queue-timeout", "5s", "v")
	if parts := resultEventParts(t, stderr); code != client.ExitInfra || len(parts) != 2 {
		t.Errorf("without fallback, --json: exit %d, parts %v", code, parts)
	} else {
		for _, p := range parts {
			if p["status"] != "failed" || p["exit_code"] != float64(125) || p["runner"] != nil || p["local"] != nil || p["shard"] != nil {
				t.Errorf("without fallback, --json: part %v", p)
			}
		}
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
	// The output ends with every part, failed first, then the verdict.
	assertTail(t, stderr,
		`^sx: unit 2/3 · failed · \S+ locally · exit 3$`,
		`^sx: unit 1/3 · passed · \S+ locally$`,
		`^sx: unit 3/3 · passed · \S+ locally$`,
		`^sx: unit · 1 of 3 parts failed \(unit 2/3\) · \S+ · exit 3$`)
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
		t.Fatalf("shards %v, result %v (stderr %q)", shards, result, stderr)
	}
	// It has each part's outcome, in shard order.
	parts, _ := result["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("result parts: %v", result["parts"])
	}
	for i, p := range parts {
		p := p.(map[string]any)
		status, code := "passed", float64(0)
		if i == 1 {
			status, code = "failed", 3
		}
		if p["task"] != "unit" || p["shard"] != float64(i+1) || p["shards"] != float64(3) || p["label"] != fmt.Sprintf("unit %d/3", i+1) ||
			p["status"] != status || p["exit_code"] != code || p["local"] != true || p["runner"] != nil {
			t.Errorf("part %d: %v", i+1, p)
		}
		if d, ok := p["duration_ms"].(float64); !ok || d < 0 {
			t.Errorf("part %d duration: %v", i+1, p["duration_ms"])
		}
	}
	if strings.Contains(stderr, "sx: ") {
		t.Errorf("--json printed a text summary: %q", stderr)
	}
	// Named in a parallel task, a sharded task fans out in place.
	os.Remove(filepath.Join(dir, "s1of3.out"))
	code, stderr = runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "all")
	if code != 3 || !strings.Contains(stderr, "sx: lint · ") || !strings.Contains(stderr, "sx: unit 3/3 · ") {
		t.Errorf("parallel: exit %d, stderr %q", code, stderr)
	}
	assertTail(t, stderr,
		`^sx: unit 2/3 · failed · \S+ locally · exit 3$`,
		`^sx: unit 1/3 · passed · \S+ locally$`,
		`^sx: unit 3/3 · passed · \S+ locally$`,
		`^sx: lint · passed · \S+ locally$`,
		`^sx: all · 1 of 4 parts failed \(unit 2/3\) · \S+ · exit 3$`)
	code, stderr = runMain(t, "sx", "-C", dir, "--json", "--fallback-local", "--queue-timeout", "5s", "all")
	allParts := resultEventParts(t, stderr)
	if code != 3 || len(allParts) != 4 {
		t.Fatalf("parallel --json: exit %d, parts %v", code, allParts)
	}
	for i, want := range []string{"unit 1/3 passed 1", "unit 2/3 failed 2", "unit 3/3 passed 3", "lint passed <nil>"} {
		p := allParts[i]
		if got := fmt.Sprintf("%v %v %v", p["label"], p["status"], p["shard"]); got != want {
			t.Errorf("parallel --json part %d: %v", i, p)
		}
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

// assertTail checks the last lines of stderr against regular expressions.
func assertTail(t *testing.T, stderr string, res ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) < len(res) {
		t.Fatalf("stderr ends before its summary: %q", stderr)
	}
	for i, line := range lines[len(lines)-len(res):] {
		if !regexp.MustCompile(res[i]).MatchString(line) {
			t.Errorf("summary line %d: %q, want %s; stderr %q", i, line, res[i], stderr)
		}
	}
}

// resultEventParts returns the parts of a --json run's closing result
// event, checking that every stderr line is NDJSON.
func resultEventParts(t *testing.T, stderr string) []map[string]any {
	t.Helper()
	var parts []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var ev struct {
			Event string           `json:"event"`
			Parts []map[string]any `json:"parts"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("not NDJSON: %q", line)
		}
		if ev.Event == "result" && ev.Parts != nil {
			parts = ev.Parts
		}
	}
	return parts
}

// partsTask is a sharded task whose shards 1 and 2 say they're running
// (r1, r2) and then wait, and whose shard 3 waits for them before exiting
// with `last`: a stopped part's code then comes before the failure.
func partsTask(t *testing.T, last string, extra string) string {
	dir := project(t, `[tasks.unit]
run = "if [ {shard} = 3 ]; then for i in $(seq 200); do [ -e r1 ] && [ -e r2 ] && break; sleep 0.05; done; `+last+`; fi; touch r{shard}; sleep 20"
shards = 3
`+extra)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestFailFastSummaryNamesStoppedParts(t *testing.T) {
	dir := partsTask(t, "exit 3", "fail_fast = true\n")
	start := time.Now()
	code, stderr := runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "unit")
	if code != 3 || time.Since(start) > 18*time.Second {
		t.Fatalf("exit %d after %s, stderr %q", code, time.Since(start), stderr)
	}
	assertTail(t, stderr,
		`^sx: unit 3/3 · failed · \S+ locally · exit 3$`,
		`^sx: unit 1/3 · stopped \(fail_fast\) · \S+ locally$`,
		`^sx: unit 2/3 · stopped \(fail_fast\) · \S+ locally$`,
		`^sx: unit · 1 of 3 parts failed \(unit 3/3\) · 2 stopped · \S+ · exit 3$`)
}

func TestInterruptedTaskSummary(t *testing.T) {
	// Shard 3 waits too, so all three are running when the interrupt
	// comes; fail_fast must not claim the parts it interrupted.
	dir := partsTask(t, "touch r3; sleep 20", "fail_fast = true\n")
	go func() {
		for i := 0; i < 400; i++ {
			if _, err := os.Stat(filepath.Join(dir, "r3")); err == nil {
				syscall.Kill(os.Getpid(), syscall.SIGINT)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	code, stderr := runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "unit")
	if code != client.ExitInterrupted {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	assertTail(t, stderr,
		`^sx: unit 1/3 · interrupted · \S+ locally · exit 130$`,
		`^sx: unit 2/3 · interrupted · \S+ locally · exit 130$`,
		`^sx: unit 3/3 · interrupted · \S+ locally · exit 130$`,
		`^sx: unit · interrupted · \S+ · exit 130$`)
}

func TestGroupedTaskPrintsEachPartAsOneBlock(t *testing.T) {
	// Each part writes to stderr (which runMain captures), taking turns
	// through marker files, so live output interleaves a1, b1, a2.
	dir := project(t, `[tasks.a]
run = "echo a1 >&2; sleep 0.1; touch a.mark; for i in $(seq 100); do [ -e b.mark ] && break; sleep 0.05; done; echo a2 >&2; echo a3 >&2"
[tasks.b]
run = "for i in $(seq 100); do [ -e a.mark ] && break; sleep 0.05; done; echo b1 >&2; sleep 0.1; touch b.mark; echo b2 >&2; echo b3 >&2; exit 4"
[tasks.v]
parallel = ["a", "b"]
group = true
`)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	code, stderr := runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "v")
	if code != 4 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	// Each block is whole, and its summary follows it at once.
	for _, want := range []string{"[a] a1\n[a] a2\n[a] a3\nsx: a · ", "[b] b1\n[b] b2\n[b] b3\nsx: b · "} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, stderr)
		}
	}
	// --group=false beats the task's setting: live, interleaved lines.
	os.Remove(filepath.Join(dir, "a.mark"))
	os.Remove(filepath.Join(dir, "b.mark"))
	_, stderr = runMain(t, "sx", "-C", dir, "--group=false", "--fallback-local", "--queue-timeout", "5s", "v")
	a1, b1, a2 := strings.Index(stderr, "[a] a1\n"), strings.Index(stderr, "[b] b1\n"), strings.Index(stderr, "[a] a2\n")
	if a1 < 0 || b1 < a1 || a2 < b1 {
		t.Errorf("--group=false still grouped: %q", stderr)
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

// Input piped into sx doesn't reach the command (stdin is /dev/null there
// too), so it is flagged once, whether it was there before the run or only
// written while the command ran; an empty pipe, as an agent harness may
// pass, is not.
func TestPipedStdinIsFlaggedNotForwarded(t *testing.T) {
	// The command signals it is running, then waits (up to 30s) for the
	// input to have been written.
	const handshake = "touch started && for i in $(seq 600); do [ -e release ] && break; sleep 0.05; done"
	dir := project(t, "[tasks.l]\nrun = \"cat > l.out\"\n[tasks.t]\nrun = \"true\"\n[tasks.v]\nparallel = [\"l\", \"t\"]\n"+
		"[tasks.w]\nrun = '"+handshake+"'\n[tasks.vw]\nparallel = [\"w\", \"t\"]\n")
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	pipe := func(data string) *os.File {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	const warning = "stdin has input, but the command doesn't get it"
	flags := []string{"-C", dir, "--fallback-local", "--queue-timeout", "5s"}
	for _, c := range []struct {
		args []string
		out  string
	}{{[]string{"--", "cat > s.out"}, "s.out"}, {[]string{"v"}, "l.out"}} {
		all := append(append([]string{}, flags...), c.args...)
		code, stderr := runMainStdin(t, pipe("x y\n"), "sx", all...)
		if code != 0 {
			t.Fatalf("%v: exit %d, stderr %q", c.args, code, stderr)
		}
		if n := strings.Count(stderr, warning); n != 1 {
			t.Errorf("%v: want one STDIN_IGNORED warning, got %d: %q", c.args, n, stderr)
		}
		if b, err := os.ReadFile(filepath.Join(dir, c.out)); err != nil || len(b) != 0 {
			t.Errorf("%v: the command read stdin: %q, %v", c.args, b, err)
		}
		code, stderr = runMainStdin(t, pipe(""), "sx", all...)
		if code != 0 || strings.Contains(stderr, "stdin") {
			t.Errorf("%v with an empty pipe: exit %d, stderr %q", c.args, code, stderr)
		}
	}
	// Input written only once the command is running, as `git diff | sx`
	// may: seen by the look after the run.
	for _, args := range [][]string{{"--", handshake}, {"vw"}} {
		started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
		_ = os.Remove(started)
		_ = os.Remove(release)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stop, wrote := make(chan struct{}), make(chan bool, 1)
		go func() {
			defer w.Close()
			for {
				select {
				case <-stop:
					wrote <- false
					return
				case <-time.After(10 * time.Millisecond):
				}
				if _, err := os.Stat(started); err == nil {
					_, err := w.Write([]byte("late\n"))
					_ = os.WriteFile(release, nil, 0o644)
					wrote <- err == nil
					return
				}
			}
		}()
		code, stderr := runMainStdin(t, r, "sx", append(append([]string{}, flags...), args...)...)
		close(stop)
		ok := <-wrote
		_ = r.Close()
		if code != 0 || !ok {
			t.Fatalf("%v: exit %d, input written %v, stderr %q", args, code, ok, stderr)
		}
		if n := strings.Count(stderr, warning); n != 1 {
			t.Errorf("%v, input written while it ran: want one STDIN_IGNORED warning, got %d: %q", args, n, stderr)
		}
	}
	code, stderr := runMainStdin(t, pipe("x y\n"), "sx", "-C", dir, "--json", "--fallback-local", "--queue-timeout", "5s", "--", "true")
	if code != 0 || !strings.Contains(stderr, `"code":"STDIN_IGNORED"`) {
		t.Errorf("--json: exit %d, stderr %q", code, stderr)
	}
}

func TestRunLocalWorkspaceWithSpaces(t *testing.T) {
	// Locally SPOTEXEC_WORKSPACE is the worktree itself, here with a space,
	// so a rewritten path must stay one word, and an argument read after a
	// cd must still name the same file.
	l := testLoaded()
	root := filepath.Join(t.TempDir(), "My Projects", "repo")
	l.ws.Root = root
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	for _, args := range [][]string{
		{"cat " + strings.ReplaceAll(root, " ", `\ `) + "/f.txt"},
		{`cat "` + root + `/f.txt"`},
		{"sh", "-c", `cd sub && cat "$1"`, "sh", root + "/f.txt"},
	} {
		var out lockedBuf
		o := client.Options{Command: buildCommand(l.ws, args), Stdout: &out, Stderr: &out}
		if !strings.Contains(o.Command, "SPOTEXEC_WORKSPACE") {
			t.Errorf("%q: path not rewritten in %q", args, o.Command)
		}
		if code := runLocal(context.Background(), nil, l, o, rep, 0, 1); code != 0 || out.String() != "ok\n" {
			t.Errorf("%q ran %q: exit %d, output %q", args, o.Command, code, out.String())
		}
	}
}
