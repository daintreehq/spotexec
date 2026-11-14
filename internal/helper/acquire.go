package helper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

var repoIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

func autoSlots() int {
	n := runtime.NumCPU() / 4
	if n < 1 {
		n = 1
	}
	return n
}

func (s *Session) acquire(a protocol.Acquire) (err error) {
	if s.layout.Root == "" {
		return perr(protocol.CodeBadRequest, "hello required before acquire")
	}
	if len(s.tokens) > 0 {
		// Rejected before the rollback below is armed: the slot already held
		// must stay held.
		return perr(protocol.CodeBadRequest, "already holding a slot")
	}
	// Any failure (or a Busy reply) leaves the session holding nothing, so a
	// still-connected client can retry instead of pinning a token.
	defer func() {
		if err != nil || s.slotLock == nil {
			s.rollbackAcquire()
		}
	}()
	if !repoIDRe.MatchString(a.RepoID) {
		return perr(protocol.CodeBadRequest, "invalid repo id %q", a.RepoID)
	}
	// Ephemeral runners refuse work until first-boot setup has finished.
	// (Checked before draining, so a failed setup keeps reporting why.)
	if _, err := os.Stat(LifecycleDir); err == nil {
		b, err := os.ReadFile(filepath.Join(LifecycleDir, "ready"))
		if err != nil {
			return perr(protocol.CodeBooting, "runner is still running its first-boot setup")
		}
		// A failed bootstrap_script leaves a VM without the toolchain the
		// project needs: refuse work (and retire the VM) rather than run
		// jobs, or build a dependency cache, on a broken machine.
		if st := strings.TrimSpace(string(b)); st != "" && st != "0" {
			if _, err := os.Stat(filepath.Join(LifecycleDir, "idle_timeout")); err == nil {
				markDraining(s.layout.Root, "bootstrap failed") // ephemeral: let it go
			}
			tail, _ := exec.Command("tail", "-n", "15", "/var/log/spotexec-bootstrap.log").Output()
			return perr(protocol.CodeBootFailed, "bootstrap_script failed (exit %s) on this VM; last lines of /var/log/spotexec-bootstrap.log:\n%s", st, strings.TrimSpace(string(tail)))
		}
	}
	if draining(s.layout.Root) {
		return perr(protocol.CodeDraining, "runner is draining (shutting down or spot interruption)")
	}
	// Ephemeral runners: hold the lifecycle lock shared so the idle watchdog
	// (which needs it exclusively) cannot shut the VM down under us.
	if _, err := os.Stat(LifecycleLock); err == nil {
		l, err := lockShared(LifecycleLock, 10*time.Second)
		if err != nil {
			return perr(protocol.CodeDraining, "%v", err)
		}
		s.lifecycle = l
		if draining(s.layout.Root) {
			return perr(protocol.CodeDraining, "runner is draining")
		}
	}
	max := a.MaxSlots
	if max <= 0 {
		max = autoSlots()
	}
	if max > 99 {
		max = 99
	}
	start := time.Now()
	deadline := start.Add(time.Duration(a.QueueMS) * time.Millisecond)
	lastNotice := time.Time{}
	holder := protocol.Holder{RepoID: a.RepoID, Client: a.Client, StartedAt: time.Now().UnixMilli(), HelperPID: os.Getpid()}

	// Tokens bound host-wide concurrency; their index drives SPOTEXEC_SLOT
	// and port offsets so different repos never collide on ports either.
	for {
		how := syscall.LOCK_SH
		if a.Exclusive {
			how = syscall.LOCK_EX
		}
		gate, gotGate, err := tryFlock(s.layout.Gate(), how)
		if err != nil {
			return err
		}
		if gotGate && a.Exclusive {
			ok := s.tryAll(max)
			if !ok { // a status probe may hold one token for an instant
				time.Sleep(25 * time.Millisecond)
				ok = s.tryAll(max)
			}
			if ok {
				s.gate = gate
				break
			}
			gate.Unlock()
		} else if gotGate {
			// Status probes (spotexec status, the pool manager) test free
			// tokens by locking them for an instant: on an all-busy first
			// pass, look again after a moment before reporting busy, or a
			// probe would make an idle VM look full (and scale out).
			for pass := 0; pass < 2 && len(s.tokens) == 0; pass++ {
				if pass > 0 {
					time.Sleep(25 * time.Millisecond)
				}
				for i := 1; i <= max; i++ {
					l, ok, err := tryLock(s.layout.TokenLock(i))
					if err != nil {
						return err
					}
					if ok {
						s.tokens, s.tokenIdx = []*flock{l}, i
						break
					}
				}
			}
			if len(s.tokens) > 0 {
				s.gate = gate
				break
			}
			gate.Unlock()
		}
		if !a.Wait {
			return s.c.Send(protocol.TypeBusy, protocol.Busy{Holders: readHolders(s.layout)})
		}
		if a.QueueMS > 0 && time.Now().After(deadline) {
			return perr(protocol.CodeQueueTimeout, "no free slot within %s", time.Duration(a.QueueMS)*time.Millisecond)
		}
		if draining(s.layout.Root) {
			return perr(protocol.CodeDraining, "runner is draining")
		}
		if s.clientGone() {
			return perr(protocol.CodeInternal, "client went away while queued")
		}
		if time.Since(lastNotice) > 2*time.Second {
			lastNotice = time.Now()
			if err := s.c.Send(protocol.TypeWaiting, protocol.Waiting{Holders: readHolders(s.layout)}); err != nil {
				return err
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	waited := time.Since(start)
	touchActivity(s.layout.Root)

	// Pick a workspace slot. Preference: an explicitly requested slot, then a
	// slot whose content already matches (0ms sync), then the slot this
	// worktree used most recently, then the least recently used slot.
	s.repoID = a.RepoID
	type cand struct {
		n     int
		score int
		used  int64
	}
	var cands []cand
	for i := 1; i <= max; i++ {
		st := loadSlotState(s.layout.Slot(a.RepoID, i))
		c := cand{n: i, used: st.LastUsed}
		switch {
		case i == a.PreferSlot:
			c.score = 4
		case a.Fingerprint != "" && st.Fingerprint == a.Fingerprint:
			c.score = 3
		case st.WorktreeKey != "" && st.WorktreeKey == a.WorktreeKey:
			c.score = 2
		case st.LastUsed == 0:
			c.score = 1 // never used: don't evict another worktree's warm slot
		}
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].score >= 2 {
			return cands[i].used > cands[j].used // most recent first
		}
		return cands[i].used < cands[j].used // LRU first
	})
	order := make([]int, 0, len(cands))
	for _, c := range cands {
		order = append(order, c.n)
	}
	slotDeadline := time.Now().Add(30 * time.Second)
	for s.slotLock == nil {
		for _, n := range order {
			dir := s.layout.Slot(a.RepoID, n)
			l, ok, err := tryLock(filepath.Join(dir, ".lock"))
			if err != nil {
				return err
			}
			if ok {
				s.slotLock, s.slotDir = l, dir
				holder.Slot = n
				break
			}
		}
		if s.slotLock == nil {
			// Only possible if max_slots differs between clients.
			if time.Now().After(slotDeadline) {
				return perr(protocol.CodeQueueTimeout, "no free workspace slot for repo %s", a.RepoID)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	s.workspace = filepath.Join(s.slotDir, "workspace")
	if err := os.MkdirAll(s.workspace, 0o755); err != nil {
		return err
	}
	// Reap any process group left behind by a previous holder that died hard.
	var prev protocol.Holder
	if readJSON(filepath.Join(s.slotDir, "holder.json"), &prev) == nil && prev.ChildPGID > 0 {
		if groupAlive(prev.ChildPGID) && groupInDir(prev.ChildPGID, s.workspace) {
			killGroup(prev.ChildPGID, 2*time.Second)
		}
	}
	// Containers a previous (dead) helper left in this slot must not keep
	// writing into the workspace.
	if _, err := exec.LookPath("docker"); err == nil {
		s.repoID = a.RepoID
		s.removeContainers(true)
		// Script/env files of runs whose helper died (they can hold secrets).
		_ = os.RemoveAll(filepath.Join(s.slotDir, "run"))
	}
	holder.Token = s.tokenIdxAt(0)
	s.writeHolder(holder)
	s.acq = a
	s.state = loadSlotState(s.slotDir)
	match := a.Fingerprint != "" && s.state.Fingerprint == a.Fingerprint
	prevKey := s.state.WorktreeKey
	if !a.KeepOwner {
		s.state.WorktreeKey = a.WorktreeKey
	}
	s.state.LastUsed = time.Now().Unix()
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return err
	}
	if a.SpotWatch && !s.watching {
		s.watching = true
		go s.watchNotices()
	}
	// A drain that began while we were acquiring (the pool manager retiring
	// this VM) wins: don't start work it was promised wouldn't start.
	if draining(s.layout.Root) {
		return perr(protocol.CodeDraining, "runner is draining")
	}
	root := s.layout.Root
	s.owned.Store(&root)
	return s.c.Send(protocol.TypeAcquired, protocol.Acquired{
		Token: holder.Token, Slot: holder.Slot, Workspace: s.workspace,
		FingerprintMatch: match, WaitedMS: waited.Milliseconds(), MaxSlots: max, PrevWorktreeKey: prevKey,
	})
}

func (s *Session) rollbackAcquire() {
	if s.stopping.Load() {
		select {} // Shutdown is releasing everything and will exit
	}
	s.owned.Store(nil)
	s.relMu.Lock() // not concurrently with a shutdown's release
	defer s.relMu.Unlock()
	defer func() { s.workspace = "" }()
	for i, t := range s.tokens {
		if t != nil {
			_ = os.Remove(s.layout.TokenHolder(s.tokenIdxAt(i)))
			t.Unlock()
		}
	}
	s.tokens = nil
	s.gate.Unlock()
	s.gate = nil
	if s.slotLock != nil {
		s.slotLock.Unlock()
		s.slotLock, s.slotDir = nil, ""
	}
	s.lifecycle.Unlock()
	s.lifecycle = nil
}

// tryAll locks every token: 1..max plus any higher-numbered token a client
// with a larger max_slots has created, so exclusive really is host-wide.
func (s *Session) tryAll(max int) bool {
	if n := highestToken(s.layout); n > max {
		max = n
	}
	var got []*flock
	for i := 1; i <= max; i++ {
		l, ok, err := tryLock(s.layout.TokenLock(i))
		if err != nil || !ok {
			for _, g := range got {
				g.Unlock()
			}
			return false
		}
		got = append(got, l)
	}
	s.tokens, s.tokenIdx = got, 1
	return true
}

func (s *Session) writeHolder(h protocol.Holder) {
	_ = writeJSON(filepath.Join(s.slotDir, "holder.json"), h)
	for i := range s.tokens {
		hh := h
		hh.Token = s.tokenIdxAt(i)
		_ = writeJSON(s.layout.TokenHolder(hh.Token), hh)
	}
}

func (s *Session) setChildPGID(pgid int) {
	var h protocol.Holder
	_ = readJSON(filepath.Join(s.slotDir, "holder.json"), &h)
	h.ChildPGID = pgid
	s.writeHolder(h)
}

func highestToken(l Layout) int {
	n := 0
	ents, _ := os.ReadDir(filepath.Join(l.Root, "tokens"))
	for _, e := range ents {
		var i int
		if _, err := fmt.Sscanf(e.Name(), "token-%d.lock", &i); err == nil && i > n {
			n = i
		}
	}
	return n
}
