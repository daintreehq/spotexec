package helper

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// admitted acquires a slot of repo r on a two-slot runner at root, without
// waiting, as an overflow request; nil when the runner answered busy.
func admitted(t *testing.T, root string) *Session { return admit(t, root, true) }

func admit(t *testing.T, root string, overflow bool) *Session {
	t.Helper()
	s := testSession(t, root)
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 2, Overflow: overflow}); err != nil {
		t.Fatal(err)
	}
	if s.slotLock == nil {
		return nil
	}
	return s
}

func holderOf(t *testing.T, root string, token int) protocol.Holder {
	t.Helper()
	for _, h := range GetStatus(root).Holders {
		if h.Token == token {
			return h
		}
	}
	t.Fatalf("no holder of token %d", token)
	return protocol.Holder{}
}

// Jobs that only prepare (sync, image, dependencies) don't fill a runner
// for an overflow request: a two-slot runner admits four, each with its
// own token and workspace, and refuses the fifth. A plain request (an
// older client's, or a client looking for a free slot first) still finds
// it full at two.
func TestPreparingJobsLeaveRoom(t *testing.T) {
	root := t.TempDir()
	tokens, slots := map[int]bool{}, map[string]bool{}
	for i := 0; i < 4; i++ {
		if i == 2 {
			if s := admit(t, root, false); s != nil {
				t.Fatal("a plain request admitted past max_slots jobs")
			}
		}
		s := admitted(t, root)
		if s == nil {
			t.Fatalf("job %d of 4 refused while every job only prepares", i+1)
		}
		tokens[s.tokenIdx], slots[s.slotDir] = true, true
	}
	if len(tokens) != 4 || len(slots) != 4 {
		t.Fatalf("tokens %v, workspaces %v: want four of each", tokens, slots)
	}
	if s := admitted(t, root); s != nil {
		t.Fatalf("fifth job admitted (token %d) past twice max_slots", s.tokenIdx)
	}
	for _, h := range GetStatus(root).Holders {
		if !h.Phased || h.Running() {
			t.Errorf("preparing holder %+v counts as running", h)
		}
	}
}

// Once max_slots jobs run their commands the runner is full again, and a
// job that finished its command (writing back) gives its slot back.
func TestRunningJobsFillRunner(t *testing.T) {
	root := t.TempDir()
	a, b := admitted(t, root), admitted(t, root)
	for _, s := range []*Session{a, b} {
		if canceled, err := s.takeRun(); err != nil || canceled {
			t.Fatalf("run permit: %v %v", canceled, err)
		}
	}
	if a.runIdx == b.runIdx {
		t.Fatalf("both commands hold run permit %d", a.runIdx)
	}
	if h := holderOf(t, root, a.tokenIdx); h.Phase != protocol.PhaseRun || h.Run != a.runIdx {
		t.Fatalf("running holder %+v", h)
	}
	if s := admitted(t, root); s != nil {
		t.Fatal("admitted while every slot runs a command")
	}
	a.releaseRun()
	if h := holderOf(t, root, a.tokenIdx); h.Phase != "" || h.Run != 0 || h.Running() {
		t.Fatalf("holder after its command %+v", h)
	}
	if s := admitted(t, root); s == nil {
		t.Fatal("refused once a slot's command finished")
	}
}

// A prepared job waits for a free run permit, holding its token and
// workspace, and is listed as waiting meanwhile (which keeps new jobs
// out); it runs once a command finishes.
func TestRunWaitsForAFreePermit(t *testing.T) {
	root := t.TempDir()
	a, b, c := admitted(t, root), admitted(t, root), admitted(t, root)
	for _, s := range []*Session{a, b} {
		if _, err := s.takeRun(); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.takeRun()
		done <- err
	}()
	// Before the sessions' own cleanup: end and join the wait on any exit.
	t.Cleanup(func() {
		c.frames <- cancelFrame(t)
		<-done
	})
	eventually(t, "the job to wait", func() bool { return holderOf(t, root, c.tokenIdx).Phase == protocol.PhaseRunWait })
	if s := admitted(t, root); s != nil {
		t.Fatal("admitted while two run and one waits to")
	}
	freed := a.runIdx
	a.releaseRun()
	eventually(t, "the waiting job to get the freed permit", func() bool { return holderOf(t, root, c.tokenIdx).Phase == protocol.PhaseRun })
	c.relMu.Lock()
	got := c.runIdx
	c.relMu.Unlock()
	if got != freed {
		t.Fatalf("waiting job got permit %d (freed %d)", got, freed)
	}
}

// A cancel while waiting for a permit ends the wait with nothing run, and
// a queue timeout ends it with a queue-timeout error; either way the job
// no longer counts as waiting.
func TestRunWaitEnds(t *testing.T) {
	root := t.TempDir()
	a, b, c := admitted(t, root), admitted(t, root), admitted(t, root)
	for _, s := range []*Session{a, b} {
		if _, err := s.takeRun(); err != nil {
			t.Fatal(err)
		}
	}
	// A permit that frees after the cancel, while the job sleeps between
	// attempts, mustn't start it. (A long poll keeps the job asleep while
	// the permit frees.)
	oldPoll := runPoll
	runPoll = 2 * time.Second
	t.Cleanup(func() { runPoll = oldPoll })
	c.frames <- cancelFrame(t)
	type res struct {
		canceled bool
		err      error
	}
	got := make(chan res, 1)
	go func() {
		canceled, err := c.takeRun()
		got <- res{canceled, err}
	}()
	eventually(t, "the job to wait", func() bool { return holderOf(t, root, c.tokenIdx).Phase == protocol.PhaseRunWait })
	a.releaseRun()
	r := <-got
	if r.err != nil || !r.canceled || c.run != nil {
		t.Fatalf("canceled wait: canceled %v, err %v, permit %d", r.canceled, r.err, c.runIdx)
	}
	if h := holderOf(t, root, c.tokenIdx); h.Phase != "" {
		t.Fatalf("holder after a canceled wait %+v", h)
	}
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}

	c.acq.QueueMS = 300
	start := time.Now()
	_, err := c.takeRun()
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeQueueTimeout {
		t.Fatalf("want a queue timeout, got %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 3*time.Second || c.run != nil || holderOf(t, root, c.tokenIdx).Phase != "" {
		t.Fatalf("timed out after %s, or still holding or waiting", d)
	}
}

// A job of an older helper (on a VM clients of two versions share) holds
// its slot's command capacity for its whole lease: it takes a run permit's
// place, and its token's CPU block, so no more than max_slots commands run
// and none shares its CPUs.
func TestLegacyHolderTakesARunPermit(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	old, ok, err := tryLock(l.TokenLock(1))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer old.Unlock()
	if err := writeJSON(l.TokenHolder(1), protocol.Holder{Token: 1, RepoID: "old", HelperPID: 1}); err != nil {
		t.Fatal(err)
	}
	a, b := admitted(t, root), admitted(t, root)
	if a == nil || b == nil {
		t.Fatal("preparing jobs refused beside one older job")
	}
	if _, err := a.takeRun(); err != nil || a.runIdx != 2 {
		t.Fatalf("first command: permit %d, %v; want 2 (the older job is pinned to token 1's CPUs)", a.runIdx, err)
	}
	if s := admitted(t, root); s != nil {
		t.Fatal("admitted with the older job and one command running on two slots")
	}
	b.acq.QueueMS = 300
	if _, err := b.takeRun(); err == nil {
		t.Fatalf("a second command ran beside the older job's (permit %d)", b.runIdx)
	}
}

// exec runs the command under a run permit and gives it back before the
// write-back; a job canceled while it waits never runs its command.
func TestExecHoldsRunPermitOnlyForTheCommand(t *testing.T) {
	root := t.TempDir()
	s := admitted(t, root)
	if err := s.exec(protocol.Exec{Shell: `cat "$ROOT/tokens/token-01.json" > seen`, Env: map[string]string{"ROOT": root}}); err != nil {
		t.Fatal(err)
	}
	var seen protocol.Holder
	if err := readJSON(filepath.Join(s.workspace, "seen"), &seen); err != nil {
		t.Fatalf("the command didn't run: %v", err)
	}
	if seen.Phase != protocol.PhaseRun || seen.Run != 1 {
		t.Fatalf("the command ran as %+v, not under a run permit", seen)
	}
	if s.run != nil || isHeld(s.layout.RunLock(1)) || holderOf(t, root, s.tokenIdx).Running() {
		t.Fatal("the run permit outlived the command")
	}

	a, b := admitted(t, root), admitted(t, root)
	for _, x := range []*Session{a, b} {
		if _, err := x.takeRun(); err != nil {
			t.Fatal(err)
		}
	}
	s.frames <- cancelFrame(t)
	out := &syncBuf{}
	s.c = protocol.NewConn(strings.NewReader(""), out)
	if err := s.exec(protocol.Exec{Shell: "touch late"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.workspace, "late")); err == nil {
		t.Fatal("a command canceled while waiting for a slot ran")
	}
	var res protocol.Result
	in := protocol.NewConn(bytes.NewReader(out.b.Bytes()), io.Discard)
	for {
		f, err := in.ReadFrame()
		if err != nil {
			break
		}
		if env, err := protocol.Decode(f); err == nil && env.Type == protocol.TypeResult {
			_ = env.Unmarshal(&res)
		}
	}
	if !res.Canceled || res.ExitCode != 130 {
		t.Fatalf("result of a run canceled while waiting: %+v", res)
	}
}

// A token held for an instant without a record (another acquire, or a
// status probe testing it) doesn't count as a job: it can't make a runner
// with room look full.
func TestUnrecordedTokenIsNoJob(t *testing.T) {
	root := t.TempDir()
	if admit(t, root, false) == nil {
		t.Fatal("first job refused")
	}
	// Token 3 exists once overflow jobs have used it; a probe tests it too.
	probe, ok, err := tryLock(Layout{Root: root}.TokenLock(3))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer probe.Unlock()
	s := admit(t, root, false)
	if s == nil {
		t.Fatal("refused while one job and a probe held tokens on two slots")
	}
	if s.tokenIdx > 2 {
		t.Fatalf("a plain request took token %d past max_slots", s.tokenIdx)
	}
}

// An older helper's job on a token above max_slots (its checkout has more
// slots) still takes a permit's place.
func TestLegacyHolderAboveMaxSlots(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	old, ok, err := tryLock(l.TokenLock(3))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer old.Unlock()
	if err := writeJSON(l.TokenHolder(3), protocol.Holder{Token: 3, RepoID: "old", HelperPID: 1}); err != nil {
		t.Fatal(err)
	}
	a, b := admitted(t, root), admitted(t, root)
	if a == nil || b == nil {
		t.Fatal("refused")
	}
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}
	b.acq.QueueMS = 300
	if _, err := b.takeRun(); err == nil {
		t.Fatalf("a second command ran beside the older job's on two slots (permit %d)", b.runIdx)
	}
}

// Jobs taking run permits at once can't both see the last one free.
func TestConcurrentRunsKeepTheCap(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	old, ok, err := tryLock(l.TokenLock(3))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer old.Unlock()
	if err := writeJSON(l.TokenHolder(3), protocol.Holder{Token: 3, RepoID: "old", HelperPID: 1}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 20; round++ {
		a, b := admitted(t, root), admitted(t, root)
		if a == nil || b == nil {
			t.Fatal("refused")
		}
		a.acq.QueueMS, b.acq.QueueMS = 100, 100
		errs := make(chan error, 2)
		for _, s := range []*Session{a, b} {
			go func() { _, err := s.takeRun(); errs <- err }()
		}
		ran := 0
		for i := 0; i < 2; i++ {
			if <-errs == nil {
				ran++
			}
		}
		if ran != 1 {
			t.Fatalf("round %d: %d commands ran beside the older job's on two slots", round, ran)
		}
		a.release()
		b.release()
	}
}

// A plain request keeps to workspaces 1..max_slots (warm ones, and the
// slot another worktree last used is reused rather than a fresh one);
// overflow jobs use the workspaces past them.
func TestPlainJobsKeepToTheirWorkspaces(t *testing.T) {
	root := t.TempDir()
	a := admit(t, root, false)
	a.release()
	b := admit(t, root, false)
	c := admit(t, root, false)
	if b == nil || c == nil {
		t.Fatal("refused")
	}
	for _, s := range []*Session{b, c} {
		if s.slotDir != s.layout.Slot("r", 1) && s.slotDir != s.layout.Slot("r", 2) {
			t.Fatalf("a plain job got workspace %s, past max_slots", s.slotDir)
		}
	}
	d := admitted(t, root)
	if d == nil || (d.slotDir != d.layout.Slot("r", 3) && d.slotDir != d.layout.Slot("r", 4)) {
		t.Fatal("an overflow job didn't get a workspace past max_slots")
	}
}

// A plain job whose workspaces 1..max_slots overflow jobs took after it
// was admitted falls back to a later one rather than failing.
func TestPlainJobFallsBackToALaterWorkspace(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	for i := 1; i <= 2; i++ {
		ws, ok, err := tryLock(filepath.Join(l.Slot("r", i), ".lock"))
		if err != nil || !ok {
			t.Fatal(err)
		}
		defer ws.Unlock()
	}
	start := time.Now()
	s := admit(t, root, false)
	if s == nil || time.Since(start) > 5*time.Second {
		t.Fatal("refused, or waited for a workspace")
	}
	if s.slotDir != l.Slot("r", 3) {
		t.Fatalf("got workspace %s, want slot 3", s.slotDir)
	}
}
