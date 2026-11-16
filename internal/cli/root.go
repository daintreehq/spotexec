// Package cli wires the spotexec command surface.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/helper"
)

// Version is set at build time.
var Version = "dev"

const shortUsage = `spotexec (sx) runs commands from your worktree on remote hardware.

Usage:
  sx [flags] <command> [args...]       run a command remotely (sync + exec + write-back)
  sx [flags] <task> [args...]          run a named task from .spotexec.toml
  sx [flags] -- <command> [args...]    run the command even if a task has its name
  spotexec run [flags] -- <command>    the same as sx -- <command>

sx -h lists the run flags and this project's tasks. spotexec <command> runs a
command too, unless it is named like one of the commands below.
`

// subcommands that take precedence over the shorthand form.
var subcommands = map[string]bool{
	"run": true, "exec": true, "sync": true, "sync-plan": true, "pull": true, "status": true,
	"monitor": true, "up": true, "down": true, "doctor": true, "unlock": true, "prune": true, "backups": true,
	"init": true, "hook": true, "tasks": true, "version": true, "help": true, "cloud-init": true,
	"completion": true, "sim": true, "image": true, "prices": true, "daemon": true, "container": true, "setup": true, "teardown": true, "supported": true, "__complete": true, "__completeNoDesc": true,
}

// Main is the entry point for `spotexec`; it returns the process exit code.
func Main(args []string) int { return MainAs("spotexec", args) }

// MainAs runs as the named binary. As `sx` it only runs things: every word
// is a command or a task, so none is shadowed by a subcommand, and spotexec's
// own failures always exit 125.
func MainAs(name string, args []string) int {
	helper.Version = Version
	if name == "sx" {
		return sxMain(args)
	}
	// Shorthand: `sx [run flags] cmd...` and `sx <task>`.
	if len(args) > 0 && !isHelpFlag(args[0]) {
		var f runFlags
		fs := pflag.NewFlagSet("spotexec", pflag.ContinueOnError)
		fs.SetInterspersed(false)
		fs.Usage = func() {}
		f.register(fs)
		// -h after run flags asks for the help, as spotexec -h prints it;
		// a real flag, so --help=false is no request and --help=x an error.
		help := fs.BoolP("help", "h", false, "")
		fs.SetOutput(nopWriter{})
		// Only run flags come before the first word here (subcommands take
		// their flags after their name), so a parse failure is a mistake in
		// a run: like sx and spotexec run, it exits 125, not Cobra's 1.
		if err := fs.Parse(args); err != nil {
			fmt.Fprintf(os.Stderr, "spotexec: %v (spotexec run -h lists the run flags)\n", err)
			return client.ExitInfra
		}
		rest := fs.Args()
		dashed := fs.ArgsLenAtDash() == 0
		runFlagsGiven := fs.NFlag() > 0 && !(fs.NFlag() == 1 && fs.Changed("help"))
		switch {
		case *help:
			args = []string{"--help"}
		case len(rest) > 0 && (dashed || !subcommands[rest[0]]):
			return shorthand(&f, rest, dashed)
		case len(rest) == 0 && (runFlagsGiven || dashed):
			fmt.Fprintln(os.Stderr, "spotexec: missing command")
			return client.ExitInfra
		case runFlagsGiven:
			// Cobra's root doesn't know the run flags: it would take the
			// subcommand's name as a flag's value and run the next word as
			// the subcommand (spotexec --json run sync-plan ran sync-plan).
			fmt.Fprintf(os.Stderr, "spotexec: run flags can't come before the %[1]s subcommand: put its flags after the subcommand name (spotexec %[1]s [flags] ...), or use -- to run a command named %[1]s (spotexec [run flags] -- %[1]s ...)\n", rest[0])
			return client.ExitInfra
		}
	}
	root := newRoot()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		if ec, ok := err.(exitCode); ok {
			return int(ec)
		}
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return 1
	}
	return 0
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// isHelpFlag reports a first word that asks for the help or the version.
// --help=false asks for neither: the run flags after it are parsed as run
// flags, not handed to Cobra's root, which doesn't know them.
func isHelpFlag(s string) bool {
	s, v, valued := strings.Cut(s, "=") // --help=true too
	if on, err := strconv.ParseBool(v); s == "--help" && valued && (err != nil || !on) {
		return false
	}
	return s == "-h" || s == "--help" || s == "--version" || s == "-v"
}

// exitCode lets RunE propagate a command's exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func exitWith(code int) error {
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

// checkCommand rejects a run with no command, or only blank words: an empty
// remote shell line would run nothing and exit 0, which reads as a pass
// (sx "$CMD" with CMD unset).
func checkCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("missing command")
	}
	for _, a := range args {
		if strings.TrimSpace(a) != "" {
			return nil
		}
	}
	return errors.New("empty command")
}

func shorthand(f *runFlags, rest []string, dashed bool) int {
	if err := checkCommand(rest); err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	l, err := load(f.dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	if !dashed {
		if _, ok := l.cfg.Project.Tasks[rest[0]]; ok {
			return f.runTask(l, rest[0], rest[1:])
		}
	}
	cmd := buildCommand(l.ws, rest)
	return f.execute(l, runSpec{command: cmd, display: displayOf(rest), taskLike: taskLikeWord(rest, dashed)})
}

func displayOf(args []string) string {
	s := strings.Join(args, " ")
	if len(s) > 120 {
		s = s[:119] + "…"
	}
	return s
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "spotexec",
		Short:         "Offload tests, linters and builds to remote runners with safe write-back",
		Long:          shortUsage,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		cmdRun(), cmdExec(), cmdSync(), cmdSyncPlan(), cmdPull(), cmdStatus(), cmdMonitor(),
		cmdUp(), cmdDown(), cmdDoctor(), cmdUnlock(), cmdPrune(), cmdBackups(), cmdInit(), cmdHook(),
		cmdTasks(), cmdVersion(), cmdCloudInit(), cmdSim(), cmdImage(), cmdPrices(), cmdDaemon(), cmdContainer(), cmdSetup(), cmdTeardown(), cmdSupported(),
	)
	root.SetVersionTemplate("spotexec {{.Version}}\n") // as `spotexec version` and `sx --version`
	addSxCompletion(root)
	return root
}

func cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Args:  noArgs,
		Short: "Print version",
		Run:   func(*cobra.Command, []string) { fmt.Println("spotexec", Version) },
	}
}

func cmdRun() *cobra.Command {
	var f runFlags
	c := &cobra.Command{
		Use:   "run [flags] -- <command> [args...]",
		Short: "Sync the worktree, run a command remotely, and write back mutated files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(f.dir)
			if err != nil {
				return infraExit(err)
			}
			return exitWith(f.execute(l, runSpec{command: buildCommand(l.ws, args), display: displayOf(args)}))
		},
	}
	c.Flags().SetInterspersed(false)
	f.register(c.Flags())
	// A bad flag must not look like the command's own exit 1 or 2.
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return infraExit(err) })
	c.Args = commandArgs
	return c
}

// noArgs is the argument check of a command that takes none: one it would
// ignore is a mistake (spotexec hook install codex meant --codex).
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%[1]s takes no arguments, got %[2]q (%[1]s -h lists its flags)", cmd.CommandPath(), args[0])
	}
	return nil
}

// commandArgs is run's and exec's argument check: a command that isn't
// empty, or exit 125.
func commandArgs(_ *cobra.Command, args []string) error {
	if err := checkCommand(args); err != nil {
		return infraExit(err)
	}
	return nil
}

func cmdExec() *cobra.Command {
	var f runFlags
	c := &cobra.Command{
		Use:   "exec [flags] -- <command> [args...]",
		Short: "Run in the last-used slot without syncing first (same as sx --same-slot --sync none --)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(f.dir)
			if err != nil {
				return infraExit(err)
			}
			return exitWith(f.executeLast(l, runSpec{command: buildCommand(l.ws, args), display: displayOf(args), sync: "none"}))
		},
	}
	c.Flags().SetInterspersed(false)
	f.register(c.Flags())
	// A bad flag must not look like the command's own exit 1 or 2.
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return infraExit(err) })
	c.Args = commandArgs
	return c
}

func cmdTasks() *cobra.Command {
	return &cobra.Command{
		Use:   "tasks",
		Args:  noArgs,
		Short: "List named tasks from .spotexec.toml",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load("")
			if err != nil {
				return err
			}
			names := l.cfg.Project.TaskNames()
			if len(names) == 0 {
				fmt.Println("no tasks defined (add [tasks.<name>] to .spotexec.toml, or run `spotexec init`)")
				return nil
			}
			for _, n := range names {
				fmt.Printf("%-16s %s\n", n, l.cfg.Project.Tasks[n].Describe())
			}
			return nil
		},
	}
}

// sxMain is `sx [flags] <command...>` / `sx <task> [args...]`.
func sxMain(args []string) int {
	// v0.2.0 could install the Claude Code hook as `sx hook claude`. That
	// line is the hook handler, never a command to run remotely.
	if len(args) == 2 && args[0] == "hook" && args[1] == "claude" {
		return claudeHook("")
	}
	// Shell completion scripts for sx ask it for candidates.
	if len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd) {
		return sxComplete(args)
	}
	var f runFlags
	fs := pflag.NewFlagSet("sx", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	fs.Usage = func() {}
	fs.SetOutput(nopWriter{})
	f.register(fs)
	help := fs.BoolP("help", "h", false, "show this help")
	version := fs.Bool("version", false, "print the version")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "sx: %v (sx -h lists the flags)\n", err)
		return client.ExitInfra
	}
	rest := fs.Args()
	switch {
	case *version:
		fmt.Println("spotexec", Version)
		return 0
	case *help:
		sxHelp(os.Stdout, fs, f.dir)
		return 0
	case len(args) == 0:
		// No command is a usage error, even with the help printed: a
		// script whose command came out empty must not see a pass.
		sxHelp(os.Stderr, fs, f.dir)
		return client.ExitInfra
	case len(rest) == 0:
		fmt.Fprintln(os.Stderr, "sx: missing command (sx -h for usage)")
		return client.ExitInfra
	}
	return shorthand(&f, rest, fs.ArgsLenAtDash() == 0)
}

func sxHelp(w io.Writer, fs *pflag.FlagSet, dir string) {
	fmt.Fprintf(w, `sx runs a command from this worktree on a remote runner: it syncs the
worktree, runs the command there, streams its output, and writes the files it
changed back here.

Usage:
  sx [flags] <command> [args...]    sx pnpm test · sx 'pnpm lint && pnpm test'
  sx [flags] <task> [args...]       a named task from .spotexec.toml
  sx [flags] -- <command> [args...] the command, even if a task has its name
  sx -s <command>                   again on the same VM and slot as last time

Flags:
%s
Exit codes are the command's own; 124 is a timeout and 125 means spotexec
itself failed. Set up and manage runners with spotexec (setup, init, status,
doctor, down, …); see spotexec -h.
`, fs.FlagUsages())
	if l, err := load(dir); err == nil && len(l.cfg.Project.Tasks) > 0 {
		names := make([]string, 0, len(l.cfg.Project.Tasks))
		for n := range l.cfg.Project.Tasks {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(w, "\nTasks in this project:")
		for _, n := range names {
			fmt.Fprintf(w, "  %-18s %s\n", n, l.cfg.Project.Tasks[n].Describe())
		}
	}
}

// infraExit prints a spotexec error and exits 125, the code reserved for
// spotexec's own failures on the run path.
func infraExit(err error) error {
	fmt.Fprintln(os.Stderr, "spotexec:", err)
	return exitCode(client.ExitInfra)
}
