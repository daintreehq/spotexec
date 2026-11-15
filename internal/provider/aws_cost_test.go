package provider

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Each VM is priced at the Spot price of its own type in its own zone and
// region, whatever the runner lists now; on-demand VMs and VMs without a
// zone are left unpriced.
func TestInstancePricesPerVM(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c6a.2xlarge", "0.1290"}, {"r-one-1b", "c6a.2xlarge", "0.1500"}, {"r-one-1a", "m7a.2xlarge", "0.3310"}},
		"r-two-1": {{"r-two-1c", "c7a.2xlarge", "0.2000"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	p := newInstancePricer(g)
	insts := []Instance{
		{ID: "i-1", Spot: true, Region: "r-one-1", Zone: "r-one-1a", Type: "c6a.2xlarge"},
		{ID: "i-2", Spot: true, Region: "r-one-1", Zone: "r-one-1b", Type: "c6a.2xlarge"},
		{ID: "i-3", Spot: true, Region: "r-one-1", Zone: "r-one-1a", Type: "m7a.2xlarge"}, // not in instance_types
		{ID: "i-4", Spot: true, Region: "r-two-1", Zone: "r-two-1c", Type: "c7a.2xlarge"},
		{ID: "i-5", Spot: false, Region: "r-one-1", Zone: "r-one-1a", Type: "c6a.2xlarge"}, // on-demand fallback
		{ID: "i-6", Spot: true, Region: "r-one-1", Type: "c6a.2xlarge"},                    // zone unknown
		{ID: "i-7", Spot: true, Region: "r-two-1", Zone: "r-two-1a", Type: "c7a.2xlarge"},  // no price in that zone
	}
	got := p.Prices(context.Background(), insts)
	want := map[string]float64{"i-1": 0.129, "i-2": 0.15, "i-3": 0.331, "i-4": 0.2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prices %v, want %v", got, want)
	}
	if n := len(f.callsOf("DescribeSpotPriceHistory")); n != 2 {
		t.Fatalf("%d price lookups, want one per region holding VMs", n)
	}
	if n := len(f.callsOf("DescribeRegions")); n != 0 {
		t.Fatal("pricing VMs looked up the enabled regions")
	}
}

// Quotes are reused for 15 minutes, and a failed lookup isn't repeated on
// every refresh; a stale quote is kept while its refresh fails.
func TestInstancePricesCacheAndRetry(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c6a.2xlarge", "0.1290"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	p := newInstancePricer(g)
	insts := []Instance{{ID: "i-1", Spot: true, Region: "r-one-1", Zone: "r-one-1a", Type: "c6a.2xlarge"}}
	ctx := context.Background()
	lookups := func() int { return len(f.callsOf("DescribeSpotPriceHistory")) }

	for i := 0; i < 5; i++ {
		if got := p.Prices(ctx, insts); got["i-1"] != 0.129 {
			t.Fatalf("refresh %d: %v", i, got)
		}
		now = now.Add(2 * time.Second)
	}
	if n := lookups(); n != 1 {
		t.Fatalf("%d lookups over five refreshes, want 1", n)
	}

	// Past the TTL the price is fetched again; while that fails the old
	// quote stands, and the failing lookup waits a minute between tries.
	now = now.Add(priceTTL)
	f.mu.Lock()
	f.priceErr = map[string]string{"r-one-1": "UnauthorizedOperation"}
	f.mu.Unlock()
	if got := p.Prices(ctx, insts); got["i-1"] != 0.129 {
		t.Fatalf("stale quote dropped on a failed refresh: %v", got)
	}
	now = now.Add(10 * time.Second)
	p.Prices(ctx, insts)
	if n := lookups(); n != 2 {
		t.Fatalf("%d lookups, want the failed one not retried within a minute", n)
	}
	now = now.Add(priceRetry)
	f.mu.Lock()
	f.priceErr = nil
	f.prices["r-one-1"] = [][3]string{{"r-one-1a", "c6a.2xlarge", "0.1400"}}
	f.mu.Unlock()
	if got := p.Prices(ctx, insts); got["i-1"] != 0.14 {
		t.Fatalf("after the retry: %v, want the new price", got)
	}

	// A region that can't be priced at all leaves its VMs out.
	f.mu.Lock()
	f.priceErr = map[string]string{"r-two-1": "UnauthorizedOperation"}
	f.mu.Unlock()
	other := []Instance{{ID: "i-2", Spot: true, Region: "r-two-1", Zone: "r-two-1a", Type: "c6a.2xlarge"}}
	if got := p.Prices(ctx, other); len(got) != 0 {
		t.Fatalf("priced a VM whose region can't be read: %v", got)
	}
}

// A single-region runner prices through its own regional provider.
func TestInstancePricesSingleRegion(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1c", "c7a.2xlarge", "0.0450"}},
	}}
	a := newSingleEnv(t, f, nil)
	p := newInstancePricer(a.ranker())
	got := p.Prices(context.Background(), []Instance{{ID: "i-1", Spot: true, Region: "r-one-1", Zone: "r-one-1c", Type: "c7a.2xlarge"}})
	if got["i-1"] != 0.045 {
		t.Fatalf("prices %v", got)
	}
}
