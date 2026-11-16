package cli

import (
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
		{"-C", dir, "status"},
		{"-C", dir, "--json", "help"},
	}
	for _, args := range cases {
		code, stderr := runMain(t, "spotexec", args...)
		if code != client.ExitInfra || !strings.Contains(stderr, "after the subcommand") || !strings.Contains(stderr, "--") {
			t.Errorf("spotexec %v: exit %d, stderr %q", args, code, stderr)
		}
	}
}

// -h after run flags is a request for help, not a failed run.
func TestHelpAfterRunFlags(t *testing.T) {
	dir := project(t, "")
	for _, h := range []string{"-h", "--help"} {
		var code int
		var stderr string
		out := captureStdout(t, func() { code, stderr = runMain(t, "spotexec", "-C", dir, "--json", h) })
		if code != 0 || !strings.Contains(out, "Usage:") {
			t.Errorf("spotexec -C dir --json %s: exit %d, stdout %q, stderr %q", h, code, out, stderr)
		}
	}
}

// A subcommand that takes no positional arguments must reject them, not
// ignore them (spotexec hook install codex installed the Claude hook).
func TestSubcommandsRejectUnexpectedArgs(t *testing.T) {
	dir := project(t, "")
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	cases := [][]string{
		{"version", "extra"},
		{"tasks", "extra"},
		{"sync", "extra"},
		{"sync-plan", "extra"},
		{"init", "my-app"},
		{"hook", "install", "codex"},
		{"daemon", "status", "extra"},
		{"daemon", "stop", "extra"},
	}
	for _, args := range cases {
		code, stderr := runMain(t, "spotexec", args...)
		if code == 0 || !strings.Contains(stderr, "accepts 0 arg") {
			t.Errorf("spotexec %v: exit %d, stderr %q", args, code, stderr)
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
