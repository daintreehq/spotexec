package provider

import (
	"context"
	"fmt"

	"github.com/daintreehq/spotexec/internal/config"
)

// MatchKey identifies the requirements Mismatches judges instances
// against: when it changes, earlier judgements no longer hold.
func MatchKey(r *config.Runner) string { return typesKey(r) }

// Mismatches implements Matcher for one region's instances.
func (a *AWSSpot) Mismatches(ctx context.Context, insts []Instance) map[string]string {
	out := map[string]string{}
	var eligible map[string]bool
	loaded := false
	for _, in := range insts {
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
		if ts, err := a.EligibleTypes(ctx, false); err == nil {
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
		for id, why := range g.Regional(region).Mismatches(ctx, ins) {
			out[id] = why
		}
	}
	return out
}
