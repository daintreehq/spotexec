// Package cli wires the spotexec command surface.
package cli

import (
	"fmt"
	"os"
	"sort"
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
  sx [flags] -- <command> [args...]    same, when the command shares a name with a subcommand
  sx <task> [args...]                  run a named task from .spotexec.toml
`

// subcommands that take precedence over the shorthand form.
var subcommands = map[string]bool{
	"run": true, "exec": true, "sync": true, "sync-plan": true, "pull": true, "status": true,
	"monitor": true, "up": true, "down": true, "doctor": true, "unlock": true, "prune": true,
	"init": true, "hook": true, "tasks": true, "version": true, "help": true, "cloud-init": true,
	"completion": true, "sim": true, "image": true, "daemon": true, "container": true, "setup": true, "__complete": true, "__completeNoDesc": true,
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
		fs.SetOutput(nopWriter{})
		if err := fs.Parse(args); err == nil {
			rest := fs.Args()
			dashed := fs.ArgsLenAtDash() == 0
			if len(rest) > 0 && (dashed || !subcommands[rest[0]]) {
				return shorthand(&f, rest, dashed)
			}
			if len(rest) == 0 && fs.NFlag() > 0 {
				fmt.Fprintln(os.Stderr, "spotexec: missing command")
				return 2
			}
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

func isHelpFlag(s string) bool {
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

func shorthand(f *runFlags, rest []string, dashed bool) int {
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
	return f.execute(l, runSpec{command: cmd, display: displayOf(rest)})
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
		cmdUp(), cmdDown(), cmdDoctor(), cmdUnlock(), cmdPrune(), cmdInit(), cmdHook(),
		cmdTasks(), cmdVersion(), cmdCloudInit(), cmdSim(), cmdImage(), cmdDaemon(), cmdContainer(), cmdSetup(),
	)
	return root
}

func cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
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
	c.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return infraExit(fmt.Errorf("missing command"))
		}
		return nil
	}
	return c
}

func cmdExec() *cobra.Command {
	var f runFlags
	c := &cobra.Command{
		Use:   "exec [flags] -- <command> [args...]",
		Short: "Run in the last-used slot without syncing first (same as sx --same-slot --sync none)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(f.dir)
			if err != nil {
				return infraExit(err)
			}
			f.sync = "none"
			return exitWith(f.executeLast(l, runSpec{command: buildCommand(l.ws, args), display: displayOf(args), sync: "none"}))
		},
	}
	c.Flags().SetInterspersed(false)
	f.register(c.Flags())
	// A bad flag must not look like the command's own exit 1 or 2.
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return infraExit(err) })
	c.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return infraExit(fmt.Errorf("missing command"))
		}
		return nil
	}
	return c
}

func cmdTasks() *cobra.Command {
	return &cobra.Command{
		Use:   "tasks",
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
				t := l.cfg.Project.Tasks[n]
				if len(t.Parallel) > 0 {
					fmt.Printf("%-16s parallel: %s\n", n, strings.Join(t.Parallel, ", "))
				} else {
					fmt.Printf("%-16s %s\n", n, t.Run)
				}
			}
			return nil
		},
	}
}

// sxMain is `sx [flags] <command...>` / `sx <task> [args...]`.
func sxMain(args []string) int {
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
	case *help || len(rest) == 0 && fs.NFlag() == 0:
		sxHelp(fs, f.dir)
		return 0
	case len(rest) == 0:
		fmt.Fprintln(os.Stderr, "sx: missing command (sx -h for usage)")
		return client.ExitInfra
	}
	return shorthand(&f, rest, fs.ArgsLenAtDash() == 0)
}

func sxHelp(fs *pflag.FlagSet, dir string) {
	fmt.Printf(`sx runs a command from this worktree on a remote runner: it syncs the
worktree, runs the command there, streams its output, and writes the files it
changed back here.

Usage:
  sx [flags] <command> [args...]    sx pnpm test · sx 'pnpm lint && pnpm test'
  sx [flags] <task> [args...]       a named task from .spotexec.toml
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
		fmt.Println("\nTasks in this project:")
		for _, n := range names {
			t := l.cfg.Project.Tasks[n]
			what := t.Run
			if len(t.Parallel) > 0 {
				what = "parallel: " + strings.Join(t.Parallel, ", ")
			}
			fmt.Printf("  %-18s %s\n", n, what)
		}
	}
}

// infraExit prints a spotexec error and exits 125, the code reserved for
// spotexec's own failures on the run path.
func infraExit(err error) error {
	fmt.Fprintln(os.Stderr, "spotexec:", err)
	return exitCode(client.ExitInfra)
}
