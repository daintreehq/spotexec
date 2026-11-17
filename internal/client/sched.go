package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// Scheduling state: the fleet's shared reservation ledger and placement
// policy, kept in pool.json beside the listing (docs/POOL.md). Every sx
// process on this machine records the VM it is about to acquire on before
// it contacts that VM, so the others see the slot as taken at once; the
// VM's helper stays the final word on what is free.

const schedVersion = 1

// VM roles in the placement policy. A role is a local admission policy,
// separate from the VM's health and from its own drain marker.
const (
	// roleRetained: ordinary jobs may be placed here.
	roleRetained = "retained"
	// roleStandby: surplus. Running jobs finish; new ones go elsewhere
	// unless the retained VMs are full, so the VM idles out.
	roleStandby = "standby"
	// roleDrainRequested: an early drain was committed locally. Never
	// reopened, even if the request to the VM times out, except by a job
	// that cancels the drain the VM took over (claim.Reclaim) while the
	// retained VMs are full.
	roleDrainRequested = "drain_requested"
	// roleDraining: the VM is draining (shutting down). Never reopened.
	roleDraining = "draining"
)

// Attempt states.
const (
	attReserved  = "reserved"  // target chosen; nothing sent to the VM yet
	attSent      = "sent"      // the acquire may have reached the helper
	attAcquired  = "acquired"  // the helper granted a slot
	attUncertain = "uncertain" // its process died after sending: may still hold a slot
)

const (
	// uncertainTTL bounds how long an attempt whose process died still
	// counts as holding a slot: the helper drops a silent client after
	// helper.ClientTimeout (45s), or up to four minutes more while the VM
	// itself stalls, then tears the session down. Uncertain slots never
	// stop a reservation (the helper is asked); they only keep the VM from
	// being let go.
	uncertainTTL = 6 * time.Minute
	// settleMargin: how long an attempt must have been uncertain before a
	// helper's holder list can settle it.
	settleMargin = 10 * time.Second
	// demandWindow is the history the shrink target looks at.
	demandWindow = 60 * time.Second
	// sustainSurplus: how long capacity must stay surplus before a VM is
	// let go.
	sustainSurplus = 60 * time.Second
	// policyEpoch: the controller acts at most this often (one VM per
	// epoch at most).
	policyEpoch = 10 * time.Second
	// minResidence: a VM that just joined the retained set (launched or
	// promoted) is not let go again before this.
	minResidence = 60 * time.Second
	// evidenceAge: how long a worktree's sync on a VM counts as a warm
	// cache there.
	evidenceAge = 2 * time.Hour
	// maxEvidence: worktrees remembered per VM.
	maxEvidence = 24
)

// schedState is pool.json's "sched" section.
type schedState struct {
	Version int `json:"version"`
	// Generation counts role changes, so a decision can tell it was made
	// against an older policy.
	Generation uint64               `json:"generation"`
	VMs        map[string]*vmSched  `json:"vms,omitempty"`
	Attempts   map[string]*claimRec `json:"attempts,omitempty"`
	// Fences: VMs held for an exclusive job at the head of the queue (VM
	// ID -> owner): no new shared jobs go there, so it empties.
	Fences map[string]*fence `json:"fences,omitempty"`
	// Warm: the VMs the pool manager keeps alive (min_instances), chosen
	// from the retained set so the daemon and sx agree.
	Warm []string `json:"warm,omitempty"`
	// Demand: the slots held or reserved over time (a step function).
	Demand       []demandPoint `json:"demand,omitempty"`
	SurplusSince time.Time     `json:"surplus_since,omitempty"`
	EpochAt      time.Time     `json:"epoch_at,omitempty"`
	// Seq numbers retained-set memberships, oldest member first: the core
	// order work is packed in, kept through an empty lull.
	Seq uint64 `json:"seq,omitempty"`
	// Model: recent lease durations by task and cache warmth
	// (sched_model.go).
	Model map[string]*durStat `json:"model,omitempty"`
	// PricesAt: when the VMs' prices were last looked up.
	PricesAt time.Time `json:"prices_at,omitempty"`
	// Leads: how long recent launches took to serve (sched_model.go), so
	// Plan can tell a slot freeing soon from one a new VM would beat.
	Leads []leadSample `json:"launch_leads,omitempty"`
}

// vmSched is what scheduling knows about one VM.
type vmSched struct {
	Role   string    `json:"role"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason,omitempty"`
	// Joined orders retained members (lower: longer in the set).
	Joined uint64 `json:"joined,omitempty"`
	// Hold: not let go before this (minimum residence after joining).
	Hold time.Time `json:"hold,omitempty"`
	// Slots: the helper-reported slot count (0 until a job reaches it).
	Slots int `json:"slots,omitempty"`
	// Admit: how many jobs its helper admits at once (Acquired.Admit):
	// jobs still preparing leave room for more, and the helper refuses
	// work once Slots of them run commands. 0: Slots (an older helper).
	Admit int `json:"admit,omitempty"`
	// LastStart/LastEnd: the latest job acquired/released here by this
	// machine (useful work, unlike keepalive activity).
	LastStart time.Time `json:"last_start,omitempty"`
	LastEnd   time.Time `json:"last_end,omitempty"`
	// Worktrees: worktree key -> when its files were last synced here.
	Worktrees map[string]time.Time `json:"worktrees,omitempty"`
	// Repos: repository -> when it last ran here (image and dependency
	// caches are per repository).
	Repos map[string]time.Time `json:"repos,omitempty"`
	// DrainOp names the committed early drain (roleDrainRequested).
	DrainOp string    `json:"drain_op,omitempty"`
	DrainAt time.Time `json:"drain_at,omitempty"`
	// DrainScheduled: the VM took the drain over (schedule-drain): its
	// watchdog ends it once idle result_retention, so it isn't asked again.
	DrainScheduled bool `json:"drain_scheduled,omitempty"`
	// Undrained: the early drain a job's acquire cancelled (reclaimed):
	// pool.json is saved after sched.json, so a retiring note left behind
	// is stale, not a committed drain.
	Undrained string `json:"undrained,omitempty"`
	// Price: the VM's hourly price in USD when known, and when it was seen.
	Price   float64   `json:"price,omitempty"`
	PriceAt time.Time `json:"price_at,omitempty"`
}

// attempt is one job's claim on a VM, from reservation to release.
type claimRec struct {
	Owner     string    `json:"owner"` // process incarnation (ownerAlive)
	PID       int       `json:"pid"`
	VM        string    `json:"vm"`
	State     string    `json:"state"`
	Units     int       `json:"units"` // slots it takes (a whole VM when exclusive)
	Exclusive bool      `json:"exclusive,omitempty"`
	Exact     bool      `json:"exact,omitempty"` // exec, pull, --same-slot
	Worktree  string    `json:"worktree,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Task      string    `json:"task,omitempty"`  // duration-model key
	Cache     int       `json:"cache,omitempty"` // cacheLevel of its VM when reserved
	Gen       uint64    `json:"gen"`
	At        time.Time `json:"at"`
	SentAt    time.Time `json:"sent_at,omitempty"`
	GotAt     time.Time `json:"acquired_at,omitempty"`
	LostAt    time.Time `json:"uncertain_at,omitempty"`
	// Expect: when it is predicted to release (zero: unknown).
	Expect time.Time `json:"expect,omitempty"`
}

// live reports whether the attempt still takes capacity.
func (a *claimRec) live() bool { return a != nil && a.State != "" }

type fence struct {
	Owner string    `json:"owner"`
	At    time.Time `json:"at"`
}

type demandPoint struct {
	T     time.Time `json:"t"`
	Units int       `json:"units"`
}

func (st *poolState) sched() *schedState {
	if st.Sched == nil || st.Sched.Version != schedVersion {
		st.Sched = &schedState{Version: schedVersion}
	}
	s := st.Sched
	if s.VMs == nil {
		s.VMs = map[string]*vmSched{}
	}
	if s.Attempts == nil {
		s.Attempts = map[string]*claimRec{}
	}
	if s.Fences == nil {
		s.Fences = map[string]*fence{}
	}
	return s
}

// --- process liveness -------------------------------------------------------

// Each sx process holds an exclusive flock on its own owner file for its
// whole life, named by its PID and a random incarnation (a reused PID is a
// different owner). A file nobody holds belongs to a dead process.
var (
	ownerOnce sync.Once
	ownerID   string
	ownerFile *os.File // held open (and locked) until exit; close-on-exec
)

func ownersDir() string { return filepath.Join(config.StateDir(), "owners") }

// processOwner is this process's owner ID ("" if its owner file couldn't
// be held: then this process records nothing in the ledger, rather than
// records the others would take for a dead process's).
func processOwner() string {
	ownerOnce.Do(func() {
		var b [6]byte
		_, _ = rand.Read(b[:])
		id := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
		if err := os.MkdirAll(ownersDir(), 0o700); err != nil {
			return
		}
		sweepOwners()
		// Locked under a temporary name, then renamed: a sweep never sees
		// it unlocked.
		tmp := filepath.Join(ownersDir(), id+".new")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil || os.Rename(tmp, filepath.Join(ownersDir(), id)) != nil {
			f.Close()
			_ = os.Remove(tmp)
			return
		}
		ownerFile, ownerID = f, id
	})
	return ownerID
}

// sweepOwners removes the owner files of processes that have exited.
func sweepOwners() {
	ents, _ := os.ReadDir(ownersDir())
	for _, e := range ents {
		path := filepath.Join(ownersDir(), e.Name())
		if strings.HasSuffix(e.Name(), ".new") {
			if fi, err := e.Info(); err != nil || time.Since(fi.ModTime()) < time.Minute {
				continue // being created
			}
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = os.Remove(path)
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		f.Close()
	}
}

// errNoOwner: this process can't take part in the ledger.
var errNoOwner = errors.New("no ledger owner file")

// ownerAlive reports whether the process that wrote owner still runs.
// Without its owner file (it couldn't be created) it falls back to the PID.
func ownerAlive(owner string, pid int) bool {
	if owner != "" && owner == processOwner() {
		return true
	}
	path := filepath.Join(ownersDir(), owner)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		return pidAlive(pid)
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		// Nobody holds it: that process is gone. Clean up.
		_ = os.Remove(path)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	return true
}

// --- ledger ---------------------------------------------------------------

// units is the capacity an instance offers: the helper-reported slots, or
// the runner's planning figure until a job has reached it.
func (s *schedState) units(r *config.Runner, id string) int {
	if v := s.VMs[id]; v != nil && v.Slots > 0 {
		return v.Slots
	}
	return slotsOf(r)
}

// admits is how many ordinary jobs an instance may hold at once: its
// helper's admission limit once a job has reached it, otherwise units.
// Within it the helper decides (it is full once units jobs run commands).
func (s *schedState) admits(r *config.Runner, id string) int {
	if v := s.VMs[id]; v != nil && v.Slots > 0 && v.Admit > v.Slots {
		return v.Admit
	}
	return s.units(r, id)
}

// held is the capacity live attempts take on a VM: firm (attempts of live
// processes) and uncertain (of dead ones, until reconciled).
func (s *schedState) held(r *config.Runner, id string) (firm, uncertain int) {
	for _, a := range s.Attempts {
		if a.VM != id || !a.live() {
			continue
		}
		u := a.Units
		if a.Exclusive {
			u = s.units(r, id)
		}
		if a.State == attUncertain {
			uncertain += u
		} else {
			firm += u
		}
	}
	return firm, uncertain
}

// inflight reports whether a VM has an acquire not yet answered.
func (s *schedState) inflight(id string) bool {
	for _, a := range s.Attempts {
		if a.VM == id && (a.State == attReserved || a.State == attSent) {
			return true
		}
	}
	return false
}

// protected reports whether something local needs the VM to stay up: a
// live attempt of any kind, or an exclusive fence.
func (s *schedState) protected(id string) bool {
	for _, a := range s.Attempts {
		if a.VM == id && a.live() {
			return true
		}
	}
	return s.Fences[id] != nil
}

// reconcileOwners settles the attempts and fences of processes that died:
// one never sent is dropped (nothing reached the VM); one that may have
// reached it becomes uncertain until uncertainTTL, never proof the slot is
// free before then.
func (s *schedState) reconcileOwners(now time.Time) bool {
	changed := false
	alive := map[string]bool{}
	isAlive := func(owner string, pid int) bool {
		v, ok := alive[owner]
		if !ok {
			v = ownerAlive(owner, pid)
			alive[owner] = v
		}
		return v
	}
	for id, a := range s.Attempts {
		switch {
		case !a.live():
			delete(s.Attempts, id)
			changed = true
		case a.State == attUncertain:
			if now.Sub(a.LostAt) > uncertainTTL {
				delete(s.Attempts, id)
				changed = true
			}
		case !isAlive(a.Owner, a.PID):
			if a.State == attReserved {
				delete(s.Attempts, id)
			} else {
				a.State, a.LostAt = attUncertain, now
			}
			changed = true
		}
	}
	for vm, f := range s.Fences {
		if !isAlive(f.Owner, 0) {
			delete(s.Fences, vm)
			changed = true
		}
	}
	return changed
}

// demandNow is the slots all live attempts take.
func (s *schedState) demandNow(r *config.Runner) int {
	n := 0
	for id := range s.VMs {
		f, u := s.held(r, id)
		n += f + u
	}
	// Attempts on VMs no longer listed still count until they end.
	for _, a := range s.Attempts {
		if a.live() && s.VMs[a.VM] == nil {
			n += max(1, a.Units)
		}
	}
	return n
}

// sample records current demand in the history, dropping what falls out of
// the window (but keeping the level in force at its start).
func (s *schedState) sample(r *config.Runner, now time.Time) {
	u := s.demandNow(r)
	if n := len(s.Demand); n == 0 || s.Demand[n-1].Units != u {
		s.Demand = append(s.Demand, demandPoint{T: now, Units: u})
	}
	cut := now.Add(-demandWindow)
	i := 0
	for i+1 < len(s.Demand) && !s.Demand[i+1].T.After(cut) {
		i++
	}
	s.Demand = s.Demand[i:]
}

// demandP90 is the time-weighted 90th percentile of demand over the window
// ending now: the level demand stayed at or below for 90% of the time (a
// burst of events doesn't count for more than its duration).
func (s *schedState) demandP90(now time.Time) int {
	if len(s.Demand) == 0 {
		return 0
	}
	start := now.Add(-demandWindow)
	type span struct {
		units int
		d     time.Duration
	}
	var spans []span
	for i, p := range s.Demand {
		from := p.T
		if from.Before(start) {
			from = start
		}
		to := now
		if i+1 < len(s.Demand) {
			to = s.Demand[i+1].T
		}
		if i == 0 && p.T.After(start) {
			// Before the first point: no history, so assume this level.
			from = start
		}
		if to.After(from) {
			spans = append(spans, span{p.Units, to.Sub(from)})
		}
	}
	if len(spans) == 0 {
		return s.Demand[len(s.Demand)-1].Units
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].units < spans[j].units })
	var total time.Duration
	for _, sp := range spans {
		total += sp.d
	}
	var acc time.Duration
	for _, sp := range spans {
		acc += sp.d
		if acc*10 >= total*9 {
			return sp.units
		}
	}
	return spans[len(spans)-1].units
}

// remember records that worktree (of repo) synced to a VM, keeping the
// newest maxEvidence worktrees.
func (v *vmSched) remember(worktree, repo string, now time.Time) {
	if worktree != "" {
		if v.Worktrees == nil {
			v.Worktrees = map[string]time.Time{}
		}
		v.Worktrees[worktree] = now
		trimEvidence(v.Worktrees, now)
	}
	if repo != "" {
		if v.Repos == nil {
			v.Repos = map[string]time.Time{}
		}
		v.Repos[repo] = now
		trimEvidence(v.Repos, now)
	}
}

func trimEvidence(m map[string]time.Time, now time.Time) {
	for k, t := range m {
		if now.Sub(t) > evidenceAge {
			delete(m, k)
		}
	}
	for len(m) > maxEvidence {
		oldest, ot := "", time.Time{}
		for k, t := range m {
			if oldest == "" || t.Before(ot) {
				oldest, ot = k, t
			}
		}
		delete(m, oldest)
	}
}

// warmFor reports whether the VM holds worktree's files (recent evidence).
func (v *vmSched) warmFor(worktree string, now time.Time) bool {
	if v == nil || worktree == "" {
		return false
	}
	t, ok := v.Worktrees[worktree]
	return ok && now.Sub(t) < evidenceAge
}

// hasRepo reports recent evidence of repo's caches on the VM.
func (v *vmSched) hasRepo(repo string, now time.Time) bool {
	if v == nil || repo == "" {
		return false
	}
	t, ok := v.Repos[repo]
	return ok && now.Sub(t) < evidenceAge
}

func newAttemptID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// claim is a job's request for capacity on one VM.
type claim struct {
	Exclusive bool
	Exact     bool
	Worktree  string // when the job syncs (its files will be there)
	Repo      string
	Task      string
	// Promote: the VM is standby and the policy chose to bring it back
	// (re-checked when reserving: only while no retained VM has room,
	// unless Latency: latency mode returning a worktree to its files).
	Promote bool
	Latency bool
	// Reclaim: the VM is drain_requested with its drain scheduled (this
	// operation), and the policy chose to take it back: the acquire
	// cancels the drain (protocol.Acquire.Undrain). Re-checked when
	// reserving like Promote.
	Reclaim string
	// Exclude: VMs this job may not use (not stored).
	Exclude map[string]bool
	// Force: reserve whatever the ledger says (legacy placement, exact
	// slots): the helper decides.
	Force bool
	// Overflow: the VM's slots are taken but its helper admits more jobs
	// to prepare beside them (schedState.admits); the acquire asks for
	// that (protocol.Acquire.Overflow).
	Overflow bool
}

// errNoRoom: the ledger shows the VM full, or it may no longer take
// ordinary work; skip it without contacting it.
var errNoRoom = errors.New("no room on this instance")

// errReaping: the VM is being terminated to make room (reach.go): not even
// an exact slot may reserve it.
var errReaping = errors.New("being terminated: this client couldn't reach it, and the fleet needed the room")

// ReserveOn records an attempt on instance in for this process, before any
// contact with it. Ordinary jobs are refused (errNoRoom) on a VM the ledger
// shows full with other live jobs, on one fenced for an exclusive job, and
// on one the policy no longer places on (unless c.Promote brings a standby
// VM back). It returns the attempt ID.
func (p *Pool) reserveOn(in provider.Instance, c claim) (string, error) {
	return p.reserveOnCtx(context.Background(), in, c)
}

// reserveOnCtx is reserveOn, but gives up waiting for the ledger's lock
// when ctx ends (an acquire out of queue time, #269).
func (p *Pool) reserveOnCtx(ctx context.Context, in provider.Instance, c claim) (string, error) {
	id := newAttemptID()
	owner := processOwner()
	if owner == "" {
		return "", errNoOwner
	}
	err := p.withCtx(ctx, func(st *poolState) error {
		s := st.sched()
		now := time.Now()
		s.reconcileOwners(now)
		v := s.vm(in.ID, now)
		if n := st.Notes[in.ID]; n.reaping(now) {
			switch {
			case n.ReapCommitted:
				return errReaping
			case !c.Force:
				return errNoRoom // being ended to make room: it can't be reached anyway
			}
			n.Reaping = time.Time{} // an exact slot wins: the reaper sees its claim and stands down
		}
		if !c.Force {
			if f := s.Fences[in.ID]; f != nil && f.Owner != owner && !c.Exact {
				return errNoRoom
			}
			switch v.Role {
			case roleDrainRequested:
				// Only to cancel the drain the VM took over, and only while
				// no retained VM has room.
				if c.Reclaim == "" || c.Reclaim != v.DrainOp || !reclaimable(v, st.Notes[in.ID], now) || p.retainedRoom(st, now, c) {
					return errNoRoom
				}
			case roleDraining:
				return errNoRoom
			case roleStandby:
				if !c.Promote || (!c.Latency && p.retainedRoom(st, now, c)) {
					// Not needed (any more): a retained VM has room.
					return errNoRoom
				}
				why := "promoted: retained VMs were full"
				if c.Latency {
					why = "promoted: latency mode, this worktree's files are here"
				}
				s.promote(in.ID, now, why)
			}
			firm, _ := s.held(p.R, in.ID)
			need, room := 1, s.units(p.R, in.ID)
			if c.Exclusive {
				need = room
			} else if c.Overflow {
				room = s.admits(p.R, in.ID)
			}
			if firm+need > room {
				return errNoRoom
			}
		}
		a := &claimRec{Owner: owner, PID: os.Getpid(), VM: in.ID, State: attReserved, Units: 1,
			Exclusive: c.Exclusive, Exact: c.Exact, Worktree: c.Worktree, Repo: c.Repo, Task: c.Task,
			Cache: cacheLevel(v, c, now), Gen: s.Generation, At: now}
		if c.Exclusive {
			a.Units = s.units(p.R, in.ID)
		}
		s.Attempts[id] = a
		s.sample(p.R, now)
		return nil
	})
	return id, err
}

// vm returns (creating) a VM's record; a new VM joins the retained set.
func (s *schedState) vm(id string, now time.Time) *vmSched {
	v := s.VMs[id]
	if v == nil {
		s.Seq++
		v = &vmSched{Role: roleRetained, Since: now, Reason: "new capacity", Joined: s.Seq, Hold: now.Add(minResidence)}
		s.VMs[id] = v
	}
	return v
}

// promote brings a standby VM back into the retained set.
func (s *schedState) promote(id string, now time.Time, why string) {
	v := s.vm(id, now)
	if v.Role != roleStandby {
		return
	}
	s.Seq++
	v.Role, v.Since, v.Reason, v.Joined, v.Hold = roleRetained, now, why, s.Seq, now.Add(minResidence)
	s.Generation++
}

// markSent records that an attempt's acquire is about to reach its VM.
// Written before the request goes out: an attempt that is still
// "reserved" when its process dies provably never reached the VM.
// It fails (and the request must not go out) if that can't be recorded.
func (p *Pool) markSent(id string) error {
	return p.markSentCtx(context.Background(), id)
}

// markSentCtx is markSent, but gives up waiting for the ledger's lock when
// ctx ends.
func (p *Pool) markSentCtx(ctx context.Context, id string) error {
	return p.withCtx(ctx, func(st *poolState) error {
		a := st.sched().Attempts[id]
		if a == nil {
			return errors.New("reservation lost")
		}
		if a.State == attReserved {
			a.State, a.SentAt = attSent, time.Now()
		}
		return nil
	})
}

// commitAttempt records the slot the helper granted.
func (p *Pool) commitAttempt(id string, acq protocol.Acquired, expect time.Time) {
	_ = p.with(func(st *poolState) error {
		s := st.sched()
		a := s.Attempts[id]
		if a == nil {
			return nil
		}
		now := time.Now()
		a.State, a.GotAt, a.Expect = attAcquired, now, expect
		if expect.IsZero() {
			if mid, _, ok := s.predict(a.Task, a.Cache); ok {
				a.Expect = now.Add(mid)
			}
		}
		v := s.vm(a.VM, now)
		if acq.MaxSlots > 0 {
			v.Slots, v.Admit = acq.MaxSlots, acq.Admit
			if a.Exclusive {
				a.Units = acq.MaxSlots
			}
		}
		v.LastStart = now
		v.remember(a.Worktree, a.Repo, now)
		if f := s.Fences[a.VM]; f != nil && f.Owner == a.Owner {
			delete(s.Fences, a.VM) // the exclusive job has its VM
		}
		s.sample(p.R, now)
		return nil
	})
}

// dropAttempt forgets an attempt the VM definitely refused (busy,
// draining, booting) or that never reached it.
func (p *Pool) dropAttempt(id string) {
	err := p.with(func(st *poolState) error {
		s := st.sched()
		if _, ok := s.Attempts[id]; ok {
			delete(s.Attempts, id)
			s.sample(p.R, time.Now())
		}
		return nil
	})
	if err != nil {
		p.retryLater(id)
	}
}

// loseAttempt marks an attempt whose outcome is unknown (the connection
// failed after the request went out): it may hold a slot.
func (p *Pool) loseAttempt(id string) {
	_ = p.with(func(st *poolState) error {
		if a := st.sched().Attempts[id]; a != nil && a.live() {
			a.State, a.LostAt = attUncertain, time.Now()
		}
		return nil
	})
}

// Releases that couldn't be saved (a full disk, say) are retried by this
// process's next transaction on the same fleet, so a live process never
// leaves a slot it gave back looking taken.
var (
	pendingMu  sync.Mutex
	pendingRel = map[string]map[string]bool{} // pool dir -> attempt IDs to forget ("fence:" + owner: its fences)
)

func (p *Pool) applyPending(st *poolState) []string {
	pendingMu.Lock()
	ids := pendingRel[p.dir]
	var out []string
	for id := range ids {
		out = append(out, id)
	}
	pendingMu.Unlock()
	if len(out) == 0 || st.Sched == nil {
		return out
	}
	s := st.sched()
	now := time.Now()
	for _, id := range out {
		if owner, ok := strings.CutPrefix(id, "fence:"); ok {
			for vm, f := range s.Fences {
				if f.Owner == owner {
					delete(s.Fences, vm)
				}
			}
			continue
		}
		if a := s.Attempts[id]; a != nil {
			if v := s.VMs[a.VM]; v != nil && a.State == attAcquired {
				v.LastEnd = now
			}
			delete(s.Attempts, id)
		}
	}
	s.sample(p.R, now)
	return out
}

func (p *Pool) pendingDone(applied []string) {
	if len(applied) == 0 {
		return
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	for _, id := range applied {
		delete(pendingRel[p.dir], id)
	}
	if len(pendingRel[p.dir]) == 0 {
		delete(pendingRel, p.dir)
	}
}

func (p *Pool) retryLater(id string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pendingRel[p.dir] == nil {
		pendingRel[p.dir] = map[string]bool{}
	}
	pendingRel[p.dir][id] = true
}

// releaseAttempt records that a job's slot is released.
func (p *Pool) releaseAttempt(id string) {
	err := p.with(func(st *poolState) error {
		s := st.sched()
		a := s.Attempts[id]
		if a == nil {
			return nil
		}
		now := time.Now()
		if v := s.VMs[a.VM]; v != nil && a.State == attAcquired {
			v.LastEnd = now
		}
		if a.State == attAcquired && !a.Exact {
			s.observe(a.Task, a.Cache, now.Sub(a.GotAt), now)
		}
		delete(s.Attempts, id)
		s.sample(p.R, now)
		return nil
	})
	if err != nil {
		p.retryLater(id)
	}
}

// settleHolders reconciles uncertain attempts on a VM against the holders
// its helper just reported: an attempt of this machine none of them
// matches has released.
//
// Only attempts that were already uncertain settleMargin before the probe
// was sent are settled: a newer one may still be in the helper's hands
// (it takes the token before it records the holder), or have become
// uncertain after the helper answered.
func (p *Pool) settleHolders(vm string, holders []protocol.Holder, probed time.Time) {
	host, _ := os.Hostname()
	_ = p.with(func(st *poolState) error {
		s := st.sched()
		settled := false
		defer func() {
			if settled {
				s.sample(p.R, time.Now())
			}
		}()
		for id, a := range s.Attempts {
			if a.VM != vm || a.State != attUncertain || !a.LostAt.Before(probed.Add(-settleMargin)) {
				continue
			}
			found := false
			for _, h := range holders {
				if h.Client.Attempt == id || (h.Client.Attempt == "" && h.Client.Host == host && h.Client.PID == a.PID) {
					found = true
				}
			}
			if !found {
				delete(s.Attempts, id)
				settled = true
			}
		}
		return nil
	})
}

// --- leases ---------------------------------------------------------------

// ledgerRef ties a lease to its attempt.
type ledgerRef struct {
	pool *Pool
	id   string
}

// release releases the attempt, once; it returns its pool the first time.
func (l *ledgerRef) release() *Pool {
	if l != nil && l.pool != nil && l.id != "" {
		l.pool.releaseAttempt(l.id)
		l.id = ""
		return l.pool
	}
	return nil
}

// forgetGone drops the records of VMs no longer listed (their attempts
// stay until their processes release them or the TTL ends them).
func (s *schedState) forgetGone(live map[string]bool) {
	for id := range s.VMs {
		if !live[id] {
			delete(s.VMs, id)
			delete(s.Fences, id)
			s.Generation++
		}
	}
	keep := s.Warm[:0]
	for _, id := range s.Warm {
		if live[id] {
			keep = append(keep, id)
		}
	}
	s.Warm = keep
}

// candidate is a VM to try for a job, with the claim to reserve there.
type candidate struct {
	provider.Instance
	claim  claim
	reason string // why it is tried (placement events)
	mode   string // score mode: heuristic | measured | legacy
}

func (c candidate) event() map[string]any {
	ev := map[string]any{}
	if c.reason != "" {
		ev["reason"] = c.reason
	}
	if c.mode != "" {
		ev["score_mode"] = c.mode
	}
	if c.claim.Promote {
		ev["promoted"] = true
	}
	if c.claim.Reclaim != "" {
		ev["drain_cancelled"] = true
	}
	return ev
}

// claimFor is the claim req makes on a fleet VM.
func claimFor(req AcquireRequest) claim {
	c := claim{Exclusive: req.Base.Exclusive, Repo: req.Base.RepoID, Task: req.Task, Exclude: req.Exclude}
	if !req.Base.KeepOwner {
		c.Worktree = req.Base.WorktreeKey // its files will be synced there
	}
	return c
}

// legacyCandidates wraps the legacy probe order: every attempt is recorded
// in the ledger, but the ledger never vetoes a probe.
func legacyCandidates(ins []provider.Instance, req AcquireRequest) []candidate {
	out := make([]candidate, 0, len(ins))
	for _, in := range ins {
		c := claimFor(req)
		c.Force = true
		out = append(out, candidate{Instance: in, claim: c, mode: "legacy"})
	}
	return out
}

// retainedRoom reports whether some retained VM could take the claim now
// (by this machine's ledger), so promoting a standby VM isn't needed.
func (p *Pool) retainedRoom(st *poolState, now time.Time, c claim) bool {
	s := st.sched()
	owner := processOwner()
	for _, w := range p.views(st, now) {
		if !w.eligible || w.v.Role != roleRetained || excluded(c.Exclude, w.in) {
			continue
		}
		if f := s.Fences[w.in.ID]; f != nil && f.Owner != owner {
			continue
		}
		if n := st.Notes[w.in.ID]; n != nil && now.Before(n.BusyUntil) {
			continue
		}
		used, need := w.firm, 1
		if c.Exclusive {
			// An exclusive probe finding a VM partly busy leaves no busy
			// hint: count uncertain slots too.
			used, need = w.firm+w.unc, w.units
		}
		if used+need <= w.units {
			return true
		}
	}
	return false
}
