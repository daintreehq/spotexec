package client

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// A job admitted to a VM beside others still preparing (overflow, #227)
// waits there for a run permit once its files are ready. Capacity that
// appears meanwhile (a VM that finished booting, or one whose jobs ended)
// never reached it: selection was over. So a job that has waited
// moveAfter looks every moveEvery for a free slot on another VM of its
// runner, and when it gets one, withdraws from the busy VM (nothing has
// run there) and carries on there: sync, image, dependencies, command.
// A move costs that preparation again, so a job that is about to get its
// permit doesn't look, and a run moves at most maxMoves times (#256).
var (
	moveAfter = 30 * time.Second
	moveEvery = 10 * time.Second
)

const maxMoves = 2

// mover looks for a free slot elsewhere while a job waits for a run
// permit. start, stop, take and close are called from the run's own
// goroutine; the search runs in one of its own.
type mover struct {
	look   func(context.Context) *Lease
	found  chan struct{} // closed once a lease elsewhere is held
	mu     sync.Mutex
	lease  *Lease // found and not yet taken or given back
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newMover(look func(context.Context) *Lease) *mover {
	return &mover{look: look, found: make(chan struct{})}
}

// start begins the search, once: the first look after moveAfter, then
// every moveEvery until a slot is found or stop.
func (mv *mover) start(ctx context.Context) {
	if mv == nil || mv.cancel != nil {
		return
	}
	ctx, mv.cancel = context.WithCancel(ctx)
	mv.wg.Add(1)
	go func() {
		defer mv.wg.Done()
		t := time.NewTimer(moveAfter)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if l := mv.look(ctx); l != nil {
				mv.mu.Lock()
				if ctx.Err() != nil { // stopped while it looked: not wanted
					mv.mu.Unlock()
					l.abandon()
					return
				}
				mv.lease = l
				mv.mu.Unlock()
				close(mv.found)
				return
			}
			t.Reset(moveEvery)
		}
	}()
}

// ready is closed once a slot elsewhere is held for the job (never, for a
// nil mover).
func (mv *mover) ready() <-chan struct{} {
	if mv == nil {
		return nil
	}
	return mv.found
}

// stop ends the search without waiting for it, and gives back a slot found
// but not taken: the job got its permit where it is.
func (mv *mover) stop() {
	if mv == nil || mv.cancel == nil {
		return
	}
	mv.mu.Lock()
	mv.cancel()
	l := mv.lease
	mv.lease = nil
	mv.mu.Unlock()
	if l != nil {
		mv.wg.Add(1)
		go func() { defer mv.wg.Done(); l.abandon() }()
	}
}

// take hands over the slot found, once.
func (mv *mover) take() *Lease {
	if mv == nil {
		return nil
	}
	mv.mu.Lock()
	defer mv.mu.Unlock()
	l := mv.lease
	mv.lease = nil
	return l
}

// close stops the search and waits for it and any give-back to end.
func (mv *mover) close() {
	if mv == nil {
		return
	}
	mv.stop()
	mv.wg.Wait()
}

// findMove makes one pass over the other VMs of from's fleet for a free
// slot: an ordinary admission (never overflow), so the VM has a run permit
// free. It launches nothing and never waits in a queue. nil when none has
// room.
func findMove(ctx context.Context, req AcquireRequest, from *Lease) *Lease {
	f, ok := from.Provider.(provider.Fleet)
	if !ok || from.InstanceID == "" {
		return nil
	}
	m := &member{r: from.Runner, p: from.Provider, pool: NewPool(from.Runner, f)}
	req.Exclude = maps.Clone(req.Exclude)
	if req.Exclude == nil {
		req.Exclude = map[string]bool{}
	}
	req.Exclude[from.InstanceID] = true
	req.Base.Overflow = false
	req.Rep = quietReporter{req.Rep}
	var cands []candidate
	if m.r.SchedPolicy() == config.PolicyConsolidate {
		cs, _, _, err := m.pool.Rank(ctx, req.Exclude, claimFor(req))
		if err != nil {
			return nil
		}
		for _, c := range cs {
			if !c.claim.Overflow {
				cands = append(cands, c)
			}
		}
	} else {
		ins, _, err := m.pool.Candidates(ctx, req.Exclude)
		if err != nil {
			return nil
		}
		readyFirst(ins, m.r.WorkRoot)
		cands = legacyCandidates(ins, req)
	}
	until := time.Now().Add(2 * time.Minute)
	for _, c := range cands {
		if ctx.Err() != nil {
			return nil
		}
		lease, _, out := m.probeInstance(ctx, req, c, until)
		if out == OutUnreachable && ctx.Err() != nil {
			out = outSkipped // a cancelled probe proves nothing about the VM
		}
		m.pool.Observe(c.Instance.ID, out, c.Instance.LaunchedAt)
		if lease != nil {
			return lease
		}
		if m.fatal != nil {
			return nil
		}
	}
	return nil
}

// abandon gives the slot back unused (a withdrawn job, or a slot found for
// a move that didn't happen): its attempt leaves the ledger without
// counting as a finished lease in the duration model.
func (l *Lease) abandon() {
	if l == nil {
		return
	}
	if l.Sess != nil {
		l.Sess.Close()
	}
	if r := l.ledger; r != nil && r.pool != nil && r.id != "" {
		r.pool.dropAttempt(r.id)
		r.id = ""
	}
}
