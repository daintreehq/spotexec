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

// A VM close to max_lifetime takes no new runs and stops counting as
// capacity, so waiting demand and the warm pool get a replacement: the hard
// poweroff never meets a run that started after the drain point.
func TestNearLifetimeInstancesDrain(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	p.R.MaxLifetime.Duration = 12 * time.Hour
	host := provider.Target{Host: "127.0.0.1", Port: 1}
	old := provider.Instance{ID: "i-old", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-12*time.Hour + 10*time.Minute)}
	young := provider.Instance{ID: "i-young", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-time.Hour)}
	f.insts = []provider.Instance{old, young}

	cands, usable, err := p.Candidates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID != "i-young" || usable != 1 {
		t.Fatalf("candidates %v usable %d: the ageing VM must not take new runs", cands, usable)
	}
	// The oldest-first order would otherwise have picked it first.
	if ids, _ := p.EnsureMin(ctx, 2); len(ids) != 1 {
		t.Fatalf("ageing VM not replaced in the warm pool: reserved %d", len(ids))
	}

	// Short lifetimes drain a quarter ahead.
	r := &config.Runner{}
	r.MaxLifetime.Duration = time.Hour
	if d := LifetimeDrainAhead(r); d != 15*time.Minute {
		t.Fatalf("drain ahead %s", d)
	}
	r.MaxLifetime.Duration = 80 * time.Second
	if d := LifetimeDrainAhead(r); d != 20*time.Second {
		t.Fatalf("drain ahead %s", d)
	}
	if !NearLifetime(r, provider.Instance{LaunchedAt: time.Now().Add(-61 * time.Second)}) || NearLifetime(r, provider.Instance{LaunchedAt: time.Now().Add(-59 * time.Second)}) {
		t.Fatal("NearLifetime boundary")
	}
}

// matchFleet is a fakeFleet that judges instances by type: a type in bad
// no longer matches; a type in unknown can't be judged.
type matchFleet struct {
	*fakeFleet
	bad, unknown map[string]bool
	calls        int
}

func (m *matchFleet) Mismatches(_ context.Context, insts []provider.Instance) map[string]string {
	m.calls++
	out := map[string]string{}
	for _, in := range insts {
		switch {
		case m.unknown[in.Type]:
		case m.bad[in.Type]:
			out[in.ID] = in.Type + " doesn't have 8 vCPUs, at least 32 GiB"
		default:
			out[in.ID] = ""
		}
	}
	return out
}

// Issue #99: after min_memory_gib was raised, the older 16 GiB VMs kept
// taking every run (oldest first) while the matching 64 GiB ones idled
// out. A VM that no longer satisfies the runner's requirements takes no new
// runs and isn't capacity (so it idles out and replacements launch), until
// the config matches it again.
func TestMismatchedInstancesTakeNoNewRuns(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	p.R.VCPUs, p.R.MinMemoryGiB = 8, 32
	m := &matchFleet{fakeFleet: f, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{}}
	p.Fleet = m
	host := provider.Target{Host: "127.0.0.1", Port: 1}
	f.insts = []provider.Instance{
		{ID: "i-old", Type: "c6a.2xlarge", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "i-new", Type: "r6a.2xlarge", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-time.Hour)},
	}

	cands, usable, err := p.Candidates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID != "i-new" || usable != 1 {
		t.Fatalf("candidates %v usable %d: the mismatched VM must not take new runs", cands, usable)
	}
	if ids, _ := p.EnsureMin(ctx, 2); len(ids) != 1 {
		t.Fatalf("mismatched VM not replaced in the warm pool: reserved %d", len(ids))
	}
	v, err := p.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mismatch["i-old"] == "" || v.Mismatch["i-new"] != "" || v.Draining["i-old"] {
		t.Fatalf("snapshot mismatch %v draining %v", v.Mismatch, v.Draining)
	}

	// Within the listing's TTL nothing is judged again under the same
	// requirements...
	calls := m.calls
	if _, _, err := p.Candidates(ctx, nil); err != nil || m.calls != calls {
		t.Fatalf("judged again without a relist or config change: %d -> %d (%v)", calls, m.calls, err)
	}
	// ...but a config change is: reverting it puts the VM back in service.
	p.R.MinMemoryGiB = 0
	m.bad = map[string]bool{}
	cands, usable, _ = p.Candidates(ctx, nil)
	if len(cands) != 2 || cands[0].ID != "i-old" || usable != 2 {
		t.Fatalf("after reverting the config: candidates %v usable %d", cands, usable)
	}

	// A failed lookup keeps the last verdict under the same requirements,
	// and never retires a VM under new ones.
	m.bad = map[string]bool{"c6a.2xlarge": true}
	p.R.MinMemoryGiB = 32
	if cands, _, _ = p.Candidates(ctx, nil); len(cands) != 1 {
		t.Fatalf("raised again: %v", cands)
	}
	m.unknown = map[string]bool{"c6a.2xlarge": true}
	_ = p.with(func(st *poolState) error { st.ListedAt = time.Time{}; return nil })
	if cands, _, _ = p.Candidates(ctx, nil); len(cands) != 1 {
		t.Fatalf("a failed lookup put the mismatched VM back: %v", cands)
	}
	p.R.MinMemoryGiB = 48
	if cands, _, _ = p.Candidates(ctx, nil); len(cands) != 2 {
		t.Fatalf("an unjudged VM retired under new requirements: %v", cands)
	}
}

// Clearing a mismatch never clears draining: a VM near max_lifetime stays
// out of service when the config matches it again.
func TestMismatchClearingKeepsDraining(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	p.R.MaxLifetime.Duration = 12 * time.Hour
	m := &matchFleet{fakeFleet: f, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{}}
	p.Fleet = m
	f.insts = []provider.Instance{{ID: "i-old", Type: "c6a.2xlarge", State: provider.StateRunning,
		Target: provider.Target{Host: "127.0.0.1"}, LaunchedAt: time.Now().Add(-12*time.Hour + 10*time.Minute)}}
	if cands, _, _ := p.Candidates(ctx, nil); len(cands) != 0 {
		t.Fatalf("%v", cands)
	}
	m.bad = map[string]bool{}
	p.R.MinMemoryGiB = 1
	if cands, usable, _ := p.Candidates(ctx, nil); len(cands) != 0 || usable != 0 {
		t.Fatalf("draining VM back in service: %v %d", cands, usable)
	}
}
