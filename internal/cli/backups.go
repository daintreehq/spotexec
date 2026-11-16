package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/ui"
	"github.com/spf13/cobra"
)

// cmdBackups lists and clears write-back backups and conflict copies.
func cmdBackups() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "backups",
		Short: "List the write-back backups and conflict copies kept for this repository",
		Long: `List the write-back backups and conflict copies kept for this repository,
newest first. Each write-back prunes old ones: a run's directory goes once
it is 14 days old, or once it is a day old and 50 newer runs of the same
kind exist. Age counts from when the copies were saved, and nothing saved
less than a day ago is pruned.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := workspaceHere()
			if err != nil {
				return err
			}
			runs := client.KeptRuns(ws, true)
			if asJSON {
				if runs == nil {
					runs = []client.KeptRun{}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"runs": runs})
			}
			printKept(cmd.OutOrStdout(), runs, time.Now())
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.AddCommand(cmdBackupsClear())
	return c
}

func cmdBackupsClear() *cobra.Command {
	var olderThan string
	var all, dry bool
	c := &cobra.Command{
		Use:   "clear ([<run-id>...] [--older-than AGE] | --all)",
		Args:  cobra.ArbitraryArgs,
		Short: "Delete kept backups and conflict copies",
		Long: `Delete kept backups and conflict copies: the runs named (both kinds), the
runs at least --older-than old, the named runs at least that old when given
both, or every run with --all.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			byAge := cmd.Flags().Changed("older-than")
			if all && (byAge || len(args) > 0) {
				return errors.New("--all takes no run IDs or --older-than")
			}
			if !all && !byAge && len(args) == 0 {
				return errors.New("name the runs to clear, or pass --older-than AGE or --all")
			}
			var age time.Duration
			if byAge {
				var err error
				if age, err = parseAge(olderThan); err != nil {
					return err
				}
			}
			ws, err := workspaceHere()
			if err != nil {
				return err
			}
			pick, err := pickKept(client.KeptRuns(ws, true), args, byAge, age, time.Now())
			if err != nil {
				return err
			}
			files, bytes := 0, int64(0)
			for _, r := range pick {
				files += r.Files
				bytes += r.Bytes
			}
			if dry {
				for _, r := range pick {
					fmt.Fprintln(cmd.OutOrStdout(), r.Dir)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "would remove %d run%s (%d file%s, %s)\n", len(pick), plural(len(pick)), files, plural(files), ui.Bytes(bytes))
				return nil
			}
			if err := client.RemoveKept(pick); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %d run%s (%d file%s, %s)\n", len(pick), plural(len(pick)), files, plural(files), ui.Bytes(bytes))
			return nil
		},
	}
	c.Flags().StringVar(&olderThan, "older-than", "", "only runs at least this old (e.g. 7d, 36h)")
	c.Flags().BoolVar(&all, "all", false, "every kept run")
	c.Flags().BoolVar(&dry, "dry-run", false, "only print what would be removed")
	return c
}

func workspaceHere() (*client.Workspace, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return client.Resolve(dir)
}

// pickKept selects the runs to clear: those named in ids (every one must
// exist), and with byAge only those at least age old.
func pickKept(runs []client.KeptRun, ids []string, byAge bool, age time.Duration, now time.Time) ([]client.KeptRun, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = false
	}
	var pick []client.KeptRun
	for _, r := range runs {
		if len(ids) > 0 {
			if _, ok := want[r.RunID]; !ok {
				continue
			}
			want[r.RunID] = true
		}
		if byAge && now.Sub(r.Time) < age {
			continue
		}
		pick = append(pick, r)
	}
	for _, id := range ids {
		if !want[id] {
			return nil, fmt.Errorf("no kept run %s (spotexec backups lists them)", id)
		}
	}
	return pick, nil
}

func printKept(w io.Writer, runs []client.KeptRun, now time.Time) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "no write-back backups or conflict copies kept")
		return
	}
	files, bytes := 0, int64(0)
	for _, r := range runs {
		fmt.Fprintf(w, "%-9s  %-28s  %5s ago  %5d file%-1s  %7s  %s\n", r.Kind, r.RunID, ago(now.Sub(r.Time)), r.Files, plural(r.Files), ui.Bytes(r.Bytes), r.Dir)
		files += r.Files
		bytes += r.Bytes
	}
	fmt.Fprintf(w, "%d run%s, %d file%s, %s\n", len(runs), plural(len(runs)), files, plural(files), ui.Bytes(bytes))
}

// ago is a coarse age: minutes, hours, then days.
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(0, int(d/time.Minute)))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
