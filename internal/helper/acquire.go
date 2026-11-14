package helper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

var repoIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

// autoSlots is the slot count without max_slots: vCPUs / 4, within the
// same bounds as an explicit max_slots.
func autoSlots(cpus int) int {
	return min(max(cpus/4, 1), protocol.MaxSlots)
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
			return &protoError{code: protocol.CodeBooting, msg: "runner is still running its first-boot setup", detail: latestBootLine(bootLog)}
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
	if err := lifetimeGate(a); err != nil {
		return err
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
		max = autoSlots(runtime.NumCPU())
	}
	max = min(max, protocol.MaxSlots)
	// exec, pull and --same-slot need one exact workspace.
	only := a.OnlySlot && a.PreferSlot >= 1
	if only && a.PreferSlot > max {
		return perr(protocol.CodeBadRequest, "workspace slot %d no longer exists: this runner now has %d slots (max_slots)", a.PreferSlot, max)
	}
	start := time.Now()
	deadline := start.Add(time.Duration(a.QueueMS) * time.Millisecond)
	lastNotice := time.Time{}
	holder := protocol.Holder{RepoID: a.RepoID, Client: a.Client, StartedAt: time.Now().UnixMilli(), HelperPID: os.Getpid()}
	waiter := &slotWaiter{}
	defer waiter.done()

	// Tokens bound host-wide concurrency; their index drives SPOTEXEC_SLOT
	// and port offsets so different repos never collide on ports either.
	// A run that needs one exact workspace waits for it holding nothing,
	// then takes a token and the workspace together: waiting must not use
	// up capacity other runs could have (or make the runner look full).
capacity:
	for {
		if only {
			busy, err := s.waitForSlot(a, waiter, holder, deadline, &lastNotice)
			if err != nil || busy {
				return err
			}
		}
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
				if ok, err := s.takeOnlySlot(a, only, &holder); err != nil || ok {
					if err != nil {
						return err
					}
					break capacity
				}
				continue
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
				if ok, err := s.takeOnlySlot(a, only, &holder); err != nil || ok {
					if err != nil {
						return err
					}
					break capacity
				}
				continue
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
		if err := lifetimeGate(a); err != nil {
			return err
		}
		if s.clientGone() {
			return perr(protocol.CodeInternal, "client went away while queued")
		}
		if only {
			w := holder
			w.Slot = a.PreferSlot
			waiter.register(s.layout, w)
		}
		if time.Since(lastNotice) > 2*time.Second {
			lastNotice = time.Now()
			if err := s.c.Send(protocol.TypeWaiting, protocol.Waiting{Holders: readHolders(s.layout)}); err != nil {
				return err
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
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
			// Otherwise only possible if max_slots differs between clients.
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
	// A slot is only handed over once nothing from an earlier run is still
	// running in it and its files are the runner user's again. A helper
	// that died hard (OOM, SIGKILL) took only its direct child with it:
	// reap the rest of its process group...
	var prev protocol.Holder
	if readJSON(filepath.Join(s.slotDir, "holder.json"), &prev) == nil && prev.ChildPGID > 0 {
		if groupAlive(prev.ChildPGID) && groupInDir(prev.ChildPGID, s.workspace) {
			killGroup(prev.ChildPGID, 2*time.Second)
		}
	}
	// ...whatever escaped the group (setsid, double fork, a subshell that
	// left the directory) but still runs in, or was started from, the
	// workspace, docker clients included, so none starts a container
	// after the sweep below...
	if left := reapWorkspace(s.workspace, 2*time.Second); len(left) > 0 {
		return perr(protocol.CodeInternal, "workspace slot %d: processes from an earlier run (pids %v) are still running in it and can't be stopped", holder.Slot, left)
	}
	// ...and its containers.
	if _, err := exec.LookPath("docker"); err == nil {
		s.repoID = a.RepoID
		if left := s.removeContainers(true); left != "" {
			return perr(protocol.CodeInternal, "workspace slot %d: containers from an earlier run can't be removed: %s", holder.Slot, left)
		}
		// Script/env files of runs whose helper died (they can hold secrets).
		_ = os.RemoveAll(filepath.Join(s.slotDir, "run"))
	}
	if err := s.reclaimFiles(holder.Slot); err != nil {
		return err
	}
	holder.Token = s.tokenIdxAt(0)
	s.writeHolder(holder)
	s.acq = a
	s.state = loadSlotState(s.slotDir)
	if s.state.Sweep {
		s.sweepPending()
	}
	// Files left pending by earlier runs must be cleaned up by a sync.
	match := a.Fingerprint != "" && s.state.Fingerprint == a.Fingerprint && len(s.state.Pending) == 0
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
	if err := lifetimeGate(a); err != nil { // the cutoff may have passed while waiting
		return err
	}
	root := s.layout.Root
	s.owned.Store(&root)
	return s.c.Send(protocol.TypeAcquired, protocol.Acquired{
		Token: holder.Token, Slot: holder.Slot, Workspace: s.workspace,
		FingerprintMatch: match, WaitedMS: time.Since(start).Milliseconds(), MaxSlots: max, PrevWorktreeKey: prevKey,
		Pending: s.state.Pending,
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

// waitForSlot waits, holding nothing, until the exact workspace slot an
// exec, pull or --same-slot run needs is free. busy: it was taken and the
// request doesn't wait (Busy was sent).
func (s *Session) waitForSlot(a protocol.Acquire, w *slotWaiter, h protocol.Holder, deadline time.Time, lastNotice *time.Time) (busy bool, err error) {
	lock := filepath.Join(s.layout.Slot(a.RepoID, a.PreferSlot), ".lock")
	if a.QueueMS <= 0 {
		deadline = time.Now().Add(24 * time.Hour)
	}
	for isHeld(lock) {
		if !a.Wait {
			return true, s.c.Send(protocol.TypeBusy, protocol.Busy{Holders: readHolders(s.layout)})
		}
		if time.Now().After(deadline) {
			return false, perr(protocol.CodeQueueTimeout, "workspace slot %d stayed busy for %s", a.PreferSlot, time.Since(time.UnixMilli(h.StartedAt)).Round(time.Second))
		}
		if draining(s.layout.Root) {
			return false, perr(protocol.CodeDraining, "runner is draining")
		}
		if err := lifetimeGate(a); err != nil {
			return false, err
		}
		if s.clientGone() {
			return false, perr(protocol.CodeInternal, "client went away while queued")
		}
		h.Slot = a.PreferSlot
		w.register(s.layout, h)
		if time.Since(*lastNotice) > 2*time.Second {
			*lastNotice = time.Now()
			if err := s.c.Send(protocol.TypeWaiting, protocol.Waiting{Holders: readHolders(s.layout)}); err != nil {
				return false, err
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false, nil
}

// takeOnlySlot runs with capacity (token and gate) just taken. For a run
// that needs one exact workspace it takes that slot too; if another run got
// there first, the capacity is given back and ok is false (wait again).
// Other runs pick their slot afterwards.
func (s *Session) takeOnlySlot(a protocol.Acquire, only bool, h *protocol.Holder) (bool, error) {
	if !only {
		return true, nil
	}
	dir := s.layout.Slot(a.RepoID, a.PreferSlot)
	l, ok, err := tryLock(filepath.Join(dir, ".lock"))
	if err != nil {
		return false, err
	}
	if ok {
		s.slotLock, s.slotDir = l, dir
		h.Slot = a.PreferSlot
		return true, nil
	}
	s.releaseCapacity()
	return false, nil
}

// releaseCapacity gives back the tokens and gate taken so far.
func (s *Session) releaseCapacity() {
	if s.stopping.Load() {
		select {} // Shutdown is releasing everything and will exit
	}
	s.relMu.Lock()
	defer s.relMu.Unlock()
	for _, t := range s.tokens {
		t.Unlock()
	}
	s.tokens = nil
	s.gate.Unlock()
	s.gate = nil
}

// slotWaiter records a run waiting for its exact workspace slot (which
// holds no token while it waits), so status can show it: a JSON file under
// waiters/, locked for as long as the run waits.
type slotWaiter struct {
	l    *flock
	path string
}

func (w *slotWaiter) register(l Layout, h protocol.Holder) {
	if w.l != nil {
		return
	}
	dir := filepath.Join(l.Root, "waiters")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	if writeJSON(path, h) != nil {
		return
	}
	lk, ok, err := tryLock(path)
	if err != nil || !ok {
		os.Remove(path)
		return
	}
	w.l, w.path = lk, path
}

func (w *slotWaiter) done() {
	if w.l != nil {
		os.Remove(w.path)
		w.l.Unlock()
		w.l = nil
	}
}

// readWaiters lists runs currently waiting for a specific workspace slot.
func readWaiters(l Layout) []protocol.Holder {
	var out []protocol.Holder
	ents, _ := os.ReadDir(filepath.Join(l.Root, "waiters"))
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(l.Root, "waiters", e.Name())
		var h protocol.Holder
		if heldFile(path) && readJSON(path, &h) == nil {
			out = append(out, h)
		}
	}
	return out
}

// heldFile reports whether someone holds a lock on an existing file
// (without creating it, unlike isHeld).
func heldFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// lifetimeGate refuses a run on a VM near its max_lifetime: the hard
// poweroff would cut it short. Copying files back (pull) is still allowed.
func lifetimeGate(a protocol.Acquire) error {
	if left, near := nearMaxLifetime(); near && !a.ReadOnly {
		return perr(protocol.CodeDraining, "runner is %s from its max_lifetime, so it takes no new runs", left.Round(time.Second))
	}
	return nil
}

// nearMaxLifetime reports whether this ephemeral VM is within
// protocol.LifetimeDrainAhead of its max_lifetime (cloud-init records both
// its birth and its lifetime), and how long it has left.
func nearMaxLifetime() (time.Duration, bool) {
	return lifetimeLeft(LifecycleDir, time.Now())
}

func lifetimeLeft(dir string, now time.Time) (time.Duration, bool) {
	read := func(name string) (int64, bool) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return n, err == nil && n > 0
	}
	created, ok1 := read("created_at")
	secs, ok2 := read("max_lifetime")
	if !ok1 || !ok2 {
		return 0, false // a static runner, or a VM from an older spotexec
	}
	lt := time.Duration(secs) * time.Second
	left := time.Unix(created, 0).Add(lt).Sub(now)
	return left, left < protocol.LifetimeDrainAhead(lt)
}

// bootLog is the VM's first-boot log (cloud-init's step markers and the
// bootstrap scripts' output).
var bootLog = "/var/log/spotexec-bootstrap.log"

// latestBootLine is the last non-empty line of the boot log so far ("" if
// none), so a client waiting on a cold boot can show what it is doing.
// Progress bars rewrite their line with \r: only the latest rewrite counts.
func latestBootLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(0, fi.Size()-4096)
	buf := make([]byte, fi.Size()-off)
	n, _ := f.ReadAt(buf, off)
	lines := strings.FieldsFunc(string(buf[:n]), func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				l = l[:300]
			}
			return l
		}
	}
	return ""
}
