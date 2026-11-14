package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// fakeFleet is an in-memory Fleet.
type fakeFleet struct {
	r       *config.Runner
	mu      sync.Mutex
	insts   []provider.Instance
	n       int
	fail    error
	termErr error
	// slowRunning: WaitRunning blocks until its context ends.
	slowRunning bool
	// goneOnWait: WaitRunning reports the instance gone (reclaimed).
	goneOnWait bool
}

func (f *fakeFleet) Runner() *config.Runner                        { return f.r }
func (f *fakeFleet) Status(context.Context) (provider.Info, error) { return provider.Info{}, nil }
func (f *fakeFleet) Up(context.Context, provider.Logger) (provider.Info, error) {
	return provider.Info{}, nil
}
func (f *fakeFleet) Down(context.Context, provider.Logger) error { return nil }
func (f *fakeFleet) Doctor(context.Context) []provider.Check     { return nil }
func (f *fakeFleet) Forget()                                     {}

func (f *fakeFleet) List(context.Context) ([]provider.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Instance(nil), f.insts...), nil
}

func (f *fakeFleet) Launch(context.Context, provider.Logger) (provider.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return provider.Instance{}, f.fail
	}
	f.n++
	in := provider.Instance{ID: "i-" + string(rune('a'+f.n)), State: provider.StateRunning, LaunchedAt: time.Now(), Target: provider.Target{Host: "192.0.2.1"}}
	f.insts = append(f.insts, in)
	return in, nil
}

func (f *fakeFleet) WaitRunning(ctx context.Context, id string, _ provider.Logger) (provider.Instance, error) {
	if f.slowRunning {
		<-ctx.Done()
		return provider.Instance{}, ctx.Err()
	}
	if f.goneOnWait {
		return provider.Instance{}, errors.New("instance reclaimed while booting")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.insts {
		if in.ID == id {
			return in, nil
		}
	}
	return provider.Instance{}, errors.New("gone")
}

func (f *fakeFleet) Terminate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.termErr != nil {
		return f.termErr
	}
	for i, in := range f.insts {
		if in.ID == id {
			f.insts = append(f.insts[:i], f.insts[i+1:]...)
			break
		}
	}
	return nil
}

func testPool(t *testing.T, max int) (*Pool, *fakeFleet) {
	t.Helper()
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "fake", MaxInstances: max, MaxSlots: 2}
	r.BootTimeout.Duration = 10 * time.Minute
	f := &fakeFleet{r: r}
	return NewPool(r, f), f
}

func TestEnsureMinCountsPendingAndListed(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 4)

	ids, err := p.EnsureMin(ctx, 2)
	if err != nil || len(ids) != 2 {
		t.Fatalf("first EnsureMin: %v %v", ids, err)
	}
	// The two reservations are pending: no more until they resolve.
	if ids, _ := p.EnsureMin(ctx, 2); len(ids) != 0 {
		t.Fatalf("pending launches not counted: reserved %d more", len(ids))
	}
	for _, id := range ids {
		if _, err := p.Provision(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Now listed (and also still in the ledger): never double counted.
	if ids, _ := p.EnsureMin(ctx, 2); len(ids) != 0 {
		t.Fatalf("listed launches double counted: reserved %d more", len(ids))
	}
	if ids, _ := p.EnsureMin(ctx, 3); len(ids) != 1 {
		t.Fatalf("raising the minimum should reserve exactly one, got %d", len(ids))
	}
}

func TestEnsureMinRespectsMaxDrainingAndBackoff(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)

	if ids, _ := p.EnsureMin(ctx, 5); len(ids) != 2 {
		t.Fatalf("max_instances not enforced: %d", len(ids))
	}

	// A draining instance doesn't count as usable: it gets replaced.
	p2, f2 := testPool(t, 3)
	ids, _ := p2.EnsureMin(ctx, 1)
	in, err := p2.Provision(ctx, ids[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	p2.Observe(in.ID, OutDraining, in.LaunchedAt)
	if ids, _ := p2.EnsureMin(ctx, 1); len(ids) != 1 {
		t.Fatalf("draining instance not replaced: %d", len(ids))
	}
	_ = f2

	// A failed launch backs off further warm launches.
	f.fail = errors.New("InsufficientInstanceCapacity")
	p3, f3 := testPool(t, 3)
	f3.fail = f.fail
	ids, _ = p3.EnsureMin(ctx, 1)
	if _, err := p3.Provision(ctx, ids[0], nil); err == nil {
		t.Fatal("expected launch failure")
	}
	if ids, _ := p3.EnsureMin(ctx, 1); len(ids) != 0 {
		t.Fatalf("no backoff after a failed launch: reserved %d", len(ids))
	}
}

func TestEnsureMinRespectsDrainingCap(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	// Three draining instances: total cap (2*max = 4) leaves room for one.
	for i := 0; i < 3; i++ {
		ids, _ := p.Reserve(ctx, 1)
		in, err := p.Provision(ctx, ids[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Observe(in.ID, OutDraining, in.LaunchedAt)
	}
	_ = f
	if ids, _ := p.EnsureMin(ctx, 2); len(ids) != 1 {
		t.Fatalf("total cap not enforced per reservation: reserved %d", len(ids))
	}
}

func TestAmbiguousLaunchExpiresWhileProcessLives(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 1)
	f.fail = provider.ErrLaunchAmbiguous
	ids, _ := p.EnsureMin(ctx, 1)
	_, _ = p.Provision(ctx, ids[0], nil)
	// Age the reservation past listLagGrace; this process is still alive.
	_ = p.with(func(st *poolState) error {
		for i := range st.Launches {
			st.Launches[i].Started = time.Now().Add(-listLagGrace - time.Minute)
			st.Launches[i].Ambiguous = time.Now().Add(-listLagGrace - time.Minute)
		}
		st.BackoffUntil = time.Time{}
		return nil
	})
	f.fail = nil
	if ids, _ := p.EnsureMin(ctx, 1); len(ids) != 1 {
		t.Fatalf("ambiguous reservation never expired: reserved %d", len(ids))
	}
}
