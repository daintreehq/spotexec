package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// rankFor ranks g's options for a launch for project pj.
func rankFor(t *testing.T, g *AWSGlobal, pj *Project) Ranking {
	t.Helper()
	tbl, err := g.Prices(WithProject(context.Background(), pj), false)
	if err != nil {
		t.Fatal(err)
	}
	return g.Rank(tbl)
}

// A bake makes a region warm for the boot part on any machine sharing the
// account: no VM of the runner has to have run there from this one.
func TestWarmthBakeWarmsBoot(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	f.bakes = map[string]bool{"r-one-1": true}
	k := rankFor(t, g, nil)
	one, two := costIn(t, k, "r-one-1"), costIn(t, k, "r-two-1")
	if !one.ColdParts.Boot.Warm || one.ColdParts.Boot.Cost != 0 || one.ColdParts.Boot.Why != "a bake of the runner" {
		t.Fatalf("baked region: %+v", one.ColdParts.Boot)
	}
	if two.ColdParts.Boot.Warm || !near(two.ColdParts.Boot.Cost, (0.096+1)*1/60) || two.ColdParts.Boot.Why != "no bake of the runner" {
		t.Fatalf("unbaked region: %+v", two.ColdParts.Boot)
	}
	// Without history the project's parts are cold in both: the bake is
	// the difference, and pays for the price gap.
	if one.ColdParts.Paid() != "deps,image" || two.ColdParts.Paid() != "boot,deps,image" || k.Prices[0].Region != "r-one-1" {
		t.Fatalf("order %v: one %+v, two %+v", k.Prices, *one.ColdParts, *two.ColdParts)
	}

	// The lookup is kept a while: the next ranking asks nothing.
	resetCalls(f)
	rankFor(t, g, nil)
	if c := f.callsOf("DescribeImages bakes"); len(c) != 0 {
		t.Fatalf("looked again: %v", c)
	}
	// A new bake elsewhere is seen once this machine forgets what it found
	// (a bake from here does that), or after evidenceTTL.
	f.mu.Lock()
	f.bakes["r-two-1"] = true
	f.mu.Unlock()
	ForgetBakeWarmth(g.r)
	if c := costIn(t, rankFor(t, g, nil), "r-two-1"); !c.ColdParts.Boot.Warm {
		t.Fatalf("after a bake: %+v", c.ColdParts.Boot)
	}
	f.mu.Lock()
	f.bakes["r-two-1"] = false
	f.mu.Unlock()
	now := time.Now().Add(evidenceTTL + time.Minute)
	g.now = func() time.Time { return now }
	if c := costIn(t, rankFor(t, g, nil), "r-two-1"); c.ColdParts.Boot.Warm {
		t.Fatalf("bake gone, past the TTL: %+v", c.ColdParts.Boot)
	}

	// A pinned AMI boots no bake anywhere.
	g.r.AMI = "ami-pinned"
	if c := costIn(t, rankFor(t, g, nil), "r-one-1"); c.ColdParts.Boot.Warm || !strings.Contains(c.ColdParts.Boot.Why, "pinned") {
		t.Fatalf("pinned ami: %+v", c.ColdParts.Boot)
	}
}

// For a project, a region is warm for dependencies only when it holds the
// archive for the run's exact key, whatever VMs of the runner ran there.
func TestWarmthProjectDepsByExactKey(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	warmUp(t, g, f, "r-one-1") // ran for another project, or an older lockfile
	f.cached = map[string]map[string]bool{"r-two-1": {"repo1/k1.tar": true}}
	pj := &Project{RepoID: "repo1", Deps: true, DepsKey: "k1"}
	k := rankFor(t, g, pj)
	one, two := costIn(t, k, "r-one-1"), costIn(t, k, "r-two-1")
	if one.ColdParts.Deps.Warm || one.ColdParts.Deps.Why != "no dependency archive for the project" || !near(one.Cold, (0.100+1)*2/60) || one.Warm {
		t.Fatalf("history only: %+v %+v", one, *one.ColdParts)
	}
	if !two.Warm || two.Cold != 0 || two.ColdParts.Image.Why != "no image to build" || k.Prices[0].Region != "r-two-1" {
		t.Fatalf("archive held: %+v %+v (order %v)", two, *two.ColdParts, k.Prices)
	}
	// The launch for the project goes there and says why.
	in, err := g.Launch(WithProject(context.Background(), pj), t.Logf)
	if err != nil || in.Region != "r-two-1" || !in.Placement.Cost.Warm || !strings.Contains(in.Placement.Why, "warm region") {
		t.Fatalf("launched %+v, %v", in, err)
	}
	// After the lockfile changes, nothing holds the new key.
	pj2 := &Project{RepoID: "repo1", Deps: true, DepsKey: "k2"}
	if c := costIn(t, rankFor(t, g, pj2), "r-two-1"); c.ColdParts.Deps.Warm {
		t.Fatalf("new key: %+v", c.ColdParts.Deps)
	}
	// Without a project, history decides the project's parts, as before.
	if c := costIn(t, rankFor(t, g, nil), "r-one-1"); !c.Warm || c.WarmSeen.IsZero() || !strings.Contains(c.ColdParts.Deps.Why, "a VM of the runner ran here lately") {
		t.Fatalf("no project: %+v %+v", c, *c.ColdParts)
	}
}

// A Dockerfile project is warm for its image where the region's registry
// holds the exact tag, and for dependencies where the cache holds the
// archive keyed by that region's own image.
func TestWarmthProjectImage(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	f.images = map[string]map[string]string{"r-one-1": {"t-old": "sha256:old"}, "r-two-1": {"t1": "sha256:two"}}
	f.cached = map[string]map[string]bool{"r-two-1": {"repo1/k-sha256:two.tar": true}, "r-one-1": {"repo1/k-sha256:old.tar": true}}
	pj := &Project{RepoID: "repo1", Image: true, ImageTag: "t1", Deps: true,
		DepsKeyFor: func(id string) (string, error) { return "k-" + id, nil }}
	k := rankFor(t, g, pj)
	one, two := costIn(t, k, "r-one-1"), costIn(t, k, "r-two-1")
	if one.ColdParts.Paid() != "deps,image" || one.ColdParts.Image.Why != "no image of the project" ||
		!strings.Contains(one.ColdParts.Deps.Why, "no image of the project") {
		t.Fatalf("older image only: %+v", *one.ColdParts)
	}
	if !two.Warm || two.ColdParts.Image.Why != "the project's image" || two.ColdParts.Deps.Why != "the project's dependency archive" {
		t.Fatalf("image and archive held: %+v", *two.ColdParts)
	}
	if c := f.callsOf("BatchGetImage"); len(c) != 2 || !strings.Contains(c[0], "spotexec/repo1:t1") {
		t.Fatalf("ECR calls %v", c)
	}
	// No repository at all is cold too.
	g2, f2 := twoRegionEnv(t, "0.100", "0.096", nil)
	f2.images = map[string]map[string]string{"r-two-1": {"t1": "sha256:two"}}
	if c := costIn(t, rankFor(t, g2, pj), "r-one-1"); c.ColdParts.Image.Warm || c.ColdParts.Image.Why != "no image of the project" {
		t.Fatalf("no repository: %+v", c.ColdParts.Image)
	}
}

// A lookup that fails says nothing: the part is left to history, and it is
// tried again soon rather than kept.
func TestWarmthLookupFailureFallsBackToHistory(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	warmUp(t, g, f, "r-one-1")
	f.cacheErr = map[string]int{"r-one-1": 403}
	f.imageErr = map[string]string{"r-one-1": "AccessDeniedException"}
	pj := &Project{RepoID: "repo1", Deps: true, DepsKey: "k1", Image: true, ImageTag: "t1"}
	c := costIn(t, rankFor(t, g, pj), "r-one-1")
	if !c.Warm || !strings.HasPrefix(c.ColdParts.Deps.Why, "lookup failed: ") || !strings.Contains(c.ColdParts.Deps.Why, "a VM of the runner ran here lately") ||
		!strings.Contains(c.ColdParts.Image.Why, "AccessDeniedException") {
		t.Fatalf("failed lookups: %+v", *c.ColdParts)
	}
	resetCalls(f)
	rankFor(t, g, pj)
	if n := len(f.callsOf("BatchGetImage")); n != 0 {
		t.Fatalf("retried at once: %d", n)
	}
	now := time.Now().Add(evidenceRetry + time.Second)
	g.now = func() time.Time { return now }
	rankFor(t, g, pj)
	if n := len(f.callsOf("BatchGetImage")); n != 1 {
		t.Fatalf("not retried after evidenceRetry: %d", n)
	}
}

// One region, or ranking by price, looks nothing up.
func TestWarmthOnlyForRunCostAcrossRegions(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", func(r *config.Runner) { r.LaunchOrder = config.LaunchCheapest })
	rankFor(t, g, &Project{RepoID: "repo1", Deps: true, DepsKey: "k1"})
	if c := append(f.callsOf("DescribeImages bakes"), f.callsOf("s3")...); len(c) != 0 {
		t.Fatalf("cheapest ranking looked up %v", c)
	}
}
