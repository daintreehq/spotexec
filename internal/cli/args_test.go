package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
)

// Run flags before a subcommand's name must not reach Cobra's root, which
// doesn't know them: it took the subcommand's name as the flag's value and
// ran the next word as the subcommand (spotexec --json run sync-plan ran
// sync-plan).
func TestRunFlagsBeforeASubcommandExit125(t *testing.T) {
	dir := project(t, "[tasks.status]\nrun = \"true\"\n")
	cases := [][]string{
		{"--json", "run", "sync-plan"},
		{"--json", "status", "pull"},
		{"-q", "run", "sync"},
		{"-s", "status"},
		{"--exclusive", "doctor"},
		{"--json=false", "status"},
		{"-C", dir, "status"},
		{"-C" + dir, "version"},
		{"-C", dir, "--json", "help"},
	}
	for _, args := range cases {
		code, stderr := runMain(t, "spotexec", args...)
		if code != client.ExitInfra || !strings.Contains(stderr, "after the subcommand name") || !strings.Contains(stderr, "use -- to run a command named") {
			t.Errorf("spotexec %v: exit %d, stderr %q", args, code, stderr)
		}
	}
}

// -- still runs a command named like a subcommand, and not a task of that
// name either: the unknown runner shows the run got as far as choosing one.
func TestRunFlagsThenDashRunTheCommand(t *testing.T) {
	dir := project(t, "[tasks.status]\nrun = \"true\"\n")
	code, stderr := runMain(t, "spotexec", "-C", dir, "-r", "nope", "--", "status")
	if code != client.ExitInfra || !strings.Contains(stderr, `unknown runner "nope"`) {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

// -h after run flags is a request for help, not a failed run, and it is
// spotexec -h's help: the directory isn't even read.
func TestHelpAfterRunFlags(t *testing.T) {
	want := captureStdout(t, func() { runMain(t, "spotexec", "-h") })
	for _, h := range []string{"-h", "--help"} {
		var code int
		var stderr string
		out := captureStdout(t, func() {
			code, stderr = runMain(t, "spotexec", "-C", filepath.Join(t.TempDir(), "missing"), "--json", h)
		})
		if code != 0 || stderr != "" || out != want || !strings.Contains(out, "Usage:") {
			t.Errorf("spotexec -C missing --json %s: exit %d, stdout %q, stderr %q", h, code, out, stderr)
		}
	}
}

// A subcommand that takes no positional arguments must reject them, not
// ignore them (spotexec hook install codex installed the Claude hook, and
// spotexec init my-app wrote .spotexec.toml here).
func TestSubcommandsRejectUnexpectedArgs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	cases := [][]string{
		{"version", "extra"},
		{"tasks", "extra"},
		{"sync", "extra"},
		{"sync-plan", "extra"},
		{"init", "my-app"},
		{"hook", "install", "codex"},
		{"daemon", "run", "extra"},
		{"daemon", "start", "extra"},
		{"daemon", "status", "extra"},
		{"daemon", "stop", "extra"},
	}
	for _, args := range cases {
		code, stderr := runMain(t, "spotexec", args...)
		path := "spotexec " + strings.Join(args[:len(args)-1], " ")
		if code == 0 || !strings.Contains(stderr, path+` takes no arguments, got "`+args[len(args)-1]+`"`) {
			t.Errorf("spotexec %v: exit %d, stderr %q", args, code, stderr)
		}
	}
	for _, f := range []string{".spotexec.toml", ".claude", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("a rejected command wrote %s", f)
		}
	}
}

// The commands that do take positional arguments keep their counts.
func TestSubcommandArgCounts(t *testing.T) {
	root := newRoot()
	cases := []struct {
		path []string
		n    int
		ok   bool
	}{
		{[]string{"sim", "interrupt"}, 2, true},
		{[]string{"sim", "interrupt"}, 3, false},
		{[]string{"sim", "rebalance"}, 2, true},
		{[]string{"sim", "rebalance"}, 3, false},
		{[]string{"sim", "build"}, 1, true},
		{[]string{"sim", "build"}, 2, false},
		{[]string{"status"}, 3, true},
		{[]string{"monitor"}, 3, true},
		{[]string{"down"}, 3, true},
		{[]string{"doctor"}, 3, true},
		{[]string{"prune"}, 3, true},
		{[]string{"backups", "clear"}, 3, true},
		{[]string{"hook", "claude"}, 1, true},
	}
	for _, c := range cases {
		cmd, _, err := root.Find(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ValidateArgs(make([]string, c.n)); (err == nil) != c.ok {
			t.Errorf("%s with %d args: %v", cmd.CommandPath(), c.n, err)
		}
	}
}

// Every runnable command states which positional arguments it takes:
// Cobra's default for a command without subcommands accepts any, and so
// silently ignores the ones it never reads.
func TestEveryCommandChecksItsArgs(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if s.Runnable() && s.Args == nil {
				t.Errorf("%q has no Args check", s.CommandPath())
			}
			walk(s)
		}
	}
	walk(newRoot())
}
