package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

func cmdSetup() *cobra.Command {
	var profile, adminProfile, user string
	var yes, rotate, skipAWS, printPolicy, noVPC bool
	c := &cobra.Command{
		Use:   "setup",
		Short: "First-time setup: runner config, plus a dedicated AWS user so you only sign in once",
		Long: `First-time setup for this machine.

1. Writes ~/.config/spotexec/config.toml if it doesn't exist yet.
2. For AWS runners, uses your current AWS sign-in once to create a dedicated
   IAM user with only spotexec's permissions, saves its access key as an AWS
   CLI profile ("spotexec"), and points the runners at it. spotexec then
   keeps working when your own AWS session (aws login, SSO) expires.

Safe to re-run: it refreshes the user's policy (for new spotexec versions)
and keeps a working key. --rotate replaces the key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printPolicy {
				fmt.Println(provider.SpotexecPolicy)
				return nil
			}
			p := config.GlobalPath()
			if _, err := os.Stat(p); err != nil {
				region, err := writeGlobalTemplate(p)
				if err != nil {
					return err
				}
				fmt.Printf("✓ wrote %s (an aws runner in %s; edit it any time)\n", p, region)
			} else {
				fmt.Printf("✓ runner config %s\n", p)
			}
			g, err := config.LoadGlobal()
			if err != nil {
				return err
			}
			aws := awsRunners(g)
			if len(aws) == 0 || skipAWS {
				if len(aws) == 0 {
					fmt.Println("  no AWS runners configured, so there is no AWS setup to do")
				}
				printSetupNext()
				return nil
			}
			if user == "" {
				user = defaultIAMUser()
			}
			region := aws[0].Region
			names := make([]string, len(aws))
			for i, r := range aws {
				names[i] = r.Name
			}
			checkVPC := true
			for _, r := range aws {
				if r.Region == region && r.SubnetID != "" {
					checkVPC = false
				}
			}
			s := &provider.AWSUserSetup{
				AdminProfile: adminProfile, Profile: profile, UserName: user, Region: region, Rotate: rotate, CheckVPC: checkVPC && !noVPC,
				Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
				Confirm: func(plan provider.AWSSetupPlan) bool {
					fmt.Printf("\nSigned in to AWS account %s as %s. This will:\n", plan.Account, plan.AdminARN)
					if plan.CreateVPC {
						fmt.Printf("  • create the default VPC in %s, which has none (AWS's standard network; free; --no-vpc skips this)\n", region)
					}
					fmt.Printf("  • create (or update) IAM user %s with only spotexec's permissions (see `spotexec setup --print-policy`)\n", user)
					fmt.Printf("  • save an access key for it as AWS profile %q in ~/.aws/credentials (if it hasn't one already)\n", profile)
					fmt.Printf("  • set profile = %q on runner(s) %s\n", profile, strings.Join(names, ", "))
					if yes {
						return true
					}
					if !ui.IsTerminal(os.Stdin) {
						fmt.Println("Not a terminal: re-run with --yes to go ahead.")
						return false
					}
					fmt.Print("Continue? [Y/n] ")
					line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
					a := strings.ToLower(strings.TrimSpace(line))
					return a == "" || a == "y" || a == "yes"
				},
			}
			fmt.Printf("Setting up AWS access for %s (region %s)…\n", strings.Join(names, ", "), region)
			res, err := s.Run(cmd.Context())
			if errors.Is(err, provider.ErrSetupDeclined) {
				return exitWith(1)
			}
			if err != nil {
				return err
			}
			switch {
			case res.KeyCreated:
				fmt.Printf("✓ AWS profile %s → %s (key %s; it doesn't expire)\n", profile, res.UserARN, res.AccessKeyID)
			case res.AlreadyReady:
				fmt.Printf("✓ AWS profile %s already works → %s\n", profile, res.UserARN)
			}
			for _, k := range res.KeysDeleted {
				fmt.Printf("  deleted the user's old access key %s\n", k)
			}
			if res.NoVPC {
				fmt.Printf("! %s has no default VPC: set subnet_id on the runner (see the README's network step)\n", region)
			}
			if res.PolicyStale {
				fmt.Println("  (not signed in as an administrator, so the user's policy wasn't refreshed; that's only needed after upgrading spotexec)")
			}
			changed, err := setRunnerProfile(g.Path, names, profile)
			if err != nil {
				return fmt.Errorf("%w\nset `profile = %q` on these runners in %s yourself", err, profile, g.Path)
			}
			if len(changed) > 0 {
				fmt.Printf("✓ set profile = %q on %s in %s\n", profile, strings.Join(changed, ", "), g.Path)
			}
			if res.QuotaVCPUs >= 0 {
				need := 0
				for _, r := range aws {
					if r.Region == region {
						need += r.MaxInstances * provider.VCPUsOf(r.InstanceTypes[0])
					}
				}
				if res.QuotaVCPUs < float64(need) {
					fmt.Printf("! Spot vCPU quota in %s is %.0f, but your runners may use up to %d. Until it's raised, fewer VMs run at once.\n", region, res.QuotaVCPUs, need)
					fmt.Printf("  Request more (as an administrator): aws service-quotas request-service-quota-increase --region %s --service-code ec2 --quota-code L-34B43A08 --desired-value %d\n", region, need)
				} else {
					fmt.Printf("✓ Spot vCPU quota in %s: %.0f (runners need up to %d)\n", region, res.QuotaVCPUs, need)
				}
			}
			printSetupNext()
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "spotexec", "AWS CLI profile to save the key as")
	c.Flags().StringVar(&adminProfile, "admin-profile", "", "AWS profile to create the user with (default: AWS_PROFILE, then the default profile)")
	c.Flags().StringVar(&user, "user", "", "IAM user name (default: spotexec-<hostname>)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	c.Flags().BoolVar(&rotate, "rotate", false, "replace the access key even if the current one works")
	c.Flags().BoolVar(&skipAWS, "skip-aws", false, "only write the runner config")
	c.Flags().BoolVar(&noVPC, "no-vpc", false, "don't create a default VPC when the region has none")
	c.Flags().BoolVar(&printPolicy, "print-policy", false, "print the IAM policy spotexec needs and exit")
	return c
}

func printSetupNext() {
	fmt.Println("\nNext: `cd` into a project, run `spotexec init`, then `sx <command>` (`spotexec doctor` checks everything).")
}

// awsRunners returns the AWS runners, default pool first.
func awsRunners(g *config.Global) []*config.Runner {
	rank := map[string]int{}
	for i, n := range g.DefaultPool {
		rank[n] = i + 1
	}
	var out []*config.Runner
	for _, r := range g.Runners {
		if r.Type == config.TypeAWSSpot {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := rank[out[i].Name], rank[out[j].Name]
		if ri == 0 {
			ri = 1 << 30
		}
		if rj == 0 {
			rj = 1 << 30
		}
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func defaultIAMUser() string {
	h, _ := os.Hostname()
	h = strings.ToLower(strings.TrimSuffix(strings.Split(h, ".")[0], "-"))
	h = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(h, "-")
	h = strings.Trim(h, "-")
	if h == "" {
		h = "machine"
	}
	if len(h) > 48 {
		h = h[:48]
	}
	return "spotexec-" + h
}

// setRunnerProfile sets `profile = "<profile>"` in each named runner's table
// of the global config, editing the text so comments and layout survive. It
// reloads the file to confirm the result and restores it on any mismatch.
func setRunnerProfile(path string, runners []string, profile string) ([]string, error) {
	orig, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	g, err := config.LoadGlobal()
	if err != nil {
		return nil, err
	}
	var todo []string
	for _, n := range runners {
		if r := g.Runners[n]; r != nil && r.Profile != profile {
			todo = append(todo, n)
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}
	out, err := setRunnerProfileText(string(orig), todo, profile)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(path)
	mode := os.FileMode(0o644)
	if fi != nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(out), mode); err != nil {
		return nil, err
	}
	g2, err := config.LoadGlobal()
	ok := err == nil
	for _, n := range todo {
		if ok && (g2.Runners[n] == nil || g2.Runners[n].Profile != profile) {
			ok = false
		}
	}
	if !ok {
		_ = os.WriteFile(path, orig, mode)
		return nil, errors.New("couldn't update the runner config automatically")
	}
	return todo, nil
}

var (
	tomlHeader  = regexp.MustCompile(`^\s*\[\s*runners\s*\.\s*("?)([^"\]]+)("?)\s*\]\s*(#.*)?$`)
	anyHeader   = regexp.MustCompile(`^\s*\[`)
	profileLine = regexp.MustCompile(`^\s*profile\s*=`)
	commented   = regexp.MustCompile(`^\s*#\s*profile\s*=`)
	typeLine    = regexp.MustCompile(`^\s*type\s*=`)
)

func setRunnerProfileText(text string, runners []string, profile string) (string, error) {
	want := map[string]bool{}
	for _, r := range runners {
		want[r] = true
	}
	lines := strings.Split(text, "\n")
	set := fmt.Sprintf("profile = %q", profile)
	for i := 0; i < len(lines); i++ {
		m := tomlHeader.FindStringSubmatch(lines[i])
		if m == nil || !want[strings.TrimSpace(m[2])] {
			continue
		}
		name := strings.TrimSpace(m[2])
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if anyHeader.MatchString(lines[j]) {
				end = j
				break
			}
		}
		at, insertAfter, hint := -1, -1, -1
		for j := i + 1; j < end; j++ {
			switch {
			case profileLine.MatchString(lines[j]):
				at = j
			case commented.MatchString(lines[j]) && hint < 0:
				hint = j
			case typeLine.MatchString(lines[j]) && insertAfter < 0:
				insertAfter = j
			}
		}
		switch {
		case at >= 0:
			lines[at] = set
		case hint >= 0:
			lines[hint] = set + "   # set by `spotexec setup`"
		default:
			if insertAfter < 0 {
				insertAfter = i
			}
			lines = append(lines[:insertAfter+1], append([]string{set + "   # set by `spotexec setup`"}, lines[insertAfter+1:]...)...)
		}
		delete(want, name)
	}
	if len(want) > 0 {
		var missing []string
		for n := range want {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		return "", fmt.Errorf("couldn't find [runners.%s]", strings.Join(missing, "], [runners."))
	}
	return strings.Join(lines, "\n"), nil
}
