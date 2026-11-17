package provider

import (
	"context"
	"testing"
	"time"
)

// launchedAgo records a launch sent to region d ago, as Launch's mark does.
func launchedAgo(t *testing.T, g *AWSGlobal, region string, d time.Duration) {
	t.Helper()
	g.now = func() time.Time { return time.Now().Add(-d) }
	defer func() { g.now = nil }()
	if err := g.mark(region)(); err != nil {
		t.Fatal(err)
	}
}

// A region removed from the config stops being listed as soon as it's
// confirmed empty, not when its last launch's hold runs out (#242).
func TestGlobalRemovedRegionDroppedOnceEmpty(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	launchedAgo(t, g, "r-two-1", 30*time.Minute) // held for another ~2.5h
	g.r.Regions = []string{"r-one-1"}            // then removed from the config
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "" {
		t.Fatalf("an empty region no longer configured is still listed: %q", got)
	}
	if got := f.callsOf("DescribeInstances owned"); len(got) != 1 || got[0] != "r-two-1 DescribeInstances owned" {
		t.Fatalf("dropped without checking for stopped VMs and builders: %v", f.callsOf("DescribeInstances"))
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("DescribeInstances"); len(got) != 0 {
		t.Fatalf("a dropped region is still queried: %v", got)
	}
}

// A region still configured keeps its hold: it's where the next launch may
// go, and listing it is what the hold is for.
func TestGlobalConfiguredRegionKeptWhileHeld(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	launchedAgo(t, g, "r-two-1", 30*time.Minute)
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("a configured region was dropped inside its hold: %q", got)
	}
	if got := f.callsOf("DescribeInstances owned"); len(got) != 0 {
		t.Fatalf("checked leftovers of a held, configured region: %v", got)
	}
}

// An exclusion takes a region out of the config like removing it does.
func TestGlobalExcludedRegionDroppedOnceEmpty(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "all")
	discovered(t, g)
	launchedAgo(t, g, "r-two-1", 30*time.Minute)
	g.r.ExcludeRegions = []string{"r-two-1"}
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "" {
		t.Fatalf("an empty excluded region is still listed: %q", got)
	}
}

// A launch too recent for EC2's listing to be sure to show it keeps the
// region listed, whatever the listing found.
func TestGlobalRemovedRegionKeptAfterRecentLaunch(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	launchedAgo(t, g, "r-two-1", time.Minute)
	g.r.Regions = []string{"r-one-1"}
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("a region launched in a minute ago was dropped: %q", got)
	}
}

// A removed region that still holds a stopped VM or a bake builder, or
// whose leftovers can't be checked, stays listed.
func TestGlobalRemovedRegionKeptWhileItHoldsAnything(t *testing.T) {
	old := regionListTimeout
	regionListTimeout = 300 * time.Millisecond
	t.Cleanup(func() { regionListTimeout = old })
	for name, f := range map[string]*fakeRegionsEC2{
		"builder":   {builders: map[string]bool{"r-two-1": true}},
		"stopped":   {instances: map[string]map[string]string{"r-two-1": {"i-off": "c7a.2xlarge"}}, state: map[string]string{"i-off": "stopped"}},
		"unlisted":  {listErr: map[string]string{"r-two-1": "RequestLimitExceeded"}},
		"unchecked": {hangOwned: map[string]bool{"r-two-1": true}},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
			discovered(t, g)
			launchedAgo(t, g, "r-two-1", 30*time.Minute)
			g.r.Regions = []string{"r-one-1"}
			_, _, _ = g.ListReachable(context.Background())
			if got := active(t, g); got != "r-two-1" {
				t.Fatalf("a region that may still hold something was dropped: %q", got)
			}
		})
	}
}

// A launch sent there while the listing was checking it keeps the region,
// whatever the check found.
func TestGlobalRemovedRegionKeptOnLaunchDuringCheck(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	launchedAgo(t, g, "r-two-1", 30*time.Minute)
	g.r.Regions = []string{"r-one-1"}
	f.onOwned = func(region string) {
		if err := g.mark(region)(); err != nil {
			t.Error(err)
		}
	}
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("a region launched in during its check was dropped: %q", got)
	}
}

// State kept before last launches were recorded has holds only: a removed
// region's last launch counts from the first empty listing, and the region
// is dropped once that has settled.
func TestGlobalRemovedRegionFromOldStateDroppedAfterSettling(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1")
	discovered(t, g)
	if err := g.markActive("r-two-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("dropped with no record of its last launch: %q", got)
	}
	g.now = func() time.Time { return time.Now().Add(launchSettle + time.Minute) }
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "" {
		t.Fatalf("still listed once settled: %q", got)
	}
}

// A runner moved from regions back to one region drops its old regions
// once they're empty too.
func TestGlobalLegacyRegionDroppedOnceEmpty(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	launchedAgo(t, g, "r-two-1", 30*time.Minute)
	g.r.Regions = nil
	g.legacy = true
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "" {
		t.Fatalf("an empty old region is still listed: %q", got)
	}
}
