package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// regionQuota is one region's Spot quota reading for `status -A`.
type regionQuota struct {
	Region  string              `json:"region"`
	Profile string              `json:"profile,omitempty"` // the AWS profile (account) read with
	Usage   provider.QuotaUsage `json:"usage"`
	Error   string              `json:"error,omitempty"`
	Err     error               `json:"-"`
}

// accountStatus is `status -A --json`.
type accountStatus struct {
	*provider.AccountSearch
	SpotQuotas []regionQuota `json:"spot_quotas"`
}

// spotQuotas reads the Spot vCPU quota and its live use in every region
// (and account) the AWS runners use.
func spotQuotas(ctx context.Context, cfg *config.Config) []regionQuota {
	seen := map[string]bool{}
	out := []regionQuota{}
	// Every runner in the account, including one a project runner masks:
	// the quota is the account's, whatever this repository selects.
	names := make([]string, 0, len(cfg.Global.Runners))
	for name := range cfg.Global.Runners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := cfg.Global.Runners[name]
		if r == nil || r.Type != config.TypeAWSSpot || seen[r.Region+"\x00"+r.Profile] {
			continue
		}
		seen[r.Region+"\x00"+r.Profile] = true
		p, err := provider.New(r)
		if err != nil {
			continue
		}
		a, ok := p.(*provider.AWSSpot)
		if !ok {
			continue
		}
		u, err := a.QuotaUsage(ctx)
		q := regionQuota{Region: r.Region, Profile: r.Profile, Usage: u, Err: err}
		if err != nil {
			q.Error = err.Error()
		}
		out = append(out, q)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Region < out[j].Region })
	return out
}

// printSpotQuotas shows each region's Spot vCPU quota against its use.
func printSpotQuotas(w io.Writer, qs []regionQuota) {
	if len(qs) == 0 {
		return
	}
	fmt.Fprintln(w, "\nSpot vCPU quota (standard instance families, shared by every machine in the account):")
	regions := map[string]int{}
	for _, q := range qs {
		regions[q.Region]++
	}
	for _, q := range qs {
		where := q.Region
		if regions[q.Region] > 1 { // several accounts in one region: say which
			where += " (profile " + q.Profile + ")"
		}
		switch {
		case q.Err != nil && q.Usage.Used == 0:
			fmt.Fprintf(w, "  %-14s couldn't read it: %v\n", where, q.Err)
		case q.Err != nil:
			fmt.Fprintf(w, "  %-14s %s (quota unreadable: %v)\n", where, q.Usage, q.Err)
		default:
			fmt.Fprintf(w, "  %-14s %s\n", where, q.Usage)
		}
	}
}

// The starter fleet in globalTemplate uses 8-vCPU VMs. When the account's
// Spot quota can't fit one (new accounts get 5 vCPUs), setup switches the
// freshly written template to 4-vCPU VMs instead, keeping max_instances:
// the quota, not the config, is what limits how many run at once.
const starterVCPUs = 8

var starterSmall = [][2]string{
	{`instance_types = ["c7a.2xlarge", "m7a.2xlarge", "c6a.2xlarge"]  # 8 vCPUs; each VM gets the cheapest with Spot capacity`,
		`instance_types = ["c7a.xlarge", "m7a.xlarge", "c6a.xlarge"]  # 4 vCPUs, to fit this account's Spot quota; once it's raised, the 2xlarge sizes (8 vCPUs) run more per VM`},
	{`max_slots = 4                  # concurrent jobs per VM (SPOTEXEC_CPUS = 2 each)`,
		`max_slots = 2                  # concurrent jobs per VM (SPOTEXEC_CPUS = 2 each)`},
}

// sizeStarterFleet rewrites the starter template at path to 4-vCPU VMs. It
// changes nothing (and reports false) unless every starter line is still
// as written.
func sizeStarterFleet(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	s := string(b)
	for _, r := range starterSmall {
		if strings.Count(s, r[0]) != 1 {
			return false, nil
		}
		s = strings.Replace(s, r[0], r[1], 1)
	}
	// Replaced whole, so a failed write never leaves half a config.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o644); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// quotaFits groups the AWS runners by region against each region's
// standard Spot quota (regions whose quota is unknown, or whose runners all
// use other quotas, are left out).
func quotaFits(runners []*config.Runner, quotas map[string]float64) []provider.QuotaFit {
	byRegion := map[string]*provider.QuotaFit{}
	others := map[string]bool{} // runners there can launch outside the standard families
	var order []string
	for _, r := range runners {
		others[r.Region] = others[r.Region] || provider.HasOtherFamiliesFor(r)
		one := provider.MinLaunchVCPUsFor(r)
		q, ok := quotas[r.Region]
		if one == 0 || !ok {
			continue
		}
		f := byRegion[r.Region]
		if f == nil {
			f = &provider.QuotaFit{Region: r.Region, Quota: int(q), OneVM: one}
			byRegion[r.Region] = f
			order = append(order, r.Region)
		}
		f.OneVM = min(f.OneVM, one)
		f.Fleet += r.MaxInstances * provider.VMVCPUs(r)
	}
	out := make([]provider.QuotaFit, 0, len(order))
	for _, rg := range order {
		f := *byRegion[rg]
		f.Others = others[rg]
		out = append(out, f)
	}
	return out
}

// reportQuotas prints setup's verdict on each region's Spot quota: a
// blocker when not even one VM fits (offering to request the increase with
// the admin session, when ask allows), a note when it only limits how many
// VMs run at once.
func reportQuotas(w io.Writer, fits []provider.QuotaFit, ask func(provider.QuotaFit) bool, request func(provider.QuotaFit) (string, error)) {
	for _, f := range fits {
		switch {
		case f.Blocked() && f.Others:
			fmt.Fprintf(w, "! Spot vCPU quota in %s is %d: not enough for one standard-family VM (%d vCPUs), so only your runners' other instance types can launch there until it's raised:\n", f.Region, f.Quota, f.OneVM)
			fmt.Fprintf(w, "  %s\n", f.Command())
		case f.Blocked():
			fmt.Fprintf(w, "✗ Spot vCPU quota in %s is %d: not enough for one VM (%d vCPUs), so spotexec can't launch there until it's raised.\n", f.Region, f.Quota, f.OneVM)
			fmt.Fprintf(w, "  Request more (as an AWS administrator): %s\n", f.Command())
			if ask != nil && ask(f) {
				if st, err := request(f); err != nil {
					fmt.Fprintf(w, "  The request failed (%v). Ask an administrator to run the command above.\n", err)
				} else {
					fmt.Fprintf(w, "✓ requested %d vCPUs in %s (%s). AWS usually decides small increases within minutes to hours; `spotexec doctor` shows the current value.\n", f.Desired(), f.Region, strings.ToLower(st))
				}
			}
		case f.Limited():
			fmt.Fprintf(w, "✓ Spot vCPU quota in %s: %d. Note: your runners there may use up to %d, so fewer VMs run at once until it's raised:\n", f.Region, f.Quota, f.Fleet)
			fmt.Fprintf(w, "  %s\n", f.Command())
		default:
			fmt.Fprintf(w, "✓ Spot vCPU quota in %s: %d (runners need up to %d)\n", f.Region, f.Quota, f.Fleet)
		}
	}
}
