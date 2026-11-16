package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
)

// helperScript stands in for every VM's helper, by address: what each
// answers to an acquire, and how often it was asked.
type helperScript struct {
	mu     sync.Mutex
	probes map[string]int
	answer func(req AcquireRequest, host string, n int) (*Lease, error) // n: this host's probe count, from 1
}

func scriptHelpers(t *testing.T, answer func(req AcquireRequest, host string, n int) (*Lease, error)) *helperScript {
	t.Helper()
	h := &helperScript{probes: map[string]int{}, answer: answer}
	old := tryAcquireFn
	tryAcquireFn = func(_ context.Context, req AcquireRequest, r *config.Runner, _ provider.Provider, tg provider.Target, _ bool) (*Lease, []protocol.Holder, error) {
		h.mu.Lock()
		h.probes[tg.Host]++
		n := h.probes[tg.Host]
		h.mu.Unlock()
		l, err := h.answer(req, tg.Host, n)
		if l != nil {
			l.Runner = r
		}
		return l, nil, err // a nil lease and error: busy
	}
	t.Cleanup(func() { tryAcquireFn = old })
	return h
}

func (h *helperScript) count(host string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probes[host]
}

// fleetsOf makes one fake fleet per name, each with the running VMs given
// (by address), and has Acquire use them.
func fleetsOf(t *testing.T, vms map[string][]string) []*config.Runner {
	pool, _ := fakeFleetsOf(t, vms, 0)
	return pool
}

// fakeFleetsOf is fleetsOf with the fleets, each allowed limit VMs (0: as
// many as it has).
func fakeFleetsOf(t *testing.T, vms map[string][]string, limit int) ([]*config.Runner, map[string]*fakeFleet) {
	t.Helper()
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	fleets := map[string]*fakeFleet{}
	var pool []*config.Runner
	for _, name := range []string{"broken", "healthy"} {
		hosts, ok := vms[name]
		if !ok {
			continue
		}
		r := &config.Runner{Name: name, Type: config.TypeAWSSpot, MaxInstances: max(limit, len(hosts)), MaxSlots: 2}
		r.BootTimeout.Duration = 10 * time.Minute
		f := &fakeFleet{r: r}
		for _, h := range hosts {
			f.insts = append(f.insts, provider.Instance{ID: "i-" + h, State: provider.StateRunning, LaunchedAt: time.Now().Add(-time.Hour), Target: provider.Target{Host: h}})
		}
		fleets[name] = f
		pool = append(pool, r)
	}
	old := newProvider
	newProvider = func(r *config.Runner) (provider.Provider, error) { return fleets[r.Name], nil }
	t.Cleanup(func() { newProvider = old })
	return pool, fleets
}

func bootFailed() error {
	return &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeBootFailed, Message: "bootstrap_script exited 1"}}
}

// #162: the first run to find a fleet's setup broken fails over to another
// runner in the pool, as later runs (which find it recorded) do, rather
// than failing the run.
func TestBootFailureFailsOverToAnotherRunner(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"broken": {"a1", "a2"}, "healthy": {"b1"}})
	h := scriptHelpers(t, func(_ AcquireRequest, host string, _ int) (*Lease, error) {
		if host == "b1" {
			return &Lease{}, nil
		}
		return nil, bootFailed()
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err != nil {
		t.Fatalf("acquire failed with a healthy runner left: %v", err)
	}
	if l.Runner.Name != "healthy" || l.InstanceID != "i-b1" {
		t.Fatalf("placed on %s/%s, want healthy/i-b1", l.Runner.Name, l.InstanceID)
	}
	// Its other VM would fail the same way: not probed.
	if a1, a2 := h.count("a1"), h.count("a2"); a1 != 1 || a2 != 0 {
		t.Fatalf("broken fleet probed a1 %d, a2 %d times; want 1, 0", a1, a2)
	}
}

// A healthy runner that is busy is waited for; the broken one is neither
// probed again nor counted as demand meanwhile.
func TestBootFailureWaitsForBusyRunner(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"broken": {"a1"}, "healthy": {"b1"}})
	h := scriptHelpers(t, func(_ AcquireRequest, host string, n int) (*Lease, error) {
		switch {
		case host != "b1":
			return nil, bootFailed()
		case n < 3:
			return nil, nil // busy
		}
		return &Lease{}, nil
	})
	l, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err != nil {
		t.Fatalf("acquire failed with a busy healthy runner left: %v", err)
	}
	if l.InstanceID != "i-b1" {
		t.Fatalf("placed on %s", l.InstanceID)
	}
	if n := h.count("a1"); n != 1 {
		t.Fatalf("broken fleet probed %d times; want once", n)
	}
}

// With nothing else in the pool, a broken setup still fails the run at
// once, saying how to fix it.
func TestBootFailureAloneFailsAtOnce(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"broken": {"a1"}})
	scriptHelpers(t, func(AcquireRequest, string, int) (*Lease, error) { return nil, bootFailed() })
	start := time.Now()
	_, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err == nil {
		t.Fatal("acquire succeeded on a broken setup")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %s to fail", took)
	}
	if want := "broken: BOOTSTRAP_FAILED"; !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "fix bootstrap_script, then `spotexec down broken`") || !isCode(err, protocol.CodeBootFailed) {
		t.Fatalf("error = %v", err)
	}
}

// Every runner broken: the run fails at once rather than queueing.
func TestBootFailureEverywhereFailsAtOnce(t *testing.T) {
	pool := fleetsOf(t, map[string][]string{"broken": {"a1"}, "healthy": {"b1"}})
	scriptHelpers(t, func(AcquireRequest, string, int) (*Lease, error) { return nil, bootFailed() })
	start := time.Now()
	_, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err == nil || !isCode(err, protocol.CodeBootFailed) {
		t.Fatalf("error = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %s to fail", took)
	}
}

// #162: on a Spot notice, an --exclusive run warms a whole VM for its
// replay. A VM with one of its two slots free has room for a shared run's
// replay, but not this one's: the spare must launch a VM rather than settle
// for that slot.
func TestSpareForExclusiveRunWarmsWholeVM(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		t.Run(fmt.Sprint("exclusive=", exclusive), func(t *testing.T) {
			pool, fleets := fakeFleetsOf(t, map[string][]string{"healthy": {"b1"}}, 2)
			var mu sync.Mutex
			var spare []AcquireRequest
			scriptHelpers(t, func(req AcquireRequest, host string, _ int) (*Lease, error) {
				mu.Lock()
				spare = append(spare, req)
				mu.Unlock()
				if host == "b1" && req.Base.Exclusive {
					return nil, nil // one slot busy: no whole VM here
				}
				return &Lease{}, nil
			})
			run := AcquireRequest{Pool: pool, QueueTimeout: time.Minute, Rep: nopRep{},
				Base: protocol.Acquire{Exclusive: exclusive, Fingerprint: "fp"}}
			req := spareRequest(run, "i-doomed", map[string]bool{"i-lost": true})
			if req.Base.Exclusive != exclusive || req.Base.Fingerprint != "" || !req.Exclude["i-doomed"] || !req.Exclude["i-lost"] || len(req.Exclude) != 2 {
				t.Fatalf("spare request = %+v", req)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			EnsureSpare(ctx, req)
			WaitLaunches(5 * time.Second)
			f := fleets["healthy"]
			f.mu.Lock()
			launched := f.n
			f.mu.Unlock()
			if want := map[bool]int{false: 0, true: 1}[exclusive]; launched != want {
				t.Fatalf("launched %d VMs, want %d", launched, want)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, r := range spare {
				if !r.Base.KeepOwner || r.Base.Exclusive != exclusive {
					t.Fatalf("spare probed with %+v", r.Base)
				}
			}
		})
	}
}
