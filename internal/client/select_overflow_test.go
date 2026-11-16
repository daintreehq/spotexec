package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// overflowHelpers stands in for every VM's helper: answer gets the host and
// whether the request asked for overflow; every request is recorded.
func overflowHelpers(t *testing.T, answer func(host string, overflow bool) (*Lease, []protocol.Holder)) map[string][]bool {
	t.Helper()
	var mu sync.Mutex
	asked := map[string][]bool{}
	old := tryAcquireFn
	tryAcquireFn = func(_ context.Context, req AcquireRequest, r *config.Runner, _ provider.Provider, tg provider.Target, _ bool) (*Lease, []protocol.Holder, error) {
		if req.onSend != nil {
			if err := req.onSend(); err != nil {
				return nil, nil, err
			}
		}
		mu.Lock()
		asked[tg.Host] = append(asked[tg.Host], req.Base.Overflow)
		mu.Unlock()
		l, hs := answer(tg.Host, req.Base.Overflow)
		if l != nil {
			l.Runner = r
		}
		return l, hs, nil
	}
	t.Cleanup(func() { tryAcquireFn = old })
	return asked
}

var (
	preparingVM = []protocol.Holder{{Token: 1, Phased: true, HelperPID: 1}, {Token: 2, Phased: true, Phase: protocol.PhaseRun, HelperPID: 2}}
	runningVM   = []protocol.Holder{{Token: 1, Phased: true, Phase: protocol.PhaseRun, HelperPID: 1}, {Token: 2, HelperPID: 2}}
	overflowed  = protocol.Acquired{Token: 3, MaxSlots: 2, Admit: 4}
)

// #227: when no VM has a free slot, a VM whose slots are taken by jobs
// still preparing is asked again to admit this one beside them (overflow),
// before the job queues or launches; a VM whose jobs all run commands is
// not.
func TestFullVMsWithPreparingJobsTakeOverflow(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}})
	asked := overflowHelpers(t, func(host string, overflow bool) (*Lease, []protocol.Holder) {
		switch {
		case host == "b2":
			return nil, runningVM
		case overflow:
			return &Lease{Acq: overflowed}, nil
		}
		return nil, preparingVM
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if l.InstanceID != "i-b1" {
		t.Fatalf("placed on %s, want i-b1 (its jobs only prepare)", l.InstanceID)
	}
	if b1 := asked["b1"]; len(b1) != 2 || b1[0] || !b1[1] {
		t.Fatalf("b1 asked %v, want a plain request then an overflow one", b1)
	}
	if b2 := asked["b2"]; len(b2) != 1 || b2[0] {
		t.Fatalf("b2 (every slot running a command) asked %v, want one plain request", b2)
	}
}

// A free slot anywhere wins over preparing beside other jobs.
func TestFreeSlotBeatsOverflow(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}})
	asked := overflowHelpers(t, func(host string, overflow bool) (*Lease, []protocol.Holder) {
		if host == "b2" || overflow {
			return &Lease{Acq: protocol.Acquired{Token: 1, MaxSlots: 2, Admit: 4}}, nil
		}
		return nil, preparingVM
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if l.InstanceID != "i-b2" {
		t.Fatalf("placed on %s, want i-b2 (a free slot)", l.InstanceID)
	}
	for _, o := range asked["b1"] {
		if o {
			t.Fatalf("b1 asked for overflow while b2 had a free slot: %v", asked["b1"])
		}
	}
}

// A VM this machine's own jobs fill, whose helper admits more, is offered
// by the ledger as an overflow candidate (and asked only for overflow)
// rather than skipped, and the overflow job's attempt stays in the ledger.
func TestLedgerOffersOverflowOnOwnFullVM(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	b1 := fleets["healthy"].insts[0]
	for i := 0; i < 2; i++ {
		id, err := p.reserveOn(b1, claim{})
		if err != nil {
			t.Fatal(err)
		}
		p.commitAttempt(id, protocol.Acquired{MaxSlots: 2, Admit: 4}, time.Time{})
	}
	asked := overflowHelpers(t, func(host string, overflow bool) (*Lease, []protocol.Holder) {
		if host == "b1" && overflow {
			return &Lease{Acq: overflowed}, nil
		}
		return nil, runningVM
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.ledger.release()
	if l.InstanceID != "i-b1" {
		t.Fatalf("placed on %s, want i-b1", l.InstanceID)
	}
	if got := asked["b1"]; len(got) != 1 || !got[0] {
		t.Fatalf("b1 asked %v, want one overflow request", got)
	}
	if firm, _ := p.state(t).held(p.R, b1.ID); firm != 3 {
		t.Fatalf("ledger holds %d jobs on b1, want 3", firm)
	}
}
