package transport

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// A silent helper whose VM the caller vouches for gets the grace it asks
// for, once, and is given up on after it; the error names the full wait.
func TestSilentHelperGrace(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(silent time.Duration) time.Duration {
		asked.Add(1)
		if silent < time.Second {
			t.Errorf("asked after %s of silence, before the limit", silent)
		}
		return 2 * time.Second
	})
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	start := time.Now()
	_, err := s.Expect(context.Background(), protocol.TypeAcquired)
	took := time.Since(start)
	if !IsConnError(err) || !strings.Contains(err.Error(), "nothing for 3s") {
		t.Fatalf("want a stopped-responding error after 3s, got %v", err)
	}
	if took < 2500*time.Millisecond || took > 8*time.Second {
		t.Fatalf("gave up after %s, want about 3s", took)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("hook asked %d times, want once", n)
	}
}

// A helper that speaks again after its grace goes back to the plain limit,
// and the hook is asked afresh when it falls silent again, so a helper that
// recovers and later dies is noticed as quickly as the first time, not only
// after the first grace too.
func TestSilentHelperGraceResets(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-pulse", "") // silent; one heartbeat once rpc-pulse-go exists; silent again
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(time.Duration) time.Duration {
		if asked.Add(1) == 1 {
			e.Set("rpc-pulse-go", "") // the helper recovers during its grace
			return 3 * time.Second
		}
		return 0
	})
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := s.Expect(ctx, protocol.TypeAcquired)
	// A grace kept from the first silence would give up only at 1s+3s,
	// without asking again.
	if !IsConnError(err) || !strings.Contains(err.Error(), "nothing for 1s") {
		t.Fatalf("want a stopped-responding error at the plain limit, got %v", err)
	}
	if n := asked.Load(); n != 2 {
		t.Fatalf("hook asked %d times, want twice (once per silence)", n)
	}
	if l := s.Limit(); l != time.Second {
		t.Fatalf("limit %s after the helper spoke again, want the plain 1s", l)
	}
}

// A hook that grants nothing leaves the limit as it was.
func TestSilentHelperNoGrace(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(time.Duration) time.Duration { asked.Add(1); return 0 })
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	start := time.Now()
	if _, err := s.Expect(context.Background(), protocol.TypeAcquired); !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("took %s with no grace", took)
	}
	if asked.Load() != 1 {
		t.Fatalf("hook asked %d times, want once", asked.Load())
	}
}

// Memory pressure in a heartbeat is remembered for the window asked about;
// heartbeats without memory (older helpers) report none.
func TestHeartbeatMemoryPressure(t *testing.T) {
	e := newFakeEnv(t)
	e.Set("rpc-pressure", "")
	s := e.session(t)
	deadline := time.Now().Add(5 * time.Second)
	for s.MemoryPressure(time.Minute) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p := s.MemoryPressure(time.Minute); !strings.Contains(p, "120 MiB of 16000 MiB") {
		t.Fatalf("pressure %q", p)
	}
	if p := s.MemoryPressure(0); p != "" {
		t.Fatalf("pressure outside the window: %q", p)
	}
	s.Abort()

	e2 := newFakeEnv(t)
	e2.Set("rpc-heartbeat", "")
	s2 := e2.session(t)
	time.Sleep(300 * time.Millisecond) // several bodiless heartbeats
	if p := s2.MemoryPressure(time.Minute); p != "" {
		t.Fatalf("pressure from bodiless heartbeats: %q", p)
	}
	var none *Session
	if none.MemoryPressure(time.Minute) != "" {
		t.Fatal("nil session reported pressure")
	}
}

// Frames the client hasn't read yet don't excuse a silent helper while
// there is room for more (#144): the session still hears the helper, or
// doesn't, and a stall behind them is given up on within the limit.
func TestUnreadFramesDontHideSilence(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-flood", "200") // over half the buffer, short of filling it
	s := e.session(t)
	deadline := time.Now().Add(10 * time.Second)
	for !s.Dead() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !s.Dead() {
		t.Fatal("a silent helper behind unread frames was never given up on")
	}
}

// A full buffer is our backlog, not the helper's silence: nothing more can
// be read until it drains (output piped into a paused pager), so the
// session waits; once it drains, silence counts from then.
func TestFullBufferIsNotSilence(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-flood", "400")
	s := e.session(t)
	time.Sleep(3 * time.Second) // three limits with nothing read
	if s.Dead() {
		t.Fatal("given up on while our own buffer was full")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start, got := time.Now(), 0
	var err error
	for err == nil {
		_, err = s.Next(ctx)
		if err == nil {
			got++
		}
	}
	if !IsConnError(err) || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("after %d frames: want a stopped-responding error, got %v", got, err)
	}
	if got != 400 {
		t.Fatalf("read %d frames before the session ended, want all 400", got)
	}
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Fatalf("given up on %s after the buffer drained: the silence counted the backlog", took)
	}
}
