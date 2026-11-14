package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/hooks"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

func cmdSetup() *cobra.Command {
	var profile, adminProfile, user, install string
	var yes, rotate, skipAWS, printPolicy, noVPC, noHooks, all, list, bake, adminFromEnv bool
	var revoke string
	c := &cobra.Command{
		Use:   "setup [host...]",
		Short: "First-time setup: runner config, plus a dedicated AWS user so you only sign in once",
		Long: `First-time setup for this machine.

1. Writes ~/.config/spotexec/config.toml if it doesn't exist yet, with an
   AWS Spot fleet of 8-vCPU VMs (4-vCPU ones when the account's Spot quota
   can't fit one).

2. For AWS runners, uses your current AWS sign-in once to create a dedicated
   IAM user with only spotexec's permissions, saves its access key as an AWS
   CLI profile ("spotexec"), and points the runners at it. spotexec then
   keeps working when your own AWS session (aws login, SSO) expires. It also
   checks the Spot vCPU quota, and offers to request more when not even one
   VM fits.

3. If Claude Code is installed, offers its hook, so agents' heavy commands
   run on your runners (--no-hooks skips it).

4. Sets up other machines over SSH: the hosts you name, or (interactively)
   the ones you pick from those found on your network and in ~/.ssh/config.
   Each gets spotexec installed (Homebrew when it has it, otherwise the
   release binary in ~/.local/bin), a copy of this runner config, and its
   own IAM user and key, created with your AWS sign-in on this machine.

5. For AWS runners without a current baked image, offers to bake one
   (about 10-15 minutes), so new VMs skip their bootstrap. --yes skips the
   offer; --bake bakes without asking.

Safe to re-run: it refreshes the user's policy (for new spotexec versions)
and keeps a working key. --rotate replaces the key.

  spotexec setup                          this machine, then pick others
  spotexec setup studio-01.local mini-01  this machine, then those two`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printPolicy {
				fmt.Println(provider.SpotexecPolicy)
				return nil
			}
			if list {
				return listMachines(cmd.Context(), adminProfile)
			}
			if revoke != "" {
				return revokeMachine(cmd.Context(), adminProfile, revoke, yes)
			}
			// Before changing anything here.
			if err := checkSSHHosts(args); err != nil {
				return err
			}
			setupLocal := func() (bool, error) {
				p := config.GlobalPath()
				wrote := false // the starter template, sized below to the Spot quota
				if _, err := os.Stat(p); err != nil {
					region, why, err := writeGlobalTemplate(p, adminProfile)
					if err != nil {
						return false, err
					}
					wrote = true
					fmt.Printf("✓ wrote %s (an aws runner in %s, %s; edit it any time)\n", p, region, why)
				} else {
					fmt.Printf("✓ runner config %s\n", p)
				}
				g, err := config.LoadGlobal()
				if err != nil {
					return false, err
				}
				aws := awsRunners(g)
				if len(aws) == 0 || skipAWS {
					if len(aws) == 0 {
						fmt.Println("  no AWS runners configured, so there is no AWS setup to do")
					}
					return false, nil
				}
				region := aws[0].Region
				names := make([]string, len(aws))
				for i, r := range aws {
					names[i] = r.Name
				}
				// Every region the runners use: each needs a network and quota.
				var regions, vpcRegions []string
				need := map[string]int{}
				ownNet := map[string]bool{}
				for _, r := range aws {
					if _, seen := need[r.Region]; !seen {
						regions = append(regions, r.Region)
					}
					need[r.Region] += r.MaxInstances * provider.VCPUsOf(r.InstanceTypes[0])
					ownNet[r.Region] = ownNet[r.Region] || r.SubnetID != ""
				}
				for _, rg := range regions {
					if !ownNet[rg] && !noVPC {
						vpcRegions = append(vpcRegions, rg)
					}
				}
				s := &provider.AWSUserSetup{
					AdminProfile: adminProfile, AdminFromEnv: adminFromEnv, Profile: profile, UserName: user, DefaultName: defaultIAMUser(), Region: region, Rotate: rotate,
					VPCRegions: vpcRegions, QuotaRegions: regions,
					Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
					Confirm: func(plan provider.AWSSetupPlan) bool {
						fmt.Printf("\nSigned in to AWS account %s as %s. This will:\n", plan.Account, plan.AdminARN)
						for _, rg := range plan.CreateVPCIn {
							fmt.Printf("  • create the default VPC in %s, which has none (AWS's standard network; free; --no-vpc skips this)\n", rg)
						}
						fmt.Printf("  • create (or update) IAM user %s with only spotexec's permissions (see `spotexec setup --print-policy`)\n", plan.UserName)
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
				fmt.Printf("Setting up AWS access for %s (%s)…\n", strings.Join(names, ", "), strings.Join(regions, ", "))
				res, err := s.Run(cmd.Context())
				if errors.Is(err, provider.ErrSetupDeclined) {
					return false, exitWith(1)
				}
				if err != nil {
					return false, err
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
				for _, rg := range res.NoVPC {
					fmt.Printf("! %s has no default VPC: set subnet_id on its runners (see https://github.com/daintreehq/spotexec/blob/main/docs/aws.md#6-check-the-network)\n", rg)
				}
				if res.PolicyStale {
					fmt.Println("  (not signed in as an administrator, so the user's policy wasn't refreshed; that's only needed after upgrading spotexec)")
				}
				changed, err := setRunnerProfile(g.Path, names, profile)
				if err != nil {
					return false, fmt.Errorf("%w\nset `profile = %q` on these runners in %s yourself", err, profile, g.Path)
				}
				if len(changed) > 0 {
					fmt.Printf("✓ set profile = %q on %s in %s\n", profile, strings.Join(changed, ", "), g.Path)
				}
				if q, ok := res.Quotas[aws[0].Region]; ok && wrote && q < starterVCPUs {
					// A new account's quota can't fit one 8-vCPU VM.
					sized, err := sizeStarterFleet(g.Path)
					if err != nil {
						return false, fmt.Errorf("sizing the starter fleet to the Spot quota: %w (set instance_types to 4-vCPU types such as c7a.xlarge in %s)", err, g.Path)
					}
					if sized {
						fmt.Printf("✓ sized the starter fleet to the Spot quota in %s (%.0f vCPUs): 4-vCPU VMs instead of 8-vCPU ones\n", aws[0].Region, q)
						g2, err := config.LoadGlobal()
						if err != nil {
							return false, err
						}
						aws = awsRunners(g2)
					}
				}
				var ask func(provider.QuotaFit) bool
				if !res.PolicyStale && !yes && ui.IsTerminal(os.Stdin) {
					// The admin session is at hand: offer to file the request.
					ask = func(f provider.QuotaFit) bool {
						fmt.Printf("  Request %d vCPUs in %s now, with your AWS sign-in? [Y/n] ", f.Desired(), f.Region)
						line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
						a := strings.ToLower(strings.TrimSpace(line))
						return a == "" || a == "y" || a == "yes"
					}
				}
				reportQuotas(os.Stdout, quotaFits(aws, res.Quotas), ask, func(f provider.QuotaFit) (string, error) {
					return s.RequestQuotaIncrease(cmd.Context(), f.Region, f.Desired())
				})
				return true, nil
			}
			hasAWS, err := setupLocal()
			if err != nil {
				return err
			}
			hookDeclined := setupClaudeHook(yes, noHooks)
			hosts := args
			if all {
				for _, m := range loadMachines() {
					if !containsFold(hosts, m.Host) {
						hosts = append(hosts, m.Host)
					}
				}
				if len(hosts) == 0 {
					fmt.Println("  no other machines have been set up from here yet")
				}
			}
			if len(hosts) == 0 && !all && !yes && ui.IsTerminal(os.Stdin) && ui.IsTerminal(os.Stdout) {
				hosts = chooseHosts(discoverHosts(cmd.Context()), bufio.NewReader(os.Stdin))
			}
			if len(hosts) > 0 {
				if err := setupHosts(cmd.Context(), hosts, remoteSetup{
					install: install, adminProfile: adminProfile, profile: profile,
					rotate: rotate, noVPC: noVPC, aws: hasAWS,
					// Each machine installs the hook if it has Claude Code,
					// unless it was turned down here.
					noHooks: noHooks || hookDeclined,
				}); err != nil {
					return err
				}
			}
			// AWS runners may exist even when --skip-aws left credentials
			// alone: they still boot cold, and --bake still applies.
			awsConfigured := hasAWS
			if g, err := config.LoadGlobal(); err == nil && len(awsRunners(g)) > 0 {
				awsConfigured = true
			}
			if hasAWS || (bake && awsConfigured) {
				interactive := ui.IsTerminal(os.Stdin) && ui.IsTerminal(os.Stdout)
				if err := offerBake(cmd.Context(), os.Stdout, bufio.NewReader(os.Stdin), bake, yes, interactive); err != nil {
					return err
				}
			}
			printSetupNext(awsConfigured)
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "spotexec", "AWS CLI profile to save the key as")
	c.Flags().StringVar(&adminProfile, "admin-profile", "", "AWS profile to create the user with (default: AWS_PROFILE, then the default profile)")
	c.Flags().StringVar(&user, "user", "", "IAM user name (default: spotexec-<hostname>)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	c.Flags().BoolVar(&rotate, "rotate", false, "replace the access key even if the current one works")
	c.Flags().BoolVar(&skipAWS, "skip-aws", false, "skip the AWS steps (IAM user and key, network, quota)")
	c.Flags().BoolVar(&all, "all", false, "also update every machine set up from here before (config, spotexec version, AWS policy)")
	c.Flags().BoolVar(&list, "list", false, "list every machine's spotexec IAM user, keys and running VMs, then exit")
	c.Flags().StringVar(&revoke, "revoke", "", "delete a machine's IAM user and keys and terminate its VMs (host or IAM user name), then exit")
	c.Flags().BoolVar(&noHooks, "no-hooks", false, "don't install the Claude Code hook")
	c.Flags().BoolVar(&noVPC, "no-vpc", false, "don't create a default VPC when the region has none")
	c.Flags().StringVar(&install, "install", "auto", "how to install spotexec on other machines: auto, brew, release, copy (this binary) or none")
	c.Flags().BoolVar(&printPolicy, "print-policy", false, "print the IAM policy spotexec needs and exit")
	c.Flags().BoolVar(&bake, "bake", false, "bake runner images at the end without asking (setup otherwise offers it, except with --yes)")
	// Set by `spotexec setup <host>` on the other machine: the admin session
	// is only what it passed in AWS_* variables, never that machine's own.
	c.Flags().BoolVar(&adminFromEnv, "admin-from-env", false, "use only the AWS credentials in AWS_ACCESS_KEY_ID etc. to manage IAM users")
	_ = c.Flags().MarkHidden("admin-from-env")
	return c
}

func printSetupNext(aws bool) {
	if os.Getenv("SPOTEXEC_SETUP_NESTED") != "" {
		return // run by another machine's setup, which says what's next
	}
	fmt.Println("\nNext: `cd` into a project, run `spotexec init`, then `sx <command>` (`spotexec doctor` checks everything).")
	if aws {
		fmt.Println("Before a demo, do one warm-up `sx` run: the first boots a VM (a minute or more, plus the bootstrap unless the image is baked), and later runs reuse it until it has been idle for idle_timeout.")
	}
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
	// macOS's LocalHostName is stable; os.Hostname can pick up a
	// DHCP/VPN-assigned name or a Bonjour "-2" suffix.
	if b, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil && strings.TrimSpace(string(b)) != "" {
		h = strings.TrimSpace(string(b))
	}
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

// tomlLineStarts reports, for each line, whether it begins a new TOML
// statement: a line inside a multi-line string or a multi-line array or
// inline table is part of the value before it, whatever it looks like.
func tomlLineStarts(lines []string) []bool {
	starts := make([]bool, len(lines))
	var ml string // the open multi-line string's delimiter, if any
	depth := 0    // open [ and { in a value
	for n, line := range lines {
		starts[n] = ml == "" && depth == 0
		if starts[n] && anyHeader.MatchString(line) {
			continue // a table header: its brackets aren't a value's
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			if ml != "" {
				if c == '\\' && ml == `"""` {
					i++
					continue
				}
				if strings.HasPrefix(line[i:], ml) {
					i += 2
					// Up to two more quotes belong to the string ("""a"""").
					for k := 0; k < 2 && i+1 < len(line) && line[i+1] == ml[0]; k++ {
						i++
					}
					ml = ""
				}
				continue
			}
			switch c {
			case '#':
				i = len(line)
			case '"', '\'':
				if d := strings.Repeat(string(c), 3); strings.HasPrefix(line[i:], d) {
					ml, i = d, i+2
					continue
				}
				for i++; i < len(line) && line[i] != c; i++ {
					if c == '"' && line[i] == '\\' {
						i++
					}
				}
			case '[', '{':
				depth++
			case ']', '}':
				if depth > 0 {
					depth--
				}
			}
		}
	}
	return starts
}

// setRunnerProfileText sets the profile line in each named runner's table,
// leaving every other byte of the file as it was.
func setRunnerProfileText(text string, runners []string, profile string) (string, error) {
	want := map[string]bool{}
	for _, r := range runners {
		want[r] = true
	}
	lines := strings.Split(text, "\n")
	starts := tomlLineStarts(lines)
	set := fmt.Sprintf("profile = %q", profile)
	// next returns the line after the statement that begins at j.
	next := func(j, end int) int {
		for j++; j < end && !starts[j]; j++ {
		}
		return j
	}
	for i := 0; i < len(lines); i++ {
		if !starts[i] {
			continue
		}
		m := tomlHeader.FindStringSubmatch(lines[i])
		if m == nil || !want[strings.TrimSpace(m[2])] {
			continue
		}
		name := strings.TrimSpace(m[2])
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if starts[j] && anyHeader.MatchString(lines[j]) {
				end = j
				break
			}
		}
		// Keep CRLF files CRLF: a line's "\r" is its terminator's first half.
		crlf := strings.Contains(text, "\r\n")
		eol := func(l string) string {
			if strings.HasSuffix(l, "\r") {
				return "\r"
			}
			return ""
		}
		at, insertAt, hint := -1, -1, -1
		for j := i + 1; j < end; j++ {
			if !starts[j] {
				continue
			}
			switch {
			case profileLine.MatchString(lines[j]):
				at = j
			case commented.MatchString(lines[j]) && hint < 0:
				hint = j
			case typeLine.MatchString(lines[j]) && insertAt < 0:
				insertAt = next(j, end)
			}
		}
		var repl []string
		switch {
		case at >= 0:
			after := next(at, end)
			insertAt, repl = at, []string{set + eol(lines[after-1])}
			lines = append(lines[:at], lines[after:]...)
		case hint >= 0:
			lines[hint] = set + "   # set by `spotexec setup`" + eol(lines[hint])
		default:
			if insertAt < 0 {
				insertAt = i + 1
			}
			repl = []string{set + "   # set by `spotexec setup`"}
			if insertAt < len(lines) {
				if crlf {
					repl[0] += "\r"
				}
			} else if crlf {
				lines[insertAt-1] += "\r" // it was the unterminated last line
			}
		}
		if repl != nil {
			lines = append(lines[:insertAt], append(repl, lines[insertAt:]...)...)
			starts = tomlLineStarts(lines)
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

// setupClaudeHook installs the Claude Code hook for all projects when Claude
// Code is on this machine, asking first in a terminal. It reports whether
// the user turned the hook down when asked.
func setupClaudeHook(yes, skip bool) (declined bool) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude", "settings.json")
	if !skip && hooks.HasLegacyClaude(p) {
		// Installed by v0.2.0 as `sx hook claude`; sx only runs commands now.
		if _, err := hooks.InstallClaude(p, transportQuote(hookBinary())+" hook claude"); err != nil {
			fmt.Printf("! couldn't update the Claude Code hook: %v\n", err)
		} else {
			fmt.Printf("✓ updated the Claude Code hook in %s to call spotexec\n", p)
			return true
		}
	}
	if hooks.HasClaude(p) {
		fmt.Printf("✓ Claude Code hook in %s\n", p)
		return false
	}
	if skip {
		return false
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err != nil {
		if _, err := exec.LookPath("claude"); err != nil {
			return false // no Claude Code here
		}
	}
	if !yes {
		if !ui.IsTerminal(os.Stdin) {
			return false
		}
		fmt.Print("\nInstall the Claude Code hook for all your projects? Agents' heavy commands (tests, linters, builds)\nthen run on your runners; it only acts in projects with a .spotexec.toml. [Y/n] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "" && a != "y" && a != "yes" {
			return true
		}
	}
	if _, err := hooks.InstallClaude(p, transportQuote(hookBinary())+" hook claude"); err != nil {
		fmt.Printf("! couldn't install the Claude Code hook: %v\n", err)
		return false
	}
	fmt.Printf("✓ installed the Claude Code hook in %s\n", p)
	fmt.Println("  Permission rules now see `spotexec run -- <cmd>`: allow `Bash(spotexec run:*)` if you pre-approve test commands.")
	return false
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// awsRegions lists the regions the configured AWS runners use.
func awsRegions() []string {
	g, err := config.LoadGlobal()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range awsRunners(g) {
		if !containsFold(out, r.Region) {
			out = append(out, r.Region)
		}
	}
	return out
}

func listMachines(ctx context.Context, adminProfile string) error {
	regions := awsRegions()
	ms, err := provider.ListMachines(ctx, adminProfile, regions)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Println("no spotexec IAM users in this account yet (run `spotexec setup`)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "IAM USER\tHOST\tKEY\tKEY CREATED\tLAST USED\tVMS\n")
	for _, m := range ms {
		name := m.User
		if m.This {
			name += " (this machine)"
		}
		key, created, used := "none", "-", "-"
		if len(m.Keys) > 0 {
			k := m.Keys[len(m.Keys)-1]
			key, created, used = k.ID, shortDate(k.Created), shortDate(k.LastUsed)
			if len(m.Keys) > 1 {
				key += fmt.Sprintf(" (+%d)", len(m.Keys)-1)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", name, orDash(m.Host), key, created, used, m.VMs)
	}
	tw.Flush()
	if len(regions) > 0 {
		fmt.Printf("\nVMs counted in %s. Revoke a machine: spotexec setup --revoke <host or user>\n", strings.Join(regions, ", "))
	}
	return nil
}

func shortDate(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortHost is the host name in an SSH destination: "me@studio-01.local"
// is studio-01.
func shortHost(dest string) string {
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		dest = dest[i+1:]
	}
	return strings.ToLower(strings.Split(dest, ".")[0])
}

// matchMachine finds the machine --revoke names: an exact IAM user name,
// a destination set up from here (the machine it reached), or a host name.
func matchMachine(ms []provider.Machine, saved []savedMachine, target string) (*provider.Machine, error) {
	for i := range ms {
		if strings.EqualFold(ms[i].User, target) {
			return &ms[i], nil
		}
	}
	var hits []int
	add := func(i int) {
		for _, h := range hits {
			if h == i {
				return
			}
		}
		hits = append(hits, i)
	}
	byID := false
	for _, e := range saved {
		if !strings.EqualFold(e.Host, target) || (e.MachineID == "" && e.Hostname == "") {
			continue
		}
		byID = byID || e.MachineID != ""
		for i, m := range ms {
			if e.MachineID != "" { // authoritative when recorded
				if m.MachineID == e.MachineID {
					add(i)
				}
			} else if strings.EqualFold(m.Host, shortHost(e.Hostname)) {
				add(i)
			}
		}
	}
	if len(hits) == 0 && byID {
		// Never another machine that happens to have that name.
		return nil, fmt.Errorf("%s was set up from here as a machine that has no spotexec IAM user any more (see spotexec setup --list)", target)
	}
	if len(hits) == 0 {
		short := shortHost(target)
		for i, m := range ms {
			if strings.EqualFold(m.Host, short) || strings.EqualFold(m.User, "spotexec-"+short) {
				add(i)
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no spotexec IAM user matches %q (see spotexec setup --list)", target)
	case 1:
		return &ms[hits[0]], nil
	}
	var users []string
	for _, i := range hits {
		users = append(users, ms[i].User)
	}
	return nil, fmt.Errorf("%q matches %s; name the IAM user", target, strings.Join(users, " and "))
}

// rosterMatch says whether a machine-list entry is the revoked machine hit,
// out of the account's machines ms. By machine ID when both are known;
// otherwise by name, and unsure when the name doesn't settle it (an entry
// recorded before machine IDs were, such as an alias, or a host name two
// machines share).
func rosterMatch(e savedMachine, hit provider.Machine, ms []provider.Machine, target string) (match, unsure bool) {
	if e.MachineID != "" && hit.MachineID != "" {
		return e.MachineID == hit.MachineID, false
	}
	if strings.EqualFold(e.Host, target) {
		return true, false // the destination revoke was given
	}
	if e.MachineID != "" {
		// The revoked user has no machine ID tag: the entry is another
		// machine only if that machine is known.
		for _, m := range ms {
			if m.MachineID == e.MachineID {
				return false, false
			}
		}
		return false, true
	}
	n := e.Hostname
	if n == "" {
		n = e.Host
	}
	same, isHit := 0, false
	for _, m := range ms {
		if m.Host != "" && strings.EqualFold(shortHost(n), m.Host) {
			same++
			isHit = isHit || m.User == hit.User
		}
	}
	switch {
	case isHit && same == 1:
		return true, false
	case isHit, same == 0 && e.Hostname == "" && e.MachineID == "":
		return false, true
	}
	return false, false
}

func revokeMachine(ctx context.Context, adminProfile, target string, yes bool) error {
	// The runner config says where to look for the machine's VMs: don't
	// revoke blind because it couldn't be read.
	if _, err := config.LoadGlobal(); err != nil {
		return fmt.Errorf("reading the runner config (it says which regions to search for the machine's VMs): %w", err)
	}
	regions := awsRegions()
	ms, err := provider.ListMachines(ctx, adminProfile, regions)
	if err != nil {
		return err
	}
	hit, err := matchMachine(ms, loadMachines(), target)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("IAM user %s (host %s) and its %d key(s)", hit.User, orDash(hit.Host), len(hit.Keys))
	if hit.VMs > 0 {
		what += fmt.Sprintf(", and terminate its %d VM(s) (stopped ones and bake builders included)", hit.VMs)
	}
	if hit.This {
		what += ". This is THIS machine: spotexec stops working here until you run `spotexec setup` again"
	}
	if !yes {
		if !ui.IsTerminal(os.Stdin) {
			return fmt.Errorf("would delete %s; re-run with --yes to go ahead", what)
		}
		fmt.Printf("Delete %s? [y/N] ", what)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return exitWith(1)
		}
	}
	if err := provider.RevokeMachine(ctx, adminProfile, hit.User, regions, func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }); err != nil {
		return err
	}
	saved := loadMachines()
	var unsure []string
	for _, e := range saved {
		if _, u := rosterMatch(e, *hit, ms, target); u {
			unsure = append(unsure, e.Host)
		}
	}
	gone := forgetMachine(func(e savedMachine) bool { m, _ := rosterMatch(e, *hit, ms, target); return m })
	fmt.Printf("✓ revoked %s", hit.User)
	if len(gone) > 0 {
		fmt.Printf(" and removed %s from the machine list", strings.Join(gone, ", "))
	}
	fmt.Println()
	if !hit.This {
		fmt.Println("  Its key no longer works. On that machine, delete the [spotexec] section of ~/.aws/credentials.")
	}
	if len(unsure) > 0 {
		fmt.Printf("! The machine list can't tell whether %s %s this machine (set up before spotexec recorded machine IDs).\n", strings.Join(unsure, ", "), map[bool]string{true: "is", false: "are"}[len(unsure) == 1])
		fmt.Printf("  Remove any that are from %s, or `spotexec setup --all` will set them up again.\n", machinesPath())
	}
	return nil
}
