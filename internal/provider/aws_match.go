package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/daintreehq/spotexec/internal/config"
)

// MatchKey identifies the requirements Mismatches judges instances
// against (instance types and regions): when it changes, earlier
// judgements no longer hold.
func MatchKey(r *config.Runner) string {
	// The set of regions, not their order (nor the home region of
	// several): a change that leaves it alone keeps earlier verdicts.
	regions := r.Region
	if len(r.Regions) > 0 {
		rs := slices.Clone(r.Regions)
		slices.Sort(rs)
		regions = strings.Join(rs, ",")
	}
	if r.AnyRegion() && len(r.ExcludeRegions) > 0 {
		ex := slices.Clone(r.ExcludeRegions)
		slices.Sort(ex)
		regions += "-" + strings.Join(ex, ",")
	}
	return typesKey(r) + "|" + regions
}

// droppedRegion is the mismatch of a VM in a region the runner no longer
// uses (where names the ones it does).
func droppedRegion(region, where string) string {
	return fmt.Sprintf("%s is no longer one of the runner's regions: %s", region, where)
}

// Mismatches implements Matcher for one region's instances.
func (a *AWSSpot) Mismatches(ctx context.Context, insts []Instance) map[string]string {
	out := map[string]string{}
	var eligible map[string]bool
	loaded := false
	for _, in := range insts {
		if in.Region != "" && in.Region != a.r.Region {
			// Listed before the runner moved to this region.
			out[in.ID] = droppedRegion(in.Region, a.r.Region)
			continue
		}
		if in.Type == "" {
			continue
		}
		why, ok := a.typeMismatch(ctx, in.Type, &eligible, &loaded)
		if ok {
			out[in.ID] = why
		}
	}
	return out
}

// typeMismatch says why instance type t no longer satisfies the runner's
// requirements ("" when it does); ok is false when that can't be told. The
// region's eligible types are loaded at most once per call, into eligible
// (nil after a failed lookup).
func (a *AWSSpot) typeMismatch(ctx context.Context, t string, eligible *map[string]bool, loaded *bool) (why string, ok bool) {
	r := a.r
	if !r.ByRequirements() {
		// Fixed lists launch exactly the listed types (patterns included),
		// whatever their family: membership is the whole requirement.
		if listedIndex(r, t) < 0 {
			return t + " is not in instance_types", true
		}
		return "", true
	}
	if x := excludedName(t, r); x != "" {
		return fmt.Sprintf("%s: %s", t, x), true
	}
	if !*loaded {
		*loaded = true
		ts, err := a.EligibleTypes(ctx, false)
		var none *noEligibleError
		if err == nil || errors.As(err, &none) {
			// Nothing qualifying is a verdict too: every VM mismatches.
			*eligible = map[string]bool{}
			for _, x := range ts {
				(*eligible)[x.Type] = true
			}
		}
	}
	if *eligible == nil {
		return "", false
	}
	if !(*eligible)[t] {
		return fmt.Sprintf("%s doesn't have %s", t, describeRequirements(r)), true
	}
	return "", true
}

// Mismatches implements Matcher, judging each instance in its own region.
// An instance in a region the runner no longer uses mismatches whatever its
// type (it is still listed until it ends: see newLegacyAWSGlobal).
func (g *AWSGlobal) Mismatches(ctx context.Context, insts []Instance) map[string]string {
	byRegion := map[string][]Instance{}
	for _, in := range insts {
		region := in.Region
		if region == "" {
			region = g.r.Region
		}
		byRegion[region] = append(byRegion[region], in)
	}
	out := map[string]string{}
	for region, ins := range byRegion {
		if !g.usesRegion(region) {
			for _, in := range ins {
				out[in.ID] = droppedRegion(region, g.Where())
			}
			continue
		}
		for id, why := range g.Regional(region).Mismatches(ctx, ins) {
			out[id] = why
		}
	}
	return out
}

// usesRegion reports whether the runner's config still lists region (with
// regions = ["all"], every region exclude_regions doesn't leave out).
func (g *AWSGlobal) usesRegion(region string) bool {
	if g.oneRegion() {
		return region == g.r.Region
	}
	if g.r.AnyRegion() {
		return !contains(g.excluded(), region)
	}
	return contains(g.r.Regions, region)
}
