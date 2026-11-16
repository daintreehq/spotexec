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
// failed a fresh probe, while a VM in another region answered. The run
// then wasn't lost to its own VM stalling, and nothing it did explains it.
type regionLoss struct {
	region   string // where the VMs went silent
	peers    int    // the region's other VMs that didn't answer either
	answered string // a region whose VM did answer
}

func (r *regionLoss) String() string {
	others := "VM"
	if r.peers > 1 {
		others = "VMs"
	}
	return fmt.Sprintf("%s looks unreachable from this client (its %d other %s there didn't answer either, while one in %s did), so the network to the region, not the run, is the likely cause", r.region, r.peers, others, r.answered)
}

// Probing a region after a loss: how many of its VMs (besides the lost
// one) and of other regions' to try, how long one probe may take (a new
// connection gets ConnectTimeout=10s), and how long the whole check may.
const (
	regionPeerProbes  = 6
	otherRegionProbes = 3
	regionProbeTime   = 15 * time.Second
	regionCheckTime   = 25 * time.Second
)

// probeReachable reports whether a VM answers SSH within d (tests replace
// it).
var probeReachable = func(ctx context.Context, r *config.Runner, in provider.Instance, d time.Duration) bool {
	return transport.New(in.Target, r.WorkRoot).Reachable(ctx, d)
}

// regionSilentFn is regionSilent (tests replace it).
var regionSilentFn = regionSilent

// regionSilent checks, once a fleet lease's VM has been lost to silence
// while the cloud still runs it, whether its whole region went silent: the
// VM and every other running VM of the fleet there that is past booting
// (up to regionPeerProbes of them; at least one) fail a fresh SSH probe,
// while a VM in another region answers one. A VM alone in its region
// proves nothing either way, nor does a client that reaches no region at
// all: nil then, as when the fleet can't be listed or the check runs out
// of time.
func regionSilent(lease *Lease) *regionLoss {
	f, ok := lease.Provider.(provider.Fleet)
	region := lease.Instance.Region
	if !ok || region == "" || lease.Runner == nil || lease.InstanceID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), regionCheckTime)
	defer cancel()
	list, err := f.List(ctx)
	if err != nil {
		return nil
	}
	var lostVM *provider.Instance
	var peers, others []provider.Instance
	for _, in := range list {
		if in.State != provider.StateRunning || in.Target.Host == "" {
			continue
		}
		switch {
		case in.ID == lease.InstanceID:
			v := in
			lostVM = &v
		case in.Region == region:
			// A VM still booting may not answer yet whatever the network.
			if time.Since(in.LaunchedAt) >= lease.Runner.BootTimeout.Duration && len(peers) < regionPeerProbes {
				peers = append(peers, in)
			}
		case in.Region != "" && len(others) < otherRegionProbes:
			others = append(others, in)
		}
	}
	if len(peers) == 0 || len(others) == 0 {
		return nil
	}
	same := peers
	if lostVM != nil {
		same = append(same, *lostVM) // it may answer again by now
	}
	var mu sync.Mutex
	sameAnswered, answered := false, ""
	var wg sync.WaitGroup
	probe := func(in provider.Instance, same bool) {
		defer wg.Done()
		ok := probeReachable(ctx, lease.Runner, in, regionProbeTime)
		mu.Lock()
		defer mu.Unlock()
		if ok && same {
			sameAnswered = true
		} else if ok && answered == "" {
			answered = in.Region
		}
	}
	for _, in := range same {
		wg.Add(1)
		go probe(in, true)
	}
	for _, in := range others {
		wg.Add(1)
		go probe(in, false)
	}
	wg.Wait()
	if sameAnswered || answered == "" || ctx.Err() != nil {
		return nil
	}
	return &regionLoss{region: region, peers: len(peers), answered: answered}
}

// regionReplayAfter is how long after losing a VM to a silent region a run
// waits before it is replayed: until the helper there has certainly given
// up on this client and stopped the run (helper.OrphanLimit), with time for
// its teardown, so the lost attempt can't still be running, or finish,
// alongside the replay. Tests shorten it.
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
// silent (res.noReplay.region): on a VM outside that region that is already
// there (a launch might well go to the silent region), once the lost
// attempt has certainly stopped. It waits for that, and is false when ctx
// ended first.
func replayAfterRegion(ctx context.Context, rep ui.Reporter, att *attempt, res Result, lost string) bool {
	rl := res.noReplay.region
	wait := regionReplayWait(res.lostWhen)
	rep.Warn("RUNNER_LOST", "lost %s %s: %s; replaying the run on a VM outside %s in %s, once the runner there has given up on this client and stopped the lost attempt", lost, res.lostAt.where(), rl, rl.region, ui.Dur(wait.Round(time.Second)))
	rep.Event("retry", map[string]any{"run_id": att.runID, "attempt": att.n + 1, "lost": lost, "region_unreachable": rl.region})
	att.exclude[regionKey(rl.region)] = true
	att.noBoot = true
	return sleepCtx(ctx, wait)
}
