package provider

import (
	"fmt"
	"strings"
)

// Service Quotas codes for the vCPU quotas of standard instance families
// (A, C, D, H, I, M, R, T, Z), the ones spotexec fleets use.
const (
	QuotaCodeSpotStandard     = "L-34B43A08"
	QuotaCodeOnDemandStandard = "L-1216C47A"
)

// QuotaError means EC2 refused a launch because of the account's vCPU quota
// in the region, not a capacity shortage: waiting for capacity won't help,
// only instances in the account ending or a quota increase will.
type QuotaError struct {
	Region  string
	Spot    bool   // the Spot quota (false: on-demand, via on_demand_fallback)
	Code    string // its Service Quotas code ("" outside the standard families)
	VCPUs   int    // vCPUs the refused launch needed
	Desired int    // a quota that would fit this runner's whole fleet
	Detail  string // what EC2 answered, per instance type
}

func (e *QuotaError) market() string {
	if e.Spot {
		return "Spot"
	}
	return "on-demand"
}

func (e *QuotaError) Error() string {
	msg := fmt.Sprintf("the account's %s vCPU quota in %s has no room for another VM (a launch needs %d vCPUs)", e.market(), e.Region, e.VCPUs)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if c := e.Command(); c != "" {
		msg += "\n  Raise it (as an AWS administrator): " + c
	} else {
		msg += "\n  Raise the " + e.market() + " vCPU quota for these instance families in the Service Quotas console."
	}
	return msg
}

// Command is the AWS CLI command that requests the increase ("" when the
// quota code isn't known).
func (e *QuotaError) Command() string {
	if e.Code == "" {
		return ""
	}
	return QuotaIncreaseCommand(e.Region, e.Code, max(e.Desired, e.VCPUs))
}

// QuotaIncreaseCommand is the AWS CLI command that requests a quota of n.
func QuotaIncreaseCommand(region, code string, n int) string {
	return fmt.Sprintf("aws service-quotas request-service-quota-increase --region %s --service-code ec2 --quota-code %s --desired-value %d", region, code, n)
}

// quotaCodeFor is the vCPU quota an instance type counts against ("" for
// families outside the standard group, which have their own quotas).
func quotaCodeFor(itype string, spot bool) string {
	if !standardFamily(itype) {
		return ""
	}
	if spot {
		return QuotaCodeSpotStandard
	}
	return QuotaCodeOnDemandStandard
}

// standardFamily reports whether an instance type is in the standard
// families (A, C, D, H, I, M, R, T, Z), which share one vCPU quota.
func standardFamily(itype string) bool {
	if itype == "" {
		return false
	}
	t := strings.ToLower(itype)
	for _, own := range []string{"inf", "dl", "trn", "hpc"} { // these have quotas of their own
		if strings.HasPrefix(t, own) {
			return false
		}
	}
	return strings.ContainsRune("acdhimrtz", rune(t[0]))
}
