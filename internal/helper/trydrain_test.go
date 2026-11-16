package helper

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The early drain only ever drains an idle VM, decided under the lifecycle
// lock: busy while a session holds it, protected while its results are
// within the retention time, idempotent for a retried operation.
func TestTryDrain(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, "lifecycle.lock")
	activity := filepath.Join(root, "activity-file")
	for _, p := range []string{lock, activity} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	try := func(op string, minIdle time.Duration) TryDrainResult {
		return tryDrain(root, op, "test", minIdle, lock, activity, true)
	}
	if r := tryDrain(root, "x", "test", 0, lock, activity, false); r.Result != DrainUnsupported {
		t.Fatalf("static runner: %+v", r)
	}

	// A session holds the lifecycle lock shared.
	f, _ := os.Open(lock)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	if r := try("op1", 0); r.Result != DrainBusy || draining(root) {
		t.Fatalf("with a session: %+v (draining %v)", r, draining(root))
	}
	f.Close()

	// A token held (a status probe, or a session caught mid-teardown).
	tok := Layout{Root: root}.TokenLock(1)
	tl, ok, err := tryLock(tok)
	if err != nil || !ok {
		t.Fatalf("token lock: %v %v", ok, err)
	}
	if r := try("op1", 0); r.Result != DrainBusy || draining(root) {
		t.Fatalf("with a token held: %+v", r)
	}
	tl.Unlock()

	// Recently active: its results are kept.
	if r := try("op1", time.Hour); r.Result != DrainProtected || draining(root) {
		t.Fatalf("idle under the retention time: %+v", r)
	}

	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(activity, old, old); err != nil {
		t.Fatal(err)
	}
	if r := try("op1", 5*time.Minute); r.Result != DrainDone || !draining(root) || r.IdleS < 590 {
		t.Fatalf("idle past retention: %+v (draining %v)", r, draining(root))
	}
	if r := try("op1", 5*time.Minute); r.Result != DrainDone {
		t.Fatalf("retry of the same operation: %+v", r)
	}
	if r := try("op2", 5*time.Minute); r.Result != DrainAlready {
		t.Fatalf("another operation on a drained VM: %+v", r)
	}
}
