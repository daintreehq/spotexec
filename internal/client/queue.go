package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	var b [4]byte
	_, _ = rand.Read(b[:])
	base := filepath.Join(q.dir, fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b[:])))
	tmp, path := base+joiningExt, base+ticketExt
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Ticket, error) {
		_ = os.Remove(tmp)
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fail(err)
	}
	if len(command) > 120 {
		command = command[:119] + "…"
	}
	t := &Ticket{f: f, path: path, tk: ticketData{Priority: priority, Enqueued: time.Now().UnixNano(), Exclusive: exclusive, Command: command, PID: os.Getpid()}}
	data, _ := json.Marshal(t.tk)
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	// A link, not a rename: it never replaces another ticket of that name.
	if err := os.Link(tmp, path); err != nil {
		return fail(err)
	}
	_ = os.Remove(tmp)
	return t, nil
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
	if st, ok := q.cached(name, asked); ok {
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
		if st, ok := q.cached(name, asked); ok {
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
	if st, ok := q.cached(name, asked); ok {
		return st
	}
	snap := q.scan()
	q.publish(snap)
	st, _ := snap.position(name)
	return st
}

// snapshot is one scan of the queue: the live tickets in admission order,
// and every other ticket it saw, so a reader can tell whether the queue
// has changed since.
type snapshot struct {
	Start int64       `json:"start"` // unix nanoseconds, when the scan began
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
// when that is as good as a scan of our own: a scan begun after we asked,
// or a recent one of a queue that hasn't changed since (no ticket joined
// or left, none of the stopped ones resumed).
func (q *Queue) cached(name string, asked time.Time) (QueueState, bool) {
	b, err := os.ReadFile(filepath.Join(q.dir, snapshotName))
	if err != nil {
		return QueueState{}, false
	}
	var s snapshot
	if json.Unmarshal(b, &s) != nil {
		return QueueState{}, false
	}
	start, now := time.Unix(0, s.Start), time.Now()
	if start.After(now) {
		return QueueState{}, false // the clock went back: can't tell its age
	}
	st, ok := s.position(name)
	if !ok {
		return QueueState{}, false // not seen yet (or no longer live): scan
	}
	if !start.Before(asked) {
		return st, true
	}
	if now.Sub(start) > snapshotTTL || !s.unchanged(q.dir) {
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

// scan lists tickets held by running processes, in admission order, and
// sweeps dead ones (and tickets abandoned while being written). Must be
// called with queue.lock held.
func (q *Queue) scan() snapshot {
	s := snapshot{Start: time.Now().UnixNano()}
	ents, _ := os.ReadDir(q.dir)
	var live []liveTicket
	buf := make([]byte, 4096)
	for _, e := range ents {
		name := e.Name()
		ticket, joining := strings.HasSuffix(name, ticketExt), strings.HasSuffix(name, joiningExt)
		if !ticket && !joining {
			continue
		}
		path := filepath.Join(q.dir, name)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		fi, ferr := f.Stat()
		if joining {
			// Its writer locks it right after creating it; one left unlocked
			// this long belongs to a process that died mid-Join.
			if ferr == nil && time.Since(fi.ModTime()) > staleAfter && syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				_ = os.Remove(path)
			}
			f.Close()
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = os.Remove(path) // nobody holds it: the process died
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			continue
		}
		if ferr == nil && time.Since(fi.ModTime()) > staleAfter {
			f.Close()
			s.Stale = append(s.Stale, name) // holder is stopped; it counts again when it resumes
			continue
		}
		n, _ := f.ReadAt(buf, 0)
		f.Close()
		var tk ticketData
		if json.Unmarshal(buf[:n], &tk) != nil {
			s.Other = append(s.Other, name)
			continue
		}
		live = append(live, liveTicket{name: name, tk: tk})
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
	return s
}
