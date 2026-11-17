package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
)

func TestReplayable(t *testing.T) {
	retry := Options{RetryPreempt: true, Sync: "auto"}
	oom := Result{noReplay: noReplay{code: "MEMORY_PRESSURE", why: "while it was out of memory (120 MiB of 16000 MiB memory available)"}}
	for _, c := range []struct {
		name     string
		lost     string
		res      Result
		o        Options
		canceled bool
		n        int
		want     bool
		code     string
	}{
		{"lost VM is replayed", "i-1", Result{}, retry, false, 1, true, ""},
		{"nothing lost", "", Result{}, retry, false, 1, false, ""},
		{"--no-retry", "i-1", Result{}, Options{Sync: "auto"}, false, 1, false, ""},
		{"interrupted", "i-1", Result{}, retry, true, 1, false, ""},
		{"second attempt", "i-1", Result{}, retry, false, 2, false, ""},
		{"--sync none", "i-1", Result{}, Options{RetryPreempt: true, Sync: "none"}, false, 1, false, ""},
		{"--same-slot", "i-1", Result{}, Options{RetryPreempt: true, Sync: "auto", SameSlot: true}, false, 1, false, ""},
		{"lost out of memory", "i-1", oom, retry, false, 1, false, "MEMORY_PRESSURE"},
		{"out of memory, --no-retry: still explained", "i-1", oom, Options{Sync: "auto"}, false, 1, false, "MEMORY_PRESSURE"},
	} {
		got, code, why := replayable(c.lost, c.res, c.o, c.canceled, c.n)
		if got != c.want || code != c.code {
			t.Errorf("%s: replay %v, code %q (%q); want %v, %q", c.name, got, code, why, c.want, c.code)
		}
		if c.code != "" && !strings.Contains(why, "120 MiB") {
			t.Errorf("%s: %q doesn't say what the runner reported", c.name, why)
		}
		if c.code != "" && strings.Contains(why, "not replaying") != c.o.RetryPreempt {
			t.Errorf("%s: %q: says it withheld a replay that --no-retry ruled out anyway, or the reverse", c.name, why)
		}
	}
}

func testFleet() (*config.Runner, *fakeFleet) {
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot}
	return r, &fakeFleet{r: r, insts: []provider.Instance{
		{ID: "i-run", State: provider.StateRunning},
		{ID: "i-pend", State: provider.StatePending},
	}}
}

var (
	running   = stage{phase: phaseCommand, dispatched: true, started: true}
	starting  = stage{phase: phaseStartCommand, dispatched: true}
	depsCheck = stage{phase: phaseDepsCheck}
)

// A lost VM the cloud still runs, with no Spot notice, isn't replayed once
// the command was dispatched; a gone or noticed one is, and so is one lost
// before the command was dispatched.
func TestWhyNoReplay(t *testing.T) {
	r, f := testFleet()
	for _, c := range []struct {
		id      string
		noticed bool
		at      stage
		code    string
	}{
		{"i-run", false, running, "RUNNER_UNRESPONSIVE"},
		{"i-run", false, stage{phase: phaseSetup, dispatched: true, started: true}, "RUNNER_UNRESPONSIVE"},
		{"i-run", false, starting, "RUNNER_UNRESPONSIVE"},
		{"i-run", true, running, ""},
		{"i-gone", false, running, ""},
		{"i-pend", false, running, ""},
		{"i-run", false, depsCheck, ""},
		{"i-run", false, stage{phase: phaseSync}, ""},
		{"i-run", false, stage{phase: phaseDeps}, ""},
	} {
		got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: c.id}, c.noticed, c.at, false)
		if got.code != c.code {
			t.Errorf("%s noticed=%v at %+v: %q (%q), want %q", c.id, c.noticed, c.at, got.code, got.why, c.code)
		}
	}
}

// #143: a VM lost while the run was still checking the dependency cache,
// though EC2 still runs it, is replayed, and the warning says where the run
// had got to rather than blaming the command.
func TestLostBeforeTheCommandIsReplayed(t *testing.T) {
	r, f := testFleet()
	lease := &Lease{Runner: r, Provider: f, InstanceID: "i-run"}
	res := Result{noReplay: replayVerdict(context.Background(), lease, false, io.EOF, depsCheck, true), lostAt: depsCheck}
	retry, code, why := replayable("i-run", res, Options{RetryPreempt: true, Sync: "auto", Exclusive: true}, false, 1)
	if !retry || code != "" || why != "" {
		t.Fatalf("not replayed: %v %q %q", retry, code, why)
	}
	if w := res.lostAt.where(); w != "while checking the dependency cache, before the command started" {
		t.Fatalf("where: %q", w)
	}
}

// Once the command was dispatched, the warning names the phase, the last
// memory reading when there is one, and suggests --exclusive only to a run
// that didn't already have it.
func TestUnresponsiveMessage(t *testing.T) {
	r, f := testFleet()
	lease := &Lease{Runner: r, Provider: f, InstanceID: "i-run"}
	why := whyNoReplay(lease, false, running, false).why
	if !strings.HasPrefix(why, "while running the command, although EC2") || !strings.Contains(why, "the command may be what overloaded it") || !strings.Contains(why, "--exclusive") || strings.Contains(why, "heartbeat") {
		t.Errorf("not exclusive: %q", why)
	}
	if why := whyNoReplay(lease, false, running, true).why; strings.Contains(why, "--exclusive") {
		t.Errorf("already exclusive: %q", why)
	}
	if why := whyNoReplay(lease, false, stage{phase: phaseSetup, dispatched: true, started: true}, false).why; !strings.Contains(why, "[setup] run may be what overloaded it") {
		t.Errorf("setup: %q", why)
	}
	// Handed over but never confirmed started: no blame, no sizing advice.
	if why := whyNoReplay(lease, false, starting, false).why; !strings.HasPrefix(why, "while starting the command, although EC2") || !strings.Contains(why, "may have started") || strings.Contains(why, "overloaded") || strings.Contains(why, "--exclusive") {
		t.Errorf("not started: %q", why)
	}
}

// The progress an attempt records as the helper reports it: dispatch, the
// setup's start, setup_done, the command's start.
func TestProgress(t *testing.T) {
	p := &progress{at: stage{phase: phaseSlot}}
	p.set(phaseDepsCheck)
	if got := p.get(); got != depsCheck {
		t.Fatalf("deps: %+v", got)
	}
	p.dispatch(true)
	if got := p.get(); got != (stage{phase: phaseStartSetup, dispatched: true}) {
		t.Fatalf("dispatched with setup: %+v", got)
	}
	p.started()
	if got := p.get(); got != (stage{phase: phaseSetup, dispatched: true, started: true}) {
		t.Fatalf("setup started: %+v", got)
	}
	p.setupDone()
	if got := p.get(); got != starting {
		t.Fatalf("setup done: %+v", got)
	}
	p.started()
	if got := p.get(); got != running {
		t.Fatalf("command started: %+v", got)
	}
	p.started() // a stray start changes nothing
	if got := p.get(); got != running {
		t.Fatalf("after a stray start: %+v", got)
	}
	p.dispatch(false)
	if got := p.get(); got != starting {
		t.Fatalf("dispatched without setup: %+v", got)
	}
}

// A runner lost after this machine slept past the runner's patience
// (#142), once the command was handed over, isn't replayed, and the
// warning says the sleep was why, not the VM or the run, whatever else the
// VM reported before it. Lost before that, the run is replayed as usual.
func TestClientAsleepVerdict(t *testing.T) {
	r, f := testFleet()
	old := sessionSuspended
	t.Cleanup(func() { sessionSuspended = old })
	sessionSuspended = func(*transport.Session) time.Duration { return 15*time.Minute + 41*time.Second }
	for _, c := range []struct {
		id      string
		noticed bool
		at      stage
		code    string
	}{
		{"i-run", false, running, "CLIENT_SUSPENDED"},
		{"i-gone", false, starting, "CLIENT_SUSPENDED"},
		{"i-run", true, running, ""},    // a Spot notice: the VM was going anyway
		{"i-run", false, depsCheck, ""}, // the command can't have run: replayed
	} {
		got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: c.id}, c.noticed, c.at, false)
		if got.code != c.code {
			t.Errorf("%s noticed=%v at %+v: %q (%q), want %q", c.id, c.noticed, c.at, got.code, got.why, c.code)
		}
		if c.code != "" && (!strings.HasPrefix(got.why, c.at.where()+", after this machine was asleep") || !strings.Contains(got.why, "15m41s") || strings.Contains(got.why, "overloaded")) {
			t.Errorf("%s: %q doesn't put the loss down to the sleep", c.id, got.why)
		}
		if retry, _, _ := replayable(c.id, Result{noReplay: got}, Options{RetryPreempt: true, Sync: "auto"}, false, 1); retry != (c.code == "") {
			t.Errorf("%s noticed=%v at %+v after a sleep: replayed %v", c.id, c.noticed, c.at, retry)
		}
	}
	// The sleep outranks memory pressure the VM reported before it.
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
	pressured := &Lease{Runner: r, Provider: f, InstanceID: "i-run", Sess: s}
	if got := whyNoReplay(pressured, false, running, false); got.code != "CLIENT_SUSPENDED" {
		t.Fatalf("a sleep under memory pressure: %q (%q), want CLIENT_SUSPENDED", got.code, got.why)
	}
	if got := whyNoReplay(pressured, false, depsCheck, false); got.code != "" {
		t.Fatalf("a sleep under memory pressure before the command: %q (%q), want a replay", got.code, got.why)
	}
	sessionSuspended = func(*transport.Session) time.Duration { return 0 }
	if got := whyNoReplay(pressured, false, depsCheck, false); got.code != "MEMORY_PRESSURE" {
		t.Fatalf("pressure without a sleep before the command: %q, want MEMORY_PRESSURE", got.code)
	}
	if got := whyNoReplay(pressured, false, running, false); got.code != "MEMORY_PRESSURE" {
		t.Fatalf("pressure without a sleep: %q, want MEMORY_PRESSURE", got.code)
	}
	if got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: "i-run"}, false, running, false); got.code != "RUNNER_UNRESPONSIVE" {
		t.Fatalf("no sleep: %q, want RUNNER_UNRESPONSIVE", got.code)
	}
	// The real one: a session that never slept, or none at all.
	if old(s) != 0 || old(nil) != 0 {
		t.Fatal("a suspension where there was none")
	}
}

// A run its VM's own shutdown killed (max_lifetime, or a Spot reclaim whose
// notice never reached us) is replayed even though EC2 still reports the VM
// running while its OS halts: the helper reporting the shutdown counts as
// the VM's notice.
func TestHelperShutdownIsANotice(t *testing.T) {
	r, f := testFleet()
	shutdown := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeShutdown, Message: "the runner is shutting down"}}
	lease := &Lease{Runner: r, Provider: f, InstanceID: "i-run"}
	// Both shapes, as runOnce returns them: the helper's error from
	// pumpExec, and a result the shutdown killed (execShutdown).
	for name, c := range map[string]struct {
		err  error
		lost string
	}{
		"helper error":  {&InfraError{Err: fmt.Errorf("lost runner aws during execution: %w", shutdown)}, lostRunner(lease, shutdown)},
		"killed result": {shutdownErr("aws#1", 143), shutdownRunner(lease)},
	} {
		res := Result{noReplay: replayVerdict(context.Background(), lease, false, c.err, running, false)}
		if res.noReplay.code != "" {
			t.Fatalf("%s: a shutdown on a running VM withheld the replay: %q (%q)", name, res.noReplay.code, res.noReplay.why)
		}
		if retry, code, why := replayable(c.lost, res, Options{RetryPreempt: true, Sync: "auto"}, false, 1); !retry {
			t.Fatalf("%s: not replayed: %q (%q)", name, code, why)
		}
	}
	// Anything else lost on a running VM still isn't replayed.
	other := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "x"}}
	for _, e := range []error{nil, other, errors.New("connection reset"), io.EOF} {
		if vmShutdown(e) {
			t.Errorf("%v counted as a shutdown", e)
		}
	}
	if got := replayVerdict(context.Background(), lease, false, io.EOF, running, false); got.code != "RUNNER_UNRESPONSIVE" {
		t.Fatalf("a dropped connection on a running VM: %q, want RUNNER_UNRESPONSIVE", got.code)
	}
}

// A VM that announced its reclamation is replayed even if it also reported
// memory pressure in the window: the cloud took it, not the run.
func TestNoticeOutranksMemoryPressure(t *testing.T) {
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
	if s.MemoryPressure(PressureWindow) == "" {
		t.Fatal("no pressure heartbeat arrived")
	}
	r, f := testFleet()
	for _, c := range []struct {
		id      string
		noticed bool
		at      stage
		code    string
	}{
		{"i-run", true, running, ""},
		{"i-gone", true, running, ""},
		{"i-run", false, running, "MEMORY_PRESSURE"},
		{"i-gone", false, running, "MEMORY_PRESSURE"},
		{"i-run", true, depsCheck, ""},
		{"i-run", false, depsCheck, "MEMORY_PRESSURE"},
	} {
		got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: c.id, Sess: s}, c.noticed, c.at, false)
		if got.code != c.code {
			t.Errorf("%s noticed=%v at %+v under pressure: %q (%q), want %q", c.id, c.noticed, c.at, got.code, got.why, c.code)
		}
		if c.code != "" && !strings.HasPrefix(got.why, c.at.where()+": it was out of memory") {
			t.Errorf("%s at %+v: %q doesn't say where the run had got to", c.id, c.at, got.why)
		}
		if retry, _, _ := replayable(c.id, Result{noReplay: got}, Options{RetryPreempt: true, Sync: "auto"}, false, 1); retry != c.noticed {
			t.Errorf("%s noticed=%v under pressure: replayed %v", c.id, c.noticed, retry)
		}
	}
}

// The unresponsive warning carries the VM's last memory reading, healthy
// or not.
func TestUnresponsiveCarriesLastMemory(t *testing.T) {
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
	r, f := testFleet()
	got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: "i-run", Sess: s}, false, running, false)
	if got.code != "RUNNER_UNRESPONSIVE" || !strings.Contains(got.why, "reported 11.7 GB of 15.6 GB memory available") {
		t.Fatalf("%q (%q)", got.code, got.why)
	}
	if lastMemory(nil) != "" {
		t.Fatal("a nil session has a reading")
	}
}

func TestWaitIfRunning(t *testing.T) {
	r, f := testFleet()
	for id, want := range map[string]bool{"i-run": true, "i-pend": false, "i-gone": false} {
		hook := waitIfRunning(&Lease{Runner: r, Provider: f, InstanceID: id}, quietRep(t))
		if got := hook(SilentGrace); (got == SilentGrace) != want || (!want && got != 0) {
			t.Errorf("%s: grace %s, want running=%v", id, got, want)
		}
	}
	// A static runner has no cloud to ask.
	hook := waitIfRunning(&Lease{Runner: r, Provider: nil, InstanceID: "i-run"}, quietRep(t))
	if hook(SilentGrace) != 0 {
		t.Error("grace without a fleet")
	}
	// Nor is there grace when the cloud can't say the VM still runs.
	f.listErr = errors.New("throttled")
	if got := waitIfRunning(&Lease{Runner: r, Provider: f, InstanceID: "i-run"}, quietRep(t))(SilentGrace); got != 0 {
		t.Errorf("grace %s after a failed lookup", got)
	}
}

func TestOOMMessage(t *testing.T) {
	if m := oomMessage(14950); !strings.Contains(m, "14.6 GiB") || !strings.Contains(m, "--exclusive") {
		t.Errorf("spotexec's pool: %q", m)
	}
	// Outside the pool the kill came from the container's own limit or from
	// the whole VM running out (--exclusive, one slot, no pool).
	if m := oomMessage(0); !strings.Contains(m, "container.options") || !strings.Contains(m, "--cgroup-parent") || !strings.Contains(m, "whole VM") || !strings.Contains(m, "Raise that limit") {
		t.Errorf("outside the pool: %q", m)
	}
}

// #266: a VM whose state EC2 couldn't report (the listing failed, or ran
// out of time) isn't taken for a gone one once the command was dispatched:
// the run isn't replayed, and the warning says EC2 couldn't tell, without
// claiming it still runs the VM. Lost before dispatch, or with a Spot
// notice, it is still replayed.
func TestFailedLookupWithholdsReplay(t *testing.T) {
	stubRegionSilent(t, nil)
	old := instanceLookupTime
	t.Cleanup(func() { instanceLookupTime = old })
	instanceLookupTime = 50 * time.Millisecond
	retry := Options{RetryPreempt: true, Sync: "auto"}
	for _, c := range []struct {
		name string
		set  func(*fakeFleet)
	}{
		{"list error", func(f *fakeFleet) { f.listErr = errors.New("RequestLimitExceeded") }},
		{"list deadline", func(f *fakeFleet) { f.slowList = true }},
	} {
		r, f := testFleet()
		c.set(f)
		lease := &Lease{Runner: r, Provider: f, InstanceID: "i-run"}
		for _, at := range []stage{running, starting} {
			got := whyNoReplay(lease, false, at, false)
			if got.code != "RUNNER_UNRESPONSIVE" || got.region != nil {
				t.Errorf("%s at %+v: %q (%q), want RUNNER_UNRESPONSIVE", c.name, at, got.code, got.why)
			}
			if !strings.HasPrefix(got.why, at.where()+", and EC2 couldn't confirm whether it is still running") || strings.Contains(got.why, "still reports it running") || strings.Contains(got.why, "overloaded") || strings.Contains(got.why, "--exclusive") || !strings.Contains(got.why, "outcome is unknown") || !strings.Contains(got.why, "side effects") {
				t.Errorf("%s at %+v: %q", c.name, at, got.why)
			}
			ok, code, why := replayable("i-run", Result{noReplay: got, lostAt: at}, retry, false, 1)
			if ok || code != "RUNNER_UNRESPONSIVE" || !strings.Contains(why, "not replaying") {
				t.Errorf("%s at %+v replayed: %v %q %q", c.name, at, ok, code, why)
			}
		}
		// A Spot notice, or a loss before dispatch, is replayed without
		// asking EC2 at all.
		f.lists = 0
		for _, cc := range []struct {
			noticed bool
			at      stage
		}{{true, running}, {false, depsCheck}, {false, stage{phase: phaseSync}}} {
			got := whyNoReplay(lease, cc.noticed, cc.at, false)
			if ok, code, why := replayable("i-run", Result{noReplay: got, lostAt: cc.at}, retry, false, 1); !ok || code != "" || why != "" || got.region != nil {
				t.Errorf("%s noticed=%v at %+v: %v %q (%q), want a replay", c.name, cc.noticed, cc.at, ok, code, why)
			}
		}
		if f.lists != 0 {
			t.Errorf("%s: listed the fleet %d times for verdicts that don't need it", c.name, f.lists)
		}
	}
}

// #266: what a lookup says about the lost VM: running, gone (listed in
// another state, or not listed at all), or unknown when the listing failed
// or ran out of time.
func TestInstanceState(t *testing.T) {
	old := instanceLookupTime
	t.Cleanup(func() { instanceLookupTime = old })
	instanceLookupTime = 50 * time.Millisecond
	r, f := testFleet()
	for id, want := range map[string]vmState{"i-run": vmRunning, "i-pend": vmGone, "i-gone": vmGone} {
		if got := instanceState(&Lease{Runner: r, Provider: f, InstanceID: id}); got != want {
			t.Errorf("%s: %v, want %v", id, got, want)
		}
	}
	if got := instanceState(&Lease{Runner: r, Provider: f}); got != vmGone {
		t.Errorf("no VM: %v", got)
	}
	if got := instanceState(&Lease{Runner: r, InstanceID: "i-run"}); got != vmUnknown {
		t.Errorf("no fleet to ask: %v", got)
	}
	f.listErr = errors.New("throttled")
	if got := instanceState(&Lease{Runner: r, Provider: f, InstanceID: "i-run"}); got != vmUnknown {
		t.Errorf("list error: %v", got)
	}
	// A listing that never answers is given up on at the deadline (no
	// sooner: the fake returns only once its context ends).
	f.listErr, f.slowList = nil, true
	done := make(chan vmState, 1)
	start := time.Now()
	go func() { done <- instanceState(&Lease{Runner: r, Provider: f, InstanceID: "i-run"}) }()
	select {
	case got := <-done:
		if el := time.Since(start); got != vmUnknown || el < instanceLookupTime {
			t.Errorf("list deadline: %v after %s", got, el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the listing had no deadline")
	}
}
