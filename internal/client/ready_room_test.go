package client

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/ui"
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

// A slot an uncertain attempt (a dead process's, not yet reconciled) may
// hold isn't free for shared demand either.
func TestUncertainSlotIsNotFree(t *testing.T) {
	p := readyFleet(t, 1, 4, 0, time.Second)
	p.loaded(t, func(st *poolState) {
		hold(st.sched(), "i-0", "a", time.Now())
		for _, a := range st.sched().Attempts {
			a.State = attUncertain
		}
	})
	if d, _ := p.Plan(context.Background(), 0); !d.Launch || d.Ready != 3 {
		t.Fatalf("%+v, want 3 slots covered and a launch for the fourth job", d)
	}
}

// An exclusive part takes the smallest empty VM, leaving the larger one's
// slots to the shared parts.
func TestExclusiveTakesSmallestEmptyVM(t *testing.T) {
	p := readyFleet(t, 2, 4, 1, time.Second)
	p.loaded(t, func(st *poolState) { st.sched().VMs["i-1"].Slots = 2 })
	if d, _ := p.Plan(context.Background(), 0); d.Launch || d.Ready != 8 {
		t.Fatalf("%+v, want the exclusive part on the two-slot VM and the shared ones on the four-slot VM", d)
	}
}

// Ready credit covers only demand nothing else does: not what this
// fleet's launches cover, nor what higher-priority fleets do, and those
// cover shared demand before an exclusive part's.
func TestReadyCreditCoversOnlyResidualDemand(t *testing.T) {
	ctx := context.Background()
	// Five shared parts: a launch already covers four, so the empty VM
	// covers only the fifth.
	p := readyFleet(t, 2, 5, 0, time.Second)
	p.loaded(t, func(st *poolState) {
		st.Launches = []launchRec{{ID: "l1", Started: time.Now(), PID: os.Getpid()}}
		st.note("i-1").Draining = true
	})
	if d, _ := p.Plan(ctx, 0); d.Launch || d.Ready != 1 || d.Capacity != 4 {
		t.Fatalf("launch pending: %+v, want 1 slot covered by the ready VM", d)
	}

	// An exclusive part and four shared ones; a higher-priority fleet
	// covers four slots, and this one has four free slots but no empty
	// VM. What is covered upstream is the shared parts: the exclusive one
	// still needs a VM.
	p = readyFleet(t, 2, 4, 1, time.Second)
	p.loaded(t, func(st *poolState) {
		for _, vm := range []string{"i-0", "i-1"} {
			hold(st.sched(), vm, "a"+vm, time.Now())
			hold(st.sched(), vm, "b"+vm, time.Now())
		}
	})
	if d, _ := p.Plan(ctx, 4); !d.Launch || d.Ready != 0 {
		t.Fatalf("covered upstream: %+v, want a launch for the exclusive part", d)
	}
}

// parkRep records events, signals each queue round (its status line), and
// while parked holds the job there, before its next round.
type parkRep struct {
	roundRep
	resume <-chan struct{}
}

func (r *parkRep) Status(f string, a ...any) {
	r.roundRep.Status(f, a...)
	<-r.resume
}

// multipartRun starts an exclusive part, and while it is still probing
// the first VM, three shared parts queued behind it (they don't probe
// past it). Once each shared part has planned a round, the exclusive part
// gets its VM, and then the shared parts go on. It returns the VMs the
// parts ran on and the events of every part.
func multipartRun(t *testing.T, pool []*config.Runner, fleets map[string]*fakeFleet) (map[string]int, []*placeRep) {
	t.Helper()
	for _, r := range pool {
		r.MaxSlots = 4
		p := NewPool(r, fleets[r.Name])
		p.loaded(t, func(st *poolState) {
			allReady(st, time.Now())
			p.views(st, time.Now())
		})
	}
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
	acquire := func(exclusive bool, rep ui.Reporter, out chan<- result) {
		req := AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: rep}
		req.Base.Exclusive = exclusive
		l, err := Acquire(context.Background(), req)
		out <- result{l, err}
	}
	var leases []*Lease
	t.Cleanup(func() {
		for _, l := range leases {
			l.Close()
		}
	})
	got := map[string]int{}
	take := func(r result) {
		t.Helper()
		if r.err != nil {
			t.Fatal(r.err)
		}
		leases = append(leases, r.l)
		got[r.l.InstanceID]++
	}
	lint, lintDone := &roundRep{rounds: make(chan struct{}, 64)}, make(chan result, 1)
	go acquire(true, lint, lintDone)
	<-probing
	resume, shared := make(chan struct{}), make(chan result, 3)
	var reps []*parkRep
	for range 3 {
		rep := &parkRep{roundRep: roundRep{rounds: make(chan struct{}, 64)}, resume: resume}
		reps = append(reps, rep)
		go acquire(false, rep, shared)
	}
	for _, rep := range reps {
		<-rep.rounds // planned a round behind the exclusive part, and parked
	}
	close(release)
	take(<-lintDone)
	close(resume)
	for range 3 {
		take(<-shared)
	}
	WaitLaunches(5 * time.Second)
	all := []*placeRep{&lint.placeRep}
	for _, rep := range reps {
		all = append(all, &rep.placeRep)
	}
	return got, all
}

func launched(t *testing.T, reps []*placeRep, fleets map[string]*fakeFleet) {
	t.Helper()
	for _, rep := range reps {
		for i, n := range rep.names {
			if n == "scale_out" {
				t.Fatalf("launched while ready VMs had room: %v", rep.events[i])
			}
		}
	}
	for name, f := range fleets {
		f.mu.Lock()
		n := f.n
		f.mu.Unlock()
		if n != 0 {
			t.Fatalf("%s launched %d VMs, want none", name, n)
		}
	}
}

// The acceptance case through Acquire: a multi-part run on an idle fleet
// with room for it launches nothing, and its parts run on both VMs.
func TestMultipartRunOnIdleFleetLaunchesNothing(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"h1", "h2"}}, 4)
	vms, reps := multipartRun(t, pool, fleets)
	launched(t, reps, fleets)
	if len(vms) != 2 {
		t.Fatalf("parts on %v, want both VMs", vms)
	}
}

// Demand the preferred fleet's ready VMs cover is covered for the next
// fleet too: that one, full but free to grow, launches nothing for it.
func TestReadyCoverReachesLowerPriorityFleets(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"broken": {"b1", "b2"}, "healthy": {"h1"}}, 4)
	full := NewPool(pool[1], fleets["healthy"])
	full.loaded(t, func(st *poolState) {
		allReady(st, time.Now())
		full.views(st, time.Now())
		for _, wt := range []string{"a", "b", "c", "d"} {
			hold(st.sched(), "i-h1", wt, time.Now())
		}
	})
	_, reps := multipartRun(t, pool, fleets)
	launched(t, reps, fleets)
}
