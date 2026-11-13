package client

import (
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
	Draining     bool      `json:"draining,omitempty"`
	SuspectUntil time.Time `json:"suspect_until,omitempty"`
	BusyUntil    time.Time `json:"busy_until,omitempty"`
	LastUsed     time.Time `json:"last_used,omitempty"`
	Ready        bool      `json:"ready,omitempty"`
}

type launchRec struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instance_id,omitempty"`
	Started    time.Time `json:"started"`
	PID        int       `json:"pid"`
	Failed     bool      `json:"failed,omitempty"`
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
func (p *Pool) SlotsPerInstance() int {
	if p.R.MaxSlots > 0 {
		return p.R.MaxSlots
	}
	return 4
}

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
	if b, err := os.ReadFile(filepath.Join(p.dir, "pool.json")); err == nil {
		_ = json.Unmarshal(b, st)
		if st.Notes == nil {
			st.Notes = map[string]*note{}
		}
	}
	if err := fn(st); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(p.dir, "pool.json.tmp"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(p.dir, "pool.json"))
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
	if err != nil {
		return err
	}
	st.Instances, st.ListedAt = insts, time.Now()
	live := map[string]bool{}
	for _, in := range insts {
		live[in.ID] = true
	}
	for id := range st.Notes {
		if !live[id] {
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
		case l.InstanceID != "" && !live[l.InstanceID] && age > listLagGrace:
			// Launched but gone (reclaimed while booting).
		case l.InstanceID != "" && st.Notes[l.InstanceID] != nil && st.Notes[l.InstanceID].Ready:
			// Became usable; it now counts as a normal instance.
		case l.InstanceID == "" && !pidAlive(l.PID) && age > orphanGrace:
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

// Candidates returns running instances worth probing, most recently used
// first (packing work onto busy instances lets the rest idle out).
func (p *Pool) Candidates(ctx context.Context, exclude map[string]bool) ([]provider.Instance, error) {
	var out []provider.Instance
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		now := time.Now()
		for _, in := range st.Instances {
			n := st.Notes[in.ID]
			if in.State != provider.StateRunning || exclude[in.ID] {
				continue
			}
			if n != nil && (n.Draining || now.Before(n.SuspectUntil) || now.Before(n.BusyUntil)) {
				continue
			}
			out = append(out, in)
		}
		sort.SliceStable(out, func(i, j int) bool {
			a, b := st.Notes[out[i].ID], st.Notes[out[j].ID]
			var ta, tb time.Time
			if a != nil {
				ta = a.LastUsed
			}
			if b != nil {
				tb = b.LastUsed
			}
			if !ta.Equal(tb) {
				return ta.After(tb)
			}
			return out[i].LaunchedAt.Before(out[j].LaunchedAt)
		})
		return nil
	})
	return out, err
}

// Outcome of probing an instance.
type Outcome int

const (
	OutAcquired Outcome = iota
	OutBusy
	OutBooting
	OutDraining
	OutUnreachable
)

// Observe records a probe result.
func (p *Pool) Observe(id string, o Outcome, launchedAt time.Time) {
	_ = p.with(func(st *poolState) error {
		n := st.note(id)
		now := time.Now()
		switch o {
		case OutAcquired:
			n.Ready, n.LastUsed, n.SuspectUntil = true, now, time.Time{}
		case OutBusy:
			n.Ready, n.BusyUntil, n.SuspectUntil = true, now.Add(busyHintFor), time.Time{}
		case OutDraining:
			n.Draining = true
		case OutBooting:
			n.BusyUntil = now.Add(2 * time.Second)
		case OutUnreachable:
			// A young instance is probably still booting sshd; an older one is
			// suspect (but still counts toward capacity: unreachable is not
			// absent).
			if time.Since(launchedAt) < p.R.BootTimeout.Duration {
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
// more instance: only if waiting demand exceeds the capacity already being
// launched and the fleet is below max_instances.
func (p *Pool) Plan(ctx context.Context) (LaunchDecision, error) {
	var d LaunchDecision
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		d.Demand = p.waiterDemand()
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
		for _, l := range st.Launches {
			if l.InstanceID == "" || !listed[l.InstanceID] {
				usable++
				total++
			}
			d.Pending++
			d.Capacity += slots
		}
		d.Live = usable
		switch {
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
			if l.InstanceID == "" || !listed[l.InstanceID] {
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
	launchesInFlight.Add(1)
	recorded := false
	defer func() {
		if !recorded {
			launchesInFlight.Done()
		}
	}()
	in, err := p.Fleet.Launch(ctx, log)
	if err != nil {
		_ = p.with(func(st *poolState) error {
			for i := range st.Launches {
				if st.Launches[i].ID == id {
					st.Launches[i].Failed = true
				}
			}
			st.BackoffUntil, st.LastLaunchErr = time.Now().Add(launchBackoff), firstLine(err.Error())
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
		st.ListedAt = time.Time{} // force the next caller to re-list
		return nil
	})
	recorded = true
	launchesInFlight.Done()
	return p.Fleet.WaitRunning(ctx, in.ID, log)
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
		b, _ := os.ReadFile(path)
		f.Close()
		w, err := strconv.Atoi(strings.TrimSpace(string(b)))
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
	err := p.with(func(st *poolState) error {
		if err := p.refresh(ctx, st, force); err != nil {
			return err
		}
		v.Instances = st.Instances
		v.Draining = map[string]bool{}
		for id, n := range st.Notes {
			if n.Draining {
				v.Draining[id] = true
			}
		}
		v.Launching = len(st.Launches)
		v.Demand = p.waiterDemand()
		v.LastLaunchErr = st.LastLaunchErr
		return nil
	})
	return v, err
}

type poolView struct {
	Instances     []provider.Instance
	Draining      map[string]bool
	Launching     int
	Demand        int
	LastLaunchErr string
}
