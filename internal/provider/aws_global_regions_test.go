package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A listed region the account hasn't enabled never answers. It must not
// stall pricing, block listings, or stop launches elsewhere, even as the
// home region (the first listed), and the reason is shown.
func TestGlobalListedRegionNotEnabled(t *testing.T) {
	old := enabledRegionsTimeout
	enabledRegionsTimeout = time.Second
	t.Cleanup(func() { enabledRegionsTimeout = old })
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}},
		dead:   map[string]bool{"r-off-1": true},
	}
	g := newGlobalEnv(t, f, "r-off-1", "r-one-1")
	g.r.Region = "r-off-1" // the home region, as config sets it for a list
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	tbl, err := g.Prices(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tbl.Regions, ","); got != "r-off-1,r-one-1" {
		t.Fatalf("regions %s", got)
	}
	if !strings.Contains(tbl.Unpriced["r-off-1"], "not enabled") {
		t.Fatalf("unpriced %v", tbl.Unpriced)
	}
	if len(tbl.Prices) != 1 || tbl.Prices[0].Region != "r-one-1" {
		t.Fatalf("prices %+v", tbl.Prices)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pricing took %v", d)
	}
	if n := len(f.callsOf("dead")); n != 1 {
		t.Fatalf("asked the dead region %d times, want once (the DescribeRegions attempt)", n)
	}
	// Listings never wait on it either (first listing: every region).
	if _, err := g.List(ctx); err != nil {
		t.Fatal(err)
	}
	in, err := g.Launch(ctx, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Region != "r-one-1" {
		t.Fatalf("launched in %s", in.Region)
	}
	if n := len(f.callsOf("dead")); n != 1 {
		t.Fatalf("the dead region was asked %d times", n)
	}
}

// When the enabled regions can't be read, a listed region is used as
// configured (nothing is left out on a guess).
func TestGlobalListedRegionsWhenEnabledUnknown(t *testing.T) {
	old := enabledRegionsTimeout
	enabledRegionsTimeout = time.Second
	t.Cleanup(func() { enabledRegionsTimeout = old })
	f := &fakeRegionsEC2{
		prices:    map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}, "r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.05"}}},
		noRegions: true,
	}
	// r-three-1 is enabled (the fake lists it), r-gone-1 isn't: with the
	// list unreadable, both are kept.
	g := newGlobalEnv(t, f, "r-one-1", "r-three-1", "r-gone-1")
	got, err := g.PlacementRegions(context.Background())
	if err != nil || strings.Join(got, ",") != "r-one-1,r-three-1,r-gone-1" {
		t.Fatalf("placement regions %v, %v", got, err)
	}
	if n := len(f.callsOf("DescribeRegions")); n < 2 {
		t.Fatalf("asked %d regions for the enabled list, want the fallbacks too", n)
	}
}

// A quota refusal for a large type leaves the smaller types in that region
// to launch, cheapest first.
func TestGlobalQuotaRefusalKeepsSmallerTypes(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}, {"r-one-1b", "m7a.2xlarge", "0.045"}, {"r-one-1a", "c7a.large", "0.05"}},
			"r-two-1": {{"r-two-1a", "c7a.large", "0.06"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.r.InstanceTypes = []string{"c7a.2xlarge", "m7a.2xlarge", "c7a.large"}
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Region != "r-one-1" || in.Type != "c7a.large" {
		t.Fatalf("launched %s in %s, want c7a.large in r-one-1", in.Type, in.Region)
	}
	// The other 8-vCPU type in the refused region was not tried.
	for _, c := range f.callsOf("RunInstances") {
		if strings.Contains(c, "m7a.2xlarge") {
			t.Fatalf("tried a type the quota had just refused: %v", f.callsOf("RunInstances"))
		}
	}
	// The next launch checks the quota afresh (a VM may have ended since).
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if in, err = g.Launch(context.Background(), t.Logf); err != nil || in.Type != "c7a.large" || in.Region != "r-one-1" {
		t.Fatalf("second launch %+v, %v", in, err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 2 || !strings.Contains(runs[0], "c7a.2xlarge") {
		t.Fatalf("second launch requests %v", runs)
	}
}

// Quota refusals everywhere are a *QuotaError (so the pool holds launches
// and the pool manager doesn't count them as failures), and the next launch
// asks again rather than trusting an old refusal.
func TestGlobalQuotaEverywhereIsQuotaError(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-two-1/c7a.2xlarge": "MaxSpotInstanceCountExceeded"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	_, err := g.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Region != "r-one-1" {
		t.Fatalf("want a QuotaError for r-one-1, got %v", err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	f.mu.Lock()
	f.runErr = nil // a VM ended: room again
	f.mu.Unlock()
	if in, err := g.Launch(context.Background(), t.Logf); err != nil || in.Region != "r-one-1" {
		t.Fatalf("after room freed up: %+v, %v", in, err)
	}
}

// Quota in one region and missing capacity in another is not a quota
// block: capacity may come back.
func TestGlobalQuotaAndCapacityIsNotQuotaError(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-two-1/c7a.2xlarge": "InsufficientInstanceCapacity"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	_, err := g.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("want a capacity error, got %v", err)
	}
}

func TestQuotaSkipCovers(t *testing.T) {
	q := quotaSkip{vcpus: 8, group: quotaGroup("c7a.2xlarge")}
	for typ, want := range map[string]bool{"c7a.2xlarge": true, "m7a.4xlarge": true, "c7a.large": false, "g5.2xlarge": false} {
		if got := q.covers(typ); got != want {
			t.Errorf("covers %s = %v, want %v", typ, got, want)
		}
	}
	// Families outside the standard quota each have their own.
	g := quotaSkip{vcpus: 8, group: quotaGroup("g5.2xlarge")}
	if g.covers("p3.2xlarge") || !g.covers("g6.4xlarge") || g.covers("c7a.4xlarge") {
		t.Error("G-family refusal covers the wrong types")
	}
	if !(quotaSkip{}).covers("c7a.large") {
		t.Error("a count limit covers every type")
	}
}

// Most regions that aren't enabled answer AuthFailure at once (ap-south-2,
// eu-south-1, il-central-1 do). As the home region, that must not stop the
// lookup from asking elsewhere, or pricing elsewhere.
func TestGlobalListedRegionAnswersAuthFailure(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}},
		deny:   map[string]bool{"r-auth-1": true},
	}
	g := newGlobalEnv(t, f, "r-auth-1", "r-one-1")
	g.r.Region = "r-auth-1"
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tbl.Unpriced["r-auth-1"], "not enabled") || len(tbl.Prices) != 1 {
		t.Fatalf("prices %+v unpriced %v", tbl.Prices, tbl.Unpriced)
	}
	if in, err := g.Launch(context.Background(), t.Logf); err != nil || in.Region != "r-one-1" {
		t.Fatalf("launch %+v, %v", in, err)
	}
}

// When the enabled regions can't be listed at all, a listed region's
// AuthFailure leaves it unpriced; only every region refusing the
// credentials means they are bad.
func TestGlobalAuthFailureInOneRegionIsNotFatal(t *testing.T) {
	f := &fakeRegionsEC2{
		prices:    map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}},
		deny:      map[string]bool{"r-auth-1": true},
		noRegions: true,
	}
	g := newGlobalEnv(t, f, "r-auth-1", "r-one-1")
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Unpriced["r-auth-1"] == "" || len(tbl.Prices) != 1 {
		t.Fatalf("prices %+v unpriced %v", tbl.Prices, tbl.Unpriced)
	}
	f.mu.Lock()
	f.deny["r-one-1"] = true
	f.mu.Unlock()
	_, err = g.Prices(context.Background(), true)
	var ce *CredentialError
	if !errors.As(err, &ce) {
		t.Fatalf("every region refusing: want a CredentialError, got %v", err)
	}
}

// The first listing (which covers every region before discovery) must not
// wait on a region that never answers, with nothing cached yet.
func TestGlobalColdListSkipsDeadRegion(t *testing.T) {
	old := enabledRegionsTimeout
	enabledRegionsTimeout = time.Second
	t.Cleanup(func() { enabledRegionsTimeout = old })
	f := &fakeRegionsEC2{dead: map[string]bool{"r-off-1": true}}
	g := newGlobalEnv(t, f, "r-off-1", "r-one-1")
	g.r.Region = "r-off-1"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := g.List(ctx); err != nil {
		t.Fatal(err)
	}
}

// Prices --refresh rechecks the enabled regions instead of trusting the
// day-long cache (a region may have been enabled since, or disabled).
func TestGlobalRefreshRechecksEnabledRegions(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}, "r-new-1": {{"r-new-1a", "c7a.2xlarge", "0.03"}}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-new-1")
	// A cached list saying r-new-1 is enabled; the account (the fake's
	// DescribeRegions) says it isn't.
	_ = g.withState(func(st *placementState) { st.Regions, st.RegionsAt = []string{"r-one-1", "r-new-1"}, time.Now() })
	if tbl, err := g.Prices(context.Background(), false); err != nil || tbl.Prices[0].Region != "r-new-1" {
		t.Fatalf("cached: %+v, %v", tbl, err)
	}
	tbl, err := g.Prices(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tbl.Unpriced["r-new-1"], "not enabled") || tbl.Prices[0].Region != "r-one-1" {
		t.Fatalf("refresh reused the cached enabled regions: %+v", tbl)
	}
}

// Untried on-demand regions mean quota hasn't been shown to block
// everything: not a *QuotaError.
func TestGlobalOnDemandCapIsNotQuotaVerdict(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.05"}},
			"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.06"}},
		},
		runErr: map[string]string{
			"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-two-1/c7a.2xlarge": "VcpuLimitExceeded", "r-three-1/c7a.2xlarge": "VcpuLimitExceeded",
			"r-one-1/c7a.2xlarge/on-demand": "VcpuLimitExceeded", "r-one-1/c6a.2xlarge/on-demand": "VcpuLimitExceeded",
			"r-two-1/c7a.2xlarge/on-demand": "VcpuLimitExceeded", "r-two-1/c6a.2xlarge/on-demand": "VcpuLimitExceeded",
		},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	g.r.OnDemandFallback = true
	_, err := g.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("want a non-quota error (r-three-1 on-demand untried), got %v", err)
	}
}

// A price refusal (an option above max_price) says nothing about room:
// quota elsewhere is still the whole story.
func TestGlobalQuotaAndPriceRefusalIsQuotaError(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-two-1/c7a.2xlarge": "SpotMaxPriceTooLow"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	_, err := g.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("want a QuotaError, got %v", err)
	}
	// And again once the price refusal is cooling down.
	if _, err = g.Launch(context.Background(), t.Logf); !errors.As(err, &qe) {
		t.Fatalf("second launch: want a QuotaError, got %v", err)
	}
}

// A region that couldn't be priced just now might have room: no quota
// verdict.
func TestGlobalUnpricedRegionBlocksQuotaVerdict(t *testing.T) {
	f := &fakeRegionsEC2{
		prices:   map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}}},
		priceErr: map[string]string{"r-two-1": "UnauthorizedOperation"},
		runErr:   map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	_, err := g.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("want a non-quota error, got %v", err)
	}
}

// Going single-region forgets the enabled-region cache even before the
// first listing ran, so a region used meanwhile can't be filtered out of
// discovery when the runner goes back to several regions.
func TestForgetDiscoveryClearsEnabledCache(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	_ = g.withState(func(st *placementState) {
		st.Regions, st.RegionsAt, st.Discovered = []string{"r-one-1"}, time.Now(), false
	})
	forgetDiscovery(g.r)
	_ = g.withState(func(st *placementState) {
		if !st.RegionsAt.IsZero() {
			t.Fatal("enabled-region cache kept")
		}
	})
}
