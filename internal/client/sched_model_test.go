package client

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

func TestModelPredict(t *testing.T) {
	s := &schedState{}
	now := time.Now()
	if _, _, ok := s.predict("t", 2); ok {
		t.Fatal("a prediction from no samples")
	}
	for _, sec := range []int{10, 20} {
		s.observe("t", 2, time.Duration(sec)*time.Second, now)
	}
	if _, _, ok := s.predict("t", 2); ok {
		t.Fatalf("a prediction from %d samples", 2)
	}
	s.observe("t", 2, 100*time.Second, now)
	mid, p90, ok := s.predict("t", 2)
	if !ok || mid != 20*time.Second || p90 != 100*time.Second {
		t.Fatalf("predict: %s %s %v, want 20s and 100s", mid, p90, ok)
	}
	// Another warmth with no samples of its own falls back to all of them.
	if mid, _, ok := s.predict("t", 0); !ok || mid != 20*time.Second {
		t.Fatalf("fallback: %s %v", mid, ok)
	}
	// Separate warmth levels are kept apart.
	for i := 0; i < 3; i++ {
		s.observe("t", 0, 300*time.Second, now)
	}
	if mid, _, _ := s.predict("t", 0); mid != 300*time.Second {
		t.Fatalf("cold lease %s, want 300s", mid)
	}
	if mid, _, _ := s.predict("t", 2); mid != 20*time.Second {
		t.Fatalf("warm lease %s, want 20s", mid)
	}
	// Bounded.
	for i := 0; i < 2*modelKeys; i++ {
		s.observe(TaskKey("r", string(rune('a'+i%26))+string(rune('a'+i/26))), 0, time.Second, now.Add(time.Duration(i)*time.Second))
	}
	if len(s.Model) > modelKeys {
		t.Fatalf("%d model keys, more than %d", len(s.Model), modelKeys)
	}
	for i := 0; i < 3*modelSamples; i++ {
		s.observe("u", 0, time.Second, now.Add(time.Hour))
	}
	if n := len(s.Model[modelKey("u", 0)].S); n != modelSamples {
		t.Fatalf("%d samples kept, want %d", n, modelSamples)
	}
}

// costPool is a pool whose VMs are priced, with idle_timeout 10m and a
// task whose lease is known to be d.
func costPool(t *testing.T, n int, d time.Duration) (*Pool, *fakeFleet) {
	p, f := schedPool(t, n, 2)
	p.R.HourlyCost = 0.12
	p.R.WaitCost = config.Dollars{V: 1, Set: true}
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, time.Now())
		s.EpochAt = time.Now().Add(time.Hour)
		for i := 0; i < modelMin; i++ {
			s.observe("job", -1, d, time.Now()) // any warmth: only "job|*"
		}
	})
	return p, f
}

func view(st *poolState, p *Pool, id string, now time.Time) vmView {
	for _, w := range p.views(st, now) {
		if w.in.ID == id {
			return w
		}
	}
	panic(id)
}

// The extra VM time a job costs uses the VM's idle time already run down:
// reviving a VM 599s into a 600s timeout costs nearly the whole lease plus
// that idle time, not just the lease.
func TestScoreChargesLifetimeExtended(t *testing.T) {
	p, _ := costPool(t, 3, 60*time.Second)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		// i-0: idle for 599s of 600.
		s.VMs["i-0"].LastEnd = now.Add(-599 * time.Second)
		// i-1: a resident releasing in 1s; the job's lease is 10 minutes.
		s.VMs["i-1"].LastStart = now
		s.Attempts["r1"] = &claimRec{Owner: processOwner(), VM: "i-1", State: attAcquired, Units: 1, GotAt: now, Expect: now.Add(time.Second)}
		// i-2: a resident releasing in 2 minutes.
		s.VMs["i-2"].LastStart = now
		s.Attempts["r2"] = &claimRec{Owner: processOwner(), VM: "i-2", State: attAcquired, Units: 1, GotAt: now, Expect: now.Add(2 * time.Minute)}

		added := func(id string, lease time.Duration) float64 {
			e0, ok := p.expiry(s, view(st, p, id, now), now)
			if !ok {
				t.Fatalf("%s: no expiry", id)
			}
			e1 := now.Add(lease + p.R.IdleTimeout.Duration)
			return math.Max(0, e1.Sub(e0).Seconds())
		}
		if got := added("i-0", 60*time.Second); math.Abs(got-659) > 1 {
			t.Fatalf("idle 599s, 60s lease: %.0fs added, want ~659 (lease + idle already run)", got)
		}
		if got := added("i-1", 10*time.Minute); math.Abs(got-599) > 1 {
			t.Fatalf("resident ending in 1s, 10m lease: %.0fs added, want ~599", got)
		}
		if got := added("i-2", 60*time.Second); got != 0 {
			t.Fatalf("resident outlasting a 60s lease: %.0fs added, want 0", got)
		}
		// The worked example: $0.12/h, wait_cost $1/h, a 60s job: the busy VM
		// costs only the waiting, the nearly idled-out one $0.02 more.
		a, _ := p.score(s, view(st, p, "i-0", now), claim{Task: "job"}, now)
		b, _ := p.score(s, view(st, p, "i-2", now), claim{Task: "job"}, now)
		if d := a.cost - b.cost; math.Abs(d-0.12/3600*659) > 1e-4 {
			t.Fatalf("cost difference $%.4f, want $%.4f", d, 0.12/3600*659)
		}
	})
}

// With prices and a learned lease, placement goes by measured cost: the
// busy VM whose job outlasts this one over the idle one (whatever the
// fixed order says), and reports it as measured. Without a price, it keeps
// the fixed order, as heuristic.
func TestRankMeasured(t *testing.T) {
	p, _ := costPool(t, 2, 60*time.Second)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		// i-0 holds this worktree's files (first in the fixed order) and is
		// idle 9 minutes; i-1 runs a 2-minute job.
		s.VMs["i-0"].remember("mine", "repo", now.Add(-9*time.Minute))
		s.VMs["i-0"].LastEnd = now.Add(-9 * time.Minute)
		s.VMs["i-1"].LastStart = now
		s.Attempts["r"] = &claimRec{Owner: processOwner(), VM: "i-1", State: attAcquired, Units: 1, GotAt: now, Expect: now.Add(2 * time.Minute)}
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{Worktree: "mine", Task: "job"})
	if len(cands) != 2 || cands[0].ID != "i-1" || cands[0].mode != "measured" {
		t.Fatalf("measured order %+v, want i-1 first", cands)
	}
	p.R.HourlyCost = 0
	cands, _, _, _ = p.Rank(context.Background(), nil, claim{Worktree: "mine", Task: "job"})
	if len(cands) != 2 || cands[0].ID != "i-0" || cands[0].mode != "heuristic" {
		t.Fatalf("unpriced order %+v, want the fixed order (i-0 first)", cands)
	}
	p.R.HourlyCost = 0.12
	cands, _, _, _ = p.Rank(context.Background(), nil, claim{Worktree: "mine", Task: "unknown"})
	if cands[0].mode != "heuristic" {
		t.Fatalf("a task with no history scored as %s", cands[0].mode)
	}
}

// A job whose lease may outlast the VM's lifetime goes last.
func TestRankAvoidsLifetimeEnd(t *testing.T) {
	p, f := costPool(t, 2, 30*time.Minute)
	p.R.MaxLifetime.Duration = 12 * time.Hour
	now := time.Now()
	f.insts[1].LaunchedAt = now.Add(-11*time.Hour - 20*time.Minute) // 40m left, 25m before the drain
	p.loaded(t, func(st *poolState) {
		if err := p.refresh(context.Background(), st, true); err != nil {
			t.Fatal(err)
		}
		s := st.sched()
		s.VMs["i-1"].LastStart = now // the busier, fixed-order favourite
		s.Attempts["r"] = &claimRec{Owner: processOwner(), VM: "i-1", State: attAcquired, Units: 1, GotAt: now, Expect: now.Add(time.Hour)}
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{Task: "job"})
	if len(cands) != 2 || cands[0].ID != "i-0" {
		t.Fatalf("order %+v: a 30m job went to a VM with 25m left", cands)
	}
}

// Releases teach the model; acquisitions get an expected release from it.
func TestLedgerTeachesModel(t *testing.T) {
	p, f := schedPool(t, 1, 4)
	for i := 0; i < modelMin; i++ {
		id, err := p.reserveOn(f.insts[0], claim{Task: "t", Worktree: "w"})
		if err != nil {
			t.Fatal(err)
		}
		p.commitAttempt(id, protocol.Acquired{MaxSlots: 4}, time.Time{})
		time.Sleep(5 * time.Millisecond)
		p.releaseAttempt(id)
	}
	s := p.state(t)
	if _, _, ok := s.predict("t", 2); !ok {
		t.Fatalf("no prediction after %d leases: %+v", modelMin, s.Model)
	}
	id, _ := p.reserveOn(f.insts[0], claim{Task: "t", Worktree: "w"})
	p.commitAttempt(id, protocol.Acquired{}, time.Time{})
	if a := p.state(t).Attempts[id]; a.Cache != 2 || a.Expect.IsZero() {
		t.Fatalf("attempt %+v: want cache 2 and an expected release", a)
	}
}

type fakePricer map[string]float64

func (f fakePricer) Prices(context.Context, []provider.Instance) map[string]float64 { return f }

// Prices are looked up at most every pricesEvery, for every process.
func TestRefreshPrices(t *testing.T) {
	p, _ := schedPool(t, 2, 2)
	pricesForAnyFleet = true
	calls := 0
	old := newFleetPricer
	newFleetPricer = func(*config.Runner) (fleetPricer, error) { calls++; return fakePricer{"i-0": 0.2}, nil }
	t.Cleanup(func() { newFleetPricer, pricesForAnyFleet = old, false })
	p.loaded(t, func(st *poolState) { p.views(st, time.Now()) })
	p.RefreshPrices(context.Background())
	p.RefreshPrices(context.Background())
	s := p.state(t)
	if calls != 1 {
		t.Fatalf("%d lookups, want 1", calls)
	}
	if usd, ok := s.VMs["i-0"].price(time.Now()); !ok || usd != 0.2 {
		t.Fatalf("i-0 price %v %v", usd, ok)
	}
	if _, ok := s.VMs["i-1"].price(time.Now()); ok {
		t.Fatal("an unpriced VM got a price")
	}
}
