package client

import (
	"context"
	"errors"
	"os"
	"slices"
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
	// placement: what Launch says about where it launched (WaitRunning,
	// like a cloud listing, doesn't know).
	placement *provider.Placement
	// listErr: List fails with it; slowList: List blocks until its
	// context ends.
	listErr  error
	slowList bool
	lists    int // List calls
}

func (f *fakeFleet) Runner() *config.Runner                        { return f.r }
func (f *fakeFleet) Status(context.Context) (provider.Info, error) { return provider.Info{}, nil }
func (f *fakeFleet) Up(context.Context, provider.Logger) (provider.Info, error) {
	return provider.Info{}, nil
}
func (f *fakeFleet) Down(context.Context, provider.Logger) error { return nil }
func (f *fakeFleet) Doctor(context.Context) []provider.Check     { return nil }
func (f *fakeFleet) Forget()                                     {}

func (f *fakeFleet) List(ctx context.Context) ([]provider.Instance, error) {
	f.mu.Lock()
	f.lists++
	slow, err := f.slowList, f.listErr
	f.mu.Unlock()
	if slow {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
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
	if f.placement != nil {
		// As a cloud answers a launch: pending, no address yet, but where
		// and why.
		in.Placement, in.Zone, in.State, in.Target = f.placement, "zone-a", provider.StatePending, provider.Target{}
	}
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
// no longer matches; a type in unknown can't be judged. With regions set,
// an instance in another region doesn't match either.
type matchFleet struct {
	*fakeFleet
	bad, unknown map[string]bool
	regions      []string
	calls        int
}

func (m *matchFleet) Mismatches(_ context.Context, insts []provider.Instance) map[string]string {
	m.calls++
	out := map[string]string{}
	for _, in := range insts {
		switch {
		case m.regions != nil && !slices.Contains(m.regions, in.Region):
			out[in.ID] = in.Region + " is no longer one of the runner's regions"
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

// Issue #127: after regions were narrowed, a VM in the dropped region kept
// taking runs. Changing the set of regions judges the VMs again at once
// (within the listing's TTL); only reordering it doesn't.
func TestNarrowedRegionsJudgedAgain(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	p.R.Region, p.R.Regions = "r-one-1", []string{"r-one-1", "r-two-1"}
	m := &matchFleet{fakeFleet: f, unknown: map[string]bool{}, regions: p.R.Regions}
	p.Fleet = m
	host := provider.Target{Host: "127.0.0.1", Port: 1}
	f.insts = []provider.Instance{
		{ID: "i-two", Region: "r-two-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "i-one", Region: "r-one-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-time.Hour)},
	}
	if cands, usable, _ := p.Candidates(ctx, nil); len(cands) != 2 || usable != 2 {
		t.Fatalf("both regions configured: %v usable %d", cands, usable)
	}
	calls := m.calls
	p.R.Regions = []string{"r-two-1", "r-one-1"}
	if _, _, _ = p.Candidates(ctx, nil); m.calls != calls {
		t.Fatal("reordering regions judged the VMs again")
	}
	p.R.Regions = nil // regions = ["r-one-1"]
	m.regions = []string{"r-one-1"}
	cands, usable, _ := p.Candidates(ctx, nil)
	if len(cands) != 1 || cands[0].ID != "i-one" || usable != 1 {
		t.Fatalf("after narrowing: candidates %v usable %d", cands, usable)
	}
}

// Issue #129: launches held by the cap on live VMs (twice max_instances)
// were blamed on draining VMs when the VMs still alive were outdated ones.
// The reason counts what holds them, by the state spotexec status shows.
func TestLiveCapReasonNamesWhatHolds(t *testing.T) {
	p, f := testPool(t, 1)
	p.Fleet = &matchFleet{fakeFleet: f, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{}}
	host := provider.Target{Host: "127.0.0.1"}
	for _, id := range []string{"i-a", "i-b", "i-c"} {
		f.insts = append(f.insts, provider.Instance{ID: id, Type: "c6a.2xlarge", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-time.Hour)})
	}
	want := func(reason string) {
		t.Helper()
		if d := planOnce(t, p); d.Launch || d.Reason != reason {
			t.Fatalf("reason %q, want %q", d.Reason, reason)
		}
	}
	want("waiting for 3 outdated VMs to end: launches pause at 2 live VMs (twice max_instances)")
	// A VM both outdated and draining counts once, as draining.
	p.Observe("i-a", OutDraining, time.Now().Add(-time.Hour))
	want("waiting for 2 outdated VMs and 1 draining VM to end: launches pause at 2 live VMs (twice max_instances)")
	p.Observe("i-b", OutDraining, time.Now().Add(-time.Hour))
	p.Observe("i-c", OutDraining, time.Now().Add(-time.Hour))
	want("waiting for 3 draining VMs to end: launches pause at 2 live VMs (twice max_instances)")
}

// Clearing a mismatch never clears draining: a VM marked draining stays
// out of service when the config matches it again.
func TestMismatchClearingKeepsDraining(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	p.R.VCPUs, p.R.MinMemoryGiB = 8, 32
	m := &matchFleet{fakeFleet: f, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{}}
	p.Fleet = m
	in := provider.Instance{ID: "i-old", Type: "c6a.2xlarge", State: provider.StateRunning,
		Target: provider.Target{Host: "127.0.0.1"}, LaunchedAt: time.Now().Add(-time.Hour)}
	f.insts = []provider.Instance{in}
	if cands, _, _ := p.Candidates(ctx, nil); len(cands) != 0 {
		t.Fatalf("%v", cands)
	}
	p.Observe(in.ID, OutDraining, in.LaunchedAt)
	m.bad = map[string]bool{}
	p.R.MinMemoryGiB = 16
	if cands, usable, _ := p.Candidates(ctx, nil); len(cands) != 0 || usable != 0 {
		t.Fatalf("draining VM back in service: %v %d", cands, usable)
	}
	if n := readState(t, p).Notes["i-old"]; n == nil || !n.Draining || n.Mismatch != "" {
		t.Fatalf("note %+v: want draining, mismatch cleared", n)
	}
}

// Under the same requirements a re-listing judges again: an instance a
// failed lookup left unjudged is retired once a lookup succeeds.
func TestMismatchJudgedAgainOnRelist(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	m := &matchFleet{fakeFleet: f, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{"c6a.2xlarge": true}}
	p.Fleet = m
	f.insts = []provider.Instance{{ID: "i-old", Type: "c6a.2xlarge", State: provider.StateRunning,
		Target: provider.Target{Host: "127.0.0.1"}, LaunchedAt: time.Now().Add(-time.Hour)}}
	if cands, _, _ := p.Candidates(ctx, nil); len(cands) != 1 {
		t.Fatalf("unjudged VM retired: %v", cands)
	}
	m.unknown = map[string]bool{}
	_ = p.with(func(st *poolState) error { st.ListedAt = time.Time{}; return nil })
	if cands, usable, _ := p.Candidates(ctx, nil); len(cands) != 0 || usable != 0 {
		t.Fatalf("not judged again on a relist: %v %d", cands, usable)
	}
}

// Waiting demand gets a replacement for a mismatched VM: it isn't live
// capacity (at max_instances), nor pending capacity while it boots or
// while its launch record lasts.
func TestMismatchedInstancesAreReplacedForDemand(t *testing.T) {
	p, f := testPool(t, 1)
	m := &matchFleet{fakeFleet: f, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{}}
	p.Fleet = m
	host := provider.Target{Host: "127.0.0.1"}

	f.insts = []provider.Instance{{ID: "i-old", Type: "c6a.2xlarge", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-time.Hour)}}
	if d := planOnce(t, p); !d.Launch || d.Live != 0 {
		t.Fatalf("at max_instances with only a mismatched VM: %+v", d)
	}

	// A young, never-ready mismatched VM with a launch record.
	p2, f2 := testPool(t, 4)
	m2 := &matchFleet{fakeFleet: f2, bad: map[string]bool{"c6a.2xlarge": true}, unknown: map[string]bool{}}
	p2.Fleet = m2
	f2.insts = []provider.Instance{{ID: "i-young", Type: "c6a.2xlarge", State: provider.StateRunning, Target: host, LaunchedAt: time.Now().Add(-time.Minute)}}
	_ = p2.with(func(st *poolState) error {
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-young", Started: time.Now().Add(-time.Minute), PID: os.Getpid()}}
		return nil
	})
	if d := planOnce(t, p2); !d.Launch || d.Pending != 1 {
		t.Fatalf("mismatched booting VM counted as pending capacity: %+v", d)
	}
	// The record survives (a config change may undo the mismatch), and
	// counts again once the VM matches.
	m2.bad = map[string]bool{}
	p2.R.MinMemoryGiB = 1
	st := readState(t, p2)
	if len(st.Launches) != 2 {
		t.Fatalf("launch records %+v", st.Launches)
	}
	if d := planOnce(t, p2); d.Launch || d.Pending != 2 {
		t.Fatalf("after the config matches again: %+v", d)
	}
}

// Why a VM launched where it did survives the wait for it to run, so the
// instance_running event can say so.
func TestProvisionKeepsPlacement(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	f.placement = &provider.Placement{Order: "run_cost", Rank: 1, Of: 3, Why: "lowest expected run cost"}
	ids, _ := p.EnsureMin(ctx, 1)
	in, err := p.Provision(ctx, ids[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Placement != f.placement || in.Zone != "zone-a" || in.Target.Host == "" || in.State != provider.StateRunning {
		t.Fatalf("provisioned %+v", in)
	}
}

type namedEvents struct {
	nopRep
	mu     sync.Mutex
	fields map[string]map[string]any
}

func (e *namedEvents) Event(name string, f map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fields == nil {
		e.fields = map[string]map[string]any{}
	}
	e.fields[name] = f
}

// A launch says where it went and why as soon as it's made, and again when
// the VM runs, under the launch_id of its scale_out.
func TestProvisionEventsSayWhereAndWhy(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	f.placement = &provider.Placement{Order: "run_cost", Rank: 2, Of: 3, Why: "#2 by run cost; passed over: 1 above max_price"}
	ids, _ := p.EnsureMin(ctx, 1)
	m := &member{r: p.R, pool: p}
	rep := &namedEvents{}
	HoldLaunch()
	m.provision(ctx, ids[0], rep)
	for _, name := range []string{"instance_launched", "instance_running"} {
		ev := rep.fields[name]
		if ev == nil || ev["launch_id"] != ids[0] || ev["zone"] != "zone-a" || ev["placement"] != f.placement || ev["runner"] != "fake" {
			t.Fatalf("%s: %v", name, ev)
		}
		if _, ok := ev["region"]; ok {
			t.Fatalf("%s: empty region reported: %v", name, ev)
		}
	}
	if rep.fields["instance_running"]["host"] != "192.0.2.1" {
		t.Fatalf("instance_running: %v", rep.fields["instance_running"])
	}
}

// A process exiting right after a launch (WaitLaunches) still reports it.
func TestWaitLaunchesCoversLaunchedEvent(t *testing.T) {
	ctx := context.Background()
	p, _ := testPool(t, 2)
	ids, _ := p.EnsureMin(ctx, 1)
	release := make(chan struct{})
	inCallback := make(chan struct{})
	HoldLaunch()
	go p.ProvisionHeldThen(ctx, ids[0], nil, func(provider.Instance) {
		close(inCallback)
		<-release
	})
	<-inCallback
	waited := make(chan struct{})
	go func() { WaitLaunches(time.Minute); close(waited) }()
	select {
	case <-waited:
		t.Fatal("WaitLaunches returned before the launch was reported")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitLaunches never returned")
	}
}

// Without max_instances, demand alone decides how many VMs launch; with
// it, launches stop at the ceiling.
func TestUncappedFleetLaunchesForDemand(t *testing.T) {
	ctx := context.Background()
	launches := func(p *Pool) (int, LaunchDecision) {
		t.Helper()
		w, err := p.Register(20) // 10 VMs' worth of 2-slot demand
		if err != nil {
			t.Fatal(err)
		}
		defer w.Done()
		n := 0
		for {
			d, err := p.Plan(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Launch {
				return n, d
			}
			if n++; n > 50 {
				t.Fatal("launches never stop")
			}
		}
	}
	p, _ := testPool(t, 0)
	if n, d := launches(p); n != 10 || d.Reason != "waiting for instances being launched" {
		t.Fatalf("uncapped: %d launches, then %+v", n, d)
	}
	p, _ = testPool(t, 4)
	if n, d := launches(p); n != 4 || d.Reason != "at max_instances (4)" {
		t.Fatalf("capped: %d launches, then %+v", n, d)
	}

	p, _ = testPool(t, 0)
	if ids, err := p.EnsureMin(ctx, 12); err != nil || len(ids) != 12 {
		t.Fatalf("EnsureMin without a ceiling: %d %v", len(ids), err)
	}
	if ids, err := p.Reserve(ctx, 20); err != nil || len(ids) != 20 {
		t.Fatalf("Reserve without a ceiling: %d %v", len(ids), err)
	}
}

// A failed launch is reported, as an event and a warning, before its hold
// ends, so a process exiting (WaitLaunches) still hears of it; a VM lost
// while booting is reported too.
func TestProvisionFailureReportedWithinTheHold(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 2)
	ids, _ := p.EnsureMin(ctx, 1)
	f.mu.Lock()
	f.fail = errors.New("InsufficientInstanceCapacity")
	f.mu.Unlock()
	m := &member{r: p.R, pool: p}
	rep := &blockingWarn{namedEvents: &namedEvents{}, in: make(chan struct{}), release: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(rep.release)
		}
	})
	HoldLaunch()
	provisioned := make(chan struct{})
	go func() { m.provision(ctx, ids[0], rep); close(provisioned) }()
	select {
	case <-rep.in:
	case <-time.After(10 * time.Second):
		t.Fatal("the failed launch was never reported")
	}
	waited := make(chan struct{})
	go func() { WaitLaunches(time.Minute); close(waited) }()
	select {
	case <-waited:
		t.Fatal("WaitLaunches returned before the failed launch was reported")
	case <-time.After(100 * time.Millisecond):
	}
	close(rep.release)
	released = true
	for _, c := range []chan struct{}{waited, provisioned} {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatal("WaitLaunches or the launch never returned")
		}
	}
	ev := rep.event("launch_failed")
	if ev == nil || ev["launch_id"] != ids[0] || ev["runner"] != "fake" || ev["message"] != "InsufficientInstanceCapacity" {
		t.Fatalf("launch_failed: %v", ev)
	}
	if rep.warned() != 1 {
		t.Fatalf("%d LAUNCH_FAILED warnings", rep.warned())
	}

	// Launched, then lost while booting: reported once, after the hold.
	p2, f2 := testPool(t, 2)
	f2.goneOnWait = true
	ids2, _ := p2.EnsureMin(ctx, 1)
	rep2 := &blockingWarn{namedEvents: &namedEvents{}}
	HoldLaunch()
	(&member{r: p2.R, pool: p2}).provision(ctx, ids2[0], rep2)
	if rep2.event("instance_launched") == nil || rep2.event("launch_failed") == nil || rep2.warned() != 1 {
		t.Fatalf("lost while booting: events %v, %d warnings", rep2.fields, rep2.warned())
	}
}

// blockingWarn records events and counts LAUNCH_FAILED warnings, blocking
// in the first until released (when in is set).
type blockingWarn struct {
	*namedEvents
	in, release chan struct{}
	n           int
}

func (b *blockingWarn) Warn(code, _ string, _ ...any) {
	if code != "LAUNCH_FAILED" {
		return
	}
	b.mu.Lock()
	b.n++
	first := b.n == 1
	b.mu.Unlock()
	if first && b.in != nil {
		close(b.in)
		<-b.release
	}
}

func (b *blockingWarn) warned() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

func (b *blockingWarn) event(name string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fields[name]
}
