// Package cli wires the spotexec command surface.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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

// Main is the entry point; it returns the process exit code.
func Main(args []string) int {
	helper.Version = Version
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
		return 2
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
				return err
			}
			return exitWith(f.execute(l, runSpec{command: buildCommand(l.ws, args), display: displayOf(args)}))
		},
	}
	c.Flags().SetInterspersed(false)
	f.register(c.Flags())
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
				return err
			}
			f.sync = "none"
			return exitWith(f.executeLast(l, runSpec{command: buildCommand(l.ws, args), display: displayOf(args), sync: "none"}))
		},
	}
	c.Flags().SetInterspersed(false)
	f.register(c.Flags())
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
