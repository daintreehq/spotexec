package client

import (
	"context"
	"os"
	"path/filepath"
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
		s.EpochAt = now.Add(time.Hour) // keep the controller out of it
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
	// Without Promote, the standby VM can't be reserved.
	if _, err := p.reserveOn(f.insts[0], claim{}); err != errNoRoom {
		t.Fatalf("reserving a standby VM without promotion: %v", err)
	}
	if _, err := p.reserveOn(f.insts[0], cands[2].claim); err != nil {
		t.Fatalf("promotion: %v", err)
	}
	if r := p.state(t).VMs["i-0"]; r.Role != roleRetained || r.Hold.IsZero() {
		t.Fatalf("after promotion: %+v", r)
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
		s.EpochAt = now.Add(time.Hour)
		s.VMs["i-0"].remember("mine", "repo", now)
		s.VMs["i-0"].Role = roleStandby
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{Worktree: "mine"})
	if len(cands) != 2 || cands[0].ID != "i-0" || !cands[0].claim.Promote {
		t.Fatalf("latency mode: %+v, want the worktree's standby VM first", cands)
	}
}

// Standby VMs come back most recently retired first.
func TestStandbyPromotionOrder(t *testing.T) {
	p, _ := schedPool(t, 3, 1)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now.Add(time.Hour)
		for i, id := range []string{"i-0", "i-1", "i-2"} {
			s.VMs[id].Role, s.VMs[id].Since = roleStandby, now.Add(time.Duration(i)*time.Minute)
		}
	})
	cands, _, _, _ := p.Rank(context.Background(), nil, claim{})
	if len(cands) != 3 || cands[0].ID != "i-2" || cands[2].ID != "i-0" {
		t.Fatalf("promotion order %+v, want i-2, i-1, i-0", cands)
	}
}

// An exclusive job's fence keeps other processes' shared jobs off its VM,
// and the controller doesn't shrink while it waits.
func TestExclusiveFence(t *testing.T) {
	p, _ := schedPool(t, 2, 2)
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
	if len(cands) != 1 || cands[0].ID != "i-1" {
		t.Fatalf("candidates %+v, want only the unfenced i-1", cands)
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
		for sec := 0; sec <= 120; sec += 10 {
			p.control(st, now.Add(time.Duration(sec)*time.Second))
		}
		if n := len(retainedIDs(st)); n != 2 {
			t.Fatalf("%d retained while an exclusive job waits, want 2", n)
		}
	})
	// This process's own fence: its VM comes first for it.
	p2, _ := schedPool(t, 2, 2)
	if id := p2.Fence(context.Background()); id == "" {
		t.Fatal("no fence placed")
	} else if cands, _, _, _ := p2.Rank(context.Background(), nil, claim{Exclusive: true}); len(cands) == 0 || cands[0].ID != id {
		t.Fatalf("own fenced VM %s not first: %+v", id, cands)
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
}

// An early drain is committed locally before the VM is asked, only for a
// settled, empty standby VM the retained ones can do without, and the VM
// is never placed on again, whatever the VM answers.
func TestEarlyDrain(t *testing.T) {
	p, f := schedPool(t, 3, 2)
	p.R.Scheduling.EarlyDrain = true
	now := time.Now()
	setup := func(fn func(s *schedState)) {
		p.loaded(t, func(st *poolState) {
			s := st.sched()
			p.views(st, now)
			s.EpochAt = now.Add(time.Hour)
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
	if _, ok, _ := p.BeginDrain(ctx, "i-2"); ok {
		t.Fatal("drained a retained VM")
	}
	p.R.Scheduling.EarlyDrain = false
	if _, ok, _ := p.BeginDrain(ctx, "i-0"); ok {
		t.Fatal("drained with early_drain off")
	}
	p.R.Scheduling.EarlyDrain = true

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
	// The VM was busy: asked again only after drainRetry, same operation.
	p.FinishDrain("i-0", op, "busy", "1 session(s)")
	if again, ok, _ := p.BeginDrain(ctx, "i-0"); ok || again != op {
		t.Fatalf("retry too soon: %q %v", again, ok)
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
	p.loaded(t, func(st *poolState) { draining = st.Notes["i-0"].closed() })
	if v := p.state(t).VMs["i-0"]; v.Role != roleDraining || !draining {
		t.Fatalf("after drained: %+v, note closed %v", v, draining)
	}
}
