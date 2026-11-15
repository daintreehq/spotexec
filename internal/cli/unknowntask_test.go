package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/ui"
)

func TestTaskLikeWord(t *testing.T) {
	cases := []struct {
		rest   []string
		dashed bool
		want   string
	}{
		{[]string{"test-al"}, false, "test-al"},
		{[]string{"lint:fix", "--x"}, false, "lint:fix"},
		{[]string{"test-al"}, true, ""},
		{[]string{"./bin/x"}, false, ""},
		{[]string{"FOO=1", "test"}, false, ""},
		{[]string{"test-al --x"}, false, ""},
		{[]string{"echo $X"}, false, ""},
		{[]string{"-x"}, false, ""},
		{[]string{"tést-al"}, false, "tést-al"},
		{nil, false, ""},
	}
	for _, c := range cases {
		if got := taskLikeWord(c.rest, c.dashed); got != c.want {
			t.Errorf("taskLikeWord(%q, %v) = %q, want %q", c.rest, c.dashed, got, c.want)
		}
	}
}

func TestNotFoundLine(t *testing.T) {
	yes := []string{
		"bash: line 1: test-al: command not found",
		"bash: test-al: command not found\r",
		"/bin/sh: 1: test-al: not found",
		"sh: test-al: not found",
		"zsh:1: command not found: test-al",
		"test-al: command not found",
	}
	for _, l := range yes {
		if !notFoundLine(l, "test-al") {
			t.Errorf("%q not matched", l)
		}
	}
	no := []string{
		"bash: line 1: xtest-al: command not found",
		"bash: line 1: jest: command not found",
		"test-al: file not found here",
		"error: test-al: command not found in PATH, retrying",
	}
	for _, l := range no {
		if notFoundLine(l, "test-al") {
			t.Errorf("%q matched", l)
		}
	}
}

func TestNotFoundWatchAcrossWrites(t *testing.T) {
	var out strings.Builder
	w := newNotFoundWatch(&out, "nosuch")
	for _, chunk := range []string{"profile banner\nbash: line 1: nos", "uch: command ", "not found\n", "tail"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !w.Seen() {
		t.Error("a message split across writes wasn't seen")
	}
	if want := "profile banner\nbash: line 1: nosuch: command not found\ntail"; out.String() != want {
		t.Errorf("output altered: %q", out.String())
	}
	// A not-found message only counts as a whole line.
	w = newNotFoundWatch(io.Discard, "nosuch")
	io.WriteString(w, "nosuch: command not found and more\n")
	io.WriteString(w, strings.Repeat("x", maxWatchedLine+10)+" nosuch: command not found\n")
	io.WriteString(w, strings.Repeat("x", maxWatchedLine-5))
	io.WriteString(w, strings.Repeat("x", 10))
	io.WriteString(w, " nosuch: command not found\n")
	if w.Seen() {
		t.Error("partial or oversized line matched")
	}
	// The line after an oversized one is watched again.
	io.WriteString(w, "bash: line 3: nosuch: command not found\n")
	if !w.Seen() {
		t.Error("a message after an oversized line wasn't seen")
	}
	// The last line counts without its newline.
	w = newNotFoundWatch(io.Discard, "nosuch")
	io.WriteString(w, "sh: nosuch: not found")
	if !w.Seen() {
		t.Error("an unterminated last line wasn't seen")
	}
}

func TestClosestName(t *testing.T) {
	names := []string{"build", "lint", "test", "test-all", "unit"}
	cases := map[string]string{
		"test-al":  "test-all",
		"tset":     "test",
		"Lint":     "lint",
		"buidl":    "build",
		"deploy":   "",
		"go":       "",
		"test-all": "test-all",
		"ts":       "",
		"lnt":      "lint",
		"tst-al":   "test-all", // 2 edits, allowed past 5 characters
		"linxx":    "",         // 2 edits, too many for 5 characters
	}
	for word, want := range cases {
		if got := closestName(word, names); got != want {
			t.Errorf("closestName(%q) = %q, want %q", word, got, want)
		}
	}
	if got := closestName("testx", []string{"testa", "tests"}); got != "" {
		t.Errorf("a tie suggested %q", got)
	}
	if got := closestName("te", []string{"tea"}); got != "" {
		t.Errorf("a two-letter word suggested %q", got)
	}
	if got := closestName("tést-al", []string{"tést-all", "test-all"}); got != "tést-all" {
		t.Errorf("unicode: %q", got)
	}
}

func TestUnknownTaskWarning(t *testing.T) {
	p := &config.Project{Tasks: map[string]*config.Task{"test-all": {Run: "x"}, "lint": {Run: "y"}}}
	if got, want := unknownTaskWarning(p, "test-al", false), `no task is named "test-al", and the shell found no command by that name on the runner; did you mean "test-all"? (spotexec tasks lists the tasks)`; got != want {
		t.Errorf("got %q", got)
	}
	if got := unknownTaskWarning(p, "deploy", true); strings.Contains(got, "did you mean") || !strings.Contains(got, " locally (spotexec tasks lists the tasks)") {
		t.Errorf("no close task: %q", got)
	}
	if got := unknownTaskWarning(&config.Project{}, "deploy", false); !strings.HasSuffix(got, "(this project defines no tasks)") {
		t.Errorf("no tasks: %q", got)
	}
}

// fallbackProject is a project whose only runner refuses connections, so a
// run with --fallback-local runs on this machine.
func fallbackProject(t *testing.T, toml string) string {
	dir := project(t, toml)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestMistypedTaskNameWarns(t *testing.T) {
	dir := fallbackProject(t, "[tasks.test-all]\nrun = \"true\"\n")
	run := func(args ...string) (int, string) {
		return runMain(t, "sx", append([]string{"-C", dir, "--fallback-local", "--queue-timeout", "5s"}, args...)...)
	}
	code, stderr := run("test-al")
	if code != 127 || !strings.Contains(stderr, `sx: warning: no task is named "test-al", and the shell found no command by that name locally; did you mean "test-all"? (spotexec tasks lists the tasks)`) {
		t.Errorf("mistyped task: exit %d, stderr %q", code, stderr)
	}
	// A command that exists but exits 127, or a word after --, says nothing.
	// Nor does a program found on PATH whose own lookup of the same name fails.
	for _, args := range [][]string{{"sh", "-c", "exit 127"}, {"--", "test-al"}, {"sh", "-c", "test-al"}, {"bash", "-c", "PATH=/nonexistent bash"}, {"eval", "exit", "127"}} {
		code, stderr := run(args...)
		if code != 127 || strings.Contains(stderr, "no task is named") {
			t.Errorf("%q: exit %d, stderr %q", args, code, stderr)
		}
	}
	// --json carries it as a warning event with a stable code.
	code, stderr = run("--json", "test-al")
	found := false
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["event"] == "warning" && ev["code"] == "UNKNOWN_TASK" {
			found = strings.Contains(ev["message"].(string), `did you mean "test-all"?`)
		}
	}
	if code != 127 || !found {
		t.Errorf("--json: exit %d, stderr %q", code, stderr)
	}
}

// warnings records what a reporter is told.
type warnings struct {
	ui.Reporter
	codes  []string
	events []string
}

func (w *warnings) Warn(code, format string, args ...any) { w.codes = append(w.codes, code) }
func (w *warnings) Event(name string, _ map[string]any)   { w.events = append(w.events, name) }

func TestWarnUnknownTaskGate(t *testing.T) {
	p := &config.Project{}
	for _, c := range []struct {
		word     string
		code     int
		notFound bool
		want     bool
	}{
		{"test-al", 127, true, true},
		{"test-al", 0, true, false},
		{"test-al", 1, true, false},
		{"test-al", 126, true, false},
		{"test-al", 127, false, false},
		{"", 127, true, false},
	} {
		var w warnings
		warnUnknownTask(&w, p, c.word, c.code, c.notFound, false)
		if got := len(w.codes) == 1 && w.codes[0] == "UNKNOWN_TASK"; got != c.want {
			t.Errorf("%+v: warned %v", c, w.codes)
		}
	}
}

func TestWatchResetsOnRetry(t *testing.T) {
	watch := newNotFoundWatch(io.Discard, "nosuch")
	var inner warnings
	rep := watchReporter{&inner, watch}
	io.WriteString(watch, "bash: line 1: nosuch: command not found\n")
	rep.Event("exec", nil)
	if !watch.Seen() {
		t.Fatal("not seen")
	}
	io.WriteString(watch, "bash: line 1: nos")
	rep.Event("retry", nil)
	if watch.Seen() {
		t.Error("a replayed run kept the earlier attempt's message")
	}
	if strings.Join(inner.events, ",") != "exec,retry" {
		t.Errorf("events not passed on: %v", inner.events)
	}
}

func TestLocalCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l := testLoaded()
	l.ws.Root = root
	// Relative PATH entries resolve against the run's directory, not this
	// process's, and builtins count as commands.
	l.cfg.Project.Env = map[string]string{"PATH": "bin:/usr/bin:/bin"}
	for word, want := range map[string]bool{"tool": true, "eval": true, "sh": true, "data": false, "test-al": false} {
		if got := localCommand(l, client.Options{}, word); got != want {
			t.Errorf("%q: found %v, want %v", word, got, want)
		}
	}
}
