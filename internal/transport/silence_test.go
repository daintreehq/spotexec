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
// Every heartbeat's memory reading replaces the last, healthy or not, while
// pressure reported within the window is kept through a healthy one.
func TestHeartbeatLastMemory(t *testing.T) {
	e := newFakeEnv(t)
	e.Set("rpc-memory", "12000,120,8000")
	s := e.session(t)
	defer s.Abort()
	deadline := time.Now().Add(5 * time.Second)
	for m, _ := s.LastMemory(); m.AvailMB != 8000 && time.Now().Before(deadline); m, _ = s.LastMemory() {
		time.Sleep(20 * time.Millisecond)
	}
	if m, ok := s.LastMemory(); !ok || m.AvailMB != 8000 || m.TotalMB != 16000 {
		t.Fatalf("last memory %+v %v, want the latest reading", m, ok)
	}
	if p := s.MemoryPressure(time.Minute); !strings.Contains(p, "120 MiB") {
		t.Fatalf("a later healthy reading cleared the pressure: %q", p)
	}

	e2 := newFakeEnv(t)
	e2.Set("rpc-memory", "12000")
	s2 := e2.session(t)
	defer s2.Abort()
	deadline = time.Now().Add(5 * time.Second)
	for _, ok := s2.LastMemory(); !ok && time.Now().Before(deadline); _, ok = s2.LastMemory() {
		time.Sleep(20 * time.Millisecond)
	}
	if m, ok := s2.LastMemory(); !ok || m.AvailMB != 12000 {
		t.Fatalf("healthy reading not kept: %+v %v", m, ok)
	}
	if p := s2.MemoryPressure(time.Minute); p != "" {
		t.Fatalf("pressure from a healthy reading: %q", p)
	}
}

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
	if m, ok := s.LastMemory(); !ok || m.AvailMB != 120 || m.TotalMB != 16000 || m.At.IsZero() {
		t.Fatalf("last memory %+v %v", m, ok)
	}
	s.Abort()

	e2 := newFakeEnv(t)
	e2.Set("rpc-heartbeat", "")
	s2 := e2.session(t)
	time.Sleep(300 * time.Millisecond) // several bodiless heartbeats
	if p := s2.MemoryPressure(time.Minute); p != "" {
		t.Fatalf("pressure from bodiless heartbeats: %q", p)
	}
	if m, ok := s2.LastMemory(); ok {
		t.Fatalf("memory from bodiless heartbeats: %+v", m)
	}
	var none *Session
	if none.MemoryPressure(time.Minute) != "" {
		t.Fatal("nil session reported pressure")
	}
	if _, ok := none.LastMemory(); ok {
		t.Fatal("nil session reported memory")
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
	select {
	case <-s.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("a silent helper behind unread frames was never given up on")
	}
	if n := len(s.frames); n < cap(s.frames)/2 {
		t.Fatalf("%d frames queued when the session ended, want at least half the buffer", n)
	}
	if err := s.LostErr(); !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("want a stopped-responding error, got %v", err)
	}
}

// A full buffer is our backlog, not the helper's silence: nothing more can
// be read until it drains (output piped into a paused pager), so the
// session waits; once a frame is taken, silence counts from then, not from
// when the helper last sent anything.
func TestFullBufferIsNotSilence(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-flood", "257") // the buffer's 256, and one more waiting to go in
	s := e.session(t)
	deadline := time.Now().Add(5 * time.Second)
	for !s.backed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.backed.Load() {
		t.Fatalf("buffer never filled (%d frames)", len(s.frames))
	}
	time.Sleep(3 * time.Second) // three limits with nothing read
	if s.Dead() {
		t.Fatal("given up on while our own buffer was full")
	}
	// Taking one frame lets the last one in; nothing else arrives.
	if _, err := s.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	taken := time.Now()
	select {
	case <-s.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("never given up on after the buffer drained")
	}
	if took := time.Since(taken); took < 500*time.Millisecond {
		t.Fatalf("given up on %s after a frame was taken: the silence counted the backlog", took)
	}
	if err := s.LostErr(); !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("want a stopped-responding error, got %v", err)
	}
}

// A grace counts from when it is granted: a hook that took a while to
// answer (asking the cloud) doesn't eat into it, so "waiting up to N more"
// is true.
func TestSilentHelperGraceCountsFromGrant(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	s.SetOnSilent(func(time.Duration) time.Duration {
		time.Sleep(1500 * time.Millisecond) // past base+grace before it answers
		return time.Second
	})
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	start := time.Now()
	_, err := s.Expect(context.Background(), protocol.TypeAcquired)
	if !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
	// Silent 1s, the hook answers 1.5s later, then 1s more: about 3.5s.
	// Added to the base limit, the grace would already have run out.
	if took := time.Since(start); took < 3300*time.Millisecond || took > 6*time.Second {
		t.Fatalf("gave up after %s, want about 3.5s", took)
	}
}

// sleepFor moves the clock that counts sleep forward by d, as a machine
// that slept for d sees it: its monotonic clock stopped meanwhile.
func sleepFor(t *testing.T, d time.Duration) {
	bootSkew.Add(int64(d))
	t.Cleanup(func() { bootSkew.Add(-int64(d)) })
}

// This machine asleep (#142) is not the helper's silence: a helper still
// there when it wakes carries on. The sleep counts against the session
// until the helper has been heard well after it.
func TestSleepIsNotHelperSilence(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	short(t, &HelperPatience, 10*time.Second)
	e.Set("rpc-heartbeat", "")
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(time.Duration) time.Duration { asked.Add(1); return 0 })
	time.Sleep(300 * time.Millisecond)
	sleepFor(t, 30*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for s.Suspended() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d := s.Suspended(); d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("Suspended() = %s, want about 30s", d)
	}
	deadline = time.Now().Add(3 * time.Second)
	for s.Suspended() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d := s.Suspended(); d != 0 {
		t.Fatalf("Suspended() = %s although the helper kept talking after it", d)
	}
	if s.Dead() || asked.Load() != 0 {
		t.Fatalf("a heartbeating helper was judged silent after a sleep (dead %v, hook asked %d times)", s.Dead(), asked.Load())
	}
}

// After a sleep longer than the helper's patience, the helper gets the
// plain limit to speak from when this machine woke, but no grace for a VM
// still running (it has most likely given up on this client), and the
// error says why.
func TestSleepPastTheHelperPatience(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	short(t, &HelperPatience, 10*time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(time.Duration) time.Duration { asked.Add(1); return time.Minute })
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	time.Sleep(600 * time.Millisecond)
	sleepFor(t, 15*time.Minute+41*time.Second)
	woke := time.Now()
	_, err := s.Expect(context.Background(), protocol.TypeAcquired)
	if !IsConnError(err) || !strings.Contains(err.Error(), "nothing for 1s after this machine was asleep or sx was stopped for 15m4") {
		t.Fatalf("want a stopped-responding error naming the sleep, got %v", err)
	}
	// Counted from the last receive, as before, it would end ~0.4s after.
	if took := time.Since(woke); took < 900*time.Millisecond || took > 4*time.Second {
		t.Fatalf("gave up %s after waking, want the plain 1s", took)
	}
	if asked.Load() != 0 {
		t.Fatalf("hook asked %d times after a sleep the helper may not have outlasted", asked.Load())
	}
	if d := s.Suspended(); d < 15*time.Minute {
		t.Fatalf("Suspended() = %s", d)
	}
}

// A sleep after a grace was granted ends it: the helper gets the plain
// limit after waking, without the hook being asked again.
func TestSleepDropsTheGrace(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	short(t, &HelperPatience, 10*time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(time.Duration) time.Duration { asked.Add(1); return time.Minute })
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	deadline := time.Now().Add(5 * time.Second)
	for s.Limit() < time.Minute && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Limit() < time.Minute {
		t.Fatal("no grace was granted")
	}
	sleepFor(t, 30*time.Second)
	woke := time.Now()
	_, err := s.Expect(context.Background(), protocol.TypeAcquired)
	if !IsConnError(err) || !strings.Contains(err.Error(), "asleep or sx was stopped for 30s") {
		t.Fatalf("want a stopped-responding error naming the sleep, got %v", err)
	}
	if took := time.Since(woke); took > 5*time.Second {
		t.Fatalf("gave up %s after waking: the minute's grace survived the sleep", took)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("hook asked %d times, want once (before the sleep)", n)
	}
}

// A sleep while the hook runs (asking the cloud) drops the grace it
// returns, and a short one leaves the hook to be asked again.
func TestSleepDuringTheHook(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	var asked atomic.Int32
	s.SetOnSilent(func(time.Duration) time.Duration {
		if asked.Add(1) == 1 {
			sleepFor(t, 20*time.Second) // under the helper's patience
			return time.Minute
		}
		return 0
	})
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.Expect(ctx, protocol.TypeAcquired); !IsConnError(err) {
		t.Fatalf("want a connection error (the minute's grace dropped), got %v", err)
	}
	if n := asked.Load(); n != 2 {
		t.Fatalf("hook asked %d times, want twice: again after the short sleep", n)
	}
}

// Waking can end the connection before the watchdog's next tick (the
// helper has gone): the read loop still records the sleep, and the error
// says so.
func TestConnectionEndingOnWakeNamesTheSleep(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, 40*time.Second) // first tick after 5s
	short(t, &HelperPatience, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	sleepFor(t, 10*time.Minute)
	s.Abort() // the channel ends: the read loop sees it first
	_, err := s.Next(context.Background())
	if !IsConnError(err) || !strings.Contains(err.Error(), "connection to runner lost after this machine was asleep or sx was stopped for 10m0s") {
		t.Fatalf("want a lost-connection error naming the sleep, got %v", err)
	}
	if d := s.Suspended(); d < 9*time.Minute {
		t.Fatalf("Suspended() = %s", d)
	}
}

// A sleep shorter than the helper's patience doesn't count against the
// session.
func TestShortSleepIsNotReported(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	short(t, &HelperPatience, time.Minute)
	e.Set("rpc-heartbeat", "")
	s := e.session(t)
	time.Sleep(300 * time.Millisecond)
	sleepFor(t, 30*time.Second)
	time.Sleep(600 * time.Millisecond)
	if d := s.Suspended(); d != 0 {
		t.Fatalf("Suspended() = %s for a sleep the helper outlasts", d)
	}
	var none *Session
	if none.Suspended() != 0 {
		t.Fatal("nil session reported a suspension")
	}
}

// The end of the connection can arrive before the watchdog's first tick
// after waking: the read loop's observation still records the sleep, and
// one taken before the sleep doesn't count it twice.
func TestObserveRecordsASleepOnce(t *testing.T) {
	s := &Session{born: time.Now(), period: time.Second, patience: 10 * time.Second}
	s.clk.boot, s.clk.bootOK = sinceBoot()
	if d := s.observe(0); d != 0 {
		t.Fatalf("observed %s without a sleep", d)
	}
	sleepFor(t, 20*time.Minute)
	if d := s.observe(0); d < 19*time.Minute || d > 21*time.Minute {
		t.Fatalf("observed %s, want about 20m", d)
	}
	if d := s.observe(0); d != 0 {
		t.Fatalf("observed the same sleep again: %s", d)
	}
	// A watchdog that was busy (or ticks late) is no suspension unless it
	// ticked late by more than the time it was busy.
	s.clk.mono -= int64(time.Minute)
	if d := s.observe(time.Minute); d != 0 {
		t.Fatalf("observed %s for a busy watchdog", d)
	}
	s.clk.mono -= int64(time.Minute)
	s.hooking.Store(true)
	if d := s.observe(0); d != 0 {
		t.Fatalf("the read loop observed %s while the watchdog was in the hook", d)
	}
	s.hooking.Store(false)
	s.clk.mono -= int64(time.Minute)
	if d := s.observe(0); d < 58*time.Second {
		t.Fatalf("observed %s for a process stopped a minute", d)
	}
	if d := s.Suspended(); d < 19*time.Minute {
		t.Fatalf("Suspended() = %s", d)
	}
}
