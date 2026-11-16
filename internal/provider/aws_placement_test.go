package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// warmUp lists the fleet with a VM that has been running in region for a
// while, the way a pool's listing records that the runner has used it.
func warmUp(t *testing.T, g *AWSGlobal, f *fakeRegionsEC2, region string) {
	t.Helper()
	listVM(t, g, f, region, "running", 2*warmAfter)
}

// listVM lists the fleet with one VM in region, in state, launched age ago.
func listVM(t *testing.T, g *AWSGlobal, f *fakeRegionsEC2, region, state string, age time.Duration) {
	t.Helper()
	id := "i-" + state + "-" + region
	f.mu.Lock()
	f.instances[region] = map[string]string{id: "c7a.2xlarge"}
	if f.state == nil {
		f.state = map[string]string{}
	}
	f.state[id] = state
	f.launched = g.clock().Add(-age).UTC().Format(time.RFC3339)
	f.mu.Unlock()
	discovered(t, g)
	if err := g.markActive(region, g.holdUntil(g.clock())); err != nil {
		t.Fatal(err)
	}
	if ins, err := g.List(context.Background()); err != nil || len(ins) == 0 {
		t.Fatalf("listed %v, %v", ins, err)
	}
	f.mu.Lock()
	delete(f.instances, region)
	f.launched = ""
	f.mu.Unlock()
}

func twoRegionEnv(t *testing.T, one, two string, edit func(r *config.Runner)) (*AWSGlobal, *fakeRegionsEC2) {
	t.Helper()
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", one}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", two}},
	}}
	// Both regions hold a bake: what history decides is the project's part.
	f.bakes = map[string]bool{"r-one-1": true, "r-two-1": true}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.r.LaunchOrder = config.LaunchRunCost
	if edit != nil {
		edit(g.r)
	}
	return g, f
}

func ranking(t *testing.T, g *AWSGlobal) (PriceTable, Ranking) {
	t.Helper()
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	return tbl, g.Rank(tbl)
}

func nextRegion(t *testing.T, g *AWSGlobal) string {
	t.Helper()
	tbl, _ := ranking(t, g)
	p, ok := g.NextLaunch(tbl)
	if !ok {
		t.Fatal("no next launch")
	}
	return p.Region
}

// costIn is the run cost of the option in region (one per region here).
func costIn(t *testing.T, k Ranking, region string) RunCost {
	t.Helper()
	for _, p := range k.Prices {
		if p.Region == region {
			c, ok := k.CostOf(p)
			if !ok {
				t.Fatalf("no cost for %s", region)
			}
			return c
		}
	}
	t.Fatalf("no option in %s", region)
	return RunCost{}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A region the fleet already runs in keeps the next VM against a cold one
// a little cheaper: the saving wouldn't pay for the cold start.
func TestRunCostWarmRegionHolds(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("with no warm region, the cheaper goes first; got %s", rg)
	}
	warmUp(t, g, f, "r-one-1")
	if rg := nextRegion(t, g); rg != "r-one-1" {
		t.Fatalf("next launch in %s, want the warm r-one-1", rg)
	}
	_, k := ranking(t, g)
	// The cold start, with a bake in both: the project's 3 minutes of the
	// VM and of waiting at $1/h.
	if c := costIn(t, k, "r-two-1"); c.Warm || !near(c.Cold, (0.096+1)*3/60) || !near(c.Score, 0.096+(0.096+1)*3/60) ||
		c.ColdParts == nil || c.ColdParts.Paid() != "deps,image" || c.ColdParts.Boot.Why != "a bake of the runner" {
		t.Fatalf("cold option %+v", c)
	}
	if c := costIn(t, k, "r-one-1"); !c.Warm || c.Cold != 0 || c.WarmSeen.IsZero() || !near(c.Score, 0.1) {
		t.Fatalf("warm option %+v", c)
	}
	resetCalls(f)
	var logged []string
	in, err := g.Launch(context.Background(), func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Region != "r-one-1" || runsOf(f) != "r-one-1 RunInstances c7a.2xlarge subnet-r-one-1a" {
		t.Fatalf("launched %+v via %s", in, runsOf(f))
	}
	// The launch says why: the warm region won, and what the cheaper one
	// would have added.
	p := in.Placement
	if p == nil || p.Order != config.LaunchRunCost || p.Rank != 1 || p.Of != 2 || p.Price != "0.100" || p.Cost == nil || !p.Cost.Warm || p.Cost.Cold != 0 {
		t.Fatalf("placement %+v", p)
	}
	if !strings.HasPrefix(p.Why, "lowest expected run cost of 2 options in 2 regions") || !strings.Contains(p.Why, "warm region") ||
		!strings.Contains(p.Why, "the cheapest, c7a.2xlarge in r-two-1a at $0.096/h, scores $0.151 (+$0.055 cold region (deps,image))") {
		t.Fatalf("why: %s", p.Why)
	}
	if last := logged[len(logged)-1]; !strings.Contains(last, "launched spot c7a.2xlarge") || !strings.Contains(last, p.Why) {
		t.Fatalf("log: %v", logged)
	}

	// Plain cheapest ranking is still there.
	g.r.LaunchOrder = config.LaunchCheapest
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("launch_order = cheapest: next launch in %s", rg)
	}
	if _, k := ranking(t, g); k.Order != config.LaunchCheapest || len(k.Cost) != 0 || k.Model != nil {
		t.Fatalf("cheapest ranking %+v", k)
	}
}

// A large enough saving still moves the fleet; how large depends on what
// the runner says waiting is worth.
func TestRunCostLargeSavingMoves(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.020", nil)
	warmUp(t, g, f, "r-one-1")
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("next launch in %s, want the much cheaper r-two-1", rg)
	}

	// A batch runner that doesn't count waiting moves for a smaller saving;
	// the default (waiting worth $1/h) doesn't.
	g, f = twoRegionEnv(t, "0.100", "0.090", nil)
	warmUp(t, g, f, "r-one-1")
	if rg := nextRegion(t, g); rg != "r-one-1" {
		t.Fatalf("default wait_cost: next launch in %s", rg)
	}
	g.r.WaitCost = config.Dollars{V: 0, Set: true}
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("wait_cost = 0: next launch in %s", rg)
	}
	// Without waiting, a cold start still costs the VM its 3 minutes.
	_, k := ranking(t, g)
	if c := costIn(t, k, "r-two-1"); !near(c.Cold, 0.090*3/60) || k.Model.WaitCost != 0 {
		t.Fatalf("cold option %+v, model %+v", c, k.Model)
	}
}

// Warmth fades: a region not used for warmFor is cold again; another
// sighting keeps it warm.
func TestRunCostWarmthExpires(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	now := time.Now()
	g.now = func() time.Time { return now }
	warmUp(t, g, f, "r-one-1")
	first := now
	now = now.Add(warmFor - time.Hour)
	if rg := nextRegion(t, g); rg != "r-one-1" {
		t.Fatalf("still warm: next launch in %s", rg)
	}
	now = now.Add(2 * time.Hour)
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("expired: next launch in %s", rg)
	}
	// Seen again before it expires: warm for another warmFor.
	now = first.Add(warmFor - time.Hour)
	g2, f2 := twoRegionEnv(t, "0.100", "0.096", nil)
	g2.now = func() time.Time { return now }
	now = first
	warmUp(t, g2, f2, "r-one-1")
	now = first.Add(warmFor - time.Hour)
	warmUp(t, g2, f2, "r-one-1")
	now = first.Add(warmFor + time.Hour)
	_, k := ranking(t, g2)
	if c := costIn(t, k, "r-one-1"); !c.Warm || !c.WarmSeen.Equal(first.Add(warmFor-time.Hour)) {
		t.Fatalf("seen again: %+v", c)
	}
}

// Warmth is recorded per AWS profile: another account's regions hold none
// of this one's caches.
func TestRunCostWarmthFollowsProfile(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.100", nil)
	g.r.Profile = "a"
	warmUp(t, g, f, "r-one-1")
	g.r.Profile = "b"
	warmUp(t, g, f, "r-two-1")
	_, k := ranking(t, g)
	if !costIn(t, k, "r-two-1").Warm || costIn(t, k, "r-one-1").Warm {
		t.Fatalf("profile b: one %+v, two %+v", costIn(t, k, "r-one-1"), costIn(t, k, "r-two-1"))
	}
	// Back to a: its record went when b's started.
	g.r.Profile = "a"
	_, k = ranking(t, g)
	if costIn(t, k, "r-one-1").Warm || costIn(t, k, "r-two-1").Warm {
		t.Fatal("profile a sees b's warmth, or its own old record")
	}
}

// Only a VM that has been up a while counts: a launch request, a pending
// VM or one that has only just started running says nothing about the
// region's caches.
func TestRunCostNewVMIsNotWarmth(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	listVM(t, g, f, "r-one-1", "pending", 2*warmAfter)
	listVM(t, g, f, "r-one-1", "running", warmAfter/2)
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("next launch in %s, want the cheaper (nothing is warm)", rg)
	}
	// Nor does a launch.
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-two-1" {
		t.Fatalf("launched %+v, %v", in, err)
	}
	_, k := ranking(t, g)
	if c := costIn(t, k, "r-two-1"); c.Warm || c.Cold == 0 {
		t.Fatalf("after a launch: %+v", c)
	}
}

// A newer generation can beat a cheaper older one: the time it saves is
// worth more than the price gap. launch_order = "cheapest" still takes the
// cheaper one.
func TestRunCostFasterTypeWins(t *testing.T) {
	prices := map[string][][3]string{"r-one-1": {
		{"r-one-1a", "m7a.2xlarge", "0.0600"},
		{"r-one-1b", "m6i.2xlarge", "0.0500"},
	}}
	for order, want := range map[string]string{config.LaunchRunCost: "m7a.2xlarge", config.LaunchCheapest: "m6i.2xlarge"} {
		f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()}, prices: prices}
		a := newSingleEnv(t, f, func(r *config.Runner) { reqRunner(r); r.LaunchOrder = order })
		in, err := a.Launch(context.Background(), t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		if in.Type != want {
			t.Fatalf("%s: launched %s, want %s", order, in.Type, want)
		}
		if order != config.LaunchRunCost {
			continue
		}
		c := in.Placement.Cost
		if c == nil || c.RelSpeed != 1 || c.Speed != 0 || c.Cold != 0 || c.Distance != 0 || !near(c.Score, 0.06) || in.Placement.Price != "0.0600" {
			t.Fatalf("winner's cost %+v", in.Placement)
		}
		// m6i: an hour of m7a's work takes it 1.1 hours.
		if !strings.Contains(in.Placement.Why, "scores $0.155 (+$0.105 slower type (91% speed))") {
			t.Fatalf("why: %s", in.Placement.Why)
		}
	}
	// A slower type still wins when its price makes up for it.
	f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()}, prices: map[string][][3]string{"r-one-1": {
		{"r-one-1a", "m7a.2xlarge", "0.3000"},
		{"r-one-1b", "m6i.2xlarge", "0.1000"},
	}}}
	a := newSingleEnv(t, f, func(r *config.Runner) { reqRunner(r); r.LaunchOrder = config.LaunchRunCost })
	if in, err := a.Launch(context.Background(), t.Logf); err != nil || in.Type != "m6i.2xlarge" {
		t.Fatalf("launched %+v, %v", in, err)
	}
}

// Types named in instance_types aren't weighed for speed: the list is the
// user's choice. Several regions still weigh warmth.
func TestRunCostFixedListIgnoresSpeed(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c6a.2xlarge", "0.050"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.055"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.r.LaunchOrder = config.LaunchRunCost
	_, k := ranking(t, g)
	for _, rg := range []string{"r-one-1", "r-two-1"} {
		if c := costIn(t, k, rg); c.RelSpeed != 1 || c.Speed != 0 {
			t.Fatalf("%s: %+v", rg, c)
		}
	}
	if k.Prices[0].Type != "c6a.2xlarge" || k.Model.SpeedByGen || !k.Model.Regions {
		t.Fatalf("ranking %+v %+v", k.Prices, k.Model)
	}
}

// A single-region runner with a fixed list ranks exactly as before
// run_cost: by price, equal prices in instance_types order, whatever the
// state says about warmth or distance.
func TestRunCostSingleRegionFixedTypesUnchanged(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-one-1": {
		{"r-one-1a", "m6a.2xlarge", "0.0500"},
		{"r-one-1b", "c7a.2xlarge", "0.0600"},
		{"r-one-1a", "c6a.2xlarge", "0.0600"},
		{"r-one-1c", "c7a.2xlarge", "0.0450"},
	}}}
	a := newSingleEnv(t, f, func(r *config.Runner) {
		r.InstanceTypes = []string{"c7a.2xlarge", "c6a.2xlarge", "m6a.2xlarge"}
		r.LaunchOrder = config.LaunchRunCost
	})
	g := a.ranker()
	if err := g.withState(func(st *placementState) {
		st.Warm, st.WarmProfile = map[string]time.Time{"r-one-1": time.Now()}, ""
		st.RTT = map[string]rttSample{"r-one-1": {MS: 300, At: time.Now()}}
	}); err != nil {
		t.Fatal(err)
	}
	tbl, k := ranking(t, g)
	if k.Order != config.LaunchCheapest || len(k.Cost) != 0 {
		t.Fatalf("ranked by %s with costs %v", k.Order, k.Cost)
	}
	want := []string{"r-one-1c/c7a.2xlarge", "r-one-1a/m6a.2xlarge", "r-one-1b/c7a.2xlarge", "r-one-1a/c6a.2xlarge"}
	var got []string
	for _, p := range k.Prices {
		got = append(got, p.Zone+"/"+p.Type)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(k.Prices, tbl.Prices) {
		t.Fatalf("order %v, want %v", got, want)
	}
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil || in.Zone != "r-one-1c" || in.Type != "c7a.2xlarge" {
		t.Fatalf("launched %+v, %v", in, err)
	}
	if p := in.Placement; p == nil || p.Order != config.LaunchCheapest || p.Cost != nil || !strings.HasPrefix(p.Why, "cheapest of 4 options") {
		t.Fatalf("placement %+v", p)
	}
}

// Equal scores keep the price table's order (price, then instance_types
// order, region, zone, type), as cheapest ranking does.
func TestRunCostTiesKeepTableOrder(t *testing.T) {
	var ps []SpotPrice
	for i, z := range []string{"e", "d", "c", "b", "a", "f", "g", "h", "i", "j", "k", "l", "m", "n"} {
		price := "0.10"
		if i%3 == 1 {
			price = "0.20"
		}
		ps = append(ps, SpotPrice{Region: "r-" + z, Zone: "r-" + z + "1", Type: "c7a.2xlarge", Price: price})
	}
	g, _ := twoRegionEnv(t, "0.1", "0.1", nil)
	k := g.Rank(PriceTable{Prices: ps})
	var cheap, dear []string
	for _, p := range ps {
		if p.Price == "0.10" {
			cheap = append(cheap, p.Zone)
		} else {
			dear = append(dear, p.Zone)
		}
	}
	var got []string
	for _, p := range k.Prices {
		got = append(got, p.Zone)
	}
	if want := append(cheap, dear...); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// A farther region pays for its round trips; unmeasured regions add
// nothing; a stale measurement is ignored even before it is cleaned up.
func TestRunCostDistance(t *testing.T) {
	g, _ := twoRegionEnv(t, "0.100", "0.099", nil)
	now := time.Now()
	if err := g.withState(func(st *placementState) {
		st.RTT = map[string]rttSample{"r-one-1": {MS: 50, At: now}, "r-two-1": {MS: 400, At: now}}
	}); err != nil {
		t.Fatal(err)
	}
	if rg := nextRegion(t, g); rg != "r-one-1" {
		t.Fatalf("next launch in %s, want the nearer r-one-1", rg)
	}
	tbl, k := ranking(t, g)
	// 300 round trips of 350 ms more, for the VM ($0.099/h) and the wait.
	if far := costIn(t, k, "r-two-1"); !near(far.Distance, 1.099*300*0.35/3600) || far.RTTMS != 400 {
		t.Fatalf("far %+v", far)
	}
	if c := costIn(t, k, "r-one-1"); c.Distance != 0 || c.RTTMS != 50 {
		t.Fatalf("near %+v", c)
	}
	// A stale sample, as left in placement.json by an older process.
	path := filepath.Join(g.dir, "placement.json")
	b, _ := os.ReadFile(path)
	var st placementState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	st.RTT["r-two-1"] = rttSample{MS: 400, At: now.Add(-rttFor - time.Minute)}
	b, _ = json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	k = g.Rank(tbl)
	if c := costIn(t, k, "r-two-1"); c.Distance != 0 || c.RTTMS != 0 || k.Prices[0].Region != "r-two-1" {
		t.Fatalf("stale: %+v first %s", c, k.Prices[0].Region)
	}
	if c := costIn(t, k, "r-one-1"); c.Distance != 0 || c.RTTMS != 50 {
		t.Fatalf("near after the far one went stale: %+v", c)
	}
}

// Old evidence is dropped from the state, and so is evidence dated ahead of
// the clock; scoring ignores both even when they're still there.
func TestRunCostForgetsOldEvidence(t *testing.T) {
	now := time.Now()
	st := &placementState{
		Warm: map[string]time.Time{"old": now.Add(-warmFor - time.Minute), "future": now.Add(2 * time.Hour), "fresh": now.Add(-time.Hour)},
		RTT:  map[string]rttSample{"old": {MS: 10, At: now.Add(-rttFor - time.Minute)}, "future": {MS: 10, At: now.Add(2 * time.Hour)}, "fresh": {MS: 10, At: now}},
	}
	ev := placementEvidence{now: now, warm: map[string]time.Time{}}
	for rg, at := range st.Warm {
		ev.warm[rg] = at
	}
	forgetOld(st, now)
	if len(st.Warm) != 1 || st.Warm["fresh"].IsZero() || len(st.RTT) != 1 || st.RTT["fresh"].MS != 10 {
		t.Fatalf("kept %v %v", st.Warm, st.RTT)
	}
	m := RunCostModel{WaitCost: 1, HorizonMinutes: 60, ColdMinutes: 4, BootMinutes: 1, DepsMinutes: 2, ImageMinutes: 1, Regions: true, RoundTrips: runRoundTrips, GenSpeedup: genSpeedup}
	// Each holds a bake: history decides the project's parts.
	ev.found = map[string]regionWarmth{}
	for _, rg := range []string{"old", "future", "fresh"} {
		ev.found[rg] = regionWarmth{boot: partSeen{state: partHas}}
	}
	var ps []SpotPrice
	for _, rg := range []string{"old", "future", "fresh"} {
		ps = append(ps, SpotPrice{Region: rg, Zone: rg + "a", Type: "c7a.2xlarge", Price: "0.1"})
	}
	c := runCosts(m, ps, ev)
	if c["old/olda/c7a.2xlarge"].Warm || c["future/futurea/c7a.2xlarge"].Warm || !c["fresh/fresha/c7a.2xlarge"].Warm {
		t.Fatalf("costs %+v", c)
	}
}

// A Spot interruption still sends a pool last, however well it scores, and
// max_price still caps the bid whatever the ranking says.
func TestRunCostKeepsHoldBackAndPriceCap(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", nil)
	warmUp(t, g, f, "r-one-1")
	g.Reclaimed(Instance{ID: "i-r", Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"})
	_, k := ranking(t, g)
	if k.Prices[0].Region != "r-two-1" || !k.HeldBack(k.Prices[1]) {
		t.Fatalf("order %+v", k.Prices)
	}
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-two-1" {
		t.Fatalf("launched %+v, %v", in, err)
	}
	if why := in.Placement.Why; !strings.Contains(why, "1 option held back after a Spot interruption would otherwise come first") || !strings.HasPrefix(why, "#1 in launch order (by run cost)") {
		t.Fatalf("why: %s", why)
	}

	g, f = twoRegionEnv(t, "0.100", "0.096", func(r *config.Runner) { r.MaxPrice = config.Dollars{V: 0.098, Set: true} })
	warmUp(t, g, f, "r-one-1")
	if rg := nextRegion(t, g); rg != "r-two-1" {
		t.Fatalf("next launch above max_price: %s", rg)
	}
	resetCalls(f)
	in, err = g.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-two-1" || runsOf(f) != "r-two-1 RunInstances c7a.2xlarge subnet-r-two-1a" {
		t.Fatalf("launched %+v (%v) via %s", in, err, runsOf(f))
	}
	p := in.Placement
	if p.Rank != 2 || p.Price != "0.096" || p.Cost == nil || p.Cost.Warm || !near(p.Cost.Cold, (0.096+1)*3/60) ||
		!strings.HasPrefix(p.Why, "#2 in launch order (by run cost) of 2 options in 2 regions; passed over: 1 above max_price") {
		t.Fatalf("placement %+v", p)
	}
}

// When the first choice has no capacity, the launch moves down the run-cost
// ranking, not the price list, and says so.
func TestRunCostFallsBackInRankOrder(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.100"}},
			"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.096"}},
			"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.104"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity"},
	}
	f.bakes = map[string]bool{"r-one-1": true, "r-two-1": true, "r-three-1": true}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	g.r.LaunchOrder = config.LaunchRunCost
	warmUp(t, g, f, "r-one-1")
	warmUp(t, g, f, "r-three-1")
	resetCalls(f)
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if want := "r-one-1 RunInstances c7a.2xlarge subnet-r-one-1a\nr-three-1 RunInstances c7a.2xlarge subnet-r-three-1a"; runsOf(f) != want || in.Region != "r-three-1" {
		t.Fatalf("launched %+v via:\n%s\nwant:\n%s", in, runsOf(f), want)
	}
	p := in.Placement
	if p.Rank != 2 || p.Of != 3 || p.Price != "0.104" || p.Cost == nil || !p.Cost.Warm || !near(p.Cost.Score, 0.104) ||
		!strings.Contains(p.Why, "passed over: 1 refused (InsufficientInstanceCapacity)") {
		t.Fatalf("placement %+v", p)
	}
}

// doctor shows the first three options and what each factor added.
func TestRunCostDoctorRanking(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.100"}, {"r-one-1b", "c7a.2xlarge", "0.101"}},
		"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.096"}},
		"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.300"}},
	}}
	f.bakes = map[string]bool{"r-one-1": true}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	g.r.LaunchOrder = config.LaunchRunCost
	warmUp(t, g, f, "r-one-1")
	_, k := ranking(t, g)
	c, ok := rankingCheck(k)
	if !ok || c.Name != "launch ranking" {
		t.Fatalf("check %+v", c)
	}
	parts := strings.Split(c.Msg, " · ")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "1. c7a.2xlarge in r-one-1a at $0.100/h: $0.100 per hour of work") ||
		!strings.Contains(parts[0], "warm region") || !strings.HasPrefix(parts[2], "3. c7a.2xlarge in r-two-1a at $0.096/h") ||
		!strings.Contains(parts[2], "+$0.073 cold region (boot,deps,image)") || strings.Contains(c.Msg, "r-three-1") {
		t.Fatalf("doctor: %s", c.Msg)
	}
	k.Order = config.LaunchCheapest
	if _, ok := rankingCheck(k); ok {
		t.Fatal("ranking check for cheapest ranking")
	}
}

// The distance probe: TCP connects to a region's EC2 endpoint are timed,
// the latest kept, and saved rounded to 10 ms as of when they were made.
func TestTraceConnectMeasuresRTT(t *testing.T) {
	region := "r-trace-1"
	t.Cleanup(func() {
		observedRTT.Lock()
		delete(observedRTT.m, region)
		observedRTT.Unlock()
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(traceConnect(ctx, region), http.MethodGet, srv.URL, nil)
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	observedRTT.Lock()
	o, ok := observedRTT.m[region]
	observedRTT.Unlock()
	if !ok || o.d <= 0 || o.at.IsZero() {
		t.Fatalf("no connect measured (%+v)", o)
	}

	// Failed connects and ones without a start are no measurement.
	tc := httptrace.ContextClientTrace(traceConnect(context.Background(), region))
	tc.ConnectDone("tcp", "192.0.2.9:443", nil)
	tc.ConnectStart("tcp", "192.0.2.9:443")
	tc.ConnectDone("tcp", "192.0.2.9:443", errors.New("refused"))
	observedRTT.Lock()
	if observedRTT.m[region] != o {
		t.Errorf("a failed or unmatched connect replaced the sample: %+v", observedRTT.m[region])
	}
	observedRTT.m[region] = rttObservation{d: 46 * time.Millisecond, at: o.at}
	observedRTT.Unlock()

	st := &placementState{}
	saveRTTs(st, []string{region, "r-unmeasured-1"})
	if s := st.RTT[region]; s.MS != 50 || !s.At.Equal(o.at) || len(st.RTT) != 1 {
		t.Fatalf("saved %+v", st.RTT)
	}
	// A newer saved sample (another process's) stays; a newer observation
	// replaces it, rounded to the nearest 10 ms.
	st.RTT[region] = rttSample{MS: 80, At: o.at.Add(time.Second)}
	saveRTTs(st, []string{region})
	if st.RTT[region].MS != 80 {
		t.Fatalf("an older observation replaced a newer sample: %+v", st.RTT[region])
	}
	observedRTT.Lock()
	observedRTT.m[region] = rttObservation{d: 44 * time.Millisecond, at: o.at.Add(time.Minute)}
	observedRTT.Unlock()
	saveRTTs(st, []string{region})
	if s := st.RTT[region]; s.MS != 40 || !s.At.Equal(o.at.Add(time.Minute)) {
		t.Fatalf("saved %+v", s)
	}
}

// Only real EC2 requests are timed: not other services, and not a test's
// endpoint.
func TestTraceOnlyRealEC2(t *testing.T) {
	traced := map[string]bool{}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		traced[r.URL.Host] = httptrace.ContextClientTrace(r.Context()) != nil
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	})
	c := &awsClient{region: "r-one-1", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
		http: &http.Client{Transport: rt}, endpoints: map[string]string{"ecr": "https://ecr.test/"}}
	for _, svc := range []string{"ec2", "sts"} {
		if _, _, err := c.do(context.Background(), svc, "text/plain", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	c.endpoints["ec2"] = "https://ec2.test/"
	if _, _, err := c.do(context.Background(), "ec2", "text/plain", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !traced["ec2.r-one-1.amazonaws.com"] || traced["sts.r-one-1.amazonaws.com"] || traced["ec2.test"] {
		t.Fatalf("traced %v", traced)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunCostsSpeedByGeneration(t *testing.T) {
	m := RunCostModel{WaitCost: 1, HorizonMinutes: 60, ColdMinutes: 4, GenSpeedup: genSpeedup, RoundTrips: runRoundTrips, SpeedByGen: true}
	ps := []SpotPrice{
		{Region: "r", Zone: "a", Type: "m9a.2xlarge", Price: "0.10"},
		{Region: "r", Zone: "a", Type: "m7a.2xlarge", Price: "0.10"},
		{Region: "r", Zone: "a", Type: "m6a.2xlarge", Price: "0.10"},
		{Region: "r", Zone: "a", Type: "odd.2xlarge", Price: "0.10"},
	}
	c := runCosts(m, ps, placementEvidence{now: time.Now()})
	// Unknown generations count as fastest rather than guessing.
	if got := c["r/a/odd.2xlarge"]; got.RelSpeed != 1 || got.Speed != 0 || !near(got.Compute, 0.10) || !near(got.Score, 0.10) {
		t.Fatalf("odd %+v", got)
	}
	if got := c["r/a/m9a.2xlarge"]; got.RelSpeed != 1 || !near(got.Score, 0.10) {
		t.Fatalf("m9a %+v", got)
	}
	// Two generations behind: an hour of m9a's work takes 1.21 hours, which
	// costs the VM and the wait.
	if got := c["r/a/m7a.2xlarge"]; !near(got.RelSpeed, 1/1.21) || !near(got.Speed, 1.1*0.21) || !near(got.Score, 0.1+1.1*0.21) {
		t.Fatalf("m7a %+v", got)
	}
	if got := c["r/a/m6a.2xlarge"]; !near(got.RelSpeed, 1/1.331) || !near(got.Score, 0.1+1.1*0.331) {
		t.Fatalf("m6a %+v", got)
	}
}
