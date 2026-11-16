package helper

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func testSession(t *testing.T, root string) *Session {
	t.Helper()
	s := &Session{c: protocol.NewConn(strings.NewReader(""), &syncBuf{}), layout: Layout{Root: root}, frames: make(chan protocol.Frame, 1), born: time.Now()}
	s.lastSeen.Store(s.now())
	t.Cleanup(s.release)
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A run waiting for its exact workspace slot (exec, pull, --same-slot)
// holds no execution token while it waits: other runs still get the free
// capacity, status lists it as a waiter rather than a holder, and it gets
// its slot once that frees up.
func TestSameSlotWaiterHoldsNoToken(t *testing.T) {
	root := t.TempDir()
	a := testSession(t, root)
	if err := a.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2, PreferSlot: 1, OnlySlot: true, Wait: true, QueueMS: 5000}); err != nil || a.slotLock == nil {
		t.Fatalf("first run: %v", err)
	}
	b := testSession(t, root)
	errc := make(chan error, 1)
	go func() {
		errc <- b.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2, PreferSlot: 1, OnlySlot: true, Wait: true, QueueMS: 10000,
			Client: protocol.ClientInfo{Command: "same-slot waiter"}})
	}()
	eventually(t, "the waiter to register", func() bool { return len(GetStatus(root).Waiters) == 1 })
	st := GetStatus(root)
	if w := st.Waiters[0]; w.Slot != 1 || w.Client.Command != "same-slot waiter" || w.Token != 0 {
		t.Fatalf("waiter shown as %+v", w)
	}
	if len(st.Holders) != 1 {
		t.Fatalf("holders %+v: the waiter must not count as one", st.Holders)
	}

	c := testSession(t, root)
	if err := c.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2}); err != nil {
		t.Fatal(err)
	}
	if c.slotLock == nil {
		t.Fatal("a free slot was refused (busy) while a same-slot run waited")
	}
	if c.tokenIdx != 2 {
		t.Fatalf("other run got token %d, want 2", c.tokenIdx)
	}

	c.release()
	a.release()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter didn't get its slot once it was free")
	}
	if b.slotLock == nil || b.slotDir != b.layout.Slot("r", 1) || b.tokenIdx != 1 {
		t.Fatalf("waiter got slot dir %q token %d", b.slotDir, b.tokenIdx)
	}
	if st := GetStatus(root); len(st.Waiters) != 0 || len(st.Holders) != 1 {
		t.Fatalf("after the wait: holders %d, waiters %d", len(st.Holders), len(st.Waiters))
	}
}

// A same-slot run whose workspace is free but the machine is taken (an
// exclusive run of another repo) is listed as waiting too.
func TestSameSlotWaiterForCapacityIsListed(t *testing.T) {
	root := t.TempDir()
	x := testSession(t, root)
	if err := x.acquire(protocol.Acquire{RepoID: "other", MaxSlots: 2, Exclusive: true}); err != nil || x.slotLock == nil {
		t.Fatalf("exclusive run: %v", err)
	}
	b := testSession(t, root)
	errc := make(chan error, 1)
	go func() {
		errc <- b.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2, PreferSlot: 1, OnlySlot: true, Wait: true, QueueMS: 10000})
	}()
	eventually(t, "the waiter to be listed", func() bool { return len(GetStatus(root).Waiters) == 1 })
	x.release()
	select {
	case err := <-errc:
		if err != nil || b.slotLock == nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter didn't get its slot")
	}
	if n := len(GetStatus(root).Waiters); n != 0 {
		t.Fatalf("%d waiters listed after acquisition", n)
	}
}

// A slot number above max_slots (max_slots was lowered since) fails at once
// instead of quietly running elsewhere or reporting busy.
func TestSlotBeyondMaxFailsFast(t *testing.T) {
	s := testSession(t, t.TempDir())
	start := time.Now()
	err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2, PreferSlot: 5, OnlySlot: true, Wait: true, QueueMS: 60000})
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeBadRequest || !strings.Contains(pe.msg, "slot 5") {
		t.Fatalf("want a bad-request error naming slot 5, got %v", err)
	}
	if time.Since(start) > time.Second || len(s.tokens) > 0 || s.slotLock != nil {
		t.Fatal("not a fast failure, or something is still held")
	}
}

func TestAutoSlotsKeepPortsValid(t *testing.T) {
	for cpus, want := range map[int]int{1: 1, 4: 1, 16: 4, 176: 44, 192: protocol.MaxSlots, 448: protocol.MaxSlots} {
		if got := autoSlots(cpus); got != want {
			t.Errorf("autoSlots(%d) = %d, want %d", cpus, got, want)
		}
	}
	if port := 20000 + protocol.MaxSlots*1000 + 999; port > 65535 {
		t.Fatalf("slot %d's port range ends at %d", protocol.MaxSlots, port)
	}
}

// Once teardown (a signal, or lastResort) has begun, the dispatch goroutine
// gives nothing up: a release there would free the slot for another run
// while teardown still cleans it.
func TestReleaseWaitsOutTeardown(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.stopping.Store(false) }) // before testSession's release
	s.stopping.Store(true)
	released := make(chan struct{})
	go func() {
		s.release()
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("released under a teardown")
	case <-time.After(200 * time.Millisecond):
	}
	if l, ok, _ := tryLock(filepath.Join(s.slotDir, ".lock")); ok {
		l.Unlock()
		t.Fatal("the slot was given up under a teardown")
	}
	// Teardown's own release goes ahead.
	s.releaseLocks(true)
	if l, ok, _ := tryLock(filepath.Join(s.slotDir, ".lock")); !ok {
		t.Fatal("teardown kept the slot")
	} else {
		l.Unlock()
	}
}

// Checkouts of one project can disagree on max_slots (a commit changed it
// and older worktrees haven't picked it up). The VM still has one slot
// count: the first run on an idle VM sets it, and every run that starts
// while any run holds a token uses it, whatever count it asked for, so CPU
// shares never overlap. The next run on an idle VM sets it afresh.
func TestSlotCountAgreedWhileBusy(t *testing.T) {
	root := t.TempDir()
	a := testSession(t, root)
	if err := a.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2}); err != nil || a.slotLock == nil {
		t.Fatalf("first run: %v", err)
	}
	b := testSession(t, root)
	if err := b.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4}); err != nil || b.slotLock == nil {
		t.Fatalf("second run: %v", err)
	}
	if b.maxSlots != 2 {
		t.Fatalf("a max_slots=4 run on a VM busy at 2 slots used %d", b.maxSlots)
	}
	c := testSession(t, root)
	if err := c.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4}); err != nil {
		t.Fatal(err)
	}
	if c.slotLock != nil || len(c.tokens) > 0 {
		t.Fatalf("a third run got token %d on a VM whose 2 slots are taken", c.tokenIdx)
	}
	if n := GetStatus(root).SlotCount; n != 2 {
		t.Fatalf("status reports %d slots, want 2", n)
	}

	a.release()
	b.release()
	d := testSession(t, root)
	if err := d.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4}); err != nil || d.slotLock == nil {
		t.Fatalf("run on the idle VM: %v", err)
	}
	if d.maxSlots != 4 {
		t.Fatalf("the first run on an idle VM used %d slots, want its own 4", d.maxSlots)
	}
	e := testSession(t, root)
	if err := e.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2}); err != nil || e.slotLock == nil {
		t.Fatalf("run beside it: %v", err)
	}
	if e.maxSlots != 4 || e.tokenIdx != 2 {
		t.Fatalf("a max_slots=2 run on a VM busy at 4 used %d slots, token %d", e.maxSlots, e.tokenIdx)
	}
	if n := GetStatus(root).SlotCount; n != 4 {
		t.Fatalf("status reports %d slots, want 4", n)
	}
}

// Runs arriving at an idle VM together with different max_slots still
// agree: every one that gets capacity uses one count, on distinct tokens
// within it, and no more of them than that count hold capacity.
func TestSlotCountAgreedUnderRace(t *testing.T) {
	root := t.TempDir()
	const runs = 8
	sess := make([]*Session, runs)
	errs := make(chan error, runs)
	for i := range sess {
		sess[i] = testSession(t, root)
	}
	var start sync.WaitGroup
	start.Add(1)
	for i, s := range sess {
		go func() {
			start.Wait()
			errs <- s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2 + 2*(i%2)})
		}()
	}
	start.Done()
	for range runs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	count, tokens := 0, map[int]bool{}
	for _, s := range sess {
		if s.slotLock == nil {
			continue
		}
		if count == 0 {
			count = s.maxSlots
		}
		if s.maxSlots != count || s.tokenIdx < 1 || s.tokenIdx > count || tokens[s.tokenIdx] {
			t.Fatalf("a run used %d slots on token %d; others %d slots, tokens %v", s.maxSlots, s.tokenIdx, count, tokens)
		}
		tokens[s.tokenIdx] = true
	}
	if len(tokens) != count {
		t.Fatalf("%d runs got capacity on a VM agreed at %d slots", len(tokens), count)
	}
}

// An exclusive run takes exactly the VM's slot count in tokens, even where
// an earlier, larger max_slots left higher-numbered token files behind, so
// status never shows more holders than slots.
func TestExclusiveTakesAgreedTokens(t *testing.T) {
	root := t.TempDir()
	for _, n := range []int{4, 2} {
		s := testSession(t, root)
		if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: n, Exclusive: true}); err != nil || s.slotLock == nil {
			t.Fatalf("exclusive run at %d slots: %v", n, err)
		}
		if st := GetStatus(root); len(st.Holders) != n || st.SlotCount != n {
			t.Fatalf("exclusive run at %d slots: %d holders, slot count %d", n, len(st.Holders), st.SlotCount)
		}
		o := testSession(t, root)
		if err := o.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4}); err != nil || o.slotLock != nil {
			t.Fatalf("another run beside an exclusive one: %v, slot %q", err, o.slotDir)
		}
		s.release()
	}
}

// An exact workspace (exec, pull, --same-slot) numbered above the count a
// busy VM's runs agreed on, but within the run's own max_slots, waits for
// the VM to go idle rather than failing; one above both fails at once.
func TestOnlySlotAboveAgreedCountWaits(t *testing.T) {
	root := t.TempDir()
	a := testSession(t, root)
	if err := a.acquire(protocol.Acquire{RepoID: "other", MaxSlots: 2}); err != nil || a.slotLock == nil {
		t.Fatalf("first run: %v", err)
	}
	bad := testSession(t, root)
	err := bad.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4, PreferSlot: 5, OnlySlot: true, Wait: true, QueueMS: 60000})
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeBadRequest || !strings.Contains(pe.msg, "slot 5") {
		t.Fatalf("want a bad-request error naming slot 5, got %v", err)
	}
	b := testSession(t, root)
	errc := make(chan error, 1)
	go func() {
		errc <- b.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4, PreferSlot: 3, OnlySlot: true, Wait: true, QueueMS: 10000})
	}()
	select {
	case err := <-errc:
		t.Fatalf("slot 3 on a VM busy at 2 slots: %v, slot %q", err, b.slotDir)
	case <-time.After(500 * time.Millisecond):
	}
	a.release()
	select {
	case err := <-errc:
		if err != nil || b.slotDir != b.layout.Slot("r", 3) || b.maxSlots != 4 {
			t.Fatalf("slot 3 once the VM was idle: %v, slot %q, %d slots", err, b.slotDir, b.maxSlots)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slot 3 wasn't granted once the VM was idle")
	}
}

// A status probe locking a token for an instant doesn't make an idle VM
// keep its old slot count: only a session holding the gate does.
func TestTokenProbeDoesNotHoldSlotCount(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	probe, ok, err := tryLock(l.TokenLock(1))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer probe.Unlock()
	if err := writeSlotCount(l, 2); err != nil {
		t.Fatal(err)
	}
	s := testSession(t, root)
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 4}); err != nil || s.slotLock == nil {
		t.Fatalf("run: %v", err)
	}
	if s.maxSlots != 4 || readSlotCount(l) != 4 {
		t.Fatalf("run used %d slots, recorded %d; want 4", s.maxSlots, readSlotCount(l))
	}
}
