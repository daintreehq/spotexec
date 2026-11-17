package provider

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// overlap counts RunInstances requests in flight at once, holding each
// for hold.
type overlap struct {
	hold      time.Duration
	now, peak atomic.Int32
}

func (o *overlap) onRun(string) {
	n := o.now.Add(1)
	for {
		p := o.peak.Load()
		if n <= p || o.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(o.hold)
	o.now.Add(-1)
}

// Independent launches of one runner reach EC2 together, up to the cap.
func TestGlobalLaunchesOverlap(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}, {"r-one-1b", "c6a.2xlarge", "0.06"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.07"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g.Prices(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	o := &overlap{hold: 100 * time.Millisecond}
	f.onRun = o.onRun
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Launch(context.Background(), t.Logf); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if p := o.peak.Load(); p < 2 || p > launchSlots {
		t.Fatalf("peak concurrent RunInstances = %d, want 2..%d", p, launchSlots)
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
	o := &overlap{hold: 100 * time.Millisecond}
	f.onRun = o.onRun
	const n = 4
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Launch(context.Background(), t.Logf); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := o.peak.Load(); p < 2 {
		t.Fatalf("peak concurrent RunInstances = %d, want overlap", p)
	}
}

// held makes RunInstances wait until released, telling started about
// each request as it arrives.
type held struct {
	started chan string
	release chan struct{}
}

func newHeld() *held { return &held{started: make(chan string, 16), release: make(chan struct{})} }

func (h *held) onRun(region string) {
	h.started <- region
	<-h.release
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
	launched := make(chan error, 3)
	launch := func() {
		_, err := g.Launch(context.Background(), t.Logf)
		launched <- err
	}
	go launch()
	go launch()
	<-h.started
	<-h.started // both are sending
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
	close(h.release)
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
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.lockLaunch(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
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
	first := make(chan error, 1)
	go func() {
		_, err := a.Launch(context.Background(), t.Logf)
		first <- err
	}()
	<-h.started
	_, err := a.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("second launch: %v, want the quota refusal", err)
	}
	close(h.release)
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
	if _, err := a.Launch(context.Background(), t.Logf); err == nil {
		t.Fatal("launch succeeded without capacity")
	}
	var live int
	if err := a.withClaims(func(cs map[string]quotaClaim) { live = len(cs) }); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("%d claim(s) left after refusals", live)
	}
}
