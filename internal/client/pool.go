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

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

// Pool is the local view of one autoscaled fleet runner, shared by every sx
// process on this machine through pool.json guarded by pool.lock. See
// docs/POOL.md for the algorithm.
type Pool struct {
	R     *config.Runner
	Fleet provider.Fleet
	dir   string
	// since is when this process started waiting (Acquire); see quotaBlocked.
	since time.Time
}

const (
	listTTL       = 8 * time.Second  // cloud listing freshness
	suspectFor    = 30 * time.Second // skip an unreachable instance this long
	busyHintFor   = 1500 * time.Millisecond
	launchBackoff = 20 * time.Second // after a failed launch
	listLagGrace  = 5 * time.Minute  // a launched instance may be missing from listings this long
	orphanGrace   = 3 * time.Minute  // a launch whose provisioner died before recording an ID
	quotaRetry    = 5 * time.Minute  // after a quota refusal, unless one of our VMs ends first
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
	// BootSeenAt: when a probe last found it still booting (or not
	// answering yet), so its first ready probe times its launch.
	BootSeenAt   time.Time `json:"boot_seen_at,omitempty"`
	BootFailed   bool      `json:"boot_failed,omitempty"`
	BootFailedAt time.Time `json:"boot_failed_at,omitempty"`
	// Why the instance no longer satisfies the runner's requirements (""
	// when it does). Unlike Draining it is recomputed, so reverting the
	// config puts the VM back in service.
	Mismatch string `json:"mismatch,omitempty"`
	// Retiring: an early drain was committed (sched.go drain_requested):
	// never placed on again, and not capacity, though the VM itself may
	// not have agreed to drain yet.
	Retiring bool `json:"retiring,omitempty"`
	// Reached: when the VM last answered this client (any reply, even
	// booting, draining or an error). Ready says it once served. Silent:
	// when a probe last got no answer at all.
	Reached time.Time `json:"reached,omitempty"`
	Silent  time.Time `json:"silent,omitempty"`
	// Reaping: when a process began ending the VM to make room (reach.go):
	// no new runs go there meanwhile.
	Reaping time.Time `json:"reaping,omitempty"`
	// ReapCommitted: its termination request is going out: no claim at
	// all, an exact slot's included, reserves it now.
	ReapCommitted bool `json:"reap_committed,omitempty"`
}

// closed reports whether an instance takes no new runs and isn't capacity:
// it is draining (or retiring), or no longer matches the runner's
// requirements (it then idles out, and replacements launch for waiting
// demand).
func (n *note) closed() bool { return n != nil && (n.Draining || n.Retiring || n.Mismatch != "") }

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
	// The account's vCPU quota refused the last launch: no launches until
	// one of this fleet's VMs ends (freeing quota) or quotaRetry passes.
	QuotaErr  string    `json:"quota_error,omitempty"`
	QuotaAt   time.Time `json:"quota_at,omitempty"`
	QuotaLive int       `json:"quota_live,omitempty"` // instances listed when it was refused
	// QuotaErr is instead every Spot price being above max_price, which
	// waiting won't fix either.
	QuotaPrice bool `json:"quota_price,omitempty"`
	// The requirements (provider.MatchKey) the notes' Mismatch reasons
	// were judged against.
	MatchKey string `json:"match_key,omitempty"`
	// Held: regions this client found it can't reach, and when it last
	// asked the fleet to hold launches out of them (reach.go).
	Held map[string]time.Time `json:"held,omitempty"`
	// Sched: the reservation ledger and placement policy (sched.go). Kept
	// in its own file, sched.json, under the same lock: a binary from
	// before it would otherwise drop it whenever it rewrote pool.json.
	Sched *schedState `json:"-"`
}

// quotaBlocked reports the quota (or max_price) refusal that still holds
// launches back ("" if none) for a process waiting since `since` (zero:
// always waiting).
//
// It holds for quotaRetry, or until one of the fleet's VMs ends and frees
// quota. When the fleet had no VM at all, it holds only for processes
// already waiting when it happened (they fail, or wait for another runner
// without retrying); a run that starts later tries once more, since the
// quota may have been raised meanwhile. A max_price refusal is always
// retried by a later run: the config may have changed, and finding every
// price above the cap again costs no launch request.
func (st *poolState) quotaBlocked(since time.Time) string {
	if st.QuotaErr == "" || time.Since(st.QuotaAt) > quotaRetry || st.vmCount() < st.QuotaLive {
		return ""
	}
	if (st.QuotaLive == 0 || st.QuotaPrice) && !st.QuotaAt.After(since) {
		return ""
	}
	return st.QuotaErr
}

// blockedReason is Plan's reason while quotaBlocked holds launches back.
func blockedReason(price bool) string {
	if price {
		return "every Spot price is above max_price, so no new VMs; waiting for a slot on running ones"
	}
	return "at the account's vCPU quota, so no new VMs; waiting for a slot on running ones"
}

// vmCount is the fleet's VMs holding quota: listed instances (draining ones
// included, until they end) plus launches not listed yet.
func (st *poolState) vmCount() int {
	n := len(st.Instances)
	listed := map[string]bool{}
	for _, in := range st.Instances {
		listed[in.ID] = true
	}
	for _, l := range st.Launches {
		if !l.Failed && l.InstanceID != "" && !listed[l.InstanceID] {
			n++
		}
	}
	return n
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
	origSched, _ := os.ReadFile(filepath.Join(p.dir, "sched.json"))
	if len(origSched) > 0 {
		var s schedState
		if json.Unmarshal(origSched, &s) == nil {
			st.Sched = &s
		}
	}
	applied := p.applyPending(st)
	if err := fn(st); err != nil {
		return err
	}
	// The ledger first: if pool.json then fails, the launch records it
	// holds were never saved (so no launch is counted that nobody makes).
	if st.Sched != nil {
		b, _ := json.MarshalIndent(st.Sched, "", "  ")
		if err := writeIfChanged(p.dir, "sched.json", b, origSched); err != nil {
			return err
		}
		p.pendingDone(applied)
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeIfChanged(p.dir, "pool.json", b, orig)
}

// writeIfChanged atomically replaces dir/name with b, unless it already
// holds exactly that (lots of read-only callers).
func writeIfChanged(dir, name string, b, orig []byte) error {
	if bytes.Equal(b, orig) {
		return nil
	}
	tmp := filepath.Join(dir, name+".tmp"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// readyGrace: how long after an instance is first seen ready its launch
// still counts as pending capacity.
const readyGrace = 20 * time.Second

func (n *note) markReady(now time.Time) {
	if !n.Ready {
		n.Ready, n.ReadyAt = true, now
	}
}

// markReady records instance id as serving, and times its launch when it
// was watched booting until now (leadGap), so the time isn't inflated by
// a stretch nobody probed it.
func (st *poolState) markReady(id string, now time.Time) {
	n := st.note(id)
	if !n.Ready && !n.BootSeenAt.IsZero() && now.Sub(n.BootSeenAt) <= leadGap {
		for _, l := range st.Launches {
			if l.InstanceID == id && !l.Failed && !l.Started.IsZero() {
				st.sched().observeLaunch(id, now.Sub(l.Started), now)
				break
			}
		}
	}
	n.markReady(now)
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

// LifetimeDrainAhead is how long before a VM's max_lifetime it stops
// taking new runs (and drains), so the hard poweroff at max_lifetime never
// interrupts a run that started after that point: 15 minutes, or a quarter
// of a shorter max_lifetime.
// (The VM's helper enforces the same cutoff when a run reaches it.)
func LifetimeDrainAhead(r *config.Runner) time.Duration {
	lt := r.MaxLifetime.Duration
	if lt <= 0 {
		lt = 12 * time.Hour // cloud-init's default
	}
	return protocol.LifetimeDrainAhead(lt)
}

// NearLifetime reports whether an instance is within LifetimeDrainAhead of
// its max_lifetime.
func NearLifetime(r *config.Runner, in provider.Instance) bool {
	lt := r.MaxLifetime.Duration
	if lt <= 0 {
		lt = 12 * time.Hour
	}
	return !in.LaunchedAt.IsZero() && time.Since(in.LaunchedAt) > lt-LifetimeDrainAhead(r)
}

// drainAging marks instances near max_lifetime draining: no new runs are
// placed on them, and they stop counting as capacity, so a replacement is
// launched for waiting demand (or the warm pool). Every sx process and the
// daemon do this, so it holds whether or not the daemon runs.
func (p *Pool) drainAging(st *poolState) {
	for _, in := range st.Instances {
		if NearLifetime(p.R, in) {
			st.note(in.ID).Draining = true
		}
	}
}

// matchConfig marks the instances that no longer satisfy the runner's
// requirements (see note.Mismatch), when the fleet was just re-listed or
// the requirements differ from those last judged against (the config
// changed, or another process runs with a different one). An instance the
// fleet can't judge keeps its last verdict under the same requirements,
// and matches under new ones: a failed lookup never retires VMs.
func (p *Pool) matchConfig(ctx context.Context, st *poolState, relisted bool) {
	m, ok := p.Fleet.(provider.Matcher)
	if !ok {
		return
	}
	key := provider.MatchKey(p.R)
	if !relisted && key == st.MatchKey {
		return
	}
	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	judged := m.Mismatches(mctx, st.Instances)
	same := key == st.MatchKey
	st.MatchKey = key
	for _, in := range st.Instances {
		why, ok := judged[in.ID]
		if !ok && same {
			continue
		}
		if why != "" {
			st.note(in.ID).Mismatch = why
		} else if n := st.Notes[in.ID]; n != nil {
			n.Mismatch = ""
		}
	}
}

// refresh re-lists the fleet when the cached listing is stale (or force),
// reconciling notes and the launch ledger, and drains instances near
// max_lifetime. Called with the lock held, so one process refreshes while
// the others wait and reuse its result.
func (p *Pool) refresh(ctx context.Context, st *poolState, force bool) error {
	defer p.drainAging(st)
	if !force && time.Since(st.ListedAt) < listTTL {
		p.matchConfig(ctx, st, false)
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
			p.matchConfig(ctx, st, false)
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
	st.Instances = insts
	p.matchConfig(ctx, st, true)
	// Fresh from now: a slow type lookup in matchConfig mustn't leave the
	// listing already stale, sending every waiting process to re-list.
	st.ListedAt = time.Now()
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
			// Draining: no longer pending capacity. (A mismatched launch keeps
			// its record, uncounted while it mismatches, since a config
			// change can undo that.)
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
	st.sched().forgetGone(live)
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
			if in.State != provider.StateRunning || in.Target.Host == "" || excluded(exclude, in) {
				continue
			}
			if !n.closed() {
				usable++
			}
			if n != nil && (n.closed() || n.reaping(now) || now.Before(n.SuspectUntil) || now.Before(n.BusyUntil)) {
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
	// outSkipped: not contacted, the ledger shows no room (or the policy no
	// longer places work there).
	outSkipped
	// outRefused: the helper answered with an error this side doesn't
	// recognize: judged like OutUnreachable, but it is an answer.
	outRefused
	// outDrainReply: the VM answered that it is draining (OutDraining is
	// also recorded with no contact at all, after a termination).
	outDrainReply
	// outContact: the VM answered, and nothing else is recorded (an
	// exclusive probe finding it partly busy; `spotexec status`).
	outContact
)

// Observe records a probe result, and holds launches out of the VM's
// region when it shows the client can't reach the region (or ends that
// hold when the VM answered).
func (p *Pool) Observe(id string, o Outcome, launchedAt time.Time) {
	if o == outSkipped {
		return
	}
	_ = p.with(func(st *poolState) error {
		n := st.note(id)
		now := time.Now()
		switch o {
		case OutAcquired:
			st.markReady(id, now)
			n.LastUsed, n.SuspectUntil, n.Failures, n.Reached = now, time.Time{}, 0, now
		case OutBusy:
			st.markReady(id, now)
			n.SawFull = true
			n.BusyUntil, n.SuspectUntil, n.Failures, n.Reached = now.Add(busyHintFor), time.Time{}, 0, now
		case OutDraining:
			n.Draining = true
		case outDrainReply:
			n.Draining = true
			n.Failures, n.Reached = 0, now
		case outContact:
			n.SuspectUntil, n.Failures, n.Reached = time.Time{}, 0, now
		case OutBootFailed:
			n.Draining, n.BootFailed = true, true
			n.Failures, n.Reached = 0, now
			if n.BootFailedAt.IsZero() {
				n.BootFailedAt = now
			}
		case OutBooting:
			n.BusyUntil = now.Add(2 * time.Second)
			n.Failures, n.Reached = 0, now
			if !n.Ready {
				n.BootSeenAt = now
			}
		case OutUnreachable, outRefused:
			if o == OutUnreachable {
				n.Silent = now
			} else {
				n.Reached = now
			}
			// A young instance is probably still booting sshd; an older one is
			// suspect (but still counts toward capacity: unreachable is not
			// absent).
			n.Failures++
			if !n.Ready {
				n.BootSeenAt = now
			}
			if time.Since(launchedAt) < p.R.BootTimeout.Duration || n.Failures < 2 {
				n.BusyUntil = now.Add(3 * time.Second)
			} else {
				n.SuspectUntil = now.Add(suspectFor)
			}
		}
		// The fleet's hold is set under this lock too, so a reply and a
		// failure observed at once can't leave it out of step with Held.
		p.applyReach(p.judgeRegion(st, id, o, now))
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
	// QuotaErr is set while the account's vCPU quota (or max_price, below
	// every Spot price) blocks launches: the fleet can only serve from the
	// VMs it already has.
	QuotaErr string
	// Reap: VMs to terminate before a launch can make room under
	// max_instances: they are in a region this client can't reach.
	Reap []string
	// Held is set when slots on running VMs are expected to free sooner
	// than a launch would serve (Reason says so): Deferred slots of demand
	// wait for them, FreeIn after which the last of them frees.
	Held     bool
	Deferred int
	FreeIn   time.Duration
	// Lead is how long a launch is expected to take to serve (LeadLearned:
	// from timed launches, not the default), and Waited how long this
	// process has waited: past Lead, demand is sustained and launches.
	Lead        time.Duration
	LeadLearned bool
	Waited      time.Duration
}

// Plan decides, under the pool lock, whether this process should launch one
// more instance: only if waiting demand (minus `covered`, the capacity that
// higher-priority fleets are already launching or waiting for) exceeds the
// capacity this fleet is launching, the fleet is below max_instances (when
// it has one), and slots on its running VMs aren't expected to free sooner
// than a new VM would serve (a short burst waits for them; demand still
// waiting after a launch's lead launches).
func (p *Pool) Plan(ctx context.Context, covered int) (LaunchDecision, error) {
	var d LaunchDecision
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		demand, wholeVM := p.waiters()
		d.Demand = demand - covered
		listed := map[string]bool{}
		usable, total := 0, 0
		for _, in := range st.Instances {
			listed[in.ID] = true
			total++
			if !st.Notes[in.ID].closed() {
				usable++
			}
		}
		slots := p.SlotsPerInstance()
		inLedger := map[string]bool{}
		pendingVM := map[string]bool{} // instances whose slots count as pending
		for _, l := range st.Launches {
			if l.Failed {
				continue
			}
			if l.InstanceID != "" {
				inLedger[l.InstanceID] = true
				if n := st.Notes[l.InstanceID]; n != nil && (n.closed() || (n.Ready && (n.SawFull || readyFor(n) > readyGrace))) {
					// Dying, or already serving (full, or ready a while):
					// not capacity a waiting agent can count on.
					continue
				}
				if listed[l.InstanceID] && p.lostVM(st, l.InstanceID, time.Now()) {
					// Never reached, in a region this client can't reach: it
					// won't serve, so another launch (elsewhere) may.
					continue
				}
			}
			if l.InstanceID == "" || !listed[l.InstanceID] {
				usable++
				total++
			}
			pendingVM[l.InstanceID] = true
			d.Pending++
			d.Capacity += slots
		}
		// Instances still booting (not yet seen ready) are pending capacity
		// too, even if their launcher's ledger entry was lost.
		for _, in := range st.Instances {
			n := st.Notes[in.ID]
			// Only never-ready instances count here: a ready one is already
			// serving (its launch-grace capacity, if any, is in the ledger).
			if inLedger[in.ID] || (n != nil && (n.Ready || n.closed())) {
				continue
			}
			if time.Since(in.LaunchedAt) < p.R.BootTimeout.Duration {
				pendingVM[in.ID] = true
				d.Pending++
				d.Capacity += slots
			}
		}
		d.Live = usable
		now := time.Now()
		d.Lead, d.LeadLearned = st.sched().launchLead(now)
		if !p.since.IsZero() {
			d.Waited = now.Sub(p.since)
		}
		d.QuotaErr = st.quotaBlocked(p.since)
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
		case !p.R.BelowMax(usable):
			d.Reason = fmt.Sprintf("at max_instances (%d)", p.R.MaxInstances)
			if id, region := p.reapable(st, time.Now()); id != "" {
				d.Reap = []string{id}
				n := st.note(id)
				n.Reaping, n.ReapCommitted = time.Now(), false
				d.Reason += fmt.Sprintf("; ending %s, in %s, which this client can't reach, to make room", id, region)
			}
		case p.R.Capped() && total >= 2*p.R.MaxInstances:
			d.Reason = liveCapReason(st, p.R.MaxInstances)
		case d.QuotaErr != "":
			d.Reason = blockedReason(st.QuotaPrice)
		case time.Now().Before(st.BackoffUntil):
			d.Reason = "backing off after a failed launch: " + st.LastLaunchErr
		case p.releaseSoon(st, &d, wholeVM, pendingVM, now):
			d.Held, d.Deferred = true, d.Demand-d.Capacity
			d.Reason = fmt.Sprintf("%s on running VMs expected to free within %s, sooner than a new VM (~%s)",
				plural(d.Deferred, "slot"), ui.Dur(d.FreeIn), ui.Dur(d.Lead))
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

// liveCapReason explains launches held by the cap on live VMs (twice
// max_instances): the VMs out of service that haven't ended yet, counted
// as draining or outdated (the states `spotexec status` names).
func liveCapReason(st *poolState, maxInstances int) string {
	outdated, draining := 0, 0
	for _, in := range st.Instances {
		switch n := st.Notes[in.ID]; {
		case n == nil:
		case n.Draining:
			draining++
		case n.Mismatch != "":
			outdated++
		}
	}
	vms := func(n int, state string) string {
		if n == 1 {
			return "1 " + state + " VM"
		}
		return fmt.Sprintf("%d %s VMs", n, state)
	}
	var held []string
	if outdated > 0 {
		held = append(held, vms(outdated, "outdated"))
	}
	if draining > 0 {
		held = append(held, vms(draining, "draining"))
	}
	if len(held) == 0 {
		held = append(held, "VMs")
	}
	return fmt.Sprintf("waiting for %s to end: launches pause at %d live VMs (twice max_instances)",
		strings.Join(held, " and "), 2*maxInstances)
}

// Reserve records up to n launches regardless of waiting demand (spotexec
// up), still respecting max_instances (when set). It returns the launch IDs.
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
			if !st.Notes[in.ID].closed() {
				usable++
			}
		}
		for _, l := range st.Launches {
			if !l.Failed && (l.InstanceID == "" || !listed[l.InstanceID]) {
				usable++
			}
		}
		for i := 0; i < n && p.R.BelowMax(usable); i++ {
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
			if !st.Notes[in.ID].closed() {
				usable++
			}
		}
		for _, l := range st.Launches {
			if !l.Failed && (l.InstanceID == "" || !listed[l.InstanceID]) {
				usable++
				total++
			}
		}
		if time.Now().Before(st.BackoffUntil) || (p.R.Capped() && total >= 2*p.R.MaxInstances) || st.quotaBlocked(time.Time{}) != "" {
			return nil
		}
		for _, n := range st.Notes {
			if n.BootFailed && time.Since(n.BootFailedAt) < time.Hour {
				return nil // the setup is broken: don't warm more of it
			}
		}
		for usable < min && p.R.BelowMax(usable) && !(p.R.Capped() && total >= 2*p.R.MaxInstances) {
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
		// Held stays: the fleet's region holds outlive a teardown, and an
		// answer from the region must still end them.
		*st = poolState{Notes: map[string]*note{}, Sched: &schedState{Version: schedVersion}, Held: st.Held}
		return nil
	})
}

// launchesInFlight tracks Provision calls until RunInstances has returned
// and its instance ID is recorded, so a process does not exit in between.
var launchesInFlight sync.WaitGroup

// WaitLaunches blocks (up to max) until in-flight launches are recorded.
func WaitLaunches(max time.Duration) { WaitLaunchesSaying(max, nil) }

// WaitLaunchesSaying is WaitLaunches that calls say once if the wait passes
// a second, so a finished command doesn't seem to hang.
func WaitLaunchesSaying(max time.Duration, say func()) {
	done := make(chan struct{})
	go func() { launchesInFlight.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(time.Second):
		if say != nil {
			say()
		}
	}
	select {
	case <-done:
	case <-time.After(max - time.Second):
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
	return p.ProvisionHeldThen(ctx, id, log, nil)
}

// ProvisionHeldThen is ProvisionHeld, calling launched (if set) as soon as
// the launch request succeeds, before waiting for the VM to run.
func (p *Pool) ProvisionHeldThen(ctx context.Context, id string, log provider.Logger, launched func(provider.Instance)) (provider.Instance, error) {
	return p.provisionHeld(ctx, id, log, launched, nil)
}

// provisionHeld is ProvisionHeldThen, also calling failed (if set) when the
// launch request fails. Both callbacks run before the launch's hold ends,
// so a process exiting (WaitLaunches) still hears how its launch went.
func (p *Pool) provisionHeld(ctx context.Context, id string, log provider.Logger, launched func(provider.Instance), failed func(error)) (provider.Instance, error) {
	recorded := false
	defer func() {
		if !recorded {
			launchesInFlight.Done()
		}
	}()
	in, err := p.Fleet.Launch(ctx, log)
	if err != nil {
		ambiguous := errors.Is(err, provider.ErrLaunchAmbiguous)
		var qe *provider.QuotaError
		var pe *provider.PriceCapError
		quota := errors.As(err, &qe)
		price := !quota && errors.As(err, &pe)
		_ = p.with(func(st *poolState) error {
			if quota || price {
				st.QuotaErr, st.QuotaAt, st.QuotaLive = err.Error(), time.Now(), st.vmCount()
				st.QuotaPrice = price
			}
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
		if failed != nil {
			failed(err)
		}
		return in, err
	}
	_ = p.with(func(st *poolState) error {
		for i := range st.Launches {
			if st.Launches[i].ID == id {
				st.Launches[i].InstanceID = in.ID
			}
		}
		// There was room after all.
		st.QuotaErr, st.QuotaAt, st.QuotaLive, st.QuotaPrice = "", time.Time{}, 0, false
		st.ListedAt = staleListing(st.ListedAt) // force the next caller to re-list
		return nil
	})
	if launched != nil {
		launched(in) // before the hold ends, so an exiting process still reports it
	}
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
		if terr != nil {
			err = fmt.Errorf("%w; terminating %s also failed: %v (run `spotexec down %s`)", err, in.ID, terr, p.R.Name)
		}
	}
	if err != nil {
		// The launched instance, so a caller can still find (or end) it.
		return in, err
	}
	if ran.Placement == nil {
		ran.Placement = in.Placement // why it launched there, for the instance_running event
	}
	if ran.Zone == "" {
		ran.Zone = in.Zone
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

// releaseSoon reports whether the demand this fleet isn't launching for
// (d.Demand - d.Capacity) can wait for slots on its running VMs, setting
// d.FreeIn: only while this process has waited less than a launch's lead
// (past it, demand is sustained), and only for slots that jobs predicted
// to end within the lead hold now. A job whose end is unknown, or already
// overdue, frees nothing here: the prediction has no say then. A
// whole-VM (exclusive) waiter needs a VM to empty, which freeing slots
// don't show, so it never waits here.
func (p *Pool) releaseSoon(st *poolState, d *LaunchDecision, wholeVM bool, pendingVM map[string]bool, now time.Time) bool {
	need := d.Demand - d.Capacity
	if need <= 0 || wholeVM || p.since.IsZero() || d.Waited >= d.Lead {
		return false
	}
	s := st.sched()
	eligible := map[string]bool{}
	for _, in := range st.Instances {
		n := st.Notes[in.ID]
		if n == nil || !n.Ready || n.closed() || now.Before(n.SuspectUntil) || pendingVM[in.ID] || s.Fences[in.ID] != nil {
			continue
		}
		eligible[in.ID] = true
	}
	type release struct {
		at    time.Time
		units int
	}
	var rs []release
	for _, a := range s.Attempts {
		if a.State != attAcquired || !eligible[a.VM] || !a.Expect.After(now) || !a.Expect.Before(now.Add(d.Lead)) {
			continue
		}
		rs = append(rs, release{a.Expect, max(a.Units, 1)})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].at.Before(rs[j].at) })
	freed := 0
	for _, r := range rs {
		if freed += r.units; freed >= need {
			d.FreeIn = r.at.Sub(now)
			return true
		}
	}
	return false
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func (p *Pool) waiterDemand() int {
	total, _ := p.waiters()
	return total
}

// waiters is the waiting demand in slots, and whether any waiter needs a
// whole VM (an exclusive job on a fleet of several-slot VMs).
func (p *Pool) waiters() (total int, wholeVM bool) {
	dir := filepath.Join(p.dir, "waiters")
	ents, _ := os.ReadDir(dir)
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
		wholeVM = wholeVM || w > 1
	}
	return total, wholeVM
}

// Snapshot returns the cached state (for status displays).
func (p *Pool) Snapshot(ctx context.Context, force bool) (poolView, error) {
	var v poolView
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, force); err != nil {
			return err
		}
		v.Instances = st.Instances
		v.Draining, v.BootFailed, v.Mismatch = map[string]bool{}, map[string]bool{}, map[string]string{}
		for id, n := range st.Notes {
			if n.Draining {
				v.Draining[id] = true
			}
			if n.Mismatch != "" {
				v.Mismatch[id] = n.Mismatch
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
	Mismatch      map[string]string // instance -> why it no longer matches the config
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
