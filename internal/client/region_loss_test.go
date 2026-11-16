package client

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
)

// regionFleet is a fleet in three regions: the lost VM i-lost and its
// peers in ap-south-1, three VMs in us-east-1 and one in eu-north-1.
func regionFleet() (*config.Runner, *fakeFleet) {
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot}
	f := &fakeFleet{r: r}
	for _, v := range [][2]string{{"i-lost", "ap-south-1"}, {"i-peer1", "ap-south-1"}, {"i-peer2", "ap-south-1"},
		{"i-us1", "us-east-1"}, {"i-us2", "us-east-1"}, {"i-us3", "us-east-1"}, {"i-eu1", "eu-north-1"}} {
		f.insts = append(f.insts, provider.Instance{ID: v[0], State: provider.StateRunning, Region: v[1], Target: provider.Target{Host: v[0]}})
	}
	return r, f
}

// stubProbes makes probeReachable report reach for each VM (Unreachable
// for any not named), and records which VMs were probed.
func stubProbes(t *testing.T, reach map[string]transport.Reach) *[]string {
	t.Helper()
	old := probeReachable
	t.Cleanup(func() { probeReachable = old })
	var mu sync.Mutex
	probed := &[]string{}
	probeReachable = func(_ context.Context, _ *config.Runner, in provider.Instance, _ time.Duration) transport.Reach {
		mu.Lock()
		defer mu.Unlock()
		*probed = append(*probed, in.ID)
		if r, ok := reach[in.ID]; ok {
			return r
		}
		return transport.Unreachable
	}
	return probed
}

// stubKnownGood makes knownGood every VM but those in bad.
func stubKnownGood(t *testing.T, f *fakeFleet, bad ...string) {
	t.Helper()
	old := knownGood
	t.Cleanup(func() { knownGood = old })
	knownGood = func(*config.Runner, provider.Fleet) map[string]bool {
		ok := map[string]bool{}
		for _, in := range f.insts {
			ok[in.ID] = true
		}
		for _, id := range bad {
			delete(ok, id)
		}
		return ok
	}
}

func lostLease(r *config.Runner, f provider.Fleet) *Lease {
	return &Lease{Runner: r, Provider: f, InstanceID: "i-lost", Target: provider.Target{Host: "i-lost"}, Instance: provider.Instance{ID: "i-lost", Region: "ap-south-1"}}
}

// #229: a lost VM's region counts as silent only when it and its other
// VMs there that this machine has reached before fail a fresh probe for
// want of a network path, and another region's VM answers one.
func TestRegionSilent(t *testing.T) {
	up, unknown := transport.Reachable, transport.ReachUnknown
	for _, c := range []struct {
		name     string
		bad      []string // not known good
		drop     []string // not in the listing
		reach    map[string]transport.Reach
		region   bool
		peers    int
		answered string
	}{
		{"region silent, another answers", nil, nil, map[string]transport.Reach{"i-us1": up}, true, 2, "us-east-1"},
		{"a peer answers", nil, nil, map[string]transport.Reach{"i-peer2": up, "i-us1": up}, false, 0, ""},
		{"a peer fails for another reason", nil, nil, map[string]transport.Reach{"i-peer1": unknown, "i-us1": up}, false, 0, ""},
		{"the lost VM answers again", nil, nil, map[string]transport.Reach{"i-lost": up, "i-us1": up}, false, 0, ""},
		{"the lost VM fails for another reason", nil, nil, map[string]transport.Reach{"i-lost": unknown, "i-us1": up}, false, 0, ""},
		{"not in the listing, the lost VM answers", nil, []string{"i-lost"}, map[string]transport.Reach{"i-lost": up, "i-us1": up}, false, 0, ""},
		{"no region answers", nil, nil, nil, false, 0, ""},
		{"another region fails for another reason", nil, nil, map[string]transport.Reach{"i-us1": unknown, "i-eu1": unknown}, false, 0, ""},
		{"only a later region answers", nil, nil, map[string]transport.Reach{"i-eu1": up}, true, 2, "eu-north-1"},
		{"alone in its region", nil, []string{"i-peer1", "i-peer2"}, map[string]transport.Reach{"i-us1": up}, false, 0, ""},
		{"its peers were never reached", []string{"i-peer1", "i-peer2"}, nil, map[string]transport.Reach{"i-us1": up}, false, 0, ""},
		{"one peer never reached, the other silent", []string{"i-peer1"}, nil, map[string]transport.Reach{"i-us1": up}, true, 1, "us-east-1"},
		{"no other region", nil, []string{"i-us1", "i-us2", "i-us3", "i-eu1"}, nil, false, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, f := regionFleet()
			stubKnownGood(t, f, c.bad...)
			kept := f.insts[:0]
			for _, in := range f.insts {
				if !strings.Contains(" "+strings.Join(c.drop, " ")+" ", " "+in.ID+" ") {
					kept = append(kept, in)
				}
			}
			f.insts = kept
			probed := stubProbes(t, c.reach)
			got := regionSilent(context.Background(), lostLease(r, f))
			if (got != nil) != c.region {
				t.Fatalf("regional loss %+v, want %v (probed %v)", got, c.region, *probed)
			}
			all := " " + strings.Join(*probed, " ") + " "
			for _, id := range c.bad {
				if strings.Contains(all, " "+id+" ") {
					t.Errorf("probed %s, never reached before", id)
				}
			}
			if strings.Contains(all, " i-us2 ") || strings.Contains(all, " i-us3 ") {
				t.Errorf("probed a second VM in one other region: %v", *probed)
			}
			if len(*probed) > 0 && !strings.Contains(all, " i-lost ") {
				t.Errorf("never probed the lost VM again: %v", *probed)
			}
			if got == nil {
				return
			}
			if got.region != "ap-south-1" || got.answered != c.answered || got.peers != c.peers {
				t.Fatalf("%+v", got)
			}
			msg := got.String()
			if !strings.Contains(msg, "ap-south-1 looks unreachable from this client") || !strings.Contains(msg, "one in "+c.answered+" did") || !strings.Contains(msg, fmt.Sprintf("its %d other", c.peers)) {
				t.Errorf("message: %q", msg)
			}
		})
	}
	// Not a fleet VM with a region, or a run already interrupted: nothing
	// to compare, and no probe.
	r, f := regionFleet()
	stubKnownGood(t, f)
	probed := stubProbes(t, map[string]transport.Reach{"i-us1": up})
	if got := regionSilent(context.Background(), &Lease{Runner: r, Provider: f, InstanceID: "i-lost"}); got != nil || len(*probed) != 0 {
		t.Fatalf("no region: %+v, probed %v", got, *probed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := regionSilent(ctx, lostLease(r, f)); got != nil || len(*probed) != 0 {
		t.Fatalf("interrupted: %+v, probed %v", got, *probed)
	}
}

// knownGood counts only VMs this machine has reached that still take runs.
func TestKnownGood(t *testing.T) {
	p, f := testPool(t, 4)
	p.Observe("i-ready", OutAcquired, time.Time{})
	p.Observe("i-busy", OutBusy, time.Time{})
	p.Observe("i-draining", OutAcquired, time.Time{})
	p.Observe("i-draining", OutDraining, time.Time{})
	p.Observe("i-booting", OutBooting, time.Time{})
	got := knownGood(p.R, f)
	if len(got) != 2 || !got["i-ready"] || !got["i-busy"] {
		t.Fatalf("known good: %v", got)
	}
}

func stubRegionSilent(t *testing.T, rl *regionLoss) *int {
	t.Helper()
	old := regionSilentFn
	t.Cleanup(func() { regionSilentFn = old })
	calls := new(int)
	regionSilentFn = func(context.Context, *Lease) *regionLoss { *calls++; return rl }
	return calls
}

// #229: a VM lost with its whole region after the command was handed over
// is replayed when its heartbeats showed its memory, without pressure; with
// no memory reading it isn't, and the warning puts it down to the region,
// not the command. Memory pressure and an earlier stage keep their verdicts,
// without probing the region.
func TestRegionLossVerdict(t *testing.T) {
	rl := &regionLoss{region: "ap-south-1", peers: 2, answered: "us-east-1"}
	calls := stubRegionSilent(t, rl)
	r, f := testFleet()
	retry := Options{RetryPreempt: true, Sync: "auto"}

	// No memory reading (an older helper, or no heartbeat carried one).
	bare := &Lease{Runner: r, Provider: f, InstanceID: "i-run"}
	got := whyNoReplay(bare, false, running, false)
	if got.code != "REGION_UNREACHABLE" || got.region != rl {
		t.Fatalf("no memory reading: %q (%q)", got.code, got.why)
	}
	if !strings.HasPrefix(got.why, "while running the command, although EC2 still reports it running: ap-south-1 looks unreachable") || strings.Contains(got.why, "overloaded") || strings.Contains(got.why, "--exclusive") || !strings.Contains(got.why, "never reported the VM's memory") {
		t.Fatalf("no memory reading: %q", got.why)
	}
	if ok, code, why := replayable("i-run", Result{noReplay: got, lostAt: running}, retry, false, 1); ok || code != "REGION_UNREACHABLE" || !strings.HasPrefix(why, "lost i-run while running the command, although EC2") || !strings.Contains(why, "not replaying") {
		t.Fatalf("no memory reading replayed: %v %q %q", ok, code, why)
	}

	// A healthy memory reading: replayed.
	sr, tgt := liveRunner(t, "rpc-memory")
	(&sshtest.Fake{State: os.Getenv("FAKE_SSH_STATE")}).Set("rpc-memory", "12000")
	_, s, err := Connect(context.Background(), sr, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	deadline := time.Now().Add(5 * time.Second)
	for lastMemory(s) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	healthy := &Lease{Runner: r, Provider: f, InstanceID: "i-run", Sess: s}
	for _, at := range []stage{running, starting, {phase: phaseSetup, dispatched: true, started: true}} {
		got := whyNoReplay(healthy, false, at, false)
		if got.code != "" || got.why != "" || got.region != rl {
			t.Fatalf("at %+v with memory: %q (%q) region %v, want a regional replay", at, got.code, got.why, got.region)
		}
		res := Result{noReplay: got, lostAt: at}
		if ok, code, why := replayable("i-run", res, retry, false, 1); !ok || code != "" || why != "" {
			t.Fatalf("at %+v: not replayed: %q %q", at, code, why)
		}
		// Ruled out by the options: still says the region went silent.
		ok, code, why := replayable("i-run", res, Options{Sync: "auto"}, false, 1)
		if ok || code != "REGION_UNREACHABLE" || !strings.HasPrefix(why, "lost i-run "+at.where()+": ap-south-1 looks unreachable") || strings.Contains(why, "not replaying") {
			t.Fatalf("at %+v, --no-retry: %v %q %q", at, ok, code, why)
		}
		if ok, code, _ := replayable("i-run", res, retry, false, 2); ok || code != "REGION_UNREACHABLE" {
			t.Fatalf("at %+v, second attempt: %v %q", at, ok, code)
		}
		if ok, code, _ := replayable("i-run", res, retry, true, 1); ok || code != "" {
			t.Fatalf("at %+v, interrupted: %v %q", at, ok, code)
		}
	}

	// Verdicts reached before the region is looked at.
	*calls = 0
	for _, c := range []struct {
		id      string
		noticed bool
		at      stage
	}{
		{"i-run", true, running},    // a Spot notice
		{"i-gone", false, running},  // the VM is gone
		{"i-run", false, depsCheck}, // the command wasn't handed over
		{"i-pend", false, starting}, // not running
		{"i-run", false, stage{phase: phaseSync}},
	} {
		if got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: c.id}, c.noticed, c.at, false); got.code != "" || got.region != nil {
			t.Errorf("%s noticed=%v at %+v: %q region %v", c.id, c.noticed, c.at, got.code, got.region)
		}
	}
	if *calls != 0 {
		t.Fatalf("probed the region %d times for verdicts that don't need it", *calls)
	}
}

// Memory pressure outranks a silent region: the run may still be what
// used the VM up.
func TestMemoryPressureOutranksRegionLoss(t *testing.T) {
	calls := stubRegionSilent(t, &regionLoss{region: "ap-south-1", peers: 1, answered: "us-east-1"})
	sr, tgt := liveRunner(t, "rpc-pressure")
	_, s, err := Connect(context.Background(), sr, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	deadline := time.Now().Add(5 * time.Second)
	for s.MemoryPressure(PressureWindow) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r, f := testFleet()
	if got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: "i-run", Sess: s}, false, running, false); got.code != "MEMORY_PRESSURE" || got.region != nil || *calls != 0 {
		t.Fatalf("%q (%q) region %v, %d probes", got.code, got.why, got.region, *calls)
	}
}

// A regional replay avoids the whole region (VMs listed later included),
// launches nothing, and waits until the helper there has given up on the
// lost attempt, unless interrupted.
func TestReplayAfterRegion(t *testing.T) {
	if regionReplayAfter <= helper.OrphanLimit {
		t.Fatalf("waits %s, no longer than a helper may outlive its client (%s)", regionReplayAfter, helper.OrphanLimit)
	}
	old := regionReplayAfter
	t.Cleanup(func() { regionReplayAfter = old })
	regionReplayAfter = 150 * time.Millisecond

	rl := &regionLoss{region: "ap-south-1", peers: 2, answered: "us-east-1"}
	res := Result{noReplay: noReplay{region: rl}, lostAt: running, lostWhen: time.Now()}
	att := attempt{runID: "r", n: 1, exclude: map[string]bool{}}
	rep := &warnRec{}
	start := time.Now()
	if !replayAfterRegion(context.Background(), rep, &att, res, "i-lost") {
		t.Fatal("not ready to replay")
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("replayed after %s, before the helper had given up on the lost attempt", d)
	}
	if !att.noBoot || !excluded(att.exclude, provider.Instance{ID: "i-new", Region: "ap-south-1"}) || excluded(att.exclude, provider.Instance{ID: "i-us", Region: "us-east-1"}) {
		t.Fatalf("attempt: %+v", att)
	}
	w := rep.take()
	if len(w) != 1 || !strings.HasPrefix(w[0], "RUNNER_LOST: lost i-lost while running the command: ap-south-1 looks unreachable") || !strings.Contains(w[0], "on a VM outside ap-south-1") {
		t.Fatalf("warnings: %q", w)
	}

	// Lost long enough ago: no wait.
	res.lostWhen = time.Now().Add(-time.Hour)
	start = time.Now()
	if !replayAfterRegion(context.Background(), rep, &attempt{exclude: map[string]bool{}}, res, "i-lost") || time.Since(start) > 50*time.Millisecond {
		t.Fatal("waited for an attempt long stopped")
	}
	// Interrupted while waiting.
	regionReplayAfter = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if replayAfterRegion(ctx, rep, &attempt{exclude: map[string]bool{}}, Result{noReplay: noReplay{region: rl}, lostAt: running, lostWhen: time.Now()}, "i-lost") {
		t.Fatal("replayed after an interrupt")
	}
}

// Excluding a region keeps every one of its VMs out of the candidates,
// including those listed after the exclusion was made.
func TestCandidatesExcludeRegion(t *testing.T) {
	p, f := testPool(t, 4)
	tgt := provider.Target{Host: "h"}
	f.insts = []provider.Instance{
		{ID: "i-ap", State: provider.StateRunning, Region: "ap-south-1", Target: tgt},
		{ID: "i-us", State: provider.StateRunning, Region: "us-east-1", Target: tgt},
		{ID: "i-none", State: provider.StateRunning, Target: tgt},
	}
	cands, usable, err := p.Candidates(context.Background(), map[string]bool{regionKey("ap-south-1"): true})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, in := range cands {
		ids = append(ids, in.ID)
	}
	if strings.Join(ids, ",") != "i-none,i-us" || usable != 2 {
		t.Fatalf("candidates %v, usable %d", ids, usable)
	}
}

// Rank and Fence keep an excluded region's VMs out too.
func TestRankAndFenceExcludeRegion(t *testing.T) {
	p, f := schedPool(t, 3, 2)
	f.insts[0].Region, f.insts[1].Region, f.insts[2].Region = "ap-south-1", "ap-south-1", "us-east-1"
	now := time.Now()
	p.loaded(t, func(st *poolState) {
		p.views(st, now)
		st.sched().EpochAt = now
	})
	away := map[string]bool{regionKey("ap-south-1"): true}
	cands, _, _, err := p.Rank(context.Background(), away, claim{Worktree: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID != "i-2" {
		t.Fatalf("candidates %+v, want only i-2", cands)
	}

	p2, f2 := schedPool(t, 2, 2)
	f2.insts[1].Region = "ap-south-1"
	p2.loaded(t, func(st *poolState) {
		p2.views(st, now)
		st.sched().VMs["i-0"].LastStart = now // Fence would pick i-1 (least recent)
	})
	if id := p2.Fence(context.Background(), away); id != "i-0" {
		t.Fatalf("fence on %q, want i-0: i-1 is in the excluded region", id)
	}
}
