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

// An upload crawling over a connection whose helper keeps answering (#265)
// is given up on within the window, as a stalled upload, and the master it
// ran on is retired so the next session dials afresh.
func TestCrawlingUploadStalls(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &UploadStallWindow, time.Second)
	short(t, &SilenceLimit, 2*time.Second) // the watchdog ticks every 500ms
	e.Set("rpc-crawl", "64")               // ~1 KiB/s, with a heartbeat every 50ms
	s := e.session(t)
	errc := upload(s)
	start := time.Now()
	_, err := s.Expect(context.Background(), protocol.TypeAcquired)
	if !IsConnError(err) || !strings.Contains(err.Error(), "upload to the runner stalled") {
		t.Fatalf("want a stalled-upload error, got %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("gave up after %s", took)
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
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.Log(), "stop 101") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got, want := e.Log(), "dial 101\nstop 101\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// A session that outlived its master's replacement never stops the
// replacement when its upload stalls.
func TestStalledUploadKeepsReplacementMaster(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &UploadStallWindow, time.Second)
	short(t, &SilenceLimit, 2*time.Second) // the watchdog ticks every 500ms
	e.Set("rpc-crawl", "0")
	s := e.session(t)
	e.Set("master", "102") // another run replaced master 101
	upload(s)
	if _, err := s.Expect(context.Background(), protocol.TypeAcquired); !s.UploadStalled() {
		t.Fatalf("want a stalled upload, got %v", err)
	}
	time.Sleep(300 * time.Millisecond) // the retirement runs after the abort
	if strings.Contains(e.Log(), "stop") {
		t.Fatalf("replacement retired:\n%s", e.Log())
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
	upload(s)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Expect(ctx, protocol.TypeAcquired); err != context.DeadlineExceeded {
		t.Fatalf("want our own deadline, got %v", err)
	}
	if s.UploadStalled() || s.Dead() {
		t.Fatal("a flowing upload was cut off")
	}
}

// The judgment from heartbeat readings alone.
func TestUploadWatchJudge(t *testing.T) {
	sec := func(n int) int64 { return int64(time.Duration(n) * time.Second) }
	type reading struct {
		at         int
		recv, sent int64
		writing    bool
		reset      bool // the watch is reset before this reading
	}
	for _, tc := range []struct {
		name  string
		reads []reading
		stall bool
	}{
		{"idle: client heartbeats in flight, each reading caught up", []reading{
			{0, 100, 130, false, false}, {20, 130, 160, false, false}, {40, 160, 190, false, false}, {60, 190, 220, false, false}, {80, 220, 250, false, false},
		}, false},
		{"crawl: 1 KiB/s with megabytes owed", []reading{
			{0, 1 << 20, 3 << 20, true, false}, {20, 1<<20 + 20<<10, 3 << 20, true, false}, {40, 1<<20 + 40<<10, 3 << 20, true, false}, {60, 1<<20 + 60<<10, 3 << 20, true, false},
		}, true},
		{"crawl: the tail of an upload stuck after the last write returned", []reading{
			{0, 1 << 20, 2 << 20, false, false}, {30, 1 << 20, 2 << 20, false, false}, {60, 1 << 20, 2 << 20, false, false},
		}, true},
		{"slow but fine: 4 KiB/s", []reading{
			{0, 0, 3 << 20, true, false}, {20, 80 << 10, 3 << 20, true, false}, {40, 160 << 10, 3 << 20, true, false}, {60, 240 << 10, 3 << 20, true, false}, {80, 320 << 10, 3 << 20, true, false},
		}, false},
		{"owed little, all of it arrived", []reading{
			{0, 1000, 1200, false, false}, {50, 1200, 1300, false, false}, {100, 1300, 1400, false, false},
		}, false},
		{"stuck across a suspension is not judged", []reading{
			{0, 1 << 20, 2 << 20, false, false}, {30, 1 << 20, 2 << 20, false, false}, {60, 1 << 20, 2 << 20, false, true}, {90, 1 << 20, 2 << 20, false, false},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := uploadWatch{window: time.Minute, floor: 128 << 10}
			stalled := false
			for _, r := range tc.reads {
				if r.reset {
					u.reset()
				}
				u.note(sec(r.at), r.recv, r.sent, r.writing)
				if why := u.judge(); why != "" {
					stalled = true
				}
				if u.judge() != "" {
					t.Fatal("a reading judged twice")
				}
			}
			if stalled != tc.stall {
				t.Fatalf("stalled %v, want %v", stalled, tc.stall)
			}
		})
	}
}
