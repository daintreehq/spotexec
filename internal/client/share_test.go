package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// fakeStore stands in for the region's shared cache (or registry) and the
// runners installing into it: a build leaves its result on its runner
// until published (the upload, the push).
type fakeStore struct {
	mu        sync.Mutex
	shared    bool // in the shared cache
	pending   bool // built, not yet published
	builds    int
	reuses    int
	publishes int
	fail      []error // what the first builds return, in order
	// hold, when set, keeps the first build going until it is closed.
	hold chan struct{}
	held bool
	// together, when set, makes every build after the first wait until
	// this many are in flight at once (or fail the test): builds that
	// would otherwise run one after another deadlock instead.
	together int
	inFlight int
	allIn    chan struct{}
	serial   bool // together wasn't reached
	// cancelFirst: the first build cancels its run's context (see
	// runParts) before returning.
	cancelFirst bool
}

type cancelKey struct{}

func (f *fakeStore) probe(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shared {
		f.reuses++
	}
	return f.shared, nil
}

func (f *fakeStore) build(ctx context.Context) error {
	f.mu.Lock()
	if f.shared {
		f.reuses++
		f.mu.Unlock()
		return nil
	}
	f.builds++
	first := !f.held
	f.held = true
	var err error
	if len(f.fail) > 0 {
		err, f.fail = f.fail[0], f.fail[1:]
	}
	hold := f.hold
	var allIn chan struct{}
	if !first && f.together > 0 {
		if f.allIn == nil {
			f.allIn = make(chan struct{})
		}
		allIn = f.allIn
		f.inFlight++
		if f.inFlight == f.together {
			close(f.allIn)
		}
	}
	f.mu.Unlock()
	if first && hold != nil {
		<-hold
	}
	if first && f.cancelFirst {
		ctx.Value(cancelKey{}).(context.CancelFunc)()
	}
	if allIn != nil {
		select {
		case <-allIn:
		case <-time.After(5 * time.Second):
			f.mu.Lock()
			f.serial = true
			f.mu.Unlock()
		}
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.pending = true
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) publish(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes++
	if f.pending {
		f.shared = true
	}
	return nil
}

func (f *fakeStore) counts() (builds, reuses, publishes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.builds, f.reuses, f.publishes
}

func fastShare(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	old := sharePoll
	sharePoll = 2 * time.Millisecond
	t.Cleanup(func() { sharePoll = old })
	// A broken wait fails the test instead of hanging the package.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type shareRun struct {
	out     shareOutcome
	err     error
	waiting []string
}

// runParts runs n parts needing the same artifact at once. With f.hold
// set, the first build is held until every other part waits for it.
func runParts(ctx context.Context, t *testing.T, n int, f *fakeStore) []shareRun {
	t.Helper()
	lock := shareLockFor("deps", "https://bucket.s3.eu-north-1.amazonaws.com/repo/key.tar")
	runs := make([]shareRun, n)
	waits := make(chan struct{}, n)
	if f.hold != nil {
		go func() {
			for range n - 1 {
				select {
				case <-waits:
				case <-ctx.Done():
				}
			}
			close(f.hold)
		}()
	}
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sh := sharer{
				lock: lock, who: "r#" + strconv.Itoa(i), limit: time.Minute,
				probe: f.probe, build: f.build, publish: f.publish,
				waiting: func(holder string) {
					runs[i].waiting = append(runs[i].waiting, holder)
					waits <- struct{}{}
				},
			}
			pctx, cancel := context.WithCancel(ctx)
			defer cancel()
			runs[i].out, runs[i].err = sh.run(context.WithValue(pctx, cancelKey{}, cancel))
		}(i)
	}
	wg.Wait()
	return runs
}

func leader(t *testing.T, runs []shareRun) int {
	t.Helper()
	at := -1
	for i, r := range runs {
		if r.out.led {
			if at >= 0 {
				t.Fatalf("parts %d and %d both led", at, i)
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no part led")
	}
	return at
}

// #139: a cold cache is installed once, by one part; the others wait for
// it, say so (naming it), and restore it once it is published.
func TestShareOnePartBuildsTheOthersWaitAndReuse(t *testing.T) {
	ctx := fastShare(t)
	f := &fakeStore{hold: make(chan struct{})}
	runs := runParts(ctx, t, 6, f)
	builds, reuses, publishes := f.counts()
	if builds != 1 || reuses != 5 || publishes != 1 {
		t.Fatalf("builds %d, reuses %d, publishes %d; want 1, 5, 1", builds, reuses, publishes)
	}
	l := leader(t, runs)
	want := fmt.Sprintf("another part of this run (r#%d)", l)
	for i, r := range runs {
		if r.err != nil {
			t.Fatalf("part %d: %v", i, r.err)
		}
		if i == l {
			continue
		}
		if len(r.waiting) != 1 || r.waiting[0] != want || r.out.waited <= 0 {
			t.Errorf("part %d: waiting %q (want %q), waited %s", i, r.waiting, want, r.out.waited)
		}
	}
}

// The leader's runner was lost, or the leader was cancelled: the next
// waiting part takes over the install instead of every part installing.
func TestShareLostLeaderHandsOver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fail   error
		cancel bool
	}{
		{"runner lost", errors.New("runner stopped responding"), false},
		// #162: the runner reporting that it is going away (a Spot reclaim
		// shutting the VM down, a drain, a helper fault) is losing it, not
		// the build failing: one waiting part takes over, not all at once.
		{"vm shutting down", fmt.Errorf("deps: %w", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeShutdown, Message: "the VM began shutting down"}}), false},
		{"draining", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeDraining}}, false},
		{"helper fault", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeInternal}}, false},
		// A failure the runner reports as the run is cancelled (fail_fast)
		// is the cancel's, not the install's.
		{"cancelled", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeDepsFailed, Message: "install command exited 143"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := fastShare(t)
			f := &fakeStore{hold: make(chan struct{}), fail: []error{tc.fail}, cancelFirst: tc.cancel}
			runs := runParts(ctx, t, 3, f)
			builds, reuses, publishes := f.counts()
			if builds != 2 || reuses != 1 || publishes != 1 {
				t.Fatalf("builds %d, reuses %d, publishes %d; want 2, 1, 1", builds, reuses, publishes)
			}
			failed, led := 0, 0
			for _, r := range runs {
				if r.err != nil {
					failed++
				}
				if r.out.led {
					led++
				}
			}
			if failed != 1 || led != 2 {
				t.Fatalf("failed %d, led %d; want the lost leader and its successor", failed, led)
			}
		})
	}
}

// An install the runner reported as failed would most likely fail again:
// the waiting parts go on at once, each on its own, instead of retrying it
// one after another. prepareImage wraps the runner's error.
func TestShareFailedInstallReleasesTheWaiters(t *testing.T) {
	ctx := fastShare(t)
	bad := fmt.Errorf("container image: %w", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "docker build: exit 1"}})
	f := &fakeStore{hold: make(chan struct{}), fail: []error{bad, bad, bad, bad}, together: 3}
	runs := runParts(ctx, t, 4, f)
	l := leader(t, runs)
	for i, r := range runs {
		if r.err == nil {
			t.Errorf("part %d succeeded: %+v", i, r.out)
		}
		if i != l && len(r.waiting) != 1 {
			t.Errorf("part %d didn't wait: %+v", i, r)
		}
	}
	if builds, _, _ := f.counts(); builds != 4 || f.serial {
		t.Fatalf("builds %d, one after another %v; want 4 at once", builds, f.serial)
	}
}

// A run alone builds without waiting, then publishes before it goes on.
func TestShareAloneBuildsAndPublishes(t *testing.T) {
	ctx := fastShare(t)
	f := &fakeStore{}
	runs := runParts(ctx, t, 1, f)
	if runs[0].err != nil || !runs[0].out.led || len(runs[0].waiting) != 0 || runs[0].out.waited != 0 {
		t.Fatalf("run = %+v", runs[0])
	}
	if builds, _, publishes := f.counts(); builds != 1 || publishes != 1 {
		t.Fatalf("builds %d, publishes %d", builds, publishes)
	}
}

// Losing the runner while waiting for the upload fails the leader and
// leaves no verdict, so the next waiting run installs.
func TestSharePublishLostLeavesNoVerdict(t *testing.T) {
	ctx := fastShare(t)
	lock := shareLockFor("deps", "k")
	f := &fakeStore{}
	sh := sharer{lock: lock, who: "me", limit: time.Minute, probe: f.probe, build: f.build,
		publish: func(context.Context) error { return errors.New("runner stopped responding") }}
	if _, err := sh.run(ctx); err == nil {
		t.Fatal("want the lost runner's error")
	}
	if lock.verdict() != "" {
		t.Fatal("verdict left")
	}
}

// A hit needs no lock at all: it doesn't wait for a holder.
func TestShareHitDoesntWait(t *testing.T) {
	ctx := fastShare(t)
	lock := shareLockFor("deps", "k")
	held, err := lock.take(ctx, lock.verdict(), "other", time.Minute, nil)
	if err != nil || !held.lead {
		t.Fatalf("take: %+v, %v", held, err)
	}
	defer held.release("")
	f := &fakeStore{shared: true}
	sh := sharer{lock: lock, who: "me", limit: time.Minute, probe: f.probe, build: f.build, publish: f.publish}
	out, err := sh.run(ctx)
	if err != nil || out.led || out.waited != 0 {
		t.Fatalf("out %+v, %v", out, err)
	}
	if builds, reuses, _ := f.counts(); builds != 0 || reuses != 1 {
		t.Fatalf("builds %d, reuses %d", builds, reuses)
	}
}

// A probe errors: nothing is built or published.
func TestShareProbeErrorStops(t *testing.T) {
	ctx := fastShare(t)
	f := &fakeStore{}
	sh := sharer{lock: shareLockFor("deps", "k"), who: "me", limit: time.Minute,
		probe: func(context.Context) (bool, error) { return false, errors.New("lost") }, build: f.build, publish: f.publish}
	if _, err := sh.run(ctx); err == nil {
		t.Fatal("want the probe's error")
	}
	if builds, _, publishes := f.counts(); builds != 0 || publishes != 0 {
		t.Fatalf("builds %d, publishes %d", builds, publishes)
	}
}

// Each cold wave leaves a fresh verdict: a part that found the old one,
// probed and missed, leads; one whose probe raced a leader finishing
// doesn't (it fetches what that leader published).
func TestShareVerdictsAreFreshPerWave(t *testing.T) {
	ctx := fastShare(t)
	lock := shareLockFor("deps", "k")
	wave := func(probe func(context.Context) (bool, error)) (shareOutcome, *fakeStore) {
		f := &fakeStore{}
		if probe == nil {
			probe = f.probe
		}
		sh := sharer{lock: lock, who: "me", limit: time.Minute, probe: probe, build: f.build, publish: f.publish}
		out, err := sh.run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out, f
	}
	first := lock.verdict()
	if out, _ := wave(nil); !out.led {
		t.Fatalf("first wave didn't lead: %+v", out)
	}
	second := lock.verdict()
	if second == first {
		t.Fatal("no verdict after the first wave")
	}
	if out, _ := wave(nil); !out.led {
		t.Fatalf("second wave (old verdict) didn't lead: %+v", out)
	}
	if lock.verdict() == second {
		t.Fatal("the second wave's verdict is the first's")
	}
	// Another run finishes while this one's probe is out.
	out, f := wave(func(ctx context.Context) (bool, error) {
		other := &fakeStore{}
		sh := sharer{lock: lock, who: "other", limit: time.Minute, probe: other.probe, build: other.build, publish: other.publish}
		if _, err := sh.run(ctx); err != nil {
			return false, err
		}
		return false, nil // this probe's miss predates that publish
	})
	if out.led {
		t.Fatalf("led after another run published meanwhile: %+v", out)
	}
	if builds, _, _ := f.counts(); builds != 1 {
		t.Fatalf("builds %d: the run should go on with a normal request", builds)
	}
}

// A leader that never finishes (stopped, wedged) holds a waiting run up
// for at most the limit; then it installs for itself.
func TestShareWaiterGivesUpAfterLimit(t *testing.T) {
	ctx := fastShare(t)
	lock := shareLockFor("deps", "k")
	held, err := lock.take(ctx, lock.verdict(), "stuck", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release("")
	f := &fakeStore{}
	var holders []string
	sh := sharer{lock: lock, who: "me", limit: 50 * time.Millisecond, probe: f.probe, build: f.build, publish: f.publish,
		waiting: func(h string) { holders = append(holders, h) }}
	out, err := sh.run(ctx)
	if err != nil || !out.timedOut || out.led || out.waited < 50*time.Millisecond {
		t.Fatalf("out %+v, %v", out, err)
	}
	if want := "another part of this run (stuck)"; len(holders) != 1 || holders[0] != want {
		t.Fatalf("holders %q, want %q", holders, want)
	}
	if builds, _, publishes := f.counts(); builds != 1 || publishes != 0 {
		t.Fatalf("builds %d, publishes %d", builds, publishes)
	}
}

// Cancelling a waiting part (fail_fast, Ctrl-C) stops its wait.
func TestShareWaitHonoursCancel(t *testing.T) {
	ctx := fastShare(t)
	lock := shareLockFor("deps", "k")
	held, err := lock.take(ctx, lock.verdict(), "leader", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release("")
	wctx, cancel := context.WithCancel(ctx)
	f := &fakeStore{}
	sh := sharer{lock: lock, who: "me", limit: time.Minute, probe: f.probe, build: f.build, publish: f.publish,
		waiting: func(string) { cancel() }}
	if _, err := sh.run(wctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if builds, _, _ := f.counts(); builds != 0 {
		t.Fatalf("builds %d", builds)
	}
}

// The lock is per shared object: a new signature for the same cache entry
// is the same lock; another key, region, repository or tag is not.
func TestShareLockIdentity(t *testing.T) {
	fastShare(t)
	a := shareLockFor("deps", stripQuery("https://b.s3.eu-north-1.amazonaws.com/repo/k1.tar?X-Amz-Signature=1"))
	if b := shareLockFor("deps", stripQuery("https://b.s3.eu-north-1.amazonaws.com/repo/k1.tar?X-Amz-Signature=2")); a.path != b.path {
		t.Fatal("a new signature changed the lock")
	}
	seen := map[string]string{a.path: "a"}
	for _, l := range []struct{ kind, id string }{
		{"deps", "https://b.s3.eu-north-1.amazonaws.com/repo/k2.tar"},
		{"deps", "https://b.s3.us-east-2.amazonaws.com/repo/k1.tar"},
		{"image", "https://b.s3.eu-north-1.amazonaws.com/repo/k1.tar"},
		{"image", "1.dkr.ecr.eu-north-1.amazonaws.com/spotexec/repo:t1"},
		{"image", "1.dkr.ecr.eu-north-1.amazonaws.com/spotexec/repo:t2"},
	} {
		p := shareLockFor(l.kind, l.id).path
		if prev, ok := seen[p]; ok {
			t.Fatalf("%s %s shares a lock with %s", l.kind, l.id, prev)
		}
		seen[p] = l.id
	}
}

// TestShareHolderProcess is the other sx process of the test below: it
// takes the lock and waits to be killed.
func TestShareHolderProcess(t *testing.T) {
	if os.Getenv("SX_SHARE_HOLDER") == "" {
		t.Skip("run by TestShareDeadHolderHandsOver")
	}
	held, err := shareLockFor("deps", "k").take(context.Background(), "", "other#1", time.Minute, nil)
	if err != nil || !held.lead {
		os.Exit(3)
	}
	os.Stdout.WriteString("held\n")
	time.Sleep(time.Minute)
}

// Another sx process leading the install is waited for, by name; when it
// dies (its flock goes with it) without a verdict, this run installs.
func TestShareDeadHolderHandsOver(t *testing.T) {
	ctx := fastShare(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestShareHolderProcess$")
	cmd.Env = append(os.Environ(), "SX_SHARE_HOLDER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if line != "held\n" {
		t.Fatalf("holder said %q", line)
	}
	f := &fakeStore{}
	var holders []string
	sh := sharer{lock: shareLockFor("deps", "k"), who: "me#2", limit: time.Minute, probe: f.probe, build: f.build, publish: f.publish,
		waiting: func(h string) {
			holders = append(holders, h)
			_ = cmd.Process.Kill()
		}}
	out, err := sh.run(ctx)
	if err != nil || !out.led || out.waited <= 0 {
		t.Fatalf("out %+v, %v", out, err)
	}
	want := fmt.Sprintf("another run on this machine (other#1, sx pid %d)", cmd.Process.Pid)
	if len(holders) != 1 || holders[0] != want {
		t.Fatalf("holders %q, want %q", holders, want)
	}
	if builds, _, _ := f.counts(); builds != 1 {
		t.Fatalf("builds %d", builds)
	}
}
