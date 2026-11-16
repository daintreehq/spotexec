package client

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

// holdOn is hold with a predicted release (zero: unknown).
func holdOn(s *schedState, vm, worktree string, now, expect time.Time) {
	hold(s, vm, worktree, now)
	for _, a := range s.Attempts {
		if a.VM == vm && a.Worktree == worktree {
			a.Expect = expect
		}
	}
}

// fullFleet is one ready two-slot VM whose two jobs end at the given
// times, with `waiting` jobs waiting for a slot (-1: one exclusive job,
// waiting for the whole VM), as seen by a process that has waited `waited`.
func fullFleet(t *testing.T, waiting int, waited time.Duration, ends ...time.Time) *Pool {
	t.Helper()
	p, _ := schedPool(t, 1, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		allReady(st, now)
		p.views(st, now)
		for i, e := range ends {
			holdOn(s, "i-0", "wt"+string(rune('a'+i)), now, e)
		}
	})
	weights := make([]int, max(waiting, 0))
	for i := range weights {
		weights[i] = 1
	}
	if waiting < 0 {
		weights = []int{p.SlotsPerInstance()}
	}
	for _, n := range weights {
		w, err := p.Register(n)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Done)
	}
	p.since = now.Add(-waited)
	return p
}

// #226: a burst that fills every slot, on a VM whose jobs end in seconds,
// waits for those slots instead of launching a VM that would serve only
// after the burst is over; and says why.
func TestShortBurstWaitsForSlotsAboutToFree(t *testing.T) {
	now := time.Now()
	p := fullFleet(t, 1, time.Second, now.Add(10*time.Second), now.Add(20*time.Second))
	d, err := p.Plan(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Launch || !d.Held || d.Deferred != 1 || d.Lead != leadDefault || d.LeadLearned {
		t.Fatalf("burst: %+v, want held for the slot freeing in 10s", d)
	}
	if d.FreeIn <= 0 || d.FreeIn > 10*time.Second || !strings.Contains(d.Reason, "sooner than a new VM") {
		t.Fatalf("hold: free in %s, reason %q", d.FreeIn, d.Reason)
	}

	// Two waiting: both slots must free within the lead.
	p = fullFleet(t, 2, time.Second, now.Add(10*time.Second), now.Add(20*time.Second))
	if d, _ := p.Plan(context.Background(), 0); d.Launch || d.Deferred != 2 || d.FreeIn <= 10*time.Second {
		t.Fatalf("two waiting: %+v, want held until the second slot frees", d)
	}
}

// Demand still waiting after a launch's lead is sustained: it launches as
// before, whatever is predicted to free.
func TestSustainedWaitLaunches(t *testing.T) {
	now := time.Now()
	p := fullFleet(t, 1, leadDefault+time.Second, now.Add(10*time.Second), now.Add(20*time.Second))
	if d, _ := p.Plan(context.Background(), 0); !d.Launch || d.Held || d.Waited < leadDefault {
		t.Fatalf("sustained: %+v, want a launch", d)
	}
}

// A prediction that is missing, already overdue, or later than a launch
// would serve holds nothing; nor do too few releases, or a whole-VM waiter.
func TestHoldNeedsPredictedReleases(t *testing.T) {
	now := time.Now()
	soon := now.Add(5 * time.Second)
	for _, c := range []struct {
		name    string
		waiting int
		ends    []time.Time
	}{
		{"unknown", 1, []time.Time{{}, {}}},
		{"overdue", 1, []time.Time{now.Add(-time.Second), now.Add(-time.Second)}},
		{"after the lead", 1, []time.Time{now.Add(leadDefault + time.Minute), now.Add(leadDefault + time.Minute)}},
		{"too few", 2, []time.Time{soon, {}}},
		// An exclusive job needs a VM to empty, which freeing slots don't
		// show (two may free on different VMs).
		{"whole VM", -1, []time.Time{soon, soon}},
	} {
		p := fullFleet(t, c.waiting, time.Second, c.ends...)
		if d, err := p.Plan(context.Background(), 0); err != nil || !d.Launch || d.Held {
			t.Errorf("%s: %+v %v, want a launch", c.name, d, err)
		}
	}
}

// Slots freeing on a VM that isn't serving new jobs hold nothing.
func TestHoldIgnoresClosedVMs(t *testing.T) {
	now := time.Now()
	p := fullFleet(t, 1, time.Second, now.Add(5*time.Second), now.Add(5*time.Second))
	p.loaded(t, func(st *poolState) { st.note("i-0").Draining = true })
	if d, _ := p.Plan(context.Background(), 0); !d.Launch {
		t.Fatalf("draining VM: %+v, want a launch", d)
	}
}

// The lead is learned from timed launches: a fleet that serves in 30s
// launches rather than wait 45s for a slot.
func TestLearnedLeadDecides(t *testing.T) {
	now := time.Now()
	p := fullFleet(t, 1, time.Second, now.Add(45*time.Second), now.Add(45*time.Second))
	p.loaded(t, func(st *poolState) {
		for i, id := range []string{"i-x", "i-y", "i-z"} {
			st.sched().observeLaunch(id, time.Duration(25+5*i)*time.Second, now)
		}
	})
	d, _ := p.Plan(context.Background(), 0)
	if !d.Launch || d.Lead != 30*time.Second || !d.LeadLearned {
		t.Fatalf("fast fleet: %+v, want a launch with a learned 30s lead", d)
	}
}

func TestLaunchLead(t *testing.T) {
	now := time.Now()
	s := &schedState{}
	if l, ok := s.launchLead(now); l != leadDefault || ok {
		t.Fatalf("no samples: %s %v", l, ok)
	}
	s.observeLaunch("i-a", 4*time.Minute, now)
	s.observeLaunch("i-a", time.Second, now) // the same VM once
	s.observeLaunch("i-b", 6*time.Minute, now)
	if l, ok := s.launchLead(now); l != leadDefault || ok || len(s.Leads) != 2 {
		t.Fatalf("two samples: %s %v %+v", l, ok, s.Leads)
	}
	s.observeLaunch("i-c", 5*time.Minute, now)
	if l, ok := s.launchLead(now); l != 5*time.Minute || !ok {
		t.Fatalf("three samples: %s %v", l, ok)
	}
	for i := 0; i < leadSamples; i++ {
		s.observeLaunch("i-slow"+string(rune('a'+i)), time.Hour, now)
	}
	if l, _ := s.launchLead(now); l != leadMax || len(s.Leads) != leadSamples {
		t.Fatalf("slow launches: %s, %d samples", l, len(s.Leads))
	}
	if l, ok := s.launchLead(now.Add(modelAge + time.Hour)); l != leadDefault || ok {
		t.Fatalf("old samples: %s %v", l, ok)
	}
}

// A launch is timed when its VM, watched booting, first serves; not when
// it was first seen ready after nobody had probed it for a while.
func TestReadyTimesItsLaunch(t *testing.T) {
	p, f := testPool(t, 4)
	now := time.Now()
	f.insts = []provider.Instance{{ID: "i-new", State: provider.StateRunning, LaunchedAt: now, Target: provider.Target{Host: "192.0.2.9"}}}
	p.loaded(t, func(st *poolState) {
		st.Launches = []launchRec{
			{ID: "l1", InstanceID: "i-new", Started: now.Add(-3 * time.Minute), PID: os.Getpid()},
			{ID: "l2", InstanceID: "i-quiet", Started: now.Add(-3 * time.Minute), PID: os.Getpid()},
		}
	})
	p.Observe("i-new", OutBooting, now)
	p.Observe("i-new", OutAcquired, now)
	p.Observe("i-quiet", OutBusy, now) // never seen booting
	p.Observe("i-new", OutAcquired, now)
	s := p.state(t)
	if len(s.Leads) != 1 || s.Leads[0].Instance != "i-new" || s.Leads[0].S < 179 || s.Leads[0].S > 200 {
		t.Fatalf("leads %+v, want one ~3m sample for i-new", s.Leads)
	}
}

// Demand a fleet holds for slots about to free is covered: a
// lower-priority fleet doesn't launch for it, and the job gets the slot
// that frees, with the hold on record.
func TestHeldDemandIsCoveredAcrossFleets(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"broken": {"b1"}, "healthy": {"h1"}}, 4)
	first, second := NewPool(pool[0], fleets["broken"]), NewPool(pool[1], fleets["healthy"])
	now := time.Now()
	for _, c := range []struct {
		p    *Pool
		vm   string
		ends time.Time
	}{{first, "i-b1", now.Add(3 * time.Second)}, {second, "i-h1", time.Time{}}} {
		c.p.loaded(t, func(st *poolState) {
			s := st.sched()
			allReady(st, now)
			c.p.views(st, now)
			s.EpochAt = now
			holdOn(s, c.vm, "a", now, c.ends)
			holdOn(s, c.vm, "b", now, c.ends)
		})
	}
	go func() {
		// The first fleet's jobs end as predicted.
		time.Sleep(1500 * time.Millisecond)
		first.loaded(t, func(st *poolState) {
			s := st.sched()
			for id, a := range s.Attempts {
				if a.Worktree == "a" {
					delete(s.Attempts, id)
				}
			}
		})
	}()
	scriptHelpers(t, func(_ AcquireRequest, host string, _ int) (*Lease, error) {
		if host == "b1" {
			return &Lease{}, nil
		}
		return nil, nil // busy
	})
	rep := &placeRep{}
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: rep})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.InstanceID != "i-b1" {
		t.Fatalf("placed on %s, want the slot freeing on i-b1", l.InstanceID)
	}
	held := 0
	for i, n := range rep.names {
		switch {
		case n == "scale_out":
			t.Fatalf("launched for demand held for a slot about to free: %v", rep.events[i])
		case n == "scale_held":
			held++
			if rep.events[i]["runner"] != "broken" || rep.events[i]["reason"] == nil {
				t.Fatalf("scale_held %v", rep.events[i])
			}
		}
	}
	if held != 1 {
		t.Fatalf("%d scale_held events, want 1: %v", held, rep.names)
	}
}
