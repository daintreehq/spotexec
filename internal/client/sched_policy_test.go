package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// loaded runs fn on the pool's state under the lock (saving it).
func (p *Pool) loaded(t *testing.T, fn func(st *poolState)) {
	t.Helper()
	if err := p.with(func(st *poolState) error {
		if len(st.Instances) == 0 {
			if err := p.refresh(context.Background(), st, true); err != nil {
				return err
			}
		}
		fn(st)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// allReady marks every VM as having answered (only those count as
// capacity the controller can shrink to).
func allReady(st *poolState, now time.Time) {
	for _, in := range st.Instances {
		st.note(in.ID).markReady(now)
	}
}

func retainedIDs(st *poolState) []string {
	var out []string
	for _, in := range st.Instances {
		if v := st.sched().VMs[in.ID]; v != nil && v.Role == roleRetained {
			out = append(out, in.ID)
		}
	}
	return out
}

// hold makes an acquired attempt on vm for worktree, owned by this process.
func hold(s *schedState, vm, worktree string, now time.Time) {
	s.Attempts[newAttemptID()] = &claimRec{Owner: processOwner(), PID: os.Getpid(), VM: vm, State: attAcquired, Units: 1, Worktree: worktree, At: now, GotAt: now}
	s.VMs[vm].remember(worktree, "repo", now)
	s.VMs[vm].LastStart = now
}

// The main defect: after a burst to twelve two-slot VMs, six worktrees
// keep working, each on its own VM. Once demand has stayed at six slots,
// the retained set shrinks one VM per epoch to the three VMs those six
// jobs fit on, never below, and the rest stop being placed on.
func TestControllerConvergesAfterBurst(t *testing.T) {
	p, _ := schedPool(t, 12, 2)
	start := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		allReady(st, start)
		p.views(st, start)
		for i, in := range st.Instances {
			s.VMs[in.ID].Hold = time.Time{}
			if i < 6 {
				hold(s, in.ID, "wt"+in.ID, start)
			}
		}
		s.Demand = []demandPoint{{start.Add(-2 * time.Minute), 6}}
		for sec := 0; sec <= 300; sec += 10 {
			p.control(st, start.Add(time.Duration(sec)*time.Second))
			if n := len(retainedIDs(st)); n < 3 {
				t.Fatalf("at %ds: %d retained VMs, below the 3 that six jobs need", sec, n)
			}
		}
		if got := retainedIDs(st); len(got) != 3 {
			t.Fatalf("retained %v, want 3 VMs", got)
		}
		// The empty VMs went first: every survivor is one of the busy six.
		for _, id := range retainedIDs(st) {
			if f, _ := s.held(p.R, id); f == 0 {
				t.Fatalf("an idle VM (%s) survived while busy ones went", id)
			}
		}
	})
}

// No shrink before the surplus has lasted sustainSurplus, and at most one
// VM per policy epoch.
func TestControllerHysteresis(t *testing.T) {
	p, _ := schedPool(t, 4, 2)
	start := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		allReady(st, start)
		p.views(st, start)
		for _, v := range s.VMs {
			v.Hold = time.Time{}
		}
		p.control(st, start)
		p.control(st, start.Add(50*time.Second))
		if n := len(retainedIDs(st)); n != 4 {
			t.Fatalf("%d retained after 50s of surplus, want all 4", n)
		}
		p.control(st, start.Add(61*time.Second))
		if n := len(retainedIDs(st)); n != 3 {
			t.Fatalf("%d retained after 61s, want 3 (one let go)", n)
		}
		p.control(st, start.Add(65*time.Second))
		if n := len(retainedIDs(st)); n != 3 {
			t.Fatalf("%d retained 4s later, want 3 (one per epoch)", n)
		}
	})
}

// The warm floor (min_instances) stays retained, and is what WarmSet
// reports for the pool manager.
func TestControllerKeepsWarmFloor(t *testing.T) {
	p, _ := schedPool(t, 5, 2)
	p.R.MinInstances = 2
	start := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		allReady(st, start)
		p.views(st, start)
		for _, v := range s.VMs {
			v.Hold = time.Time{}
		}
		for sec := 0; sec <= 200; sec += 10 {
			p.control(st, start.Add(time.Duration(sec)*time.Second))
		}
		if got := retainedIDs(st); len(got) != 2 {
			t.Fatalf("retained %v, want the 2 warm VMs", got)
		}
		if len(s.Warm) != 2 || s.VMs[s.Warm[0]].Role != roleRetained || s.VMs[s.Warm[1]].Role != roleRetained {
			t.Fatalf("warm %v", s.Warm)
		}
	})
	warm, roles, err := p.WarmSet(context.Background())
	if err != nil || len(warm) != 2 || roles[warm[0]] != roleRetained {
		t.Fatalf("WarmSet: %v %v %v", warm, roles, err)
	}
}

// A worktree whose last VM went standby is placed on a retained VM with
// room: its previous VM comes last, as a promotion, only if the retained
// ones are full.
func TestRankPrefersRetainedOverWarmStandby(t *testing.T) {
	p, f := schedPool(t, 3, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now // the controller already ran this epoch
		s.VMs["i-0"].remember("mine", "repo", now)
		s.VMs["i-0"].Role = roleStandby
		hold(s, "i-1", "other", now)
	})
	cands, _, _, err := p.Rank(context.Background(), nil, claim{Worktree: "mine", Repo: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	if len(cands) != 3 || cands[0].ID != "i-1" || cands[2].ID != "i-0" || !cands[2].claim.Promote || cands[0].claim.Promote {
		t.Fatalf("order %v (%+v), want i-1 (busy, same repo) first and the standby i-0 last, as a promotion", ids, cands)
	}
	// Without Promote, the standby VM can't be reserved; nor with it while
	// a retained VM still has room (the ranking may be stale).
	if _, err := p.reserveOn(f.insts[0], claim{}); err != errNoRoom {
		t.Fatalf("reserving a standby VM without promotion: %v", err)
	}
	if _, err := p.reserveOn(f.insts[0], cands[2].claim); err != errNoRoom {
		t.Fatalf("promoting while retained VMs have room: %v", err)
	}
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		hold(s, "i-1", "x", now)
		hold(s, "i-2", "y", now)
		hold(s, "i-2", "z", now)
	})
	before := time.Now()
	if _, err := p.reserveOn(f.insts[0], cands[2].claim); err != nil {
		t.Fatalf("promotion with the retained VMs full: %v", err)
	}
	if r := p.state(t).VMs["i-0"]; r.Role != roleRetained || r.Hold.Before(before.Add(minResidence-time.Second)) {
		t.Fatalf("after promotion: %+v (want retained, held for a minute)", r)
	}
}

// Latency mode lets a worktree return to its standby VM before cold
// retained ones.
func TestRankLatencyMode(t *testing.T) {
	p, _ := schedPool(t, 2, 2)
	p.R.Scheduling.Mode = config.ModeLatency
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		s.VMs["i-0"].remember("mine", "repo", now)
		s.VMs["i-0"].Role = roleStandby
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{Worktree: "mine"})
	if len(cands) != 2 || cands[0].ID != "i-0" || !cands[0].claim.Promote {
		t.Fatalf("latency mode: %+v, want the worktree's standby VM first", cands)
	}
	// A retained VM with its files too: no promotion needed first.
	p.loaded(t, func(st *poolState) { st.sched().VMs["i-1"].remember("mine", "repo", now) })
	cands, _, _, _ = p.Rank(context.Background(), nil, claim{Worktree: "mine"})
	if len(cands) != 2 || cands[0].ID != "i-1" || cands[0].claim.Promote {
		t.Fatalf("latency mode with a warm retained VM: %+v, want i-1 first", cands)
	}
}

// Standby VMs come back most recently retired first.
func TestStandbyPromotionOrder(t *testing.T) {
	p, _ := schedPool(t, 3, 1)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		for i, id := range []string{"i-1", "i-0", "i-2"} {
			s.VMs[id].Role, s.VMs[id].Since = roleStandby, now.Add(-time.Duration(i)*time.Minute)
		}
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{})
	if len(cands) != 3 || cands[0].ID != "i-1" || cands[1].ID != "i-0" || cands[2].ID != "i-2" || !cands[0].claim.Promote {
		t.Fatalf("promotion order %+v, want i-1, i-0, i-2 (most recently retired first), as promotions", cands)
	}
}

// An exclusive job's fence keeps other processes' shared jobs off its VM,
// and the controller doesn't shrink while it waits.
func TestExclusiveFence(t *testing.T) {
	p, _ := schedPool(t, 3, 2)
	now := time.Now()
	// Another live process: an owner file someone holds.
	other := "1-other"
	_ = os.MkdirAll(ownersDir(), 0o700)
	fh, err := os.OpenFile(filepath.Join(ownersDir(), other), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.Fences["i-0"] = &fence{Owner: other, At: now}
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{})
	for _, c := range cands {
		if c.ID == "i-0" {
			t.Fatalf("candidates %+v include the fenced i-0", cands)
		}
	}
	if _, err := p.reserveOn(cands[0].Instance, claim{}); err != nil {
		t.Fatal(err)
	}
	fencedVM := cands[0].Instance
	fencedVM.ID = "i-0"
	if _, err := p.reserveOn(fencedVM, claim{}); err != errNoRoom {
		t.Errorf("reserving the fenced VM: %v", err)
	}
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		for _, v := range s.VMs {
			v.Hold = time.Time{}
		}
		allReady(st, now)
		for sec := 0; sec <= 120; sec += 10 {
			p.control(st, now.Add(time.Duration(sec)*time.Second))
		}
		if n := len(retainedIDs(st)); n != 3 {
			t.Fatalf("%d retained while an exclusive job waits, want all 3", n)
		}
	})
	// This process's own fence: its VM comes first for it.
	p2, _ := schedPool(t, 2, 2)
	p2.loaded(t, func(st *poolState) {
		s := st.sched()
		p2.views(st, now)
		s.VMs["i-0"].LastStart = now // fresher: Fence picks i-1 (least recent), the fixed order prefers i-0
	})
	id := p2.Fence(context.Background(), nil)
	if id != "i-1" || p2.state(t).Fences["i-1"] == nil || p2.state(t).Fences["i-1"].Owner != processOwner() {
		t.Fatalf("fence on %q, stored %v", id, p2.state(t).Fences)
	}
	if cands, _, _, _ := p2.Rank(context.Background(), nil, claim{Exclusive: true}); len(cands) == 0 || cands[0].ID != id {
		t.Fatalf("own fenced VM %s not first: %+v", id, cands)
	}
	// Its VM turns standby: the next call fences another.
	p2.loaded(t, func(st *poolState) { st.sched().VMs["i-1"].Role = roleStandby })
	if id := p2.Fence(context.Background(), nil); id != "i-0" || p2.state(t).Fences["i-1"] != nil {
		t.Fatalf("fence after its VM went standby: %q, %v", id, p2.state(t).Fences)
	}
	p2.Unfence()
	if s := p2.state(t); len(s.Fences) != 0 {
		t.Fatalf("fences after Unfence: %v", s.Fences)
	}
}

// A dead process's fence is dropped.
func TestFenceOfDeadProcessDropped(t *testing.T) {
	p, _ := schedPool(t, 1, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		st.sched().Fences["i-0"] = &fence{Owner: "1-gone", At: now}
	})
	if cands, _, _, _ := p.Rank(context.Background(), nil, claim{}); len(cands) != 1 {
		t.Fatalf("candidates %+v: a dead process's fence still holds the VM", cands)
	}
	if f := p.state(t).Fences; len(f) != 0 {
		t.Fatalf("a dead process's fence was kept: %v", f)
	}
}

// An early drain is committed locally before the VM is asked, only for a
// settled, empty standby VM the retained ones can do without, and the VM
// is never placed on again, whatever the VM answers.
func TestEarlyDrain(t *testing.T) {
	p, f := schedPool(t, 3, 2)
	now := time.Now()
	setup := func(fn func(s *schedState)) {
		p.loaded(t, func(st *poolState) {
			s := st.sched()
			p.views(st, now)
			s.EpochAt = now
			fn(s)
		})
	}
	setup(func(s *schedState) {
		s.VMs["i-0"].Role, s.VMs["i-0"].Since = roleStandby, now.Add(-time.Minute)
		s.VMs["i-1"].Role, s.VMs["i-1"].Since = roleStandby, now // just retired
	})
	ctx := context.Background()
	if _, ok, _ := p.BeginDrain(ctx, "i-1"); ok {
		t.Fatal("drained a VM retired a moment ago")
	}
	setup(func(s *schedState) { s.VMs["i-2"].Since = now.Add(-time.Hour) })
	if _, ok, _ := p.BeginDrain(ctx, "i-2"); ok {
		t.Fatal("drained a retained VM")
	}
	off := false
	p.R.Scheduling.EarlyDrain = &off
	if _, ok, _ := p.BeginDrain(ctx, "i-0"); ok {
		t.Fatal("drained with early_drain off")
	}
	p.R.Scheduling.EarlyDrain = nil // the default: on

	// Something of this machine's on it: not drained.
	id, err := p.reserveOn(f.insts[0], claim{Exact: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.BeginDrain(ctx, "i-0"); ok {
		t.Fatal("drained a VM an exec is reaching")
	}
	p.releaseAttempt(id)

	// Demand the retained VM alone can't cover: not drained.
	setup(func(s *schedState) { s.Demand = []demandPoint{{now.Add(-time.Minute), 3}} })
	if _, ok, _ := p.BeginDrain(ctx, "i-0"); ok {
		t.Fatal("drained a VM recent demand still needs")
	}
	setup(func(s *schedState) { s.Demand = nil })

	op, ok, err := p.BeginDrain(ctx, "i-0")
	if err != nil || !ok || op == "" {
		t.Fatalf("BeginDrain: %q %v %v", op, ok, err)
	}
	if v := p.state(t).VMs["i-0"]; v.Role != roleDrainRequested || v.DrainOp != op {
		t.Fatalf("after BeginDrain: %+v", v)
	}
	// Committed: never placed on again, even with the retained VMs full.
	setup(func(s *schedState) { hold(s, "i-2", "a", now); hold(s, "i-2", "b", now) })
	cands, _, _, _ := p.Rank(ctx, nil, claim{})
	for _, c := range cands {
		if c.ID == "i-0" {
			t.Fatalf("a drain-requested VM is a candidate: %+v", cands)
		}
	}
	if _, err := p.reserveOn(f.insts[0], claim{Promote: true}); err != errNoRoom {
		t.Fatalf("reserving a drain-requested VM: %v", err)
	}
	// The VM was busy: asked again only drainRetry after that answer, with
	// the same operation.
	setup(func(s *schedState) { s.VMs["i-0"].DrainAt = now.Add(-time.Hour) })
	p.FinishDrain("i-0", op, "busy", "1 session(s)")
	if again, ok, _ := p.BeginDrain(ctx, "i-0"); ok || again != op {
		t.Fatalf("retry too soon after a busy answer: %q %v", again, ok)
	}
	setup(func(s *schedState) { s.VMs["i-0"].DrainAt = now.Add(-drainRetry - time.Second) })
	if again, ok, _ := p.BeginDrain(ctx, "i-0"); !ok || again != op {
		t.Fatalf("retry after drainRetry: %q %v, want the same op", again, ok)
	}
	// Not capacity while it waits: launches may replace it.
	var closed bool
	p.loaded(t, func(st *poolState) { closed = st.Notes["i-0"].closed() })
	if !closed {
		t.Fatal("a drain-requested VM still counts as capacity")
	}
	if v := p.state(t).VMs["i-0"]; v.Role != roleDrainRequested {
		t.Fatalf("a busy answer reopened the VM: %+v", v)
	}
	p.FinishDrain("i-0", "another-op", "drained", "")
	if v := p.state(t).VMs["i-0"]; v.Role != roleDrainRequested {
		t.Fatal("another operation's answer was applied")
	}
	p.FinishDrain("i-0", op, "drained", "")
	var draining bool
	p.loaded(t, func(st *poolState) { draining = st.Notes["i-0"].Draining })
	if v := p.state(t).VMs["i-0"]; v.Role != roleDraining || !draining {
		t.Fatalf("after drained: %+v, note closed %v", v, draining)
	}
}

// End to end: with the retained VM full, a job is placed on a standby VM
// (promoted) before anything launches.
func TestAcquirePromotesStandbyBeforeLaunch(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		s.VMs["i-b2"].Role, s.VMs["i-b2"].Since = roleStandby, now.Add(-time.Minute)
		hold(s, "i-b1", "a", now)
		hold(s, "i-b1", "b", now)
	})
	h := scriptHelpers(t, func(AcquireRequest, string, int) (*Lease, error) { return &Lease{}, nil })
	rep := &placeRep{}
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: rep, NoBoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.InstanceID != "i-b2" || h.count("b1") != 0 {
		t.Fatalf("placed on %s (b1 probed %d times), want the standby i-b2 without contacting the full b1", l.InstanceID, h.count("b1"))
	}
	if v := p.state(t).VMs["i-b2"]; v.Role != roleRetained {
		t.Fatalf("i-b2 after placement: %+v", v)
	}
	if !rep.saw("placed", "promoted") {
		t.Fatalf("no promoted placement event: %v", rep.events)
	}
}

type placeRep struct {
	nopRep
	events []map[string]any
	names  []string
}

func (r *placeRep) Event(name string, f map[string]any) {
	r.names = append(r.names, name)
	r.events = append(r.events, f)
}

func (r *placeRep) saw(name, field string) bool {
	for i, n := range r.names {
		if n == name && r.events[i][field] != nil {
			return true
		}
	}
	return false
}

// A VM launched moments ago counts as launch capacity until it is seen
// serving at capacity. The ledger skips probing a VM its own jobs fill, so
// Rank must record that it is full, or Plan keeps counting its slots as
// still to come and launches too few VMs for waiting jobs.
func TestRankMarksLedgerFullVMSeenFull(t *testing.T) {
	p, _ := schedPool(t, 1, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-0", Started: now.Add(-10 * time.Second), PID: os.Getpid()}}
		st.note("i-0").markReady(now.Add(-5 * time.Second)) // within readyGrace
		hold(s, "i-0", "a", now)
		hold(s, "i-0", "b", now)
	})
	w, err := p.Register(4) // four jobs waiting
	if err != nil {
		t.Fatal(err)
	}
	defer w.Done()
	d, err := p.Plan(context.Background(), 0)
	if err != nil || d.Capacity != 4 || !d.Launch {
		t.Fatalf("before Rank: %+v %v, want the young VM's 2 pending slots plus this launch's 2", d, err)
	}
	if cands, _, _, _ := p.Rank(context.Background(), nil, claim{}); len(cands) != 0 {
		t.Fatalf("candidates %+v on a full VM", cands)
	}
	d, err = p.Plan(context.Background(), 0)
	// Pending now: only the launch planned above (2 slots), so 4 waiting
	// jobs call for another. Counting the full VM too (4 slots) would not.
	if err != nil || !d.Launch || d.Capacity != 4 {
		t.Fatalf("after Rank: %+v %v, want a second launch with the full VM no longer pending", d, err)
	}
}

// #297: a VM this machine launched that never got a job (and so never
// answered a probe) goes standby on the first epoch after its minimum
// residence once the busy VM covers the slots held now, even while recent
// demand (p90) was higher. Nothing else changes role.
func TestUnusedLaunchGoesStandby(t *testing.T) {
	setup := func(t *testing.T, fn func(st *poolState, now time.Time)) (*Pool, time.Time) {
		t.Helper()
		p, _ := schedPool(t, 2, 6)
		start := time.Now()
		p.loaded(t, func(st *poolState) {
			s := st.sched()
			st.note("i-0").markReady(start) // i-1 never answered
			p.views(st, start)
			for _, v := range s.VMs {
				v.Hold = time.Time{}
			}
			for i := range 4 {
				hold(s, "i-0", "wt"+string(rune('a'+i)), start)
			}
			st.Launches = []launchRec{{ID: "l1", InstanceID: "i-1", Started: start.Add(-time.Minute), PID: os.Getpid(), Demand: true}}
			s.Demand = []demandPoint{{start.Add(-time.Minute), 8}, {start.Add(-time.Second), 4}}
			if fn != nil {
				fn(st, start)
			}
		})
		return p, start
	}
	roles := func(p *Pool, t *testing.T) (string, string) {
		t.Helper()
		s := p.state(t)
		return s.VMs["i-0"].Role, s.VMs["i-1"].Role
	}

	p, start := setup(t, nil)
	p.loaded(t, func(st *poolState) { p.control(st, start) })
	if r0, r1 := roles(p, t); r0 != roleRetained || r1 != roleStandby {
		t.Fatalf("roles %s/%s, want the unused launch on standby at once", r0, r1)
	}
	if v := p.state(t).VMs["i-1"]; !strings.Contains(v.Reason, "unused") {
		t.Fatalf("reason %q", v.Reason)
	}

	// The launch record may expire (boot_timeout) before any controller
	// pass: the listing that prunes it keeps, on the VM's own record, that
	// this machine launched it for demand.
	p, start = setup(t, func(st *poolState, now time.Time) {
		st.Launches[0].Started = now.Add(-p.R.BootTimeout.Duration - 2*time.Minute)
		delete(st.sched().VMs, "i-1")
	})
	if err := p.with(func(st *poolState) error {
		if err := p.refresh(context.Background(), st, true); err != nil {
			return err
		}
		if len(st.Launches) != 0 {
			t.Fatalf("launch record kept: %+v", st.Launches)
		}
		if v := st.sched().VMs["i-1"]; v != nil {
			v.Hold = time.Time{}
		}
		p.control(st, start)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, r1 := roles(p, t); r1 != roleStandby {
		t.Fatalf("after its launch record expired: i-1 %s, want standby", r1)
	}

	for _, c := range []struct {
		name string
		fn   func(st *poolState, now time.Time)
	}{
		{"spotexec up", func(st *poolState, now time.Time) { st.Launches[0].Demand = false }},
		{"partial listing", func(st *poolState, now time.Time) { st.Unlisted = []string{"eu-north-1"} }},
		{"residence", func(st *poolState, now time.Time) { st.sched().VMs["i-1"].Hold = now.Add(time.Minute) }},
		{"another machine's", func(st *poolState, now time.Time) { st.Launches = nil }},
		{"used", func(st *poolState, now time.Time) { st.sched().VMs["i-1"].LastEnd = now.Add(-time.Minute) }},
		{"used by probe", func(st *poolState, now time.Time) { st.note("i-1").LastUsed = now.Add(-time.Minute) }},
		{"attempt", func(st *poolState, now time.Time) {
			hold(st.sched(), "i-1", "wtz", now)
			st.sched().VMs["i-1"].LastStart = time.Time{}
		}},
		{"demand now", func(st *poolState, now time.Time) {
			hold(st.sched(), "i-0", "wte", now)
			hold(st.sched(), "i-0", "wtf", now)
			hold(st.sched(), "i-0", "wtg", now) // seven slots held: more than i-0 has
		}},
		{"demand now, it answering", func(st *poolState, now time.Time) {
			st.note("i-1").markReady(now)
			hold(st.sched(), "i-0", "wte", now)
			hold(st.sched(), "i-0", "wtf", now)
			hold(st.sched(), "i-0", "wtg", now) // its own slots don't count
		}},
		{"spare slots", func(st *poolState, now time.Time) {}},
		{"warm", func(st *poolState, now time.Time) {}},
		{"replacement not answering", func(st *poolState, now time.Time) { st.note("i-0").Ready = false }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, start := setup(t, c.fn)
			switch c.name {
			case "spare slots":
				p.R.Scheduling.SpareSlots = 3 // 4 held + 3 > 6
			case "warm":
				p.R.MinInstances = 2 // both kept warm
			}
			p.loaded(t, func(st *poolState) { p.control(st, start) })
			if _, r1 := roles(p, t); r1 != roleRetained {
				t.Fatalf("%s: i-1 %s, want retained", c.name, r1)
			}
		})
	}

	// A job waiting for a slot keeps it too.
	p, start = setup(t, nil)
	w, err := p.Register(1)
	if err != nil {
		t.Fatal(err)
	}
	p.loaded(t, func(st *poolState) { p.control(st, start) })
	w.Done()
	if _, r1 := roles(p, t); r1 != roleRetained {
		t.Fatalf("with a waiter: i-1 %s, want retained", r1)
	}

}

// keepDemand marks a demand launch's VM, but starts no record (nor its
// minimum residence) for a VM not yet serving: views does that once it is.
func TestKeepDemandWaitsForServingVMs(t *testing.T) {
	st := &poolState{Launches: []launchRec{
		{ID: "a", InstanceID: "i-pending", Demand: true},
		{ID: "b", InstanceID: "i-up", Demand: true},
		{ID: "c", InstanceID: "i-asked", Demand: false},
	}}
	st.keepDemand(map[string]bool{"i-pending": false, "i-up": true, "i-asked": true}, time.Now())
	s := st.sched()
	if s.VMs["i-pending"] != nil || s.VMs["i-asked"] != nil || s.VMs["i-up"] == nil || !s.VMs["i-up"].Demand {
		t.Fatalf("records %+v", s.VMs)
	}
}
