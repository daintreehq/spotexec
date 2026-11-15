package provider

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A zone and type whose VM got a Spot interruption notice ranks last for
// new launches for a while (longer if it keeps happening), so a fleet moves
// away from a pool that keeps reclaiming it; `spotexec prices` says why.
func TestGlobalReclaimedRanksLast(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	now := time.Now()
	g.now = func() time.Time { return now }
	reclaimed := Instance{ID: "i-1", Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"}
	g.Reclaimed(reclaimed)

	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if next, ok := g.NextLaunch(tbl); !ok || next.Region != "r-two-1" {
		t.Fatalf("next launch %+v", next)
	}
	held := g.HeldBack(tbl.Prices)
	if why := held["r-one-1/r-one-1a/c7a.2xlarge"]; !strings.Contains(why, "interruption") || !strings.Contains(why, "i-1") {
		t.Fatalf("held back %v", held)
	}
	if len(held) != 1 || len(g.Cooldowns(tbl.Prices)) != 0 {
		t.Fatalf("held back %v, cooling down %v", held, g.Cooldowns(tbl.Prices))
	}
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-two-1" {
		t.Fatalf("launched %+v, %v", in, err)
	}

	step := func() time.Duration {
		var s time.Duration
		_ = g.withState(func(st *placementState) {
			s = st.Cooldown[reclaimKey(SpotPrice{Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"})].Step
		})
		return s
	}
	// Another run on the same VM reporting the same notice doesn't count twice.
	g.Reclaimed(reclaimed)
	if s := step(); s != reclaimHoldMin {
		t.Fatalf("step after the same notice again: %v", s)
	}
	// Another VM reclaimed there: held back longer.
	g.Reclaimed(Instance{ID: "i-2", Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"})
	if s := step(); s != 2*reclaimHoldMin {
		t.Fatalf("step after a second VM: %v", s)
	}
	// The first VM's notice again, after the second's: still two VMs.
	g.Reclaimed(reclaimed)
	if s := step(); s != 2*reclaimHoldMin {
		t.Fatalf("step after the first notice again: %v", s)
	}
	// Missing placement details: nothing recorded.
	g.Reclaimed(Instance{ID: "i-3", Region: "r-two-1"})
	_ = g.withState(func(st *placementState) {
		for k := range st.Cooldown {
			if strings.HasPrefix(k, reclaimPrefix) && !strings.Contains(k, "r-one-1a") {
				t.Errorf("recorded %s", k)
			}
		}
		if _, ok := st.Reclaimed["i-3"]; ok {
			t.Error("recorded i-3")
		}
	})

	// Once the hold is over, the cheapest goes first again.
	now = now.Add(2*reclaimHoldMin + 15*time.Minute)
	if next, ok := g.NextLaunch(tbl); !ok || next.Region != "r-one-1" {
		t.Fatalf("next launch after the hold %+v", next)
	}
	// The backoff is remembered for a while after (longer than a capacity
	// cooldown's): a third reclaim holds it back longer still.
	g.Reclaimed(Instance{ID: "i-4", Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"})
	if s := step(); s != 4*reclaimHoldMin {
		t.Fatalf("step after a third VM: %v", s)
	}
	// Long after, a reclaim there starts afresh, even with no other use of
	// the state in between.
	now = now.Add(4*reclaimHoldMin + reclaimHoldMax + time.Minute)
	g.Reclaimed(Instance{ID: "i-5", Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"})
	if s := step(); s != reclaimHoldMin {
		t.Fatalf("step long after: %v", s)
	}
	_ = g.withState(func(st *placementState) {
		if _, ok := st.Reclaimed["i-1"]; ok {
			t.Error("old reclaimed VMs are never forgotten")
		}
	})
}

// A held-back option is still an option: when everything else fails, the
// launch goes there rather than failing.
func TestGlobalReclaimedIsLastResort(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
		},
		runErr: map[string]string{"r-two-1/c7a.2xlarge": "InsufficientInstanceCapacity"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.Reclaimed(Instance{ID: "i-1", Region: "r-one-1", Zone: "r-one-1a", Type: "c7a.2xlarge"})
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-one-1" {
		t.Fatalf("launched %+v, %v", in, err)
	}
	runs := f.callsOf("RunInstances")
	if len(runs) != 2 || !strings.HasPrefix(runs[0], "r-two-1") {
		t.Fatalf("want r-two-1 tried first, then the held-back r-one-1: %v", runs)
	}
	// Launching there doesn't lift the hold.
	tbl, _ := g.Prices(context.Background(), false)
	if len(g.HeldBack(tbl.Prices)) != 1 {
		t.Fatal("a launch into a held-back option cleared the hold")
	}
	// With every other option skipped, it is the next launch.
	if next, ok := g.NextLaunch(tbl); !ok || next.Region != "r-one-1" {
		t.Fatalf("next launch %+v, %v", next, ok)
	}
}
