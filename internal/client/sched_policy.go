package client

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
)

// The placement policy (docs/POOL.md, "Placement"). Two questions are
// answered separately:
//
//  1. Which VMs are worth keeping? The controller sizes a retained set to
//     recent demand. Surplus VMs become standby: their jobs finish, new
//     ones go elsewhere, so their idle clocks run out and the watchdog ends
//     them. Standby is reversible: when the retained VMs fill up, the
//     standby VM that left most recently comes back before any launch.
//  2. Where does this job go? Among retained VMs with room, the one with
//     this worktree's files (or this repository's caches), then a busy one
//     over an idle one, the tightest fit, the most recently used, and the
//     longest-standing member of the set.
//
// A worktree's previous VM is a cost input, not an order: it never keeps a
// surplus VM alive on its own.

// vmView is what the policy needs to know about one listed VM.
type vmView struct {
	in       provider.Instance
	v        *vmSched
	units    int
	firm     int  // slots taken by live attempts
	unc      int  // slots taken by uncertain attempts
	eligible bool // running, reachable, matching, not draining
	ready    bool // seen answering
}

// views lists the running VMs with their scheduling records, creating the
// records of new ones and syncing roles with what the notes say.
func (p *Pool) views(st *poolState, now time.Time) []vmView {
	s := st.sched()
	var out []vmView
	for _, in := range st.Instances {
		if in.State != provider.StateRunning || in.Target.Host == "" {
			continue
		}
		v := s.vm(in.ID, now)
		n := st.Notes[in.ID]
		if n != nil && n.Draining && v.Role != roleDraining {
			v.Role, v.Since, v.Reason = roleDraining, now, "draining"
			s.Generation++
		} else if n != nil && n.Retiring && v.Role != roleDraining && v.Role != roleDrainRequested {
			v.Role, v.Since, v.Reason = roleDrainRequested, now, "early drain requested"
			s.Generation++
		} else if v.Role == roleDrainRequested && (n == nil || !n.Retiring) {
			st.note(in.ID).Retiring = true // the two files disagreed: a committed drain wins
			n = st.Notes[in.ID]
		}
		f, u := s.held(p.R, in.ID)
		out = append(out, vmView{
			in: in, v: v, units: s.units(p.R, in.ID), firm: f, unc: u,
			eligible: !n.closed() && (v.Role == roleRetained || v.Role == roleStandby) && (n == nil || !now.Before(n.SuspectUntil)),
			ready:    n != nil && n.Ready,
		})
	}
	return out
}

// lastUseful is when a VM last did work for this machine.
func (v *vmSched) lastUseful() time.Time {
	if v.LastEnd.After(v.LastStart) {
		return v.LastEnd
	}
	return v.LastStart
}

// control runs the retained-set controller: at most once per policyEpoch,
// it keeps the warm subset and lets one surplus VM go once capacity has
// stayed above recent demand for sustainSurplus. Called under pool.lock;
// it does no I/O beyond reading the waiter files.
func (p *Pool) control(st *poolState, now time.Time) {
	s := st.sched()
	s.reconcileOwners(now)
	vs := p.views(st, now)
	s.sample(p.R, now)
	if p.R.SchedPolicy() == config.PolicyLegacy {
		s.Warm = nil
		return
	}
	if now.Sub(s.EpochAt) < policyEpoch && !now.Before(s.EpochAt) {
		return
	}
	s.EpochAt = now

	// The warm floor (min_instances): the longest-standing retained VMs,
	// ready ones first. Standby VMs come back to fill it.
	retained := func() []*vmView {
		var r []*vmView
		for i := range vs {
			if vs[i].eligible && vs[i].v.Role == roleRetained {
				r = append(r, &vs[i])
			}
		}
		sort.SliceStable(r, func(i, j int) bool {
			if r[i].ready != r[j].ready {
				return r[i].ready
			}
			return r[i].v.Joined < r[j].v.Joined
		})
		return r
	}
	ret := retained()
	if short := p.R.MinInstances - len(ret); short > 0 {
		for _, sv := range standbyOrder(vs) {
			if short == 0 {
				break
			}
			s.promote(sv.in.ID, now, "promoted: keeps min_instances warm")
			short--
		}
		ret = retained()
	}
	warm := map[string]bool{}
	s.Warm = s.Warm[:0]
	for _, w := range ret {
		if len(s.Warm) >= p.R.MinInstances {
			break
		}
		s.Warm = append(s.Warm, w.in.ID)
		warm[w.in.ID] = true
	}

	// Shrink: only with no queued work, no exclusive job wanting a whole
	// VM, and capacity to spare over recent demand.
	// An exclusive job waiting for a whole VM (a fence) stops all
	// shrinking; one already running only keeps its own units counted.
	// Capacity is what answers: a VM still booting can't stand in for one
	// that works.
	exclusive := len(s.Fences) > 0
	demand := max(s.demandNow(p.R), s.demandP90(now)) + p.R.Scheduling.SpareSlots
	capacity := 0
	for _, w := range ret {
		if w.ready {
			capacity += w.units
		}
	}
	var victims []*vmView
	if !exclusive && p.waiterDemand() == 0 {
		for _, w := range ret {
			if !w.ready || warm[w.in.ID] || now.Before(w.v.Hold) || s.inflight(w.in.ID) || s.Fences[w.in.ID] != nil || s.exactOn(w.in.ID) {
				continue
			}
			if capacity-w.units >= demand {
				victims = append(victims, w)
			}
		}
	}
	if len(victims) == 0 {
		s.SurplusSince = time.Time{}
		return
	}
	if s.SurplusSince.IsZero() || s.SurplusSince.After(now) {
		s.SurplusSince = now
		return
	}
	if now.Sub(s.SurplusSince) < sustainSurplus {
		return
	}
	// Not reset: one VM goes per epoch while the surplus lasts.
	victim := p.pickVictim(s, victims, ret, now)
	v := victim.v
	v.Role, v.Since, v.Hold = roleStandby, now, time.Time{}
	v.Reason = fmt.Sprintf("surplus: recent demand (%d slots) fits the other retained VMs (%d slots)", demand, capacity-victim.units)
	s.Generation++
}

// exactOn reports a live exec/pull/--same-slot attempt on a VM.
func (s *schedState) exactOn(id string) bool {
	for _, a := range s.Attempts {
		if a.VM == id && a.live() && a.Exact {
			return true
		}
	}
	return false
}

// pickVictim chooses which surplus VM to let go: an empty one before a
// busy one, one whose caches the others also hold, a dearer one, one
// nearer its max_lifetime (the survivors amortise their caches longer),
// the one used least recently.
func (p *Pool) pickVictim(s *schedState, victims, ret []*vmView, now time.Time) *vmView {
	unique := func(w *vmView) int {
		n := 0
		for k, t := range w.v.Worktrees {
			if now.Sub(t) > 30*time.Minute {
				continue
			}
			shared := false
			for _, o := range ret {
				if o != w && o.v.warmFor(k, now) {
					shared = true
					break
				}
			}
			if !shared {
				n++
			}
		}
		return n
	}
	lifeLeft := func(w *vmView) time.Duration {
		lt := p.R.MaxLifetime.Duration
		if lt <= 0 || w.in.LaunchedAt.IsZero() {
			return 1 << 62
		}
		// In half-hour steps: a few minutes' difference is no reason.
		return (lt - now.Sub(w.in.LaunchedAt)) / (30 * time.Minute)
	}
	sort.SliceStable(victims, func(i, j int) bool {
		a, b := victims[i], victims[j]
		if (a.firm+a.unc == 0) != (b.firm+b.unc == 0) {
			return a.firm+a.unc == 0
		}
		if ua, ub := unique(a), unique(b); ua != ub {
			return ua < ub
		}
		if pa, pb, ok := pricesDiffer(a.v, b.v, now); ok {
			return pa > pb
		}
		if la, lb := lifeLeft(a), lifeLeft(b); la != lb {
			return la < lb
		}
		if ta, tb := a.v.lastUseful(), b.v.lastUseful(); !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return a.in.ID < b.in.ID
	})
	return victims[0]
}

// pricesDiffer compares two VMs' known prices per slot, when both are
// known and differ by more than 5%.
func pricesDiffer(a, b *vmSched, now time.Time) (float64, float64, bool) {
	pa, oka := a.price(now)
	pb, okb := b.price(now)
	if !oka || !okb {
		return 0, 0, false
	}
	pa /= float64(max(1, a.Slots))
	pb /= float64(max(1, b.Slots))
	if pa == pb || (pa > pb && pa < pb*1.05) || (pb > pa && pb < pa*1.05) {
		return 0, 0, false
	}
	return pa, pb, true
}

// priceTTL: how long a recorded VM price is trusted.
const priceTTL = time.Hour

// price is the VM's hourly price, when recently known.
func (v *vmSched) price(now time.Time) (float64, bool) {
	if v == nil || v.Price <= 0 || now.Sub(v.PriceAt) > priceTTL {
		return 0, false
	}
	return v.Price, true
}

// standbyOrder lists eligible standby VMs in the order they come back: the
// one that left the retained set last first (it was useful most recently
// and has the most idle time left), then by recent use.
func standbyOrder(vs []vmView) []*vmView {
	var out []*vmView
	for i := range vs {
		if vs[i].eligible && vs[i].v.Role == roleStandby {
			out = append(out, &vs[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].v, out[j].v
		if !a.Since.Equal(b.Since) {
			return a.Since.After(b.Since)
		}
		if ta, tb := a.lastUseful(), b.lastUseful(); !ta.Equal(tb) {
			return ta.After(tb)
		}
		return out[i].in.ID < out[j].in.ID
	})
	return out
}

// Rank is the consolidating placement: the VMs to try for a job, best
// first, each with the claim to reserve there. usable and window are as
// for Candidates (window counts slots, from each VM's own slot count).
func (p *Pool) Rank(ctx context.Context, exclude map[string]bool, c claim) (out []candidate, usable, window int, err error) {
	err = p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		now := time.Now()
		p.control(st, now)
		s := st.sched()
		owner := processOwner()
		need := func(w vmView) int {
			if c.Exclusive {
				return w.units
			}
			return 1
		}
		type ranked struct {
			vmView
			cache int  // 2: this worktree's files, 1: this repository's caches
			ready bool // this machine installed the helper there (no upload first)
		}
		var keep, latency []ranked
		var standby []vmView
		for _, w := range p.views(st, now) {
			n := st.Notes[w.in.ID]
			if excluded(exclude, w.in) {
				continue
			}
			if !n.closed() && (w.v.Role == roleRetained || w.v.Role == roleStandby) {
				usable++
				window += w.units
			}
			if !w.eligible || (n != nil && now.Before(n.BusyUntil)) {
				continue
			}
			if f := s.Fences[w.in.ID]; f != nil && f.Owner != owner {
				continue // held for an exclusive job
			}
			if w.firm+need(w) > w.units {
				// Full, by this machine's own reservations: not probed, so
				// record what a busy probe would have (a VM serving at
				// capacity stops counting as launch capacity in Plan).
				if w.firm >= w.units && n != nil && n.Ready && !n.SawFull {
					n.SawFull = true
				}
				continue
			}
			r := ranked{vmView: w, ready: helperInstalled(w.in.Target, p.R.WorkRoot)}
			switch {
			case w.v.warmFor(c.Worktree, now):
				r.cache = 2
			case w.v.hasRepo(c.Repo, now):
				r.cache = 1
			}
			switch {
			case w.v.Role == roleRetained:
				keep = append(keep, r)
			case p.R.SchedMode() == config.ModeLatency && r.cache == 2:
				latency = append(latency, r)
			default:
				standby = append(standby, w)
			}
		}
		fenced := func(w vmView) bool { f := s.Fences[w.in.ID]; return f != nil && f.Owner == owner }
		sort.SliceStable(keep, func(i, j int) bool {
			a, b := keep[i], keep[j]
			if fa, fb := fenced(a.vmView), fenced(b.vmView); fa != fb {
				return fa // an exclusive job's own fenced VM first
			}
			if a.ready != b.ready {
				return a.ready // a first contact uploads the helper, which can take a while
			}
			if a.cache != b.cache {
				return a.cache > b.cache
			}
			if ba, bb := a.firm > 0, b.firm > 0; ba != bb {
				return ba // pack: a busy VM before an idle one
			}
			if fa, fb := a.units-a.firm-need(a.vmView), b.units-b.firm-need(b.vmView); fa != fb {
				return fa < fb // tightest fit
			}
			if ta, tb := a.v.lastUseful(), b.v.lastUseful(); !ta.Equal(tb) {
				return ta.After(tb)
			}
			if a.v.Joined != b.v.Joined {
				return a.v.Joined < b.v.Joined
			}
			return a.in.ID < b.in.ID
		})
		mode := "heuristic"
		kv := make([]vmView, len(keep))
		for i, r := range keep {
			kv[i] = r.vmView
		}
		if measured := p.measure(s, kv, c, now); measured != nil {
			// Reorder by measured cost; within a tie band of the cheapest,
			// the order above stands.
			idx := map[string]int{}
			for i, r := range keep {
				idx[r.in.ID] = i
			}
			sort.SliceStable(keep, func(i, j int) bool {
				ki, kj := keep[i], keep[j]
				// Never costed: this process's own fence, and a VM needing
				// no helper upload first (the lease model can't see one).
				if fa, fb := fenced(ki.vmView), fenced(kj.vmView); fa != fb {
					return fa
				}
				if ki.ready != kj.ready {
					return ki.ready
				}
				a, b := measured[ki.in.ID], measured[kj.in.ID]
				if a.rank != b.rank {
					return a.rank < b.rank
				}
				if a.rank == 0 {
					return idx[ki.in.ID] < idx[kj.in.ID]
				}
				return a.cost < b.cost
			})
			mode = "measured"
		}
		base := c
		base.Force = false
		add := func(w vmView, cl claim, why string) {
			out = append(out, candidate{Instance: w.in, claim: cl, reason: why, mode: mode})
		}
		// Latency mode: the worktree's own standby VM comes before the
		// retained ones, unless a retained VM already holds its files.
		warmRetained := false
		for _, r := range keep {
			warmRetained = warmRetained || r.cache == 2
		}
		addLatency := func() {
			for _, l := range latency {
				cl := base
				cl.Promote, cl.Latency = true, true
				add(l.vmView, cl, "latency mode: this worktree's files are on this standby VM")
			}
		}
		if !warmRetained {
			addLatency()
		}
		for _, r := range keep {
			add(r.vmView, base, retainedReason(r.cache, r.firm))
		}
		if warmRetained {
			addLatency()
		}
		for _, w := range standbyOrder(standby) {
			cl := base
			cl.Promote = true
			add(*w, cl, "retained VMs full: bringing back a standby VM")
		}
		return nil
	})
	return out, usable, window, err
}

func retainedReason(cache, firm int) string {
	switch {
	case cache == 2:
		return "this worktree's files are here"
	case cache == 1:
		return "this repository's caches are here"
	case firm > 0:
		return "packing onto a busy retained VM"
	}
	return "retained VM with room"
}

// Fence holds a VM for this process's exclusive job at the head of the
// queue: no new shared jobs go there, so it empties. It keeps a fence it
// already has; the VM is a retained one with the fewest slots taken.
// Launches go on regardless (an exclusive job mustn't wait behind a long
// job when new capacity could be ready sooner).
func (p *Pool) Fence(ctx context.Context, exclude map[string]bool) string {
	owner := processOwner()
	got := ""
	if owner == "" {
		return ""
	}
	_ = p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		s := st.sched()
		now := time.Now()
		s.reconcileOwners(now)
		vs := p.views(st, now)
		usable := map[string]bool{}
		for _, w := range vs {
			usable[w.in.ID] = w.eligible && w.v.Role == roleRetained && !excluded(exclude, w.in)
		}
		for vm, f := range s.Fences {
			if f.Owner != owner {
				continue
			}
			if usable[vm] {
				got = vm
				return nil
			}
			delete(s.Fences, vm) // it went draining or standby: fence another
		}
		var best *vmView
		for i := range vs {
			w := &vs[i]
			if !usable[w.in.ID] || s.Fences[w.in.ID] != nil {
				continue
			}
			if best == nil || w.firm+w.unc < best.firm+best.unc ||
				(w.firm+w.unc == best.firm+best.unc && w.v.LastStart.Before(best.v.LastStart)) {
				best = w
			}
		}
		if best != nil {
			s.Fences[best.in.ID] = &fence{Owner: owner, At: now}
			got = best.in.ID
		}
		return nil
	})
	return got
}

// Unfence drops this process's fences.
func (p *Pool) Unfence() {
	owner := processOwner()
	if owner == "" {
		return
	}
	err := p.with(func(st *poolState) error {
		s := st.sched()
		for vm, f := range s.Fences {
			if f.Owner == owner {
				delete(s.Fences, vm)
			}
		}
		return nil
	})
	if err != nil {
		p.retryLater("fence:" + owner) // dropped by this process's next transaction
	}
}

// WarmSet runs the controller and returns the VMs the pool manager keeps
// warm (in order) and each listed VM's role. With the legacy policy it
// returns nil: the manager keeps the oldest VMs warm, as that placement
// packs onto them.
func (p *Pool) WarmSet(ctx context.Context) (warm []string, roles map[string]string, err error) {
	err = p.withCtx(ctx, func(st *poolState) error {
		if err := p.refresh(ctx, st, false); err != nil {
			return err
		}
		p.control(st, time.Now())
		s := st.sched()
		if p.R.SchedPolicy() != config.PolicyLegacy {
			warm = append([]string{}, s.Warm...)
		}
		roles = map[string]string{}
		for id, v := range s.VMs {
			roles[id] = v.Role
		}
		return nil
	})
	return warm, roles, err
}

// Role is a VM's placement role, and why ("" when unknown).
type Role struct {
	Role   string `json:"role"`
	Reason string `json:"reason,omitempty"`
}

// Roles reports every known VM's role without changing anything.
func (p *Pool) Roles(ctx context.Context) map[string]Role {
	out := map[string]Role{}
	_ = p.withCtx(ctx, func(st *poolState) error {
		if st.Sched == nil {
			return nil
		}
		for id, v := range st.Sched.VMs {
			out[id] = Role{Role: v.Role, Reason: v.Reason}
		}
		return nil
	})
	return out
}

// standbySettle: how long a VM stays standby before an early drain may
// end it (a burst that comes straight back promotes it instead).
const standbySettle = 30 * time.Second

// BeginDrain commits an early drain of a standby VM locally, before the VM
// is asked (scheduling.early_drain): it re-checks, under the lock, that
// the VM is standby, settled, not warm, holds nothing for this machine
// (no attempt, uncertain or not, and no fence), that no job is queued, and
// that the retained VMs cover recent demand without it. From then on the
// VM is drain_requested and never placed on again, even if the request to
// it times out. A VM already drain_requested returns its operation, to be
// retried. ok is false when it may not be drained now.
func (p *Pool) BeginDrain(ctx context.Context, id string) (op string, ok bool, err error) {
	if !p.R.EarlyDrain() || p.R.SchedPolicy() != config.PolicyConsolidate || !p.R.Ephemeral() {
		return "", false, nil
	}
	err = p.withCtx(ctx, func(st *poolState) error {
		now := time.Now()
		s := st.sched()
		s.reconcileOwners(now)
		v := s.VMs[id]
		if v == nil {
			return nil
		}
		if v.Role == roleDrainRequested {
			st.note(id).Retiring = true
			op, ok = v.DrainOp, !v.DrainScheduled && now.Sub(v.DrainAt) >= drainRetry
			return nil
		}
		if v.Role != roleStandby || now.Sub(v.Since) < standbySettle || now.Before(v.DrainAt) || s.protected(id) || p.waiterDemand() > 0 {
			return nil
		}
		for _, w := range s.Warm {
			if w == id {
				return nil
			}
		}
		capacity := 0
		for _, w := range p.views(st, now) {
			if w.eligible && w.v.Role == roleRetained {
				capacity += w.units
			}
		}
		if capacity < max(s.demandNow(p.R), s.demandP90(now))+p.R.Scheduling.SpareSlots {
			return nil // it may be needed back
		}
		op, ok = newAttemptID(), true
		v.Role, v.Since, v.Reason, v.DrainOp, v.DrainAt = roleDrainRequested, now, "early drain requested", op, now
		v.DrainScheduled = false
		st.note(id).Retiring = true // no longer capacity: replacements may launch
		s.Generation++
		return nil
	})
	return op, ok, err
}

// drainRetry: how often a drain the VM refused (busy, or results still
// kept) is asked again.
const drainRetry = 30 * time.Second

// FinishDrain records the VM's answer to an early drain (a helper
// TryDrain result): drained ends the VM's placement for good (it powers
// off at its watchdog's next pass); scheduled leaves the wait to the VM
// (its watchdog ends it once idle result_retention), so it isn't asked
// again; busy or protected is asked again later. The VM stays
// drain_requested whatever the answer, except unsupported: nothing was
// changed on the VM, so it goes back to standby and idles out.
func (p *Pool) FinishDrain(id, op, result, detail string) {
	p.finishDrain(context.Background(), id, op, result, detail)
}

// finishDrain is FinishDrain, giving up when ctx ends (the drain stays
// committed and is asked again later, with the same operation).
func (p *Pool) finishDrain(ctx context.Context, id, op, result, detail string) {
	_ = p.withCtx(ctx, func(st *poolState) error {
		s := st.sched()
		v := s.VMs[id]
		if v == nil || v.DrainOp != op {
			return nil
		}
		now := time.Now()
		v.DrainAt = now
		switch result {
		case "drained", "already_draining":
			v.Role, v.Since, v.Reason = roleDraining, now, "early drain: idle, its results kept long enough"
			st.note(id).Draining = true
			s.Generation++
		case helper.DrainScheduled:
			v.DrainScheduled = true
			v.Reason = "early drain scheduled: ends once idle " + p.R.ResultRetention().String()
		case "unsupported":
			v.Role, v.Since, v.Reason, v.DrainOp = roleStandby, now, "surplus; early drain unsupported on this VM, so it idles out", ""
			v.DrainAt = now.Add(time.Hour) // don't keep asking
			st.note(id).Retiring = false
			s.Generation++
		default:
			v.Reason = "early drain requested: " + result
			if detail != "" {
				v.Reason += " (" + detail + ")"
			}
		}
		return nil
	})
}

// measuredRank is a candidate's place under the cost model: rank 0 within
// the tie band of the cheapest, 1 above it (ordered by cost), 2 when its
// lease may outlast the VM's lifetime.
type measuredRank struct {
	rank int
	cost float64
}

// measure scores every candidate (sched_model.go), or returns nil if any
// figure is unknown (then the fixed order stands).
func (p *Pool) measure(s *schedState, vs []vmView, c claim, now time.Time) map[string]measuredRank {
	if len(vs) < 2 {
		return nil
	}
	out := map[string]measuredRank{}
	best, band := math.Inf(1), 0.0
	for _, w := range vs {
		sc, ok := p.score(s, w, c, now)
		if !ok {
			return nil
		}
		price, _ := p.hourly(w.v, now)
		band = max(band, p.tieBand(price))
		r := measuredRank{rank: 1, cost: sc.cost}
		if sc.late {
			r.rank = 2
		} else if sc.cost < best {
			best = sc.cost
		}
		out[w.in.ID] = r
	}
	for id, r := range out {
		if r.rank == 1 && r.cost <= best+band {
			r.rank = 0
			out[id] = r
		}
	}
	return out
}
