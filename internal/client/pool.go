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
	"slices"
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

// A refresh runs under the pool lock, so every process using the runner's
// pool waits behind it. refreshBudget bounds the whole of it (listing and
// judging the VMs against the config), and listBudget the listing within
// it, so one region whose API doesn't answer delays them only that long.
var (
	refreshBudget = 15 * time.Second
	listBudget    = 10 * time.Second
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
	// BootSeenAt: when a probe last found its helper still running
	// first-boot setup, so its first ready probe times its launch. (Not a
	// failure to answer: a VM that was ready unobserved can fail one too.)
	BootSeenAt time.Time `json:"boot_seen_at,omitempty"`
	// A watch, which also times the launch when no booting answer was seen
	// (a baked VM can finish first-boot setup before it first answers):
	// WatchN probes before it first served found it not serving yet, the
	// first within watchStart of its launch, the last (WatchAt) and each
	// since within leadGap of the one before. It saturates at watchMin, and
	// is -1 once broken: a late start, a gap, or a silent VM past
	// boot_timeout or in a region this client can't reach.
	WatchAt      time.Time `json:"watch_at,omitempty"`
	WatchN       int       `json:"watch_n,omitempty"`
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
	// Region the instance launched in ("" until known): while that region
	// can't be listed, the instance's absence proves nothing.
	Region string `json:"region,omitempty"`
}

type poolState struct {
	Instances []provider.Instance `json:"instances"`
	ListedAt  time.Time           `json:"listed_at"`
	// Unlisted: the regions the last listing couldn't list. Their VMs are
	// those last seen there, and nothing is forgotten for being absent
	// until a listing covers every region again.
	Unlisted      []string         `json:"unlisted,omitempty"`
	Launches      []launchRec      `json:"launches"`
	Notes         map[string]*note `json:"notes"`
	BackoffUntil  time.Time        `json:"backoff_until,omitempty"`
	LastLaunchErr string           `json:"last_launch_error,omitempty"`
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

const (
	// watchMin: probes a watch needs before it times a launch. One failure
	// to answer just before serving proves little: a VM ready unobserved
	// can fail one too.
	watchMin = 2
	// watchStart: how soon after its launch a watch's first probe must
	// end. Looser than leadGap: listing a new VM (pending, then running,
	// through an 8s cache) and an SSH connect timeout come first, and a
	// probe that found the VM not serving yet bounds nothing it missed.
	watchStart = time.Minute
)

// markReady records instance id as serving, and times its launch when it
// was watched booting until now (leadGap), or watched from its launch
// until now (note.WatchN), so the time isn't inflated by a stretch nobody
// probed it.
func (st *poolState) markReady(id string, now time.Time) {
	n := st.note(id)
	booted := !n.BootSeenAt.IsZero() && now.Sub(n.BootSeenAt) <= leadGap
	watched := n.WatchN >= watchMin && now.Sub(n.WatchAt) <= leadGap
	if !n.Ready && (booted || watched) {
		st.timeLaunch(id, now)
	}
	n.markReady(now)
}

// watch extends instance id's watch with a probe at now that found it not
// serving yet (no answer, or still booting). launchedAt is the provider's
// launch time (zero if unknown); late says a VM still not answering may
// have been ready behind an unreachable network all along (it is past
// boot_timeout, or its region is held).
func (st *poolState) watch(id string, launchedAt time.Time, late bool, now time.Time) {
	n := st.note(id)
	if n.Ready || n.WatchN < 0 {
		return
	}
	from, within := n.WatchAt, leadGap
	if n.WatchN == 0 {
		var ok bool
		if from, ok = st.launchStart(id, launchedAt, now); !ok {
			return // not this fleet's launch on record (yet): nothing to time
		}
		within = watchStart
	}
	if late || now.Sub(from) > within || now.Before(from) {
		n.WatchN, n.WatchAt = -1, time.Time{}
		return
	}
	n.WatchN, n.WatchAt = min(n.WatchN+1, watchMin), now
}

// launchStart is when instance id came to exist, as far as a watch goes:
// its launch record's start, or the provider's launch time when later
// (planning, pricing and capacity retries come first, and a VM can't be
// probed before it exists) but not after now (the provider's clock may
// run ahead of this one).
func (st *poolState) launchStart(id string, launchedAt, now time.Time) (time.Time, bool) {
	for _, l := range st.Launches {
		if l.InstanceID == id && !l.Failed && !l.Started.IsZero() {
			if launchedAt.After(l.Started) && !launchedAt.After(now) {
				return launchedAt, true
			}
			return l.Started, true
		}
	}
	return time.Time{}, false
}

// regionHeld reports whether instance id is in a region this client holds
// launches out of, as one it can't reach (reach.go).
func (st *poolState) regionHeld(id string, now time.Time) bool {
	for _, in := range st.Instances {
		if in.ID == id {
			at, held := st.Held[in.Region]
			return in.Region != "" && held && now.Sub(at) <= heldFor
		}
	}
	return false
}

// timeLaunch records how long instance id's launch took to serve, from
// its launch record.
func (st *poolState) timeLaunch(id string, now time.Time) {
	for _, l := range st.Launches {
		if l.InstanceID == id && !l.Failed && !l.Started.IsZero() {
			st.sched().observeLaunch(id, now.Sub(l.Started), now)
			return
		}
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
// and matches under new ones: a failed lookup never retires VMs. ctx
// bounds it (refresh's budget).
func (p *Pool) matchConfig(ctx context.Context, st *poolState, relisted bool) {
	m, ok := p.Fleet.(provider.Matcher)
	if !ok {
		return
	}
	key := provider.MatchKey(p.R)
	if !relisted && key == st.MatchKey {
		return
	}
	judged := m.Mismatches(ctx, st.Instances)
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
// the others wait and reuse its result; refreshBudget bounds how long.
//
// A fleet spread over regions lists the regions that answer: their VMs
// replace the cached ones, while a region that doesn't answer keeps the
// VMs last seen there, and nothing is forgotten for being absent (see
// poolState.Unlisted). A forced refresh (status, the pool manager) gets
// that view too: it is the truth for every region that answered.
func (p *Pool) refresh(ctx context.Context, st *poolState, force bool) error {
	defer p.drainAging(st)
	rctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	if !force && time.Since(st.ListedAt) < listTTL {
		p.matchConfig(rctx, st, false)
		return nil
	}
	insts, unlisted, err := p.list(rctx)
	if ctx.Err() != nil {
		// The caller gave up: what was listed is cut short by that, not by
		// the cloud, and it acts on nothing.
		return ctx.Err()
	}
	if err == nil && len(unlisted) > 0 && st.ListedAt.IsZero() {
		// Nothing known yet of the regions that didn't answer.
		err = joinUnlisted(unlisted)
	}
	if err == nil {
		for _, e := range unlisted {
			var ce *provider.CredentialError
			if errors.As(e, &ce) {
				err = joinUnlisted(unlisted)
				break
			}
		}
	}
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
			p.matchConfig(rctx, st, false)
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
	// Launch records from before they carried a region take it from the
	// listings that show their instance.
	regionOf := map[string]string{}
	for _, in := range append(slices.Clone(st.Instances), insts...) {
		if in.Region != "" {
			regionOf[in.ID] = in.Region
		}
	}
	for i, l := range st.Launches {
		if l.Region == "" && l.InstanceID != "" {
			st.Launches[i].Region = regionOf[l.InstanceID]
		}
	}
	st.Instances = keepUnlisted(insts, st.Instances, unlisted)
	st.Unlisted = nil
	for rg := range unlisted {
		st.Unlisted = append(st.Unlisted, rg)
	}
	sort.Strings(st.Unlisted)
	partial := len(unlisted) > 0
	p.matchConfig(rctx, st, true)
	// Fresh from now: a slow type lookup in matchConfig mustn't leave the
	// listing already stale, sending every waiting process to re-list.
	st.ListedAt = time.Now()
	live := map[string]bool{}
	for _, in := range st.Instances {
		live[in.ID] = true
	}
	if !partial {
		for id, n := range st.Notes {
			// A bootstrap failure is remembered for an hour after its VM is
			// gone: it says the runner's setup is broken, not just that VM.
			if !live[id] && !(n.BootFailed && time.Since(n.BootFailedAt) < time.Hour) {
				delete(st.Notes, id)
			}
		}
	}
	var keep []launchRec
	for _, l := range st.Launches {
		age := time.Since(l.Started)
		switch {
		case l.Failed:
		case partial && !live[l.InstanceID] && (l.Region == "" || unlisted[l.Region] != nil):
			// Not seen, and it may be in a region that didn't answer: still
			// counted until a listing can tell.
			keep = append(keep, l)
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
	if !partial {
		st.sched().forgetGone(live)
	}
	return nil
}

// list lists the fleet within listBudget: a fleet spread over regions
// lists those that answer, and says which didn't.
func (p *Pool) list(ctx context.Context) ([]provider.Instance, map[string]error, error) {
	ctx, cancel := context.WithTimeout(ctx, listBudget)
	defer cancel()
	if rl, ok := p.Fleet.(provider.RegionLister); ok {
		return rl.ListReachable(ctx)
	}
	insts, err := p.Fleet.List(ctx)
	return insts, nil, err
}

// keepUnlisted is a listing with, for each region it couldn't list, the
// instances last seen there (and those of no known region), since their
// absence proves nothing.
func keepUnlisted(listed, cached []provider.Instance, unlisted map[string]error) []provider.Instance {
	if len(unlisted) == 0 {
		return listed
	}
	out := append([]provider.Instance(nil), listed...)
	seen := map[string]bool{}
	for _, in := range listed {
		seen[in.ID] = true
	}
	for _, in := range cached {
		if seen[in.ID] {
			continue
		}
		if _, gone := unlisted[in.Region]; gone || in.Region == "" {
			seen[in.ID] = true
			out = append(out, in)
		}
	}
	return out
}

// joinUnlisted is one error for the regions a listing couldn't list,
// keeping a credential failure findable with errors.As.
func joinUnlisted(unlisted map[string]error) error {
	regions := make([]string, 0, len(unlisted))
	for rg := range unlisted {
		regions = append(regions, rg)
	}
	sort.Strings(regions)
	errs := make([]error, 0, len(regions))
	for _, rg := range regions {
		errs = append(errs, fmt.Errorf("%s: %w", rg, unlisted[rg]))
	}
	return errors.Join(errs...)
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
			// It just took a job: a busy hint from before (one an overflow
			// request then got past) no longer holds.
			n.LastUsed, n.SuspectUntil, n.Failures, n.Reached, n.BusyUntil = now, time.Time{}, 0, now, time.Time{}
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
			st.watch(id, launchedAt, false, now)
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
			young := time.Since(launchedAt) < p.R.BootTimeout.Duration
			if young || n.Failures < 2 {
				n.BusyUntil = now.Add(3 * time.Second)
			} else {
				n.SuspectUntil = now.Add(suspectFor)
			}
			if o == OutUnreachable {
				st.watch(id, launchedAt, (!young && !launchedAt.IsZero()) || st.regionHeld(id, now), now)
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
	// Ready: slots of demand that VMs already serving can take now, by
	// this machine's ledger (readyRoom): an empty VM for each whole-VM
	// waiter, free slots for the rest.
	Ready int
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
// capacity this fleet is launching plus what its ready VMs can take now
// (readyRoom), the fleet is below max_instances (when it has one), and
// slots on its running VMs aren't expected to free sooner than a new VM
// would serve (a short burst waits for them; demand still waiting after a
// launch's lead launches).
//
// A VM booting in a region some waiters can't use (replays kept out of a
// silent region, #240) still counts toward max_instances, but its slots
// cover only the demand of the waiters that can use it, so those replays
// aren't left waiting on it.
func (p *Pool) Plan(ctx context.Context, covered int) (LaunchDecision, error) {
	var d LaunchDecision
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		demand, whole, away := p.waitersAway()
		d.Demand = demand - covered
		listed := map[string]bool{}
		unusable := map[string]string{} // listed, in a region some waiters can't use: its region
		usable, total := 0, 0
		s, now := st.sched(), time.Now()
		for _, in := range st.Instances {
			listed[in.ID] = true
			if in.Region != "" && away.avoided(in.Region) {
				unusable[in.ID] = in.Region
			}
			total++
			n := st.Notes[in.ID]
			// A VM whose drain a job may still cancel counts toward
			// max_instances: it may serve again.
			if v := s.VMs[in.ID]; !n.closed() || (v != nil && in.State == provider.StateRunning && reclaimable(v, n, now)) {
				usable++
			}
		}
		slots := p.SlotsPerInstance()
		excludedCap := 0                 // pending slots in VMs some waiters can't use
		awayRegions := map[string]bool{} // their regions
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
				rg := unusable[l.InstanceID]
				if rg == "" && !listed[l.InstanceID] && l.Region != "" && away.avoided(l.Region) {
					rg = l.Region // launched there, not listed yet
				}
				if rg != "" {
					if !listed[l.InstanceID] {
						usable++
						total++
					}
					excludedCap += slots // it won't serve every waiter
					awayRegions[rg], pendingVM[l.InstanceID] = true, true
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
			if unusable[in.ID] != "" && time.Since(in.LaunchedAt) < p.R.BootTimeout.Duration {
				excludedCap += slots // it won't serve every waiter
				awayRegions[in.Region], pendingVM[in.ID] = true, true
				continue
			}
			if time.Since(in.LaunchedAt) < p.R.BootTimeout.Duration {
				pendingVM[in.ID] = true
				d.Pending++
				d.Capacity += slots
			}
		}
		// Booting VMs some waiters can't use serve only the others.
		d.Capacity += min(excludedCap, max(0, d.Demand-away.weightAvoiding(awayRegions)))
		d.Live = usable
		now = time.Now()
		d.Lead, d.LeadLearned = st.sched().launchLead(now)
		if !p.since.IsZero() {
			d.Waited = now.Sub(p.since)
		}
		d.QuotaErr = st.quotaBlocked(p.since)
		var wholeVM bool
		d.Ready, wholeVM = p.readyRoom(st, d.Demand-d.Capacity, demand, covered, whole, away, pendingVM, d.Waited, d.Lead, now)
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
		case d.Demand <= d.Capacity+d.Ready:
			d.Reason = fmt.Sprintf("%s free on ready VMs", plural(d.Ready, "slot"))
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
			d.Held, d.Deferred = true, d.Demand-d.Capacity-d.Ready
			d.Reason = fmt.Sprintf("%s on running VMs expected to free within %s, sooner than a new VM (~%s)",
				plural(d.Deferred, "slot"), ui.Dur(d.FreeIn), ui.Dur(d.Lead))
		case p.briefWait(st, &d, demand, whole, away, pendingVM, now):
			d.Held, d.Deferred = true, 1
			d.Reason = fmt.Sprintf("a full VM's jobs may end any moment (their ends unknown or just due): waiting up to %s more before launching (~%s)",
				ui.Dur(min(briefWaitFor, d.Lead)-d.Waited), ui.Dur(d.Lead))
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
// up), still respecting max_instances and the cap on live VMs (when set).
// It returns the launch IDs.
func (p *Pool) Reserve(ctx context.Context, n int) ([]string, error) {
	res, err := p.ReserveWhy(ctx, n)
	return res.IDs, err
}

// Reservation is what ReserveWhy recorded, and why it fell short.
type Reservation struct {
	IDs []string
	// Reason says why fewer launches were recorded than asked for (empty
	// when all were): at max_instances, or held by the cap on live VMs.
	Reason string
	// LiveCap is set when the cap on live VMs (twice max_instances,
	// counting draining and outdated VMs and launches not yet listed) held
	// them, rather than max_instances.
	LiveCap bool
}

// ReserveWhy is Reserve, also saying why fewer than n launches were
// recorded.
func (p *Pool) ReserveWhy(ctx context.Context, n int) (Reservation, error) {
	var res Reservation
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, true); err != nil {
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
		for len(res.IDs) < n {
			if !p.R.BelowMax(usable) {
				res.Reason = fmt.Sprintf("at max_instances (%d)", p.R.MaxInstances)
				break
			}
			if p.R.Capped() && total >= 2*p.R.MaxInstances {
				res.Reason, res.LiveCap = liveCapReason(st, p.R.MaxInstances), true
				break
			}
			var b [6]byte
			_, _ = rand.Read(b[:])
			id := hex.EncodeToString(b[:])
			st.Launches = append(st.Launches, launchRec{ID: id, Started: time.Now(), PID: os.Getpid()})
			res.IDs = append(res.IDs, id)
			usable++
			total++
		}
		return nil
	})
	return res, err
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
				st.Launches[i].Region = in.Region
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
	return p.registerAs(weight, false)
}

// RegisterExclusive adds a waiter for a whole VM (--exclusive), weighing
// one instance: freeing slots don't serve it, so Plan never holds a launch
// for them while it waits (even when its weight, the planning slot count,
// is 1 on a fleet of mixed sizes).
func (p *Pool) RegisterExclusive(weight int) (*Waiter, error) {
	return p.registerAs(weight, true)
}

func (p *Pool) registerAs(weight int, exclusive bool) (*Waiter, error) {
	return p.RegisterAway(weight, exclusive, nil)
}

// RegisterAway adds a waiter (an exclusive one with exclusive) that can't
// use VMs in the regions away: a VM booting there doesn't cover its demand.
func (p *Pool) RegisterAway(weight int, exclusive bool, away []string) (*Waiter, error) {
	var w *Waiter
	err := p.with(func(*poolState) error {
		var err error
		w, err = p.register(weight, exclusive, away)
		return err
	})
	return w, err
}

// exclusiveWaiter marks a whole-VM waiter's file name (older versions
// count it by its weight alone).
const exclusiveWaiter = ".x.w"

// register must run under pool.lock: waiterDemand (also under the lock)
// unlinks unlocked waiter files, so creation and locking must be atomic
// with respect to it.
func (p *Pool) register(weight int, exclusive bool, away []string) (*Waiter, error) {
	dir := filepath.Join(p.dir, "waiters")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	ext := ".w"
	if exclusive {
		ext = exclusiveWaiter
	}
	tag := ""
	if len(away) > 0 {
		tag = awayMark + strings.Join(away, awaySep)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s%s%s", os.Getpid(), hex.EncodeToString(b[:]), tag, ext))
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

// briefWaitFor bounds briefWait: a lone job waits this long at most for a
// full VM's slot before its fleet launches (#297).
const briefWaitFor = 15 * time.Second

// briefWait reports whether a lone waiting job, which has waited only
// moments, may wait a little longer for a slot on a full VM whose jobs
// have no prediction, or only just passed it (releaseSoon counts neither):
// one of them is likely to end before a new VM would serve, and a VM
// launched for that job would get no work (#297). Past briefWaitFor (or
// the launch's lead, if shorter) the demand launches as before. A burst,
// a whole-VM waiter, or a VM with jobs admitted beyond its slots launches
// at once, as does a fleet whose jobs are known to end later.
func (p *Pool) briefWait(st *poolState, d *LaunchDecision, demand int, whole []int, away awayDemand, pendingVM map[string]bool, now time.Time) bool {
	if demand != 1 || len(whole) > 0 || d.Demand-d.Capacity-d.Ready != 1 || p.since.IsZero() || d.Waited < 0 || d.Waited >= min(briefWaitFor, d.Lead) {
		return false
	}
	s := st.sched()
	for _, in := range st.Instances {
		n := st.Notes[in.ID]
		if in.State != provider.StateRunning || n == nil || !n.Ready || n.closed() || now.Before(n.SuspectUntil) ||
			pendingVM[in.ID] || s.Fences[in.ID] != nil || (in.Region != "" && away.avoided(in.Region)) {
			continue
		}
		// Full with this machine's own jobs, none beyond its slots: the
		// next slot to free is the waiting job's.
		if firm, unc := s.held(p.R, in.ID); unc > 0 || firm != s.units(p.R, in.ID) {
			continue
		}
		for _, a := range s.Attempts {
			if a.VM == in.ID && a.State == attAcquired && (a.Expect.IsZero() || (!a.Expect.After(now) && now.Sub(a.Expect) < briefWaitFor)) {
				return true
			}
		}
	}
	return false
}

// releaseSoon reports whether the demand this fleet isn't launching for,
// and its ready VMs can't take now (d.Demand - d.Capacity - d.Ready), can
// wait for slots on its running VMs, setting
// d.FreeIn: only while this process has waited less than a launch's lead
// (past it, demand is sustained), and only for slots that jobs predicted
// to end within the lead hold now. A job whose end is unknown, or already
// overdue, frees nothing here: the prediction has no say then. A
// whole-VM (exclusive) waiter that no empty VM covers (wholeVM) needs a
// VM to empty, which freeing slots don't show, so it never waits here. Jobs a VM's helper admitted
// beyond its slots (overflow, preparing beside them) take the first slots
// that free there: those releases free nothing for this demand.
func (p *Pool) releaseSoon(st *poolState, d *LaunchDecision, wholeVM bool, pendingVM map[string]bool, now time.Time) bool {
	need := d.Demand - d.Capacity - d.Ready
	if need <= 0 || wholeVM || p.since.IsZero() || d.Waited >= d.Lead {
		return false
	}
	s := st.sched()
	queued := map[string]int{} // eligible VMs: units held beyond their slots
	for _, in := range st.Instances {
		n := st.Notes[in.ID]
		if n == nil || !n.Ready || n.closed() || now.Before(n.SuspectUntil) || pendingVM[in.ID] || s.Fences[in.ID] != nil {
			continue
		}
		firm, _ := s.held(p.R, in.ID)
		queued[in.ID] = max(firm-s.units(p.R, in.ID), 0)
	}
	type release struct {
		at    time.Time
		units int
	}
	byVM := map[string][]release{}
	for _, a := range s.Attempts {
		if _, ok := queued[a.VM]; !ok || a.State != attAcquired || !a.Expect.After(now) || !a.Expect.Before(now.Add(d.Lead)) {
			continue
		}
		byVM[a.VM] = append(byVM[a.VM], release{a.Expect, max(a.Units, 1)})
	}
	var rs []release
	for vm, vrs := range byVM {
		sort.Slice(vrs, func(i, j int) bool { return vrs[i].at.Before(vrs[j].at) })
		skip := queued[vm]
		for _, r := range vrs {
			if skip > 0 {
				taken := min(skip, r.units)
				skip -= taken
				if r.units -= taken; r.units == 0 {
					continue
				}
			}
			rs = append(rs, r)
		}
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

// readyRoom is how much of need (the demand this fleet isn't launching
// for) its ready VMs can take now, by this machine's ledger, and whether a
// whole-VM waiter is left without a VM. Each whole-VM waiter takes a VM
// with nothing on it, the smallest first; the shared demand takes the free
// slots left, on the empty VMs no whole-VM waiter took and beside the jobs
// on the others. The demand higher-priority fleets cover (`covered`, not
// saying whose) comes off the shared waiters first: what is left is the
// hardest to place, so a whole-VM waiter no fleet has an empty VM for is
// never taken as covered by slots that serve only shared ones. A waiter that can't probe yet (behind an exclusive job
// in the queue) is demand all the same, so without this an idle fleet
// launches for it moments before it takes a free slot (#239).
//
// A VM counts only while a job would be placed on it: ready, open (or
// with an early drain the job would cancel), not suspect or just found busy, not being ended to make room or held back
// for an exclusive job (a fence), and not still counted as starting
// capacity (pendingVM). Slots held by this machine's live or uncertain
// attempts aren't free, an attempt still in flight included. And only
// while this process has waited less than a launch's lead: the ledger
// knows only this machine's jobs, so a VM another machine fills looks free
// here, and demand still waiting past the lead launches as before.
//
// Room on a VM in a region some waiters can't use (away: replays kept out
// of a silent region, #240) takes only the shared demand of the others,
// and no whole-VM waiter, so those replays don't wait out a lead for it.
func (p *Pool) readyRoom(st *poolState, need, total, covered int, whole []int, away awayDemand, pendingVM map[string]bool, waited, lead time.Duration, now time.Time) (covers int, wholeLeft bool) {
	shared := total
	for _, wt := range whole {
		shared -= wt
	}
	shared = max(shared, 0)
	upstream := min(max(covered, 0), shared)
	shared -= upstream
	upstream = max(covered, 0) - upstream
	var left []int // whole-VM waiters not covered upstream
	for _, wt := range whole {
		if upstream >= wt {
			upstream -= wt
			continue
		}
		left = append(left, wt)
	}
	if need <= 0 || waited >= lead {
		return 0, len(left) > 0
	}
	s := st.sched()
	var empty []int
	free, freeAway := 0, 0
	for _, w := range p.views(st, now) {
		// A VM whose drain can still be cancelled is room too: the next
		// job to probe takes it back.
		if !(w.eligible || w.reclaim) || !w.ready || pendingVM[w.in.ID] || s.Fences[w.in.ID] != nil {
			continue
		}
		if n := st.Notes[w.in.ID]; n.reaping(now) || now.Before(n.BusyUntil) {
			continue
		}
		if w.in.Region != "" && away.avoided(w.in.Region) {
			freeAway += max(w.units-w.firm-w.unc, 0)
			continue
		}
		if w.firm+w.unc == 0 {
			empty = append(empty, w.units)
			continue
		}
		free += max(w.units-w.firm-w.unc, 0)
	}
	sort.Ints(empty)
	placed := 0
	for _, wt := range left {
		if placed < len(empty) {
			covers += wt
			placed++
		}
	}
	for _, u := range empty[placed:] {
		free += u
	}
	awayW := 0 // shared demand that can't use freeAway
	for _, a := range away {
		if !a.whole {
			awayW += a.weight
		}
	}
	covers += min(shared, free+min(freeAway, max(0, shared-awayW)))
	return min(covers, need), placed < len(left)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// awayMark ends a waiter's file name with the regions it can't use (a
// replay kept out of a silent region, #240), joined by awaySep, before its
// extension. Older versions read only the weight inside.
const awayMark, awaySep = "~", "+"

// awayDemand is what the waiters that can't use some regions weigh.
type awayDemand []struct {
	weight  int
	whole   bool // a whole-VM waiter
	regions []string
}

// avoided reports whether a waiter can't use region.
func (a awayDemand) avoided(region string) bool {
	for _, w := range a {
		if slices.Contains(w.regions, region) {
			return true
		}
	}
	return false
}

// weightAvoiding is the demand of the waiters that can't use one of
// regions. A waiter that avoids any of those
// regions counts whole, even if it could use a VM in another of them: with
// several regions avoided at once that may cost a launch, never a wait.
func (a awayDemand) weightAvoiding(regions map[string]bool) int {
	n := 0
	for _, w := range a {
		for _, rg := range w.regions {
			if regions[rg] {
				n += w.weight
				break
			}
		}
	}
	return n
}

// awayRegions are the regions exclude keeps a request out of (regionKey).
func awayRegions(exclude map[string]bool) []string {
	var out []string
	for k := range exclude {
		if rg, ok := strings.CutPrefix(k, regionKey("")); ok && rg != "" && !strings.ContainsAny(rg, awaySep+awayMark+"./") {
			out = append(out, rg)
		}
	}
	sort.Strings(out)
	return out
}

func (p *Pool) waiterDemand() int {
	total, _ := p.waiters()
	return total
}

// waiters is the waiting demand in slots, and the weights of the waiters
// that need a whole VM (exclusive jobs on a fleet of several-slot VMs).
func (p *Pool) waiters() (total int, whole []int) {
	total, whole, _ = p.waitersAway()
	return total, whole
}

// waitersAway is waiters, with the waiters that can't use some regions.
func (p *Pool) waitersAway() (total int, whole []int, away awayDemand) {
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
		if w > 1 || strings.HasSuffix(e.Name(), exclusiveWaiter) {
			whole = append(whole, w)
		}
		if _, tail, ok := strings.Cut(e.Name(), awayMark); ok {
			tail, _, _ = strings.Cut(tail, ".")
			away = append(away, struct {
				weight  int
				whole   bool
				regions []string
			}{w, w > 1 || strings.HasSuffix(e.Name(), exclusiveWaiter), strings.Split(tail, awaySep)})
		}
	}
	return total, whole, away
}

// Snapshot returns the cached state (for status displays).
func (p *Pool) Snapshot(ctx context.Context, force bool) (poolView, error) {
	var v poolView
	err := p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, force); err != nil {
			return err
		}
		v.Instances, v.Unlisted = st.Instances, st.Unlisted
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
	// Unlisted: regions the listing couldn't list (poolState.Unlisted).
	// Their VMs are the ones last seen there, so absence from this view
	// doesn't mean a VM there is gone.
	Unlisted []string
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
