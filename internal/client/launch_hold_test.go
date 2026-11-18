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
// waiting for the whole VM; -2: the same, weighing one slot as on a fleet
// of mixed sizes), as seen by a process that has waited `waited`.
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
	register := func(n int, excl bool) {
		t.Helper()
		reg := p.Register
		if excl {
			reg = p.RegisterExclusive
		}
		w, err := reg(n)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Done)
	}
	switch waiting {
	case -1:
		register(p.SlotsPerInstance(), true)
	case -2:
		register(1, true)
	default:
		for range waiting {
			register(1, false)
		}
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

// Jobs a helper admitted beyond its slots (#227 overflow, preparing
// beside them) take the first slots that free there: those releases don't
// hold a launch for jobs still waiting to be admitted.
func TestHoldSkipsSlotsOverflowJobsTake(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	soon, later := now.Add(10*time.Second), now.Add(20*time.Second)

	// Two running jobs end soon, but two overflow jobs wait for their
	// slots; the overflow jobs end only later.
	p := fullFleet(t, 1, time.Second, soon, soon, time.Time{}, time.Time{})
	if d, _ := p.Plan(ctx, 0); !d.Launch || d.Held {
		t.Fatalf("slots go to overflow jobs: %+v, want a launch", d)
	}
	// One overflow job: the second slot to free is the waiting job's.
	p = fullFleet(t, 1, time.Second, soon, later, time.Time{})
	d, _ := p.Plan(ctx, 0)
	if d.Launch || !d.Held || d.Deferred != 1 || d.FreeIn <= 10*time.Second {
		t.Fatalf("one overflow job: %+v, want held until the second slot frees", d)
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

// A prediction that is missing, or already overdue, holds nothing once a
// lone job has waited past briefWaitFor (before, briefWait may hold it);
// one later than a launch would serve holds nothing; nor do too few
// releases, a whole-VM waiter, or a job overdue for long.
func TestHoldNeedsPredictedReleases(t *testing.T) {
	now := time.Now()
	soon := now.Add(5 * time.Second)
	brief, past := time.Second, briefWaitFor+time.Second
	for _, c := range []struct {
		name    string
		waiting int
		waited  time.Duration
		ends    []time.Time
	}{
		{"unknown", 1, past, []time.Time{{}, {}}},
		{"overdue", 1, past, []time.Time{now.Add(-time.Second), now.Add(-time.Second)}},
		{"long overdue", 1, brief, []time.Time{now.Add(-briefWaitFor - time.Second), now.Add(-time.Minute)}},
		{"after the lead", 1, brief, []time.Time{now.Add(leadDefault + time.Minute), now.Add(leadDefault + time.Minute)}},
		{"too few", 2, brief, []time.Time{soon, {}}},
		// An exclusive job needs a VM to empty, which freeing slots don't
		// show (two may free on different VMs).
		{"whole VM", -1, brief, []time.Time{soon, soon}},
		{"whole VM weighing a slot", -2, brief, []time.Time{soon, soon}},
		{"whole VM, ends unknown", -2, brief, []time.Time{{}, {}}},
		{"two waiting, ends unknown", 2, brief, []time.Time{{}, {}}},
	} {
		p := fullFleet(t, c.waiting, c.waited, c.ends...)
		if d, err := p.Plan(context.Background(), 0); err != nil || !d.Launch || d.Held {
			t.Errorf("%s: %+v %v, want a launch", c.name, d, err)
		}
	}
}

// #297: a lone job that has waited seconds on a full VM whose jobs' ends
// are unknown, or only just due, waits up to briefWaitFor for one of them
// to end instead of launching a VM it would likely never use; and says
// why. No launch is recorded while it waits.
func TestLoneShortWaitHoldsForAFullVM(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		ends []time.Time
	}{
		{"unknown", []time.Time{{}, {}}},
		{"just due", []time.Time{now.Add(-time.Second), now.Add(time.Hour)}},
		{"one unknown", []time.Time{{}, now.Add(leadDefault + time.Minute)}},
	} {
		p := fullFleet(t, 1, 8*time.Second, c.ends...)
		d, err := p.Plan(ctx, 0)
		if err != nil || d.Launch || !d.Held || d.Deferred != 1 || d.FreeIn != 0 || !strings.Contains(d.Reason, "may end any moment") {
			t.Fatalf("%s: %+v %v, want held for a brief wait", c.name, d, err)
		}
		p.loaded(t, func(st *poolState) {
			if len(st.Launches) != 0 {
				t.Fatalf("%s: launch recorded while held: %+v", c.name, st.Launches)
			}
		})
	}

	// A predicted release within the lead is the ordinary hold, which says
	// when the slot frees.
	p := fullFleet(t, 1, 8*time.Second, time.Time{}, now.Add(30*time.Second))
	if d, _ := p.Plan(ctx, 0); !d.Held || d.FreeIn <= 0 || !strings.Contains(d.Reason, "sooner than a new VM") {
		t.Fatalf("predicted release: %+v, want the ordinary hold", d)
	}
	// Another waiter, even one a higher-priority fleet covers, is a burst.
	p = fullFleet(t, 2, 8*time.Second, time.Time{}, time.Time{})
	if d, _ := p.Plan(ctx, 1); !d.Launch || d.Held {
		t.Fatalf("second waiter covered upstream: %+v, want a launch", d)
	}

	// The wait is bounded: past briefWaitFor the same demand launches.
	p = fullFleet(t, 1, 16*time.Second, time.Time{}, time.Time{})
	if d, _ := p.Plan(ctx, 0); !d.Launch || d.Held {
		t.Fatalf("after the brief wait: %+v, want a launch", d)
	}
	// So it is by a learned lead shorter than it.
	p = fullFleet(t, 1, 8*time.Second, time.Time{}, time.Time{})
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		for range modelMin {
			s.Leads = append(s.Leads, leadSample{At: now, S: 5})
		}
	})
	if d, _ := p.Plan(ctx, 0); !d.Launch || d.Held {
		t.Fatalf("lead shorter than the wait: %+v, want a launch", d)
	}
	// A VM with jobs admitted beyond its slots (even one whose helper
	// admits more), or with a holder that is uncertain besides its full
	// slots, isn't the lone job's to wait for; nor is one with no address.
	p = fullFleet(t, 1, 8*time.Second, time.Time{}, time.Time{}, time.Time{})
	p.loaded(t, func(st *poolState) { st.sched().VMs["i-0"].Slots, st.sched().VMs["i-0"].Admit = 2, 4 })
	if d, _ := p.Plan(ctx, 0); !d.Launch || d.Held {
		t.Fatalf("overflow: %+v, want a launch", d)
	}
	p = fullFleet(t, 1, 8*time.Second, time.Time{}, time.Time{}, time.Time{})
	p.loaded(t, func(st *poolState) {
		for _, a := range st.sched().Attempts {
			if a.Worktree == "wtc" {
				a.State = attUncertain
			}
		}
	})
	if d, _ := p.Plan(ctx, 0); !d.Launch || d.Held {
		t.Fatalf("uncertain holder: %+v, want a launch", d)
	}
	p = fullFleet(t, 1, 8*time.Second, time.Time{}, time.Time{})
	p.loaded(t, func(st *poolState) { st.Instances[0].Target.Host = "" })
	if d, _ := p.Plan(ctx, 0); d.Held {
		t.Fatalf("no address: %+v, want no hold", d)
	}
}

// Slots freeing hold nothing on a VM that won't take the waiting job, nor
// from a job not known to hold its slot.
func TestHoldIgnoresUnservingVMs(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   func(st *poolState)
	}{
		{"draining", func(st *poolState) { st.note("i-0").Draining = true }},
		{"retiring", func(st *poolState) { st.note("i-0").Retiring = true }},
		{"outdated", func(st *poolState) { st.note("i-0").Mismatch = "wrong type" }},
		{"suspect", func(st *poolState) { st.note("i-0").SuspectUntil = time.Now().Add(time.Minute) }},
		{"never ready", func(st *poolState) { st.note("i-0").Ready = false }},
		{"no note", func(st *poolState) { delete(st.Notes, "i-0") }},
		{"fenced", func(st *poolState) { st.sched().Fences["i-0"] = &fence{Owner: "other", At: time.Now()} }},
		{"not listed", func(st *poolState) {
			for _, a := range st.sched().Attempts {
				a.VM = "i-gone"
			}
		}},
	} {
		now := time.Now()
		p := fullFleet(t, 1, time.Second, now.Add(5*time.Second), now.Add(5*time.Second))
		p.loaded(t, c.fn)
		if d, _ := p.Plan(context.Background(), 0); d.Held {
			t.Errorf("%s: %+v, want no hold", c.name, d)
		}
	}
	for _, state := range []string{attReserved, attSent, attUncertain} {
		now := time.Now()
		p := fullFleet(t, 1, time.Second, now.Add(5*time.Second), now.Add(5*time.Second))
		p.loaded(t, func(st *poolState) {
			for _, a := range st.sched().Attempts {
				a.State = state
			}
		})
		if d, _ := p.Plan(context.Background(), 0); d.Held {
			t.Errorf("%s attempts: %+v, want no hold", state, d)
		}
	}
}

// Only demand nothing else covers waits, and only for releases that aren't
// already counted as starting capacity.
func TestHoldCoversOnlyResidualDemand(t *testing.T) {
	now := time.Now()
	soon := now.Add(5 * time.Second)
	ctx := context.Background()

	// Three waiting, two covered by a higher-priority fleet: one slot
	// freeing is enough.
	p := fullFleet(t, 3, time.Second, soon, time.Time{})
	if d, _ := p.Plan(ctx, 2); !d.Held || d.Demand != 1 || d.Deferred != 1 {
		t.Fatalf("covered upstream: %+v, want one slot held", d)
	}
	// Fully covered upstream: nothing to hold for.
	if d, _ := p.Plan(ctx, 3); d.Held || d.Launch || d.Deferred != 0 {
		t.Fatalf("all covered: %+v", d)
	}

	// A job holding two slots (exclusive) frees both for two shared jobs.
	p = fullFleet(t, 2, time.Second)
	p.loaded(t, func(st *poolState) {
		holdOn(st.sched(), "i-0", "x", now, soon)
		for _, a := range st.sched().Attempts {
			a.Units, a.Exclusive = 2, true
		}
	})
	if d, _ := p.Plan(ctx, 0); !d.Held || d.Deferred != 2 {
		t.Fatalf("exclusive job ending: %+v, want both slots held", d)
	}

	// The VM is young enough that its launch still counts its two slots as
	// starting: its job ending soon isn't a second source of slots, so the
	// third waiting job launches.
	p = fullFleet(t, 3, time.Second, soon, soon)
	p.loaded(t, func(st *poolState) {
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-0", Started: now.Add(-time.Minute), PID: os.Getpid()}}
		st.note("i-0").ReadyAt = now
	})
	if d, _ := p.Plan(ctx, 0); !d.Launch || d.Held {
		t.Fatalf("release on a starting VM: %+v, want a launch", d)
	}

	// Without a waiting clock (no Acquire behind it), Plan never holds.
	p = fullFleet(t, 1, time.Second, soon, soon)
	p.since = time.Time{}
	if d, _ := p.Plan(ctx, 0); !d.Launch {
		t.Fatalf("no waiting clock: %+v, want a launch", d)
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
// it was first seen ready after nobody had probed it for a while, nor
// after a lone failure to answer (a VM ready unobserved fails those too).
func TestReadyTimesItsLaunch(t *testing.T) {
	p, f := testPool(t, 4)
	now := time.Now()
	for _, id := range []string{"i-new", "i-busy", "i-quiet", "i-flaky"} {
		f.insts = append(f.insts, provider.Instance{ID: id, State: provider.StateRunning, LaunchedAt: now, Target: provider.Target{Host: "192.0.2.9"}})
	}
	p.loaded(t, func(st *poolState) {
		for _, id := range []string{"i-new", "i-busy", "i-quiet", "i-flaky"} {
			st.Launches = append(st.Launches, launchRec{ID: "l-" + id, InstanceID: id, Started: now.Add(-3 * time.Minute), PID: os.Getpid()})
		}
	})
	p.Observe("i-new", OutBooting, now)
	p.Observe("i-new", OutAcquired, now)
	p.Observe("i-new", OutAcquired, now)
	p.Observe("i-busy", OutBooting, now)
	p.Observe("i-busy", OutBusy, now)
	p.Observe("i-quiet", OutBusy, now) // never seen booting
	p.Observe("i-flaky", OutUnreachable, now)
	p.Observe("i-flaky", OutAcquired, now)
	got := map[string]float64{}
	for _, l := range p.state(t).Leads {
		got[l.Instance] = l.S
	}
	if len(got) != 2 || got["i-new"] < 179 || got["i-new"] > 200 || got["i-busy"] < 179 {
		t.Fatalf("leads %v, want ~3m samples for i-new and i-busy only", got)
	}
}

// The booting sighting must be recent: a VM last seen booting longer ago
// than leadGap may have been ready unobserved since.
func TestReadyAfterUnwatchedGapIsNotTimed(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		seen time.Time
		want int
	}{{now.Add(-leadGap), 1}, {now.Add(-leadGap - time.Second), 0}} {
		st := &poolState{Notes: map[string]*note{}}
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-a", Started: now.Add(-3 * time.Minute)}}
		st.note("i-a").BootSeenAt = c.seen
		st.markReady("i-a", now)
		if n := len(st.sched().Leads); n != c.want || !st.Notes["i-a"].Ready {
			t.Errorf("seen booting %s before ready: %d samples, want %d", now.Sub(c.seen), n, c.want)
		}
	}
}

// #243: a baked VM can finish first-boot setup before it first answers,
// so it is never seen booting. Watched steadily from its launch until it
// first served, its launch is timed all the same; a watch that started
// late, broke off, or is a lone failure to answer times nothing.
func TestWatchedLaunchIsTimed(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	type probe struct {
		s    int
		late bool
	}
	for _, c := range []struct {
		name     string
		launched time.Time // the provider's launch time
		probes   []probe
		ready    int
		want     bool
	}{
		{"steady", at(5), []probe{{20, false}, {35, false}, {50, false}}, 60, true},
		{"edges", at(5), []probe{{65, false}, {95, false}}, 125, true},
		{"anchored on the planned launch", time.Time{}, []probe{{60, false}, {90, false}}, 120, true},
		{"provider time before the plan", at(-5), []probe{{60, false}, {90, false}}, 120, true},
		{"provider clock ahead", at(3600), []probe{{50, false}, {70, false}}, 80, true},
		{"one probe", at(5), []probe{{20, false}}, 25, false},
		{"started late", at(5), []probe{{66, false}, {80, false}}, 90, false},
		{"started late, no provider time", time.Time{}, []probe{{61, false}, {80, false}}, 90, false},
		{"gap", at(5), []probe{{20, false}, {51, false}, {60, false}}, 70, false},
		{"ready unobserved", at(5), []probe{{20, false}, {35, false}}, 66, false},
		{"late", at(5), []probe{{20, false}, {35, false}, {50, true}}, 60, false},
		{"clock went back", at(5), []probe{{20, false}, {35, false}, {30, false}}, 40, false},
	} {
		st := &poolState{Notes: map[string]*note{}}
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-a", Started: t0}}
		for _, p := range c.probes {
			st.watch("i-a", c.launched, p.late, at(p.s))
		}
		st.markReady("i-a", at(c.ready))
		if got := len(st.sched().Leads) == 1; got != c.want {
			t.Errorf("%s: timed %v, want %v (note %+v)", c.name, got, c.want, *st.Notes["i-a"])
		}
		if c.want && st.sched().Leads[0].S != float64(c.ready) {
			t.Errorf("%s: lead %vs, want %ds from planning", c.name, st.sched().Leads[0].S, c.ready)
		}
	}

	// A VM probed before its launch record names it isn't watched yet:
	// the watch starts with the first probe that can be anchored. The
	// count saturates, and once ready nothing more is timed or watched.
	st := &poolState{Notes: map[string]*note{}}
	st.watch("i-a", at(5), false, at(15))
	if n := st.Notes["i-a"]; n.WatchN != 0 {
		t.Fatalf("unrecorded launch: %+v, want no watch", *n)
	}
	st.Launches = []launchRec{{ID: "l1", InstanceID: "i-a", Started: t0}}
	for _, s := range []int{25, 40, 45} {
		st.watch("i-a", at(5), false, at(s))
	}
	if n := st.Notes["i-a"]; n.WatchN != watchMin || !n.WatchAt.Equal(at(45)) {
		t.Fatalf("watched thrice: %+v, want %d probes, the last at 45s", *n, watchMin)
	}
	st.markReady("i-a", at(50))
	st.watch("i-a", at(5), false, at(55))
	st.markReady("i-a", at(60))
	if l, n := st.sched().Leads, st.Notes["i-a"]; len(l) != 1 || l[0].S != 50 || !n.WatchAt.Equal(at(45)) {
		t.Fatalf("leads %+v, note %+v", l, *n)
	}

	// A failed launch record anchors nothing.
	st = &poolState{Notes: map[string]*note{}}
	st.Launches = []launchRec{{ID: "l1", InstanceID: "i-a", Started: t0, Failed: true}}
	st.watch("i-a", at(5), false, at(15))
	if n := st.Notes["i-a"]; n.WatchN != 0 {
		t.Fatalf("failed launch: %+v, want no watch", *n)
	}
}

// Observe feeds the watch: failures to answer and booting replies extend
// it, other replies don't, and a VM still silent past boot_timeout (or in
// a region held as unreachable) breaks it. An unknown provider launch time
// isn't past boot_timeout.
func TestObserveWatchesLaunches(t *testing.T) {
	p, f := testPool(t, 8)
	p.R.BootTimeout.Duration = 5 * time.Second
	now := time.Now()
	ids := []string{"i-zero", "i-old", "i-boot", "i-refused", "i-held"}
	for _, id := range ids {
		in := provider.Instance{ID: id, State: provider.StateRunning, LaunchedAt: now, Target: provider.Target{Host: "192.0.2.9"}}
		if id == "i-held" {
			in.Region = "far"
		}
		f.insts = append(f.insts, in)
	}
	p.loaded(t, func(st *poolState) {
		for _, id := range ids {
			st.Launches = append(st.Launches, launchRec{ID: "l-" + id, InstanceID: id, Started: now.Add(-3 * time.Second), PID: os.Getpid()})
		}
		st.Held = map[string]time.Time{"far": time.Now()}
	})
	for _, o := range []Outcome{OutUnreachable, OutUnreachable} {
		p.Observe("i-zero", o, time.Time{})
		p.Observe("i-old", o, now.Add(-10*time.Second))
		p.Observe("i-held", o, now)
	}
	p.loaded(t, func(st *poolState) { st.note("i-boot").WatchN, st.note("i-boot").WatchAt = 1, time.Now() })
	p.Observe("i-boot", OutBooting, now)
	p.Observe("i-refused", outRefused, now)
	p.Observe("i-refused", outRefused, now)
	want := map[string]int{"i-zero": watchMin, "i-old": -1, "i-boot": watchMin, "i-refused": 0, "i-held": -1}
	p.loaded(t, func(st *poolState) {
		for id, n := range want {
			if got := st.note(id).WatchN; got != n {
				t.Errorf("%s: watch %d, want %d", id, got, n)
			}
		}
	})
}

// Three launches watched from launch to serving, never seen booting, make
// the lead learned, which scale_out events report as lead_learned.
func TestWatchedLaunchesLearnTheLead(t *testing.T) {
	p, f := testPool(t, 4)
	now := time.Now()
	ids := []string{"i-a", "i-b", "i-c"}
	for _, id := range ids {
		f.insts = append(f.insts, provider.Instance{ID: id, State: provider.StateRunning, LaunchedAt: now, Target: provider.Target{Host: "192.0.2.9"}})
	}
	p.loaded(t, func(st *poolState) {
		for _, id := range ids {
			st.Launches = append(st.Launches, launchRec{ID: "l-" + id, InstanceID: id, Started: now.Add(-40 * time.Second), PID: os.Getpid()})
		}
	})
	for _, id := range ids {
		p.Observe(id, OutUnreachable, now)
		p.Observe(id, OutUnreachable, now)
		p.Observe(id, OutAcquired, now)
	}
	d, err := p.Plan(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !d.LeadLearned || d.Lead < 40*time.Second || d.Lead > time.Minute {
		t.Fatalf("plan %+v, want a learned ~40s lead", d)
	}
}

// spotexec up watches its VM until it is ready, so it times the launch.
func TestPrewarmTimesItsLaunch(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 2)
	ids, _ := p.Reserve(ctx, 1)
	in, err := p.Prewarm(ctx, ids[0], nil, func(context.Context, provider.Instance) error {
		// A setup slow enough that the launch record is pruned meanwhile.
		return p.with(func(st *poolState) error { st.Launches = nil; return nil })
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l := p.state(t).Leads; len(l) != 1 || l[0].Instance != in.ID {
		t.Fatalf("leads %+v, want one for %s", l, in.ID)
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
	}{{first, "i-b1", now.Add(30 * time.Second)}, {second, "i-h1", time.Time{}}} {
		c.p.loaded(t, func(st *poolState) {
			s := st.sched()
			allReady(st, now)
			c.p.views(st, now)
			s.EpochAt = now
			holdOn(s, c.vm, "a", now, c.ends)
			holdOn(s, c.vm, "b", now, c.ends)
		})
	}
	rep := &roundRep{rounds: make(chan struct{}, 64)}
	released := make(chan error, 1)
	go func() {
		// Once the job has planned (and held) a few rounds, the first
		// fleet's job ends as predicted.
		for range 3 {
			<-rep.rounds
		}
		released <- first.with(func(st *poolState) error {
			s := st.sched()
			for id, a := range s.Attempts {
				if a.Worktree == "a" {
					delete(s.Attempts, id)
				}
			}
			return nil
		})
	}()
	scriptHelpers(t, func(_ AcquireRequest, host string, _ int) (*Lease, error) {
		if host == "b1" {
			return &Lease{}, nil
		}
		return nil, nil // busy
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: rep})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("%d scale_held events over several held rounds, want 1: %v", held, rep.names)
	}
}

// roundRep records events, and signals each queue round (its status line).
type roundRep struct {
	placeRep
	rounds chan struct{}
}

func (r *roundRep) Status(string, ...any) {
	select {
	case r.rounds <- struct{}{}:
	default:
	}
}
