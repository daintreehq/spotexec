package helper

import (
	"path/filepath"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// A VM has one slot count at a time, whatever max_slots each checkout
// sends: checkouts of one project disagree whenever a commit changes it and
// older worktrees haven't picked that up, and jobs sized by different counts
// would be pinned to overlapping CPUs while others sit free. The first run
// on an idle VM (no session holds the gate) sets the count; every run that
// takes capacity while any session holds it uses that count, and the next
// run on an idle VM sets it afresh. Deciding the count and taking capacity
// under it happen under one VM-wide mutex, so two runs arriving at an idle
// VM together can't each pick their own.

// SlotsLock is the mutex held while a run decides the slot count and takes
// its capacity under it. Never unlinked, like every lock file.
func (l Layout) SlotsLock() string { return filepath.Join(l.Root, "tokens", "slots.lock") }

// SlotsFile records the slot count the runs holding capacity agreed on.
func (l Layout) SlotsFile() string { return filepath.Join(l.Root, "tokens", "slots.json") }

type slotRecord struct {
	SlotCount int `json:"slot_count"`
}

// readSlotCount is the recorded slot count, or 0 when there is none (a VM
// only older helpers have used) or it is unreadable.
func readSlotCount(l Layout) int {
	var r slotRecord
	if readJSON(l.SlotsFile(), &r) != nil || r.SlotCount < 1 || r.SlotCount > protocol.MaxSlots {
		return 0
	}
	return r.SlotCount
}

func writeSlotCount(l Layout, n int) error {
	return writeJSON(l.SlotsFile(), slotRecord{SlotCount: n})
}

// slotsLockWait bounds the wait for the slot-count mutex, which is only
// ever held for a few lock attempts.
var slotsLockWait = 5 * time.Second

// lockSlots takes the slot-count mutex; ok is false if it stayed busy.
func lockSlots(l Layout) (_ *flock, ok bool, err error) {
	deadline := time.Now().Add(slotsLockWait)
	for {
		mu, ok, err := tryLock(l.SlotsLock())
		if err != nil || ok {
			return mu, ok, err
		}
		if time.Now().After(deadline) {
			return nil, false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// agreedSlots is the slot count a run asking for own takes capacity under,
// with the slot-count mutex held: own on an idle VM, else the recorded
// count (own if there is none: only older helpers hold capacity).
func agreedSlots(l Layout, own int) int {
	rec := readSlotCount(l)
	if rec == 0 || rec == own {
		return own
	}
	// The gate is held (shared) by every session holding capacity. Probing
	// it exclusively can't make another acquirer miss it: new helpers take
	// it only under the mutex held here.
	g, free, err := tryLock(l.Gate())
	if err != nil || !free {
		return rec
	}
	g.Unlock()
	return own
}
