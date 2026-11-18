package helper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// runPoll is how often a job waiting for a run permit looks again.
var runPoll = 200 * time.Millisecond

// takeRun takes a run permit for the job's [setup] run and command: one of
// max_slots, so no more commands run at once than the runner has slots,
// however many jobs it admitted to prepare. A job holding its slot's
// capacity for its whole lease (an older helper's, on a VM two clients
// share) takes a permit's place, and the permit of its token's number,
// whose CPU block it is pinned to.
//
// It waits for one, holding its token and workspace, until the queue
// timeout or the client cancels, withdraws the job (errWithdrawn: it found
// a free slot on another VM) or goes. canceled: the client canceled while
// it waited (nothing ran). A job admitted before the VM neared its
// max_lifetime still runs, as one that never waited does.
func (s *Session) takeRun() (canceled bool, err error) {
	s.relMu.Lock()
	skip := !s.phased() || s.run != nil
	s.relMu.Unlock()
	if skip {
		return false, nil
	}
	start := time.Now()
	noticed := "" // why it waits ("slots" or "memory"), once it does
	unwait := func() {
		if noticed != "" {
			s.updateHolder(func(h *protocol.Holder) { h.Phase = "" })
		}
	}
	for attempt := 0; ; attempt++ {
		if s.stopping.Load() {
			return false, perr(protocol.CodeShutdown, "the runner is shutting down")
		}
		if attempt > 0 {
			// Before every later attempt: a permit that frees just as the
			// client cancels, or the wait runs out, must not start it.
			if canceled, withdrawn := s.stopQueued(); canceled {
				unwait()
				return true, nil
			} else if withdrawn {
				unwait()
				return false, errWithdrawn
			}
			if q := time.Duration(s.acq.QueueMS) * time.Millisecond; q > 0 && time.Since(start) > q {
				unwait()
				return false, perr(protocol.CodeQueueTimeout, "no free slot to run the command within %s", q)
			}
		}
		got, memWait, err := s.tryRun()
		if err != nil {
			return false, err
		}
		if got {
			if noticed != "" {
				what := "a free slot"
				if noticed == "memory" {
					what = "memory"
				}
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "run_slot", Message: fmt.Sprintf("waited %s for %s", time.Since(start).Round(100*time.Millisecond), what)})
			}
			return false, nil
		}
		if noticed == "" {
			s.updateHolder(func(h *protocol.Holder) { h.Phase = protocol.PhaseRunWait })
		}
		// Told once per reason: memory's figures change on every attempt.
		reason, msg := "slots", fmt.Sprintf("all %d slots are running commands; waiting for one to finish", s.maxSlots)
		if memWait != "" {
			reason, msg = "memory", memWait
		}
		if reason != noticed {
			noticed = reason
			_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "run_wait", Message: msg})
		}
		time.Sleep(runPoll)
	}
}

// errWithdrawn ends a run-permit wait the client withdrew
// (protocol.TypeWithdraw): nothing ran, and the job goes elsewhere.
var errWithdrawn = errors.New("withdrawn while waiting for a run permit")

// tryRun takes a free run permit if there is room for one, under the run
// gate: two jobs counting at once could each see the last one free, or
// count each other's probes as held. memWait says why it didn't when a
// permit is free but the memory pool has no room for the command.
func (s *Session) tryRun() (got bool, memWait string, err error) {
	// Held for an instant: a brief wait at most, so the outer loop's
	// cancel and timeout checks stay current.
	var gate *flock
	for deadline := time.Now().Add(100 * time.Millisecond); gate == nil; {
		g, ok, err := tryLock(s.layout.RunGate())
		if err != nil {
			return false, "", err
		}
		if ok {
			gate = g
		} else if time.Now().After(deadline) {
			return false, "", nil // try again on the next attempt
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	defer gate.Unlock()
	for _, i := range s.runPermits() {
		l, ok, err := tryLock(s.layout.RunLock(i))
		if err != nil {
			return false, "", err
		}
		if ok {
			if s.stopping.Load() { // own would park, holding the gate
				l.Unlock()
				return false, "", perr(protocol.CodeShutdown, "the runner is shutting down")
			}
			if why := s.memoryWait(i); why != "" {
				l.Unlock()
				return false, why, nil
			}
			// Before the gate opens: the next job to look counts this
			// command's reservation.
			_ = writeJSON(s.layout.RunMem(i), s.runMem)
			s.own(func() {
				s.run, s.runIdx = l, i
				s.updateHolder(func(h *protocol.Holder) { h.Phase, h.Run = protocol.PhaseRun, i })
			})
			return true, "", nil
		}
	}
	return false, "", nil
}

// runPermits lists the run permits this job may take: none once permits
// held and jobs holding their slot's capacity for their whole lease
// together reach max_slots, otherwise 1..max_slots less the permits of
// such jobs' tokens (they are pinned to those CPU blocks). Counts are
// approximate while a token is being taken (one without its record counts
// as such a job), which only delays a command. An older helper never
// looks at run permits: one starting a job beside phased commands (a
// client upgrade on a running VM) can still exceed max_slots until they
// end.
func (s *Session) runPermits() []int {
	legacy := map[int]bool{}
	for _, h := range readHolders(s.layout) {
		if !h.Phased {
			legacy[h.Token] = true
		}
	}
	held := 0
	for i := 1; i <= s.maxSlots; i++ {
		if isHeld(s.layout.RunLock(i)) {
			held++
		}
	}
	if held+len(legacy) >= s.maxSlots {
		return nil
	}
	var out []int
	for i := 1; i <= s.maxSlots; i++ {
		if !legacy[i] {
			out = append(out, i)
		}
	}
	return out
}

// releaseRun gives the run permit back once the command (and its
// container) is gone, so a job writing back leaves the slot to another.
func (s *Session) releaseRun() {
	s.relMu.Lock()
	defer s.relMu.Unlock()
	if s.run == nil || s.stopping.Load() || s.exiting.Load() {
		return // none held, or released (or being released) with everything else
	}
	os.Remove(s.layout.RunMem(s.runIdx))
	s.run.Unlock()
	s.run, s.runIdx = nil, 0
	s.updateHolder(func(h *protocol.Holder) { h.Phase, h.Run = "", 0 })
}

// phased reports whether this session takes run permits: every lease but
// an exclusive one, which holds every token.
func (s *Session) phased() bool {
	return !s.acq.Exclusive && len(s.tokens) == 1 && s.maxSlots > 0
}

// updateHolder rewrites the session's holder records with change applied.
func (s *Session) updateHolder(change func(*protocol.Holder)) {
	var h protocol.Holder
	_ = readJSON(filepath.Join(s.slotDir, "holder.json"), &h)
	change(&h)
	s.writeHolder(h)
}
