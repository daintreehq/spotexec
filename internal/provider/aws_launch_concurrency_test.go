package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// launchAll starts n launches and returns their results.
func launchAll(n int, launch func() error) <-chan error {
	out := make(chan error, n)
	for range n {
		go func() { out <- launch() }()
	}
	return out
}

// noMore fails if a launch could take another slot, or another
// RunInstances arrives within a short wait.
func noMore(t *testing.T, a *AWSSpot, h *held, why string) {
	t.Helper()
	for i := range launchSlots {
		release, ok, err := tryLockFile(a.lockPath(fmt.Sprintf("up.slot.%d.lock", i)), syscall.LOCK_EX)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			release()
			t.Fatalf("slot %d free while %d launches are sending", i, launchSlots)
		}
	}
	select {
	case r := <-h.started:
		t.Fatalf("a launch reached %s %s", r, why)
	case <-time.After(200 * time.Millisecond):
	}
}

// Independent launches of one runner reach EC2 together, launchSlots at a
// time: the rest go as those finish.
func TestGlobalLaunchesOverlap(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}, {"r-one-1b", "c6a.2xlarge", "0.06"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.07"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g.Prices(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h := newHeld()
	f.onRun = h.onRun
	t.Cleanup(h.free)
	const n = 8
	done := launchAll(n, func() error { _, err := g.Launch(context.Background(), t.Logf); return err })
	for range launchSlots {
		h.wait(t) // these are all sending at once
	}
	noMore(t, g.home(), h, "past the launch slots")
	h.free()
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.callsOf("RunInstances")); got != n {
		t.Fatalf("RunInstances calls = %d, want %d", got, n)
	}
}

// A single-region runner's launches overlap too.
func TestRegionalLaunchesOverlap(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	h := newHeld()
	f.onRun = h.onRun
	t.Cleanup(h.free)
	const n = 6
	done := launchAll(n, func() error { _, err := a.Launch(context.Background(), t.Logf); return err })
	for range launchSlots {
		h.wait(t)
	}
	noMore(t, a, h, "past the launch slots")
	h.free()
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// held makes RunInstances wait until freed, telling started about each
// request as it arrives.
type held struct {
	started chan string
	release chan struct{}
	once    sync.Once
}

func newHeld() *held { return &held{started: make(chan string, 16), release: make(chan struct{})} }

func (h *held) onRun(region string) {
	h.started <- region
	<-h.release
}

// free lets every held request (and every later one) answer.
func (h *held) free() { h.once.Do(func() { close(h.release) }) }

// wait waits for the next request to arrive.
func (h *held) wait(t *testing.T) string {
	t.Helper()
	select {
	case r := <-h.started:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a RunInstances request")
		return ""
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Down waits for the launches already sending, and launches that come
// after it announced itself wait for it: it terminates what the earlier
// ones made, and nothing launches behind its back.
func TestDownCoordinatesWithOverlappingLaunches(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	if _, err := g.Prices(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h := newHeld()
	f.onRun = h.onRun
	t.Cleanup(h.free)
	launched := make(chan error, 3)
	launch := func() {
		_, err := g.Launch(context.Background(), t.Logf)
		launched <- err
	}
	go launch()
	go launch()
	h.wait(t)
	h.wait(t) // both are sending
	downDone := make(chan error, 1)
	go func() { downDone <- g.Down(context.Background(), t.Logf) }()
	pending := g.home().lockPath(upWriterPending)
	waitFor(t, "Down to announce itself", func() bool { _, err := os.Stat(pending); return err == nil })
	go launch() // after Down announced itself
	select {
	case err := <-downDone:
		t.Fatalf("Down finished (%v) while launches were sending", err)
	case r := <-h.started:
		t.Fatalf("a launch reached %s while Down was waiting", r)
	case <-time.After(300 * time.Millisecond):
	}
	h.free()
	for range 2 {
		if err := <-launched; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-downDone; err != nil {
		t.Fatal(err)
	}
	// Down ended both earlier VMs; the third launch went after it.
	terms := f.callsOf("TerminateInstances")
	if len(terms) != 1 || strings.Count(terms[0], "i-") != 2 {
		t.Fatalf("terminations %v, want both earlier VMs", terms)
	}
	if err := <-launched; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	left := len(f.instances["r-one-1"])
	f.mu.Unlock()
	if left != 1 {
		t.Fatalf("%d instance(s) left, want the one launched after Down", left)
	}
	if _, err := os.Stat(pending); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("writer marker left behind: %v", err)
	}
}

// A writer marker left by a process that died doesn't block launches.
func TestLaunchClearsDeadWritersMarker(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}}}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.lockPath(upWriterPending), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.Launch(ctx, t.Logf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.lockPath(upWriterPending)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dead writer's marker still there: %v", err)
	}
}

// A launch waiting for the lock gives up when its context ends.
func TestLaunchLockHonorsContext(t *testing.T) {
	f := &fakeRegionsEC2{}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	unlock, err := a.lockUp(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.lockLaunch(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	unlock()
	// Every slot taken.
	for range launchSlots {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		release, err := a.lockLaunch(sctx)
		scancel()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.lockLaunch(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with every slot taken: err = %v, want the deadline", err)
	}
}

// Overlapping launches in one region can't both spend the same quota
// headroom: the second counts the first's claim until listings show its VM.
func TestOverlappingLaunchesShareQuota(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}},
		quota:  map[string]int{"r-one-1": 8}, // one 8-vCPU VM
	}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	h := newHeld()
	f.onRun = h.onRun
	t.Cleanup(h.free)
	first := launchAll(1, func() error { _, err := a.Launch(context.Background(), t.Logf); return err })
	h.wait(t)
	second := launchAll(1, func() error { _, err := a.Launch(context.Background(), t.Logf); return err })
	select {
	case err := <-second:
		var qe *QuotaError
		if !errors.As(err, &qe) {
			t.Fatalf("second launch: %v, want the quota refusal", err)
		}
		if !strings.Contains(err.Error(), "8 of 8 vCPUs in use: 8 by this machine") {
			t.Fatalf("refusal doesn't count the first launch as this machine's: %v", err)
		}
	case r := <-h.started:
		t.Fatalf("second launch sent to %s into the same headroom", r)
	case <-time.After(10 * time.Second):
		t.Fatal("second launch hung")
	}
	h.free()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsOf("RunInstances")); n != 1 {
		t.Fatalf("RunInstances sent %d times, want 1", n)
	}
	// The claim follows the VM: once listed, it counts once.
	f.mu.Lock()
	f.quota["r-one-1"] = 16
	f.mu.Unlock()
	if _, err := a.Launch(context.Background(), t.Logf); err != nil {
		t.Fatalf("launch with room for a second VM: %v", err)
	}
}

// A refused launch frees its claim at once.
func TestRefusedLaunchFreesItsClaim(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}},
		quota:  map[string]int{"r-one-1": 8},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity", "r-one-1/c6a.2xlarge": "InsufficientInstanceCapacity"},
	}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	if _, err := a.Launch(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
		t.Fatalf("err = %v, want EC2's capacity refusal", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 {
		t.Fatalf("RunInstances %v, want the one priced option sent", runs)
	}
	var live int
	if err := a.withClaims(func(cs map[string]quotaClaim) { live = len(cs) }); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("%d claim(s) left after refusals", live)
	}
}

// Down frees the quota its VMs held: a replacement can launch at once,
// though their claims haven't lapsed.
func TestDownReleasesClaims(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}},
		quota:  map[string]int{"r-one-1": 8},
	}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	if _, err := a.Launch(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if err := a.Down(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Launch(context.Background(), t.Logf); err != nil {
		t.Fatalf("launch after down: %v", err)
	}
}

// Terminating one VM frees its claim too (#295): a replacement launched at
// once near the Spot quota isn't refused for the VM just ended. One whose
// termination EC2 refused keeps it.
func TestTerminateReleasesItsClaim(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}},
		quota:  map[string]int{"r-one-1": 8},
	}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.termErr = map[string]string{"r-one-1": "UnauthorizedOperation"}
	f.mu.Unlock()
	if err := a.Terminate(context.Background(), in.ID); err == nil {
		t.Fatal("terminate succeeded though EC2 refused it")
	}
	f.mu.Lock()
	f.termErr = nil
	f.hidden = map[string]bool{in.ID: true} // listings no longer show it: only its claim counts
	f.mu.Unlock()
	if _, err := a.Launch(context.Background(), t.Logf); err == nil {
		t.Fatal("launched past the quota: a refused termination released the claim")
	}
	if err := a.Terminate(context.Background(), in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Launch(context.Background(), t.Logf); err != nil {
		t.Fatalf("launch right after terminate: %v", err)
	}
}

// Down finds a VM launched moments ago that the tag-filtered listing
// doesn't show yet, by its ID (#295), and only if it is still this
// runner's.
func TestDownFindsUnlistedLaunches(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}}}
	g := newGlobalEnv(t, f, "r-one-1")
	a := g.home()
	var ids []string
	for range 3 {
		in, err := a.Launch(context.Background(), t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, in.ID)
	}
	f.mu.Lock()
	f.hidden = map[string]bool{ids[1]: true, ids[2]: true}
	f.foreign = map[string]bool{ids[2]: true}
	f.calls = nil
	f.mu.Unlock()
	if err := a.Down(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	terms := f.callsOf("TerminateInstances")
	if len(terms) != 1 || !strings.Contains(terms[0], ids[0]) || !strings.Contains(terms[0], ids[1]) || strings.Contains(terms[0], ids[2]) {
		t.Fatalf("terminated %v, want %s and the unlisted %s (not %s, another machine's)", terms, ids[0], ids[1], ids[2])
	}
	if by := f.callsOf("DescribeInstances " + ids[0]); len(by) != 0 {
		t.Fatalf("looked up %s by ID though the listing showed it", ids[0])
	}
	// Terminated: not looked up again.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if err := a.Down(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if by := f.callsOf("DescribeInstances " + ids[1]); len(by) != 0 {
		t.Fatalf("looked %s up again after terminating it: %v", ids[1], by)
	}
}

// A failed trial only clears its own claim: a later launch's trial in the
// region stays.
func TestDropTrialAtKeepsANewerTrial(t *testing.T) {
	g := newGlobalEnv(t, &fakeRegionsEC2{}, "r-one-1", "r-two-1")
	c := fixClock(g)
	older, newer := c.now(), c.now().Add(time.Hour)
	if err := g.withState(func(st *placementState) {
		st.Cooldown[unreachKey("r-two-1")] = cooldown{Trial: newer}
	}); err != nil {
		t.Fatal(err)
	}
	trialAt := func() (at time.Time) {
		_ = g.withState(func(st *placementState) { at = st.Cooldown[unreachKey("r-two-1")].Trial })
		return at
	}
	g.dropTrialAt("r-two-1", older)
	if got := trialAt(); !got.Equal(newer) {
		t.Fatalf("trial = %v, want the newer one kept", got)
	}
	g.dropTrialAt("r-two-1", newer)
	if got := trialAt(); !got.IsZero() {
		t.Fatalf("trial = %v, want it dropped", got)
	}
}
