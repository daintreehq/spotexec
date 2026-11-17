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
	launchErr error         // Launch refuses with this
	waitErr   error         // WaitRunning fails with this (after Launch created the VM)
	hold      chan struct{} // WaitRunning blocks until it is closed
	built     map[string]int
	launched  []string // "runner:types" of each launch request
	waited    []string // "runner:types" of the provider each WaitRunning ran on
}

func newCloud() *cloud {
	return &cloud{insts: map[string][]provider.Instance{}, built: map[string]int{}}
}

func (c *cloud) add(runner string, in provider.Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insts[runner] = append(c.insts[runner], in)
}

// born sets when every instance of runner launched.
func (c *cloud) born(runner string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.insts[runner] {
		c.insts[runner][i].LaunchedAt = at
	}
}

func (c *cloud) launches() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.launched)
}

func (c *cloud) set(fn func(c *cloud)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// fakeFleet is an in-memory provider.Fleet (and provider.Matcher: a VM
// matches while its type is one of the runner's instance_types).
type fakeFleet struct {
	r *config.Runner
	c *cloud
}

func (f *fakeFleet) key() string { return f.r.Name + ":" + strings.Join(f.r.InstanceTypes, ",") }

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
	f.c.launched = append(f.c.launched, f.key())
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
	f.c.waited = append(f.c.waited, f.key())
	hold := f.c.hold
	f.c.mu.Unlock()
	if hold != nil {
		<-hold
	}
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

// setup isolates config and state in temp dirs, pins the timings, and
// installs the fake cloud, with probes that find every running VM healthy
// and drains that confirm (unless the test changes them).
func setup(t *testing.T) (*cloud, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("SPOTEXEC_CONFIG", cfgPath)
	t.Setenv("SPOTEXEC_STATE_DIR", filepath.Join(dir, "state"))
	if err := os.MkdirAll(Dir(), 0o700); err != nil { // Lock makes it in production
		t.Fatal(err)
	}
	c := newCloud()
	oldFleet, oldProbe, oldDrain := newFleet, probeFleet, drainVM
	oldSettle, oldRetire, oldProbeEvery := reloadSettle, retireAhead, probeEvery
	t.Cleanup(func() {
		newFleet, probeFleet, drainVM = oldFleet, oldProbe, oldDrain
		reloadSettle, retireAhead, probeEvery = oldSettle, oldRetire, oldProbeEvery
	})
	reloadSettle, retireAhead, probeEvery = 0, 10*time.Minute, 30*time.Second
	newFleet = func(r *config.Runner) (provider.Fleet, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.built[r.Name]++
		return &fakeFleet{r: r, c: c}, nil
	}
	probeFleet = func(d *Daemon, _ context.Context, fl *fleet, running []provider.Instance, _ map[string]bool) ([]string, map[string]int) {
		n := map[string]int{}
		for _, in := range running {
			fl.lastOK[in.ID] = time.Now()
			n[in.Region]++
		}
		return nil, n
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

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) matching(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

func (l *logs) count(sub string) int { return len(l.matching(sub)) }

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("%q", l.lines)
}

func start(t *testing.T) (*Daemon, *logs) {
	t.Helper()
	g, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	l := &logs{}
	d, err := New(&config.Config{Global: g}, func(f string, a ...any) {
		l.mu.Lock()
		l.lines = append(l.lines, fmt.Sprintf(f, a...))
		l.mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settle(t, d) })
	return d, l
}

// settle waits for the daemon's background launches (also those of the
// given fleets, which may no longer be managed) to finish.
func settle(t *testing.T, d *Daemon, extra ...*fleet) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		busy := 0
		d.mu.Lock()
		for _, fl := range append(slices.Clone(d.fleets), extra...) {
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

func names(d *Daemon) []string {
	var n []string
	for _, fl := range d.fleets {
		n = append(n, fl.r.Name)
	}
	return n
}

const simRunner = `[runners.pool]
type = "sim"
instance_types = [%q]
min_instances = %d
max_instances = 4
`

func runnerConfig(typ string, min int, extra ...string) string {
	return fmt.Sprintf(simRunner, typ, min) + strings.Join(extra, "\n") + "\n"
}

// The daemon follows edits to config.toml: once sx has re-judged the pool
// under the new requirements, the daemon's passes keep them instead of
// writing the startup ones back.
func TestReloadFollowsConfigEdits(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("old.large", 0))
	c.add("pool", provider.Instance{ID: "i-old", Type: "old.large", State: provider.StateRunning, LaunchedAt: time.Now(), Target: provider.Target{Host: "192.0.2.1"}})
	d, l := start(t)
	ctx := context.Background()
	d.pass(ctx)
	fl := fleetNamed(d, "pool")
	if c.built["pool"] != 1 {
		t.Fatalf("an unchanged config rebuilt the fleet: built %d times", c.built["pool"])
	}
	seen := fl.seen["i-old"]
	if seen.IsZero() || fl.lastOK["i-old"].IsZero() {
		t.Fatal("the first pass didn't see the instance")
	}

	writeConfig(t, cfgPath, runnerConfig("new.large", 1))
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
		t.Error("a changed runner lost its fleet")
	}
	if !fl.seen["i-old"].Equal(seen) {
		t.Error("a config change forgot when the daemon first saw its instances")
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
	for id, n := range saved.Notes {
		if id != "i-old" && n.Mismatch != "" {
			t.Errorf("the daemon marked new VM %s mismatched: %s", id, n.Mismatch)
		}
	}
	if got := c.launches(); len(got) != 1 || got[0] != "pool:new.large" {
		t.Errorf("warm launches = %v, want one under the new config", got)
	}
	if l.count("config changed") != 1 {
		t.Errorf("logs = %s", l)
	}
}

// A launch in flight when the config changes finishes on the provider and
// pool it started with, and is recorded; later launches use the new config.
func TestReloadDuringLaunch(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 1))
	d, _ := start(t)
	ctx := context.Background()
	hold := make(chan struct{})
	c.set(func(c *cloud) { c.hold = hold })
	d.pass(ctx) // launches i-1 under a.large, which then waits on hold
	for len(c.launches()) == 0 {
		time.Sleep(time.Millisecond)
	}

	writeConfig(t, cfgPath, runnerConfig("b.large", 1))
	d.pass(ctx) // reloads while i-1's launch is still waiting
	fl := fleetNamed(d, "pool")
	if fl.r.InstanceTypes[0] != "b.large" {
		t.Fatal("not reloaded")
	}
	c.set(func(c *cloud) { c.hold = nil })
	close(hold)
	settle(t, d)
	for i := 0; i < 2; i++ {
		d.pass(ctx)
		settle(t, d)
	}

	c.mu.Lock()
	waited := slices.Clone(c.waited)
	c.mu.Unlock()
	if len(waited) == 0 || waited[0] != "pool:a.large" {
		t.Errorf("the in-flight launch waited on %v, want its original provider", waited)
	}
	if got := c.launches(); !slices.Equal(got, []string{"pool:a.large", "pool:b.large"}) {
		t.Errorf("launches = %v, want the old one then one under the new config", got)
	}
	v, err := fl.pool.Snapshot(ctx, true)
	if err != nil || v.Launching != 0 {
		t.Errorf("launches left unrecorded: %d (%v)", v.Launching, err)
	}
}

// A config that doesn't load (half-saved, invalid, missing) leaves the
// daemon on the last good one, each distinct error reported once; fixing it
// applies it.
func TestReloadKeepsLastGoodConfig(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	d, l := start(t)
	ctx := context.Background()
	d.pass(ctx)
	const failed = "config reload failed"

	writeConfig(t, cfgPath, "[runners.pool\ntype = ")
	d.pass(ctx)
	d.pass(ctx)
	if n := l.count(failed); n != 1 {
		t.Errorf("reported the bad config %d times, want once: %s", n, l)
	}
	bogus := "[runners.pool]\ntype = \"sim\"\nbogus = 1\n"
	writeConfig(t, cfgPath, bogus)
	d.pass(ctx)
	if n := l.count(failed); n != 2 {
		t.Errorf("a different error wasn't reported: %s", l)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	d.pass(ctx)
	if n := l.count(failed); n != 3 {
		t.Errorf("a missing config wasn't reported: %s", l)
	}
	if fl := fleetNamed(d, "pool"); fl == nil || fl.r.InstanceTypes[0] != "a.large" {
		t.Fatal("a bad or missing config replaced the running one")
	}

	writeConfig(t, cfgPath, runnerConfig("b.large", 0))
	d.pass(ctx)
	if fl := fleetNamed(d, "pool"); fl == nil || fl.r.InstanceTypes[0] != "b.large" {
		t.Fatal("the fixed config wasn't applied")
	}
	if c.built["pool"] != 2 {
		t.Errorf("fleet built %d times, want 2", c.built["pool"])
	}
	// After a good config, the same bad one is news again.
	writeConfig(t, cfgPath, bogus)
	d.pass(ctx)
	if n := l.count(failed); n != 4 {
		t.Errorf("a recurring error wasn't reported after recovery: %s", l)
	}
}

// A config whose save is still settling isn't applied until it is.
func TestReloadWaitsForSave(t *testing.T) {
	_, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	d, _ := start(t)
	ctx := context.Background()
	d.pass(ctx)
	// The save goes on between the reload's two reads.
	reads := 0
	readConfig = func(path string) ([]byte, error) {
		b, err := os.ReadFile(path)
		if reads++; reads == 1 {
			writeConfig(t, cfgPath, runnerConfig("c.large", 0))
		}
		return b, err
	}
	t.Cleanup(func() { readConfig = os.ReadFile })
	writeConfig(t, cfgPath, runnerConfig("b.large", 0))
	d.pass(ctx)
	if got := fleetNamed(d, "pool").r.InstanceTypes[0]; got != "a.large" {
		t.Fatalf("applied %s while the file was still changing", got)
	}
	d.pass(ctx)
	if got := fleetNamed(d, "pool").r.InstanceTypes[0]; got != "c.large" {
		t.Fatalf("applied %s, want the settled c.large", got)
	}
	// A reload cut short by shutdown applies nothing.
	writeConfig(t, cfgPath, runnerConfig("d.large", 0))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	for _, settle := range []time.Duration{0, time.Hour} {
		reloadSettle = settle
		d.reload(cctx)
		if got := fleetNamed(d, "pool").r.InstanceTypes[0]; got != "c.large" {
			t.Fatalf("applied %s after shutdown began (settle %s)", got, settle)
		}
	}
}

// Runners added to or removed from the config are picked up, in priority
// order; a fleet that fails to build leaves the whole config unapplied.
func TestReloadAddsAndRemovesRunners(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	d, l := start(t)
	ctx := context.Background()
	d.pass(ctx)
	first := fleetNamed(d, "pool")
	firstFleet, firstPool := first.f, first.pool

	// "zfast" sorts after "pool" by name but before it by priority.
	const zfast = "\n[runners.zfast]\ntype = \"sim\"\npriority = -1\n"
	build := newFleet
	newFleet = func(r *config.Runner) (provider.Fleet, error) {
		if r.Name == "zfast" {
			return nil, errors.New("no credentials")
		}
		return build(r)
	}
	writeConfig(t, cfgPath, runnerConfig("b.large", 0)+zfast)
	d.pass(ctx)
	if len(d.fleets) != 1 || l.count("no credentials") != 1 {
		t.Fatalf("a failed build applied part of the config: %v, logs %s", names(d), l)
	}
	if first.r.InstanceTypes[0] != "a.large" || first.f != firstFleet || first.pool != firstPool {
		t.Fatal("a failed build still changed the other runner")
	}

	newFleet = build
	d.pass(ctx)
	if got := names(d); !slices.Equal(got, []string{"zfast", "pool"}) || d.fleets[1] != first || first.r.InstanceTypes[0] != "b.large" {
		t.Fatalf("fleets after adding: %v", got)
	}

	writeConfig(t, cfgPath, zfast)
	d.pass(ctx)
	if got := names(d); !slices.Equal(got, []string{"zfast"}) {
		t.Fatalf("fleets after removing: %v", got)
	}
	if c.built["pool"] != 2 || c.built["zfast"] != 1 {
		t.Errorf("builds = %v", c.built)
	}

	writeConfig(t, cfgPath, "")
	d.pass(ctx)
	if len(d.fleets) != 0 {
		t.Errorf("fleets after removing all: %v", names(d))
	}
}

func recent(d *Daemon, fl *fleet) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(fl.failures)
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

// age moves the breaker's clock back by dt.
func age(d *Daemon, fl *fleet, dt time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range fl.failures {
		fl.failures[i] = fl.failures[i].Add(-dt)
	}
	fl.paused = fl.paused.Add(-dt)
}

// Warm launches refused together (a network blip, an expired SSO session,
// no capacity) created no VM, so they never count toward the circuit
// breaker; VMs launched together that then fail together count once per
// pass, so it takes three such passes to pause, and the pause holds for an
// hour.
func TestBreakerIgnoresBurstFailures(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 3))
	d, l := start(t)
	ctx := context.Background()
	fl := fleetNamed(d, "pool")
	pass := func() FleetStatus {
		t.Helper()
		clearBackoff(t, fl)
		d.pass(ctx)
		settle(t, d)
		st, err := ReadStatus()
		if err != nil || len(st.Fleets) != 1 {
			t.Fatalf("status: %v", err)
		}
		return st.Fleets[0]
	}

	c.set(func(c *cloud) { c.launchErr = errors.New("ExpiredToken: the SSO session has expired") })
	d.pass(ctx)
	settle(t, d)
	if n := len(c.launches()); n != 3 {
		t.Fatalf("launch requests = %d, want 3", n)
	}
	if n := recent(d, fl); n != 0 {
		t.Errorf("refused launches counted %d failures", n)
	}

	c.set(func(c *cloud) { c.launchErr, c.waitErr = nil, errors.New("instance reclaimed while booting") })
	for want := 1; want <= 2; want++ {
		if st := pass(); st.LastError != "" {
			t.Fatalf("paused after %d failed passes: %q", want, st.LastError)
		}
		if n := recent(d, fl); n != want {
			t.Fatalf("after %d passes of 3 VMs failing together: %d failures", want, n)
		}
	}
	if n := len(c.launches()); n != 9 {
		t.Fatalf("launch requests = %d, want 9", n)
	}
	pass()
	if st := pass(); !strings.Contains(st.LastError, "paused") || l.count("pausing warm launches") != 1 {
		t.Fatalf("3 failed passes didn't pause: %q", st.LastError)
	}
	if n := len(c.launches()); n != 12 {
		t.Fatalf("launched while paused: %d requests, want 12", n)
	}

	c.set(func(c *cloud) { c.waitErr = nil })
	age(d, fl, 59*time.Minute)
	if st := pass(); !strings.Contains(st.LastError, "paused") || len(c.launches()) != 12 {
		t.Fatalf("pause ended early: %q, %d launches", st.LastError, len(c.launches()))
	}
	age(d, fl, 2*time.Minute)
	if st := pass(); st.LastError != "" || len(c.launches()) != 15 || recent(d, fl) != 0 {
		t.Fatalf("pause didn't end after an hour: %q, %d launches, %d failures", st.LastError, len(c.launches()), recent(d, fl))
	}
}

// Each instance counts once whichever path reports it; launch batches count
// once each, older ones not at all; instances and launches from before the
// breaker restarted don't count.
func TestBreakerAccounting(t *testing.T) {
	_, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	d, _ := start(t)
	fl := fleetNamed(d, "pool")
	now := time.Now()
	for id, pass := range map[string]int{"i-1": 5, "i-2": 5, "i-3": 4, "i-4": 6} {
		fl.batchOf[id] = launchBatch{pass: pass, at: now}
	}
	steps := []struct {
		id   string
		want int
	}{
		{"i-1", 1}, // pass 5
		{"i-2", 1}, // pass 5 again
		{"i-3", 1}, // an older pass
		{"i-1", 1}, // reported twice
		{"i-4", 2}, // pass 6
		{"i-5", 3}, // not a launch of this daemon
		{"i-5", 3},
	}
	for _, s := range steps {
		d.countFailure(fl, s.id, time.Time{})
		if n := recent(d, fl); n != s.want {
			t.Fatalf("after %s: %d failures, want %d", s.id, n, s.want)
		}
	}
	// Reported by a health check first, then by its launch.
	d.countFailure(fl, "i-6", time.Time{})
	fl.batchOf["i-6"] = launchBatch{pass: 9, at: now}
	d.countFailure(fl, "i-6", time.Time{})
	if n := recent(d, fl); n != 4 {
		t.Fatalf("i-6 counted twice: %d", n)
	}

	// A restarted breaker ignores what came before it.
	fl.failures, fl.epoch, fl.since = nil, 1, now
	fl.batchOf["i-7"] = launchBatch{pass: 20, epoch: 0, at: now}
	d.countFailure(fl, "i-7", time.Time{})
	d.countFailure(fl, "i-8", now.Add(-time.Minute))
	if n := recent(d, fl); n != 0 {
		t.Fatalf("pre-restart failures counted: %d", n)
	}
	d.countFailure(fl, "i-9", now.Add(time.Minute))
	if n := recent(d, fl); n != 1 {
		t.Fatalf("post-restart failure not counted: %d", n)
	}

	// Long-unlisted entries are forgotten; recent ones are kept.
	fl.counted["i-old"] = now.Add(-2 * forgetAfter)
	fl.batchOf["i-old"] = launchBatch{at: now.Add(-2 * forgetAfter)}
	fl.lastPrb = time.Now()
	d.manage(context.Background(), fl)
	if _, ok := fl.counted["i-old"]; ok {
		t.Error("an old unlisted counted entry was kept")
	}
	if _, ok := fl.batchOf["i-old"]; ok {
		t.Error("an old unlisted launch batch was kept")
	}
	if _, ok := fl.counted["i-9"]; !ok {
		t.Error("a recent counted entry was dropped")
	}
}

// A config edit that may fix the fleet (a new bootstrap_script) lifts a
// pause and launches resume; one that only schedules or shapes jobs
// (priority, remote_path_setup) doesn't.
func TestConfigChangeResetsBreaker(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 1))
	d, _ := start(t)
	ctx := context.Background()
	fl := fleetNamed(d, "pool")
	for _, id := range []string{"i-1", "i-2", "i-3"} {
		d.countFailure(fl, id, time.Time{})
	}
	d.pass(ctx)
	settle(t, d)
	if fl.paused.IsZero() || len(c.launches()) != 0 {
		t.Fatal("not paused")
	}
	writeConfig(t, cfgPath, runnerConfig("a.large", 1, "priority = 3", `remote_path_setup = "export PATH=/opt/bin:$PATH"`))
	d.pass(ctx)
	settle(t, d)
	if fl.r.Priority != 3 || fl.r.RemotePathSetup == "" || fl.paused.IsZero() || len(c.launches()) != 0 {
		t.Fatal("a priority or remote_path_setup change lifted the pause")
	}
	writeConfig(t, cfgPath, runnerConfig("a.large", 1, "priority = 3", `remote_path_setup = "export PATH=/opt/bin:$PATH"`, `bootstrap_script = "echo fixed"`))
	d.pass(ctx)
	settle(t, d)
	if !fl.paused.IsZero() || recent(d, fl) != 0 || len(c.launches()) != 1 {
		t.Errorf("a bootstrap_script change kept the pause: %d launches", len(c.launches()))
	}
}

// retirementFleet is a fleet of two co-born warm VMs (min_instances = 2,
// max_lifetime = 2h) launched ago; booting reports whether a replacement
// VM is still in first boot. pass runs one management round with a probe.
func retirementFleet(t *testing.T, ago time.Duration, booting func() bool) (c *cloud, l *logs, pass func()) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 2, `max_lifetime = "2h"`))
	for _, id := range []string{"i-a", "i-b"} {
		c.add("pool", provider.Instance{ID: id, Type: "a.large", State: provider.StateRunning, LaunchedAt: time.Now().Add(-ago), Target: provider.Target{Host: "192.0.2.1"}})
	}
	probeFleet = func(d *Daemon, _ context.Context, fl *fleet, running []provider.Instance, _ map[string]bool) ([]string, map[string]int) {
		n := map[string]int{}
		for _, in := range running {
			fl.lastOK[in.ID] = time.Now()
			fl.booting[in.ID] = in.ID != "i-a" && in.ID != "i-b" && booting()
			n[in.Region]++
		}
		return nil, n
	}
	d, l := start(t)
	pass = func() {
		fleetNamed(d, "pool").lastPrb = time.Time{} // probe every pass
		d.pass(context.Background())
		settle(t, d)
	}
	return c, l, pass
}

// retireWindow is an age past the retirement point of a 2h max_lifetime
// (2h - drain-ahead - retireAhead) but short of the pool's own drain-ahead.
func retireWindow() time.Duration {
	r := &config.Runner{MaxLifetime: config.Duration{Duration: 2 * time.Hour}}
	return 2*time.Hour - client.LifetimeDrainAhead(r) - retireAhead/2
}

// Warm VMs retire ahead of max_lifetime only once in the window, and one at
// a time: the second waits for the first's drain to be confirmed, even
// with its replacement ready.
func TestRetirementWaitsForDrainConfirmation(t *testing.T) {
	c, l, pass := retirementFleet(t, time.Hour, func() bool { return false })
	var mu sync.Mutex
	confirmed := false
	drainVM = func(context.Context, *config.Runner, provider.Instance) bool {
		mu.Lock()
		defer mu.Unlock()
		return confirmed
	}
	pass()
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 0 {
		t.Fatalf("retired young VMs: %v", got)
	}
	c.born("pool", time.Now().Add(-retireWindow()))
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 1 {
		t.Fatalf("retired %v, want exactly one", got)
	}
	// Its replacement launches and answers; the drain stays unconfirmed.
	pass()
	pass()
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 1 {
		t.Fatalf("retired %v while the first drain was unconfirmed", got)
	}
	if n := len(c.launches()); n != 1 {
		t.Fatalf("replacements launched = %d, want 1", n)
	}
	mu.Lock()
	confirmed = true
	mu.Unlock()
	pass()
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("retired %v, want both, one after the other", got)
	}
}

// With drains confirmed, the second co-born VM still waits until the first
// one's replacement is ready, so warm capacity never drops below
// min_instances.
func TestRetirementWaitsForReplacement(t *testing.T) {
	var mu sync.Mutex
	booting := true
	c, l, pass := retirementFleet(t, retireWindow(), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return booting
	})
	// First pass: nothing answered yet, so nothing counts as ready.
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 0 {
		t.Fatalf("retired before any VM answered: %v", got)
	}
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 1 {
		t.Fatalf("retired %v, want exactly one", got)
	}
	pass()
	pass()
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 1 {
		t.Fatalf("retired %v before the replacement was ready", got)
	}
	if n := len(c.launches()); n != 1 {
		t.Fatalf("replacements launched = %d, want 1", n)
	}
	mu.Lock()
	booting = false
	mu.Unlock()
	pass()
	pass()
	if got := l.matching("retiring warm instance"); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("retired %v, want both, one after the other", got)
	}
}

// unlistedFleet is a fleet whose ap-south-1 doesn't answer: it lists the
// other regions, names that one, and records what it was asked to end.
type unlistedFleet struct {
	*fakeFleet
	terminated []string
}

func (f *unlistedFleet) ListReachable(ctx context.Context) ([]provider.Instance, map[string]error, error) {
	insts, err := f.List(ctx)
	insts = slices.DeleteFunc(insts, func(in provider.Instance) bool { return in.Region == "ap-south-1" })
	return insts, map[string]error{"ap-south-1": errors.New("no answer")}, err
}

func (f *unlistedFleet) Terminate(ctx context.Context, id string) error {
	f.terminated = append(f.terminated, id)
	return f.fakeFleet.Terminate(ctx, id)
}

// Issue #241: while a region can't be listed, a VM's absence from the
// view proves nothing, so the manager forgets nothing for it; a listing
// of every region then forgets as before.
func TestManageForgetsNothingWhileARegionIsUnlisted(t *testing.T) {
	_, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	d, _ := start(t)
	fl := fleetNamed(d, "pool")
	fl.lastPrb = time.Now()
	d.manage(context.Background(), fl)

	ff := fl.pool.Fleet.(*fakeFleet)
	fl.pool.Fleet = &unlistedFleet{fakeFleet: ff}
	old := time.Now().Add(-2 * forgetAfter)
	fl.counted["i-old"] = old
	fl.batchOf["i-old"] = launchBatch{at: old}
	fl.killing["i-old"] = "test"
	fl.seen["i-old"] = old
	d.manage(context.Background(), fl)
	_, counted := fl.counted["i-old"]
	_, batch := fl.batchOf["i-old"]
	_, killing := fl.killing["i-old"]
	_, seen := fl.seen["i-old"]
	if !counted || !batch || !killing || !seen {
		t.Fatalf("forgotten while a region was unlisted: counted %v batch %v killing %v seen %v", counted, batch, killing, seen)
	}

	fl.pool.Fleet = ff
	d.manage(context.Background(), fl)
	_, counted = fl.counted["i-old"]
	_, batch = fl.batchOf["i-old"]
	_, killing = fl.killing["i-old"]
	_, seen = fl.seen["i-old"]
	if counted || batch || killing || seen {
		t.Fatalf("kept after a full listing: counted %v batch %v killing %v seen %v", counted, batch, killing, seen)
	}
}

// A VM of a region that can't be listed is known only as it was last
// seen: the manager neither judges it from that state (a VM seen pending
// may have booted since) nor asks that region's API to end it, until the
// region answers again.
func TestManageLeavesUnlistedRegionsVMsAlone(t *testing.T) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	c.add("pool", provider.Instance{ID: "i-south", Type: "a.large", Region: "ap-south-1", State: provider.StatePending,
		LaunchedAt: time.Now()})
	c.add("pool", provider.Instance{ID: "i-kill", Type: "a.large", Region: "ap-south-1", State: provider.StateRunning,
		LaunchedAt: time.Now(), Target: provider.Target{Host: "192.0.2.1"}})
	c.born("pool", time.Now().Add(-2*time.Hour))
	d, _ := start(t)
	fl := fleetNamed(d, "pool")
	fl.lastPrb = time.Now()
	// Listed pending while it could still be booting.
	boot := fl.r.BootTimeout.Duration
	fl.r.BootTimeout.Duration = 3 * time.Hour
	d.manage(context.Background(), fl)
	fl.r.BootTimeout.Duration = boot

	// The region stops answering; the VM booted meanwhile, but is now old
	// enough that a VM still pending would be ended.
	// And both VMs are past max_lifetime, which the reaper would end.
	uf := &unlistedFleet{fakeFleet: fl.pool.Fleet.(*fakeFleet)}
	fl.pool.Fleet, fl.f = uf, uf
	fl.r.MaxLifetime.Duration = time.Hour
	fl.killing["i-kill"] = "test"
	d.manage(context.Background(), fl)
	if len(uf.terminated) != 0 {
		t.Fatalf("ended VMs of a region that couldn't be listed: %v", uf.terminated)
	}
	if fl.killing["i-kill"] == "" {
		t.Fatal("a termination in flight was forgotten")
	}
}

// regionFleet is three running VMs in us-east-1 and one in us-west-2, all
// launched an hour ago and last heard from 15 minutes ago. The probe hears
// only from the VMs whose region is up (and not in down). pass runs one
// management round with a probe.
func regionFleet(t *testing.T) (c *cloud, fl *fleet, up map[string]bool, down map[string]bool, pass func()) {
	c, cfgPath := setup(t)
	writeConfig(t, cfgPath, runnerConfig("a.large", 0))
	for _, in := range []struct{ id, region string }{{"i-e1", "us-east-1"}, {"i-e2", "us-east-1"}, {"i-e3", "us-east-1"}, {"i-w1", "us-west-2"}} {
		c.add("pool", provider.Instance{ID: in.id, Type: "a.large", Region: in.region, State: provider.StateRunning,
			LaunchedAt: time.Now().Add(-time.Hour), Target: provider.Target{Host: "192.0.2.1"}})
	}
	up = map[string]bool{"us-east-1": true, "us-west-2": true}
	down = map[string]bool{}
	var mu sync.Mutex
	probeFleet = func(d *Daemon, _ context.Context, fl *fleet, running []provider.Instance, _ map[string]bool) ([]string, map[string]int) {
		mu.Lock()
		defer mu.Unlock()
		n := map[string]int{}
		for _, in := range running {
			if up[in.Region] && !down[in.ID] {
				fl.lastOK[in.ID] = time.Now()
				n[in.Region]++
			}
		}
		return nil, n
	}
	d, _ := start(t)
	fl = fleetNamed(d, "pool")
	for _, id := range []string{"i-e1", "i-e2", "i-e3", "i-w1"} {
		fl.lastOK[id] = time.Now().Add(-15 * time.Minute)
	}
	pass = func() {
		t.Helper()
		fl.lastPrb = time.Time{} // probe every pass
		d.manage(context.Background(), fl)
		settle(t, d)
	}
	return c, fl, up, down, pass
}

func (c *cloud) ids(runner string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, in := range c.insts[runner] {
		out = append(out, in.ID)
	}
	return out
}

// Issue #267: a region whose VMs all stop answering while another region
// answers is a lost route, not three dead VMs. One pass must not end them
// all: that region is paced as a fleet with no answers is, one replacement
// an unhealthy period after the silence was noticed, then one per 30
// minutes, and the answering region is never touched.
func TestSilentRegionIsNotEmptiedByAnotherRegionsAnswers(t *testing.T) {
	c, fl, up, _, pass := regionFleet(t)
	up["us-east-1"] = false
	pass()
	if got := c.ids("pool"); len(got) != 4 {
		t.Fatalf("one pass ended VMs of a silent region while another answered: left %v", got)
	}
	pass()
	if got := c.ids("pool"); len(got) != 4 {
		t.Fatalf("replaced before the silent region's grace period ended: left %v", got)
	}
	gate := fl.regionBlind["us-east-1"]
	if gate == nil || fl.regionBlind["us-west-2"] != nil || !fl.blind.at.IsZero() {
		t.Fatalf("gates: east %v, west %v, fleet-wide %v; want only east's armed", gate, fl.regionBlind["us-west-2"], fl.blind)
	}
	gate.at = gate.at.Add(-unhealthyAfter)
	pass()
	if got := c.ids("pool"); len(got) != 3 || !slices.Contains(got, "i-w1") {
		t.Fatalf("after the grace period: left %v, want one east VM ended", got)
	}
	pass()
	gate.at = gate.at.Add(-29 * time.Minute)
	pass()
	if got := c.ids("pool"); len(got) != 3 {
		t.Fatalf("a second replacement within 30 minutes: left %v", got)
	}
	gate.at = gate.at.Add(-time.Minute)
	pass()
	if got := c.ids("pool"); len(got) != 2 || !slices.Contains(got, "i-w1") {
		t.Fatalf("after 30 more minutes: left %v, want a second east VM ended", got)
	}
}

// When the silent region's route comes back its pacing ends, and a later
// silence starts a fresh grace period instead of resuming the old cadence.
func TestSilentRegionRecoveryResetsItsPacing(t *testing.T) {
	c, fl, up, _, pass := regionFleet(t)
	up["us-east-1"] = false
	pass()
	fl.regionBlind["us-east-1"].at = time.Now().Add(-time.Hour)
	fl.regionBlind["us-east-1"].kills = 1
	up["us-east-1"] = true
	pass()
	if g := fl.regionBlind["us-east-1"]; g != nil {
		t.Fatalf("pacing kept after the region answered again: %+v", g)
	}
	for _, id := range []string{"i-e1", "i-e2", "i-e3"} {
		if time.Since(fl.lastOK[id]) > time.Minute {
			t.Fatalf("%s not healthy after the region answered again", id)
		}
		fl.lastOK[id] = time.Now().Add(-15 * time.Minute)
	}
	up["us-east-1"] = false
	pass()
	if got := c.ids("pool"); len(got) != 4 {
		t.Fatalf("a new silence resumed the old cadence: left %v", got)
	}
	if g := fl.regionBlind["us-east-1"]; g == nil || g.kills != 0 || time.Since(g.at) > time.Minute {
		t.Fatalf("a new silence didn't arm a fresh grace period: %+v", g)
	}
}

// A VM that stops answering while its region's other VMs answer is dead,
// not cut off: it is replaced in the same pass.
func TestSilentVMWithAnsweringRegionPeersIsReplacedAtOnce(t *testing.T) {
	c, fl, _, down, pass := regionFleet(t)
	down["i-e2"] = true
	pass()
	if got := c.ids("pool"); len(got) != 3 || slices.Contains(got, "i-e2") {
		t.Fatalf("left %v, want only i-e2 ended", got)
	}
	if len(fl.regionBlind) != 0 || !fl.blind.at.IsZero() {
		t.Fatalf("paced a VM with answering peers: regional %v, fleet-wide %+v", fl.regionBlind, fl.blind)
	}
}

// With no answers from any region the fleet-wide pacing holds as before:
// one replacement per period across the whole fleet, not one per region.
func TestFleetWideSilencePacesAcrossRegions(t *testing.T) {
	c, fl, up, _, pass := regionFleet(t)
	up["us-east-1"], up["us-west-2"] = false, false
	pass()
	if got := c.ids("pool"); len(got) != 4 {
		t.Fatalf("one pass with no answers ended VMs: left %v", got)
	}
	fl.blind.at = fl.blind.at.Add(-unhealthyAfter)
	pass()
	if got := c.ids("pool"); len(got) != 3 {
		t.Fatalf("after the grace period with no answers: left %v, want 3", got)
	}
	if len(fl.regionBlind) != 0 {
		t.Fatalf("regional pacing used while no region answered: %v", fl.regionBlind)
	}
}

// A region's pacing is kept while a listing misses some region (its VMs
// may still be there) and dropped once a full listing shows it has none.
func TestRegionPacingForgottenOnceTheRegionIsEmpty(t *testing.T) {
	_, fl, _, _, pass := regionFleet(t)
	fl.regionBlind["ap-south-1"] = &blindGate{at: time.Now()}
	ff := fl.pool.Fleet.(*fakeFleet)
	uf := &unlistedFleet{fakeFleet: ff}
	fl.pool.Fleet, fl.f = uf, uf
	pass()
	if fl.regionBlind["ap-south-1"] == nil {
		t.Fatal("pacing of a region forgotten while it couldn't be listed")
	}
	fl.pool.Fleet, fl.f = ff, ff
	pass()
	if g := fl.regionBlind["ap-south-1"]; g != nil {
		t.Fatalf("pacing of an empty region kept after a full listing: %+v", g)
	}
}
