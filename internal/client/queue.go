package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// Queue is the local, fair admission queue for one pool of runners. Every
// waiting sx process holds a ticket (a flock'd file) ordered by priority,
// then arrival. Only tickets near the head may probe runners for a free
// slot, so a hundred waiting agents cost a handful of SSH probes per second
// instead of a hundred, and nobody is overtaken by a later arrival. A
// ticket whose process dies releases its flock and is swept.
type Queue struct {
	dir string
}

// Ticket is one waiting process's place in the queue.
type Ticket struct {
	f    *os.File
	path string
	tk   ticketData
}

type ticketData struct {
	Priority  int    `json:"p"`
	Enqueued  int64  `json:"t"` // unix nanoseconds
	Exclusive bool   `json:"x,omitempty"`
	Command   string `json:"c,omitempty"`
	PID       int    `json:"pid"`
}

// QueueFor returns the queue shared by every process using this set of
// runners (order-independent).
func QueueFor(pool []*config.Runner) *Queue {
	names := make([]string, 0, len(pool))
	for _, r := range pool {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	h := sha256.Sum256([]byte(strings.Join(names, ",")))
	return &Queue{dir: filepath.Join(config.StateDir(), "queues", hex.EncodeToString(h[:])[:12])}
}

// staleAfter: a ticket whose holder has not refreshed it for this long is
// ignored (the process is alive but stopped, e.g. Ctrl-Z): it must not block
// the queue. It counts again as soon as its holder resumes.
const staleAfter = 20 * time.Second

// queueLockWait bounds how long a lookup waits for queue.lock: a process
// stopped while holding it must not wedge every other agent. Routine
// contention never comes near it, since a waiter is answered by whoever
// holds the lock (snapshotTTL, below).
var queueLockWait = 5 * time.Second

// snapshotTTL: how long one scan of the queue answers every lookup whose
// queue is unchanged since. A queue of N tickets polled by all N would
// otherwise cost N² ticket inspections, serialized under queue.lock (#272).
var snapshotTTL = 250 * time.Millisecond

const (
	ticketExt    = ".ticket"
	joiningExt   = ".joining" // a ticket being written, not yet in the queue
	snapshotName = "snapshot.json"
)

// Touch refreshes the ticket's heartbeat.
func (t *Ticket) Touch() {
	if t != nil && t.f != nil {
		now := time.Now()
		_ = os.Chtimes(t.path, now, now)
	}
}

// Join enqueues this process. The ticket is written and locked under a
// private name, and only then linked into the queue, so a sweep never sees
// it unlocked and removes it as a dead process's. It takes no queue lock:
// a burst of arrivals is never refused because lookups are busy.
func (q *Queue) Join(priority int, exclusive bool, command string) (*Ticket, error) {
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		return nil, err
	}
	if len(command) > 120 {
		command = command[:119] + "…"
	}
	for attempt := 1; ; attempt++ {
		t, err := q.join(ticketData{Priority: priority, Enqueued: time.Now().UnixNano(), Exclusive: exclusive, Command: command, PID: os.Getpid()})
		if err == nil || !errors.Is(err, errJoinRace) || attempt == 3 {
			return t, err
		}
	}
}

// errJoinRace: a sweep took the half-written ticket (this process was
// stopped mid-Join for longer than staleAfter); try again under a new name.
var errJoinRace = errors.New("queue ticket swept while being written")

func (q *Queue) join(tk ticketData) (*Ticket, error) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	base := filepath.Join(q.dir, fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b[:])))
	tmp, path := base+joiningExt, base+ticketExt
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if os.IsExist(err) {
		return nil, errJoinRace // a name already taken: draw another
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Ticket, error) {
		_ = os.Remove(tmp)
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			_ = f.Close() // the sweep holds it and removes it
			return nil, errJoinRace
		}
		return fail(err)
	}
	data, _ := json.Marshal(tk)
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	// A link, not a rename, where the filesystem has them: it never
	// replaces another ticket of that name.
	err = os.Link(tmp, path)
	switch {
	case err == nil:
		_ = os.Remove(tmp)
	case os.IsExist(err):
		return fail(errJoinRace)
	case os.IsNotExist(err):
		_ = f.Close()
		return nil, errJoinRace
	default:
		if err := os.Rename(tmp, path); err != nil {
			if os.IsNotExist(err) {
				_ = f.Close()
				return nil, errJoinRace
			}
			return fail(err)
		}
	}
	return &Ticket{f: f, path: path, tk: tk}, nil
}

// Leave removes the ticket.
func (t *Ticket) Leave() {
	if t == nil || t.f == nil {
		return
	}
	_ = os.Remove(t.path)
	_ = syscall.Flock(int(t.f.Fd()), syscall.LOCK_UN)
	_ = t.f.Close()
	t.f = nil
}

// QueueState is a ticket's view of the queue.
type QueueState struct {
	Position      int // 0-based
	Total         int
	ExclusiveHead bool // an exclusive ticket is ahead of us
}

// unknownPosition: the position of a ticket whose place can't be told; it
// never probes on a guess.
const unknownPosition = 1 << 20

// State returns the ticket's position among live tickets (and refreshes
// the ticket's heartbeat). One process at a time scans the queue, under
// queue.lock, and publishes what it saw; everyone else answers from that.
func (q *Queue) State(t *Ticket) QueueState {
	t.Touch()
	name := filepath.Base(t.path)
	asked := time.Now()
	var looked os.FileInfo // the last snapshot read, so it is decoded once
	if st, ok := q.cached(name, asked, &looked); ok {
		return st
	}
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		return QueueState{Position: unknownPosition}
	}
	lf, err := os.OpenFile(filepath.Join(q.dir, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return QueueState{Position: unknownPosition}
	}
	defer lf.Close()
	deadline := asked.Add(queueLockWait)
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			return QueueState{Position: unknownPosition}
		}
		// Busy: whoever holds it is scanning, and its snapshot answers us.
		if st, ok := q.cached(name, asked, &looked); ok {
			return st
		}
		if time.Now().After(deadline) {
			// Nobody published anything for all that time: the holder is
			// stopped. Can't tell where we are: don't probe this round.
			return QueueState{Position: unknownPosition}
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	if st, ok := q.cached(name, asked, &looked); ok {
		return st
	}
	snap, ok := q.scan()
	if !ok {
		return QueueState{Position: unknownPosition} // can't tell: don't probe this round
	}
	q.publish(snap)
	st, _ := snap.position(name)
	return st
}

// snapshot is one scan of the queue: the live tickets in admission order,
// and every other ticket it saw, so a reader can tell whether the queue
// has changed since.
type snapshot struct {
	Start int64       `json:"start"` // unix nanoseconds, when the scan began
	Done  int64       `json:"done"`  // and when it finished
	Live  []snapEntry `json:"live"`
	Stale []string    `json:"stale,omitempty"` // held, but not refreshed lately
	Other []string    `json:"other,omitempty"` // unreadable
}

type snapEntry struct {
	Name      string `json:"n"`
	Exclusive bool   `json:"x,omitempty"`
}

// position finds name among the live tickets.
func (s *snapshot) position(name string) (QueueState, bool) {
	st := QueueState{Total: len(s.Live)}
	for i, e := range s.Live {
		if e.Name == name {
			st.Position = i
			return st, true
		}
		if e.Exclusive {
			st.ExclusiveHead = true
		}
	}
	st.Position = unknownPosition // our ticket is unreadable: never admit on a guess
	return st, false
}

// cached answers a lookup asked at `asked` from the published snapshot,
// when that is as good as a scan of our own: a scan that began after we
// asked, or a recent one of a queue that hasn't changed since (no ticket
// joined or left, none of the stopped ones resumed). looked is the last
// snapshot this lookup read: one found wanting isn't decoded again.
func (q *Queue) cached(name string, asked time.Time, looked *os.FileInfo) (QueueState, bool) {
	path := filepath.Join(q.dir, snapshotName)
	same := func(fi os.FileInfo) bool {
		prev := *looked
		return prev != nil && os.SameFile(prev, fi) && prev.ModTime().Equal(fi.ModTime()) && prev.Size() == fi.Size()
	}
	if fi, err := os.Stat(path); err != nil || same(fi) {
		return QueueState{}, false // none yet, or the one already found wanting
	}
	f, err := os.Open(path)
	if err != nil {
		return QueueState{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return QueueState{}, false
	}
	*looked = fi
	b, err := io.ReadAll(f)
	if err != nil {
		return QueueState{}, false
	}
	var s snapshot
	if json.Unmarshal(b, &s) != nil {
		return QueueState{}, false
	}
	start, done, now := time.Unix(0, s.Start), time.Unix(0, s.Done), time.Now()
	if start.After(done) || done.After(now) {
		return QueueState{}, false // the clock went back: can't tell its age
	}
	st, ok := s.position(name)
	if !ok {
		return QueueState{}, false // not seen yet (or no longer live): scan
	}
	if !start.Before(asked) {
		// As good as our own scan. One that waited longer than we would
		// have for the lock is not (this process was stopped meanwhile).
		return st, now.Sub(done) <= queueLockWait
	}
	if now.Sub(done) > snapshotTTL || !s.unchanged(q.dir) {
		return QueueState{}, false
	}
	return st, true
}

// unchanged reports whether the queue still holds exactly the tickets the
// snapshot saw, and the stopped ones are still stopped. A live ticket
// that has since died or stopped counts until the snapshot expires, which
// only holds back those behind it.
func (s *snapshot) unchanged(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	seen := make(map[string]bool, len(s.Live)+len(s.Stale)+len(s.Other))
	for _, e := range s.Live {
		seen[e.Name] = true
	}
	for _, n := range s.Stale {
		seen[n] = true
	}
	for _, n := range s.Other {
		seen[n] = true
	}
	count := 0
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ticketExt) {
			continue
		}
		if !seen[e.Name()] {
			return false // joined since
		}
		count++
	}
	if count != len(seen) {
		return false // left since
	}
	for _, n := range s.Stale {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil || time.Since(fi.ModTime()) <= staleAfter {
			return false // resumed
		}
	}
	return true
}

// publish replaces the snapshot. Must be called with queue.lock held.
func (q *Queue) publish(s snapshot) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := filepath.Join(q.dir, snapshotName+".tmp")
	if os.WriteFile(tmp, data, 0o600) != nil {
		_ = os.Remove(tmp)
		return
	}
	if os.Rename(tmp, filepath.Join(q.dir, snapshotName)) != nil {
		_ = os.Remove(tmp)
	}
}

type liveTicket struct {
	name string
	tk   ticketData
}

// queueScans counts full scans of a queue (tests check they're coalesced).
var queueScans atomic.Int64

// beforeInspect, when set, runs before a scan looks at each ticket (tests
// change the queue mid-scan).
var beforeInspect func(name string)

// scanPasses bounds how many times a scan lists the queue again to look
// at tickets that joined while it ran.
const scanPasses = 32

// scan lists tickets held by running processes, in admission order, and
// sweeps dead ones (and tickets abandoned while being written). It looks
// again at the queue until a listing holds no ticket it hasn't looked at,
// so what it returns was true at that moment: a ticket that joined while
// it ran is never missed because the one it overtook left meanwhile. ok
// is false when it couldn't see the whole queue (an I/O error, or tickets
// joining faster than it could look): nobody may be admitted on it. Must
// be called with queue.lock held.
func (q *Queue) scan() (s snapshot, ok bool) {
	queueScans.Add(1)
	s.Start = time.Now().UnixNano()
	var live []liveTicket
	buf := make([]byte, 4096)
	looked := map[string]bool{}
	for pass := 1; ; pass++ {
		ents, err := os.ReadDir(q.dir)
		if err != nil {
			return s, false
		}
		more := false
		for _, e := range ents {
			name := e.Name()
			if looked[name] {
				continue
			}
			looked[name] = true
			switch {
			case strings.HasSuffix(name, joiningExt):
				q.sweepJoining(name)
			case strings.HasSuffix(name, ticketExt):
				more = true
				if beforeInspect != nil {
					beforeInspect(name)
				}
				tk, kind, err := q.inspect(name, buf)
				if err != nil {
					return s, false
				}
				switch kind {
				case ticketLive:
					live = append(live, liveTicket{name: name, tk: tk})
				case ticketStale:
					s.Stale = append(s.Stale, name)
				case ticketUnreadable:
					s.Other = append(s.Other, name)
				}
			}
		}
		if !more {
			break
		}
		if pass == scanPasses {
			return s, false
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].tk.Priority != live[j].tk.Priority {
			return live[i].tk.Priority > live[j].tk.Priority
		}
		if live[i].tk.Enqueued != live[j].tk.Enqueued {
			return live[i].tk.Enqueued < live[j].tk.Enqueued
		}
		return live[i].name < live[j].name
	})
	s.Live = make([]snapEntry, len(live))
	for i, x := range live {
		s.Live[i] = snapEntry{Name: x.name, Exclusive: x.tk.Exclusive}
	}
	s.Done = time.Now().UnixNano()
	return s, true
}

type ticketKind int

const (
	ticketGone       ticketKind = iota // left, or its process died (swept)
	ticketLive                         // in the queue
	ticketStale                        // held, but its holder is stopped
	ticketUnreadable                   // held, but not a ticket we can read
)

// inspect looks at one ticket. An error means it couldn't tell.
func (q *Queue) inspect(name string, buf []byte) (ticketData, ticketKind, error) {
	var tk ticketData
	path := filepath.Join(q.dir, name)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return tk, ticketGone, nil
	}
	if err != nil {
		return tk, ticketGone, err
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		_ = os.Remove(path) // nobody holds it: the process died
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return tk, ticketGone, nil
	}
	fi, err := f.Stat()
	if err != nil {
		return tk, ticketGone, err
	}
	if time.Since(fi.ModTime()) > staleAfter {
		return tk, ticketStale, nil // it counts again when it resumes
	}
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return tk, ticketGone, err
	}
	if json.Unmarshal(buf[:n], &tk) != nil {
		return tk, ticketUnreadable, nil
	}
	return tk, ticketLive, nil
}

// sweepJoining removes a ticket abandoned while being written. Its writer
// locks it right after creating it; one left unlocked this long belongs to
// a process that died mid-Join.
func (q *Queue) sweepJoining(name string) {
	path := filepath.Join(q.dir, name)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || time.Since(fi.ModTime()) <= staleAfter {
		return
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		_ = os.Remove(path)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
}
