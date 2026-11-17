package client

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

// Regions this client can't reach (#225). The cloud may report a region's
// VMs healthy while the client's network path to it is down: its VMs then
// never get work, and every launch there only adds another. When probes
// show that, the pool asks the fleet (a provider.RegionHolder) to hold new
// launches out of the region until a VM there answers again; see
// internal/provider/reach.go for how a hold lapses and is tried again.
const (
	// holdRenew: how often a region still found silent renews its hold.
	holdRenew = time.Minute
	// heldFor: how long the pool remembers asking for a hold (the fleet
	// forgets it within an hour of it lapsing).
	heldFor = 3 * time.Hour
	// reapAfter: a region renewed within this long is still held as far as
	// ending its VMs to make room goes.
	reapAfter = 5 * time.Minute
)

// regionReach is what a probe result asks of the fleet's region holds.
type regionReach struct {
	hold, why string // hold launches out of this region
	reached   string // end this region's hold
}

// judgeRegion decides, under the pool lock, what the probe of id says
// about its region: a reply ends a hold this pool asked for, and a failure
// that leaves the region silent holds it (renewed at most every
// holdRenew).
func (p *Pool) judgeRegion(st *poolState, id string, o Outcome, now time.Time) regionReach {
	return p.judgeRegionIn(st, id, "", o, now)
}

// judgeRegionIn is judgeRegion, with id's region given when the cached
// listing may not have it ("": look it up).
func (p *Pool) judgeRegionIn(st *poolState, id, region string, o Outcome, now time.Time) regionReach {
	if _, ok := p.Fleet.(provider.RegionHolder); !ok {
		return regionReach{}
	}
	for rg, at := range st.Held {
		if now.Sub(at) > heldFor {
			delete(st.Held, rg)
		}
	}
	for _, in := range st.Instances {
		if region == "" && in.ID == id {
			region = in.Region
			break
		}
	}
	if region == "" {
		return regionReach{} // not listed yet, or the fleet has no regions
	}
	switch o {
	case OutAcquired, OutBusy, OutBooting, OutBootFailed, outRefused, outDrainReply, outContact:
		if _, held := st.Held[region]; held {
			delete(st.Held, region)
			return regionReach{reached: region}
		}
	case OutUnreachable:
		why := p.silentRegion(st, region, now)
		if why == "" || now.Sub(st.Held[region]) < holdRenew {
			return regionReach{}
		}
		if st.Held == nil {
			st.Held = map[string]time.Time{}
		}
		st.Held[region] = now
		return regionReach{hold: region, why: why}
	}
	return regionReach{}
}

// applyReach passes a probe's verdict on to the fleet. Called under the
// pool lock (the fleet keeps holds under its own, taken after it).
func (p *Pool) applyReach(r regionReach) {
	rh, ok := p.Fleet.(provider.RegionHolder)
	if !ok {
		return
	}
	if r.reached != "" {
		rh.ReachedRegion(r.reached)
	}
	if r.hold != "" {
		rh.HoldRegion(r.hold, r.why)
	}
}

// silentRegion says why region's VMs can't be reached from this client
// ("" when they can, or it can't be told yet): none answered its last
// probe, and either several past boot_timeout failed theirs (twice or more
// in a row), or one has never answered since its launch. One VM that
// served and then went quiet may just be overloaded; that alone holds
// nothing.
func (p *Pool) silentRegion(st *poolState, region string, now time.Time) string {
	var silent []string
	never := ""
	var neverAge time.Duration
	for _, in := range st.Instances {
		n := st.Notes[in.ID]
		if in.Region != region || n == nil || n.closed() {
			continue // not probed yet, or out of service
		}
		if n.reached() && !n.Silent.After(n.Reached) {
			return "" // it answered its last probe
		}
		age := now.Sub(in.LaunchedAt)
		if n.Failures < 2 || n.Silent.IsZero() || age < p.R.BootTimeout.Duration {
			continue // may still be booting sshd
		}
		silent = append(silent, in.ID)
		if !n.reached() && never == "" {
			never, neverAge = in.ID, age
		}
	}
	sort.Strings(silent)
	switch {
	case len(silent) >= 2:
		ids := silent
		if len(ids) > 3 {
			ids = append(ids[:3:3], fmt.Sprintf("and %d more", len(silent)-3))
		}
		return fmt.Sprintf("%d VMs there don't answer SSH: %s", len(silent), strings.Join(ids, ", "))
	case never != "":
		return fmt.Sprintf("%s has never answered SSH, %s after its launch", never, neverAge.Round(time.Second))
	}
	return ""
}

// reaping reports whether a process is ending the VM to make room
// (marked by Plan; if the request fails the VM is back after a minute).
func (n *note) reaping(now time.Time) bool {
	return n != nil && !n.Reaping.IsZero() && now.Sub(n.Reaping) < time.Minute
}

// commitReap reports, just before it is terminated, whether a VM Plan
// chose to end still is (no run reserved it since: an exact slot clears
// the mark), and if so commits to it: from then on no claim reserves it.
func (p *Pool) commitReap(id string) bool {
	ok := false
	err := p.with(func(st *poolState) error {
		n := st.Notes[id]
		now := time.Now()
		ok = n.reaping(now) && !st.sched().protected(id)
		if ok {
			// From now: the exclusion covers the termination request.
			n.Reaping, n.ReapCommitted = now, true
		}
		return nil
	})
	return ok && err == nil // unsaved, other processes don't see the commitment
}

// Reached records that a VM answered this client outside a job's probe
// (`spotexec status`, which lists it afresh: its region is taken from in
// when the pool's cached listing doesn't have it yet), ending its region's
// hold.
func (p *Pool) Reached(in provider.Instance) {
	_ = p.with(func(st *poolState) error {
		now := time.Now()
		n := st.note(in.ID)
		n.SuspectUntil, n.Failures, n.Reached = time.Time{}, 0, now
		p.applyReach(p.judgeRegionIn(st, in.ID, in.Region, outContact, now))
		return nil
	})
}

// reached reports whether the VM ever answered this client (Ready: it
// served, which notes from before Reached existed only say this way).
func (n *note) reached() bool { return n != nil && (n.Ready || !n.Reached.IsZero()) }

// lostVM reports whether a listed VM has never answered this client, past
// boot_timeout, in a region held as unreachable.
func (p *Pool) lostVM(st *poolState, id string, now time.Time) bool {
	for _, in := range st.Instances {
		if in.ID != id {
			continue
		}
		n := st.Notes[id]
		at, held := st.Held[in.Region]
		return in.Region != "" && held && now.Sub(at) <= reapAfter && n != nil && !n.reached() && n.Failures >= 2 && !n.Silent.IsZero() &&
			now.Sub(in.LaunchedAt) >= p.R.BootTimeout.Duration
	}
	return false
}

// reapable returns a VM to end, and its region, so a fleet at
// max_instances can launch for waiting jobs: a VM past boot_timeout, in a
// region this client holds as unreachable, that failed its last probes
// and has no run or reservation of this client's on it ("" when there is
// none). Only while some other region can take the launch: otherwise those
// VMs are how the region is seen to answer again.
func (p *Pool) reapable(st *poolState, now time.Time) (string, string) {
	rh, ok := p.Fleet.(provider.RegionHolder)
	if !ok || len(st.Held) == 0 {
		return "", ""
	}
	var cands []provider.Instance
	for _, in := range st.Instances {
		n := st.Notes[in.ID]
		at, held := st.Held[in.Region]
		if in.Region == "" || !held || now.Sub(at) > reapAfter || in.State != provider.StateRunning ||
			n == nil || n.closed() || n.reaping(now) || n.Failures < 2 || n.Silent.IsZero() || n.Reached.After(n.Silent) ||
			now.Sub(in.LaunchedAt) < p.R.BootTimeout.Duration || st.sched().protected(in.ID) {
			continue
		}
		cands = append(cands, in)
	}
	if len(cands) == 0 {
		return "", ""
	}
	holds, open := rh.RegionHolds()
	if !open {
		return "", ""
	}
	// The oldest first: the nearest its own end anyway.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].LaunchedAt.Before(cands[j].LaunchedAt) })
	for _, in := range cands {
		if _, ok := holds[in.Region]; ok {
			return in.ID, in.Region
		}
	}
	return "", ""
}

// holdSilent holds launches out of region, found silent by a run that lost
// its VM there (regionSilent), as judgeRegion does when this pool's probes
// find it silent: recorded in the pool, so the hold ends as soon as a VM
// there answers. False when the fleet keeps no holds, or the pool's state
// couldn't be read before ctx ended.
func (p *Pool) holdSilent(ctx context.Context, region, why string) bool {
	if _, ok := p.Fleet.(provider.RegionHolder); !ok || region == "" {
		return false
	}
	return p.withCtx(ctx, func(st *poolState) error {
		if st.Held == nil {
			st.Held = map[string]time.Time{}
		}
		st.Held[region] = time.Now()
		p.applyReach(regionReach{hold: region, why: why})
		return nil
	}) == nil
}

// launchOutside renews region's hold, set by holdSilent, and reports
// whether a launch now goes outside it: the hold still stands (no VM there
// has answered since; one that did ended it, and a launch might go there
// again) and the fleet has another region open. False on a nil pool.
func (p *Pool) launchOutside(ctx context.Context, region, why string) bool {
	if p == nil {
		return false
	}
	rh, ok := p.Fleet.(provider.RegionHolder)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, regionProbeTime)
	defer cancel()
	held := false
	if err := p.withCtx(ctx, func(st *poolState) error {
		if _, held = st.Held[region]; held {
			st.Held[region] = time.Now()
			p.applyReach(regionReach{hold: region, why: why})
		}
		return nil
	}); err != nil || !held {
		return false
	}
	holds, open := rh.RegionHolds()
	_, stands := holds[region]
	return stands && open
}

// mayServe reports whether a VM of the fleet, as last listed, may yet
// serve a request kept off exclude's VMs and regions: one in service
// (busy, booting or ready) outside them, or a launch not yet listed. True
// when the pool's state can't be read.
func (p *Pool) mayServe(ctx context.Context, exclude map[string]bool) bool {
	may := false
	if err := p.withCtx(ctx, func(st *poolState) error {
		listed := map[string]bool{}
		for _, in := range st.Instances {
			listed[in.ID] = true
			if n := st.Notes[in.ID]; (n == nil || !n.closed()) && !excluded(exclude, in) {
				may = true
			}
		}
		for _, l := range st.Launches {
			if !l.Failed && (l.InstanceID == "" || !listed[l.InstanceID]) {
				may = true
			}
		}
		return nil
	}); err != nil {
		return true
	}
	return may
}
