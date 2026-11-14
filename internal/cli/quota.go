package cli

import (
	"context"
	"fmt"
	"io"
	"sort"

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
	for _, name := range cfg.RunnerNames() {
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
