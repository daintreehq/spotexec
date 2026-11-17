package client

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// Seeking marks a run looking for a slot, from before it scans its files
// until it holds one, on each fleet of its pool that drains surplus VMs
// early: no new early drain is committed there meanwhile (BeginDrain).
// Waiters say the same, but only once a job has found every slot taken,
// after its scan and first probes: a standby VM drained in that gap is room
// the job may have needed (#257). It weighs nothing: Plan never launches
// for it. One flock'd file per fleet, under the fleet's `seeking/`,
// refreshed every seekBeat until withdrawn, however long the scan takes; a
// run that dies releases its flock, and one that stops refreshing it (a
// stopped process) stops counting after staleAfter.
type Seeking struct {
	mu    sync.Mutex
	files []*os.File
	paths []string
	done  bool
	stop  chan struct{}
	beat  sync.WaitGroup
}

// seekBeat: how often a mark is refreshed, well within staleAfter.
var seekBeat = 5 * time.Second

// drainsEarly reports whether a runner's surplus VMs end early
// (scheduling.early_drain), the only fleets Seeking marks.
func drainsEarly(r *config.Runner) bool {
	return r.EarlyDrain() && r.SchedPolicy() == config.PolicyConsolidate && r.Ephemeral()
}

const seekingDir = "seeking"

// Seek marks this process as seeking a slot on pool's early-draining
// fleets. It is best effort: a fleet whose mark can't be made is left
// unmarked (a drain committed there is still taken back while the VM is
// up). Never nil.
func Seek(pool []*config.Runner) *Seeking {
	s := &Seeking{}
	for _, r := range pool {
		if !drainsEarly(r) {
			continue
		}
		dir := filepath.Join(provider.RunnerStateDir(r.Name), seekingDir)
		if os.MkdirAll(dir, 0o700) != nil {
			continue
		}
		var b [4]byte
		_, _ = rand.Read(b[:])
		path := filepath.Join(dir, fmt.Sprintf("%d-%s.s", os.Getpid(), hex.EncodeToString(b[:])))
		// Locked before it takes its name: a sweep (seeking, below) never
		// sees it unlocked and removes it as a dead process's.
		tmp := path + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil || os.Rename(tmp, path) != nil {
			_ = os.Remove(tmp)
			_ = f.Close()
			continue
		}
		s.files, s.paths = append(s.files, f), append(s.paths, path)
	}
	if len(s.files) > 0 {
		s.stop = make(chan struct{})
		s.beat.Add(1)
		go func() {
			defer s.beat.Done()
			t := time.NewTicker(seekBeat)
			defer t.Stop()
			for {
				select {
				case <-s.stop:
					return
				case <-t.C:
					s.Touch()
				}
			}
		}()
	}
	return s
}

// withdrawn reports whether Done has been called (a request copied from
// one that already acquired carries a spent mark).
func (s *Seeking) withdrawn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Touch refreshes the marks (a stopped process stops counting).
func (s *Seeking) Touch() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, path := range s.paths {
		_ = os.Chtimes(path, now, now)
	}
}

// Done withdraws the marks; later calls do nothing.
func (s *Seeking) Done() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.beat.Wait() // its last refresh can't touch a removed path's successor
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.files {
		_ = os.Remove(s.paths[i])
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	s.files, s.paths = nil, nil
}

// seeking reports whether a live process is seeking a slot on this fleet,
// sweeping the marks dead processes left. Called under pool.lock.
func (p *Pool) seeking() bool {
	dir := filepath.Join(p.dir, seekingDir)
	ents, _ := os.ReadDir(dir)
	live := false
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		tmp := strings.HasSuffix(e.Name(), ".tmp")
		if tmp {
			// Being made (locked a moment from now), or left by a process
			// that died making it: only an old, unlocked one is swept, so
			// a run stopped while making its mark still gets it.
			if fi, err := e.Info(); err != nil || time.Since(fi.ModTime()) <= staleAfter {
				continue
			}
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = os.Remove(path) // nobody holds it: the process died
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			continue
		}
		fi, err := f.Stat()
		_ = f.Close()
		if !tmp && err == nil && time.Since(fi.ModTime()) <= staleAfter {
			live = true
		}
	}
	return live
}
