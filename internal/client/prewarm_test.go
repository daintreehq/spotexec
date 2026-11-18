package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

func listed(t *testing.T, f *fakeFleet) int {
	t.Helper()
	ins, _ := f.List(context.Background())
	return len(ins)
}

func readState(t *testing.T, p *Pool) poolState {
	t.Helper()
	var out poolState
	_ = p.with(func(st *poolState) error { out = *st; return nil })
	return out
}

func TestPrewarmKeepsReadyInstance(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	ids, _ := p.Reserve(ctx, 1)
	in, err := p.Prewarm(ctx, ids[0], nil, func(context.Context, provider.Instance) error { return nil }, nil)
	if err != nil || in.ID == "" {
		t.Fatalf("Prewarm: %v %v", in, err)
	}
	if listed(t, f) != 1 {
		t.Fatal("ready instance was terminated")
	}
}

func TestPrewarmTerminatesFailedBootstrap(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	ids, _ := p.Reserve(ctx, 1)
	_, err := p.Prewarm(ctx, ids[0], nil, func(context.Context, provider.Instance) error {
		return &bootstrapError{"fake: bootstrap_script exited 1"}
	}, func(context.Context, provider.Instance) bool { return true })
	if !errors.Is(err, ErrBootstrapFailed) || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("want a bootstrap failure that says the VM was terminated, got %v", err)
	}
	if listed(t, f) != 0 {
		t.Fatal("VM whose bootstrap failed was left running")
	}
	st := readState(t, p)
	var bootFailed bool
	for _, n := range st.Notes {
		bootFailed = bootFailed || n.BootFailed
	}
	if !bootFailed {
		t.Fatal("bootstrap failure not recorded as a failed boot")
	}
	for _, l := range st.Launches {
		if l.ID == ids[0] && !l.Failed {
			t.Fatal("terminated VM still counted as pending capacity")
		}
	}
	// Like a failure a run finds: no new launches until fixed.
	d, err := p.Plan(ctx, 0)
	if err != nil || d.Launch || !strings.Contains(d.Reason, "bootstrap_script failed") {
		t.Fatalf("Plan after a failed boot: %+v %v", d, err)
	}
}

func TestPrewarmTerminatesUnreachableAndBacksOff(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	ids, _ := p.Reserve(ctx, 1)
	_, err := p.Prewarm(ctx, ids[0], nil, func(context.Context, provider.Instance) error {
		return fmt.Errorf("runner fake not ready after 10m0s: dial tcp: i/o timeout")
	}, func(context.Context, provider.Instance) bool { return false })
	if err == nil || errors.Is(err, ErrBootstrapFailed) {
		t.Fatalf("want a plain readiness failure, got %v", err)
	}
	if listed(t, f) != 0 {
		t.Fatal("unreachable VM was left running")
	}
	st := readState(t, p)
	if !time.Now().Before(st.BackoffUntil) || !strings.Contains(st.LastLaunchErr, "not ready") {
		t.Fatalf("no launch backoff after a failed boot: %+v", st)
	}
	for _, n := range st.Notes {
		if n.BootFailed {
			t.Fatal("a readiness timeout is not a bootstrap failure")
		}
	}
}

func TestPrewarmInterruptedTerminates(t *testing.T) {
	p, f := testPool(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	ids, _ := p.Reserve(ctx, 1)
	_, err := p.Prewarm(ctx, ids[0], nil, func(ctx context.Context, _ provider.Instance) error {
		cancel() // Ctrl-C while waiting for readiness
		<-ctx.Done()
		return ctx.Err()
	}, func(context.Context, provider.Instance) bool { return false })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if listed(t, f) != 0 {
		t.Fatal("Ctrl-C left the launched VM running")
	}
	if st := readState(t, p); time.Now().Before(st.BackoffUntil) {
		t.Fatal("an interrupt is not a fleet problem: no backoff expected")
	}
}

func TestPrewarmInterruptedBeforeLaunch(t *testing.T) {
	p, f := testPool(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	ids, _ := p.Reserve(ctx, 1)
	cancel()
	called := false
	_, err := p.Prewarm(ctx, ids[0], nil, func(context.Context, provider.Instance) error { called = true; return nil }, nil)
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("Prewarm after cancel: err %v, ready called %v", err, called)
	}
	if f.n != 0 {
		t.Fatal("launched after the user interrupted")
	}
	for _, l := range readState(t, p).Launches {
		if l.ID == ids[0] && !l.Failed {
			t.Fatal("abandoned reservation still counted")
		}
	}
}

func TestPrewarmReportsFailedTerminate(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	ids, _ := p.Reserve(ctx, 1)
	f.termErr = errors.New("UnauthorizedOperation")
	_, err := p.Prewarm(ctx, ids[0], nil, func(context.Context, provider.Instance) error { return errors.New("boom") }, nil)
	if err == nil || !strings.Contains(err.Error(), "spotexec down fake") {
		t.Fatalf("a failed terminate must tell the user how to clean up: %v", err)
	}
	st := readState(t, p)
	for _, l := range st.Launches {
		if l.ID == ids[0] && l.Failed {
			t.Fatal("reservation freed although the VM may still be running")
		}
	}
	for id, n := range st.Notes {
		if n.Draining {
			t.Fatalf("%s marked draining although it may still be running: health checks would skip it", id)
		}
	}
}

func TestPrewarmLeavesFinishedVMForOtherRuns(t *testing.T) {
	p, f := testPool(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	ids, _ := p.Reserve(ctx, 1)
	_, err := p.Prewarm(ctx, ids[0], nil, func(ctx context.Context, _ provider.Instance) error {
		cancel() // Ctrl-C just after the VM became ready, during the helper install
		return ctx.Err()
	}, func(context.Context, provider.Instance) bool { return true })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("want an interrupt that says the VM was left running, got %v", err)
	}
	if listed(t, f) != 1 {
		t.Fatal("a VM that finished booting (and may be serving other runs) was terminated")
	}
}

func TestPrewarmTerminatesVMStillPendingAtDeadline(t *testing.T) {
	p, f := testPool(t, 2)
	p.R.BootTimeout.Duration = -time.Minute + 50*time.Millisecond // provisioning deadline: 50ms
	f.slowRunning = true
	ids, _ := p.Reserve(context.Background(), 1)
	_, err := p.Prewarm(context.Background(), ids[0], nil, func(context.Context, provider.Instance) error {
		t.Fatal("ready called for a VM that never reached running")
		return nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("want the provisioning timeout and a terminated VM, got %v", err)
	}
	if listed(t, f) != 0 {
		t.Fatal("a VM still not running at the provisioning deadline was left running")
	}
}

// #295: a VM whose status never read through boot, while listings showed
// it serving, isn't ended under a run already placed on it; one nothing
// reserved is fenced while it is ended, so no run is placed on it then.
func TestProvisionFencesABootFailureBeforeEndingIt(t *testing.T) {
	ctx := context.Background()
	vm := func(f *fakeFleet, id string) provider.Instance {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, in := range f.insts {
			if in.ID == id {
				return in
			}
		}
		t.Fatalf("no %s listed", id)
		return provider.Instance{}
	}
	t.Run("a run placed on it", func(t *testing.T) {
		p, f := testPool(t, 2)
		f.goneOnWait = true
		f.onWait = func(id string) {
			aid, err := p.reserveOn(vm(f, id), claim{})
			if err == nil {
				err = p.markSent(aid) // its request went out: the VM answered it
			}
			if err != nil {
				t.Errorf("placing a run while it boots: %v", err)
			}
		}
		terminated := false
		f.onTerminate = func(string) { terminated = true }
		ids, _ := p.Reserve(ctx, 1)
		in, err := p.Provision(ctx, ids[0], nil)
		if err == nil || !strings.Contains(err.Error(), "left running") {
			t.Fatalf("err = %v, want the VM left running", err)
		}
		if terminated || listed(t, f) != 1 {
			t.Fatalf("ended %s under the run placed on it", in.ID)
		}
		if _, err := p.reserveOn(vm(f, in.ID), claim{}); err != nil {
			t.Fatalf("the VM left running is fenced: %v", err)
		}
	})
	t.Run("a run that sent nothing yet", func(t *testing.T) {
		p, f := testPool(t, 2)
		f.goneOnWait = true
		var aid string
		f.onWait = func(id string) {
			var err error
			if aid, err = p.reserveOn(vm(f, id), claim{}); err != nil {
				t.Errorf("reserving while it boots: %v", err)
			}
		}
		ids, _ := p.Reserve(ctx, 1)
		if _, err := p.Provision(ctx, ids[0], nil); err == nil || strings.Contains(err.Error(), "left running") {
			t.Fatalf("err = %v, want the boot failure", err)
		}
		if listed(t, f) != 0 {
			t.Fatal("left running for a reservation that never reached it: nothing would end it")
		}
		if err := p.markSent(aid); !errors.Is(err, errReservationLost) {
			t.Fatalf("the revoked reservation's send: %v, want it refused as lost (not a reachability failure)", err)
		}
	})
	t.Run("nothing placed on it", func(t *testing.T) {
		p, f := testPool(t, 2)
		f.goneOnWait = true
		var during error
		f.onTerminate = func(id string) { _, during = p.reserveOn(vm(f, id), claim{Force: true}) }
		ids, _ := p.Reserve(ctx, 1)
		if _, err := p.Provision(ctx, ids[0], nil); err == nil || strings.Contains(err.Error(), "left running") {
			t.Fatalf("err = %v, want the boot failure", err)
		}
		if !errors.Is(during, errReaping) {
			t.Fatalf("an exact slot reserved it while it was being ended: %v", during)
		}
		if listed(t, f) != 0 {
			t.Fatal("the VM that never got ready was left running")
		}
	})
}

func TestProvisionReportsFailedTerminate(t *testing.T) {
	p, f := testPool(t, 2)
	f.goneOnWait = true
	f.termErr = errors.New("UnauthorizedOperation")
	ids, _ := p.Reserve(context.Background(), 1)
	in, err := p.Provision(context.Background(), ids[0], nil)
	if err == nil || in.ID == "" || !strings.Contains(err.Error(), "spotexec down fake") || !strings.Contains(err.Error(), in.ID) {
		t.Fatalf("a failed cleanup must name the VM and how to end it: %v %+v", err, in)
	}
	for _, l := range readState(t, p).Launches {
		if l.ID == ids[0] && l.Failed {
			t.Fatal("reservation freed although the VM may still be running")
		}
	}
}
