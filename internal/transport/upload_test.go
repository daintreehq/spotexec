package transport

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// upload writes blob frames to the helper until a write fails, and reports
// that failure.
func upload(s *Session) <-chan error {
	errc := make(chan error, 1)
	go func() {
		chunk := make([]byte, 64<<10)
		for {
			if err := s.Conn.WriteFrame(protocol.KindBlob, chunk); err != nil {
				errc <- err
				return
			}
		}
	}()
	return errc
}

// expectWithin waits for a reply the helper never sends, for at most d.
func expectWithin(s *Session, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, err := s.Expect(ctx, protocol.TypeAcquired)
	return err
}

// An upload crawling over a connection whose helper keeps answering (#265)
// is given up on within the window, as a stalled upload, and the master it
// ran on is retired before anyone hears of it, so a retry dials afresh.
func TestCrawlingUploadStalls(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &UploadStallWindow, time.Second)
	short(t, &SilenceLimit, 2*time.Second) // the watchdog ticks every 500ms
	e.Set("rpc-crawl", "64")               // ~1 KiB/s, with a heartbeat every 50ms
	s := e.session(t)
	s.WatchUploads(true)
	errc := upload(s)
	err := expectWithin(s, 10*time.Second)
	if !IsConnError(err) || !strings.Contains(err.Error(), "upload to the runner stalled") {
		t.Fatalf("want a stalled-upload error, got %v", err)
	}
	if !strings.Contains(e.Log(), "stop 101") {
		t.Fatalf("master not retired by the time the stall was heard of:\n%s", e.Log())
	}
	if !s.UploadStalled() || !s.Dead() {
		t.Fatalf("stalled %v dead %v, want both", s.UploadStalled(), s.Dead())
	}
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("write still blocked")
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session never ended")
	}
	if got, want := e.Log(), "dial 101\nstop 101\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// Uploads aren't judged unless watched: once a command runs, only control
// messages go up, and replies held up behind its output must not end it.
func TestUnwatchedUploadIsNotJudged(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &UploadStallWindow, 300*time.Millisecond)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-crawl", "64")
	s := e.session(t)
	defer s.Abort()
	s.WatchUploads(true)
	s.WatchUploads(false)
	upload(s)
	if err := expectWithin(s, 2*time.Second); err != context.DeadlineExceeded {
		t.Fatalf("want our own deadline, got %v", err)
	}
	if s.UploadStalled() || s.Dead() {
		t.Fatal("an unwatched upload was judged")
	}
}

// An upload the helper takes in as fast as it is sent is never cut off,
// however long it runs.
func TestFlowingUploadIsNotStalled(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &UploadStallWindow, 300*time.Millisecond)
	short(t, &SilenceLimit, time.Second) // the watchdog ticks every 250ms
	e.Set("rpc-heartbeat", "")
	s := e.session(t)
	defer s.Abort()
	s.WatchUploads(true)
	upload(s)
	if err := expectWithin(s, 2*time.Second); err != context.DeadlineExceeded {
		t.Fatalf("want our own deadline, got %v", err)
	}
	if s.UploadStalled() || s.Dead() {
		t.Fatal("a flowing upload was cut off")
	}
	if got := s.sent.Load(); got < 4*UploadStallFloor {
		t.Fatalf("only %d bytes sent: the upload never got going", got)
	}
}

// retireMaster stops only the exact master it is given, and nothing for an
// unknown one.
func TestRetireMasterOnlyThatMaster(t *testing.T) {
	e := newFakeEnv(t)
	e.Set("master", "102")
	e.c.retireMaster(101) // replaced meanwhile
	e.c.retireMaster(0)   // unknown
	if !e.c.MasterAlive(context.Background()) || e.Log() != "" {
		t.Fatalf("retired a master it wasn't given: %q", e.Log())
	}
	e.c.retireMaster(102)
	if e.c.MasterAlive(context.Background()) || e.Log() != "stop 102\n" {
		t.Fatalf("its own master kept: %q", e.Log())
	}
}

// The judgment from heartbeat readings alone: each reading's verdict.
func TestUploadWatchJudge(t *testing.T) {
	type reading struct {
		at      int // s
		recv    int64
		held    int // s the helper held what we sent unread, in all
		sent    int64
		writing bool
		reset   bool // the watch is reset before this reading
		stall   bool // the verdict on this reading
	}
	const M = 1 << 20
	for _, tc := range []struct {
		name  string
		reads []reading
	}{
		{"idle: client heartbeats in flight, each reading caught up", []reading{
			{at: 0, recv: 100, sent: 130}, {at: 20, recv: 130, sent: 160}, {at: 40, recv: 160, sent: 190}, {at: 60, recv: 190, sent: 220}, {at: 80, recv: 220, sent: 250},
		}},
		{"crawl: 1 KiB/s with megabytes owed", []reading{
			{at: 0, recv: M, sent: 3 * M, writing: true}, {at: 20, recv: M + 20<<10, sent: 3 * M, writing: true}, {at: 40, recv: M + 40<<10, sent: 3 * M, writing: true},
			{at: 59, recv: M + 59<<10, sent: 3 * M, writing: true}, {at: 60, recv: M + 60<<10, sent: 3 * M, writing: true, stall: true},
		}},
		{"the tail of an upload stuck after the last write returned", []reading{
			{at: 0, recv: M, sent: 2 * M}, {at: 30, recv: M, sent: 2 * M}, {at: 60, recv: M, sent: 2 * M, stall: true},
		}},
		{"a blocked write with everything written arrived", []reading{
			{at: 0, recv: M, sent: M, writing: true}, {at: 60, recv: M, sent: M, writing: true, stall: true},
		}},
		{"exactly the floor in the window", []reading{
			{at: 0, sent: 3 * M, writing: true}, {at: 60, recv: 128 << 10, sent: 3 * M, writing: true}, {at: 120, recv: 256 << 10, sent: 3 * M, writing: true},
		}},
		{"owed little, all of it arrived", []reading{
			{at: 0, recv: 1000, sent: 1200}, {at: 50, recv: 1200, sent: 1300}, {at: 100, recv: 1300, sent: 1400},
		}},
		{"the helper held what we sent: its time doesn't count", []reading{
			{at: 0, recv: M, sent: 3 * M, writing: true}, {at: 60, recv: M, held: 50, sent: 3 * M, writing: true}, {at: 109, recv: M, held: 50, sent: 3 * M, writing: true},
			{at: 110, recv: M, held: 50, sent: 3 * M, writing: true, stall: true},
		}},
		{"stuck across a suspension is judged only from after it", []reading{
			{at: 0, recv: M, sent: 2 * M}, {at: 30, recv: M, sent: 2 * M}, {at: 60, recv: M, sent: 2 * M, reset: true}, {at: 90, recv: M, sent: 2 * M},
			{at: 120, recv: M, sent: 2 * M, stall: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := uploadWatch{window: time.Minute, floor: 128 << 10}
			u.arm(true)
			for _, r := range tc.reads {
				if r.reset {
					u.reset()
				}
				u.note(int64(time.Duration(r.at)*time.Second), r.recv, time.Duration(r.held)*time.Second, r.sent, r.writing)
				if got := u.judge() != ""; got != r.stall {
					t.Fatalf("at %ds: stalled %v, want %v", r.at, got, r.stall)
				}
				if u.judge() != "" {
					t.Fatal("a reading judged twice")
				}
			}
		})
	}
	// Unarmed, the same crawl is never judged.
	u := uploadWatch{window: time.Minute, floor: 128 << 10}
	for _, at := range []int{0, 60, 120} {
		u.note(int64(time.Duration(at)*time.Second), 1<<20, 0, 3<<20, true)
		if u.judge() != "" {
			t.Fatal("an unarmed watch judged a stall")
		}
	}
}
