package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

// The account teardown (tests replace them).
var (
	planTeardown  = provider.PlanTeardown
	applyTeardown = func(ctx context.Context, p *provider.TeardownPlan, log func(string, ...any)) error {
		return p.Apply(ctx, log)
	}
)

func cmdTeardown() *cobra.Command {
	var adminProfile string
	var regions []string
	var yes, dryRun bool
	c := &cobra.Command{
		Use:   "teardown",
		Short: "Remove everything spotexec created in your AWS account",
		Long: `Remove everything spotexec created in the AWS account of your admin session:
instances (fleet VMs and image-bake builders, any machine's), baked AMIs and
their snapshots, spotexec's security groups, its spotexec/* ECR repositories,
its spotexec-cache-<account>-<region> S3 buckets and the per-machine IAM
users under /spotexec/.

It searches every region enabled in the account (or those given with
--region), lists what it found and asks before deleting anything. It then
deactivates every machine's key, terminates every spotexec VM (including any
launched meanwhile) and waits until they're gone before removing the rest.
With --region, only those regions are cleaned up: the IAM users are
account-wide, so they and their keys are kept for machines still running
elsewhere.
Only resources with spotexec's tags or its exact names are touched. Anything
it couldn't list or delete is reported, and the exit status is then
non-zero; running it again picks up where it left off.

Needs an administrator's AWS sign-in (like setup). The default VPC that
setup may have created and the EC2 Spot service-linked role are left alone,
as are local files: ~/.config/spotexec and the [spotexec] profile in ~/.aws.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return teardownAccount(cmd.Context(), adminProfile, regions, yes, dryRun)
		},
	}
	c.Flags().StringVar(&adminProfile, "admin-profile", "", "AWS profile with admin rights (default: AWS_PROFILE / the default chain)")
	c.Flags().StringSliceVar(&regions, "region", nil, "only these regions (default: every region enabled in the account)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "only list what would be deleted")
	return c
}

func teardownAccount(ctx context.Context, adminProfile string, regions []string, yes, dryRun bool) error {
	fmt.Println("Looking for spotexec's resources in your AWS account…")
	p, err := planTeardown(ctx, adminProfile, regions)
	if err != nil {
		return err
	}
	if p.Regional {
		fmt.Printf("Account %s (signed in as %s), only %s: the account-wide IAM users and their keys are kept.\n", p.Account, p.AdminARN, strings.Join(p.Regions, ", "))
	} else {
		fmt.Printf("Account %s (signed in as %s), %d region(s).\n", p.Account, p.AdminARN, len(p.Regions))
	}
	for _, f := range p.Failed {
		fmt.Fprintf(os.Stderr, "warning: couldn't list %s\n", f)
	}
	if len(p.Items) == 0 {
		fmt.Println("Nothing of spotexec's found.")
		return p.Err()
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tREGION\tID\tDETAIL")
	for _, it := range p.Items {
		region := it.Region
		if region == "" {
			region = "global"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Kind, region, it.ID, it.Note)
	}
	tw.Flush()
	if dryRun {
		fmt.Printf("\n%d resource(s) would be deleted (dry run: nothing was).\n", len(p.Items))
		return p.Err()
	}
	if !yes {
		if !ui.IsTerminal(os.Stdin) {
			return fmt.Errorf("would delete these %d resource(s); re-run with --yes to go ahead", len(p.Items))
		}
		fmt.Printf("\nDelete these %d resource(s)? Running VMs are terminated and cached data is lost; this can't be undone. [y/N] ", len(p.Items))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return exitWith(1)
		}
	}
	err = applyTeardown(ctx, p, func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) })
	if err = errors.Join(err, p.Err()); err != nil {
		return fmt.Errorf("%w\nre-run `spotexec teardown` to retry", err)
	}
	if p.Regional {
		fmt.Printf("✓ removed what spotexec created in %s.\n", strings.Join(p.Regions, ", "))
		fmt.Println("  The IAM users and their keys are account-wide and were kept; run `spotexec teardown` without --region to remove them.")
		return nil
	}
	fmt.Println("✓ removed everything spotexec created in the account.")
	fmt.Println("  Locally, you can delete ~/.config/spotexec and the [spotexec] sections of ~/.aws/credentials and ~/.aws/config.")
	return nil
}
