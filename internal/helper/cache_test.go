package helper

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A detached cache upload holds the lifecycle lock from the moment it
// starts: once the run that started it releases its own (a quick run ends
// right after), the idle watchdog still can't take the lock exclusively
// and power the VM off mid-upload. It lets go when the upload ends.
func TestCacheUploadHandsOffTheLifecycleLock(t *testing.T) {
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
	tr, err := startCacheUploadWith(lifecycle, root, src, keyLock, "file://"+filepath.Join(root, "shared", "k.tar"))
	if err != nil {
		t.Fatal(err)
	}
	session.Unlock()
	if l, ok, _ := tryLock(lifecycle); ok {
		l.Unlock()
		t.Fatal("the watchdog could take the lifecycle lock while the upload runs")
	}
	key.Unlock()
	select {
	case <-tr.done:
	case <-time.After(30 * time.Second):
		t.Fatal("upload never ended")
	}
	if tr.err != nil {
		// The archive step needs a GNU-style tar when zstd is installed;
		// what matters here is the lock.
		log, _ := os.ReadFile(filepath.Join(root, "cache-upload.log"))
		t.Logf("upload: %v: %s", tr.err, log)
	}
	l, ok, err := tryLock(lifecycle)
	if err != nil || !ok {
		t.Fatalf("the ended upload still holds the lifecycle lock: %v", err)
	}
	l.Unlock()
}
