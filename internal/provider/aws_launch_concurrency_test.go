package provider

import (
	"context"
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
