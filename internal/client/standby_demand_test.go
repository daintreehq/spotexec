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

// #257: an exclusive part with no empty retained VM takes the empty
// standby VM (promoting it) before anything launches.
func TestExclusiveAcquirePromotesEmptyStandby(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		allReady(st, now)
		s.VMs["i-b2"].Role, s.VMs["i-b2"].Since = roleStandby, now.Add(-time.Minute)
		hold(s, "i-b1", "a", now) // partly busy: no whole VM there
	})
	h := scriptHelpers(t, func(AcquireRequest, string, int) (*Lease, error) { return &Lease{}, nil })
	rep := &placeRep{}
	req := AcquireRequest{Pool: pool, QueueTimeout: 3 * time.Second, Rep: rep, NoBoot: true}
	req.Base.Exclusive = true
	l, err := Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("exclusive part not placed on the empty standby VM: %v", err)
	}
	defer l.Close()
	if l.InstanceID != "i-b2" || h.count("b1") != 0 {
		t.Fatalf("placed on %s (b1 probed %d times), want the empty standby i-b2", l.InstanceID, h.count("b1"))
	}
	if v := p.state(t).VMs["i-b2"]; v.Role != roleRetained {
		t.Fatalf("i-b2 after placement: %+v, want it back in the retained set", v)
	}
}

// #257: a run seeking a slot, from before its scan until it holds one,
// keeps the fleet's standby VMs from being drained early: they are room it
// may be about to take, before any waiter says so.
func TestNoEarlyDrainWhileARunSeeks(t *testing.T) {
	p, _ := schedPool(t, 2, 2)
	now := time.Now()
	ctx := context.Background()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		allReady(st, now)
		s.VMs["i-0"].Role, s.VMs["i-0"].Since = roleStandby, now.Add(-time.Minute)
	})
	seek := Seek([]*config.Runner{p.R})
	if _, ok, _ := p.BeginDrain(ctx, "i-0"); ok {
		t.Fatal("drained a standby VM while a run is seeking a slot")
	}
	if v := p.state(t).VMs["i-0"]; v.Role != roleStandby {
		t.Fatalf("i-0: %+v, want it still standby", v)
	}
	seek.Done()
	seek.Done() // idempotent
	if _, ok, _ := p.BeginDrain(ctx, "i-0"); !ok {
		t.Fatal("not drained once no run seeks a slot")
	}
}

// A mark counts only while its run lives and refreshes it, and dead runs'
// marks are swept.
func TestSeekingMarkLifecycle(t *testing.T) {
	p, _ := schedPool(t, 1, 2)
	off := false
	other := *p.R
	other.Name, other.Scheduling.EarlyDrain = "kept", &off
	seek := Seek([]*config.Runner{p.R, &other})
	defer seek.Done()
	if len(seek.paths) != 1 {
		t.Fatalf("marks %v: want one, on the early-draining fleet only", seek.paths)
	}
	own := seek.paths[0]
	if !p.seeking() {
		t.Fatal("a live run's mark doesn't count")
	}
	// Stopped (not refreshed): no longer counts, but isn't removed.
	old := time.Now().Add(-staleAfter - time.Second)
	if err := os.Chtimes(seek.paths[0], old, old); err != nil {
		t.Fatal(err)
	}
	if p.seeking() {
		t.Fatal("a stopped run's mark still counts")
	}
	seek.Touch()
	if !p.seeking() {
		t.Fatal("a refreshed mark doesn't count again")
	}
	// Dead: nobody holds the file, or a mark was never finished.
	dir := filepath.Join(p.dir, seekingDir)
	dead := filepath.Join(dir, "1-dead.s")
	tmp := filepath.Join(dir, "1-half.s.tmp")
	for _, f := range []string{dead, tmp} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	// One being made right now, or by a run stopped while making it
	// (locked), is left alone.
	fresh := filepath.Join(dir, "1-fresh.s.tmp")
	if err := os.WriteFile(fresh, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	making := filepath.Join(dir, "1-making.s.tmp")
	mf, err := os.OpenFile(making, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Close()
	if err := syscall.Flock(int(mf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(making, old, old); err != nil {
		t.Fatal(err)
	}
	seek.Done()
	if p.seeking() {
		t.Fatal("a dead run's mark, or one still being made, counts")
	}
	for _, f := range []string{fresh, making} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s swept while being made: %v", f, err)
		}
	}
	for _, f := range []string{dead, tmp, own} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
}

// End to end: Acquire marks the fleet while it looks for a slot, and
// withdraws the mark once it holds one.
func TestAcquireSeeksUntilPlaced(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	var during bool
	scriptHelpers(t, func(AcquireRequest, string, int) (*Lease, error) {
		during = p.seeking()
		return &Lease{}, nil
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: 3 * time.Second, Rep: nopRep{}, NoBoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !during {
		t.Fatal("not marked seeking while probing")
	}
	if p.seeking() {
		t.Fatal("still marked seeking once placed")
	}
}

// #257: a waiting exclusive part counts an empty standby VM as room, so
// nothing launches for it while the VM is there to take.
func TestPlanCountsEmptyStandbyForExclusive(t *testing.T) {
	p := readyFleet(t, 2, 0, 1, time.Second)
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		s.VMs["i-0"].Role, s.VMs["i-0"].Since = roleStandby, time.Now().Add(-time.Minute)
		hold(s, "i-1", "a", time.Now())
	})
	if d, err := p.Plan(context.Background(), 0); err != nil || d.Launch || d.Ready != 4 {
		t.Fatalf("plan %+v %v: want no launch, the empty standby VM covering the exclusive part", d, err)
	}
}

// A mark stays live through a scan longer than staleAfter: it refreshes
// itself until withdrawn, and not after.
func TestSeekingRefreshesItself(t *testing.T) {
	p, _ := schedPool(t, 1, 2)
	old := seekBeat
	seekBeat = 10 * time.Millisecond
	t.Cleanup(func() { seekBeat = old })
	seek := Seek([]*config.Runner{p.R})
	path := seek.paths[0]
	stale := time.Now().Add(-staleAfter - time.Second)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !p.seeking() {
		if time.Now().After(deadline) {
			t.Fatal("a mark left stale by a long scan was never refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	seek.Done()
	time.Sleep(50 * time.Millisecond) // a refresh after Done would recreate nothing, but must not run
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mark still there after Done: %v", err)
	}
}

// The run's mark, handed to Acquire, is withdrawn as Acquire returns (not
// held through the command); a request carrying a mark already withdrawn
// (a spare warmed for a replay copies its run's) gets a fresh one.
func TestAcquireWithdrawsHandedMark(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	var during []bool
	scriptHelpers(t, func(AcquireRequest, string, int) (*Lease, error) {
		during = append(during, p.seeking())
		return &Lease{}, nil
	})
	seek := Seek(pool)
	req := AcquireRequest{Pool: pool, QueueTimeout: 3 * time.Second, Rep: nopRep{}, NoBoot: true, seeking: seek}
	l, err := Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if !seek.withdrawn() || p.seeking() {
		t.Fatal("the run's mark outlived its acquire")
	}
	l, err = Acquire(context.Background(), spareRequest(req, "i-other", nil))
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if len(during) != 2 || !during[0] || !during[1] {
		t.Fatalf("seeking while probing: %v, want both acquires marked", during)
	}
	if p.seeking() {
		t.Fatal("the spare's mark outlived its acquire")
	}
}
