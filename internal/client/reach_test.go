package client

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

// holdFleet is a fakeFleet that keeps region holds (a RegionHolder).
type holdFleet struct {
	*fakeFleet
	hmu     sync.Mutex
	holds   map[string]string // region -> why
	asked   []string          // HoldRegion calls, by region
	reached []string          // ReachedRegion calls
	closed  bool              // RegionHolds reports no region left open
}

func (h *holdFleet) HoldRegion(region, why string) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.holds[region] = why
	h.asked = append(h.asked, region)
}

func (h *holdFleet) ReachedRegion(region string) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	delete(h.holds, region)
	h.reached = append(h.reached, region)
}

func (h *holdFleet) RegionHolds() (map[string]provider.RegionHold, bool) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	out := map[string]provider.RegionHold{}
	for rg, why := range h.holds {
		out[rg] = provider.RegionHold{Why: why}
	}
	return out, !h.closed
}

// regionPool is a pool over a holdFleet listing insts (refreshed).
func regionPool(t *testing.T, max int, insts ...provider.Instance) (*Pool, *holdFleet) {
	t.Helper()
	p, f := testPool(t, max)
	h := &holdFleet{fakeFleet: f, holds: map[string]string{}}
	p.Fleet = h
	f.insts = insts
	if _, _, err := p.Candidates(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return p, h
}

func vmIn(id, region string, age time.Duration) provider.Instance {
	return provider.Instance{ID: id, Region: region, State: provider.StateRunning, Target: provider.Target{Host: "192.0.2.1"},
		LaunchedAt: time.Now().Add(-age)}
}

func failTwice(p *Pool, in provider.Instance) {
	p.Observe(in.ID, OutUnreachable, in.LaunchedAt)
	p.Observe(in.ID, OutUnreachable, in.LaunchedAt)
}

// Several VMs in a region going silent together hold launches out of it
// (once, however often they are probed), and an answer from any VM there
// ends the hold (#225).
func TestSilentRegionIsHeldUntilAVMAnswers(t *testing.T) {
	a1, a2, b1 := vmIn("i-a1", "r-a", time.Hour), vmIn("i-a2", "r-a", time.Hour), vmIn("i-b1", "r-b", time.Hour)
	p, h := regionPool(t, 4, a1, a2, b1)
	for _, in := range []provider.Instance{a1, a2, b1} {
		p.Observe(in.ID, OutAcquired, in.LaunchedAt) // all served once
	}
	failTwice(p, a1)
	if len(h.asked) != 0 {
		t.Fatalf("one served VM going quiet held its region: %v", h.asked)
	}
	failTwice(p, a2)
	if len(h.asked) != 1 || h.asked[0] != "r-a" || !strings.Contains(h.holds["r-a"], "2 VMs there don't answer SSH: i-a1, i-a2") {
		t.Fatalf("asked %v, holds %v", h.asked, h.holds)
	}
	failTwice(p, a1)
	if len(h.asked) != 1 {
		t.Fatalf("renewed within a minute: %v", h.asked)
	}
	// Another region answering says nothing about r-a.
	p.Observe(b1.ID, OutAcquired, b1.LaunchedAt)
	if len(h.reached) != 0 {
		t.Fatalf("reached %v", h.reached)
	}
	p.Observe(a2.ID, OutBusy, a2.LaunchedAt)
	if len(h.reached) != 1 || h.reached[0] != "r-a" || len(h.holds) != 0 {
		t.Fatalf("an answer from r-a didn't end its hold: reached %v, holds %v", h.reached, h.holds)
	}
	if _, held := readState(t, p).Held["r-a"]; held {
		t.Fatal("the pool still remembers the hold")
	}
	// Answered again, a later single failure holds nothing.
	failTwice(p, a1)
	if len(h.asked) != 1 {
		t.Fatalf("held again though i-a2 just answered: %v", h.asked)
	}
}

// One VM never reached since launch, past boot_timeout, holds its region;
// one still within boot_timeout doesn't.
func TestNeverReachedVMHoldsItsRegion(t *testing.T) {
	young := vmIn("i-young", "r-a", time.Minute)
	p, h := regionPool(t, 4, young)
	failTwice(p, young)
	if len(h.asked) != 0 {
		t.Fatalf("a VM still booting held its region: %v", h.asked)
	}
	old := vmIn("i-old", "r-b", 11*time.Minute)
	p2, h2 := regionPool(t, 4, old)
	failTwice(p2, old)
	if len(h2.asked) != 1 || !strings.Contains(h2.holds["r-b"], "i-old has never answered SSH") {
		t.Fatalf("asked %v, holds %v", h2.asked, h2.holds)
	}
}

// A booting reply is an answer too: it ends the hold.
func TestBootingReplyEndsHold(t *testing.T) {
	old := vmIn("i-old", "r-a", 11*time.Minute)
	p, h := regionPool(t, 4, old)
	failTwice(p, old)
	p.Observe(old.ID, OutBooting, old.LaunchedAt)
	if len(h.reached) != 1 || len(h.holds) != 0 {
		t.Fatalf("reached %v, holds %v", h.reached, h.holds)
	}
	if n := readState(t, p).Notes[old.ID]; n.Failures != 0 || n.Reached.IsZero() || n.Ready {
		t.Fatalf("note %+v", n)
	}
}

// A fleet at max_instances, all of it VMs this client can't reach, ends
// one of them so jobs can be served elsewhere; never one with a run on it,
// and not when every region is held (#225).
func TestCapFullOfUnreachableVMsMakesRoom(t *testing.T) {
	a1, a2 := vmIn("i-a1", "r-a", time.Hour), vmIn("i-a2", "r-a", 2*time.Hour)
	p, h := regionPool(t, 2, a1, a2)
	failTwice(p, a1)
	failTwice(p, a2)
	d := planOnce(t, p)
	if d.Launch || len(d.Reap) != 1 || d.Reap[0] != "i-a2" || !strings.Contains(d.Reason, "can't reach") {
		t.Fatalf("want the oldest unreachable VM reaped: %+v", d)
	}

	// i-a2 has a run in flight (as far as this client knows): only i-a1.
	_ = p.with(func(st *poolState) error {
		st.sched().Attempts["x"] = &claimRec{VM: "i-a2", State: "sent", At: time.Now(), SentAt: time.Now()}
		return nil
	})
	if d := planOnce(t, p); len(d.Reap) != 1 || d.Reap[0] != "i-a1" {
		t.Fatalf("reaped a VM with a run on it: %+v", d)
	}

	// Nowhere else to launch: keep them (they show when r-a answers again).
	h.closed = true
	if d := planOnce(t, p); len(d.Reap) != 0 {
		t.Fatalf("reaped with every region held: %+v", d)
	}
	h.closed = false

	// Ended: the next plan launches (elsewhere: the fleet skips r-a).
	_ = p.Fleet.Terminate(context.Background(), "i-a1")
	p.Observe("i-a1", OutDraining, time.Time{})
	if d := planOnce(t, p); !d.Launch {
		t.Fatalf("no launch after making room: %+v", d)
	}
}

// Below max_instances nothing is reaped: the launch simply goes elsewhere.
func TestNoReapBelowMax(t *testing.T) {
	a1, a2 := vmIn("i-a1", "r-a", time.Hour), vmIn("i-a2", "r-a", time.Hour)
	p, _ := regionPool(t, 4, a1, a2)
	failTwice(p, a1)
	failTwice(p, a2)
	if d := planOnce(t, p); !d.Launch || len(d.Reap) != 0 {
		t.Fatalf("%+v", d)
	}
}

// Fleets without regions (or that can't hold them) are judged as before.
func TestNoRegionNoHold(t *testing.T) {
	old := vmIn("i-old", "", time.Hour)
	p, h := regionPool(t, 1, old)
	failTwice(p, old)
	if len(h.asked) != 0 {
		t.Fatalf("held %v", h.asked)
	}
	if d := planOnce(t, p); len(d.Reap) != 0 {
		t.Fatalf("%+v", d)
	}
}

// A launch whose VM never answered, in a held region, stops covering
// waiting jobs at once (not a minute after boot_timeout), so the next
// launch goes elsewhere.
func TestLostLaunchStopsCoveringDemand(t *testing.T) {
	old := vmIn("i-old", "r-a", 11*time.Minute)
	p, _ := regionPool(t, 4, old)
	_ = p.with(func(st *poolState) error {
		st.Launches = []launchRec{{ID: "l1", InstanceID: "i-old", Started: old.LaunchedAt, PID: os.Getpid()}}
		return nil
	})
	if d := planOnce(t, p); d.Launch || d.Pending != 1 {
		t.Fatalf("before any failed probe: %+v", d)
	}
	failTwice(p, old)
	if d := planOnce(t, p); !d.Launch || d.Pending != 1 {
		t.Fatalf("the lost launch still covers demand: %+v", d)
	}
}
