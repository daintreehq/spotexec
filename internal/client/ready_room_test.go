package client

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// readyFleet is `vms` ready four-slot VMs, idle by this machine's ledger,
// with `shared` one-slot jobs and `exclusive` whole-VM jobs waiting, as
// seen by a process that has waited `waited`.
func readyFleet(t *testing.T, vms, shared, exclusive int, waited time.Duration) *Pool {
	t.Helper()
	p, _ := schedPool(t, vms, 4)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		allReady(st, now)
		p.views(st, now)
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
	for range exclusive {
		register(p.SlotsPerInstance(), true)
	}
	for range shared {
		register(1, false)
	}
	p.since = now.Add(-waited)
	return p
}

// #239: a multi-part run (one exclusive part, three shared) waiting on a
// fleet whose two VMs are ready and idle launches nothing: one empty VM
// covers the exclusive part, the other's free slots the shared ones.
func TestReadyVMsCoverMultipartDemand(t *testing.T) {
	p := readyFleet(t, 2, 3, 1, time.Second)
	d, err := p.Plan(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Launch || d.Held || d.Ready != 7 || d.Pending != 0 || !strings.Contains(d.Reason, "ready VMs") {
		t.Fatalf("idle fleet with room: %+v, want no launch, 7 slots covered by ready VMs", d)
	}
	// A VM with some slots taken still covers shared demand in the rest.
	p = readyFleet(t, 2, 2, 1, time.Second)
	p.loaded(t, func(st *poolState) {
		hold(st.sched(), "i-1", "a", time.Now())
		hold(st.sched(), "i-1", "b", time.Now())
	})
	if d, _ := p.Plan(context.Background(), 0); d.Launch || d.Ready != 6 {
		t.Fatalf("partly busy VM: %+v, want the exclusive part on i-0, the shared ones beside i-1's jobs", d)
	}
}

// Demand ready VMs can't cover launches as before.
func TestReadyVMsThatDontFitLaunch(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name              string
		vms, shared, excl int
		waited            time.Duration
		fn                func(st *poolState)
	}{
		// The exclusive part takes the only VM; the shared parts need another.
		{name: "one VM", vms: 1, shared: 3, excl: 1},
		// Two exclusive parts can't share an empty VM.
		{name: "two exclusive", vms: 2, excl: 2, fn: func(st *poolState) { hold(st.sched(), "i-1", "a", now) }},
		// Free slots spread over VMs don't make an empty one.
		{name: "fragmented", vms: 2, excl: 1, fn: func(st *poolState) {
			hold(st.sched(), "i-0", "a", now)
			hold(st.sched(), "i-1", "b", now)
		}},
		{name: "more shared than free slots", vms: 1, shared: 5},
		// Past a launch's lead the ledger's view of free slots is not
		// trusted (another machine's jobs may fill them).
		{name: "sustained", vms: 1, shared: 1, waited: leadDefault + time.Second},
		{name: "busy hint", vms: 1, shared: 1, fn: func(st *poolState) { st.note("i-0").BusyUntil = now.Add(time.Minute) }},
		{name: "suspect", vms: 1, shared: 1, fn: func(st *poolState) { st.note("i-0").SuspectUntil = now.Add(time.Minute) }},
		{name: "draining", vms: 1, shared: 1, fn: func(st *poolState) { st.note("i-0").Draining = true }},
		{name: "outdated", vms: 1, shared: 1, fn: func(st *poolState) { st.note("i-0").Mismatch = "wrong type" }},
		{name: "never ready", vms: 1, shared: 1, fn: func(st *poolState) { st.note("i-0").Ready = false }},
		{name: "reaping", vms: 1, shared: 1, fn: func(st *poolState) { st.note("i-0").Reaping = now }},
		{name: "fenced", vms: 1, shared: 1, fn: func(st *poolState) { st.sched().Fences["i-0"] = &fence{Owner: "other", At: now} }},
		{name: "uncertain slot", vms: 1, excl: 1, fn: func(st *poolState) {
			hold(st.sched(), "i-0", "a", now)
			for _, a := range st.sched().Attempts {
				a.State = attUncertain
			}
		}},
	} {
		p := readyFleet(t, c.vms, c.shared, c.excl, max(c.waited, time.Second))
		if c.fn != nil {
			p.loaded(t, c.fn)
		}
		if d, err := p.Plan(context.Background(), 0); err != nil || !d.Launch {
			t.Errorf("%s: %+v %v, want a launch", c.name, d, err)
		}
	}
}

// A VM still counted as starting capacity (its launch's grace) isn't
// counted a second time as free slots.
func TestStartingVMCountsOnce(t *testing.T) {
	p := readyFleet(t, 1, 5, 0, time.Second)
	p.loaded(t, func(st *poolState) {
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-0", Started: time.Now().Add(-time.Minute), PID: os.Getpid()}}
		st.note("i-0").ReadyAt = time.Now()
	})
	if d, _ := p.Plan(context.Background(), 0); !d.Launch || d.Ready != 0 || d.Capacity != 8 {
		t.Fatalf("starting VM: %+v, want its 4 slots once, and a launch for the fifth job", d)
	}
}

// Ready VMs cover what they can; only the rest waits for slots about to
// free (#226), which an exclusive part an empty VM covers no longer
// stops.
func TestReadyCoverThenHold(t *testing.T) {
	now := time.Now()
	soon := now.Add(5 * time.Second)
	p := readyFleet(t, 2, 2, 1, time.Second)
	p.loaded(t, func(st *poolState) {
		for _, wt := range []string{"a", "b", "c", "d"} {
			holdOn(st.sched(), "i-1", wt, now, soon)
		}
	})
	d, _ := p.Plan(context.Background(), 0)
	if d.Launch || !d.Held || d.Ready != 4 || d.Deferred != 2 {
		t.Fatalf("%+v, want the exclusive part on i-0 and the shared ones held for i-1's slots", d)
	}
}

// The acceptance case through Acquire: an exclusive part at the head of
// the queue is still probing the first idle VM when three shared parts
// queue behind it (they don't probe past it). They launch nothing, and
// all four parts run on the two VMs.
func TestMultipartRunOnIdleFleetLaunchesNothing(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"h1", "h2"}}, 4)
	pool[0].MaxSlots = 4
	f := fleets["healthy"]
	p := NewPool(pool[0], f)
	p.loaded(t, func(st *poolState) {
		allReady(st, time.Now())
		p.views(st, time.Now())
	})
	probing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	scriptHelpers(t, func(req AcquireRequest, _ string, _ int) (*Lease, error) {
		if req.Base.Exclusive {
			once.Do(func() { close(probing) })
			<-release
		}
		return &Lease{}, nil
	})
	type result struct {
		l   *Lease
		err error
	}
	results := make(chan result, 4)
	acquire := func(exclusive bool, rep *roundRep) {
		req := AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: rep}
		req.Base.Exclusive = exclusive
		l, err := Acquire(context.Background(), req)
		results <- result{l, err}
	}
	lint := &roundRep{rounds: make(chan struct{}, 64)}
	go acquire(true, lint)
	<-probing
	reps := []*roundRep{}
	for range 3 {
		rep := &roundRep{rounds: make(chan struct{}, 64)}
		reps = append(reps, rep)
		go acquire(false, rep)
	}
	// Each shared part has planned a few queued rounds behind it.
	for _, rep := range reps {
		for range 2 {
			<-rep.rounds
		}
	}
	close(release)
	vms := map[string]int{}
	for range 4 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		vms[r.l.InstanceID]++
		defer r.l.Close()
	}
	WaitLaunches(5 * time.Second)
	for _, rep := range append(reps, lint) {
		for i, n := range rep.names {
			if n == "scale_out" {
				t.Fatalf("launched while ready VMs had room: %v", rep.events[i])
			}
		}
	}
	if f.n != 0 || len(vms) != 2 {
		t.Fatalf("%d launched, parts on %v, want none launched and the parts on both VMs", f.n, vms)
	}
}
