package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
)

func TestReplayable(t *testing.T) {
	retry := Options{RetryPreempt: true, Sync: "auto"}
	oom := Result{noReplay: noReplay{"MEMORY_PRESSURE", "while it was out of memory (120 MiB of 16000 MiB memory available)"}}
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

// A lost VM the cloud still runs, with no Spot notice, isn't replayed; a
// gone or noticed one is.
func TestWhyNoReplay(t *testing.T) {
	r, f := testFleet()
	for _, c := range []struct {
		id      string
		noticed bool
		code    string
	}{
		{"i-run", false, "RUNNER_UNRESPONSIVE"},
		{"i-run", true, ""},
		{"i-gone", false, ""},
		{"i-pend", false, ""},
	} {
		got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: c.id}, c.noticed)
		if got.code != c.code {
			t.Errorf("%s noticed=%v: %q (%q), want %q", c.id, c.noticed, got.code, got.why, c.code)
		}
	}
}

// A run its VM's own shutdown killed (max_lifetime, or a Spot reclaim whose
// notice never reached us) is replayed even though EC2 still reports the VM
// running while its OS halts: the helper reporting the shutdown counts as
// the VM's notice.
func TestHelperShutdownIsANotice(t *testing.T) {
	r, f := testFleet()
	shutdown := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeShutdown, Message: "the runner is shutting down"}}
	// As runOnce returns it: wrapped, from pumpExec.
	err := &InfraError{Err: fmt.Errorf("lost runner aws during execution: %w", shutdown)}
	if !helperShutdown(err) {
		t.Fatalf("a wrapped shutdown error not recognised: %v", err)
	}
	other := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "x"}}
	for _, e := range []error{nil, other, errors.New("connection reset"), io.EOF} {
		if helperShutdown(e) {
			t.Errorf("%v counted as a helper-reported shutdown", e)
		}
	}
	lease := &Lease{Runner: r, Provider: f, InstanceID: "i-run"}
	if got := whyNoReplay(lease, helperShutdown(err)); got.code != "" {
		t.Fatalf("a helper-reported shutdown on a running VM withheld the replay: %q (%q)", got.code, got.why)
	}
	res := Result{noReplay: whyNoReplay(lease, helperShutdown(err))}
	if retry, code, why := replayable(lostRunner(lease, err), res, Options{RetryPreempt: true, Sync: "auto"}, false, 1); !retry {
		t.Fatalf("not replayed: %q (%q)", code, why)
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
		code    string
	}{
		{"i-run", true, ""},
		{"i-gone", true, ""},
		{"i-run", false, "MEMORY_PRESSURE"},
		{"i-gone", false, "MEMORY_PRESSURE"},
	} {
		got := whyNoReplay(&Lease{Runner: r, Provider: f, InstanceID: c.id, Sess: s}, c.noticed)
		if got.code != c.code {
			t.Errorf("%s noticed=%v under pressure: %q (%q), want %q", c.id, c.noticed, got.code, got.why, c.code)
		}
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
}

func TestOOMMessage(t *testing.T) {
	if m := oomMessage(14950); !strings.Contains(m, "14.6 GiB") || !strings.Contains(m, "--exclusive") {
		t.Errorf("spotexec's pool: %q", m)
	}
	// Outside the pool the kill came from the container's own limit or from
	// the whole VM running out (--exclusive, one slot, no pool).
	if m := oomMessage(0); !strings.Contains(m, "container.options") || !strings.Contains(m, "whole VM") {
		t.Errorf("outside the pool: %q", m)
	}
}
