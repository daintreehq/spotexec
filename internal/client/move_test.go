package client

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// #256: a job admitted to b1 beside others still preparing waits there
// for a run permit; b2, which came up since, has a free slot. The look for
// a move finds it with a plain request (never overflow, so b2 has a run
// permit free), without asking b1 again.
func TestMoveFindsAFreeSlotOnAnotherVM(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	asked := overflowHelpers(t, func(host string, overflow bool) (*Lease, []protocol.Holder) {
		if host == "b2" && !overflow {
			return &Lease{Acq: protocol.Acquired{Token: 1, MaxSlots: 2, Admit: 4}}, nil
		}
		return nil, preparingVM
	})
	f := fleets["healthy"]
	from := &Lease{Runner: pool[0], Provider: f, InstanceID: "i-b1"}
	l := findMove(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}}, from)
	if l == nil || l.InstanceID != "i-b2" {
		t.Fatalf("moved to %+v, want i-b2 (a free slot)", l)
	}
	defer l.abandon()
	if got := asked["b1"]; len(got) != 0 {
		t.Fatalf("the VM the job waits on was asked again: %v", got)
	}
	if got := asked["b2"]; len(got) != 1 || got[0] {
		t.Fatalf("b2 asked %v, want one plain request", got)
	}
	if firm, _ := NewPool(pool[0], f).state(t).held(pool[0], "i-b2"); firm != 1 {
		t.Fatalf("ledger holds %d jobs on b2, want the moving one", firm)
	}
}

// With no free slot anywhere else, the job stays: the look never asks to
// prepare beside other jobs (that would only move the wait) and launches
// nothing.
func TestMoveNeverOverflows(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	asked := overflowHelpers(t, func(host string, overflow bool) (*Lease, []protocol.Holder) {
		if overflow {
			return &Lease{Acq: overflowed}, nil
		}
		return nil, preparingVM
	})
	f := fleets["healthy"]
	from := &Lease{Runner: pool[0], Provider: f, InstanceID: "i-b1"}
	if l := findMove(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}}, from); l != nil {
		t.Fatalf("moved to %s with no free slot there", l.InstanceID)
	}
	for host, reqs := range asked {
		for _, o := range reqs {
			if o {
				t.Fatalf("%s asked for overflow: %v", host, reqs)
			}
		}
	}
	if n := len(f.insts); n != 2 {
		t.Fatalf("%d VMs after a look for a move, want the 2 there were", n)
	}
}

// fakeExec plays a helper's side of an exec: it sends what script says,
// and answers a withdraw with a withdrawn result.
type fakeExec struct {
	t      *testing.T
	frames chan protocol.Frame
	mu     sync.Mutex
	sent   []string
}

func newFakeExec(t *testing.T) *fakeExec {
	return &fakeExec{t: t, frames: make(chan protocol.Frame, 16)}
}

func (f *fakeExec) push(typ string, body any) {
	f.t.Helper()
	var b bytes.Buffer
	if err := protocol.NewConn(nil, &b).Send(typ, body); err != nil {
		f.t.Fatal(err)
	}
	fr, err := protocol.NewConn(&b, io.Discard).ReadFrame()
	if err != nil {
		f.t.Fatal(err)
	}
	f.frames <- fr
}

func (f *fakeExec) Next(ctx context.Context) (protocol.Frame, error) {
	select {
	case fr := <-f.frames:
		return fr, nil
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

func (f *fakeExec) Send(typ string, _ any) error {
	f.mu.Lock()
	f.sent = append(f.sent, typ)
	f.mu.Unlock()
	if typ == protocol.TypeWithdraw {
		f.push(protocol.TypeResult, protocol.Result{Withdrawn: true})
	}
	return nil
}

func (f *fakeExec) Abort() {}

func (f *fakeExec) sentTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func shortMoves(t *testing.T) {
	oldA, oldE := moveAfter, moveEvery
	moveAfter, moveEvery = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { moveAfter, moveEvery = oldA, oldE })
}

// heldLease is a lease on b2 recorded in its fleet's ledger, as findMove
// returns one.
func heldLease(t *testing.T) (*Lease, *Pool) {
	t.Helper()
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}}, 0)
	p := NewPool(pool[0], fleets["healthy"])
	b2 := fleets["healthy"].insts[1]
	id, err := p.reserveOn(b2, claim{})
	if err != nil {
		t.Fatal(err)
	}
	p.commitAttempt(id, protocol.Acquired{Token: 1, MaxSlots: 2, Admit: 4}, time.Time{})
	return &Lease{Runner: pool[0], InstanceID: b2.ID, Instance: b2, ledger: &ledgerRef{pool: p, id: id}}, p
}

// Once a slot elsewhere is held for a job waiting for a run permit, the
// exec asks its helper to withdraw the job, and the run carries on with
// that slot.
func TestWaitingJobWithdrawsOnceASlotIsFound(t *testing.T) {
	shortMoves(t)
	to, _ := heldLease(t)
	looks := 0
	mv := newMover(func(context.Context) *Lease {
		if looks++; looks < 2 {
			return nil // nothing yet: looks again
		}
		return to
	})
	defer mv.close()
	f := newFakeExec(t)
	f.push(protocol.TypeEvent, protocol.Event{Name: "run_wait", Message: "all 2 slots are running commands"})
	onRunWait := func(waiting bool, _ string) {
		if waiting {
			mv.start(context.Background())
		} else {
			mv.stop()
		}
	}
	res, err := pumpExec(f, io.Discard, io.Discard, Options{Rep: nopRep{}}, nil, nil, nil, onRunWait, mv.ready())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Withdrawn {
		t.Fatalf("result %+v, want the job withdrawn", res)
	}
	if got := f.sentTypes(); len(got) != 1 || got[0] != protocol.TypeWithdraw {
		t.Fatalf("sent %v, want one withdraw", got)
	}
	if l := mv.take(); l != to {
		t.Fatalf("took %+v, want the slot found", l)
	}
}

// A job that gets its run permit where it is gives back a slot found
// elsewhere meanwhile, unused, as soon as run_slot says so (while its
// command still runs), and the ledger forgets it.
func TestPermitFirstGivesTheFoundSlotBack(t *testing.T) {
	shortMoves(t)
	to, p := heldLease(t)
	mv := newMover(func(context.Context) *Lease { return to })
	defer mv.close()
	f := newFakeExec(t)
	f.push(protocol.TypeEvent, protocol.Event{Name: "run_wait", Message: "all 2 slots are running commands"})
	onRunWait := func(waiting bool, _ string) {
		if waiting {
			mv.start(context.Background())
			<-mv.ready() // found before the permit came
			f.push(protocol.TypeEvent, protocol.Event{Name: "run_slot"})
			return
		}
		mv.stop()
	}
	type out struct {
		res protocol.Result
		err error
	}
	done := make(chan out, 1)
	// The withdraw the found slot prompts races the run_slot already
	// queued: a helper ignores one that comes after the permit.
	go func() {
		res, err := pumpExec(noWithdrawReply{f}, io.Discard, io.Discard, Options{Rep: nopRep{}}, nil, nil, nil, onRunWait, mv.ready())
		done <- out{res, err}
	}()
	held := func() int { firm, _ := p.state(t).held(p.R, to.InstanceID); return firm }
	for deadline := time.Now().Add(5 * time.Second); held() != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("ledger still holds %d jobs on %s while the command runs", held(), to.InstanceID)
		}
	}
	select {
	case o := <-done:
		t.Fatalf("pumpExec returned %+v, %v before the command's result", o.res, o.err)
	default:
	}
	f.push(protocol.TypeResult, protocol.Result{ExitCode: 0})
	o := <-done
	if o.err != nil || o.res.Withdrawn {
		t.Fatalf("result %+v, %v: want the command's own", o.res, o.err)
	}
	if l := mv.take(); l != nil {
		t.Fatal("the slot found was kept after the permit came")
	}
}

// noWithdrawReply is a helper that already gave the job its permit: a
// withdraw gets no reply.
type noWithdrawReply struct{ *fakeExec }

func (n noWithdrawReply) Send(typ string, _ any) error {
	n.mu.Lock()
	n.sent = append(n.sent, typ)
	n.mu.Unlock()
	return nil
}

// A mover stopped before its look returns gives the slot it then finds
// back at once.
func TestStoppedMoverGivesBackALateSlot(t *testing.T) {
	shortMoves(t)
	to, p := heldLease(t)
	looking, release := make(chan struct{}), make(chan struct{})
	mv := newMover(func(context.Context) *Lease {
		close(looking)
		<-release
		return to
	})
	mv.start(context.Background())
	<-looking
	mv.stop()
	close(release)
	mv.close()
	if l := mv.take(); l != nil {
		t.Fatal("a stopped mover kept the slot it found")
	}
	if firm, _ := p.state(t).held(p.R, to.InstanceID); firm != 0 {
		t.Fatalf("ledger still holds %d jobs on %s", firm, to.InstanceID)
	}
	select {
	case <-mv.ready():
		t.Fatal("a stopped mover reported a slot found")
	default:
	}
}

var _ provider.Fleet = (*fakeFleet)(nil)
