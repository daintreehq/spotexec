package client

import (
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
	if _, ok := p.Fleet.(provider.RegionHolder); !ok {
		return regionReach{}
	}
	for rg, at := range st.Held {
		if now.Sub(at) > heldFor {
			delete(st.Held, rg)
		}
	}
	region := ""
	for _, in := range st.Instances {
		if in.ID == id {
			region = in.Region
			break
		}
	}
	if region == "" {
		return regionReach{} // not listed yet, or the fleet has no regions
	}
	switch o {
	case OutAcquired, OutBusy, OutBooting, OutBootFailed:
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

// applyReach passes a probe's verdict on to the fleet (outside the pool
// lock: the fleet keeps holds under its own).
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
		if n.Failures == 0 && n.reached() {
			return "" // it answered its last probe
		}
		age := now.Sub(in.LaunchedAt)
		if n.Failures < 2 || age < p.R.BootTimeout.Duration {
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

// reached reports whether the VM ever answered this client (Ready: it
// served, which notes from before Reached existed only say this way).
func (n *note) reached() bool { return n != nil && (n.Ready || !n.Reached.IsZero()) }

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
			n == nil || n.closed() || n.Failures < 2 || now.Sub(in.LaunchedAt) < p.R.BootTimeout.Duration ||
			st.sched().protected(in.ID) {
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
