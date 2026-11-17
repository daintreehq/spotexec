package client

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// Issue #273: manual prewarming (spotexec up) counted only usable VMs, so a
// capped fleet whose old VMs were draining grew past twice max_instances
// alive. Reserve honours the same live cap as Plan and EnsureMin, and says
// what holds the launches.
func TestReserveRespectsLiveCap(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 2)
	drain := func(n int) {
		t.Helper()
		ids, _ := p.Reserve(ctx, n)
		if len(ids) != n {
			t.Fatalf("reserved %d of %d", len(ids), n)
		}
		for _, id := range ids {
			in, err := p.Provision(ctx, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.Observe(in.ID, OutDraining, in.LaunchedAt)
		}
	}

	// Three live, all draining: one more fits under 2*max = 4.
	drain(2)
	drain(1)
	ids, why, err := p.ReserveWhy(ctx, 2)
	if err != nil || len(ids) != 1 {
		t.Fatalf("three live: reserved %d (%v), want 1", len(ids), err)
	}
	if !strings.Contains(why, "3 draining VMs") || !strings.Contains(why, "4 live VMs") {
		t.Fatalf("reason %q doesn't name the draining VMs holding the allowance", why)
	}
	// That reservation is pending and unlisted: it counts, so a concurrent
	// prewarm gets nothing.
	if ids, why, _ := p.ReserveWhy(ctx, 2); len(ids) != 0 || !strings.Contains(why, "twice max_instances") {
		t.Fatalf("pending launch not counted: reserved %d (%q)", len(ids), why)
	}
	if _, err := p.Provision(ctx, ids[0], nil); err != nil {
		t.Fatal(err)
	}
	// Four live (three draining, one usable), listed and in the ledger: none.
	if ids, why, _ := p.ReserveWhy(ctx, 2); len(ids) != 0 || !strings.Contains(why, "3 draining VMs") {
		t.Fatalf("four live: reserved %d (%q), want 0", len(ids), why)
	}
	if ids, _ := p.Reserve(ctx, 2); len(ids) != 0 {
		t.Fatalf("four live via Reserve: reserved %d, want 0", len(ids))
	}
	if n := listed(t, p.Fleet.(*fakeFleet)); n != 4 {
		t.Fatalf("live VMs %d, want 4", n)
	}
}

// Below the live cap, max_instances is what limits a prewarm, and the
// reason says so.
func TestReserveWhyNamesMaxInstances(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 2)
	ids, why, err := p.ReserveWhy(ctx, 3)
	if err != nil || len(ids) != 2 || why != "at max_instances (2)" {
		t.Fatalf("reserved %d, reason %q, err %v", len(ids), why, err)
	}
	if ids, why, _ := p.ReserveWhy(ctx, 1); len(ids) != 0 || why != "at max_instances (2)" {
		t.Fatalf("reserved %d, reason %q", len(ids), why)
	}
	p, _ = testPool(t, 2)
	if _, why, _ := p.ReserveWhy(ctx, 2); why != "" {
		t.Fatalf("full grant gave a reason: %q", why)
	}
}

// Two prewarms at once (separate processes sharing the ledger) at three
// live VMs get one launch between them.
func TestReserveLiveCapAcrossConcurrentCallers(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	for range 3 {
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
	pools := []*Pool{NewPool(p.R, f), NewPool(p.R, f)}
	got := make(chan int, len(pools))
	var wg sync.WaitGroup
	for _, q := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, _, err := q.ReserveWhy(ctx, 2)
			if err != nil {
				t.Error(err)
			}
			got <- len(ids)
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
