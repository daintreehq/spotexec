package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// cloud is the fake fleets' shared inventory: like a real cloud, it
// outlives a provider rebuilt by a config reload.
type cloud struct {
	mu        sync.Mutex
	insts     map[string][]provider.Instance // runner -> live instances
	n         int
	launchErr error // Launch refuses with this
	waitErr   error // WaitRunning fails with this (after Launch created the VM)
	built     map[string]int
	launched  []string // "runner:types" of each launch request
}

func newCloud() *cloud {
	return &cloud{insts: map[string][]provider.Instance{}, built: map[string]int{}}
}

func (c *cloud) add(runner string, in provider.Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insts[runner] = append(c.insts[runner], in)
}

// fakeFleet is an in-memory provider.Fleet (and provider.Matcher: a VM
// matches while its type is one of the runner's instance_types).
type fakeFleet struct {
	r *config.Runner
	c *cloud
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
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	return slices.Clone(f.c.insts[f.r.Name]), nil
}

func (f *fakeFleet) Launch(context.Context, provider.Logger) (provider.Instance, error) {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	f.c.launched = append(f.c.launched, f.r.Name+":"+strings.Join(f.r.InstanceTypes, ","))
	if f.c.launchErr != nil {
		return provider.Instance{}, f.c.launchErr
	}
	f.c.n++
	typ := ""
	if len(f.r.InstanceTypes) > 0 {
		typ = f.r.InstanceTypes[0]
	}
	in := provider.Instance{ID: fmt.Sprintf("i-%d", f.c.n), Type: typ, State: provider.StateRunning, LaunchedAt: time.Now(), Target: provider.Target{Host: "192.0.2.1"}}
	f.c.insts[f.r.Name] = append(f.c.insts[f.r.Name], in)
	return in, nil
}

func (f *fakeFleet) WaitRunning(_ context.Context, id string, _ provider.Logger) (provider.Instance, error) {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	if f.c.waitErr != nil {
		return provider.Instance{}, f.c.waitErr
	}
	for _, in := range f.c.insts[f.r.Name] {
		if in.ID == id {
			return in, nil
		}
	}
	return provider.Instance{}, errors.New("gone")
}

func (f *fakeFleet) Terminate(_ context.Context, id string) error {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	f.c.insts[f.r.Name] = slices.DeleteFunc(f.c.insts[f.r.Name], func(in provider.Instance) bool { return in.ID == id })
	return nil
}

func (f *fakeFleet) Mismatches(_ context.Context, insts []provider.Instance) map[string]string {
	out := map[string]string{}
	for _, in := range insts {
		if !slices.Contains(f.r.InstanceTypes, in.Type) {
			out[in.ID] = in.Type + " is no longer one of instance_types"
		} else {
			out[in.ID] = ""
		}
	}
	return out
}

// setup isolates config and state in temp dirs and installs the fake
// cloud, with probes that find every running VM healthy and drains that
// confirm (unless the test changes them).
func setup(t *testing.T) (*cloud, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("SPOTEXEC_CONFIG", cfgPath)
	t.Setenv("SPOTEXEC_STATE_DIR", filepath.Join(dir, "state"))
	c := newCloud()
	oldFleet, oldProbe, oldDrain := newFleet, probeFleet, drainVM
	t.Cleanup(func() { newFleet, probeFleet, drainVM = oldFleet, oldProbe, oldDrain })
	newFleet = func(r *config.Runner) (provider.Fleet, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.built[r.Name]++
		return &fakeFleet{r: r, c: c}, nil
	}
	probeFleet = func(d *Daemon, _ context.Context, fl *fleet, running []provider.Instance, _ map[string]bool) ([]string, int) {
		for _, in := range running {
			fl.lastOK[in.ID] = time.Now()
		}
		return nil, len(running)
	}
	drainVM = func(context.Context, *config.Runner, provider.Instance) bool { return true }
	return c, cfgPath
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func start(t *testing.T) (*Daemon, *[]string) {
	t.Helper()
	g, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	logs := &[]string{}
	d, err := New(&config.Config{Global: g}, func(f string, a ...any) {
		mu.Lock()
		*logs = append(*logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settle(t, d) })
	return d, logs
}

// settle waits for the daemon's background launches to finish.
func settle(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		busy := 0
		d.mu.Lock()
		for _, fl := range d.fleets {
			busy += len(fl.busy)
		}
		d.mu.Unlock()
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background launches never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fleetNamed(d *Daemon, name string) *fleet {
	for _, fl := range d.fleets {
		if fl.r.Name == name {
			return fl
		}
	}
	return nil
}

func count(logs []string, sub string) int {
	n := 0
	for _, l := range logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

const simRunner = `[runners.pool]
type = "sim"
instance_types = [%q]
min_instances = %d
max_instances = 4
`

// The daemon follows edits to config.toml: once sx has re-judged the pool
// under the new requirements, the daemon's passes keep them instead of
// writing the startup ones back.
func TestReloadFollowsConfigEdits(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "old.large", 0))
	c.add("pool", provider.Instance{ID: "i-old", Type: "old.large", State: provider.StateRunning, LaunchedAt: time.Now(), Target: provider.Target{Host: "192.0.2.1"}})
	d, logs := start(t)
	ctx := context.Background()
	d.pass(ctx)
	fl := fleetNamed(d, "pool")
	if c.built["pool"] != 1 {
		t.Fatalf("an unchanged config rebuilt the fleet: built %d times", c.built["pool"])
	}

	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "new.large", 1))
	g, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	r := g.Runners["pool"]
	sx := client.NewPool(r, &fakeFleet{r: r, c: c})
	if v, err := sx.Snapshot(ctx, true); err != nil || v.Mismatch["i-old"] == "" {
		t.Fatalf("sx under the new config: mismatch %v, err %v", v.Mismatch, err)
	}

	for i := 0; i < 3; i++ {
		d.pass(ctx)
		settle(t, d)
	}
	if fleetNamed(d, "pool") != fl {
		t.Error("a changed runner lost its fleet's state")
	}
	if fl.r.MinInstances != 1 || fl.r.InstanceTypes[0] != "new.large" {
		t.Errorf("daemon still runs the old runner: %+v", fl.r)
	}
	// pool.json as the daemon left it (a Snapshot under the new config
	// would re-judge it).
	var saved struct {
		MatchKey string `json:"match_key"`
		Notes    map[string]struct {
			Mismatch string `json:"mismatch"`
		} `json:"notes"`
	}
	raw, err := os.ReadFile(filepath.Join(provider.RunnerStateDir("pool"), "pool.json"))
	if err != nil || json.Unmarshal(raw, &saved) != nil {
		t.Fatalf("pool.json: %v", err)
	}
	if saved.MatchKey != provider.MatchKey(r) || saved.Notes["i-old"].Mismatch == "" {
		t.Errorf("the daemon rewrote pool.json under the old config: key %q, notes %+v", saved.MatchKey, saved.Notes)
	}
	v, err := sx.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mismatch["i-old"] == "" {
		t.Error("the daemon cleared the outdated VM's mismatch")
	}
	for id, why := range v.Mismatch {
		if id != "i-old" && why != "" {
			t.Errorf("the daemon marked new VM %s mismatched: %s", id, why)
		}
	}
	c.mu.Lock()
	launched := slices.Clone(c.launched)
	c.mu.Unlock()
	if len(launched) != 1 || launched[0] != "pool:new.large" {
		t.Errorf("warm launches = %v, want one under the new config", launched)
	}
	if count(*logs, "config changed") != 1 {
		t.Errorf("logs = %q", *logs)
	}
}

// A config that doesn't load (half-saved, invalid) leaves the daemon on the
// last good one, reported once; fixing it applies it.
func TestReloadKeepsLastGoodConfig(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 0))
	d, logs := start(t)
	ctx := context.Background()
	d.pass(ctx)

	writeConfig(t, cfgPath, "[runners.pool\ntype = ")
	d.pass(ctx)
	d.pass(ctx)
	if n := count(*logs, "config reload failed"); n != 1 {
		t.Errorf("reported the bad config %d times, want once: %q", n, *logs)
	}
	writeConfig(t, cfgPath, "[runners.pool]\ntype = \"sim\"\nbogus = 1\n")
	d.pass(ctx)
	if n := count(*logs, "config reload failed"); n != 2 {
		t.Errorf("a different error wasn't reported: %q", *logs)
	}
	if fl := fleetNamed(d, "pool"); fl == nil || fl.r.InstanceTypes[0] != "a.large" {
		t.Fatal("a bad config replaced the running one")
	}

	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "b.large", 0))
	d.pass(ctx)
	if fl := fleetNamed(d, "pool"); fl == nil || fl.r.InstanceTypes[0] != "b.large" {
		t.Fatal("the fixed config wasn't applied")
	}
	if c.built["pool"] != 2 {
		t.Errorf("fleet built %d times, want 2", c.built["pool"])
	}
}

// Runners added to or removed from the config are picked up, in priority
// order; a fleet that fails to build leaves the whole config unapplied.
func TestReloadAddsAndRemovesRunners(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 0))
	d, logs := start(t)
	ctx := context.Background()
	d.pass(ctx)
	first := fleetNamed(d, "pool")

	two := fmt.Sprintf(simRunner, "a.large", 0) + "\n[runners.fast]\ntype = \"sim\"\npriority = -1\n"
	build := newFleet
	newFleet = func(r *config.Runner) (provider.Fleet, error) {
		if r.Name == "fast" {
			return nil, errors.New("no credentials")
		}
		return build(r)
	}
	writeConfig(t, cfgPath, two)
	d.pass(ctx)
	if len(d.fleets) != 1 || count(*logs, "no credentials") != 1 {
		t.Fatalf("a failed build applied part of the config: %d fleets, logs %q", len(d.fleets), *logs)
	}

	newFleet = build
	d.pass(ctx)
	if len(d.fleets) != 2 || d.fleets[0].r.Name != "fast" || d.fleets[1] != first {
		t.Fatalf("fleets after adding: %v", names(d))
	}

	writeConfig(t, cfgPath, "[runners.fast]\ntype = \"sim\"\n")
	d.pass(ctx)
	if got := names(d); len(got) != 1 || got[0] != "fast" {
		t.Fatalf("fleets after removing: %v", got)
	}
	if c.built["pool"] != 1 {
		t.Errorf("unchanged runner rebuilt %d times", c.built["pool"])
	}

	writeConfig(t, cfgPath, "")
	d.pass(ctx)
	if len(d.fleets) != 0 {
		t.Errorf("fleets after removing all: %v", names(d))
	}
}

func names(d *Daemon) []string {
	var n []string
	for _, fl := range d.fleets {
		n = append(n, fl.r.Name)
	}
	return n
}

func recent(d *Daemon, fl *fleet) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(fl.failures)
}

// Warm launches refused together (a network blip, an expired SSO session,
// no capacity) created no VM, so they never count toward the circuit
// breaker; VMs launched together that then fail together count once.
func TestBreakerIgnoresBurstFailures(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 3))
	d, _ := start(t)
	ctx := context.Background()
	fl := fleetNamed(d, "pool")

	c.mu.Lock()
	c.launchErr = errors.New("ExpiredToken: the SSO session has expired")
	c.mu.Unlock()
	d.pass(ctx)
	settle(t, d)
	if n := len(c.launched); n != 3 {
		t.Fatalf("launch requests = %d, want 3", n)
	}
	if n := recent(d, fl); n != 0 {
		t.Errorf("refused launches counted %d failures", n)
	}

	c.mu.Lock()
	c.launchErr, c.waitErr = nil, errors.New("instance reclaimed while booting")
	c.mu.Unlock()
	clearBackoff(t, fl)
	d.pass(ctx)
	settle(t, d)
	if n := len(c.launched); n != 6 {
		t.Fatalf("launch requests = %d, want 6", n)
	}
	if n := recent(d, fl); n != 1 {
		t.Errorf("3 VMs failing together counted %d failures, want 1", n)
	}
	st := d.manage(ctx, fl)
	if !fl.paused.IsZero() || st.LastError != "" {
		t.Errorf("one burst paused warm launches: %q", st.LastError)
	}
}

// clearBackoff ends the pool's backoff after a failed launch, as time would.
func clearBackoff(t *testing.T, fl *fleet) {
	t.Helper()
	p := filepath.Join(provider.RunnerStateDir(fl.r.Name), "pool.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	delete(st, "backoff_until")
	if b, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Distinct instances failing to become healthy still trip the breaker; the
// same instance seen failing twice counts once; a later launch batch counts
// again but an earlier one doesn't.
func TestBreakerAccounting(t *testing.T) {
	_, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 1))
	d, logs := start(t)
	ctx := context.Background()
	fl := fleetNamed(d, "pool")

	d.countLaunchFailure(fl, "i-1", 5)
	d.countLaunchFailure(fl, "i-2", 5)
	d.countLaunchFailure(fl, "i-3", 4)
	d.countFailure(fl, "i-1")
	if n := recent(d, fl); n != 1 {
		t.Fatalf("failures = %d, want 1", n)
	}
	d.countLaunchFailure(fl, "i-4", 6)
	d.countFailure(fl, "i-5")
	d.countFailure(fl, "i-5")
	if n := recent(d, fl); n != 3 {
		t.Fatalf("failures = %d, want 3", n)
	}
	st := d.manage(ctx, fl)
	if fl.paused.IsZero() || !strings.Contains(st.LastError, "paused") || count(*logs, "pausing warm launches") != 1 {
		t.Fatalf("3 unhealthy instances didn't pause: %q", st.LastError)
	}
	settle(t, d)

	// An hour on, old failures expire and launches resume.
	d.mu.Lock()
	for i := range fl.failures {
		fl.failures[i] = fl.failures[i].Add(-2 * time.Hour)
	}
	d.mu.Unlock()
	fl.paused = fl.paused.Add(-2 * time.Hour)
	if st := d.manage(ctx, fl); st.LastError != "" || recent(d, fl) != 0 {
		t.Errorf("breaker didn't reset: %q, %d failures", st.LastError, recent(d, fl))
	}
}

// Editing the config (say, to fix bootstrap_script) lifts a pause.
func TestConfigChangeResetsBreaker(t *testing.T) {
	_, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 0))
	d, _ := start(t)
	ctx := context.Background()
	d.pass(ctx)
	fl := fleetNamed(d, "pool")
	for _, id := range []string{"i-1", "i-2", "i-3"} {
		d.countFailure(fl, id)
	}
	d.pass(ctx)
	if fl.paused.IsZero() {
		t.Fatal("not paused")
	}
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 0)+"bootstrap_script = \"echo fixed\"\n")
	d.pass(ctx)
	if !fl.paused.IsZero() || recent(d, fl) != 0 {
		t.Error("a config change kept the pause")
	}
}

// Warm VMs launched together retire one at a time ahead of max_lifetime:
// the second only once the first's replacement answers, so warm capacity
// never drops below min_instances.
func TestStaggeredRetirement(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, fmt.Sprintf(simRunner, "a.large", 2)+"max_lifetime = \"2h\"\n")
	d, logs := start(t)
	ctx := context.Background()
	fl := fleetNamed(d, "pool")
	// Past the retirement point (2h - drain-ahead - 10m), short of the
	// pool's own drain-ahead.
	born := time.Now().Add(-(2*time.Hour - client.LifetimeDrainAhead(fl.r) - 5*time.Minute))
	for _, id := range []string{"i-a", "i-b"} {
		c.add("pool", provider.Instance{ID: id, Type: "a.large", State: provider.StateRunning, LaunchedAt: born, Target: provider.Target{Host: "192.0.2.1"}})
	}
	var confirm sync.Mutex
	confirmed := false
	drainVM = func(context.Context, *config.Runner, provider.Instance) bool {
		confirm.Lock()
		defer confirm.Unlock()
		return confirmed
	}
	// Hold the replacement in first boot until the test lets it finish.
	booting := true
	probeFleet = func(d *Daemon, _ context.Context, fl *fleet, running []provider.Instance, _ map[string]bool) ([]string, int) {
		for _, in := range running {
			fl.lastOK[in.ID] = time.Now()
			fl.booting[in.ID] = booting && in.ID != "i-a" && in.ID != "i-b"
		}
		return nil, len(running)
	}

	retiring := func() []string {
		var ids []string
		for _, l := range *logs {
			if strings.Contains(l, "retiring warm instance") {
				ids = append(ids, l[strings.LastIndex(l, " i-")+1:strings.Index(l, " ahead")])
			}
		}
		return ids
	}
	pass := func() {
		fl.lastPrb = time.Time{} // probe every pass
		d.pass(ctx)
		settle(t, d)
	}

	// First pass: nothing answered yet, so nothing counts as ready.
	pass()
	if got := retiring(); len(got) != 0 {
		t.Fatalf("retired before any VM answered: %v", got)
	}
	// Second: both ready, exactly one retires; the VM hasn't confirmed.
	pass()
	if got := retiring(); len(got) != 1 {
		t.Fatalf("retired %v, want exactly one", got)
	}
	// Unconfirmed drain: no second retirement meanwhile.
	pass()
	if got := retiring(); len(got) != 1 {
		t.Fatalf("retired %v while the first drain was unconfirmed", got)
	}
	// Confirmed, but the replacement is still booting.
	confirm.Lock()
	confirmed = true
	confirm.Unlock()
	pass()
	pass()
	if got := retiring(); len(got) != 1 {
		t.Fatalf("retired %v before the replacement was ready", got)
	}
	c.mu.Lock()
	launches := len(c.launched)
	c.mu.Unlock()
	if launches == 0 {
		t.Fatal("no replacement launched")
	}
	// Replacement ready: now the second retires.
	booting = false
	pass()
	pass()
	if got := retiring(); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("retired %v, want both, one after the other", got)
	}
}
