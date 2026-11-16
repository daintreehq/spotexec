package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// holdingsEnv is a two-region runner whose repositories hold project
// repo1's images: r-one-1 the current tag t1 (and an older t0), r-two-1
// only t0.
func holdingsEnv(t *testing.T) (*AWSGlobal, *fakeRegionsEC2) {
	t.Helper()
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	f.images = map[string]map[string]string{
		"r-one-1": {"t1": "sha256:one", "t0": "sha256:old"},
		"r-two-1": {"t0": "sha256:old2"},
	}
	f.pushed = map[string]map[string]int64{
		"r-one-1": {"t1": 1794739200123, "t0": 1794652800000},
		"r-two-1": {"t0": 1794600000000},
	}
	return g, f
}

// For each region: whether it holds the checkout's image and when it was
// pushed, the archive keyed by that region's own image, and every tag the
// repository keeps, newest first.
func TestProjectHoldingsImageAndDeps(t *testing.T) {
	g, f := holdingsEnv(t)
	f.cached = map[string]map[string]bool{"r-one-1": {"repo1/k-sha256:one.tar": true}}
	var keyed []string
	pj := &Project{RepoID: "repo1", Image: true, ImageTag: "t1", Deps: true, DepsKeyFor: func(id string) (string, error) {
		keyed = append(keyed, id)
		return "k-" + id, nil
	}}
	hs, err := g.ProjectHoldings(context.Background(), pj)
	if err != nil || len(hs) != 2 || hs[0].Region != "r-one-1" || hs[1].Region != "r-two-1" {
		t.Fatalf("holdings %+v, %v", hs, err)
	}
	one, two := hs[0], hs[1]
	pushed := time.UnixMilli(1794739200123).UTC()
	if one.Image.State != HoldPresent || one.ImageID != "sha256:one" || !one.Pushed.Equal(pushed) ||
		one.Deps.State != HoldPresent || one.DepsKey != "k-sha256:one" {
		t.Fatalf("r-one-1: %+v", one)
	}
	want := []ECRImage{
		{Tags: []string{"t1"}, Digest: "sha256:m-t1", PushedAt: pushed, Bytes: 1000},
		{Tags: []string{"t0"}, Digest: "sha256:m-t0", PushedAt: time.UnixMilli(1794652800000).UTC(), Bytes: 1000},
	}
	if !reflect.DeepEqual(one.Kept, want) || one.KeptErr != "" {
		t.Fatalf("r-one-1 kept %+v (%s), want %+v", one.Kept, one.KeptErr, want)
	}
	// No image here: the archive for it can't exist, and isn't looked for.
	if two.Image.State != HoldMissing || !two.Pushed.IsZero() || two.Deps.State != HoldMissing ||
		!strings.Contains(two.Deps.Why, "no image here") || len(two.Kept) != 1 || two.Kept[0].Tags[0] != "t0" {
		t.Fatalf("r-two-1: %+v", two)
	}
	if !reflect.DeepEqual(keyed, []string{"sha256:one"}) {
		t.Fatalf("deps keys made for %v", keyed)
	}
	if c := f.callsOf("HEAD s3"); len(c) != 1 || !strings.HasPrefix(c[0], "r-one-1 ") {
		t.Fatalf("cache lookups %v", c)
	}
	// Both images were listed page by page.
	if c := f.callsOf("DescribeImages spotexec/repo1"); len(c) != 3 {
		t.Fatalf("listing calls %v", c)
	}
	// Read-only: nothing is created, logged into or configured.
	for _, c := range f.calls {
		if !strings.Contains(c, " DescribeImages ") && !strings.Contains(c, " BatchGetImage ") && !strings.Contains(c, " HEAD s3") && !strings.Contains(c, " DescribeRegions") {
			t.Errorf("unexpected call %q", c)
		}
	}
}

// Without a container image the host's archive key is checked in every
// region; a single-region runner reports its one region.
func TestProjectHoldingsHostDepsSingleRegion(t *testing.T) {
	g, f := holdingsEnv(t)
	f.cached = map[string]map[string]bool{"r-one-1": {"repo1/k1.tar": true}}
	single := g.Regional("r-one-1").ranker()
	hs, err := single.ProjectHoldings(context.Background(), &Project{RepoID: "repo1", Deps: true, DepsKey: "k1"})
	if err != nil || len(hs) != 1 {
		t.Fatalf("holdings %+v, %v", hs, err)
	}
	h := hs[0]
	if h.Region != "r-one-1" || h.Image.State != HoldNotUsed || h.Deps.State != HoldPresent || h.DepsKey != "k1" || h.Kept != nil {
		t.Fatalf("holding %+v", h)
	}
	if c := f.callsOf("DescribeImages"); len(c) != 0 {
		t.Fatalf("listed images for a project without one: %v", c)
	}

	// The cache turned off: nothing to look up.
	resetCalls(f)
	single.r.CacheBucket = "off"
	single.Regional("r-one-1").r.CacheBucket = "off"
	hs, _ = single.ProjectHoldings(context.Background(), &Project{RepoID: "repo1", Deps: true, DepsKey: "k1"})
	if hs[0].Deps.State != HoldMissing || !strings.Contains(hs[0].Deps.Why, "cache_bucket") || len(f.callsOf("HEAD s3")) != 0 {
		t.Fatalf("cache off: %+v", hs[0])
	}
}

// Failures are reported per region, keeping what could be found; a policy
// without ecr:Describe* still tells whether the tag is held.
func TestProjectHoldingsFailures(t *testing.T) {
	g, f := holdingsEnv(t)
	f.describeErr = map[string]string{"r-one-1": "AccessDeniedException"}
	f.imageErr = map[string]string{"r-two-1": "ThrottlingException"}
	f.cacheErr = map[string]int{"r-one-1": 403}
	pj := &Project{RepoID: "repo1", Image: true, ImageTag: "t1", Deps: true, DepsKeyFor: func(id string) (string, error) { return "k-" + id, nil }}
	hs, err := g.ProjectHoldings(context.Background(), pj)
	if err != nil {
		t.Fatal(err)
	}
	one, two := hs[0], hs[1]
	if one.Image.State != HoldPresent || !one.Pushed.IsZero() || !strings.Contains(one.KeptErr, "spotexec setup") ||
		one.Deps.State != HoldUnknown || !strings.Contains(one.Deps.Why, "403") {
		t.Fatalf("r-one-1: %+v", one)
	}
	if two.Image.State != HoldUnknown || !strings.Contains(two.Image.Why, "ThrottlingException") ||
		two.Deps.State != HoldUnknown || !strings.Contains(two.Deps.Why, "couldn't be looked up") || len(two.Kept) != 1 {
		t.Fatalf("r-two-1: %+v", two)
	}

	// No repository in a region: nothing held, nothing kept; an unknown tag
	// isn't looked up.
	f.describeErr, f.imageErr, f.cacheErr = nil, nil, nil
	delete(f.images, "r-two-1")
	pj.ImageTag = ""
	hs, _ = g.ProjectHoldings(context.Background(), pj)
	if two := hs[1]; two.Image.State != HoldUnknown || two.Kept != nil || two.KeptErr != "" || two.Deps.State != HoldUnknown {
		t.Fatalf("no repository, no tag: %+v", two)
	}
	pj.ImageTag = "t1"
	hs, _ = g.ProjectHoldings(context.Background(), pj)
	if two := hs[1]; two.Image.State != HoldMissing || two.Kept != nil || two.KeptErr != "" {
		t.Fatalf("no repository: %+v", two)
	}
}
