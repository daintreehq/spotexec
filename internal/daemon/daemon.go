// Package daemon is spotexec's optional background pool manager.
//
// sx works without it: every sx process schedules for itself and each VM
// terminates itself when idle. The daemon adds what a short-lived process
// can't do, sharing the same local pool state (pool.json, waiters, ledger):
//
//   - warm pools: keeps min_instances per fleet running (launching them and
//     refreshing their idle clock), so the first job of the day doesn't wait
//     for a boot;
//   - faster interruption recovery: an instance with a spot notice is
//     drained and replaced during its two-minute notice;
//   - health: replaces instances that stop answering or never finish
//     booting;
//   - an outside reaper: terminates instances past max_lifetime (in case a
//     VM's own watchdog failed) and leftover image-bake builders.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
)

// Timings (variables so tests can shorten them; see SPOTEXEC_DAEMON_FAST).
var (
	Interval       = 10 * time.Second
	probeEvery     = 30 * time.Second
	unhealthyAfter = 8 * time.Minute
	sweepEvery     = 10 * time.Minute
	// Warm VMs retire up to this long (a quarter of max_lifetime at most)
	// before client.LifetimeDrainAhead drains whatever is left, one at a
	// time, so their replacements are warm by then.
	retireAhead = 10 * time.Minute
	// A changed config file may be mid-save: its new contents are applied
	// only once they stay the same this long.
	reloadSettle = time.Second
	// How long the circuit breaker remembers an instance it counted (or a
	// VM this daemon launched) after it stops being listed: longer than a
	// launch can take to show up in listings.
	forgetAfter = 10 * time.Minute
)

func init() {
	if os.Getenv("SPOTEXEC_DAEMON_FAST") == "1" {
		Interval, probeEvery, unhealthyAfter, retireAhead = 2*time.Second, 4*time.Second, 25*time.Second, 25*time.Second
	}
}

// Hooks for tests: build a runner's fleet, load the global config, and tell
// a retiring VM to drain.
var (
	newFleet = func(r *config.Runner) (provider.Fleet, error) {
		p, err := provider.New(r)
		if err != nil {
			return nil, err
		}
		f, _ := p.(provider.Fleet)
		return f, nil
	}
	drainVM    = drainRemote
	probeFleet = (*Daemon).probe
	readConfig = os.ReadFile
)

// Dir holds the daemon's lock, status and log.
func Dir() string { return filepath.Join(config.StateDir(), "daemon") }

// FleetStatus is one fleet in status.json.
type FleetStatus struct {
	Runner    string           `json:"runner"`
	Min       int              `json:"min_instances"`
	Max       int              `json:"max_instances"` // 0: no ceiling
	Instances []InstanceStatus `json:"instances"`
	Demand    int              `json:"demand"`
	Launching int              `json:"launching"`
	LastError string           `json:"last_error,omitempty"`
}

// InstanceStatus is one instance as the daemon sees it.
type InstanceStatus struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	State    string `json:"state"`
	Healthy  bool   `json:"healthy"`
	Booting  bool   `json:"booting,omitempty"`
	Draining bool   `json:"draining,omitempty"`
	// Why it no longer matches the runner's config: it takes no new runs
	// and idles out.
	Mismatch string    `json:"mismatch,omitempty"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	Warm     bool      `json:"warm"`
	Launched time.Time `json:"launched_at"`
}

// Status is written to status.json every pass.
type Status struct {
	PID     int           `json:"pid"`
	Started time.Time     `json:"started"`
	Updated time.Time     `json:"updated"`
	Fleets  []FleetStatus `json:"fleets"`
	Actions []string      `json:"recent_actions"`
}

type fleet struct {
	r       *config.Runner
	f       provider.Fleet
	pool    *client.Pool
	lastOK  map[string]time.Time
	lastPrb time.Time
	lastSwp time.Time
	// Replacement pacing while silence may be our own route rather than
	// dead VMs: regionBlind (by region) when none of a region's instances
	// answered, and blind as well when no instance answered at all.
	blind       blindGate
	regionBlind map[string]*blindGate
	busy        map[string]bool // launch ids being provisioned by this daemon
	noticed     map[string]bool
	seen        map[string]time.Time         // when this daemon first saw each instance
	booting     map[string]bool              // answered, but first-boot setup unfinished
	bootFailed  map[string]bool              // its bootstrap_script failed
	counted     map[string]time.Time         // already counted by the circuit breaker (when)
	killing     map[string]string            // terminations to retry until gone (id -> reason)
	retire      map[string]provider.Instance // warm VMs to drain remotely, until confirmed
	roles       map[string]string            // placement role of each VM (client.Pool.WarmSet)
	warmPlan    []string                     // the last warm set WarmSet chose (nil: legacy)
	unprobed    map[string]bool              // skipped by the last probe round on purpose
	// Circuit breaker: replacements of instances that never became healthy.
	failures []time.Time
	paused   time.Time
	// VMs this daemon launched that haven't become usable yet, by the pass
	// that sent them, and the latest such pass that already counted a
	// failure: VMs launched together that fail together count once.
	batchOf     map[string]launchBatch
	failedBatch int
	// The breaker restarts when the runner's config changes in a way that
	// may fix it: instances launched before then (since), and launches from
	// an earlier epoch, no longer count.
	epoch int
	since time.Time
}

// blindGate paces replacements of silent instances when silence may be a
// lost route rather than dead VMs: the first silent round arms it, the
// first replacement waits another unhealthy period, then at most one per
// 30 minutes. The clock advances (spend) before the request is sent, so a
// failed request still uses up its turn.
type blindGate struct {
	at    time.Time // armed, or last replacement allowed
	kills int
}

// ready arms an unarmed gate and reports whether a replacement may go now.
func (g *blindGate) ready() bool {
	if g.at.IsZero() {
		g.at, g.kills = time.Now(), 0
		return false
	}
	wait := unhealthyAfter
	if g.kills > 0 {
		wait = 30 * time.Minute
	}
	return time.Since(g.at) >= wait
}

func (g *blindGate) spend() {
	g.at = time.Now()
	g.kills++
}

type launchBatch struct {
	pass, epoch int
	at          time.Time // recorded, or when its launch finished waiting
	pending     bool      // its launch is still waiting for the VM to run
}

func newFleetState(r *config.Runner, f provider.Fleet) *fleet {
	return &fleet{r: r, f: f, pool: client.NewPool(r, f), lastOK: map[string]time.Time{}, busy: map[string]bool{}, noticed: map[string]bool{}, seen: map[string]time.Time{}, booting: map[string]bool{}, bootFailed: map[string]bool{}, counted: map[string]time.Time{}, batchOf: map[string]launchBatch{}, killing: map[string]string{}, retire: map[string]provider.Instance{}, regionBlind: map[string]*blindGate{}}
}

// Daemon manages every fleet runner in the global config.
type Daemon struct {
	mu      sync.Mutex
	fleets  []*fleet
	started time.Time
	actions []string
	log     func(string, ...any)
	passes  int // management rounds so far (numbers launch batches)
	// The global config file, reloaded when its contents change, and the
	// contents last applied ("" path: never reloaded).
	cfgPath string
	cfgRaw  []byte
	cfgErr  string // the last reload error reported (reported once)
	// installing: instance IDs whose helper is being uploaded in the
	// background (a first contact over a slow link outlasts a probe round).
	installing sync.Map
	// installed: instance IDs whose helper this daemon installed (in case
	// the local install markers couldn't be written).
	installed sync.Map
}

// guard serialises Lock and Running, so Running's probe of daemon.lock never
// collides with a daemon taking it, and never reads a half-written PID.
func guard() func() {
	_ = os.MkdirAll(Dir(), 0o700)
	f, err := os.OpenFile(filepath.Join(Dir(), "daemon.mu"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

// Lock takes the single-instance lock (held for the daemon's lifetime).
func Lock() (*os.File, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	defer guard()()
	f, err := os.OpenFile(filepath.Join(Dir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("a spotexec daemon is already running")
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}

// Running returns the PID of a running daemon (0 if none).
func Running() int {
	if _, err := os.Stat(filepath.Join(Dir(), "daemon.lock")); err != nil {
		return 0
	}
	defer guard()()
	f, err := os.OpenFile(filepath.Join(Dir(), "daemon.lock"), os.O_RDWR, 0)
	if err != nil {
		return 0
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0 // nobody holds it
	}
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(string(trim(b[:n])))
	return pid
}

func trim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return b
}

// fleetRunners lists cfg's fleet runners in priority order.
func fleetRunners(cfg *config.Config) []*config.Runner {
	var runners []*config.Runner
	for _, n := range cfg.RunnerNames() {
		if r := cfg.Global.Runners[n]; r.Ephemeral() {
			runners = append(runners, r)
		}
	}
	sort.SliceStable(runners, func(i, j int) bool { return runners[i].Priority < runners[j].Priority })
	return runners
}

// New builds a daemon for the fleet runners in cfg (priority order). The
// global config file it came from (cfg.Global.Path) is watched: Run reloads
// it whenever its contents change.
func New(cfg *config.Config, log func(string, ...any)) (*Daemon, error) {
	d := &Daemon{started: time.Now(), log: log, cfgPath: cfg.Global.Path}
	for _, r := range fleetRunners(cfg) {
		f, err := newFleet(r)
		if err != nil {
			return nil, err
		}
		if f != nil {
			d.fleets = append(d.fleets, newFleetState(r, f))
		}
	}
	if len(d.fleets) == 0 {
		return nil, errors.New("no fleet (aws-spot or sim) runners configured")
	}
	return d, nil
}

// reload re-reads the global config when its contents changed since last
// applied, so a running daemon follows edits instead of managing (and
// rewriting pool.json's requirements for) the config it started with. A
// config that doesn't load leaves everything as it was; the error is
// reported once and the file read again next pass.
func (d *Daemon) reload(ctx context.Context) {
	if d.cfgPath == "" {
		return
	}
	raw, err := readConfig(d.cfgPath)
	if err == nil && d.cfgRaw != nil && bytes.Equal(raw, d.cfgRaw) {
		return
	}
	if err == nil {
		// Maybe mid-save: a valid prefix of the new file (or an empty one)
		// must not be applied. Changed meanwhile: try again next pass.
		select {
		case <-ctx.Done():
			return
		case <-time.After(reloadSettle):
		}
		again, err2 := readConfig(d.cfgPath)
		if err2 != nil || !bytes.Equal(raw, again) || ctx.Err() != nil {
			return
		}
	}
	if err == nil {
		var g *config.Global
		if g, err = config.ParseGlobal(d.cfgPath, raw); err == nil {
			err = d.apply(&config.Config{Global: g})
		}
	}
	if err != nil {
		if msg := err.Error(); msg != d.cfgErr {
			d.cfgErr = msg
			d.action("config reload failed, still using the previous config: %v", err)
		}
		return
	}
	d.cfgRaw, d.cfgErr = raw, ""
}

// apply switches the daemon to cfg's fleet runners. A fleet whose runner is
// unchanged carries on as it was; a changed one keeps what this daemon knows
// about its instances but gets the new settings (and a fresh circuit
// breaker: the edit may be the fix); a removed one is no longer managed (its
// VMs idle out on their own). Nothing changes unless every fleet builds.
func (d *Daemon) apply(cfg *config.Config) error {
	type change struct {
		fl *fleet
		r  *config.Runner
		f  provider.Fleet
	}
	old := map[string]*fleet{}
	for _, fl := range d.fleets {
		old[fl.r.Name] = fl
	}
	var next []*fleet
	var changes []change
	for _, r := range fleetRunners(cfg) {
		fl := old[r.Name]
		if fl != nil && reflect.DeepEqual(fl.r, r) {
			next = append(next, fl)
			delete(old, r.Name)
			continue
		}
		f, err := newFleet(r)
		if err != nil {
			return err
		}
		if f == nil {
			continue
		}
		if fl == nil {
			fl = newFleetState(r, f)
			d.action("%s: added to the config; managing it", r.Name)
		} else {
			changes = append(changes, change{fl, r, f})
			delete(old, r.Name)
		}
		next = append(next, fl)
	}
	for _, c := range changes {
		d.action("%s: config changed; applying it", c.r.Name)
		d.mu.Lock()
		if !reflect.DeepEqual(healthSettings(c.fl.r), healthSettings(c.r)) {
			// The edit may be the fix (a new image, bootstrap_script or
			// instance types): give it a fresh circuit breaker.
			c.fl.failures, c.fl.paused = nil, time.Time{}
			c.fl.epoch, c.fl.since = c.fl.epoch+1, time.Now()
		}
		c.fl.r, c.fl.f, c.fl.pool = c.r, c.f, client.NewPool(c.r, c.f)
		d.mu.Unlock()
	}
	for name := range old {
		d.action("%s: no longer a fleet runner in the config; no longer managing it", name)
	}
	d.mu.Lock()
	d.fleets = next
	d.mu.Unlock()
	return nil
}

// Run manages the fleets until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	d.action("daemon started (pid %d), managing %d fleet(s)", os.Getpid(), len(d.fleets))
	d.writeStatus(nil) // replace the previous daemon's status straight away
	for {
		d.pass(ctx)
		select {
		case <-ctx.Done():
			d.action("daemon stopping")
			// Let launches already sent to the cloud record their instance
			// IDs, so nothing is left unaccounted for.
			client.WaitLaunches(60 * time.Second)
			d.writeStatus(nil)
			return nil
		case <-time.After(Interval):
		}
	}
}

func (d *Daemon) action(format string, args ...any) {
	msg := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	d.mu.Lock()
	d.actions = append(d.actions, msg)
	if len(d.actions) > 50 {
		d.actions = d.actions[len(d.actions)-50:]
	}
	d.mu.Unlock()
	if d.log != nil {
		d.log(format, args...) // the logger adds its own timestamp
	}
}

// healthSettings is r without the settings that only size or schedule the
// fleet, or shape its jobs: changing those can't fix instances that never
// became healthy.
func healthSettings(r *config.Runner) config.Runner {
	c := *r
	c.Priority, c.MinInstances, c.MaxInstances, c.MaxSlots, c.HourlyCost = 0, 0, 0, 0, 0
	c.RemotePathSetup = ""
	c.IdleTimeout, c.MaxLifetime = config.Duration{}, config.Duration{}
	c.LaunchOrder, c.MaxPrice, c.WaitCost = "", config.Dollars{}, config.Dollars{}
	return c
}

// pass is one management round over every fleet.
func (d *Daemon) pass(ctx context.Context) {
	d.reload(ctx)
	d.passes++
	var statuses []FleetStatus
	for _, fl := range d.fleets {
		if ctx.Err() != nil {
			return
		}
		statuses = append(statuses, d.manage(ctx, fl))
	}
	d.writeStatus(statuses)
}

func (d *Daemon) manage(ctx context.Context, fl *fleet) FleetStatus {
	r := fl.r
	st := FleetStatus{Runner: r.Name, Min: r.MinInstances, Max: r.MaxInstances}
	view, err := fl.pool.Snapshot(ctx, true)
	if err != nil {
		st.LastError = err.Error()
		return st
	}
	st.Demand, st.Launching = view.Demand, view.Launching
	now := time.Now()
	listed := map[string]bool{}
	// gone: id is not listed, and the listing covered every region. While
	// a region can't be listed, absence proves nothing: what is known
	// about its VMs (counted launches, terminations in flight) is kept.
	gone := func(id string) bool { return !listed[id] && len(view.Unlisted) == 0 }
	// carried: id is a VM of a region that couldn't be listed, as last
	// seen there. Its state may be old, and its region's API isn't
	// answering: nothing is judged from that state or asked of that API
	// until the region answers again.
	unlisted := map[string]bool{}
	for _, rg := range view.Unlisted {
		unlisted[rg] = true
	}
	carriedIDs := map[string]bool{}
	carried := func(id string) bool { return carriedIDs[id] }
	for _, in := range view.Instances {
		listed[in.ID] = true
		if len(unlisted) > 0 && (in.Region == "" || unlisted[in.Region]) {
			carriedIDs[in.ID] = true
		}
		if _, ok := fl.seen[in.ID]; !ok {
			fl.seen[in.ID] = now
		}
	}
	for id := range fl.seen {
		if gone(id) {
			delete(fl.seen, id)
			delete(fl.noticed, id)
			delete(fl.bootFailed, id)
			if !view.BootFailed[id] { // still remembered: keep it counted once
				d.mu.Lock()
				delete(fl.counted, id)
				d.mu.Unlock()
			}
		}
	}
	// Forget the pacing of a region its listing shows empty, before any
	// launch, so a new VM there doesn't inherit an old clock. A region that
	// couldn't be listed (and "", while any couldn't) may still have VMs.
	present := map[string]bool{}
	for _, in := range view.Instances {
		present[in.Region] = true
	}
	for rg := range fl.regionBlind {
		if !present[rg] && !unlisted[rg] && (rg != "" || len(unlisted) == 0) {
			delete(fl.regionBlind, rg)
		}
	}
	// Launched or counted but never listed (a VM that ended before any
	// listing showed it): forgotten once listings would show it.
	d.mu.Lock()
	for id, t := range fl.counted {
		if gone(id) && !view.BootFailed[id] && time.Since(t) > forgetAfter {
			delete(fl.counted, id)
		}
	}
	for id, b := range fl.batchOf {
		// Kept while its launch may still report it failed, or an sx
		// client's bootstrap failure report for it is still to be counted
		// below, so its batch still counts once.
		if gone(id) && !b.pending && !view.BootFailed[id] && time.Since(b.at) > forgetAfter {
			delete(fl.batchOf, id)
		}
	}
	d.mu.Unlock()

	// 1. Retry terminations that haven't taken effect yet.
	for id, why := range fl.killing {
		if !listed[id] || carried(id) {
			if gone(id) {
				delete(fl.killing, id)
			}
			continue
		}
		d.terminate(ctx, fl, id, why, true)
	}

	// Running, non-draining instances; the oldest that still match the
	// config are the warm ones (ties broken by ID, as sx's placement does,
	// so both pick the same ones). A mismatched VM is still health-checked
	// but never warm, so it gets no keepalive and idles out while matching
	// ones take over the warm pool.
	running := []provider.Instance{}
	for _, in := range view.Instances {
		if in.State == provider.StateRunning && in.Target.Host != "" && !view.Draining[in.ID] && fl.killing[in.ID] == "" {
			running = append(running, in)
		}
	}
	sort.SliceStable(running, func(i, j int) bool {
		if !running[i].LaunchedAt.Equal(running[j].LaunchedAt) {
			return running[i].LaunchedAt.Before(running[j].LaunchedAt)
		}
		return running[i].ID < running[j].ID
	})
	// The placement policy picks the warm VMs (from the VMs it keeps
	// placing work on), so the manager keeps alive what sx uses: warm ones
	// first, then the rest of the retained set, then standby VMs (which
	// are left to idle out). The legacy policy packs onto the oldest VMs,
	// so then the oldest are warm.
	warmIDs, roles, err := fl.pool.WarmSet(ctx)
	if err == nil {
		fl.roles, fl.warmPlan = roles, warmIDs
	} else {
		// Keep the last plan (and its roles) rather than fall back to the
		// oldest VMs, which may be standby ones left to idle out.
		warmIDs, roles = fl.warmPlan, fl.roles
		st.LastError = "placement policy: " + err.Error()
	}
	inPlan := map[string]bool{}
	for _, id := range warmIDs {
		inPlan[id] = true
	}
	if warmIDs != nil {
		order := map[string]int{}
		for i, id := range warmIDs {
			order[id] = i
		}
		rank := func(in provider.Instance) (int, int) {
			if i, ok := order[in.ID]; ok {
				return 0, i
			}
			if roles[in.ID] == "retained" {
				return 1, 0
			}
			return 2, 0
		}
		sort.SliceStable(running, func(i, j int) bool {
			ai, aj := rank(running[i])
			bi, bj := rank(running[j])
			if ai != bi {
				return ai < bi
			}
			return aj < bj
		})
	}

	// 2. Retire a warm instance shortly before max_lifetime, while its jobs
	// can still finish, so the VM's hard stop never kills one and the
	// replacement is warm in time.
	if lt := r.MaxLifetime.Duration; lt > 0 {
		retireAt := lt - client.LifetimeDrainAhead(r) - min(retireAhead, lt/4)
		// Ready warm capacity right now (answering, not booting): a VM is
		// retired only while the others still cover the minimum, so co-born
		// warm VMs are replaced one after another, never all at once.
		ready := 0
		for _, in := range running {
			if _, ok := fl.lastOK[in.ID]; ok && !fl.booting[in.ID] && view.Mismatch[in.ID] == "" {
				ready++
			}
		}
		var keep []provider.Instance
		i := 0 // position among the matching instances
		for _, in := range running {
			if view.Mismatch[in.ID] != "" {
				keep = append(keep, in)
				continue
			}
			i++
			// One at a time: warm VMs launched together would otherwise all
			// retire in the same pass and leave no warm capacity.
			if i <= r.MinInstances && (warmIDs == nil || inPlan[in.ID]) && len(fl.retire) == 0 && ready >= r.MinInstances && time.Since(in.LaunchedAt) > retireAt {
				// Drained locally now (so the warm pool replaces it); the
				// VM itself is told below, retried until it confirms.
				d.action("%s: retiring warm instance %s ahead of max_lifetime", r.Name, in.ID)
				fl.pool.Observe(in.ID, client.OutDraining, in.LaunchedAt)
				fl.retire[in.ID] = in
				view.Draining[in.ID] = true
				continue
			}
			keep = append(keep, in)
		}
		running = keep
	}
	for id, in := range fl.retire {
		if !listed[id] || drainVM(ctx, r, in) {
			delete(fl.retire, id)
		}
	}
	// With the placement policy's plan, exactly its warm VMs; otherwise
	// (legacy) the oldest matching ones.
	warm := map[string]bool{}
	for _, in := range running {
		if len(warm) < r.MinInstances && view.Mismatch[in.ID] == "" && (warmIDs == nil || inPlan[in.ID]) {
			warm[in.ID] = true
		}
	}

	// 3. Health probes (and keepalive for warm instances). An instance with
	// a spot interruption notice is drained here.
	probed, answered := false, map[string]int{}
	if time.Since(fl.lastPrb) > probeEvery {
		fl.lastPrb = time.Now()
		var notified []string
		notified, answered = probeFleet(d, ctx, fl, running, warm)
		probed = true
		d.mu.Lock()
		for id := range fl.lastOK {
			// Usable now: no longer a failed launch. One that answers while
			// still booting, or with a failed bootstrap, keeps its batch, so
			// VMs launched together that fail together still count once.
			if !fl.booting[id] {
				delete(fl.batchOf, id)
			}
		}
		d.mu.Unlock()
		for _, id := range notified {
			view.Draining[id] = true
		}
	}
	if ctx.Err() != nil {
		return st // stopping: a cancelled probe proves nothing
	}
	if probed {
		fl.pool.RefreshPrices(ctx) // for the placement cost model (at most every 15m)
	}
	if probed && r.EarlyDrain() {
		for _, id := range d.earlyDrain(ctx, fl, running) {
			view.Draining[id] = true
		}
	}

	// 4. Replace instances that stopped answering, judged only right after
	// a probe round. When no instance answered that round it may be our own
	// network (a firewall blocking SSH, say), so then replace at most one
	// instance per 30 minutes instead of churning the fleet. Likewise for a
	// region none of whose instances answered while others did: a route to
	// one region can fail on its own (#229), and an answer from elsewhere
	// says nothing about it. Only a silent instance with answering peers in
	// its own region is replaced at once.
	if probed {
		total := 0
		for rg, n := range answered {
			total += n
			if n > 0 {
				delete(fl.regionBlind, rg)
			}
		}
		if total > 0 {
			fl.blind = blindGate{}
		}
		for _, in := range running {
			age := time.Since(in.LaunchedAt)
			// Silence counts from the last answer, or from when this daemon
			// first saw the instance (so a restart starts the clock afresh).
			since := fl.seen[in.ID]
			if last, ok := fl.lastOK[in.ID]; ok {
				since = last
			}
			if fl.unprobed[in.ID] {
				continue // left unprobed on purpose (a standby VM without our helper): silence proves nothing
			}
			if carried(in.ID) {
				continue // its region's API isn't answering: judged once it does
			}
			stale := time.Since(since) > unhealthyAfter && age > r.BootTimeout.Duration
			stuck := (fl.booting[in.ID] && age > 2*r.BootTimeout.Duration+2*time.Minute) || fl.bootFailed[in.ID]
			if !stale && !stuck {
				continue
			}
			// Nothing answers in its region: possibly our own network,
			// though the cloud API answered this pass. Paced by its region,
			// and when nothing answers at all, across the fleet too (both
			// gates armed, both spent), so switching between the two
			// never allows a burst.
			if answered[in.Region] == 0 {
				gate := fl.regionBlind[in.Region]
				if gate == nil {
					gate = &blindGate{}
					fl.regionBlind[in.Region] = gate
				}
				ok := gate.ready()
				if total == 0 {
					ok = fl.blind.ready() && ok
				}
				if !ok {
					continue
				}
				gate.spend()
				if total == 0 {
					fl.blind.spend()
				}
			}
			why := fmt.Sprintf("unresponsive for %s", time.Since(since).Round(time.Second))
			switch {
			case fl.bootFailed[in.ID]:
				why = "bootstrap_script failed (see /var/log/spotexec-bootstrap.log on the VM)"
			case !stale:
				why = "first-boot setup never finished"
			}
			// The circuit breaker counts instances that never became
			// healthy; one that served and later hung is not a setup problem.
			if _, ok := fl.lastOK[in.ID]; !ok || fl.booting[in.ID] {
				d.countFailure(fl, in.ID, in.LaunchedAt)
			}
			// Not retried if the request fails: the next probe re-judges it
			// (it may answer again once the network is back).
			if !d.terminate(ctx, fl, in.ID, why, false) {
				delete(fl.killing, in.ID)
			}
		}
	}

	// A bootstrap failure an sx client found (it drained the VM): count it
	// and end the VM now rather than letting it idle out.
	launchedAt := map[string]time.Time{}
	for _, in := range view.Instances {
		launchedAt[in.ID] = in.LaunchedAt
	}
	for id := range view.BootFailed {
		d.countFailure(fl, id, launchedAt[id]) // once per instance (counted)
		if listed[id] && !carried(id) && fl.killing[id] == "" {
			d.terminate(ctx, fl, id, "bootstrap_script failed", false)
		}
	}

	// Instances that never got an address or never left pending are not
	// usable and are invisible to the probes above.
	for _, in := range view.Instances {
		if fl.killing[in.ID] == "" && !carried(in.ID) && !in.LaunchedAt.IsZero() && time.Since(in.LaunchedAt) > r.BootTimeout.Duration+time.Minute &&
			(in.State != provider.StateRunning || in.Target.Host == "") {
			d.countFailure(fl, in.ID, in.LaunchedAt)
			d.terminate(ctx, fl, in.ID, "never became reachable (no address, or stuck pending)", false)
		}
	}

	// 5. Warm pool, after probing and replacing so a noticed instance is replaced in the
	// same pass while its two-minute notice runs. EnsureMin counts under the
	// pool lock, so launches sx planned meanwhile are included. If instances
	// keep failing to become healthy, stop launching for a while: a broken
	// bootstrap or image must not burn money in a loop.
	d.mu.Lock()
	var recent []time.Time
	for _, t := range fl.failures {
		if time.Since(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	fl.failures = recent
	d.mu.Unlock()
	if len(recent) >= 3 && time.Since(fl.paused) > time.Hour {
		fl.paused = time.Now()
		d.action("%s: %d instances failed to become healthy within an hour; pausing warm launches for 1h (check `spotexec doctor` and the bootstrap log)", r.Name, len(recent))
	}
	if time.Since(fl.paused) < time.Hour {
		st.LastError = "warm launches paused after repeated unhealthy instances"
	} else if r.MinInstances > 0 {
		if ids, err := fl.pool.EnsureMin(ctx, r.MinInstances); err == nil {
			for _, id := range ids {
				d.action("%s: launching to keep %d warm", r.Name, r.MinInstances)
				d.provision(ctx, fl, id, d.passes)
			}
		}
	}

	// 6. Outside reaper: past max_lifetime means the VM's watchdog failed.
	for _, in := range view.Instances {
		if r.MaxLifetime.Duration > 0 && !in.LaunchedAt.IsZero() && fl.killing[in.ID] == "" && !carried(in.ID) &&
			time.Since(in.LaunchedAt) > r.MaxLifetime.Duration+10*time.Minute {
			d.terminate(ctx, fl, in.ID, "past max_lifetime ("+r.MaxLifetime.Duration.String()+")", false)
		}
	}

	// 7. Leftover image-bake builders (skipped while a bake runs here).
	if a, ok := fl.f.(interface {
		SweepBuilders(context.Context, provider.Logger)
	}); ok && time.Since(fl.lastSwp) > sweepEvery {
		fl.lastSwp = time.Now()
		a.SweepBuilders(ctx, func(f string, args ...any) { d.action("%s: %s", r.Name, fmt.Sprintf(f, args...)) })
	}

	for _, in := range view.Instances {
		last, ok := fl.lastOK[in.ID]
		draining := view.Draining[in.ID] || fl.killing[in.ID] != ""
		st.Instances = append(st.Instances, InstanceStatus{
			ID: in.ID, Type: in.Type, State: string(in.State), Launched: in.LaunchedAt,
			Healthy: ok && time.Since(last) < unhealthyAfter && !fl.booting[in.ID] && !draining,
			Booting: fl.booting[in.ID], Draining: draining, Mismatch: view.Mismatch[in.ID], LastOK: last, Warm: warm[in.ID],
		})
	}
	return st
}

// terminate ends an instance, retrying on later passes until it is gone
// from listings. It is marked draining locally only once the request
// succeeds, so a failed call is never forgotten.
func (d *Daemon) terminate(ctx context.Context, fl *fleet, id, why string, retry bool) bool {
	if !retry {
		d.action("%s: terminating %s: %s", fl.r.Name, id, why)
	}
	fl.killing[id] = why
	if err := fl.f.Terminate(ctx, id); err != nil {
		if !retry {
			d.action("%s: terminating %s failed: %v", fl.r.Name, id, err)
		}
		return false
	}
	fl.pool.Observe(id, client.OutDraining, time.Time{})
	delete(fl.lastOK, id)
	return true
}

// drainRemote tells a retiring VM to drain: running jobs finish, new ones
// go elsewhere, and it powers off once empty. It reports whether the VM
// confirmed. (One that never does still stops at its own max_lifetime.)
func drainRemote(ctx context.Context, r *config.Runner, in provider.Instance) bool {
	pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	c := transport.New(in.Target, r.WorkRoot)
	if err := c.EnsureMaster(pctx); err != nil {
		return false
	}
	out, err := c.HelperCmd(pctx, "drain")
	var hs helper.Status
	return err == nil && json.Unmarshal(out, &hs) == nil && hs.Draining
}

// earlyDrain hands surplus (standby) VMs their early drains
// (scheduling.early_drain, client.Pool.EarlyDrain): the same pass every sx
// process runs as its job ends, here each probe round. It returns the VMs
// drained now.
func (d *Daemon) earlyDrain(ctx context.Context, fl *fleet, running []provider.Instance) []string {
	r := fl.r
	var drained []string
	for _, res := range fl.pool.EarlyDrain(ctx, running) {
		switch res.Result {
		case helper.DrainDone, helper.DrainAlready:
			if res.Idle >= 0 {
				d.action("%s: %s is surplus and was idle %s: drained early", r.Name, res.ID, res.Idle)
			} else {
				d.action("%s: %s is surplus: drained early", r.Name, res.ID)
			}
			drained = append(drained, res.ID)
		case helper.DrainScheduled:
			d.action("%s: %s is surplus: it ends once idle %s (early drain)", r.Name, res.ID, r.ResultRetention())
		}
	}
	return drained
}

// provision launches in the background; the daemon outlives it, so the
// launch always completes and is recorded. batch is the pass that sent it.
func (d *Daemon) provision(ctx context.Context, fl *fleet, id string, batch int) {
	d.mu.Lock()
	if fl.busy[id] {
		d.mu.Unlock()
		return
	}
	fl.busy[id] = true
	d.mu.Unlock()
	// The fleet's runner and pool as of this launch: a config reload may
	// replace them while it runs.
	r, pool, epoch := fl.r, fl.pool, fl.epoch
	if ctx.Err() != nil {
		pool.Cancel(id) // stopping: never sent, so free the reservation
		d.mu.Lock()
		delete(fl.busy, id)
		d.mu.Unlock()
		return
	}
	// Registered before the goroutine runs, so a shutdown's WaitLaunches
	// can't miss it.
	client.HoldLaunch()
	go func() {
		defer func() { d.mu.Lock(); delete(fl.busy, id); d.mu.Unlock() }()
		if ctx.Err() != nil {
			pool.Cancel(id)
			client.ReleaseLaunch()
			return
		}
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.BootTimeout.Duration+2*time.Minute)
		defer cancel()
		created := ""
		// Once the launch is done (and any failure of it counted), its
		// batch record may be forgotten like any other.
		defer func() {
			d.mu.Lock()
			if b, ok := fl.batchOf[created]; ok && created != "" {
				b.at, b.pending = time.Now(), false
				fl.batchOf[created] = b
			}
			d.mu.Unlock()
		}()
		in, err := pool.ProvisionHeldThen(pctx, id, nil, func(in provider.Instance) {
			if in.ID == "" {
				return
			}
			created = in.ID
			d.mu.Lock()
			fl.batchOf[in.ID] = launchBatch{pass: batch, epoch: epoch, at: time.Now(), pending: true}
			d.mu.Unlock()
		})
		if err != nil {
			d.action("%s: launch failed: %v", r.Name, err)
			// A refused launch request (network, credentials, capacity,
			// quota, max_price) created no VM, so it says nothing about the
			// fleet's health; the pool already backs further launches off.
			// Only a VM that was created and then never became usable counts.
			if created != "" {
				d.countFailure(fl, created, time.Time{})
			}
			return
		}
		d.action("%s: %s running at %s", r.Name, in.ID, in.Target.Host)
	}()
}

// probe checks every running instance's helper (and refreshes the idle
// clock of warm ones) in parallel. answered counts the instances that
// answered, by region.
func (d *Daemon) probe(ctx context.Context, fl *fleet, running []provider.Instance, warm map[string]bool) (notified []string, answered map[string]int) {
	answered = map[string]int{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	fl.unprobed = map[string]bool{}
	wasBooting := map[string]bool{} // read-only copy for the goroutines
	for id, b := range fl.booting {
		wasBooting[id] = b
	}
	defer func() {
		for _, id := range notified {
			if !fl.noticed[id] {
				fl.noticed[id] = true
				d.action("%s: %s is draining (spot interruption notice); replacing it", fl.r.Name, id)
			}
		}
	}()
	for _, in := range running {
		wg.Add(1)
		go func(in provider.Instance) {
			defer wg.Done()
			c := transport.New(in.Target, fl.r.WorkRoot)
			if _, done := d.installed.Load(in.ID); !done && !c.HelperInstalled() {
				if fl.roles[in.ID] == "standby" {
					// Installing touches its idle clock: a surplus VM is left
					// to idle out instead.
					mu.Lock()
					fl.unprobed[in.ID] = true
					mu.Unlock()
					return
				}
				// A first contact uploads the helper, which over a slow link
				// can outlast this round: install it in the background (once
				// per VM) and probe once it's there, so a slow upload neither
				// restarts every round nor holds up the round's other work.
				d.installHelper(ctx, c, in.ID)
				return
			}
			pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if err := c.EnsureMaster(pctx); err != nil {
				return
			}
			// Keep warm instances alive, but never one stuck in first boot.
			args := "status"
			if warm[in.ID] && !wasBooting[in.ID] {
				args = "keepalive"
			}
			out, err := c.HelperCmd(pctx, args)
			if err != nil {
				return
			}
			var hs helper.Status
			_ = json.Unmarshal(out, &hs)
			mu.Lock()
			fl.lastOK[in.ID] = time.Now()
			// A failed bootstrap never becomes usable: same as stuck booting.
			fl.booting[in.ID] = hs.Booting || hs.BootFailed != ""
			if hs.BootFailed != "" {
				fl.bootFailed[in.ID] = true
			}
			answered[in.Region]++
			mu.Unlock()
			if rc, ok := fl.f.(provider.Reclaimer); ok && hs.SpotNotice != "" {
				rc.Reclaimed(in) // its replacement goes elsewhere
			}
			if (hs.SpotNotice != "" || hs.Draining) && hs.BootFailed == "" {
				// Replace it now, during the two-minute notice, rather than
				// after the instance vanishes from listings.
				fl.pool.Observe(in.ID, client.OutDraining, in.LaunchedAt)
				mu.Lock()
				notified = append(notified, in.ID)
				mu.Unlock()
			}
		}(in)
	}
	wg.Wait()
	live := map[string]bool{}
	for _, in := range running {
		live[in.ID] = true
	}
	for id := range fl.lastOK {
		if !live[id] {
			delete(fl.lastOK, id)
		}
	}
	for id := range fl.booting {
		if !live[id] {
			delete(fl.booting, id)
		}
	}
	return notified, answered
}

func (d *Daemon) writeStatus(fleets []FleetStatus) {
	d.mu.Lock()
	st := Status{PID: os.Getpid(), Started: d.started, Updated: time.Now(), Fleets: fleets, Actions: append([]string(nil), d.actions...)}
	d.mu.Unlock()
	b, _ := json.MarshalIndent(st, "", "  ")
	p := filepath.Join(Dir(), "status.json")
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// ReadStatus loads the last status written by the daemon.
func ReadStatus() (*Status, error) {
	b, err := os.ReadFile(filepath.Join(Dir(), "status.json"))
	if err != nil {
		return nil, err
	}
	var s Status
	return &s, json.Unmarshal(b, &s)
}

// countFailure records, once, an instance that never became healthy (it was
// launched at launched, zero if unknown), for the warm-launch circuit
// breaker. VMs this daemon launched in the same pass that fail together (a
// Spot capacity reclaim, a network blip while they boot) count once, as
// does a pass older than one already counted; instances from before the
// breaker last restarted don't count.
func (d *Daemon) countFailure(fl *fleet, id string, launched time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := fl.counted[id]; ok {
		return
	}
	fl.counted[id] = time.Now()
	if !launched.IsZero() && launched.Before(fl.since) {
		return
	}
	if b, ok := fl.batchOf[id]; ok {
		if b.epoch != fl.epoch || b.pass <= fl.failedBatch {
			return
		}
		fl.failedBatch = b.pass
	}
	fl.failures = append(fl.failures, time.Now())
}

// installHelper uploads the helper to a VM in the background, unless an
// upload to it is already under way.
func (d *Daemon) installHelper(ctx context.Context, c *transport.Client, id string) {
	if _, busy := d.installing.LoadOrStore(id, true); busy {
		return
	}
	go func() {
		defer d.installing.Delete(id)
		ictx, cancel := context.WithTimeout(ctx, 25*time.Second+transport.HelperUploadBudget)
		defer cancel()
		if err := c.EnsureMaster(ictx); err != nil {
			return
		}
		if _, err := c.EnsureHelper(ictx, false); err == nil {
			d.installed.Store(id, true)
		}
	}()
}
