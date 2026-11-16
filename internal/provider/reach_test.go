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
	f.prices["r-two-1"] = nil
	g := newGlobalEnv(t, f, "r-one-1")
	g.HoldRegion("r-one-1", "i-1 has never answered SSH, 2m0s after its launch")
	_, err := g.Launch(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "r-one-1 is unreachable from this client") || !strings.Contains(err.Error(), "i-1 has never answered") {
		t.Fatalf("want an error naming the held region, got %v", err)
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
	_ = g.withState(func(st *placementState) {
		if c := st.Cooldown[unreachKey("r-one-1")]; !c.Trial.IsZero() {
			t.Errorf("a failed trial still blocks the region: %+v", c)
		}
	})
}

func TestSimPickRegion(t *testing.T) {
	now := time.Now()
	m := map[string]cooldown{}
	regions := []string{"sim-a", "sim-b"}
	if rg, err := simPickRegion(m, regions, now, time.Minute); err != nil || rg != "sim-a" {
		t.Fatalf("got %q, %v", rg, err)
	}
	holdUnreachable(m, unreachKey("sim-a"), "cut off", now)
	if rg, err := simPickRegion(m, regions, now, time.Minute); err != nil || rg != "sim-b" {
		t.Fatalf("got %q, %v; want sim-b while sim-a is held", rg, err)
	}
	holdUnreachable(m, unreachKey("sim-b"), "cut off", now)
	if _, err := simPickRegion(m, regions, now, time.Minute); err == nil || !strings.Contains(err.Error(), "held back") {
		t.Fatalf("want every region held, got %v", err)
	}
	later := now.Add(unreachHoldMin + time.Second)
	if rg, err := simPickRegion(m, regions, later, time.Minute); err != nil || rg != "sim-a" {
		t.Fatalf("got %q, %v; want a trial in sim-a", rg, err)
	}
	if rg, err := simPickRegion(m, regions, later, time.Minute); err != nil || rg != "sim-b" {
		t.Fatalf("got %q, %v; want sim-b's trial while sim-a's boots", rg, err)
	}
}

func TestSimRegionHoldsAndCutOff(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_SIM_REGIONS", "sim-a, sim-b")
	r := config.Runner{Name: "s", Type: config.TypeSim, BootTimeout: config.Duration{Duration: time.Minute}}
	p, _ := NewSim(&r)
	s := p.(*Sim)
	s.HoldRegion("sim-a", "cut off")
	holds, open := s.RegionHolds()
	if _, ok := holds["sim-a"]; !ok || !open {
		t.Fatalf("holds %v, open %v", holds, open)
	}
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
