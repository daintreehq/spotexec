package client

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// launchDraining reserves, provisions and drains n VMs one at a time, so
// each stays live but out of service.
func launchDraining(t *testing.T, p *Pool, n int) {
	t.Helper()
	ctx := context.Background()
	for range n {
		ids, _ := p.Reserve(ctx, 1)
		if len(ids) != 1 {
			t.Fatalf("setup reserved %d", len(ids))
		}
		in, err := p.Provision(ctx, ids[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Observe(in.ID, OutDraining, in.LaunchedAt)
	}
}

// Issue #273: manual prewarming (spotexec up) counted only usable VMs, so a
// capped fleet whose old VMs were draining grew past twice max_instances
// alive. Reserve honours the same live cap as Plan and EnsureMin, and says
// what holds the launches.
func TestReserveRespectsLiveCap(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)

	// Three live, all draining: one more fits under 2*max = 4.
	launchDraining(t, p, 3)
	res, err := p.ReserveWhy(ctx, 2)
	if err != nil || len(res.IDs) != 1 || !res.LiveCap {
		t.Fatalf("three live: %+v (%v), want 1 held by the live cap", res, err)
	}
	if !strings.Contains(res.Reason, "3 draining VMs") || !strings.Contains(res.Reason, "4 live VMs") {
		t.Fatalf("reason %q doesn't name the draining VMs holding the allowance", res.Reason)
	}
	// That reservation is pending and unlisted: it counts, so a second
	// prewarm gets nothing.
	if r2, _ := p.ReserveWhy(ctx, 2); len(r2.IDs) != 0 || !r2.LiveCap {
		t.Fatalf("pending launch not counted: %+v", r2)
	}
	if _, err := p.Provision(ctx, res.IDs[0], nil); err != nil {
		t.Fatal(err)
	}
	// Four live (three draining, one usable), listed and in the ledger: none.
	if r2, _ := p.ReserveWhy(ctx, 2); len(r2.IDs) != 0 || !strings.Contains(r2.Reason, "3 draining VMs") {
		t.Fatalf("four live: %+v, want 0", r2)
	}
	if ids, _ := p.Reserve(ctx, 2); len(ids) != 0 {
		t.Fatalf("four live via Reserve: reserved %d, want 0", len(ids))
	}
	if n := listed(t, f); n != 4 {
		t.Fatalf("live VMs %d, want 4", n)
	}
}

// A listed VM still in the ledger (booted, not yet ready) counts once
// toward the live cap: at max_instances = 3, four draining and one booting
// VM leave room for exactly one more of the six.
func TestReserveLiveCapCountsListedLaunchOnce(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 3)
	launchDraining(t, p, 4)
	ids, _ := p.Reserve(ctx, 1)
	if _, err := p.Provision(ctx, ids[0], nil); err != nil {
		t.Fatal(err)
	}
	if st := readState(t, p); len(st.Launches) != 1 || st.Launches[0].InstanceID == "" {
		t.Fatalf("fixture: want the booting VM in the ledger, got %+v", st.Launches)
	}
	res, err := p.ReserveWhy(ctx, 3)
	if err != nil || len(res.IDs) != 1 || !res.LiveCap {
		t.Fatalf("reserved %+v (%v), want 1 held by the live cap", res, err)
	}
	if !strings.Contains(res.Reason, "4 draining VMs") || !strings.Contains(res.Reason, "6 live VMs") {
		t.Fatalf("reason %q", res.Reason)
	}
}

// Below the live cap, max_instances is what limits a prewarm, and the
// reason says so; when both bind, max_instances is named, as Plan does.
func TestReserveWhyNamesMaxInstances(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 2)
	res, err := p.ReserveWhy(ctx, 3)
	if err != nil || len(res.IDs) != 2 || res.Reason != "at max_instances (2)" || res.LiveCap {
		t.Fatalf("reserved %+v, err %v", res, err)
	}
	if res, _ := p.ReserveWhy(ctx, 1); len(res.IDs) != 0 || res.Reason != "at max_instances (2)" {
		t.Fatalf("reserved %+v", res)
	}
	p, _ = testPool(t, 2)
	if res, _ := p.ReserveWhy(ctx, 2); res.Reason != "" || len(res.IDs) != 2 {
		t.Fatalf("full grant: %+v", res)
	}

	// Two draining and two usable at max_instances = 2: both limits bind.
	p, _ = testPool(t, 2)
	launchDraining(t, p, 2)
	ids, _ := p.Reserve(ctx, 2)
	for _, id := range ids {
		if _, err := p.Provision(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := p.ReserveWhy(ctx, 1); len(res.IDs) != 0 || res.Reason != "at max_instances (2)" || res.LiveCap {
		t.Fatalf("both limits: %+v", res)
	}
}

// Two prewarms at once at three live VMs get one launch between them. The
// two pool handles share the ledger and its lock file, as separate
// processes do.
func TestReserveLiveCapAcrossConcurrentCallers(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	launchDraining(t, p, 3)
	pools := []*Pool{NewPool(p.R, f), NewPool(p.R, f)}
	got := make(chan int, len(pools))
	var wg sync.WaitGroup
	for _, q := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := q.ReserveWhy(ctx, 2)
			if err != nil {
				t.Error(err)
			}
			got <- len(res.IDs)
		}()
	}
	wg.Wait()
	close(got)
	sum := 0
	for n := range got {
		sum += n
	}
	if sum != 1 {
		t.Fatalf("concurrent prewarms reserved %d at three live, want 1", sum)
	}
}

// A launch reserved while `spotexec down` runs (a concurrent up's prewarm)
// keeps its reservation through the reset that follows (#295), so another
// process can't reserve the same room and push the fleet past
// max_instances. The VMs the teardown ended free theirs.
func TestResetKeepsLaunchesReservedDuringDown(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	ids, _ := p.Reserve(ctx, 1)
	if _, err := p.Provision(ctx, ids[0], nil); err != nil {
		t.Fatal(err)
	}
	mark := p.MarkDown(context.Background())
	// The prewarm reserves while the teardown lists and terminates.
	prewarm, _ := p.Reserve(ctx, 1)
	if len(prewarm) != 1 {
		t.Fatalf("prewarm reserved %d", len(prewarm))
	}
	f.mu.Lock()
	f.insts = nil // down ended the VM it listed
	f.mu.Unlock()
	p.Reset(mark)
	if st := readState(t, p); len(st.Launches) != 1 || st.Launches[0].ID != prewarm[0] {
		t.Fatalf("launches after reset %+v, want just the prewarm's", st.Launches)
	}
	if got, _ := p.Reserve(ctx, 2); len(got) != 1 {
		t.Fatalf("reserved %d after down, want 1: the prewarm's launch still counts", len(got))
	}
	if _, err := p.Provision(ctx, prewarm[0], nil); err != nil {
		t.Fatalf("the prewarm's launch lost its reservation: %v", err)
	}
	if st := readState(t, p); !hasLaunch(st, prewarm[0]) {
		t.Fatalf("prewarm's launch record gone: %+v", st.Launches)
	}
}

func hasLaunch(st poolState, id string) bool {
	for _, l := range st.Launches {
		if l.ID == id && l.InstanceID != "" {
			return true
		}
	}
	return false
}

// A VM launched after down listed the fleet, with a run already placed on
// it, keeps that run in the ledger through the reset (#295): it still
// protects the VM from boot cleanup.
func TestResetKeepsRunsOnVMsDownDidNotSee(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 2)
	mark := p.MarkDown(ctx)
	ids, _ := p.Reserve(ctx, 1)
	in, err := p.Provision(ctx, ids[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	aid, err := p.reserveOn(in, claim{})
	if err == nil {
		err = p.markSent(aid)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.Reset(mark)
	if !p.state(t).protected(in.ID) {
		t.Fatalf("the run on %s was forgotten by the reset", in.ID)
	}
	if p.fenceBooting(in.ID) {
		t.Fatal("boot cleanup may end a VM a run is using")
	}
}
