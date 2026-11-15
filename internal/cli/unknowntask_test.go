package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
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
	}
	for word, want := range cases {
		if got := closestName(word, names); got != want {
			t.Errorf("closestName(%q) = %q, want %q", word, got, want)
		}
	}
	if got := closestName("testx", []string{"testa", "tests"}); got != "" {
		t.Errorf("a tie suggested %q", got)
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
	for _, args := range [][]string{{"sh", "-c", "exit 127"}, {"--", "test-al"}, {"sh", "-c", "test-al"}} {
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
