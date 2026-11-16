package client

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
)

// fakeDrains scripts the VMs' answers to schedule-drain and try-drain.
type fakeDrains struct {
	mu        sync.Mutex
	installed map[string]bool
	answer    map[string]string // "<vm> <command>" -> result
	calls     []string
}

func (f *fakeDrains) install(t *testing.T) {
	t.Helper()
	ready, cmd := drainHelperReady, drainCmd
	t.Cleanup(func() { drainHelperReady, drainCmd = ready, cmd })
	drainHelperReady = func(_ *config.Runner, in provider.Instance) bool { return f.installed[in.ID] }
	drainCmd = func(_ context.Context, _ *config.Runner, in provider.Instance, args ...string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, in.ID+" "+args[0])
		op := ""
		for i, a := range args {
			if a == "--op" && i+1 < len(args) {
				op = args[i+1]
			}
		}
		return json.Marshal(helper.TryDrainResult{Result: f.answer[in.ID+" "+args[0]], Op: op, IdleS: -1})
	}
}

func (f *fakeDrains) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// With no pool manager running, the last sx process to finish hands the
// surplus VMs their drains, and each VM's own watchdog carries them out:
// a scheduled VM is never placed on or asked again; a VM whose watchdog
// predates scheduled drains is asked to drain now instead; one that can't
// drain at all goes back to standby and idles out.
func TestDrainSurplusSchedulesStandbyVMs(t *testing.T) {
	p, _ := schedPool(t, 4, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		for _, id := range []string{"i-0", "i-1", "i-3"} {
			s.VMs[id].Role, s.VMs[id].Since = roleStandby, now.Add(-time.Minute)
		}
	})
	f := &fakeDrains{
		installed: map[string]bool{"i-0": true, "i-1": true, "i-2": true},
		answer: map[string]string{
			"i-0 schedule-drain": helper.DrainScheduled,
			"i-1 schedule-drain": helper.DrainUnsupported, "i-1 try-drain": helper.DrainProtected,
		},
	}
	f.install(t)

	p.DrainSurplus()
	calls := map[string]bool{}
	for _, c := range f.took() {
		calls[c] = true
	}
	// i-2 is retained, i-3 has no helper from this machine (asking would
	// install one, touching its idle clock).
	if len(calls) != 3 || !calls["i-0 schedule-drain"] || !calls["i-1 schedule-drain"] || !calls["i-1 try-drain"] {
		t.Fatalf("calls %v", calls)
	}
	st := p.state(t)
	if v := st.VMs["i-0"]; v.Role != roleDrainRequested || !v.DrainScheduled {
		t.Fatalf("i-0 after a scheduled answer: %+v", v)
	}
	if v := st.VMs["i-1"]; v.Role != roleDrainRequested || v.DrainScheduled {
		t.Fatalf("i-1 still keeping its results: %+v", v)
	}
	if v := st.VMs["i-2"]; v.Role != roleRetained || v.DrainOp != "" {
		t.Fatalf("a retained VM was drained: %+v", v)
	}
	if v := st.VMs["i-3"]; v.Role != roleStandby {
		t.Fatalf("i-3 without a helper: %+v", v)
	}

	// The next pass: i-0 is the VM's to end, so it isn't asked again; i-1
	// is asked again once drainRetry has passed, with the same operation.
	op := st.VMs["i-1"].DrainOp
	p.loaded(t, func(st *poolState) { st.sched().VMs["i-1"].DrainAt = now.Add(-drainRetry - time.Second) })
	f.answer["i-1 try-drain"] = helper.DrainDone
	p.DrainSurplus()
	if c := f.took(); len(c) != 2 || c[0] != "i-1 schedule-drain" || c[1] != "i-1 try-drain" {
		t.Fatalf("second pass calls %v", c)
	}
	if v := p.state(t).VMs["i-1"]; v.Role != roleDraining || v.DrainOp != op {
		t.Fatalf("i-1 after drained: %+v", v)
	}
	p.DrainSurplus()
	if c := f.took(); len(c) != 0 {
		t.Fatalf("asked again: %v", c)
	}

	// A VM that can't drain at all (not an ephemeral spotexec VM) goes back
	// to standby, and isn't asked again for an hour.
	f.installed["i-3"] = true
	f.answer["i-3 schedule-drain"], f.answer["i-3 try-drain"] = helper.DrainUnsupported, helper.DrainUnsupported
	p.DrainSurplus()
	if c := f.took(); len(c) != 2 {
		t.Fatalf("i-3 calls %v", c)
	}
	var closed bool
	p.loaded(t, func(st *poolState) { closed = st.Notes["i-3"].closed() })
	if v := p.state(t).VMs["i-3"]; v.Role != roleStandby || v.DrainOp != "" || closed {
		t.Fatalf("i-3 after unsupported: %+v (closed %v)", v, closed)
	}
	p.DrainSurplus()
	if c := f.took(); len(c) != 0 {
		t.Fatalf("an unsupported VM was asked again at once: %v", c)
	}

	// early_drain = false: nothing is asked.
	off := false
	p.R.Scheduling.EarlyDrain = &off
	p.loaded(t, func(st *poolState) {
		v := st.sched().VMs["i-3"]
		v.DrainAt, v.Since = time.Time{}, now.Add(-time.Minute)
	})
	p.DrainSurplus()
	if c := f.took(); len(c) != 0 {
		t.Fatalf("early_drain off: %v", c)
	}
}

// A lease's end runs the drain pass, once.
func TestLeaseCloseDrainsSurplus(t *testing.T) {
	p, f := schedPool(t, 2, 2)
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		s := st.sched()
		p.views(st, now)
		s.EpochAt = now
		s.VMs["i-0"].Role, s.VMs["i-0"].Since = roleStandby, now.Add(-time.Minute)
	})
	fd := &fakeDrains{installed: map[string]bool{"i-0": true}, answer: map[string]string{"i-0 schedule-drain": helper.DrainScheduled}}
	fd.install(t)
	id, err := p.reserveOn(f.insts[1], claim{})
	if err != nil {
		t.Fatal(err)
	}
	l := &Lease{ledger: &ledgerRef{pool: p, id: id}}
	l.Close()
	if c := fd.took(); len(c) != 1 || c[0] != "i-0 schedule-drain" {
		t.Fatalf("calls %v", c)
	}
	if v := p.state(t).VMs["i-0"]; !v.DrainScheduled {
		t.Fatalf("after close: %+v", v)
	}
	l.Close()
	if c := fd.took(); len(c) != 0 {
		t.Fatalf("a second Close asked again: %v", c)
	}
}
