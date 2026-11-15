package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
)

// parsed returns run flags as if given on the command line, with the
// SPOTEXEC_* environment applied.
func parsed(t *testing.T, args ...string) *runFlags {
	t.Helper()
	var f runFlags
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	f.applyEnv()
	return &f
}

func testLoaded() *loaded {
	p, _ := config.LoadProject("")
	return &loaded{cfg: &config.Config{Global: &config.Global{}, Project: p}, ws: &client.Workspace{Root: "/w", Cwd: "."}}
}

func TestFlagsBeatEnv(t *testing.T) {
	t.Setenv("SPOTEXEC_JSON", "1")
	t.Setenv("SPOTEXEC_PRIORITY", "100")
	t.Setenv("SPOTEXEC_RUNNER", "envrunner")
	t.Setenv("SPOTEXEC_SYNC", "in")
	t.Setenv("SPOTEXEC_TIMEOUT", "9m")
	l := testLoaded()
	task := runSpec{command: "x", priority: 5}

	// Without flags the environment applies.
	f := parsed(t)
	o := f.options(l, task, nil, nil, nil, nil)
	if !f.json || o.Priority != 100 || !reflect.DeepEqual(o.Pool, []string{"envrunner"}) || o.Sync != "in" || o.Timeout.String() != "9m0s" {
		t.Fatalf("env not applied: json=%v %+v", f.json, o)
	}

	// Explicit flags win, whatever their value.
	f = parsed(t, "--json=false", "--priority", "0", "--pool", "a,b", "--sync", "auto", "--timeout", "1m")
	o = f.options(l, task, nil, nil, nil, nil)
	if f.json {
		t.Error("SPOTEXEC_JSON=1 beat --json=false")
	}
	t.Setenv("SPOTEXEC_AGENT", "1")
	if rep, err := parsed(t, "--json=false").reporter(); err != nil || rep.IsJSON() {
		t.Error("SPOTEXEC_AGENT=1 beat --json=false")
	}
	if rep, err := parsed(t).reporter(); err != nil || !rep.IsJSON() {
		t.Error("SPOTEXEC_AGENT=1 ignored")
	}
	if o.Priority != 0 {
		t.Errorf("--priority 0 lost to env or task priority: %d", o.Priority)
	}
	if !reflect.DeepEqual(o.Pool, []string{"a", "b"}) {
		t.Errorf("SPOTEXEC_RUNNER beat --pool: %v", o.Pool)
	}
	if o.Sync != "auto" || o.Timeout.String() != "1m0s" {
		t.Errorf("env beat --sync/--timeout: %+v", o)
	}
	if f := parsed(t, "-r", "mine"); f.runner != "mine" {
		t.Errorf("SPOTEXEC_RUNNER beat -r: %s", f.runner)
	}
	// --priority 0 resets a task's priority even with no environment.
	t.Setenv("SPOTEXEC_PRIORITY", "")
	if o := parsed(t, "--priority", "0").options(l, task, nil, nil, nil, nil); o.Priority != 0 {
		t.Errorf("--priority 0 didn't reset the task priority: %d", o.Priority)
	}
	if o := parsed(t).options(l, task, nil, nil, nil, nil); o.Priority != 5 {
		t.Errorf("task priority lost: %d", o.Priority)
	}
}

func TestFailOnConflictOptIn(t *testing.T) {
	l := testLoaded()
	t.Setenv("SPOTEXEC_FAIL_ON_CONFLICT", "")
	if o := parsed(t).options(l, runSpec{command: "x"}, nil, nil, nil, nil); o.FailOnConflict {
		t.Error("on by default")
	}
	if o := parsed(t, "--fail-on-conflict").options(l, runSpec{command: "x"}, nil, nil, nil, nil); !o.FailOnConflict {
		t.Error("--fail-on-conflict ignored")
	}
	t.Setenv("SPOTEXEC_FAIL_ON_CONFLICT", "1")
	if o := parsed(t).options(l, runSpec{command: "x"}, nil, nil, nil, nil); !o.FailOnConflict {
		t.Error("SPOTEXEC_FAIL_ON_CONFLICT=1 ignored")
	}
	if o := parsed(t, "--fail-on-conflict=false").options(l, runSpec{command: "x"}, nil, nil, nil, nil); o.FailOnConflict {
		t.Error("SPOTEXEC_FAIL_ON_CONFLICT=1 beat --fail-on-conflict=false")
	}
	t.Setenv("SPOTEXEC_FAIL_ON_CONFLICT", "0")
	if o := parsed(t).options(l, runSpec{command: "x"}, nil, nil, nil, nil); o.FailOnConflict {
		t.Error("SPOTEXEC_FAIL_ON_CONFLICT=0 turned it on")
	}
}

func TestSyncAndExecPinTheirSyncMode(t *testing.T) {
	l := testLoaded()
	t.Setenv("SPOTEXEC_SYNC", "none")
	if o := parsed(t).options(l, runSpec{syncOnly: true, sync: "in"}, nil, nil, nil, nil); o.Sync != "in" {
		t.Errorf("SPOTEXEC_SYNC=none turned spotexec sync into %q", o.Sync)
	}
	t.Setenv("SPOTEXEC_SYNC", "auto")
	if o := parsed(t).options(l, runSpec{preferLast: true, sync: "none"}, nil, nil, nil, nil); o.Sync != "none" {
		t.Errorf("SPOTEXEC_SYNC=auto made exec sync: %q", o.Sync)
	}
	if err := parsed(t, "--sync", "none").check(runSpec{syncOnly: true, sync: "in"}); err == nil {
		t.Error("spotexec sync --sync none accepted")
	}
}

func TestValidateRejectsUnknownValues(t *testing.T) {
	for _, args := range [][]string{{"--writeback", "nevre"}, {"--sync", "bogus"}, {"--writeback="}, {"--sync="}, {"--runner="}, {"--pool="}} {
		if err := parsed(t, args...).validate(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	t.Setenv("SPOTEXEC_SYNC", "bogus")
	if err := parsed(t).validate(); err == nil {
		t.Error("SPOTEXEC_SYNC=bogus accepted")
	}
	t.Setenv("SPOTEXEC_SYNC", "")
	for _, args := range [][]string{{"--writeback", "on-success", "--sync", "in"}, {"--writeback", "never"}, {}} {
		if err := parsed(t, args...).validate(); err != nil {
			t.Errorf("%v rejected: %v", args, err)
		}
	}
}

// project makes a project directory with a global config of two runners
// nobody can reach, and isolated state.
func project(t *testing.T, toml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	global := "version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@192.0.2.1\"]\n[runners.b]\ntype = \"ssh\"\nhosts = [\"nobody@192.0.2.2\"]\n"
	if err := os.WriteFile(cfg, []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	for _, k := range []string{"SPOTEXEC_SYNC", "SPOTEXEC_RUNNER", "SPOTEXEC_POOL", "SPOTEXEC_JSON", "SPOTEXEC_AGENT", "SPOTEXEC_SAME_SLOT", "SPOTEXEC_PRIORITY", "SPOTEXEC_TIMEOUT", "SPOTEXEC_FAIL_ON_CONFLICT"} {
		t.Setenv(k, "")
	}
	return dir
}

// runMain runs a CLI entry point and returns its exit code and stderr.
// Whatever the test binary was given as stdin, the run sees none, so no
// STDIN_IGNORED warning depends on how the tests were started.
func runMain(t *testing.T, name string, args ...string) (int, string) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	return runMainStdin(t, null, name, args...)
}

// runMainStdin is runMain with stdin as the CLI's stdin.
func runMainStdin(t *testing.T, stdin *os.File, name string, args ...string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old, oldIn := os.Stderr, os.Stdin
	os.Stderr, os.Stdin = w, stdin
	out := make(chan string)
	go func() { b, _ := io.ReadAll(r); out <- string(b) }()
	code := MainAs(name, args)
	os.Stderr, os.Stdin = old, oldIn
	w.Close()
	return code, <-out
}

func TestBadRunValuesExit125BeforeAnyRunner(t *testing.T) {
	dir := project(t, "[tasks.lint]\nrun = \"true\"\n[tasks.test]\nrun = \"true\"\n[tasks.verify]\nparallel = [\"lint\", \"test\"]\n")
	t.Setenv("SPOTEXEC_POOL", "b") // --pool= must not fall back to it
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"sx", []string{"-C", dir, "--writeback", "nevre", "rm", "important.txt"}, "--writeback must be"},
		{"spotexec", []string{"-C", dir, "--writeback", "nevre", "rm", "important.txt"}, "--writeback must be"},
		{"spotexec", []string{"run", "-C", dir, "--writeback", "nevre", "--", "rm", "important.txt"}, "--writeback must be"},
		{"sx", []string{"-C", dir, "--sync", "bogus", "true"}, "--sync must be"},
		{"sx", []string{"-C", dir, "--writeback", "nevre", "verify"}, "--writeback must be"},
		{"sx", []string{"-C", dir, "--sync", "bogus", "lint"}, "--sync must be"},
		{"sx", []string{"-C", dir, "--writeback=", "rm", "important.txt"}, "--writeback needs a value"},
		{"sx", []string{"-C", dir, "--writeback=", "verify"}, "--writeback needs a value"},
		{"sx", []string{"-C", dir, "--pool=", "true"}, "--pool needs a value"},
	}
	for _, c := range cases {
		code, stderr := runMain(t, c.name, c.args...)
		if code != client.ExitInfra || !strings.Contains(stderr, c.want) {
			t.Errorf("%s %v: exit %d, stderr %q", c.name, c.args, code, stderr)
		}
	}
}

func TestSameSlotRejectsAnotherRunner(t *testing.T) {
	dir := project(t, "")
	ws, err := client.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(config.StateDir(), "worktrees", ws.WorktreeKey+".json")
	b, _ := json.Marshal(client.LastRun{Runner: "a", Slot: 1})
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", dir, "-s", "-r", "b", "true"},
		{"-C", dir, "-s", "--pool", "b", "true"},
	} {
		code, stderr := runMain(t, "sx", args...)
		if code != client.ExitInfra || !strings.Contains(stderr, "last ran on a,") {
			t.Errorf("sx %v: exit %d, stderr %q", args, code, stderr)
		}
	}
	code, stderr := runMain(t, "spotexec", "exec", "-C", dir, "-r", "b", "--", "true")
	if code != client.ExitInfra || !strings.Contains(stderr, "last ran on a,") {
		t.Errorf("exec -r b: exit %d, stderr %q", code, stderr)
	}
	// Only explicit flags constrain where --same-slot may return.
	l := testLoaded()
	t.Setenv("SPOTEXEC_RUNNER", "b")
	if o := parsed(t, "-s").options(l, runSpec{command: "true"}, nil, nil, nil, nil); o.LastWithin != nil {
		t.Errorf("SPOTEXEC_RUNNER constrained --same-slot: %v", o.LastWithin)
	}
	if o := parsed(t, "-s", "--pool", "b,a").options(l, runSpec{command: "true"}, nil, nil, nil, nil); !reflect.DeepEqual(o.LastWithin, []string{"b", "a"}) {
		t.Errorf("--pool b,a: %v", o.LastWithin)
	}
}

// A project pool built on a runner this machine lacks doesn't stop exec or
// --same-slot from returning to the last runner (#66).
func TestSameSlotIgnoresUnavailableProjectPool(t *testing.T) {
	dir := project(t, "version = 1\npool = [\"aws-big\"]\n[runners.aws-big]\nextends = \"aws\"\n")
	ws, err := client.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(config.StateDir(), "worktrees", ws.WorktreeKey+".json")
	b, _ := json.Marshal(client.LastRun{Runner: "gone", Slot: 1})
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// The remembered runner is looked up (and, here, missing), so the
	// unavailable pool was not what stopped it.
	code, stderr := runMain(t, "spotexec", "exec", "-C", dir, "--", "true")
	if code != client.ExitInfra || !strings.Contains(stderr, `unknown runner "gone"`) {
		t.Errorf("exec: exit %d, stderr %q", code, stderr)
	}
	code, stderr = runMain(t, "sx", "-C", dir, "-s", "true")
	if code != client.ExitInfra || !strings.Contains(stderr, `unknown runner "gone"`) {
		t.Errorf("--same-slot: exit %d, stderr %q", code, stderr)
	}
	// An explicit pool must still be valid, even with the last runner in it.
	code, stderr = runMain(t, "spotexec", "exec", "-C", dir, "--pool", "typo,gone", "--", "true")
	if code != client.ExitInfra || !strings.Contains(stderr, `unknown runner "typo"`) {
		t.Errorf("exec --pool typo,gone: exit %d, stderr %q", code, stderr)
	}
	// An ordinary run selects the pool and says what's missing.
	code, stderr = runMain(t, "sx", "-C", dir, "true")
	if code != client.ExitInfra || !strings.Contains(stderr, `"aws-big"`) || !strings.Contains(stderr, `extends "aws"`) {
		t.Errorf("run: exit %d, stderr %q", code, stderr)
	}
}

func TestUsageAndConfigFailuresExit125(t *testing.T) {
	bad := project(t, "[sync]\nmode = \"sideways\"\n")
	cases := [][]string{
		{"sx", "--timeout", "invalid", "go", "test"},
		{"spotexec", "--timeout", "invalid", "go", "test"},
		{"spotexec", "run", "--timeout", "invalid", "--", "go", "test"},
		{"sx", "--timeout", "1s"},
		{"spotexec", "--timeout", "1s"},
		{"spotexec", "run", "--timeout", "1s"},
		{"sx", "--no-such-flag", "go", "test"},
		{"spotexec", "--no-such-flag", "go", "test"},
		{"spotexec", "run", "--no-such-flag", "--", "go", "test"},
		{"sx", "-C", bad, "true"},
		{"spotexec", "-C", bad, "true"},
		{"spotexec", "run", "-C", bad, "--", "true"},
	}
	for _, c := range cases {
		if code, stderr := runMain(t, c[0], c[1:]...); code != client.ExitInfra {
			t.Errorf("%v: exit %d, want 125 (stderr %q)", c, code, stderr)
		}
	}
}

func TestHelpFormsStillWork(t *testing.T) {
	for _, a := range []string{"-h", "--help", "--help=true", "--version", "--version=true"} {
		if code, stderr := runMain(t, "spotexec", a); code != 0 {
			t.Errorf("spotexec %s: exit %d (stderr %q)", a, code, stderr)
		}
	}
}

func TestSxRunsTasksNamedLikeSubcommands(t *testing.T) {
	dir := project(t, "[tasks.lint]\nrun = \"true\"\n[tasks.sync]\nrun = \"true\"\n[tasks.status]\nparallel = [\"lint\", \"sync\"]\n")
	// The parallel task refuses extra arguments: proof that sx dispatched
	// to the task rather than to anything else called status.
	code, stderr := runMain(t, "sx", "-C", dir, "status", "x")
	if code != client.ExitInfra || !strings.Contains(stderr, "status is a parallel task") {
		t.Errorf("sx status x: exit %d, stderr %q", code, stderr)
	}
}

func TestGroupedOutputPrecedence(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		args  []string
		task  *bool
		agent bool
		want  bool
	}{
		{nil, nil, false, false}, // live output by default
		{nil, nil, true, true},   // agents get blocks
		{nil, &no, true, false},  // the task's setting beats the agent default
		{nil, &yes, false, true}, //
		{[]string{"--group"}, &no, false, true},
		{[]string{"--group=false"}, &yes, true, false}, // the flag beats both
	} {
		if got := parsed(t, c.args...).grouped(&config.Task{Group: c.task}, c.agent); got != c.want {
			t.Errorf("%v task=%v agent=%v: got %v", c.args, c.task, c.agent, got)
		}
	}
}
