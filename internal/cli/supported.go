package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/spf13/cobra"
)

func cmdSupported() *cobra.Command {
	var dir string
	var quiet bool
	c := &cobra.Command{
		Use:   "supported",
		Short: "Print true if sx can run commands from this directory, else false",
		Long: `Print true if sx can run commands from this directory, else false.

True means the project has opted in with a valid .spotexec.toml (here or in a
parent directory, up to the repository root), spotexec syncs its workspace
(not your home directory, / or a credential directory), there is a runner to
run on, and the checks a run makes before its command starts pass on the local
files: the working set is within the sync limit, and a container built from a
Dockerfile has its build inputs synced. Whether a runner is reachable, and
whether the image builds, only a run finds out.
It exits 0 when true and 1 when false, so scripts and agents can use either:

  spotexec supported            # true
  if spotexec supported >/dev/null; then sx pnpm test; else pnpm test; fi

When it is false, the reason goes to stderr (--quiet leaves it out), and
spotexec doctor run from that directory checks the config and runners.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := supported(cmd.Context(), dir)
			fmt.Fprintln(cmd.OutOrStdout(), err == nil)
			if err != nil {
				if !quiet {
					fmt.Fprintln(cmd.ErrOrStderr(), "spotexec:", err)
				}
				return exitCode(1)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&dir, "dir", "C", "", "check this directory instead of the current one")
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the reason it is false on stderr")
	return c
}

// supported returns nil when a plain sx from dir would get as far as a
// runner and start its command there, as far as the local files tell, and
// otherwise why not: the project opted in with a .spotexec.toml that loads,
// its workspace is one spotexec syncs, a runner is configured for it, and
// the working set passes the checks a run makes before its command starts
// (client.Preflight). The project and runners are resolved by load, as
// every run resolves them.
func supported(ctx context.Context, dir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dir != "" {
		// A missing directory would otherwise resolve to a parent's project.
		if fi, err := os.Stat(dir); err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
	}
	l, err := load(dir)
	if err != nil {
		return err
	}
	if l.cfg.Project.Path == "" {
		return fmt.Errorf("%s has no .spotexec.toml here or above it, up to the repository root; `spotexec init` opts a project in", filepath.Join(l.ws.Root, l.ws.Cwd))
	}
	// The sync mode a plain sx uses: SPOTEXEC_SYNC beats the project's, and
	// a value the run would reject fails every run.
	mode := l.cfg.Project.Sync.Mode
	if v := os.Getenv("SPOTEXEC_SYNC"); v != "" {
		mode = v
	}
	if mode != "auto" && mode != "in" && mode != "none" {
		return fmt.Errorf("SPOTEXEC_SYNC must be auto, in or none (got %q)", mode)
	}
	// As client.Run: when it syncs, a workspace rooted at home, / or a
	// credential directory is refused, and sync.source = "git" needs a Git
	// worktree.
	if mode != "none" {
		if !l.ws.Marked {
			return fmt.Errorf("spotexec never syncs %s: it is your home directory, / or a credential directory (~/.aws, ~/.ssh)", l.ws.Root)
		}
		if l.cfg.Project.Sync.Source == "git" && !l.ws.Git {
			return fmt.Errorf("%s is not a Git worktree but sync.source = \"git\"", l.ws.Root)
		}
	}
	var override []string // as UsableRunners: SPOTEXEC_RUNNER beats the pool
	if r := os.Getenv("SPOTEXEC_RUNNER"); r != "" {
		override = []string{r}
	}
	if _, err := l.cfg.Pool(override); err != nil {
		return err
	}
	return client.Preflight(ctx, l.cfg.Project, l.ws, mode)
}
