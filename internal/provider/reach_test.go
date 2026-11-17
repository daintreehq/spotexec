package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

func twoRegionPrices() *fakeRegionsEC2 {
	return &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
		},
	}
}

// A region the client can't reach takes no launch, however well it ranks,
// until a VM there answers again (#225).
func TestGlobalUnreachableRegionTakesNoLaunch(t *testing.T) {
	f := twoRegionPrices()
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	ctx := context.Background()
	g.HoldRegion("r-one-1", "2 VMs there don't answer SSH: i-1, i-2")

	tbl, _ := g.Prices(ctx, false)
	if next, ok := g.NextLaunch(tbl); !ok || next.Region != "r-two-1" {
		t.Fatalf("next launch %+v, %v; want r-two-1", next, ok)
	}
	why := g.Cooldowns(tbl.Prices)["r-one-1/r-one-1a/c7a.2xlarge"]
	if !strings.Contains(why, "unreachable from this client") || !strings.Contains(why, "i-1, i-2") {
		t.Fatalf("prices don't say why r-one-1 is skipped: %q", why)
	}
	holds, open := g.RegionHolds()
	if _, ok := holds["r-one-1"]; !ok || len(holds) != 1 || !open {
		t.Fatalf("holds %v, open %v", holds, open)
	}

	in, err := g.Launch(ctx, t.Logf)
	if err != nil || in.Region != "r-two-1" {
		t.Fatalf("launched %+v, %v; want r-two-1", in, err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 || !strings.HasPrefix(runs[0], "r-two-1") {
		t.Fatalf("a launch request went to the held region: %v", runs)
	}
	if !strings.Contains(in.Placement.Why, "in a region this client can't reach") {
		t.Fatalf("placement doesn't say what was passed over: %q", in.Placement.Why)
	}

	// A VM there answers: the region ranks first again.
	g.ReachedRegion("r-one-1")
	if holds, _ := g.RegionHolds(); len(holds) != 0 {
		t.Fatalf("still held after an answer: %v", holds)
	}
	if in, err := g.Launch(ctx, t.Logf); err != nil || in.Region != "r-one-1" {
		t.Fatalf("launched %+v, %v; want r-one-1 again", in, err)
	}
}

// With every region held nothing is sent, and the error says why.
func TestGlobalEveryRegionUnreachable(t *testing.T) {
	f := twoRegionPrices()
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.HoldRegion("r-one-1", "i-1 has never answered SSH, 2m0s after its launch")
	g.HoldRegion("r-two-1", "2 VMs there don't answer SSH: i-2, i-3")
	_, err := g.Launch(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "r-one-1 is unreachable from this client") || !strings.Contains(err.Error(), "i-1 has never answered") ||
		!strings.Contains(err.Error(), "r-two-1 is unreachable from this client") {
		t.Fatalf("want an error naming both held regions, got %v", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 0 {
		t.Fatalf("launch requests sent: %v", runs)
	}
	if _, open := g.RegionHolds(); open {
		t.Fatal("open with its only region held")
	}
}

// Once a hold lapses one VM may launch there to try it; while it boots the
// region takes no other launch, and held again it lasts twice as long.
func TestGlobalUnreachableHoldLapsesToOneTrial(t *testing.T) {
	f := twoRegionPrices()
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	now := time.Now()
	g.now = func() time.Time { return now }
	ctx := context.Background()
	g.HoldRegion("r-one-1", "2 VMs there don't answer SSH: i-1, i-2")

	now = now.Add(unreachHoldMin + time.Second)
	if in, err := g.Launch(ctx, t.Logf); err != nil || in.Region != "r-one-1" {
		t.Fatalf("trial launch %+v, %v; want r-one-1", in, err)
	}
	// The trial VM is booting: the next launch goes elsewhere.
	if in, err := g.Launch(ctx, t.Logf); err != nil || in.Region != "r-two-1" {
		t.Fatalf("second launch %+v, %v; want r-two-1 while the trial boots", in, err)
	}
	// The trial VM never answers either: held again, twice as long.
	now = now.Add(2 * time.Minute)
	g.HoldRegion("r-one-1", "i-3 has never answered SSH, 2m0s after its launch")
	_ = g.withState(func(st *placementState) {
		c := st.Cooldown[unreachKey("r-one-1")]
		if c.Step != 2*unreachHoldMin || !c.Until.Equal(now.Add(2*unreachHoldMin)) || !c.Trial.IsZero() {
			t.Errorf("held again: %+v", c)
		}
	})
	// Still silent while held: extended from now, not doubled again.
	now = now.Add(time.Minute)
	g.HoldRegion("r-one-1", "i-3 has never answered SSH, 3m0s after its launch")
	_ = g.withState(func(st *placementState) {
		if c := st.Cooldown[unreachKey("r-one-1")]; c.Step != 2*unreachHoldMin || !c.Until.Equal(now.Add(2*unreachHoldMin)) {
			t.Errorf("renewed: %+v", c)
		}
	})
}

// A trial launch that fails sends nothing there to try: the next launch
// may try again.
func TestGlobalFailedTrialFreesTheRegion(t *testing.T) {
	f := twoRegionPrices()
	f.runErr = map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity"}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	now := time.Now()
	g.now = func() time.Time { return now }
	g.HoldRegion("r-one-1", "2 VMs there don't answer SSH: i-1, i-2")
	now = now.Add(unreachHoldMin + time.Second)
	if in, err := g.Launch(context.Background(), t.Logf); err != nil || in.Region != "r-two-1" {
		t.Fatalf("launched %+v, %v", in, err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 2 || !strings.HasPrefix(runs[0], "r-one-1") {
		t.Fatalf("want the trial tried in r-one-1 first: %v", runs)
	}
	_ = g.withState(func(st *placementState) {
		if c := st.Cooldown[unreachKey("r-one-1")]; !c.Trial.IsZero() || c.Step == 0 {
			t.Errorf("a failed trial still blocks the region (or the hold was lost): %+v", c)
		}
	})
	// Capacity back there (and its pool's capacity cooldown over), none in
	// r-two-1: the launch is r-one-1's trial, not skipped.
	f.runErr = map[string]string{"r-two-1/c7a.2xlarge": "InsufficientInstanceCapacity"}
	now = now.Add(poolCoolMin + time.Second)
	if in, err := g.Launch(context.Background(), t.Logf); err != nil || in.Region != "r-one-1" {
		t.Fatalf("launched %+v, %v; want r-one-1's trial", in, err)
	}
}

// A trial still booting keeps its hold however long ago the hold lapsed.
func TestGlobalPruningKeepsBootingTrial(t *testing.T) {
	g := newGlobalEnv(t, twoRegionPrices(), "r-one-1", "r-two-1")
	now := time.Now()
	g.now = func() time.Time { return now }
	g.HoldRegion("r-one-1", "x")
	now = now.Add(unreachHoldMin + unreachHoldMax - time.Minute)
	if !g.claimTrial("r-one-1") {
		t.Fatal("no trial")
	}
	now = now.Add(time.Minute + time.Second) // past Until + coolMemory
	_, _ = g.RegionHolds()                   // prunes after reading
	if holds, _ := g.RegionHolds(); len(holds) != 1 {
		t.Fatalf("the booting trial's hold was pruned: %v", holds)
	}
}

func TestSimPickRegion(t *testing.T) {
	now := time.Now()
	m := map[string]cooldown{}
	regions := []string{"sim-a", "sim-b"}
	if rg, trial, err := simPickRegion(m, regions, now, time.Minute); err != nil || rg != "sim-a" || trial {
		t.Fatalf("got %q, %v, %v", rg, trial, err)
	}
	holdUnreachable(m, unreachKey("sim-a"), "cut off", now)
	if rg, _, err := simPickRegion(m, regions, now, time.Minute); err != nil || rg != "sim-b" {
		t.Fatalf("got %q, %v; want sim-b while sim-a is held", rg, err)
	}
	holdUnreachable(m, unreachKey("sim-b"), "cut off", now)
	if _, _, err := simPickRegion(m, regions, now, time.Minute); err == nil || !strings.Contains(err.Error(), "held back") {
		t.Fatalf("want every region held, got %v", err)
	}
	later := now.Add(unreachHoldMin + time.Second)
	if rg, trial, err := simPickRegion(m, regions, later, time.Minute); err != nil || rg != "sim-a" || !trial {
		t.Fatalf("got %q, %v, %v; want a trial in sim-a", rg, trial, err)
	}
	if rg, trial, err := simPickRegion(m, regions, later, time.Minute); err != nil || rg != "sim-b" || !trial {
		t.Fatalf("got %q, %v, %v; want sim-b's trial while sim-a's boots", rg, trial, err)
	}
	if _, _, err := simPickRegion(m, regions, later.Add(30*time.Second), time.Minute); err == nil {
		t.Fatal("a third launch while both trials boot")
	}
	// A trial that never answered stops blocking once trialFor passes.
	if rg, trial, err := simPickRegion(m, regions, later.Add(time.Minute+time.Second), time.Minute); err != nil || rg != "sim-a" || !trial {
		t.Fatalf("got %q, %v, %v; want another trial in sim-a", rg, trial, err)
	}
}

// While a trial VM boots, the hold's deadline is the trial's.
func TestRegionHoldUntilCoversTrial(t *testing.T) {
	now := time.Now()
	m := map[string]cooldown{}
	holdUnreachable(m, unreachKey("sim-a"), "cut off", now)
	later := now.Add(unreachHoldMin + time.Second)
	if ok, trial := claimTrial(m, unreachKey("sim-a"), later, time.Minute); !ok || !trial {
		t.Fatal("no trial")
	}
	holds, open := regionHolds(m, []string{"sim-a", "sim-b"}, later, time.Minute)
	if h := holds["sim-a"]; !h.Until.Equal(later.Add(time.Minute)) || !strings.Contains(h.Why, "one VM launched") || !open {
		t.Fatalf("hold %+v, open %v", h, open)
	}
}

// Holding doubles each time it lapses and comes back, up to an hour.
func TestUnreachHoldDoublesToAnHour(t *testing.T) {
	now := time.Now()
	m := map[string]cooldown{}
	k := unreachKey("r")
	holdUnreachable(m, k, "x", now)
	for _, want := range []time.Duration{20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		now = m[k].Until.Add(time.Second)
		holdUnreachable(m, k, "x", now)
		if m[k].Step != want {
			t.Fatalf("step %s, want %s", m[k].Step, want)
		}
	}
}

func TestSimRegionHoldsAndCutOff(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_SIM_REGIONS", "sim-a, sim-b")
	r := config.Runner{Name: "s", Type: config.TypeSim, BootTimeout: config.Duration{Duration: time.Minute}}
	p, _ := NewSim(&r)
	s := p.(*Sim)
	if got := simRegions(); len(got) != 2 || got[0] != "sim-a" || got[1] != "sim-b" {
		t.Fatalf("regions %q", got)
	}
	s.HoldRegion("sim-a", "cut off")
	holds, open := s.RegionHolds()
	if _, ok := holds["sim-a"]; !ok || !open {
		t.Fatalf("holds %v, open %v", holds, open)
	}
	s.HoldRegion("sim-b", "cut off")
	if _, open := s.RegionHolds(); open {
		t.Fatal("open with every region held")
	}
	s.ReachedRegion("sim-b")
	s.ReachedRegion("sim-a")
	if holds, _ := s.RegionHolds(); len(holds) != 0 {
		t.Fatalf("still held: %v", holds)
	}
	if err := s.SetReachable("sim-b", false); err != nil {
		t.Fatal(err)
	}
	if cut := s.cutOff(); !cut["sim-b"] || len(cut) != 1 {
		t.Fatalf("cut off %v", cut)
	}
	_ = s.SetReachable("sim-b", true)
	if cut := s.cutOff(); len(cut) != 0 {
		t.Fatalf("cut off %v after restoring", cut)
	}
}

// #240: AnswersOutside lists the runner's VMs in the fleet's other launch
// regions (inactive ones included) and returns one that answered; errors,
// a region that never answers, and the asked region itself don't count.
func TestGlobalAnswersOutside(t *testing.T) {
	ctx := context.Background()
	f := &fakeRegionsEC2{listErr: map[string]string{"r-two-1": "UnauthorizedOperation"}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	discovered(t, g)
	if got := g.AnswersOutside(ctx, "r-one-1"); got != "r-three-1" {
		t.Fatalf("answered %q, want r-three-1", got)
	}
	for _, c := range f.callsOf("DescribeInstances") {
		if strings.HasPrefix(c, "r-one-1 ") {
			t.Fatalf("asked the silent region itself: %v", f.callsOf("DescribeInstances"))
		}
	}

	// Every other region fails, or never answers before ctx ends.
	f2 := &fakeRegionsEC2{listErr: map[string]string{"r-two-1": "UnauthorizedOperation"}, dead: map[string]bool{"r-three-1": true}, noRegions: true}
	g2 := newGlobalEnv(t, f2, "r-one-1", "r-two-1", "r-three-1")
	discovered(t, g2)
	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := g2.AnswersOutside(tctx, "r-one-1"); got != "" || time.Since(start) > 5*time.Second {
		t.Fatalf("answered %q after %s", got, time.Since(start))
	}

	// One region: nothing outside it to ask.
	f3 := &fakeRegionsEC2{}
	g3 := newGlobalEnv(t, f3, "r-one-1")
	g3.legacy = true
	if got := g3.AnswersOutside(ctx, "r-one-1"); got != "" || len(f3.callsOf("DescribeInstances")) != 0 {
		t.Fatalf("legacy runner answered %q, calls %v", got, f3.callsOf("DescribeInstances"))
	}
}
