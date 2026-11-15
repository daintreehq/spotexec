package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
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
	// Usage is the account's live usage when spotexec checked before
	// launching (nil when EC2 refused the launch itself).
	Usage *QuotaUsage
	// Fair: the quota has room, but this machine is at its share and the
	// last room is left to the other machines using it.
	Fair bool
}

func (e *QuotaError) market() string {
	if e.Spot {
		return "Spot"
	}
	return "on-demand"
}

func (e *QuotaError) Error() string {
	msg := fmt.Sprintf("the account's %s vCPU quota in %s has no room for another VM (a launch needs %d vCPUs)", e.market(), e.Region, e.VCPUs)
	if e.Fair {
		msg = fmt.Sprintf("this machine has its share of the account's %s vCPU quota in %s, and the room left (%d vCPUs) is kept for the other machines using it", e.market(), e.Region, e.Usage.Quota-e.Usage.Used)
	}
	if e.Usage != nil {
		msg += " (" + e.Usage.String() + ")"
	}
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

// QuotaUsage is a region's standard Spot vCPU quota and what is using it,
// read live from the account: every machine sharing the account sees the
// same numbers.
type QuotaUsage struct {
	Region string `json:"region"`
	Quota  int    `json:"quota"` // vCPUs; -1 when it couldn't be read
	Used   int    `json:"used"`  // vCPUs of pending and running standard-family Spot instances
	Mine   int    `json:"mine"`  // of which this machine's spotexec VMs
	// Machines are the other spotexec machines' vCPUs, by machine name.
	Machines map[string]int `json:"machines,omitempty"`
	Other    int            `json:"other"` // Spot instances spotexec didn't launch
}

// Admit decides whether this machine may launch need more vCPUs, sharing
// the quota between the machines that use it:
//
//   - never past the quota;
//   - while other spotexec machines have VMs in the account, a machine
//     already at or above its share (the quota divided among the machines
//     with VMs, itself included) only grows while room for another VM
//     remains, so it can't take the last room from the others. Below its
//     share it may use whatever room is left.
//
// Machines with no VMs don't count toward the shares; idle VMs end after
// idle_timeout and give their vCPUs back.
func (u QuotaUsage) Admit(need int) (ok bool, fair bool) {
	if u.Quota < 0 {
		return true, false // unknown: let EC2 decide
	}
	if u.Used+need > u.Quota {
		return false, false
	}
	if len(u.Machines) == 0 {
		return true, false
	}
	share := u.Quota / (len(u.Machines) + 1)
	if u.Mine < share || u.Used+2*need <= u.Quota {
		return true, false
	}
	return false, true
}

// String describes the usage, e.g. "12 of 16 vCPUs in use: 4 by this
// machine, 8 by studio-01".
func (u QuotaUsage) String() string {
	s := fmt.Sprintf("%d of %d vCPUs in use", u.Used, u.Quota)
	if u.Quota < 0 {
		s = fmt.Sprintf("%d vCPUs in use (quota unknown)", u.Used)
	}
	var parts []string
	if u.Mine > 0 {
		parts = append(parts, fmt.Sprintf("%d by this machine", u.Mine))
	}
	names := make([]string, 0, len(u.Machines))
	for n := range u.Machines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%d by %s", u.Machines[n], n))
	}
	if u.Other > 0 {
		parts = append(parts, fmt.Sprintf("%d by Spot instances spotexec didn't launch", u.Other))
	}
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, ", ")
	}
	return s
}

// desired suggests a quota that fits this machine's whole fleet next to
// what the others use now, and at least one more launch.
func (u QuotaUsage) desired(need, fleet int) int {
	return max(u.Quota+need, u.Used-u.Mine+max(u.Mine+need, fleet))
}

// QuotaUsage reads the region's standard Spot vCPU quota and counts what
// uses it: every pending or running Spot instance in the account and region
// (DescribeInstances), attributed to this machine, other spotexec machines
// (by their host tag) or neither.
func (a *AWSSpot) QuotaUsage(ctx context.Context) (QuotaUsage, error) {
	u := QuotaUsage{Region: a.r.Region, Quota: -1, Machines: map[string]int{}}
	me, _ := ownerID()
	labels := map[string]string{} // owner -> machine name, unique per owner
	taken := map[string]bool{}
	label := func(host, owner string) string {
		if l, ok := labels[owner]; ok {
			return l
		}
		l := machineLabel(host, owner)
		if taken[l] {
			l += " (" + machineLabel("", owner) + ")"
		}
		labels[owner], taken[l] = l, true
		return l
	}
	p := url.Values{"Filter.1.Name": {"instance-lifecycle"}, "Filter.1.Value.1": {"spot"},
		"Filter.2.Name": {"instance-state-name"}, "Filter.2.Value.1": {"pending"}, "Filter.2.Value.2": {"running"},
		"MaxResults": {"1000"}}
	lctx, cancel := context.WithTimeout(ctx, 4*quotaLookupTimeout)
	defer cancel()
	for page := 0; ; page++ {
		if page == 200 {
			return u, errors.New("DescribeInstances: too many pages; usage incomplete")
		}
		b, err := a.api.ec2(lctx, "DescribeInstances", p)
		if err != nil {
			return u, err
		}
		ins, next, err := parseDescribeInstances(b)
		if err != nil {
			return u, err
		}
		for _, in := range ins {
			if !standardFamily(in.InstanceType) {
				continue // counts against another quota
			}
			n := vcpusOf(in.InstanceType) // quotas count a type's default vCPUs, whatever its CPU options
			u.Used += n
			switch owner := in.tag(tagOwner); {
			case owner == "" || (in.tag(tagRunner) == "" && in.tag(tagBake) == ""):
				u.Other += n
			case owner == me:
				u.Mine += n
			default:
				u.Machines[label(in.tag(tagHost), owner)] += n
			}
		}
		if next == "" {
			break
		}
		p.Set("NextToken", next)
	}
	if q, err := a.quotaValue(ctx, QuotaCodeSpotStandard); err == nil {
		u.Quota = q
	} else {
		return u, fmt.Errorf("Spot vCPU quota: %w", err)
	}
	return u, nil
}

// machineLabel names a spotexec machine for people: its host tag, else the
// start of its owner ID.
func machineLabel(host, owner string) string {
	if host != "" {
		return host
	}
	return "owner " + strings.Split(owner, "-")[0]
}

// admitSpot is the pre-launch check for one standard-family Spot launch of
// need vCPUs: nil when it fits the account's live usage u, else the
// QuotaError explaining why not.
func (a *AWSSpot) admitSpot(u QuotaUsage, need int) *QuotaError {
	ok, fair := u.Admit(need)
	if ok {
		return nil
	}
	fleet := a.r.MaxInstances * VMVCPUs(a.r)
	return &QuotaError{Region: a.r.Region, Spot: true, Code: QuotaCodeSpotStandard, VCPUs: need,
		Desired: u.desired(need, fleet), Usage: &u, Fair: fair}
}

// admitting wraps a launch attempt with the pre-launch check: Spot
// candidates in the standard families must fit the live usage (nil: not
// known, so EC2 decides).
func (a *AWSSpot) admitting(usage *QuotaUsage, run func(itype string, spot bool) (ec2Instance, error)) func(string, bool) (ec2Instance, error) {
	return func(itype string, spot bool) (ec2Instance, error) {
		if spot && usage != nil && standardFamily(itype) {
			if qe := a.admitSpot(*usage, vcpusOf(itype)); qe != nil {
				return ec2Instance{}, qe
			}
		}
		return run(itype, spot)
	}
}

func firstLineOfErr(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

// minStandardVCPUs is the smallest launch among a runner's standard-family
// instance types (0 if it has none).
func minStandardVCPUs(types []string) int {
	n := 0
	for _, t := range types {
		if standardFamily(t) && (n == 0 || vcpusOf(t) < n) {
			n = vcpusOf(t)
		}
	}
	return n
}

// QuotaFit compares a region's standard Spot vCPU quota with what the
// runners there need: one VM (the smallest launch) to work at all, and every
// fleet at max_instances to run all at once.
type QuotaFit struct {
	Region string
	Quota  int // vCPUs
	OneVM  int // the smallest launch
	Fleet  int // every fleet at max_instances
	// Others: the runners can also launch types outside the standard
	// families, which have quotas of their own.
	Others bool
}

// Blocked: not even one VM can launch.
func (f QuotaFit) Blocked() bool { return f.Quota < f.OneVM }

// Limited: fewer VMs than max_instances can run at once.
func (f QuotaFit) Limited() bool { return f.Quota < f.Fleet }

// Desired is the quota to ask for: room for every fleet at max_instances.
func (f QuotaFit) Desired() int { return max(f.Fleet, f.OneVM) }

// Command requests that quota.
func (f QuotaFit) Command() string {
	return QuotaIncreaseCommand(f.Region, QuotaCodeSpotStandard, f.Desired())
}

// RequestQuotaIncrease asks Service Quotas, with the admin session, to
// raise the region's standard Spot vCPU quota to desired. It returns the
// request's status (e.g. PENDING, or CASE_OPENED).
func (s *AWSUserSetup) RequestQuotaIncrease(ctx context.Context, region string, desired int) (string, error) {
	creds := s.adminCreds // resolved before setup rewrote any profile
	if creds == nil {
		creds = &awsCredResolver{profile: s.AdminProfile}
	}
	cl := s.client(region, creds)
	body, _ := json.Marshal(map[string]any{"ServiceCode": "ec2", "QuotaCode": QuotaCodeSpotStandard, "DesiredValue": desired})
	status, b, err := cl.do(ctx, "servicequotas", "application/x-amz-json-1.1",
		map[string]string{"X-Amz-Target": "ServiceQuotasV20190624.RequestServiceQuotaIncrease"}, body)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg2    string `json:"Message"`
		}
		_ = json.Unmarshal(b, &e)
		if strings.HasSuffix(e.Type, "ResourceAlreadyExistsException") {
			return "already requested", nil
		}
		code := e.Type
		if i := strings.LastIndex(code, "#"); i >= 0 {
			code = code[i+1:]
		}
		return "", &awsError{Status: status, Code: code, Message: e.Message + e.Msg2}
	}
	var v struct {
		RequestedQuota struct{ Status string }
	}
	_ = json.Unmarshal(b, &v)
	return v.RequestedQuota.Status, nil
}

// quotaCheck is doctor's Spot quota finding: it fails only when not even
// one VM fits, and says how to raise the quota whenever the quota is what
// limits the fleet. ok is false when there is nothing to report.
func (a *AWSSpot) quotaCheck(ctx context.Context) (Check, bool) {
	one := MinLaunchVCPUsFor(a.r)
	if one == 0 {
		return Check{}, false // these types use other quotas
	}
	q, err := a.quotaValue(ctx, QuotaCodeSpotStandard)
	if err != nil {
		return Check{}, false
	}
	return quotaFinding(QuotaFit{Region: a.r.Region, Quota: q, OneVM: one, Fleet: a.r.MaxInstances * VMVCPUs(a.r),
		Others: HasOtherFamiliesFor(a.r)}), true
}

// HasOtherFamilies reports instance types outside the standard families.
func HasOtherFamilies(types []string) bool {
	for _, t := range types {
		if !standardFamily(t) {
			return true
		}
	}
	return false
}

func quotaFinding(f QuotaFit) Check {
	c := Check{Name: "spot vCPU quota", OK: !f.Blocked() || f.Others}
	switch {
	case f.Blocked() && f.Others:
		c.Msg = fmt.Sprintf("%d vCPUs for standard Spot instances in %s: not enough for one standard-family VM (%d vCPUs), so only this runner's other instance types can launch. Raise it (as an AWS administrator): %s", f.Quota, f.Region, f.OneVM, f.Command())
	case f.Blocked():
		c.Msg = fmt.Sprintf("%d vCPUs for standard Spot instances in %s: not enough for one VM (%d vCPUs), so nothing can launch. Raise it (as an AWS administrator): %s", f.Quota, f.Region, f.OneVM, f.Command())
	case f.Limited():
		c.Msg = fmt.Sprintf("%d vCPUs for standard Spot instances in %s: fewer VMs run at once than max_instances allows (up to %d vCPUs). To run them all at once (as an AWS administrator): %s", f.Quota, f.Region, f.Fleet, f.Command())
	default:
		c.Msg = fmt.Sprintf("%d vCPUs for standard Spot instances in %s; this runner may use up to %d", f.Quota, f.Region, f.Fleet)
	}
	return c
}
