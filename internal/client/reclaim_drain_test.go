package client

import (
	"context"
	"testing"
	"time"
)

// scheduledDrain marks vm drain_requested with its drain op scheduled on the
// VM (not carried out yet), as finishDrain leaves it.
func scheduledDrain(st *poolState, vm, op string, now time.Time) {
	v := st.sched().VMs[vm]
	v.Role, v.Since, v.Reason = roleDrainRequested, now.Add(-time.Minute), "early drain scheduled"
	v.DrainOp, v.DrainAt, v.DrainScheduled = op, now.Add(-time.Minute), true
	st.note(vm).Retiring = true
}

// #255: with the retained VMs full, a VM whose early drain is scheduled
// but not carried out yet is taken back (its drain cancelled by the
// acquire) before anything launches, and rejoins the retained set.
func TestAcquireReclaimsDrainingVMBeforeLaunch(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		allReady(st, now)
		scheduledDrain(st, "i-b2", "op1", now)
		hold(s, "i-b1", "a", now)
		hold(s, "i-b1", "b", now)
	})
	undrain := map[string]string{}
	h := scriptHelpers(t, func(req AcquireRequest, host string, _ int) (*Lease, error) {
		undrain[host] = req.Base.Undrain
		return &Lease{}, nil
	})
	rep := &placeRep{}
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: 3 * time.Second, Rep: rep, NoBoot: true})
	if err != nil {
		t.Fatalf("not placed on the VM still up: %v", err)
	}
	defer l.Close()
	if l.InstanceID != "i-b2" || h.count("b1") != 0 || undrain["b2"] != "op1" {
		t.Fatalf("placed on %s (b1 probed %d times), undrain %q: want i-b2, its drain op1 cancelled", l.InstanceID, h.count("b1"), undrain["b2"])
	}
	if v := p.state(t).VMs["i-b2"]; v.Role != roleRetained || v.DrainOp != "" || v.DrainScheduled {
		t.Fatalf("i-b2 after placement: %+v", v)
	}
	var retiring bool
	p.loaded(t, func(st *poolState) { retiring = st.Notes["i-b2"].Retiring })
	if retiring {
		t.Fatal("i-b2 still counts as retiring")
	}
	if !rep.saw("placed", "drain_cancelled") {
		t.Fatalf("no drain_cancelled placement event: %v", rep.events)
	}
}

// Only a drain the VM took over is cancelled, only by its own operation,
// only once no retained VM has room, and only after the standby VMs.
func TestReclaimCandidates(t *testing.T) {
	p, f := schedPool(t, 4, 2)
	now := time.Now()
	ctx := context.Background()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		allReady(st, now)
		scheduledDrain(st, "i-1", "op1", now)
		scheduledDrain(st, "i-2", "op2", now)
		s.VMs["i-2"].DrainScheduled = false // still being asked: a request may be on its way
		s.VMs["i-3"].Role, s.VMs["i-3"].Since = roleStandby, now.Add(-time.Minute)
	})
	// The retained VM has room: nothing is taken back.
	if _, err := p.reserveOn(f.insts[1], claim{Reclaim: "op1"}); err != errNoRoom {
		t.Fatalf("reclaimed with a retained VM free: %v", err)
	}
	p.loaded(t, func(st *poolState) { hold(st.sched(), "i-0", "a", now); hold(st.sched(), "i-0", "b", now) })
	cands, usable, window, err := p.Rank(ctx, nil, claim{})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range cands {
		order = append(order, c.ID)
		if c.ID == "i-1" && c.claim.Reclaim != "op1" {
			t.Fatalf("reclaim candidate without its op: %+v", c.claim)
		}
	}
	if len(order) != 2 || order[0] != "i-3" || order[1] != "i-1" {
		t.Fatalf("candidates %v: want the standby i-3, then i-1's drain cancelled (never i-2's, still being asked)", order)
	}
	if usable != 3 || window != 6 {
		t.Fatalf("usable %d window %d: the reclaimable VM counts toward the probe window", usable, window)
	}
	if _, err := p.reserveOn(f.insts[1], claim{Reclaim: "other"}); err != errNoRoom {
		t.Fatalf("reclaimed with another drain's op: %v", err)
	}
	if _, err := p.reserveOn(f.insts[1], claim{Promote: true}); err != errNoRoom {
		t.Fatalf("an ordinary promotion took a drain-requested VM: %v", err)
	}
	if _, err := p.reserveOn(f.insts[2], claim{Reclaim: "op2"}); err != errNoRoom {
		t.Fatalf("reclaimed a drain still being asked for: %v", err)
	}
	id, err := p.reserveOn(f.insts[1], claim{Reclaim: "op1"})
	if err != nil {
		t.Fatal(err)
	}
	if v := p.state(t).VMs["i-1"]; v.Role != roleDrainRequested {
		t.Fatalf("reserving reopened the VM before the helper cancelled its drain: %+v", v)
	}
	p.releaseAttempt(id)
	// Once the VM is seen draining, it is gone for good.
	p.loaded(t, func(st *poolState) { st.note("i-1").Draining = true })
	if _, err := p.reserveOn(f.insts[1], claim{Reclaim: "op1"}); err != errNoRoom {
		t.Fatalf("reclaimed a draining VM: %v", err)
	}
	p.reclaimed("i-1", "other")
	if v := p.state(t).VMs["i-1"]; v.Role == roleRetained {
		t.Fatal("another drain's answer reopened the VM")
	}
}

// Waiting demand the scheduled-drain VM can take launches nothing: the
// next job to probe takes it back.
func TestPlanCountsReclaimableVM(t *testing.T) {
	p, _ := schedPool(t, 2, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		allReady(st, now)
		scheduledDrain(st, "i-1", "op1", now)
		hold(s, "i-0", "a", now)
		hold(s, "i-0", "b", now)
	})
	w, err := p.Register(1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Done()
	d, err := p.Plan(context.Background(), 0)
	if err != nil || d.Launch || d.Ready != 1 {
		t.Fatalf("plan %+v %v: want no launch, the draining VM's room counted", d, err)
	}
	// Carried out (draining): no longer room, so demand launches.
	p.loaded(t, func(st *poolState) { st.note("i-1").Draining = true })
	if d, err := p.Plan(context.Background(), 0); err != nil || !d.Launch {
		t.Fatalf("plan %+v %v: want a launch once the VM is draining", d, err)
	}
}
