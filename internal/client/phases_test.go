package client

import (
	"testing"
	"time"
)

func TestProgressTimesEachPhase(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	at := func(s int) { now = time.Date(2026, 11, 16, 10, 0, s, 0, time.UTC) }
	p := &progress{at: stage{phase: phaseSlot}, spent: map[string]time.Duration{}, now: func() time.Time { return now }}
	p.since = now
	at(1)
	p.set(phaseSync) // 1s setting up the slot: not counted
	at(4)
	p.set(phaseGitSeed)
	at(5)
	p.set(phaseImage)
	at(15)
	p.set(phaseDepsCheck)
	at(16)
	p.set(phaseDeps)
	at(25)
	p.set(phaseDepsFlush) // #253: writing them to disk is dependency time
	at(30)
	p.dispatch(true)
	at(31)
	p.started()
	at(33)
	p.setupDone()
	at(34)
	p.started()
	at(44)
	if s := p.get(); s.phase != phaseCommand || !s.started || !s.dispatched {
		t.Fatalf("stage %+v", s)
	}
	p.set(phaseWriteback)
	at(46)
	got := p.timings()
	want := map[string]time.Duration{PhaseSync: 4 * time.Second, PhaseImage: 10 * time.Second, PhaseDeps: 15 * time.Second,
		PhaseSetup: 3 * time.Second, PhaseCommand: 11 * time.Second, PhaseWriteback: 2 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("timings %v, want %v", got, want)
	}
	for k, d := range want {
		if got[k] != d {
			t.Errorf("%s: %s, want %s", k, got[k], d)
		}
	}
	// Reading the timings doesn't change the stage a lost runner is
	// reported in, and counts nothing twice.
	if s := p.get(); s.phase != phaseWriteback {
		t.Errorf("stage after timings: %+v", s)
	}
	if again := p.timings(); again[PhaseWriteback] != 2*time.Second {
		t.Errorf("counted twice: %v", again)
	}
}

func TestEveryAttemptPhaseHasAKind(t *testing.T) {
	for _, ph := range []string{phaseSync, phaseGitSeed, phaseImage, phaseDepsCheck, phaseDeps, phaseDepsFlush, phaseRunWait, phaseStartSetup, phaseSetup, phaseStartCommand, phaseCommand, phaseWriteback} {
		if phaseKind[ph] == "" {
			t.Errorf("%q counts as no kind of time", ph)
		}
	}
}

// Time the helper holds a dispatched run back for a free slot counts as
// queued, and the attempt is still one whose process hasn't started.
func TestProgressRunWaitIsQueued(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	at := func(s int) { now = time.Date(2026, 11, 16, 10, 0, s, 0, time.UTC) }
	p := &progress{at: stage{phase: phaseDeps}, spent: map[string]time.Duration{}, now: func() time.Time { return now }}
	p.since = now
	p.dispatch(true)
	at(1)
	p.runWait()
	if s := p.get(); s.phase != phaseRunWait || !s.dispatched || s.started {
		t.Fatalf("waiting: stage %+v", s)
	}
	at(9)
	p.runSlot()
	at(10)
	p.started()
	if s := p.get(); s.phase != phaseSetup || !s.started {
		t.Fatalf("after the wait: stage %+v", s)
	}
	at(12)
	got := p.timings()
	if got[PhaseQueued] != 8*time.Second || got[PhaseSetup] != 4*time.Second {
		t.Fatalf("timings %v", got)
	}

	// Started without run_slot (an event lost or reordered) still lands in
	// the right phase.
	q := &progress{at: stage{phase: phaseDeps}, spent: map[string]time.Duration{}, now: func() time.Time { return now }}
	q.since = now
	q.dispatch(false)
	q.runWait()
	q.started()
	if s := q.get(); s.phase != phaseCommand || !s.started {
		t.Fatalf("started from the wait: stage %+v", s)
	}
}
