package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
)

// schedPool is a pool over n running VMs (i-0..), each with slots slots.
func schedPool(t *testing.T, n, slots int) (*Pool, *fakeFleet) {
	t.Helper()
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "fleet", Type: config.TypeAWSSpot, MaxSlots: slots}
	r.BootTimeout.Duration = 10 * time.Minute
	r.IdleTimeout.Duration = 10 * time.Minute
	f := &fakeFleet{r: r}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		f.insts = append(f.insts, provider.Instance{ID: fmt.Sprintf("i-%d", i), State: provider.StateRunning,
			LaunchedAt: base.Add(time.Duration(i) * time.Second), Target: provider.Target{Host: fmt.Sprintf("10.0.0.%d", i)}})
	}
	return NewPool(r, f), f
}

func (p *Pool) state(t *testing.T) *schedState {
	t.Helper()
	var out *schedState
	_ = p.with(func(st *poolState) error { out = st.sched(); return nil })
	return out
}

// A reservation counts as soon as it is made, and is gone once released;
// the slot count the helper reports replaces the planning figure.
func TestLedgerLifecycle(t *testing.T) {
	p, f := schedPool(t, 1, 2)
	in := f.insts[0]
	id, err := p.reserveOn(in, claim{Worktree: "wt", Repo: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	s0 := p.state(t)
	if a := s0.Attempts[id]; a == nil || a.State != attReserved {
		t.Fatalf("attempt %+v, want reserved", a)
	}
	if s0.demandNow(p.R) != 1 || len(s0.Demand) == 0 || s0.Demand[len(s0.Demand)-1].Units != 1 {
		t.Fatalf("a reservation must count at once: demand %d, history %v", s0.demandNow(p.R), s0.Demand)
	}
	if err := p.markSent(id); err != nil {
		t.Fatal(err)
	}
	if a := p.state(t).Attempts[id]; a.State != attSent {
		t.Fatalf("state %s, want sent", a.State)
	}
	p.commitAttempt(id, protocol.Acquired{MaxSlots: 3}, time.Time{})
	s := p.state(t)
	if a := s.Attempts[id]; a.State != attAcquired {
		t.Fatalf("state %s, want acquired", a.State)
	}
	v := s.VMs[in.ID]
	if v.Slots != 3 || !v.warmFor("wt", time.Now()) || !v.hasRepo("repo", time.Now()) {
		t.Fatalf("vm record %+v", v)
	}
	if v.warmFor("other", time.Now()) || v.hasRepo("other", time.Now()) || v.warmFor("wt", time.Now().Add(evidenceAge+time.Minute)) {
		t.Fatalf("evidence for a worktree never synced there, or expired: %+v", v)
	}
	if got := s.demandNow(p.R); got != 1 {
		t.Fatalf("demand %d, want 1", got)
	}
	// The helper's three slots, not the runner's two, bound reservations.
	for i := 0; i < 2; i++ {
		if _, err := p.reserveOn(in, claim{}); err != nil {
			t.Fatalf("reservation %d with 3 helper slots: %v", i+2, err)
		}
	}
	if _, err := p.reserveOn(in, claim{}); err != errNoRoom {
		t.Fatalf("a fourth reservation on 3 slots: %v", err)
	}
	p.releaseAttempt(id)
	s = p.state(t)
	if _, ok := s.Attempts[id]; ok || s.VMs[in.ID].LastEnd.IsZero() || s.demandNow(p.R) != 2 || s.Demand[len(s.Demand)-1].Units != 2 {
		t.Fatalf("after release: attempts %v, vm %+v, history %v", s.Attempts, s.VMs[in.ID], s.Demand)
	}
}

// Ordinary reservations stop at the VM's capacity; forced ones (legacy
// placement, exact slots) never do; an exclusive one needs the whole VM.
func TestLedgerCapacity(t *testing.T) {
	p, f := schedPool(t, 1, 2)
	in := f.insts[0]
	for i := 0; i < 2; i++ {
		if _, err := p.reserveOn(in, claim{}); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	if _, err := p.reserveOn(in, claim{}); err != errNoRoom {
		t.Fatalf("third reservation: %v, want errNoRoom", err)
	}
	forced, err := p.reserveOn(in, claim{Force: true})
	if err != nil {
		t.Fatalf("forced reservation: %v", err)
	}
	if s := p.state(t); s.Attempts[forced] == nil || s.demandNow(p.R) != 3 {
		t.Fatalf("forced reservation not recorded: %v", s.Attempts)
	}
	pe, fe := schedPool(t, 1, 2)
	ex, err := pe.reserveOn(fe.insts[0], claim{Exclusive: true})
	if err != nil {
		t.Fatalf("exclusive on an empty VM: %v", err)
	}
	if a := pe.state(t).Attempts[ex]; a.Units != 2 {
		t.Fatalf("exclusive attempt takes %d units, want the whole VM (2)", a.Units)
	}
	if _, err := pe.reserveOn(fe.insts[0], claim{}); err != errNoRoom {
		t.Fatalf("a shared job beside an exclusive one: %v", err)
	}
	p2, f2 := schedPool(t, 1, 2)
	if _, err := p2.reserveOn(f2.insts[0], claim{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p2.reserveOn(f2.insts[0], claim{Exclusive: true}); err != errNoRoom {
		t.Fatalf("exclusive on a part-busy VM: %v, want errNoRoom", err)
	}
}

// Twenty-four processes racing for twelve two-slot VMs fill them exactly:
// each sees the reservations the others made before it.
func TestLedgerConcurrentReservations(t *testing.T) {
	p, f := schedPool(t, 12, 2)
	var mu sync.Mutex
	per := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, in := range f.insts {
				if _, err := p.reserveOn(in, claim{}); err == nil {
					mu.Lock()
					per[in.ID]++
					mu.Unlock()
					return
				}
			}
			t.Error("no VM had room")
		}()
	}
	wg.Wait()
	for _, in := range f.insts {
		if per[in.ID] != 2 {
			t.Fatalf("%s got %d reservations, want 2 (%v)", in.ID, per[in.ID], per)
		}
	}
}

// A dead process's reservation that never went out is dropped; one that
// may have reached the VM stays counted (uncertain) until the TTL.
func TestLedgerDeadOwners(t *testing.T) {
	p, f := schedPool(t, 1, 3)
	now := time.Now()
	_ = p.with(func(st *poolState) error {
		s := st.sched()
		s.vm(f.insts[0].ID, now)
		for id, state := range map[string]string{"never-sent": attReserved, "sent": attSent, "held": attAcquired} {
			s.Attempts[id] = &claimRec{Owner: "1-dead", PID: 1 << 30, VM: f.insts[0].ID, State: state, Units: 1, At: now}
		}
		s.Attempts["old"] = &claimRec{Owner: "1-dead", VM: f.insts[0].ID, State: attUncertain, Units: 1, LostAt: now.Add(-uncertainTTL - time.Second)}
		s.Attempts["recent"] = &claimRec{Owner: "1-dead", VM: f.insts[0].ID, State: attUncertain, Units: 1, LostAt: now.Add(-uncertainTTL + time.Minute)}
		return nil
	})
	if _, err := p.reserveOn(f.insts[0], claim{}); err != nil {
		t.Fatal(err)
	}
	s := p.state(t)
	if _, ok := s.Attempts["never-sent"]; ok {
		t.Fatal("an unsent reservation of a dead process was kept")
	}
	if _, ok := s.Attempts["old"]; ok {
		t.Fatal("an uncertain attempt outlived its TTL")
	}
	for _, id := range []string{"sent", "held", "recent"} {
		if a := s.Attempts[id]; a == nil || a.State != attUncertain {
			t.Fatalf("%s: %+v, want uncertain", id, a)
		}
	}
	// Uncertain slots fill the VM (3 of 3 with ours), yet don't stop a
	// reservation: the helper is asked.
	if firm, unc := s.held(p.R, f.insts[0].ID); firm != 1 || unc != 3 {
		t.Fatalf("held firm %d uncertain %d, want 1 and 3", firm, unc)
	}
	mine, err := p.reserveOn(f.insts[0], claim{})
	if err != nil {
		t.Fatalf("uncertain slots blocked a reservation: %v", err)
	}
	// A holder list settles only attempts uncertain since well before the
	// probe: "sent" and "held" just became uncertain, so they stay...
	p.settleHolders(f.insts[0].ID, []protocol.Holder{{Client: protocol.ClientInfo{Attempt: "recent"}}}, time.Now())
	if s = p.state(t); s.Attempts["sent"] == nil || s.Attempts["held"] == nil {
		t.Fatal("a just-uncertain attempt was settled by a holder list")
	}
	// ...while "recent" (uncertain for minutes) goes unless held, and firm
	// attempts are never touched.
	p.settleHolders(f.insts[0].ID, []protocol.Holder{{Client: protocol.ClientInfo{Attempt: "recent"}}}, time.Now().Add(time.Minute))
	s = p.state(t)
	if s.Attempts["recent"] == nil {
		t.Fatal("an uncertain attempt the helper still holds was dropped")
	}
	if s.Attempts["sent"] != nil || s.Attempts["held"] != nil {
		t.Fatal("uncertain attempts the helper doesn't hold were kept")
	}
	if s.Attempts[mine] == nil {
		t.Fatal("settling holders dropped a live process's attempt")
	}
	if got := s.Demand[len(s.Demand)-1].Units; got != s.demandNow(p.R) {
		t.Fatalf("history ends at %d after settling, demand is %d", got, s.demandNow(p.R))
	}
}

// Demand's 90th percentile weighs time, not events: a short spike of many
// events doesn't raise it, a level held for most of the window does.
func TestDemandP90(t *testing.T) {
	now := time.Now()
	s := &schedState{}
	at := func(sec int) time.Time { return now.Add(time.Duration(sec-60) * time.Second) }
	s.Demand = []demandPoint{{at(-30), 2}, {at(10), 6}, {at(11), 2}, {at(12), 6}, {at(13), 2}}
	if got := s.demandP90(now); got != 2 {
		t.Fatalf("p90 %d, want 2 (spikes of 2s in 60s)", got)
	}
	s.Demand = []demandPoint{{at(0), 6}, {at(50), 1}}
	if got := s.demandP90(now); got != 6 {
		t.Fatalf("p90 %d, want 6 (held 50 of 60s)", got)
	}
	s.Demand = []demandPoint{{at(0), 6}, {at(1), 2}}
	if got := s.demandP90(now); got != 2 {
		t.Fatalf("p90 %d, want 2 (6 for 1s, then 2 for 59s)", got)
	}
	s.Demand = []demandPoint{{at(-100), 1}, {at(5), 6}}
	if got := s.demandP90(now); got != 6 {
		t.Fatalf("p90 %d, want 6 (1 for 5s of the window, then 6 for 55s)", got)
	}
	s.Demand = nil
	if got := s.demandP90(now); got != 0 {
		t.Fatalf("p90 %d with no history, want 0", got)
	}
}

// Acquire records the attempt and the lease releases it; a VM that says
// busy leaves nothing behind.
func TestAcquireUsesLedger(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}})
	ids := map[string]string{}
	h := scriptHelpers(t, func(req AcquireRequest, host string, _ int) (*Lease, error) {
		a := NewPool(pool[0], nil).state(t).Attempts[req.Base.Client.Attempt]
		if a == nil || a.State != attSent || a.VM != "i-"+host {
			t.Errorf("acquire on %s: its attempt %q is %+v, want sent on that VM", host, req.Base.Client.Attempt, a)
		}
		ids[host] = req.Base.Client.Attempt
		if host == "b2" {
			return &Lease{Acq: protocol.Acquired{MaxSlots: 2}}, nil
		}
		return nil, nil // busy
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{},
		Base: protocol.Acquire{RepoID: "r", WorktreeKey: "w"}})
	if err != nil {
		t.Fatal(err)
	}
	if h.count("b1")+h.count("b2") != 2 || ids["b1"] == ids["b2"] {
		t.Fatalf("probes b1 %d b2 %d, attempt IDs %v: want one each, distinct", h.count("b1"), h.count("b2"), ids)
	}
	p := NewPool(pool[0], nil)
	s := p.state(t)
	if len(s.Attempts) != 1 || s.Attempts[ids["b2"]] == nil || s.Attempts[ids["b2"]].State != attAcquired {
		t.Fatalf("attempts %v, want just b2's, acquired", s.Attempts)
	}
	l.Close()
	if s := p.state(t); len(s.Attempts) != 0 {
		t.Fatalf("attempts after Close: %v", s.Attempts)
	}
}

// A queue timeout this side declares after the helper went quiet is not a
// refusal (the helper may hold the slot); the helper's own is.
func TestRefusalClassification(t *testing.T) {
	helperSaid := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeQueueTimeout}}
	if !isRefusal(helperSaid) {
		t.Fatal("the helper's queue timeout must count as a refusal")
	}
	if isRefusal(&gaveUpError{helperSaid}) {
		t.Fatal("a locally declared timeout counted as a refusal")
	}
	if !isCode(&gaveUpError{helperSaid}, protocol.CodeQueueTimeout) {
		t.Fatal("a locally declared timeout lost its code")
	}
	if isRefusal(errors.New("connection reset")) {
		t.Fatal("a connection error counted as a refusal")
	}
}
