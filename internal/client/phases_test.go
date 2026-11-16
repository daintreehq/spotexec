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
	for _, ph := range []string{phaseSync, phaseGitSeed, phaseImage, phaseDepsCheck, phaseDeps, phaseStartSetup, phaseSetup, phaseStartCommand, phaseCommand, phaseWriteback} {
		if phaseKind[ph] == "" {
			t.Errorf("%q counts as no kind of time", ph)
		}
	}
}
