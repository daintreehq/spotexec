package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func cmdSupported() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "supported",
		Short: "Print true if sx can run commands from this directory, else false",
		Long: `Print true if sx can run commands from this directory, else false.

True means the project has opted in with a valid .spotexec.toml (here or in a
parent directory, up to the repository root), spotexec syncs its workspace
(not your home directory, / or a credential directory) and there is a runner
to run on.
It exits 0 when true and 1 when false, so scripts and agents can use either:

  spotexec supported            # true
  if spotexec supported >/dev/null; then sx pnpm test; else pnpm test; fi

When it is false, spotexec doctor run from that directory checks the config
and runners.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ok := supported(dir)
			fmt.Fprintln(cmd.OutOrStdout(), ok)
			if !ok {
				return exitCode(1)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&dir, "dir", "C", "", "check this directory instead of the current one")
	return c
}

// supported reports whether a plain sx from dir would get as far as a runner:
// the project opted in with a .spotexec.toml that loads, its workspace is one
// spotexec syncs, and a runner is configured for it. The project and runners
// are resolved by load, as every run resolves them.
func supported(dir string) bool {
	if dir != "" {
		// A missing directory would otherwise resolve to a parent's project.
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return false
		}
	}
	l, err := load(dir)
	if err != nil || l.cfg.Project.Path == "" {
		return false
	}
	// The sync mode a plain sx uses: SPOTEXEC_SYNC beats the project's, and
	// a value the run would reject fails every run.
	mode := l.cfg.Project.Sync.Mode
	if v := os.Getenv("SPOTEXEC_SYNC"); v != "" {
		mode = v
	}
	if mode != "auto" && mode != "in" && mode != "none" {
		return false
	}
	// As client.Run: when it syncs, a workspace rooted at home, / or a
	// credential directory is refused, and sync.source = "git" needs a Git
	// worktree.
	if mode != "none" && (!l.ws.Marked || l.cfg.Project.Sync.Source == "git" && !l.ws.Git) {
		return false
	}
	var override []string // as UsableRunners: SPOTEXEC_RUNNER beats the pool
	if r := os.Getenv("SPOTEXEC_RUNNER"); r != "" {
		override = []string{r}
	}
	_, err = l.cfg.Pool(override)
	return err == nil
}
