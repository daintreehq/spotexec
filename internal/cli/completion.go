package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// sxCompletionRoot describes sx to cobra for shell completion only: sx
// parses its own arguments (sxMain), but the completion scripts call
// `sx __complete <words>`, which sxMain hands to this command.
func sxCompletionRoot() *cobra.Command {
	var f runFlags
	c := &cobra.Command{
		Use:           "sx [flags] <command|task> [args...]",
		Short:         "Run a command from this worktree on a remote runner",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		Run:           func(*cobra.Command, []string) {},
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
			if len(args) > 0 {
				// The command's own arguments: files.
				return nil, cobra.ShellCompDirectiveDefault
			}
			// First word: this project's tasks, else files (commands on
			// PATH are the shell's business).
			return taskCompletions(f.dir, toComplete), cobra.ShellCompDirectiveDefault
		},
	}
	c.CompletionOptions.DisableDefaultCmd = true
	// Flags end at the first word: everything after belongs to the command.
	c.Flags().SetInterspersed(false)
	f.register(c.Flags())
	c.Flags().Bool("version", false, "print the version")
	registerRunFlagCompletions(c, &f)
	return c
}

// taskCompletions lists the project's task names starting with prefix, each
// with what it runs.
func taskCompletions(dir, prefix string) []cobra.Completion {
	l, err := load(dir)
	if err != nil {
		return nil
	}
	var out []cobra.Completion
	for _, n := range l.cfg.Project.TaskNames() {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		t := l.cfg.Project.Tasks[n]
		what := t.Run
		if len(t.Parallel) > 0 {
			what = "parallel: " + strings.Join(t.Parallel, ", ")
		}
		out = append(out, cobra.CompletionWithDesc(n, "task: "+strings.Join(strings.Fields(what), " ")))
	}
	return out
}

// registerRunFlagCompletions completes the values of run flags that take a
// fixed set or a runner name.
func registerRunFlagCompletions(c *cobra.Command, f *runFlags) {
	fixed := func(vals ...string) cobra.CompletionFunc {
		return cobra.FixedCompletions(vals, cobra.ShellCompDirectiveNoFileComp)
	}
	runners := func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		l, err := load(f.dir)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return l.cfg.RunnerNames(), cobra.ShellCompDirectiveNoFileComp
	}
	for name, fn := range map[string]cobra.CompletionFunc{
		"sync":      fixed("auto", "in", "none"),
		"writeback": fixed("always", "on-success", "never"),
		"runner":    runners,
		"pool":      runners,
	} {
		if c.Flags().Lookup(name) != nil {
			_ = c.RegisterFlagCompletionFunc(name, fn)
		}
	}
	if c.Flags().Lookup("dir") != nil {
		_ = c.MarkFlagDirname("dir")
	}
}

// cmdSxCompletion is `spotexec completion sx <shell>`: the completion script
// for the sx command.
func cmdSxCompletion() *cobra.Command {
	return &cobra.Command{
		Use:       "sx {bash|zsh|fish|powershell}",
		Short:     "Generate the autocompletion script for sx (its flags and this project's tasks)",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			sx, out := sxCompletionRoot(), cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return sx.GenBashCompletionV2(out, true)
			case "zsh":
				return sx.GenZshCompletion(out)
			case "fish":
				return sx.GenFishCompletion(out, true)
			case "powershell":
				return sx.GenPowerShellCompletionWithDesc(out)
			}
			return fmt.Errorf("unsupported shell %q (bash, zsh, fish or powershell)", args[0])
		},
	}
}

// addSxCompletion hangs `sx` under spotexec's default completion command.
func addSxCompletion(root *cobra.Command) {
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			c.AddCommand(cmdSxCompletion())
		}
	}
}

// sxComplete answers a completion script's `sx __complete …` request.
func sxComplete(args []string) int {
	c := sxCompletionRoot()
	c.SetArgs(args)
	if err := c.Execute(); err != nil {
		return 1
	}
	return 0
}
