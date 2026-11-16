package client

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
)

// regionFleet is a fleet in two regions: the lost VM i-lost and its peers
// in ap-south-1, and VMs in us-east-1, all past booting unless young.
func regionFleet(young ...string) (*config.Runner, *fakeFleet) {
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot}
	r.BootTimeout.Duration = 5 * time.Minute
	old := time.Now().Add(-time.Hour)
	f := &fakeFleet{r: r}
	add := func(id, region string) {
		in := provider.Instance{ID: id, State: provider.StateRunning, Region: region, LaunchedAt: old, Target: provider.Target{Host: id}}
		for _, y := range young {
			if y == id {
				in.LaunchedAt = time.Now()
			}
		}
		f.insts = append(f.insts, in)
	}
	add("i-lost", "ap-south-1")
	add("i-peer1", "ap-south-1")
	add("i-peer2", "ap-south-1")
	add("i-us1", "us-east-1")
	add("i-us2", "us-east-1")
	return r, f
}

// stubProbes makes probeReachable answer for the VMs in up only, and
// records which VMs were probed.
func stubProbes(t *testing.T, up ...string) *[]string {
	t.Helper()
	old := probeReachable
	t.Cleanup(func() { probeReachable = old })
	var mu sync.Mutex
	probed := &[]string{}
	probeReachable = func(_ context.Context, _ *config.Runner, in provider.Instance, _ time.Duration) bool {
		mu.Lock()
		defer mu.Unlock()
		*probed = append(*probed, in.ID)
		for _, id := range up {
			if id == in.ID {
				return true
			}
		}
		return false
	}
	return probed
}

func lostLease(r *config.Runner, f provider.Fleet) *Lease {
	return &Lease{Runner: r, Provider: f, InstanceID: "i-lost", Instance: provider.Instance{ID: "i-lost", Region: "ap-south-1"}}
}

// #229: a lost VM's region counts as silent only when its other
// established VMs there fail a fresh probe too, and another region's VM
// answers one.
func TestRegionSilent(t *testing.T) {
	for _, c := range []struct {
		name   string
		young  []string
		drop   []string // VMs not in the listing
		up     []string
		region bool
		peers  int
	}{
		{"region silent, another answers", nil, nil, []string{"i-us2"}, true, 2},
		{"a peer answers", nil, nil, []string{"i-peer2", "i-us1"}, false, 0},
		{"the lost VM answers again", nil, nil, []string{"i-lost", "i-us1"}, false, 0},
		{"no region answers", nil, nil, nil, false, 0},
		{"alone in its region", nil, []string{"i-peer1", "i-peer2"}, []string{"i-us1"}, false, 0},
		{"its peers are still booting", []string{"i-peer1", "i-peer2"}, nil, []string{"i-us1"}, false, 0},
		{"one peer booting, the other silent", []string{"i-peer1"}, nil, []string{"i-us1"}, true, 1},
		{"no other region", nil, []string{"i-us1", "i-us2"}, nil, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, f := regionFleet(c.young...)
			kept := f.insts[:0]
			for _, in := range f.insts {
				drop := false
				for _, d := range c.drop {
					drop = drop || d == in.ID
				}
				if !drop {
					kept = append(kept, in)
				}
			}
			f.insts = kept
			probed := stubProbes(t, c.up...)
			got := regionSilent(lostLease(r, f))
			if (got != nil) != c.region {
				t.Fatalf("regional loss %+v, want %v (probed %v)", got, c.region, *probed)
			}
			if got == nil {
				return
			}
			if got.region != "ap-south-1" || got.answered != "us-east-1" || got.peers != c.peers {
				t.Fatalf("%+v", got)
			}
			for _, id := range c.young {
				for _, p := range *probed {
					if p == id {
						t.Errorf("probed %s, still booting", id)
					}
				}
			}
			msg := got.String()
			if !strings.Contains(msg, "ap-south-1 looks unreachable from this client") || !strings.Contains(msg, "one in us-east-1 did") {
				t.Errorf("message: %q", msg)
			}
		})
	}
	// Not a fleet VM with a region: nothing to compare, and no probe.
	probed := stubProbes(t)
	r, f := regionFleet()
	if got := regionSilent(&Lease{Runner: r, Provider: f, InstanceID: "i-lost"}); got != nil || len(*probed) != 0 {
		t.Fatalf("no region: %+v, probed %v", got, *probed)
	}
}

func stubRegionSilent(t *testing.T, rl *regionLoss) *int {
	t.Helper()
	old := regionSilentFn
	t.Cleanup(func() { regionSilentFn = old })
	calls := new(int)
	regionSilentFn = func(*Lease) *regionLoss { *calls++; return rl }
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
// launches nothing, and waits until the lost attempt has certainly
// stopped, unless interrupted.
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
		t.Fatalf("replayed after %s, before the lost attempt had certainly stopped", d)
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
