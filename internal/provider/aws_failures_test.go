package provider

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// dryNewestEnv is #219's shape: a vcpus runner whose newest generation
// ranks first on speed but has no Spot capacity, and an older generation,
// cheaper, in another region.
func dryNewestEnv(t *testing.T) (*AWSGlobal, *fakeRegionsEC2, *time.Time) {
	t.Helper()
	f := &fakeRegionsEC2{
		types: map[string][]string{"r-one-1": requirementTypes(), "r-two-1": requirementTypes()},
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "m7a.2xlarge", "0.1301"}},
			"r-two-1": {{"r-two-1a", "m6i.2xlarge", "0.0736"}},
		},
		runErr: map[string]string{"r-one-1/m7a.2xlarge": "InsufficientInstanceCapacity"},
	}
	f.bakes = map[string]bool{"r-one-1": true, "r-two-1": true}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	reqRunner(g.r)
	g.r.LaunchOrder = config.LaunchRunCost
	now := time.Now()
	g.now = func() time.Time { return now }
	return g, f, &now
}

// A pool that keeps refusing for capacity ranks lower for longer, so later
// launches go straight to an older generation instead of asking the dry
// pool again every time its cooldown ends.
func TestDryPoolFallsBehind(t *testing.T) {
	g, f, now := dryNewestEnv(t)
	dry := 0
	for round := 0; round < 6; round++ {
		*now = now.Add(poolCoolMax + time.Minute) // past any cooldown
		resetCalls(f)
		in, err := g.Launch(context.Background(), t.Logf)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if in.Type != "m6i.2xlarge" {
			t.Fatalf("round %d: launched %s", round, in.Type)
		}
		if strings.Contains(runsOf(f), "m7a.2xlarge") {
			dry++
		}
	}
	if dry > 2 {
		t.Fatalf("asked the dry pool in %d of 6 launches", dry)
	}
}

// Each capacity refusal adds to the pool's score, halving every
// failHalfLife; the explanation says why; other refusals add nothing.
func TestFailureHistoryDecays(t *testing.T) {
	g, f, now := dryNewestEnv(t)
	f.runErr["r-two-1/m6i.2xlarge"] = "SpotMaxPriceTooLow" // a refusal, not for capacity
	if _, err := g.Launch(context.Background(), t.Logf); err == nil {
		t.Fatal("launched with every option refused")
	}
	tbl, k := ranking(t, g)
	dry, other := tbl.Prices[0], tbl.Prices[1]
	if dry.Type != "m7a.2xlarge" {
		dry, other = other, dry
	}
	c, _ := k.CostOf(dry)
	perFailure := (dry.USD() + g.r.WaitCostPerHour()) * failCost.Hours()
	if c.FailedHere == nil || !near(c.FailedHere.Capacity, 1) || c.FailedHere.Reclaims != 0 || !near(c.Failures, perFailure) ||
		!near(c.Score, c.Compute+c.Speed+c.Cold+c.Distance+c.Failures) {
		t.Fatalf("after one refusal: %+v %+v", c, c.FailedHere)
	}
	if !strings.Contains(c.Summary(), "failed here lately (1.0 capacity refusals, last ") {
		t.Fatalf("summary: %s", c.Summary())
	}
	if oc, _ := k.CostOf(other); oc.Failures != 0 || oc.FailedHere != nil {
		t.Fatalf("a price refusal counted: %+v", oc)
	}
	// Two hours on, it counts half.
	*now = now.Add(failHalfLife)
	_, k = ranking(t, g)
	if c, _ = k.CostOf(dry); !near(c.FailedHere.Capacity, 0.5) || !near(c.Failures, perFailure/2) {
		t.Fatalf("after a half-life: %+v", c.FailedHere)
	}
	// A second refusal adds a whole one.
	g.coolPool(dry, "InsufficientInstanceCapacity")
	_, k = ranking(t, g)
	if c, _ = k.CostOf(dry); !near(c.FailedHere.Capacity, 1.5) {
		t.Fatalf("after a second refusal: %+v", c.FailedHere)
	}
	// A launch there doesn't clear it: only time does.
	g.clearCool(dry)
	_, k = ranking(t, g)
	if c, _ = k.CostOf(dry); !near(c.FailedHere.Capacity, 1.5) {
		t.Fatalf("after clearing the cooldown: %+v", c.FailedHere)
	}
	// Long enough, and the history is gone, from the state too.
	*now = now.Add(20 * failHalfLife)
	_, k = ranking(t, g)
	if c, _ = k.CostOf(dry); c.FailedHere != nil || c.Failures != 0 {
		t.Fatalf("not forgotten: %+v", c)
	}
	_ = g.withState(func(st *placementState) {
		if len(st.Failures) != 0 {
			t.Errorf("state keeps %v", st.Failures)
		}
	})
}

// A pool that failed all day counts at most failMaxWeight failures, so it
// comes back within a day.
func TestFailureHistoryCapped(t *testing.T) {
	st := &placementState{}
	p := SpotPrice{Region: "r", Zone: "ra", Type: "m7a.2xlarge"}
	now := time.Now()
	for i := 0; i < 40; i++ {
		recordFailure(st, p, i%2 == 0, now)
	}
	f := st.Failures[poolKey(p)]
	if !near(f.weight(), failMaxWeight) || f.Capacity < 7 || f.Reclaims < 7 {
		t.Fatalf("capped: %+v", f)
	}
	forgetOldFailures(st, now.Add(24*time.Hour))
	if len(st.Failures) != 0 {
		t.Fatalf("a day later: %+v", st.Failures)
	}
	// A clock set back decays nothing and adds nothing extra.
	st.Failures = nil
	recordFailure(st, p, true, now)
	if f := st.Failures[poolKey(p)].decayed(now.Add(-time.Hour)); !near(f.weight(), 1) {
		t.Fatalf("clock set back: %+v", f)
	}
}

// A VM reclaimed soon after launch counts against its pool, beyond the
// hold-back; one reclaimed after a long run, or of unknown age, doesn't.
func TestEarlyReclaimCounts(t *testing.T) {
	g, _, now := dryNewestEnv(t)
	pool := SpotPrice{Region: "r-two-1", Zone: "r-two-1a", Type: "m6i.2xlarge"}
	failures := func() float64 {
		var w float64
		_ = g.withState(func(st *placementState) { w = st.Failures[poolKey(pool)].Reclaims })
		return w
	}
	in := Instance{Region: pool.Region, Zone: pool.Zone, Type: pool.Type}
	for _, c := range []struct {
		id  string
		age time.Duration
		add float64
	}{
		{"i-old", 2 * time.Hour, 0},
		{"i-unknown", -1, 0},
		{"i-future", -time.Hour, 0},
		{"i-2m", 2 * time.Minute, 1},
		{"i-2m", 2 * time.Minute, 0}, // the same notice again
		{"i-52m", 52 * time.Minute, 1},
	} {
		in.ID, in.LaunchedAt = c.id, now.Add(-c.age)
		if c.age == -1 {
			in.LaunchedAt = time.Time{}
		}
		before := failures()
		g.Reclaimed(in)
		if got := failures() - before; !near(got, c.add) {
			t.Errorf("%s: added %v, want %v", c.id, got, c.add)
		}
	}
	// Once the hold-back is over (five notices: the longest), the pool
	// still scores worse for a while.
	*now = now.Add(reclaimHoldMax + time.Minute)
	tbl, k := ranking(t, g)
	if len(g.HeldBack(tbl.Prices)) != 0 {
		t.Fatalf("still held back: %v", g.HeldBack(tbl.Prices))
	}
	found := false
	for _, p := range k.Prices {
		if poolKey(p) != poolKey(pool) {
			continue
		}
		found = true
		c, _ := k.CostOf(p)
		// Two early reclaims, a half-life (4 h) of decay later.
		want := (p.USD() + g.r.WaitCostPerHour()) * failCost.Hours() * 2 * math.Exp2(-(reclaimHoldMax+time.Minute).Hours()/failHalfLife.Hours())
		if c.FailedHere == nil || c.FailedHere.Capacity != 0 || !near(c.Failures, want) ||
			!strings.Contains(c.Factors(), "VMs reclaimed within 1 hour of launch") {
			t.Fatalf("after the hold: %+v %+v, want failures %v", c, c.FailedHere, want)
		}
	}
	if !found {
		t.Fatal("pool not ranked")
	}
}

// History too small to show as a count still says what it was.
func TestFailureHistoryTinyParts(t *testing.T) {
	h := FailureHistory{Capacity: 0.008, Reclaims: 0.008}
	if got := h.String(); got != "<0.1 capacity refusals; <0.1 VMs reclaimed within 1 hour of launch" {
		t.Fatalf("%q", got)
	}
}

// Failure history only weighs under run_cost: launch_order = "cheapest"
// still goes by price alone.
func TestFailureHistoryOnlyRunCost(t *testing.T) {
	g, f, now := dryNewestEnv(t)
	g.r.LaunchOrder = config.LaunchCheapest
	f.prices["r-one-1"][0][2] = "0.0500" // the dry pool is the cheapest
	// Enough history that run_cost would put the dry pool last.
	for i := 0; i < failMaxWeight; i++ {
		g.coolPool(SpotPrice{Region: "r-one-1", Zone: "r-one-1a", Type: "m7a.2xlarge"}, "InsufficientInstanceCapacity")
	}
	if _, k := ranking(t, g); k.Cost != nil || k.Model != nil || k.Prices[0].Type != "m7a.2xlarge" {
		t.Fatalf("cheapest ranked by run cost: %+v", k)
	}
	for round := 0; round < 3; round++ {
		*now = now.Add(poolCoolMax + time.Minute)
		resetCalls(f)
		if _, err := g.Launch(context.Background(), t.Logf); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(runsOf(f), "r-one-1 RunInstances m7a.2xlarge") {
			t.Fatalf("round %d: tried\n%s", round, runsOf(f))
		}
	}
}
