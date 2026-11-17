package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// testClock is a settable clock for g.now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func fixClock(g *AWSGlobal) *testClock {
	c := &testClock{t: time.Now().Truncate(time.Second)}
	g.now = c.now
	return c
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// launchedAgo records a launch sent to region d before the clock's time,
// as Launch's mark does.
func launchedAgo(t *testing.T, g *AWSGlobal, c *testClock, region string, d time.Duration) {
	t.Helper()
	now := c.now()
	c.set(now.Add(-d))
	defer c.set(now)
	if err := g.mark(region)(); err != nil {
		t.Fatal(err)
	}
}

func busyAt(t *testing.T, g *AWSGlobal, region string) time.Time {
	t.Helper()
	var at time.Time
	if err := g.withState(func(st *placementState) { at = st.Busy[region] }); err != nil {
		t.Fatal(err)
	}
	return at
}

func listOK(t *testing.T, g *AWSGlobal) {
	t.Helper()
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A region removed from the config stops being listed as soon as it's
// confirmed empty, not when its last launch's hold runs out (#242).
func TestGlobalRemovedRegionDroppedOnceEmpty(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	c := fixClock(g)
	discovered(t, g)
	launchedAgo(t, g, c, "r-two-1", 30*time.Minute) // held for another ~2.5h
	g.r.Regions = []string{"r-one-1"}               // then removed from the config
	listOK(t, g)
	if got := active(t, g); got != "" {
		t.Fatalf("an empty region no longer configured is still listed: %q", got)
	}
	if got := f.callsOf("DescribeInstances owned"); len(got) != 1 || got[0] != "r-two-1 DescribeInstances owned" {
		t.Fatalf("dropped without checking for stopped VMs and builders: %v", f.callsOf("DescribeInstances"))
	}
	if !busyAt(t, g, "r-two-1").IsZero() {
		t.Fatal("a dropped region's activity record was kept")
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	listOK(t, g)
	if got := f.callsOf("DescribeInstances"); len(got) != 0 {
		t.Fatalf("a dropped region is still queried: %v", got)
	}
}

// A region still configured keeps its hold: it's where the next launch may
// go, and listing it is what the hold is for.
func TestGlobalConfiguredRegionKeptWhileHeld(t *testing.T) {
	for name, regions := range map[string][]string{
		"listed": {"r-one-1", "r-two-1"},
		"all":    {config.AllRegions},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRegionsEC2{}
			g := newGlobalEnv(t, f, regions...)
			c := fixClock(g)
			discovered(t, g)
			launchedAgo(t, g, c, "r-two-1", 30*time.Minute)
			listOK(t, g)
			if got := active(t, g); got != "r-two-1" {
				t.Fatalf("a configured region was dropped inside its hold: %q", got)
			}
			if got := f.callsOf("DescribeInstances owned"); len(got) != 0 {
				t.Fatalf("checked leftovers of a held, configured region: %v", got)
			}
		})
	}
}

// An exclusion takes a region out of the config like removing it does.
func TestGlobalExcludedRegionDroppedOnceEmpty(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, config.AllRegions)
	c := fixClock(g)
	discovered(t, g)
	launchedAgo(t, g, c, "r-two-1", 30*time.Minute)
	g.r.ExcludeRegions = []string{"r-two-1"}
	listOK(t, g)
	if got := active(t, g); got != "" {
		t.Fatalf("an empty excluded region is still listed: %q", got)
	}
}

func TestGlobalConfigured(t *testing.T) {
	g := newGlobalEnv(t, &fakeRegionsEC2{}, "r-one-1", "r-two-1")
	for _, tc := range []struct {
		regions, exclude []string
		legacy           bool
		region           string
		want             bool
	}{
		{regions: []string{"r-one-1", "r-two-1"}, region: "r-two-1", want: true},
		{regions: []string{"r-one-1", "r-two-1"}, region: "r-three-1", want: false},
		{regions: []string{config.AllRegions}, region: "r-three-1", want: true},
		{regions: []string{config.AllRegions}, exclude: []string{"r-three-1"}, region: "r-three-1", want: false},
		{legacy: true, region: "r-one-1", want: true},
		{legacy: true, region: "r-two-1", want: false},
	} {
		g.r.Regions, g.r.ExcludeRegions, g.legacy = tc.regions, tc.exclude, tc.legacy
		if got := g.configured(tc.region); got != tc.want {
			t.Errorf("regions %v exclude %v legacy %v: configured(%s) = %v", tc.regions, tc.exclude, tc.legacy, tc.region, got)
		}
	}
	if got := len(g.regional); got != 0 {
		t.Fatalf("configured asked the cloud (%d regional providers made)", got)
	}
}

// Activity too recent for EC2's listing to be sure to show what it made
// keeps the region listed, whatever the listing found. The cutoff is
// launchSettle before the listing began.
func TestGlobalRemovedRegionSettleCutoff(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		kept bool
	}{
		{time.Minute, true},
		{launchSettle - time.Second, true},
		{launchSettle, false},
		{launchSettle + time.Second, false},
	} {
		f := &fakeRegionsEC2{}
		g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
		c := fixClock(g)
		discovered(t, g)
		launchedAgo(t, g, c, "r-two-1", tc.ago)
		g.r.Regions = []string{"r-one-1"}
		listOK(t, g)
		if kept := active(t, g) == "r-two-1"; kept != tc.kept {
			t.Errorf("launched %s before the listing: kept = %v, want %v", tc.ago, kept, tc.kept)
		}
		if checked := len(f.callsOf("DescribeInstances owned")) > 0; checked == tc.kept {
			t.Errorf("launched %s before the listing: leftovers checked = %v", tc.ago, checked)
		}
	}
}

// The cutoff counts from when the listing began, not when it ended: a slow
// listing must not count its own time as settling.
func TestGlobalRemovedRegionSettlesFromListingStart(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	c := fixClock(g)
	discovered(t, g)
	launchedAgo(t, g, c, "r-two-1", launchSettle-time.Minute)
	g.r.Regions = []string{"r-one-1"}
	start := c.now()
	slowed := false
	f.onList = func(region string) {
		if region == "r-two-1" {
			c.set(start.Add(2 * time.Minute))
			slowed = true
		}
	}
	listOK(t, g)
	if !slowed {
		t.Fatal("the removed region was never listed")
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("dropped on a listing begun inside the settle window: %q", got)
	}
}

// A removed region that still holds a stopped VM or a bake builder, or
// whose leftovers can't be checked, stays listed, and is dropped once it
// holds nothing.
func TestGlobalRemovedRegionKeptWhileItHoldsAnything(t *testing.T) {
	for name, f := range map[string]*fakeRegionsEC2{
		"builder":   {builders: map[string]bool{"r-two-1": true}},
		"stopped":   {instances: map[string]map[string]string{"r-two-1": {"i-off": "c7a.2xlarge"}}, state: map[string]string{"i-off": "stopped"}},
		"unchecked": {hangOwned: map[string]bool{"r-two-1": true}},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "unchecked" {
				old := regionListTimeout
				regionListTimeout = 300 * time.Millisecond
				t.Cleanup(func() { regionListTimeout = old })
			}
			g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
			c := fixClock(g)
			discovered(t, g)
			launchedAgo(t, g, c, "r-two-1", 30*time.Minute)
			g.r.Regions = []string{"r-one-1"}
			ins, unlisted, err := g.ListReachable(context.Background())
			if err != nil || len(ins) != 0 || len(unlisted) != 0 {
				t.Fatalf("listing = %v, %v, %v", ins, unlisted, err)
			}
			if got := f.callsOf("DescribeInstances owned"); len(got) == 0 {
				t.Fatal("leftovers never checked")
			}
			if got := active(t, g); got != "r-two-1" {
				t.Fatalf("a region that may still hold something was dropped: %q", got)
			}
			f.mu.Lock()
			f.builders, f.instances, f.hangOwned = nil, map[string]map[string]string{}, nil
			f.mu.Unlock()
			listOK(t, g)
			if got := active(t, g); got != "" {
				t.Fatalf("still listed once it holds nothing: %q", got)
			}
		})
	}
}

// A removed region that can't be listed at all stays listed.
func TestGlobalRemovedRegionKeptWhileUnlisted(t *testing.T) {
	f := &fakeRegionsEC2{listErr: map[string]string{"r-two-1": "UnauthorizedOperation"}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	c := fixClock(g)
	discovered(t, g)
	launchedAgo(t, g, c, "r-two-1", 30*time.Minute)
	g.r.Regions = []string{"r-one-1"}
	if _, err := g.List(context.Background()); err == nil {
		t.Fatal("a region that couldn't be listed listed fine")
	}
	if got := f.callsOf("DescribeInstances owned"); len(got) != 0 {
		t.Fatalf("checked leftovers after a failed listing: %v", got)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("a region that couldn't be listed was dropped: %q", got)
	}
}

// A launch sent there, a bake begun there, or an instance seen there
// (however old) while the listing was checking it keeps the region,
// whatever the check found.
func TestGlobalRemovedRegionKeptOnActivityDuringCheck(t *testing.T) {
	for name, act := range map[string]func(g *AWSGlobal, region string) error{
		"launch": func(g *AWSGlobal, region string) error { return g.mark(region)() },
		"bake": func(g *AWSGlobal, region string) error {
			_, err := g.Use(region)
			return err
		},
		"sighting": func(g *AWSGlobal, region string) error {
			return g.remember([]Instance{{ID: "i-old", Region: region, LaunchedAt: g.clock().Add(-time.Hour)}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRegionsEC2{}
			g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
			c := fixClock(g)
			discovered(t, g)
			launchedAgo(t, g, c, "r-two-1", 30*time.Minute)
			g.r.Regions = []string{"r-one-1"}
			calls := 0
			f.onOwned = func(region string) {
				calls++
				if err := act(g, region); err != nil {
					t.Error(err)
				}
			}
			listOK(t, g)
			if calls != 1 {
				t.Fatalf("leftovers checked %d times", calls)
			}
			if got := active(t, g); got != "r-two-1" {
				t.Fatalf("a region active during its check was dropped: %q", got)
			}
		})
	}
}

// State kept before activity was recorded has holds only: a removed
// region's activity counts from the first empty listing, and the region is
// dropped once that has settled.
func TestGlobalRemovedRegionFromOldStateDroppedAfterSettling(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1")
	c := fixClock(g)
	discovered(t, g)
	if err := g.markActive("r-two-1", c.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	listOK(t, g)
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("dropped with no record of its last activity: %q", got)
	}
	if got := f.callsOf("DescribeInstances owned"); len(got) != 0 {
		t.Fatalf("checked leftovers before settling: %v", got)
	}
	first := busyAt(t, g, "r-two-1")
	if !first.Equal(c.now()) {
		t.Fatalf("baseline = %v, want %v", first, c.now())
	}
	c.set(c.now().Add(time.Minute))
	listOK(t, g)
	if got := busyAt(t, g, "r-two-1"); !got.Equal(first) {
		t.Fatalf("baseline moved from %v to %v", first, got)
	}
	c.set(first.Add(launchSettle))
	listOK(t, g)
	if got := active(t, g); got != "" {
		t.Fatalf("still listed once settled: %q", got)
	}
}

// A runner moved from regions back to one region drops its old regions
// once they're empty too, and keeps its own.
func TestGlobalLegacyRegionDroppedOnceEmpty(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	c := fixClock(g)
	discovered(t, g)
	launchedAgo(t, g, c, "r-one-1", 30*time.Minute)
	launchedAgo(t, g, c, "r-two-1", 30*time.Minute)
	g.r.Regions = nil
	g.legacy = true
	listOK(t, g)
	if got := active(t, g); got != "r-one-1" {
		t.Fatalf("active = %q, want only the runner's own region", got)
	}
}

// A launch refreshes its region's record once it has stopped sending, so
// a long run of attempts or a lost response still counts as recent.
func TestGlobalLaunchRecordsActivityAfterSending(t *testing.T) {
	f := &fakeRegionsEC2{hang: true, prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	c := fixClock(g)
	if _, err := g.Prices(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	later := c.now().Add(launchSettle * 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The request is sent, then its answer is lost.
	f.onRun = func(string) {
		c.set(later)
		cancel()
	}
	if _, err := g.Launch(ctx, t.Logf); !errors.Is(err, ErrLaunchAmbiguous) {
		t.Fatalf("err = %v, want ErrLaunchAmbiguous", err)
	}
	if got := busyAt(t, g, "r-one-1"); !got.Equal(later) {
		t.Fatalf("activity = %v, want %v (when sending ended)", got, later)
	}
}

// A bake builder's launch is recorded as it's sent: the bake may have
// waited long after its region was picked.
func TestGlobalBakeLaunchRecordsActivity(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	c := fixClock(g)
	a, err := g.Use("r-two-1")
	if err != nil {
		t.Fatal(err)
	}
	// Meanwhile a listing found the region empty and dropped it.
	if err := g.withState(func(st *placementState) {
		delete(st.Active, "r-two-1")
		delete(st.Busy, "r-two-1")
	}); err != nil {
		t.Fatal(err)
	}
	later := c.now().Add(launchSettle * 2)
	c.set(later)
	// Read in the fake's handler goroutine, asserted back here.
	var sent, held bool
	var busy time.Time
	f.onRun = func(region string) {
		sent = true
		if err := g.withState(func(st *placementState) {
			_, held = st.Active[region]
			busy = st.Busy[region]
		}); err != nil {
			t.Error(err)
		}
	}
	if _, err := a.BakeLaunch(context.Background(), t.Logf, nil); err != nil {
		t.Fatal(err)
	}
	if !sent {
		t.Fatal("no builder sent")
	}
	if !held {
		t.Error("builder sent with its region unlisted")
	}
	if !busy.Equal(later) {
		t.Errorf("activity when sent = %v, want %v", busy, later)
	}
}
