package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// cmdImage manages baked runner images (AMIs) for aws-spot fleets.
func cmdImage() *cobra.Command {
	c := &cobra.Command{Use: "image", Short: "Bake runner images (AMIs) so new VMs skip their bootstrap"}
	pick := func(args []string) (*config.Runner, *provider.AWSSpot, error) {
		cfg, err := loadGlobal()
		if err != nil {
			return nil, nil, err
		}
		var r *config.Runner
		if len(args) > 0 {
			r, err = cfg.Runner(args[0])
		} else {
			r, err = firstEphemeral(cfg)
		}
		if err != nil {
			return nil, nil, err
		}
		p, err := provider.New(r)
		if err != nil {
			return nil, nil, err
		}
		a, ok := p.(*provider.AWSSpot)
		if !ok {
			return nil, nil, fmt.Errorf("runner %s is not an aws-spot runner", r.Name)
		}
		return r, a, nil
	}
	var pulls []string
	bake := &cobra.Command{
		Use:   "bake [runner]",
		Short: "Run the runner's bootstrap once (plus Docker) and snapshot it into an AMI that new VMs boot from",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, a, err := pick(args)
			if err != nil {
				return err
			}
			if r.AMI != "" && r.AMI != "auto" {
				return fmt.Errorf("runner %s sets ami = %q, so it would never use a bake; remove it (or set \"auto\") first", r.Name, r.AMI)
			}
			for _, p := range pulls {
				if !provider.ImageRefOK(p) {
					return fmt.Errorf("invalid image reference %q", p)
				}
			}
			// Pre-pull the project's container image when there is one and the
			// builder can reach it (it has no registry credentials, so not a
			// private ECR image).
			if l, err := load(""); err == nil && l.cfg.Project.Container.Image != "" {
				img := l.cfg.Project.Container.Image
				if strings.Contains(img, ".dkr.ecr.") {
					fmt.Fprintf(os.Stderr, "not pre-pulling %s (private ECR: VMs pull it at run time)\n", img)
				} else if provider.ImageRefOK(img) {
					pulls = append(pulls, img)
				}
			}
			rep, _ := ui.New(ui.ModeAuto, os.Stderr, "")
			ctx, _, stop := signals()
			defer stop()
			start := time.Now()
			logf := func(f string, a ...any) {
				msg := fmt.Sprintf(f, a...)
				rep.Status("bake %s: %s", r.Name, msg)
				rep.Event("bake", map[string]any{"runner": r.Name, "message": msg})
			}
			unlockBake, err := a.LockBake()
			if err != nil {
				return err
			}
			defer unlockBake()
			a.SweepBuildersLocked(ctx, logf)
			logf("launching on-demand builder")
			in, err := a.BakeLaunch(ctx, logf, pulls)
			if err != nil {
				return err
			}
			builder := in.ID
			handedOff := false // BakeImage owns cleanup from then on
			defer func() {
				if !handedOff {
					rep.Warn("BAKE_FAILED", "terminating builder %s", builder)
					if _, err := a.BakeImageCleanup(context.WithoutCancel(ctx), builder); err != nil {
						rep.Warn("BAKE_CLEANUP", "could not terminate builder %s: %v; terminate it in the EC2 console", builder, err)
					}
				}
			}()
			ran, err := a.WaitRunning(ctx, builder, logf)
			if err != nil {
				return err
			}
			budget := 2*a.BakeBootstrapTimeout(len(pulls) > 0) + r.BootTimeout.Duration
			if err := client.WaitReady(ctx, r, ran.Target, rep, budget); err != nil {
				return err
			}
			tc := transport.New(ran.Target, r.WorkRoot)
			if out, err := tc.Run(ctx, "cat /var/tmp/spotexec/ready", nil); err != nil || strings.TrimSpace(string(out)) != "0" {
				tail, _ := tc.Run(ctx, "tail -n 30 /var/log/spotexec-bootstrap.log", nil)
				return fmt.Errorf("bootstrap failed on the builder; not baking:\n%s", strings.TrimSpace(string(tail)))
			}
			logf("sanitizing builder")
			if out, err := tc.Run(ctx, a.SanitizeScript(), nil); err != nil {
				return fmt.Errorf("sanitize: %v: %s", err, out)
			}
			tc.CloseMaster()
			handedOff = true
			ami, err := a.BakeImage(ctx, builder, logf)
			if err != nil {
				return err
			}
			rep.Event("baked", map[string]any{"runner": r.Name, "ami": ami, "bootstrap_hash": a.BakeHash(), "duration_ms": time.Since(start).Milliseconds()})
			rep.Done(fmt.Sprintf("%s: baked %s in %s; new VMs boot from it automatically (bootstrap %s)", r.Name, ami, ui.Dur(time.Since(start)), a.BakeHash()), true)
			return nil
		},
	}
	bake.Flags().StringArrayVar(&pulls, "pull", nil, "also pre-pull this container image into the bake (repeatable)")
	list := &cobra.Command{
		Use:   "list [runner]",
		Short: "List a runner's baked images, from every machine in the account",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, a, err := pick(args)
			if err != nil {
				return err
			}
			bakes, err := a.Bakes(cmd.Context())
			if err != nil {
				return err
			}
			if len(bakes) == 0 {
				fmt.Printf("%s: no baked images (bootstrap hash %s); run `spotexec image bake %s`\n", r.Name, a.BakeHash(), r.Name)
				return nil
			}
			printBakes(os.Stdout, r.Name, bakes, time.Now())
			return nil
		},
	}
	var clearAll bool
	clear := &cobra.Command{
		Use:   "clear [runner] [--all]",
		Short: "Deregister a runner's baked images (and their snapshots)",
		Long: `Deregister a runner's baked images and their snapshots: every bake this
machine made or would boot, and expired bakes from any machine. Bakes that
other machines made for a different bootstrap are kept (--all removes them
too). A bake a VM is booting from right now is never removed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, a, err := pick(args)
			if err != nil {
				return err
			}
			res, err := a.ClearBakes(cmd.Context(), clearAll)
			fmt.Printf("removed %d baked image(s)\n", res.Removed)
			for _, k := range res.Kept {
				fmt.Printf("  kept %s\n", k)
			}
			return err
		},
	}
	clear.Flags().BoolVar(&clearAll, "all", false, "also remove bakes other machines made for a different bootstrap")
	c.AddCommand(bake, list, clear)
	return c
}

var _ = errors.New

// printBakes shows a runner's bakes: age, base image, bootstrap, and
// whether new VMs boot from it (or why not).
func printBakes(w io.Writer, runner string, bakes []provider.Bake, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IMAGE\tCREATED\tAGE\tBASE IMAGE\tBOOTSTRAP\tMADE BY\tSTATUS")
	stale := false
	for _, b := range bakes {
		base := b.Base
		if base == "" {
			base = "-"
		}
		boot := b.Hash
		if b.Current {
			boot += " (current)"
		}
		var status string
		switch {
		case b.InUse && b.Stale != "":
			status, stale = "in use, stale: "+b.Stale, true
		case b.InUse:
			status = "in use"
		case b.Expired:
			status = "expired: " + b.Stale
			stale = stale || b.Current
		case b.Stale != "":
			status = "stale: " + b.Stale
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, b.Created.Local().Format("2006-01-02"), provider.AgeText(b.Age(now)), base, boot, b.Maker(), status)
	}
	tw.Flush()
	for _, b := range bakes {
		if b.CheckErr != "" {
			fmt.Fprintf(w, "note: %s; bakes were judged by age only\n", b.CheckErr)
			break
		}
	}
	if stale {
		fmt.Fprintf(w, "run `spotexec image bake %s` to pick up the current Ubuntu image and security updates\n", runner)
	}
}
