package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
)

// A vcpus-based runner's VMs are judged against its current requirements:
// the 16 GiB type it launched before min_memory_gib was raised no longer
// matches, a type it can't judge is left out.
func TestMismatchesByRequirements(t *testing.T) {
	f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()}}
	a := newSingleEnv(t, f, reqRunner)
	insts := []Instance{
		{ID: "i-c7a", Type: "c7a.2xlarge"}, // 16 GiB
		{ID: "i-r7a", Type: "r7a.2xlarge"},
		{ID: "i-m5a", Type: "m5a.2xlarge"}, // generation 5
		{ID: "i-unknown"},
	}
	got := a.Mismatches(context.Background(), insts)
	if why, ok := got["i-r7a"]; !ok || why != "" {
		t.Fatalf("matching VM not judged a match: %q", got)
	}
	if !strings.Contains(got["i-c7a"], "at least 32 GiB") || !strings.Contains(got["i-m5a"], "generation 5") {
		t.Fatalf("mismatches %q", got)
	}
	if _, ok := got["i-unknown"]; ok {
		t.Fatalf("an instance without a type was judged: %q", got)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 1 {
		t.Fatalf("described %d times, want 1", n)
	}

	// Lowering the requirement brings it back.
	a.r.MinMemoryGiB = 16
	if got := a.Mismatches(context.Background(), insts[:1]); got["i-c7a"] != "" || len(got) != 1 {
		t.Fatalf("after lowering min_memory_gib: %q", got)
	}
}

// A failed type lookup judges nothing it would need it for: it never
// retires VMs.
func TestMismatchesFailOpen(t *testing.T) {
	f := &fakeRegionsEC2{typesErr: map[string]string{"r-one-1": "RequestLimitExceeded"}}
	a := newSingleEnv(t, f, reqRunner)
	got := a.Mismatches(context.Background(), []Instance{{ID: "i-c7a", Type: "c7a.2xlarge"}, {ID: "i-m5a", Type: "m5a.2xlarge"}})
	if _, ok := got["i-c7a"]; ok {
		t.Fatalf("judged without a type list: %q", got)
	}
	if got["i-m5a"] == "" {
		t.Fatalf("a name rule needs no lookup: %q", got)
	}
}

// When the region was described and no type qualifies, every VM there
// mismatches (that is an answer, not a failed lookup), and the answer is
// cached.
func TestMismatchesNothingQualifies(t *testing.T) {
	f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()}}
	a := newSingleEnv(t, f, func(r *config.Runner) { reqRunner(r); r.MinMemoryGiB = 512 })
	for range 2 {
		got := a.Mismatches(context.Background(), []Instance{{ID: "i-r7a", Type: "r7a.2xlarge"}})
		if !strings.Contains(got["i-r7a"], "at least 512 GiB") {
			t.Fatalf("mismatches %q", got)
		}
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 1 {
		t.Fatalf("described %d times, want 1 (an empty answer is cached)", n)
	}
	if _, err := a.EligibleTypes(context.Background(), false); err == nil || !strings.Contains(err.Error(), "no instance type in r-one-1") {
		t.Fatalf("cached empty answer: %v", err)
	}
}

// A fixed list matches by membership only, so a listed type outside the
// usual families still matches.
func TestMismatchesFixedList(t *testing.T) {
	f := &fakeRegionsEC2{}
	a := newSingleEnv(t, f, func(r *config.Runner) { r.InstanceTypes = []string{"c7a.*", "g5.2xlarge"} })
	got := a.Mismatches(context.Background(), []Instance{{ID: "a", Type: "c7a.4xlarge"}, {ID: "b", Type: "g5.2xlarge"}, {ID: "c", Type: "c6a.2xlarge"}})
	if len(got) != 3 || got["a"] != "" || got["b"] != "" || got["c"] != "c6a.2xlarge is not in instance_types" {
		t.Fatalf("mismatches %q", got)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 0 {
		t.Fatalf("a fixed list described types %d times", n)
	}
}

// A VM in a region the runner no longer lists mismatches whatever its
// type, without asking that region anything; the listed regions are
// judged as usual.
func TestMismatchesDroppedRegion(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	insts := []Instance{
		{ID: "a", Type: "c7a.2xlarge", Region: "r-one-1"},
		{ID: "b", Type: "c6a.2xlarge", Region: "r-two-1"},
		{ID: "c", Type: "c7a.2xlarge", Region: "r-three-1"},
		{ID: "d", Type: "g5.2xlarge", Region: "r-two-1"},
		{ID: "e", Region: "r-three-1"}, // no type: still in a dropped region
	}
	got := g.Mismatches(context.Background(), insts)
	if got["a"] != "" || got["b"] != "" || got["d"] != "g5.2xlarge is not in instance_types" {
		t.Fatalf("mismatches %q", got)
	}
	for _, id := range []string{"c", "e"} {
		if why := got[id]; !strings.Contains(why, "r-three-1") || !strings.Contains(why, "r-one-1, r-two-1") {
			t.Fatalf("VM %s in a dropped region: %q", id, why)
		}
	}

	// A dropped region's types are never looked up, even for requirements.
	g = newGlobalEnv(t, f, "r-one-1")
	reqRunner(g.r)
	if got = g.Mismatches(context.Background(), insts[2:3]); !strings.Contains(got["c"], "r-three-1") {
		t.Fatalf("requirements, dropped region: %q", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("judging asked AWS: %v", f.calls)
	}

	// Moved back to one region while VMs remain elsewhere.
	g = newGlobalEnv(t, f, "r-one-1")
	g.legacy = true
	got = g.Mismatches(context.Background(), insts[:3])
	if got["a"] != "" || !strings.Contains(got["b"], "r-two-1") || !strings.Contains(got["c"], "r-three-1") {
		t.Fatalf("one region left: %q", got)
	}
	// A single-region placement has no regions list, only its region.
	g = newGlobalEnv(t, f)
	g.single = true
	got = g.Mismatches(context.Background(), insts[:2])
	if got["a"] != "" || !strings.Contains(got["b"], "r-two-1") {
		t.Fatalf("single region: %q", got)
	}

	// "all" keeps every region.
	g = newGlobalEnv(t, f, config.AllRegions)
	got = g.Mismatches(context.Background(), insts[:3])
	if len(got) != 3 || got["a"] != "" || got["b"] != "" || got["c"] != "" {
		t.Fatalf("all regions: %q", got)
	}
	// ... except the ones exclude_regions leaves out.
	g.r.ExcludeRegions = []string{"r-three-1"}
	got = g.Mismatches(context.Background(), insts[:3])
	if got["a"] != "" || got["b"] != "" || !strings.Contains(got["c"], "r-three-1 is no longer one of the runner's regions") {
		t.Fatalf("all regions, one excluded: %q", got)
	}
}

// A single-region runner judging a cached listing from before its region
// changed: the VM elsewhere mismatches, one without a region is judged by
// type as before.
func TestMismatchesOtherRegionSingle(t *testing.T) {
	f := &fakeRegionsEC2{}
	a := newSingleEnv(t, f, nil)
	got := a.Mismatches(context.Background(), []Instance{
		{ID: "a", Type: "c7a.2xlarge", Region: "r-one-1"},
		{ID: "b", Type: "c7a.2xlarge", Region: "r-two-1"},
		{ID: "c", Type: "c7a.2xlarge"},
	})
	if len(got) != 3 || got["a"] != "" || got["c"] != "" || got["b"] != "r-two-1 is no longer one of the runner's regions: r-one-1" {
		t.Fatalf("mismatches %q", got)
	}
}

// The match key changes with the set of regions, so the pool judges its VMs
// again when they change, but not when they are only reordered.
func TestMatchKeyCoversRegions(t *testing.T) {
	r := &config.Runner{Region: "r-one-1", Regions: []string{"r-one-1", "r-two-1", "r-three-1"}, InstanceTypes: []string{"c7a.2xlarge"}}
	k := MatchKey(r)
	diff := func(what string, edit func(r *config.Runner)) {
		t.Helper()
		c := *r
		edit(&c)
		if MatchKey(&c) == k {
			t.Fatalf("%s kept the match key", what)
		}
	}
	diff("narrowing to one region", func(r *config.Runner) { r.Regions = nil })
	diff("narrowing", func(r *config.Runner) { r.Regions = []string{"r-one-1", "r-two-1"} })
	diff("replacing a region", func(r *config.Runner) { r.Regions = []string{"r-one-1", "r-two-1", "r-four-1"} })
	diff("all regions", func(r *config.Runner) { r.Regions = []string{config.AllRegions} })
	all := config.Runner{Type: config.TypeAWSSpot, Regions: []string{config.AllRegions}, InstanceTypes: r.InstanceTypes}
	allKey := MatchKey(&all)
	all.ExcludeRegions = []string{"r-two-1", "r-one-1"}
	if MatchKey(&all) == allKey {
		t.Fatal("excluding a region kept the match key")
	}
	reordered := all
	reordered.ExcludeRegions = []string{"r-one-1", "r-two-1"}
	if MatchKey(&reordered) != MatchKey(&all) {
		t.Fatal("reordering exclude_regions changed the match key")
	}
	one := &config.Runner{Region: "r-one-1", InstanceTypes: r.InstanceTypes}
	if moved := (config.Runner{Region: "r-two-1", InstanceTypes: r.InstanceTypes}); MatchKey(&moved) == MatchKey(one) {
		t.Fatal("changing region kept the match key")
	}
	same := *r
	same.Region, same.Regions = "r-two-1", []string{"r-three-1", "r-two-1", "r-one-1"}
	if MatchKey(&same) != k {
		t.Fatal("reordering regions changed the match key")
	}
}
