package helper

import (
	"bytes"
	"errors"
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
	s := &Session{c: protocol.NewConn(strings.NewReader(""), &syncBuf{}), layout: Layout{Root: root}, frames: make(chan protocol.Frame, 1)}
	s.lastSeen.Store(time.Now().UnixNano())
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
