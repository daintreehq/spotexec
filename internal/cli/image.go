package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
	var region string
	// pickAll returns the runner and its regional providers: one for a
	// single-region runner, or for a multi-region one the --region given,
	// else (when all is set) every region it uses.
	pickAll := func(ctx context.Context, args []string, all bool) (*config.Runner, []*provider.AWSSpot, error) {
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
		if region == "" && !all && !r.MultiRegion() {
			region = r.Region // a single-region runner bakes in its region, even while it still tidies up others
		}
		if region != "" && !config.ValidRegion(region) {
			return nil, nil, fmt.Errorf("invalid region %q", region)
		}
		if region != "" && all {
			// Listing and clearing may target any region, including one the
			// runner no longer uses (its images still bill).
			a, err := provider.RegionalAWS(r, region)
			if err != nil {
				return nil, nil, err
			}
			return r, []*provider.AWSSpot{a}, nil
		}
		p, err := provider.New(r)
		if err != nil {
			return nil, nil, err
		}
		switch a := p.(type) {
		case *provider.AWSSpot:
			if region != "" && region != r.Region {
				return nil, nil, fmt.Errorf("runner %s only uses %s", r.Name, r.Region)
			}
			return r, []*provider.AWSSpot{a}, nil
		case *provider.AWSGlobal:
			if region != "" {
				regions, err := a.PlacementRegions(ctx)
				if err != nil {
					return nil, nil, err
				}
				if !slices.Contains(regions, region) {
					return nil, nil, fmt.Errorf("runner %s doesn't launch in %s (its regions: %s)", r.Name, region, strings.Join(regions, ", "))
				}
				// A bake is per region; mark it as used so listings and the
				// pool manager look there for builders.
				reg, err := a.Use(region)
				if err != nil {
					return nil, nil, err
				}
				return r, []*provider.AWSSpot{reg}, nil
			}
			if !all {
				return nil, nil, fmt.Errorf("runner %s spans several regions and images are per region: pass --region (`spotexec prices %s` shows where launches go)", r.Name, r.Name)
			}
			as, err := a.RegionalProviders(ctx)
			return r, as, err
		}
		return nil, nil, fmt.Errorf("runner %s is not an aws-spot runner", r.Name)
	}
	pick := func(args []string) (*config.Runner, *provider.AWSSpot, error) {
		r, as, err := pickAll(context.Background(), args, false)
		if err != nil {
			return nil, nil, err
		}
		return r, as[0], nil
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
			return runBake(r, a, pulls)
		},
	}
	bake.Flags().StringArrayVar(&pulls, "pull", nil, "also pre-pull this container image into the bake (repeatable)")
	list := &cobra.Command{
		Use:   "list [runner]",
		Short: "List a runner's baked images, from every machine in the account",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, as, err := pickAll(cmd.Context(), args, true)
			if err != nil && len(as) == 0 {
				return err
			}
			if len(as) == 0 {
				fmt.Printf("%s: no regions to look in (none of its regions is enabled in this AWS account)\n", r.Name)
				return nil
			}
			total := 0
			for _, a := range as {
				bakes, err := a.Bakes(cmd.Context())
				if err != nil {
					return err
				}
				if len(bakes) == 0 {
					continue
				}
				if total > 0 {
					fmt.Println()
				}
				if len(as) > 1 {
					fmt.Printf("%s:\n", a.Runner().Region)
				}
				printBakes(os.Stdout, r.Name, bakes, time.Now())
				total += len(bakes)
			}
			if total == 0 {
				fmt.Printf("%s: no baked images (bootstrap hash %s); run `spotexec image bake %s`\n", r.Name, as[0].BakeHash(), r.Name)
			}
			return err
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
			_, as, err := pickAll(cmd.Context(), args, true)
			if err != nil && len(as) == 0 {
				return err
			}
			removed := 0
			for _, a := range as {
				res, cerr := a.ClearBakes(cmd.Context(), clearAll)
				removed += res.Removed
				for _, k := range res.Kept {
					if len(as) > 1 {
						k = a.Runner().Region + ": " + k
					}
					fmt.Printf("  kept %s\n", k)
				}
				err = errors.Join(err, cerr)
			}
			fmt.Printf("removed %d baked image(s)\n", removed)
			return err
		},
	}
	clear.Flags().BoolVar(&clearAll, "all", false, "also remove bakes other machines made for a different bootstrap")
	c.PersistentFlags().StringVar(&region, "region", "", "region to bake in, list or clear (multi-region runners)")
	c.AddCommand(bake, list, clear)
	return c
}

var _ = errors.New

// runBake bakes runner r's image (see `image bake`), reporting progress on
// stderr; Ctrl-C abandons it and terminates the builder.
func runBake(r *config.Runner, a *provider.AWSSpot, pulls []string) (err error) {
	rep, _ := ui.New(ui.ModeAuto, os.Stderr, "")
	ctx, _, stop := signals()
	defer stop()
	// Runs before stop: an interrupted bake says so, whatever step failed.
	defer func() { err = interrupted(ctx, err) }()
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
}

// interrupted marks err as caused by ctx ending (Ctrl-C), even when the
// step that failed lost that (an SSH command killed mid-way), so callers
// can tell an interrupt from a failure.
func interrupted(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || errors.Is(err, ctx.Err()) {
		return err
	}
	return fmt.Errorf("%w: %v", ctx.Err(), err)
}

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
