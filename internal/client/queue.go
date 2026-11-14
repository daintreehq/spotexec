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

// lock takes queue.lock without blocking forever: a process stopped while
// holding it must not wedge every other agent.
func (q *Queue) lock() (func(), error) {
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(q.dir, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("queue lock busy (another sx process stopped while holding it?)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Touch refreshes the ticket's heartbeat.
func (t *Ticket) Touch() {
	if t != nil && t.f != nil {
		now := time.Now()
		_ = os.Chtimes(t.path, now, now)
	}
}

// Join enqueues this process. Creation happens under queue.lock so the
// sweeper (also under the lock) never removes a ticket before it is locked.
func (q *Queue) Join(priority int, exclusive bool, command string) (*Ticket, error) {
	unlock, err := q.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	var b [4]byte
	_, _ = rand.Read(b[:])
	path := filepath.Join(q.dir, fmt.Sprintf("%d-%s.ticket", os.Getpid(), hex.EncodeToString(b[:])))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	if len(command) > 120 {
		command = command[:119] + "…"
	}
	t := &Ticket{f: f, path: path, tk: ticketData{Priority: priority, Enqueued: time.Now().UnixNano(), Exclusive: exclusive, Command: command, PID: os.Getpid()}}
	data, _ := json.Marshal(t.tk)
	_, _ = f.Write(data)
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

// Waiting reports whether anyone is queued (newcomers must not jump them).
func (q *Queue) Waiting() int {
	unlock, err := q.lock()
	if err != nil {
		return 0
	}
	defer unlock()
	return len(q.live())
}

// State returns the ticket's position among live tickets (and refreshes
// the ticket's heartbeat).
func (q *Queue) State(t *Ticket) QueueState {
	t.Touch()
	unlock, err := q.lock()
	if err != nil {
		return QueueState{Position: 1 << 20} // can't tell: don't probe this round
	}
	defer unlock()
	all := q.live()
	st := QueueState{Total: len(all)}
	for i, x := range all {
		if x.path == t.path {
			st.Position = i
			return st
		}
		if x.tk.Exclusive {
			st.ExclusiveHead = true
		}
	}
	st.Position = 1 << 20 // our ticket is unreadable: never admit on a guess
	return st
}

type liveTicket struct {
	path string
	tk   ticketData
}

// live lists tickets held by running processes, in admission order, and
// sweeps dead ones. Must be called with queue.lock held.
func (q *Queue) live() []liveTicket {
	ents, _ := os.ReadDir(q.dir)
	var out []liveTicket
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".ticket") {
			continue
		}
		path := filepath.Join(q.dir, e.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = os.Remove(path) // nobody holds it: the process died
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			continue
		}
		fi, ferr := f.Stat()
		b := make([]byte, 4096)
		n, _ := f.ReadAt(b, 0)
		f.Close()
		var tk ticketData
		if json.Unmarshal(b[:n], &tk) != nil {
			continue // still being written
		}
		if ferr == nil && time.Since(fi.ModTime()) > staleAfter {
			continue // holder is stopped; it counts again when it resumes
		}
		out = append(out, liveTicket{path: path, tk: tk})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].tk.Priority != out[j].tk.Priority {
			return out[i].tk.Priority > out[j].tk.Priority
		}
		if out[i].tk.Enqueued != out[j].tk.Enqueued {
			return out[i].tk.Enqueued < out[j].tk.Enqueued
		}
		return out[i].path < out[j].path
	})
	return out
}
