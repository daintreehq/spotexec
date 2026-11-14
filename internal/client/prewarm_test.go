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
