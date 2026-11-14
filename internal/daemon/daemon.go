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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	retireAhead    = 15 * time.Minute // drain warm VMs this long before max_lifetime
)

func init() {
	if os.Getenv("SPOTEXEC_DAEMON_FAST") == "1" {
		Interval, probeEvery, unhealthyAfter, retireAhead = 2*time.Second, 4*time.Second, 25*time.Second, 25*time.Second
	}
}

// Dir holds the daemon's lock, status and log.
func Dir() string { return filepath.Join(config.StateDir(), "daemon") }

// FleetStatus is one fleet in status.json.
type FleetStatus struct {
	Runner    string           `json:"runner"`
	Min       int              `json:"min_instances"`
	Max       int              `json:"max_instances"`
	Instances []InstanceStatus `json:"instances"`
	Demand    int              `json:"demand"`
	Launching int              `json:"launching"`
	LastError string           `json:"last_error,omitempty"`
}

// InstanceStatus is one instance as the daemon sees it.
type InstanceStatus struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	State    string    `json:"state"`
	Healthy  bool      `json:"healthy"`
	Booting  bool      `json:"booting,omitempty"`
	Draining bool      `json:"draining,omitempty"`
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
	r          *config.Runner
	f          provider.Fleet
	pool       *client.Pool
	lastOK     map[string]time.Time
	lastPrb    time.Time
	lastSwp    time.Time
	blind      time.Time // last replacement made while no instance answered
	blindKills int
	busy       map[string]bool // launch ids being provisioned by this daemon
	noticed    map[string]bool
	seen       map[string]time.Time         // when this daemon first saw each instance
	booting    map[string]bool              // answered, but first-boot setup unfinished
	bootFailed map[string]bool              // its bootstrap_script failed
	counted    map[string]bool              // already counted by the circuit breaker
	killing    map[string]string            // terminations to retry until gone (id -> reason)
	retire     map[string]provider.Instance // warm VMs to drain remotely, until confirmed
	// Circuit breaker: replacements of instances that never became healthy.
	failures []time.Time
	paused   time.Time
}

// Daemon manages every fleet runner in the global config.
type Daemon struct {
	mu      sync.Mutex
	fleets  []*fleet
	started time.Time
	actions []string
	log     func(string, ...any)
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

// New builds a daemon for the fleet runners in cfg (priority order).
func New(cfg *config.Config, log func(string, ...any)) (*Daemon, error) {
	d := &Daemon{started: time.Now(), log: log}
	names := cfg.RunnerNames()
	var runners []*config.Runner
	for _, n := range names {
		if r := cfg.Global.Runners[n]; r.Ephemeral() {
			runners = append(runners, r)
		}
	}
	sort.SliceStable(runners, func(i, j int) bool { return runners[i].Priority < runners[j].Priority })
	for _, r := range runners {
		p, err := provider.New(r)
		if err != nil {
			return nil, err
		}
		f, ok := p.(provider.Fleet)
		if !ok {
			continue
		}
		d.fleets = append(d.fleets, &fleet{r: r, f: f, pool: client.NewPool(r, f), lastOK: map[string]time.Time{}, busy: map[string]bool{}, noticed: map[string]bool{}, seen: map[string]time.Time{}, booting: map[string]bool{}, bootFailed: map[string]bool{}, counted: map[string]bool{}, killing: map[string]string{}, retire: map[string]provider.Instance{}})
	}
	if len(d.fleets) == 0 {
		return nil, errors.New("no fleet (aws-spot or sim) runners configured")
	}
	return d, nil
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

// pass is one management round over every fleet.
func (d *Daemon) pass(ctx context.Context) {
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
	for _, in := range view.Instances {
		listed[in.ID] = true
		if _, ok := fl.seen[in.ID]; !ok {
			fl.seen[in.ID] = now
		}
	}
	for id := range fl.seen {
		if !listed[id] {
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

	// 1. Retry terminations that haven't taken effect yet.
	for id, why := range fl.killing {
		if !listed[id] {
			delete(fl.killing, id)
			continue
		}
		d.terminate(ctx, fl, id, why, true)
	}

	// Running, non-draining instances; the oldest are the warm ones (ties
	// broken by ID, as sx's placement does, so both pick the same ones).
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

	// 2. Retire a warm instance shortly before max_lifetime, while its jobs
	// can still finish, so the VM's hard stop never kills one and the
	// replacement is warm in time.
	if lt := r.MaxLifetime.Duration; lt > 2*retireAhead {
		// Ready warm capacity right now (answering, not booting): a VM is
		// retired only while the others still cover the minimum, so co-born
		// warm VMs are replaced one after another, never all at once.
		ready := 0
		for _, in := range running {
			if _, ok := fl.lastOK[in.ID]; ok && !fl.booting[in.ID] {
				ready++
			}
		}
		var keep []provider.Instance
		for i, in := range running {
			// One at a time: warm VMs launched together would otherwise all
			// retire in the same pass and leave no warm capacity.
			if i < r.MinInstances && len(fl.retire) == 0 && ready >= r.MinInstances && time.Since(in.LaunchedAt) > lt-retireAhead {
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
		if !listed[id] || d.drainRemote(ctx, fl, in) {
			delete(fl.retire, id)
		}
	}
	warm := map[string]bool{}
	for i := 0; i < len(running) && i < r.MinInstances; i++ {
		warm[running[i].ID] = true
	}

	// 3. Health probes (and keepalive for warm instances). An instance with
	// a spot interruption notice is drained here.
	probed, answered := false, 0
	if time.Since(fl.lastPrb) > probeEvery {
		fl.lastPrb = time.Now()
		var notified []string
		notified, answered = d.probe(ctx, fl, running, warm)
		probed = true
		for _, id := range notified {
			view.Draining[id] = true
		}
	}
	if ctx.Err() != nil {
		return st // stopping: a cancelled probe proves nothing
	}

	// 4. Replace instances that stopped answering, judged only right after
	// a probe round. When no instance answered that round it may be our own
	// network (a firewall blocking SSH, say), so then replace at most one
	// instance per 30 minutes instead of churning the fleet.
	if probed {
		if answered > 0 {
			fl.blind, fl.blindKills = time.Time{}, 0
		}
		for _, in := range running {
			age := time.Since(in.LaunchedAt)
			// Silence counts from the last answer, or from when this daemon
			// first saw the instance (so a restart starts the clock afresh).
			since := fl.seen[in.ID]
			if last, ok := fl.lastOK[in.ID]; ok {
				since = last
			}
			stale := time.Since(since) > unhealthyAfter && age > r.BootTimeout.Duration
			stuck := (fl.booting[in.ID] && age > 2*r.BootTimeout.Duration+2*time.Minute) || fl.bootFailed[in.ID]
			if !stale && !stuck {
				continue
			}
			if answered == 0 {
				// Nothing answers: possibly our own network (though the cloud
				// API answered this pass). The first such round arms a clock;
				// the first replacement waits another unhealthy period, then
				// at most one per 30 minutes.
				if fl.blind.IsZero() {
					fl.blind = time.Now()
					fl.blindKills = 0
					continue
				}
				wait := unhealthyAfter
				if fl.blindKills > 0 {
					wait = 30 * time.Minute
				}
				if time.Since(fl.blind) < wait {
					continue
				}
				fl.blind = time.Now()
				fl.blindKills++
			} else {
				fl.blind = time.Time{}
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
				d.countFailure(fl, in.ID)
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
	for id := range view.BootFailed {
		d.countFailure(fl, id) // once per instance (counted)
		if listed[id] && fl.killing[id] == "" {
			d.terminate(ctx, fl, id, "bootstrap_script failed", false)
		}
	}

	// Instances that never got an address or never left pending are not
	// usable and are invisible to the probes above.
	for _, in := range view.Instances {
		if fl.killing[in.ID] == "" && !in.LaunchedAt.IsZero() && time.Since(in.LaunchedAt) > r.BootTimeout.Duration+time.Minute &&
			(in.State != provider.StateRunning || in.Target.Host == "") {
			d.countFailure(fl, in.ID)
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
				d.provision(ctx, fl, id)
			}
		}
	}

	// 6. Outside reaper: past max_lifetime means the VM's watchdog failed.
	for _, in := range view.Instances {
		if r.MaxLifetime.Duration > 0 && !in.LaunchedAt.IsZero() && fl.killing[in.ID] == "" &&
			time.Since(in.LaunchedAt) > r.MaxLifetime.Duration+10*time.Minute {
			d.terminate(ctx, fl, in.ID, "past max_lifetime ("+r.MaxLifetime.Duration.String()+")", false)
		}
	}

	// 7. Leftover image-bake builders (skipped while a bake runs here).
	if a, ok := fl.f.(*provider.AWSSpot); ok && time.Since(fl.lastSwp) > sweepEvery {
		fl.lastSwp = time.Now()
		a.SweepBuilders(ctx, func(f string, args ...any) { d.action("%s: %s", r.Name, fmt.Sprintf(f, args...)) })
	}

	for _, in := range view.Instances {
		last, ok := fl.lastOK[in.ID]
		draining := view.Draining[in.ID] || fl.killing[in.ID] != ""
		st.Instances = append(st.Instances, InstanceStatus{
			ID: in.ID, Type: in.Type, State: string(in.State), Launched: in.LaunchedAt,
			Healthy: ok && time.Since(last) < unhealthyAfter && !fl.booting[in.ID] && !draining,
			Booting: fl.booting[in.ID], Draining: draining, LastOK: last, Warm: warm[in.ID],
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
func (d *Daemon) drainRemote(ctx context.Context, fl *fleet, in provider.Instance) bool {
	pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	c := transport.New(in.Target, fl.r.WorkRoot)
	if err := c.EnsureMaster(pctx); err != nil {
		return false
	}
	out, err := c.HelperCmd(pctx, "drain")
	var hs helper.Status
	return err == nil && json.Unmarshal(out, &hs) == nil && hs.Draining
}

// provision launches in the background; the daemon outlives it, so the
// launch always completes and is recorded.
func (d *Daemon) provision(ctx context.Context, fl *fleet, id string) {
	d.mu.Lock()
	if fl.busy[id] {
		d.mu.Unlock()
		return
	}
	fl.busy[id] = true
	d.mu.Unlock()
	if ctx.Err() != nil {
		fl.pool.Cancel(id) // stopping: never sent, so free the reservation
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
			fl.pool.Cancel(id)
			client.ReleaseLaunch()
			return
		}
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fl.r.BootTimeout.Duration+2*time.Minute)
		defer cancel()
		in, err := fl.pool.ProvisionHeld(pctx, id, nil)
		if err != nil {
			d.action("%s: launch failed: %v", fl.r.Name, err)
			d.countFailure(fl, "") // a launch that never became usable
			return
		}
		d.action("%s: %s running at %s", fl.r.Name, in.ID, in.Target.Host)
	}()
}

// probe checks every running instance's helper (and refreshes the idle
// clock of warm ones) in parallel.
func (d *Daemon) probe(ctx context.Context, fl *fleet, running []provider.Instance, warm map[string]bool) (notified []string, answered int) {
	var wg sync.WaitGroup
	var mu sync.Mutex
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
			pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			c := transport.New(in.Target, fl.r.WorkRoot)
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
			answered++
			mu.Unlock()
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

// countFailure records one instance (or launch) that never became healthy,
// once, for the warm-launch circuit breaker.
func (d *Daemon) countFailure(fl *fleet, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id != "" {
		if fl.counted[id] {
			return
		}
		fl.counted[id] = true
	}
	fl.failures = append(fl.failures, time.Now())
}
