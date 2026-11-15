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
	if got["i-r7a"] != "" || !strings.Contains(got["i-c7a"], "at least 32 GiB") || !strings.Contains(got["i-m5a"], "generation 5") {
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
	if got := a.Mismatches(context.Background(), insts[:1]); got["i-c7a"] != "" {
		t.Fatalf("after lowering min_memory_gib: %q", got)
	}
}

// A failed type lookup judges nothing it would need it for: it never
// retires VMs.
func TestMismatchesFailOpen(t *testing.T) {
	f := &fakeRegionsEC2{types: map[string][]string{}}
	a := newSingleEnv(t, f, reqRunner)
	got := a.Mismatches(context.Background(), []Instance{{ID: "i-c7a", Type: "c7a.2xlarge"}, {ID: "i-m5a", Type: "m5a.2xlarge"}})
	if _, ok := got["i-c7a"]; ok {
		t.Fatalf("judged without a type list: %q", got)
	}
	if got["i-m5a"] == "" {
		t.Fatalf("a name rule needs no lookup: %q", got)
	}
}

// A fixed list matches by membership only, so a listed type outside the
// usual families still matches.
func TestMismatchesFixedList(t *testing.T) {
	f := &fakeRegionsEC2{}
	a := newSingleEnv(t, f, func(r *config.Runner) { r.InstanceTypes = []string{"c7a.*", "g5.2xlarge"} })
	got := a.Mismatches(context.Background(), []Instance{{ID: "a", Type: "c7a.4xlarge"}, {ID: "b", Type: "g5.2xlarge"}, {ID: "c", Type: "c6a.2xlarge"}})
	if got["a"] != "" || got["b"] != "" || got["c"] != "c6a.2xlarge is not in instance_types" {
		t.Fatalf("mismatches %q", got)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 0 {
		t.Fatalf("a fixed list described types %d times", n)
	}
}
