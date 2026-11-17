package helper

import (
	"os"
	"path/filepath"
	"strings"
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

// A scheduled drain hands the wait to the watchdog: it records the least
// idle time once (whatever sessions hold the VM meanwhile), only on a VM
// whose watchdog reads it, and a retry of the operation reads as done.
func TestScheduleDrain(t *testing.T) {
	root := t.TempDir()
	if r := scheduleDrain(root, "op1", 2*time.Minute, false); r.Result != DrainUnsupported {
		t.Fatalf("static runner: %+v", r)
	}
	if r := scheduleDrain(root, "op1", 2*time.Minute, true); r.Result != DrainUnsupported {
		t.Fatalf("a watchdog from before scheduled drains: %+v", r)
	}
	// An unreadable features file proves nothing: asked again later.
	if err := os.Mkdir(filepath.Join(root, "features"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := scheduleDrain(root, "op1", 2*time.Minute, true); r.Result != DrainBusy {
		t.Fatalf("features unreadable: %+v", r)
	}
	os.Remove(filepath.Join(root, "features"))
	if err := os.WriteFile(filepath.Join(root, "features"), []byte("drain_after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := scheduleDrain(root, "op1", 2*time.Minute, false); r.Result != DrainUnsupported {
		t.Fatalf("static runner with a features file: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "drain_after")); err == nil {
		t.Fatal("a static runner got a drain scheduled")
	}
	if r := scheduleDrain(root, "op1", 90*time.Second+time.Millisecond, true); r.Result != DrainScheduled || r.Op != "op1" {
		t.Fatalf("schedule: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "drain_after")); string(b) != "91\n" {
		t.Fatalf("drain_after %q, want whole seconds rounded up", b)
	}
	if draining(root) {
		t.Fatal("scheduling drained the VM at once")
	}
	// A retry (its answer lost), or another operation: the first record
	// stands.
	if r := scheduleDrain(root, "op2", time.Second, true); r.Result != DrainScheduled {
		t.Fatalf("again: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "drain_after")); string(b) != "91\n" {
		t.Fatalf("drain_after rewritten: %q", b)
	}
	// Scheduled stays scheduled, even if the features file can't be read
	// on a retry: unsupported would put the VM back into service.
	os.Remove(filepath.Join(root, "features"))
	if r := scheduleDrain(root, "op1", time.Second, true); r.Result != DrainScheduled {
		t.Fatalf("retry without features: %+v", r)
	}
	if r := scheduleDrain(root, "op1", time.Second, false); r.Result != DrainScheduled {
		t.Fatalf("retry with the lifecycle check failing: %+v", r)
	}
	// Once the watchdog has drained it, the operation reads as done.
	if err := os.WriteFile(filepath.Join(root, "draining"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := scheduleDrain(root, "op1", time.Second, true); r.Result != DrainDone {
		t.Fatalf("after draining, same op: %+v", r)
	}
	if r := scheduleDrain(root, "op9", time.Second, true); r.Result != DrainAlready {
		t.Fatalf("after draining, another op: %+v", r)
	}
}

// #255: renewed demand takes back a VM whose drain is scheduled. The
// cancel withdraws drain_after, keeps the VM's activity fresh, and records
// the operation, so a request for it that arrives late (schedule-drain or
// try-drain) drains nothing; another operation may still drain the VM.
func TestCancelDrain(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, "lifecycle.lock")
	activity := filepath.Join(root, "activity-file")
	for _, p := range []string{lock, activity} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "features"), []byte("drain_after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := scheduleDrain(root, "op1", 2*time.Minute, true); r.Result != DrainScheduled {
		t.Fatalf("schedule: %+v", r)
	}
	old := time.Now().Add(-10 * time.Minute)
	act := filepath.Join(root, "activity") // what touchActivity refreshes off a VM
	if err := os.WriteFile(act, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(act, old, old); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "a b", "../x", strings.Repeat("a", 65)} {
		if err := CancelDrain(root, bad); err == nil {
			t.Fatalf("cancelled with operation %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "drain_after")); err != nil {
		t.Fatal("an invalid cancel withdrew the drain")
	}
	for range 2 { // a retry is harmless
		if err := CancelDrain(root, "op1"); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"drain_after", "drain_op"} {
		if _, err := os.Stat(filepath.Join(root, f)); !os.IsNotExist(err) {
			t.Fatalf("%s left after the cancel: %v", f, err)
		}
	}
	if fi, err := os.Stat(act); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("activity not refreshed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "drain_cancelled")); string(b) != "op1\n" {
		t.Fatalf("cancelled list %q", b)
	}
	// The cancelled operation's late requests drain nothing.
	if r := scheduleDrain(root, "op1", 2*time.Minute, true); r.Result != DrainCancelled {
		t.Fatalf("late schedule-drain: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "drain_after")); !os.IsNotExist(err) {
		t.Fatal("a late schedule-drain rescheduled a cancelled drain")
	}
	if err := os.Chtimes(activity, old, old); err != nil {
		t.Fatal(err)
	}
	if r := tryDrain(root, "op1", "test", time.Minute, lock, activity, true); r.Result != DrainCancelled || draining(root) {
		t.Fatalf("late try-drain: %+v (draining %v)", r, draining(root))
	}
	// A later drain of the VM is a new operation, and a retried cancel of
	// the old one leaves it standing.
	if r := scheduleDrain(root, "op2", 2*time.Minute, true); r.Result != DrainScheduled {
		t.Fatalf("a new drain after the cancel: %+v", r)
	}
	if err := CancelDrain(root, "op1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "drain_after")); err != nil {
		t.Fatal("a retried cancel withdrew a newer drain")
	}
	// A stuck holder of the drain-state lock fails the cancel, in time.
	l, ok, err := tryLock(drainStateLock(root))
	if err != nil || !ok {
		t.Fatalf("drain lock: %v %v", ok, err)
	}
	start := time.Now()
	if err := CancelDrain(root, "op2"); err == nil || time.Since(start) > drainStateWait+2*time.Second {
		t.Fatalf("cancel past a stuck lock: %v after %s", err, time.Since(start))
	}
	l.Unlock()
	if _, err := os.Stat(filepath.Join(root, "drain_after")); err != nil {
		t.Fatal("a failed cancel withdrew the drain")
	}
}
