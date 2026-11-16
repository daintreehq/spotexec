package helper

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A detached cache upload holds the lifecycle lock from the moment it
// starts: once the run that started it releases its own (a quick run ends
// right after), the idle watchdog still can't take the lock exclusively
// and power the VM off mid-upload. It lets go when the upload ends.
func TestCacheUploadHandsOffTheLifecycleLock(t *testing.T) {
	// gzip, not zstd: macOS's tar can't run "zstd -T0 ..." as a compressor.
	bin := t.TempDir()
	for _, tool := range []string{"tar", "gzip", "cp"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	lifecycle := filepath.Join(root, "lifecycle.lock")
	if err := os.WriteFile(lifecycle, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	session, err := lockShared(lifecycle, time.Second) // the run's own
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "deps")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "installed"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The run still holds its dependency key exclusively: the upload waits
	// for it, so it is still running below.
	keyLock := filepath.Join(root, "deps.lock")
	key, ok, err := tryLock(keyLock)
	if err != nil || !ok {
		t.Fatalf("key lock: %v %v", ok, err)
	}
	shared := filepath.Join(root, "shared", "k.tar")
	tr, err := startCacheUploadWith(lifecycle, root, src, keyLock, "file://"+shared)
	if err != nil {
		t.Fatal(err)
	}
	session.Unlock()
	// At once (the upload may not have run a line yet), and once it is
	// waiting for the key, past taking the inherited lock as its own.
	for _, wait := range []time.Duration{0, 500 * time.Millisecond} {
		time.Sleep(wait)
		if l, ok, _ := tryLock(lifecycle); ok {
			l.Unlock()
			t.Fatalf("the watchdog could take the lifecycle lock %v into the upload", wait)
		}
	}
	key.Unlock()
	select {
	case <-tr.done:
	case <-time.After(30 * time.Second):
		t.Fatal("upload never ended")
	}
	if tr.err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "cache-upload.log"))
		t.Fatalf("upload: %v: %s", tr.err, log)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("nothing uploaded: %v", err)
	}
	l, ok, err := tryLock(lifecycle)
	if err != nil || !ok {
		t.Fatalf("the ended upload still holds the lifecycle lock: %v", err)
	}
	l.Unlock()
}
