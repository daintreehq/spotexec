package client

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// slowHelpers has every VM's helper take after to answer an acquire: a
// connection failure, or (lease) a slot. A helper that honors its context
// gives up when the probe's context ends; one that doesn't answers anyway.
func slowHelpers(t *testing.T, after time.Duration, lease, honor bool) func() []string {
	t.Helper()
	var mu sync.Mutex
	var probed []string
	old := tryAcquireFn
	tryAcquireFn = func(ctx context.Context, req AcquireRequest, r *config.Runner, _ provider.Provider, tg provider.Target, _ bool) (*Lease, []protocol.Holder, error) {
		mu.Lock()
		probed = append(probed, tg.Host)
		mu.Unlock()
		if req.onSend != nil {
			if err := req.onSend(); err != nil {
				return nil, nil, err
			}
		}
		if honor {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(after):
			}
		} else {
			time.Sleep(after)
		}
		if lease {
			return &Lease{Runner: r}, nil, nil
		}
		return nil, nil, errors.New("dial tcp " + tg.Host + ":22: connect: connection refused")
	}
	t.Cleanup(func() { tryAcquireFn = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), probed...)
	}
}

// #269: a scan of the fleet stops at queue_timeout instead of giving every
// remaining VM a full probe, and a VM cut off by the deadline isn't held
// against it.
func TestAcquireScanStopsAtQueueDeadline(t *testing.T) {
	pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1", "b2", "b3", "b4"}}, 4)
	probed := slowHelpers(t, 5*time.Second, false, true)
	const queue = 500 * time.Millisecond
	start := time.Now()
	_, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: queue, Rep: nopRep{}, NoBoot: true})
	took := time.Since(start)
	WaitLaunches(5 * time.Second)
	if err == nil || !strings.Contains(err.Error(), protocol.CodeQueueTimeout) {
		t.Fatalf("error = %v, want a queue timeout", err)
	}
	if took > queue+time.Second {
		t.Fatalf("returned after %s with a %s queue timeout (probed %v)", took, queue, probed())
	}
	if p := probed(); len(p) != 1 {
		t.Fatalf("probed %v, want just the first VM, cut off by the deadline", p)
	}
	st := readState(t, NewPool(pool[0], fleets["healthy"]))
	for _, h := range []string{"b1", "b2", "b3", "b4"} {
		if n := st.Notes["i-"+h]; n != nil && (n.Failures != 0 || !n.SuspectUntil.IsZero()) {
			t.Fatalf("%s marked unreachable for a probe the deadline cut: %+v", h, n)
		}
	}
	// The request went out before the deadline cut it: the helper may hold
	// the slot until it notices, so the attempt is left uncertain, not firm.
	atts := NewPool(pool[0], nil).state(t).Attempts
	if len(atts) != 1 {
		t.Fatalf("attempts %v, want the one interrupted probe's", atts)
	}
	for id, a := range atts {
		if a.State != attUncertain {
			t.Fatalf("attempt %s left %v, want uncertain", id, a.State)
		}
	}
}

// #269: a helper that grants the slot only after the queue deadline passed
// doesn't place the run: the slot is given back and the run times out.
func TestAcquireLateLeaseIsReleased(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"healthy": {"b1", "b2"}})
	probed := slowHelpers(t, time.Second, true, false)
	rep := &recRep{}
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: 500 * time.Millisecond, Rep: rep, NoBoot: true})
	WaitLaunches(5 * time.Second)
	if l != nil || err == nil || !strings.Contains(err.Error(), protocol.CodeQueueTimeout) {
		t.Fatalf("lease %v, error %v; want a queue timeout", l, err)
	}
	if p := probed(); len(p) != 1 {
		t.Fatalf("probed %v, want just the first VM", p)
	}
	if s := NewPool(pool[0], nil).state(t); len(s.Attempts) != 0 {
		t.Fatalf("attempts after a late lease: %v", s.Attempts)
	}
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if slices.Contains(rep.events, "placed") {
		t.Fatalf("a late lease reported as placed: %v", rep.events)
	}
}

// #269: a cloud listing that outlasts queue_timeout ends with the queue
// timeout, not after the listing gives up on its own, and not as "no runner
// available".
func TestAcquireListingStopsAtQueueDeadline(t *testing.T) {
	for _, policy := range []string{config.PolicyConsolidate, config.PolicyLegacy} {
		t.Run(policy, func(t *testing.T) {
			pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1"}}, 1)
			pool[0].Scheduling.Policy = policy
			f := fleets["healthy"]
			f.slowList = true
			probed := slowHelpers(t, 0, true, true)
			const queue = 200 * time.Millisecond
			start := time.Now()
			_, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: queue, Rep: nopRep{}, NoBoot: true})
			took := time.Since(start)
			WaitLaunches(5 * time.Second)
			if err == nil || !strings.Contains(err.Error(), protocol.CodeQueueTimeout) {
				t.Fatalf("error = %v, want a queue timeout", err)
			}
			if took > queue+time.Second {
				t.Fatalf("returned after %s with a %s queue timeout", took, queue)
			}
			f.mu.Lock()
			listed := f.listDeadline
			f.mu.Unlock()
			// Acquire sets its deadline just after start: allow it that.
			if by := start.Add(queue + 100*time.Millisecond); listed.IsZero() || listed.After(by) {
				t.Fatalf("listing's deadline %v, want the queue's (by %v)", listed, by)
			}
			if p := probed(); len(p) != 0 {
				t.Fatalf("probed %v without a listing", p)
			}
		})
	}
}
