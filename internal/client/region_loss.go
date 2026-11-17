package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// regionLoss says a lost VM's whole region went silent, as this client
// sees it (#229): the VM and every other established VM of its fleet there
// failed a fresh probe, while a VM in another region answered, or, with no
// VM running elsewhere to ask, the cloud's API in another region did
// (#240). The run then wasn't lost to its own VM stalling, and nothing it
// did explains it.
type regionLoss struct {
	region   string // where the VMs went silent
	peers    int    // the region's other VMs that didn't answer either
	answered string // a region whose VM (or, with api, whose API) did answer
	api      bool   // answered is a region whose cloud API answered
	// pool holds launches out of region (#225) while it is silent: nil
	// when its fleet keeps no region holds, or the hold couldn't be set.
	pool *Pool
}

func (r *regionLoss) String() string {
	others := "VM"
	if r.peers > 1 {
		others = "VMs"
	}
	answer := "one in " + r.answered + " did"
	if r.api {
		answer = "the cloud API in " + r.answered + " answered"
	}
	return fmt.Sprintf("%s looks unreachable from this client (its %d other %s there didn't answer either, while %s), so the network to the region, not the run, is the likely cause", r.region, r.peers, others, answer)
}

// why is how a hold on the region describes it.
func (r *regionLoss) why() string {
	others := "VM"
	if r.peers > 1 {
		others = "VMs"
	}
	return fmt.Sprintf("a VM lost with its %d other %s there", r.peers, others)
}

// Probing a region after a loss: how many of its VMs (besides the lost
// one) and of other regions (one VM each) to try, how long one probe may
// take (a new connection gets ConnectTimeout=10s), and how long the whole
// check may.
const (
	regionPeerProbes  = 6
	otherRegionProbes = 3
	regionProbeTime   = 15 * time.Second
	regionCheckTime   = 25 * time.Second
)

// probeReachable probes a VM over SSH within d (tests replace it).
var probeReachable = func(ctx context.Context, r *config.Runner, in provider.Instance, d time.Duration) transport.Reach {
	return transport.New(in.Target, r.WorkRoot).Reachable(ctx, d)
}

// regionSilentFn is regionSilent (tests replace it).
var regionSilentFn = regionSilent

// knownGood lists the fleet's VMs this machine has reached before and
// still places runs on (its pool notes: ready, not draining, retiring or
// mismatched): a VM like that going silent says something about the
// network; one that never answered, or is on its way out, doesn't.
var knownGood = func(ctx context.Context, r *config.Runner, f provider.Fleet) map[string]bool {
	ok := map[string]bool{}
	_ = NewPool(r, f).withCtx(ctx, func(st *poolState) error {
		for id, n := range st.Notes {
			if n != nil && n.Ready && !n.closed() {
				ok[id] = true
			}
		}
		return nil
	})
	return ok
}

// regionSilent checks, once a fleet lease's VM has been lost to silence
// while the cloud still runs it, whether its whole region went silent: the
// VM and every other VM of the fleet there that this machine has reached
// before and still uses (knownGood, up to regionPeerProbes of them; at
// least one) fail a fresh SSH probe for want of a network path, while a VM
// in another region answers one. With no VM running in another region to
// ask, the cloud's API answering in another of the fleet's regions shows
// the client's network is up instead (provider.RegionAnswerer, #240); the
// lost region's own API proves nothing, since it reaches the API, not the
// VMs. A VM elsewhere that doesn't answer isn't overruled by an API that
// does. A VM alone in its region proves nothing either way, nor does a
// client that reaches no region at all, nor a probe that failed for some
// other reason: nil then, as when the fleet can't be listed, ctx ends, or
// the check runs out of time.
func regionSilent(ctx context.Context, lease *Lease) *regionLoss {
	f, ok := lease.Provider.(provider.Fleet)
	region := lease.Instance.Region
	if !ok || region == "" || lease.Runner == nil || lease.InstanceID == "" || ctx.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, regionCheckTime)
	defer cancel()
	list, err := f.List(ctx)
	if err != nil {
		return nil
	}
	good := knownGood(ctx, lease.Runner, f)
	lostVM := lease.Instance
	lostVM.ID = lease.InstanceID
	if lostVM.Target.Host == "" {
		lostVM.Target = lease.Target
	}
	var peers, others []provider.Instance
	seen := map[string]bool{region: true}
	for _, in := range list {
		if in.State != provider.StateRunning || in.Target.Host == "" {
			continue
		}
		switch {
		case in.ID == lease.InstanceID:
			lostVM = in
		case in.Region == region:
			if good[in.ID] && len(peers) < regionPeerProbes {
				peers = append(peers, in)
			}
		case in.Region != "" && !seen[in.Region] && len(others) < otherRegionProbes:
			// One VM per region, so a region that answers gets asked.
			seen[in.Region] = true
			others = append(others, in)
		}
	}
	ans, _ := lease.Provider.(provider.RegionAnswerer)
	if len(peers) == 0 || (len(others) == 0 && ans == nil) {
		return nil
	}
	var mu sync.Mutex
	vetoed, answered, api := false, "", false
	var wg sync.WaitGroup
	probe := func(in provider.Instance, same bool) {
		defer wg.Done()
		got := probeReachable(ctx, lease.Runner, in, regionProbeTime)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case same && got != transport.Unreachable:
			vetoed = true // it answered, or failed for some other reason
		case !same && got == transport.Reachable && answered == "":
			answered = in.Region
		}
	}
	for _, in := range append(peers, lostVM) {
		wg.Add(1)
		go probe(in, true)
	}
	for _, in := range others {
		wg.Add(1)
		go probe(in, false)
	}
	if len(others) == 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, regionProbeTime)
			defer cancel()
			if rg := ans.AnswersOutside(actx, region); rg != "" && rg != region {
				mu.Lock()
				answered, api = rg, true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if vetoed || answered == "" || ctx.Err() != nil {
		return nil
	}
	return &regionLoss{region: region, peers: len(peers), answered: answered, api: api}
}

// holdSilent holds launches out of rl's region, found silent by the run
// that lost lease's VM there (#225): through the runner's pool, as its own
// probes would, so the hold ends as soon as a VM there answers again. It
// records the pool in rl when the fleet keeps region holds and the hold
// was set (tests replace it).
var holdSilent = func(ctx context.Context, lease *Lease, rl *regionLoss) {
	f, ok := lease.Provider.(provider.Fleet)
	if !ok || lease.Runner == nil {
		return
	}
	if _, ok := f.(provider.RegionHolder); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, regionProbeTime)
	defer cancel()
	p := NewPool(lease.Runner, f)
	if p.holdSilent(ctx, rl.region, rl.why()) {
		rl.pool = p
	}
}

// regionReplayAfter is how long after losing a VM to a silent region a run
// waits before it is replayed: past the longest a helper outlives the last
// word from its client before it tears the run down and exits
// (helper.OrphanLimit), with time for that teardown, so the lost attempt
// isn't still running alongside the replay. The client sent its last word
// no later than when it gave up (Result.lostWhen); bytes it had queued and
// that arrive later are followed by the connection's end, which stops the
// run at once. Tests shorten it.
var regionReplayAfter = helper.OrphanLimit + 30*time.Second

// regionReplayWait is how much longer a run lost at lostAt must wait
// before its replay.
func regionReplayWait(lostAt time.Time) time.Duration {
	return max(0, regionReplayAfter-time.Since(lostAt))
}

// regionKey is the Exclude key that keeps a request off every VM in a
// region, including VMs listed after the request was made.
func regionKey(region string) string { return "region:" + region }

// excluded reports whether in is one of exclude's VMs, or in one of its
// regions (regionKey).
func excluded(exclude map[string]bool, in provider.Instance) bool {
	return exclude[in.ID] || (in.Region != "" && exclude[regionKey(in.Region)])
}

// sleepCtx waits d, or until ctx ends: false then.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// replayAfterRegion readies att to replay a run lost to its region going
// silent (res.noReplay.region): on a VM outside that region, once the
// helper there has given up on the lost attempt (regionReplayAfter). It
// waits for that, and is false when ctx ended first. The replay may launch
// a VM when the region is still held (#225), renewed now, and the fleet
// has a region left open, so the launch goes elsewhere (#240: the whole
// fleet may have been in the silent region); otherwise it takes only a VM
// already running outside the region, since a launch might well go there.
func replayAfterRegion(ctx context.Context, rep ui.Reporter, att *attempt, res Result, lost string) bool {
	rl := res.noReplay.region
	wait := regionReplayWait(res.lostWhen)
	rep.Warn("RUNNER_LOST", "lost %s %s: %s; replaying the run on a VM outside %s in %s, once the runner there has given up on this client and torn the lost attempt down", lost, res.lostAt.where(), rl, rl.region, ui.Dur(wait.Round(time.Second)))
	rep.Event("retry", map[string]any{"run_id": att.runID, "attempt": att.n + 1, "lost": lost, "region_unreachable": rl.region})
	att.exclude[regionKey(rl.region)] = true
	att.noBoot = true
	if !sleepCtx(ctx, wait) {
		return false
	}
	att.noBoot = !rl.pool.launchOutside(ctx, rl.region, rl.why())
	return true
}
