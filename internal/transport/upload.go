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
	// armed: an upload spotexec drives (a sync, a .git seed) is under way.
	// Nothing else is judged: once a command runs, only control messages
	// go up, and a reply held up behind its output must not look like an
	// upload going nowhere.
	armed bool
	// The latest heartbeat's reading, not yet judged: when it arrived (on
	// the session clock), what the helper had received, how long it had
	// left what we sent unread (ns), and what it was owed then.
	fresh          bool
	at, recv, held int64
	owed           int64
	watching       bool  // bytes were owed at the checkpoint
	ckAt           int64 // the checkpoint: when,
	ckRecv, ckHeld int64 // what had arrived, how long the helper had held,
	ckOwed         int64 // and what was still owed
}

// note records a heartbeat's report, at now, that recv bytes have arrived
// and the helper held what we sent unread for held in all, with sent
// written so far and writing whether a write is blocked.
func (u *uploadWatch) note(now, recv int64, held time.Duration, sent int64, writing bool) {
	owed := sent - recv
	if writing {
		owed = max(owed, 1)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fresh, u.at, u.recv, u.held, u.owed = true, now, recv, int64(held), owed
}

// reset forgets the checkpoint and any reading not yet judged: time passed
// that says nothing about the connection (this process suspended, or not
// reading the helper).
func (u *uploadWatch) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.watching, u.fresh = false, false
}

// arm starts (on) or ends judging uploads, from the next reading.
func (u *uploadWatch) arm(on bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.armed, u.watching, u.fresh = on, false, false
}

// judge takes in the latest heartbeat's reading and, if the upload has
// stalled, hands why to commit while still holding the watch (so the upload
// can't be declared over meanwhile), and reports whether commit took it.
// Only readings judge: a helper that went quiet altogether is the silence
// watchdog's to judge. Time the helper held what we sent unread doesn't
// count.
func (u *uploadWatch) judge(commit func(why string) bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if why := u.verdict(); why != "" && commit(why) {
		u.armed = false
		return true
	}
	return false
}

// verdict is judge's reading of the latest heartbeat, with mu held.
func (u *uploadWatch) verdict() string {
	if !u.fresh || !u.armed {
		u.fresh = false
		return ""
	}
	u.fresh = false
	if u.watching {
		got := u.recv - u.ckRecv
		if got >= min(u.floor, u.ckOwed) {
			u.watching = false // it got through: start afresh from here
		} else if d := time.Duration(u.at - u.ckAt - max(u.held-u.ckHeld, 0)); d >= u.window {
			return fmt.Sprintf("%s arrived in %s, with %s still on its way", kib(max(got, 0)), d.Round(time.Second), kib(u.owed))
		} else {
			return ""
		}
	}
	if u.owed > 0 {
		u.watching, u.ckAt, u.ckRecv, u.ckHeld, u.ckOwed = true, u.at, u.recv, u.held, u.owed
	}
	return ""
}

func kib(n int64) string {
	if n >= 10<<20 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d KiB", (n+1023)>>10)
}

// WatchUploads starts (on) or ends watching that what is sent to the helper
// arrives: on around an upload spotexec drives, such as a sync, off before
// anything else. An upload judged stalled before it is turned off stays so
// (UploadStalled).
func (s *Session) WatchUploads(on bool) { s.up.arm(on) }

// UploadStalled reports whether the session ended because what was sent to
// the helper stopped arriving while the helper itself kept answering: the
// connection, not the runner, failed. Once it does, the master the session
// ran on has been retired (or found replaced) before it returns, so a retry
// can't open its session there: it waits up to retireWait for that.
func (s *Session) UploadStalled() bool {
	if s == nil {
		return false
	}
	s.errMu.Lock()
	err := s.err
	s.errMu.Unlock()
	if !errors.Is(err, errUploadStalled) {
		return false
	}
	select {
	case <-s.retired:
	case <-time.After(retireWait):
	}
	return true
}

// retireWait bounds how long UploadStalled waits for the master to be
// retired: the lock and two control commands, each within muxTimeout.
const retireWait = 2*muxTimeout + time.Second
