package transport

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// An upload is judged stalled when, over UploadStallWindow while bytes were
// owed to the helper, less than UploadStallFloor of them (or of what was
// owed, if less) arrived. The helper's keepalives and heartbeats can carry
// on over a connection that delivers almost nothing upstream (#265: a
// retired master crawling at ~1 KB/s, a sync taking 12 minutes instead of
// seconds), so neither the silence watchdog nor ssh's keepalives notice. The
// floor (~2 KiB/s) is far below any link a sync can use, and the window
// spans three helper heartbeats, which carry what has arrived.
var (
	UploadStallWindow       = 60 * time.Second
	UploadStallFloor  int64 = 128 << 10
)

// errUploadStalled is the session error once the watchdog has given up on
// an upload.
var errUploadStalled = errors.New("upload to the runner stalled")

// writePiece bounds each write to the ssh client, so what got through
// shows up within a large frame, not only at its end.
const writePiece = 16 << 10

// countWriter counts what is written to the helper, and notes a write
// under way (blocked on a full pipe).
type countWriter struct {
	w io.Writer
	s *Session
}

func (c countWriter) Write(b []byte) (int, error) {
	c.s.writing.Store(true)
	defer c.s.writing.Store(false)
	n := 0
	for n < len(b) {
		k, err := c.w.Write(b[n:min(len(b), n+writePiece)])
		n += k
		c.s.sent.Add(int64(k))
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// uploadWatch compares what the helper reports having received (in its
// heartbeats) with what was sent to it. Bytes written to the ssh client
// aren't delivered: a master holds up to its channel window and the socket
// buffer, so the tail of an upload can sit there long after the last write
// returned.
type uploadWatch struct {
	mu sync.Mutex
	// window and floor are UploadStallWindow and UploadStallFloor when the
	// session opened.
	window time.Duration
	floor  int64
	// The latest heartbeat's reading, not yet judged: when it arrived (on
	// the session clock), what the helper had received, and what it was
	// owed then.
	fresh    bool
	at, recv int64
	owed     int64
	watching bool  // bytes were owed at the checkpoint
	ckAt     int64 // the checkpoint: when,
	ckRecv   int64 // what had arrived,
	ckOwed   int64 // and what was still owed
}

// note records a heartbeat's report that recv bytes have arrived, at now,
// with sent written so far and writing whether a write is blocked.
func (u *uploadWatch) note(now, recv, sent int64, writing bool) {
	owed := sent - recv
	if writing {
		owed = max(owed, 1)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fresh, u.at, u.recv, u.owed = true, now, recv, owed
}

// reset forgets the checkpoint: time passed that says nothing about the
// connection (this process suspended, or not reading the helper).
func (u *uploadWatch) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.watching, u.fresh = false, false
}

// judge takes in the latest heartbeat's reading and says why the upload has
// stalled, or "". Only readings judge: a helper that went quiet altogether
// is the silence watchdog's to judge.
func (u *uploadWatch) judge() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.fresh {
		return ""
	}
	u.fresh = false
	if u.watching {
		got := u.recv - u.ckRecv
		if got >= min(u.floor, u.ckOwed) {
			u.watching = false // it got through: start afresh from here
		} else if d := time.Duration(u.at - u.ckAt); d >= u.window {
			return fmt.Sprintf("%s arrived in %s, with %s still on its way", kib(max(got, 0)), d.Round(time.Second), kib(u.owed))
		} else {
			return ""
		}
	}
	if u.owed > 0 {
		u.watching, u.ckAt, u.ckRecv, u.ckOwed = true, u.at, u.recv, u.owed
	}
	return ""
}

func kib(n int64) string {
	if n >= 10<<20 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d KiB", (n+1023)>>10)
}

// UploadStalled reports whether the session ended because what was sent to
// the helper stopped arriving while the helper itself kept answering: the
// connection, not the runner, failed.
func (s *Session) UploadStalled() bool { return s != nil && s.stalled.Load() }
