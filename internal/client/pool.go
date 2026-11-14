package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/provider"
)

// Pool is the local view of one autoscaled fleet runner, shared by every sx
// process on this machine through pool.json guarded by pool.lock. See
// docs/POOL.md for the algorithm.
type Pool struct {
	R     *config.Runner
	Fleet provider.Fleet
	dir   string
}

const (
	listTTL       = 8 * time.Second  // cloud listing freshness
	suspectFor    = 30 * time.Second // skip an unreachable instance this long
	busyHintFor   = 1500 * time.Millisecond
	launchBackoff = 20 * time.Second // after a failed launch
	listLagGrace  = 5 * time.Minute  // a launched instance may be missing from listings this long
	orphanGrace   = 3 * time.Minute  // a launch whose provisioner died before recording an ID
)

type note struct {
	Failures     int       `json:"failures,omitempty"`
	Draining     bool      `json:"draining,omitempty"`
	SuspectUntil time.Time `json:"suspect_until,omitempty"`
	BusyUntil    time.Time `json:"busy_until,omitempty"`
	LastUsed     time.Time `json:"last_used,omitempty"`
	Ready        bool      `json:"ready,omitempty"`
	ReadyAt      time.Time `json:"ready_at,omitempty"`
	SawFull      bool      `json:"saw_full,omitempty"` // observed with every slot taken
	BootFailed   bool      `json:"boot_failed,omitempty"`
	BootFailedAt time.Time `json:"boot_failed_at,omitempty"`
}

type launchRec struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instance_id,omitempty"`
	Started    time.Time `json:"started"`
	PID        int       `json:"pid"`
	Failed     bool      `json:"failed,omitempty"`
	Ambiguous  time.Time `json:"ambiguous,omitempty"` // when RunInstances' outcome became unknown
}

type poolState struct {
	Instances     []provider.Instance `json:"instances"`
	ListedAt      time.Time           `json:"listed_at"`
	Launches      []launchRec         `json:"launches"`
	Notes         map[string]*note    `json:"notes"`
	BackoffUntil  time.Time           `json:"backoff_until,omitempty"`
	LastLaunchErr string              `json:"last_launch_error,omitempty"`
}

// NewPool wraps a fleet runner.
func NewPool(r *config.Runner, f provider.Fleet) *Pool {
	return &Pool{R: r, Fleet: f, dir: provider.RunnerStateDir(r.Name)}
}

// SlotsPerInstance is the planning capacity of one instance.
func (p *Pool) SlotsPerInstance() int { return slotsOf(p.R) }

// with runs fn under pool.lock with the current state, saving it afterwards.
func (p *Pool) with(fn func(st *poolState) error) error {
	return p.withCtx(context.Background(), fn)
}

// withCtx is with, but gives up waiting for the lock when ctx ends (another
// process may hold it during a slow cloud listing).
func (p *Pool) withCtx(ctx context.Context, fn func(st *poolState) error) error {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(p.dir, "pool.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	st := &poolState{Notes: map[string]*note{}}
	orig, _ := os.ReadFile(filepath.Join(p.dir, "pool.json"))
	if len(orig) > 0 {
		_ = json.Unmarshal(orig, st)
		if st.Notes == nil {
			st.Notes = map[string]*note{}
		}
	}
	if err := fn(st); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if bytes.Equal(b, orig) {
		return nil // unchanged: skip the write (lots of read-only callers)
	}
	tmp := filepath.Join(p.dir, "pool.json.tmp"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(p.dir, "pool.json"))
}

// readyGrace: how long after an instance is first seen ready its launch
// still counts as pending capacity.
const readyGrace = 20 * time.Second

func (n *note) markReady(now time.Time) {
	if !n.Ready {
		n.Ready, n.ReadyAt = true, now
	}
}

func readyFor(n *note) time.Duration {
	if n == nil || !n.Ready {
		return 0
	}
	if n.ReadyAt.IsZero() {
		return readyGrace + time.Second
	}
	return time.Since(n.ReadyAt)
}

func (st *poolState) note(id string) *note {
	n := st.Notes[id]
	if n == nil {
		n = &note{}
		st.Notes[id] = n
	}
	return n
}

// refresh re-lists the fleet when the cached listing is stale (or force),
// reconciling notes and the launch ledger. Called with the lock held, so one
// process refreshes while the others wait and reuse its result.
func (p *Pool) refresh(ctx context.Context, st *poolState, force bool) error {
	if !force && time.Since(st.ListedAt) < listTTL {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	insts, err := p.Fleet.List(lctx)
	if err != nil && force {
		// A forced refresh (status, the pool manager) wants the truth, not
		// a cached view it might act on.
		return err
	}
	if err != nil {
		// Expired or missing credentials won't fix themselves: say so now
		// instead of waiting out the queue on a cached view.
		var ce *provider.CredentialError
		if errors.As(err, &ce) {
			return err
		}
		if !st.ListedAt.IsZero() {
			// Serve the cached view and back off, rather than letting every
			// waiting agent retry the API (throttling) or give up.
			st.ListedAt = time.Now().Add(-listTTL + 5*time.Second)
			return nil
		}
		return err
	}
	seenBefore := map[string]bool{}
	for _, in := range st.Instances {
		seenBefore[in.ID] = true
	}
	for id, n := range st.Notes {
		if n.Ready {
			seenBefore[id] = true
		}
	}
	st.Instances, st.ListedAt = insts, time.Now()
	live := map[string]bool{}
	for _, in := range insts {
		live[in.ID] = true
	}
	for id, n := range st.Notes {
		// A bootstrap failure is remembered for an hour after its VM is
		// gone: it says the runner's setup is broken, not just that VM.
		if !live[id] && !(n.BootFailed && time.Since(n.BootFailedAt) < time.Hour) {
			delete(st.Notes, id)
		}
	}
	var keep []launchRec
	for _, l := range st.Launches {
		age := time.Since(l.Started)
		switch {
		case l.Failed:
		case age > p.R.BootTimeout.Duration+time.Minute:
			// Never became usable: give up counting it as pending capacity.
		case l.InstanceID != "" && !live[l.InstanceID] && (age > listLagGrace || seenBefore[l.InstanceID]):
			// Launched but gone: reclaimed while booting, or it already
			// served and has since terminated (not listing lag).
		case l.InstanceID != "" && st.Notes[l.InstanceID] != nil && st.Notes[l.InstanceID].Draining:
			// Draining: no longer pending capacity.
		case l.InstanceID != "" && st.Notes[l.InstanceID] != nil && st.Notes[l.InstanceID].Ready &&
			(st.Notes[l.InstanceID].SawFull || readyFor(st.Notes[l.InstanceID]) > readyGrace):
			// Usable for a while; it now counts as a normal instance. (Not at
			// the first acquisition: its other slots are still capacity that
			// waiting agents will take within a polling interval.)
		case l.InstanceID == "" && ((!pidAlive(l.PID) && age > orphanGrace) || (!l.Ambiguous.IsZero() && time.Since(l.Ambiguous) > listLagGrace)):
			// Provisioner died before RunInstances returned; if an instance
			// was created it will appear in listings and be counted there.
		default:
			keep = append(keep, l)
		}
	}
	st.Launches = keep
	return nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Candidates returns running instances worth probing, oldest first:
// packing work onto the oldest instances deterministically lets the newest
// ones go idle and terminate (scale-in). usable is the number of running,
// non-draining instances (the size of the probe window).
func (p *Pool) Candidates(ctx context.Context, exclude map[string]bool) (out []provider.Instance, usable int, err error) {
	err = p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		now := time.Now()
		for _, in := range st.Instances {
			n := st.Notes[in.ID]
			if in.State != provider.StateRunning || in.Target.Host == "" || exclude[in.ID] {
				continue
			}
			if n == nil || !n.Draining {
				usable++
			}
			if n != nil && (n.Draining || now.Before(n.SuspectUntil) || now.Before(n.BusyUntil)) {
				continue
			}
			out = append(out, in)
		}
		sort.SliceStable(out, func(i, j int) bool {
			if !out[i].LaunchedAt.Equal(out[j].LaunchedAt) {
				return out[i].LaunchedAt.Before(out[j].LaunchedAt)
			}
			return out[i].ID < out[j].ID
		})
		return nil
	})
	return out, usable, err
}

// Outcome of probing an instance.
type Outcome int

const (
	OutAcquired Outcome = iota
	OutBusy
	OutBooting
	OutDraining
	OutUnreachable
	OutBootFailed // its bootstrap_script failed: draining, and a fleet problem
)

// Observe records a probe result.
func (p *Pool) Observe(id string, o Outcome, launchedAt time.Time) {
	_ = p.with(func(st *poolState) error {
		n := st.note(id)
		now := time.Now()
		switch o {
		case OutAcquired:
			n.markReady(now)
			n.LastUsed, n.SuspectUntil, n.Failures = now, time.Time{}, 0
		case OutBusy:
			n.markReady(now)
			n.SawFull = true
			n.BusyUntil, n.SuspectUntil, n.Failures = now.Add(busyHintFor), time.Time{}, 0
		case OutDraining:
			n.Draining = true
		case OutBootFailed:
			n.Draining, n.BootFailed = true, true
			if n.BootFailedAt.IsZero() {
				n.BootFailedAt = now
			}
		case OutBooting:
			n.BusyUntil = now.Add(2 * time.Second)
		case OutUnreachable:
			// A young instance is probably still booting sshd; an older one is
			// suspect (but still counts toward capacity: unreachable is not
			// absent).
			n.Failures++
			if time.Since(launchedAt) < p.R.BootTimeout.Duration || n.Failures < 2 {
				n.BusyUntil = now.Add(3 * time.Second)
			} else {
				n.SuspectUntil = now.Add(suspectFor)
			}
		}
		return nil
	})
}

// LaunchDecision is returned by Plan.
type LaunchDecision struct {
	Launch   bool
	ID       string
	Reason   string
	Pending  int // launches in flight
	Live     int // usable instances (not draining)
	Demand   int
	Capacity int // pending slots
}

// Plan decides, under the pool lock, whether this process should launch one
// more instance: only if waiting demand (minus `covered`, the capacity that
// higher-priority fleets are already launching) exceeds the capacity this
// fleet is launching, and the fleet is below max_instances.
func (p *Pool) Plan(ctx context.Context, covered int) (LaunchDecision, error) {
	var d LaunchDecision
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		d.Demand = p.waiterDemand() - covered
		listed := map[string]bool{}
		usable, total := 0, 0
		for _, in := range st.Instances {
			listed[in.ID] = true
			total++
			if n := st.Notes[in.ID]; n == nil || !n.Draining {
				usable++
			}
		}
		slots := p.SlotsPerInstance()
		inLedger := map[string]bool{}
		for _, l := range st.Launches {
			if l.Failed {
				continue
			}
			if l.InstanceID != "" {
				inLedger[l.InstanceID] = true
				if n := st.Notes[l.InstanceID]; n != nil && (n.Draining || (n.Ready && (n.SawFull || readyFor(n) > readyGrace))) {
					// Dying, or already serving (full, or ready a while):
					// not capacity a waiting agent can count on.
					continue
				}
			}
			if l.InstanceID == "" || !listed[l.InstanceID] {
				usable++
				total++
			}
			d.Pending++
			d.Capacity += slots
		}
		// Instances still booting (not yet seen ready) are pending capacity
		// too, even if their launcher's ledger entry was lost.
		for _, in := range st.Instances {
			n := st.Notes[in.ID]
			// Only never-ready instances count here: a ready one is already
			// serving (its launch-grace capacity, if any, is in the ledger).
			if inLedger[in.ID] || (n != nil && (n.Ready || n.Draining)) {
				continue
			}
			if time.Since(in.LaunchedAt) < p.R.BootTimeout.Duration {
				d.Pending++
				d.Capacity += slots
			}
		}
		d.Live = usable
		bootFailed := ""
		for id, n := range st.Notes {
			if n.BootFailed && time.Since(n.BootFailedAt) < time.Hour {
				bootFailed = id
			}
		}
		switch {
		case bootFailed != "":
			// Another VM would run the same broken setup.
			d.Reason = "bootstrap_script failed on " + bootFailed + " (fix it, or wait an hour before new launches)"
		case d.Demand <= 0:
			d.Reason = "demand covered by higher-priority runners"
		case d.Demand <= d.Capacity:
			d.Reason = "waiting for instances being launched"
		case usable >= p.R.MaxInstances:
			d.Reason = fmt.Sprintf("at max_instances (%d)", p.R.MaxInstances)
		case total >= 2*p.R.MaxInstances:
			d.Reason = "too many draining instances still alive"
		case time.Now().Before(st.BackoffUntil):
			d.Reason = "backing off after a failed launch: " + st.LastLaunchErr
		default:
			var b [6]byte
			_, _ = rand.Read(b[:])
			d.Launch, d.ID = true, hex.EncodeToString(b[:])
			st.Launches = append(st.Launches, launchRec{ID: d.ID, Started: time.Now(), PID: os.Getpid()})
			d.Pending++
			d.Capacity += slots
		}
		return nil
	})
	return d, err
}

// Reserve records up to n launches regardless of waiting demand (spotexec
// up), still respecting max_instances. It returns the launch IDs.
func (p *Pool) Reserve(ctx context.Context, n int) ([]string, error) {
	var ids []string
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, true); err != nil {
			return err
		}
		listed := map[string]bool{}
		usable := 0
		for _, in := range st.Instances {
			listed[in.ID] = true
			if nt := st.Notes[in.ID]; nt == nil || !nt.Draining {
				usable++
			}
		}
		for _, l := range st.Launches {
			if !l.Failed && (l.InstanceID == "" || !listed[l.InstanceID]) {
				usable++
			}
		}
		for i := 0; i < n && usable < p.R.MaxInstances; i++ {
			var b [6]byte
			_, _ = rand.Read(b[:])
			id := hex.EncodeToString(b[:])
			st.Launches = append(st.Launches, launchRec{ID: id, Started: time.Now(), PID: os.Getpid()})
			ids = append(ids, id)
			usable++
		}
		return nil
	})
	return ids, err
}

// EnsureMin reserves launches to bring the fleet up to min usable instances
// (warm pools), computed atomically under the pool lock, so a launch planned
// concurrently by an sx process counts. It honours launch backoff and the
// cap on draining instances like Plan does.
func (p *Pool) EnsureMin(ctx context.Context, min int) ([]string, error) {
	var ids []string
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		listed := map[string]bool{}
		usable, total := 0, 0
		for _, in := range st.Instances {
			listed[in.ID] = true
			total++
			if nt := st.Notes[in.ID]; nt == nil || !nt.Draining {
				usable++
			}
		}
		for _, l := range st.Launches {
			if !l.Failed && (l.InstanceID == "" || !listed[l.InstanceID]) {
				usable++
				total++
			}
		}
		if time.Now().Before(st.BackoffUntil) || total >= 2*p.R.MaxInstances {
			return nil
		}
		for usable < min && usable < p.R.MaxInstances && total < 2*p.R.MaxInstances {
			var b [6]byte
			_, _ = rand.Read(b[:])
			id := hex.EncodeToString(b[:])
			st.Launches = append(st.Launches, launchRec{ID: id, Started: time.Now(), PID: os.Getpid()})
			ids = append(ids, id)
			usable++
			total++
		}
		return nil
	})
	return ids, err
}

// Cancel abandons a planned launch that was never sent.
func (p *Pool) Cancel(id string) {
	_ = p.with(func(st *poolState) error {
		for i := range st.Launches {
			if st.Launches[i].ID == id && st.Launches[i].InstanceID == "" {
				st.Launches[i].Failed = true
			}
		}
		return nil
	})
}

// Reset forgets all local pool state (after `spotexec down`).
func (p *Pool) Reset() {
	_ = p.with(func(st *poolState) error {
		*st = poolState{Notes: map[string]*note{}}
		return nil
	})
}

// launchesInFlight tracks Provision calls until RunInstances has returned
// and its instance ID is recorded, so a process does not exit in between.
var launchesInFlight sync.WaitGroup

// WaitLaunches blocks (up to max) until in-flight launches are recorded.
func WaitLaunches(max time.Duration) {
	done := make(chan struct{})
	go func() { launchesInFlight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(max):
	}
}

// Provision launches the instance planned under id and records it. It runs
// outside the pool lock; failures free the ledger entry and back off.
func (p *Pool) Provision(ctx context.Context, id string, log provider.Logger) (provider.Instance, error) {
	HoldLaunch()
	return p.ProvisionHeld(ctx, id, log)
}

// HoldLaunch registers a launch about to start (so WaitLaunches waits for
// it) before the goroutine that runs it is even scheduled. Pair it with
// ProvisionHeld, or ReleaseLaunch if the launch is abandoned.
func HoldLaunch() { launchesInFlight.Add(1) }

// ReleaseLaunch undoes HoldLaunch for a launch that was never sent.
func ReleaseLaunch() { launchesInFlight.Done() }

// ProvisionHeld is Provision for a caller that already called HoldLaunch.
func (p *Pool) ProvisionHeld(ctx context.Context, id string, log provider.Logger) (provider.Instance, error) {
	recorded := false
	defer func() {
		if !recorded {
			launchesInFlight.Done()
		}
	}()
	in, err := p.Fleet.Launch(ctx, log)
	if err != nil {
		ambiguous := errors.Is(err, provider.ErrLaunchAmbiguous)
		_ = p.with(func(st *poolState) error {
			for i := range st.Launches {
				// An ambiguous failure (the request may have created an
				// instance) keeps its reservation until the ledger expires
				// it, so it can never push the fleet past max_instances.
				if st.Launches[i].ID == id {
					if ambiguous {
						// Expires after listLagGrace even if this process lives
						// on (the pool manager): by then any instance the request
						// created is visible in listings.
						st.Launches[i].Ambiguous = time.Now()
					} else {
						st.Launches[i].Failed = true
					}
				}
			}
			st.BackoffUntil, st.LastLaunchErr = time.Now().Add(launchBackoff), firstLine(err.Error())
			st.ListedAt = staleListing(st.ListedAt)
			return nil
		})
		return in, err
	}
	_ = p.with(func(st *poolState) error {
		for i := range st.Launches {
			if st.Launches[i].ID == id {
				st.Launches[i].InstanceID = in.ID
			}
		}
		st.ListedAt = staleListing(st.ListedAt) // force the next caller to re-list
		return nil
	})
	recorded = true
	launchesInFlight.Done()
	ran, err := p.Fleet.WaitRunning(ctx, in.ID, log)
	if err != nil && ctx.Err() == nil {
		// Reclaimed or broken while booting (a bad volume key, an instant
		// Spot reclaim, no public IP): end it so it can't linger unusable,
		// stop counting it, and back off like any failed launch, so nothing
		// relaunches in a tight loop.
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		terr := p.Fleet.Terminate(tctx, in.ID)
		cancel()
		_ = p.with(func(st *poolState) error {
			for i := range st.Launches {
				// Only free the reservation once the instance is surely going
				// away; otherwise it still counts until listings show it.
				if st.Launches[i].ID == id && terr == nil {
					st.Launches[i].Failed = true
				}
			}
			st.BackoffUntil, st.LastLaunchErr = time.Now().Add(launchBackoff), firstLine(err.Error())
			st.ListedAt = staleListing(st.ListedAt)
			return nil
		})
	}
	return ran, err
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:159] + "…"
	}
	return s
}

// --- waiters: demand signalling between local sx processes ----------------

// Waiter marks this process as waiting for a slot in the pool. Demand is the
// sum of live waiters' weights; a waiter that dies releases its flock.
type Waiter struct {
	f    *os.File
	path string
}

// Register adds a waiter with weight (1, or a whole instance for --exclusive).
func (p *Pool) Register(weight int) (*Waiter, error) {
	var w *Waiter
	err := p.with(func(*poolState) error {
		var err error
		w, err = p.register(weight)
		return err
	})
	return w, err
}

// register must run under pool.lock: waiterDemand (also under the lock)
// unlinks unlocked waiter files, so creation and locking must be atomic
// with respect to it.
func (p *Pool) register(weight int) (*Waiter, error) {
	dir := filepath.Join(p.dir, "waiters")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.w", os.Getpid(), hex.EncodeToString(b[:])))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	_, _ = f.WriteString(strconv.Itoa(weight))
	return &Waiter{f: f, path: path}, nil
}

// Touch refreshes the waiter's heartbeat (stopped processes stop counting).
func (w *Waiter) Touch() {
	if w != nil && w.f != nil {
		now := time.Now()
		_ = os.Chtimes(w.path, now, now)
	}
}

// Done withdraws the waiter.
func (w *Waiter) Done() {
	if w == nil || w.f == nil {
		return
	}
	_ = os.Remove(w.path)
	_ = syscall.Flock(int(w.f.Fd()), syscall.LOCK_UN)
	_ = w.f.Close()
	w.f = nil
}

func (p *Pool) waiterDemand() int {
	dir := filepath.Join(p.dir, "waiters")
	ents, _ := os.ReadDir(dir)
	total := 0
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			// Nobody holds it: a dead waiter. Clean up.
			_ = os.Remove(path)
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			continue
		}
		fi, ferr := f.Stat()
		b := make([]byte, 32)
		n, _ := f.ReadAt(b, 0)
		f.Close()
		if ferr == nil && time.Since(fi.ModTime()) > staleAfter {
			continue // holder stopped (SIGSTOP): not current demand
		}
		w, err := strconv.Atoi(strings.TrimSpace(string(b[:n])))
		if err != nil || w < 1 {
			w = 1
		}
		total += w
	}
	return total
}

// Snapshot returns the cached state (for status displays).
func (p *Pool) Snapshot(ctx context.Context, force bool) (poolView, error) {
	var v poolView
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, force); err != nil {
			return err
		}
		v.Instances = st.Instances
		v.Draining, v.BootFailed = map[string]bool{}, map[string]bool{}
		for id, n := range st.Notes {
			if n.Draining {
				v.Draining[id] = true
			}
			if n.BootFailed {
				v.BootFailed[id] = true
			}
		}
		// Launches not yet visible as instances (listed ones are counted
		// in Instances already).
		listed := map[string]bool{}
		for _, in := range st.Instances {
			listed[in.ID] = true
		}
		for _, l := range st.Launches {
			if !l.Failed && (l.InstanceID == "" || !listed[l.InstanceID]) {
				v.Launching++
			}
		}
		v.Demand = p.waiterDemand()
		v.LastLaunchErr = st.LastLaunchErr
		return nil
	})
	return v, err
}

type poolView struct {
	Instances     []provider.Instance
	Draining      map[string]bool
	BootFailed    map[string]bool
	Launching     int
	Demand        int
	LastLaunchErr string
}

// staleListing forces the next refresh to re-list while keeping the cached
// view usable as a fallback if that listing fails (a zero time would not).
func staleListing(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.Unix(1, 0)
}

// BootFailure reports a recent bootstrap failure on this fleet ("" if none).
func (p *Pool) BootFailure() string {
	out := ""
	_ = p.with(func(st *poolState) error {
		for id, n := range st.Notes {
			if n.BootFailed && time.Since(n.BootFailedAt) < time.Hour {
				out = id
			}
		}
		return nil
	})
	return out
}
