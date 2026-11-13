package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/protocol"
)

// Lifecycle paths shared with the cloud-init watchdog on ephemeral runners.
const (
	LifecycleDir  = "/var/tmp/spotexec"
	LifecycleLock = LifecycleDir + "/lifecycle.lock"
	ActivityFile  = LifecycleDir + "/activity"
	DrainingFile  = LifecycleDir + "/draining"
)

// flock is an advisory lock held through an open file descriptor. The file is
// never unlinked or replaced while in use, so all lockers share one inode.
type flock struct {
	f    *os.File
	path string
}

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// O_CLOEXEC is set by Go, so spawned commands never inherit the lock.
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

// tryLock attempts a non-blocking exclusive lock.
func tryLock(path string) (*flock, bool, error) { return tryFlock(path, syscall.LOCK_EX) }

// tryFlock attempts a non-blocking lock of the given kind (LOCK_EX/LOCK_SH).
func tryFlock(path string, how int) (*flock, bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &flock{f: f, path: path}, true, nil
}

// lockShared takes a blocking shared lock (used on the lifecycle lock).
func lockShared(path string, timeout time.Duration) (*flock, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return &flock{f: f, path: path}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("lifecycle lock busy (runner shutting down?)")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (l *flock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// isHeld probes whether someone holds path (without keeping the lock).
func isHeld(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	l, ok, err := tryLock(path)
	if err != nil {
		return false
	}
	if ok {
		l.Unlock()
		return false
	}
	return true
}

// Layout computes paths under the work root.
type Layout struct{ Root string }

// Gate is held shared by every session and exclusively by --exclusive runs,
// so exclusivity holds whatever max_slots each client uses.
func (l Layout) Gate() string { return filepath.Join(l.Root, "tokens", "gate.lock") }

func (l Layout) TokenLock(i int) string {
	return filepath.Join(l.Root, "tokens", fmt.Sprintf("token-%02d.lock", i))
}
func (l Layout) TokenHolder(i int) string {
	return filepath.Join(l.Root, "tokens", fmt.Sprintf("token-%02d.json", i))
}
func (l Layout) Repo(id string) string { return filepath.Join(l.Root, "repos", id) }
func (l Layout) Slot(id string, n int) string {
	return filepath.Join(l.Repo(id), "slots", fmt.Sprintf("slot-%02d", n))
}
func (l Layout) Deps(id string) string { return filepath.Join(l.Repo(id), "deps") }

// SlotState persists per-slot metadata.
type SlotState struct {
	Fingerprint string `json:"fingerprint"`
	WorktreeKey string `json:"worktree_key"`
	DepsKey     string `json:"deps_key"`
	LastUsed    int64  `json:"last_used"`
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSON writes atomically via rename.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadSlotState(dir string) SlotState {
	var s SlotState
	_ = readJSON(filepath.Join(dir, "state.json"), &s)
	return s
}

func saveSlotState(dir string, s SlotState) error {
	return writeJSON(filepath.Join(dir, "state.json"), s)
}

// readHolders lists the holders of currently-locked tokens.
func readHolders(l Layout) []protocol.Holder {
	var out []protocol.Holder
	ents, _ := os.ReadDir(filepath.Join(l.Root, "tokens"))
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "token-"), ".lock"))
		if err != nil {
			continue
		}
		if !isHeld(l.TokenLock(n)) {
			continue
		}
		var h protocol.Holder
		_ = readJSON(l.TokenHolder(n), &h)
		h.Token = n
		out = append(out, h)
	}
	return out
}

// touchActivity records activity for the idle watchdog.
func touchActivity(root string) {
	now := time.Now()
	for _, p := range []string{filepath.Join(root, "activity"), ActivityFile} {
		if p == ActivityFile {
			if _, err := os.Stat(LifecycleDir); err != nil {
				continue
			}
		}
		// Rewriting the content (rather than utimes with explicit times) only
		// needs write permission, so it works on the root-owned 0666 file that
		// cloud-init creates.
		_ = os.WriteFile(p, []byte(strconv.FormatInt(now.Unix(), 10)+"\n"), 0o666)
	}
}

// draining reports whether the runner refuses new work.
func draining(root string) bool {
	for _, p := range []string{DrainingFile, filepath.Join(root, "draining")} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// processAlive reports whether a process group still has members.
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killGroup terminates a process group: SIGTERM, wait up to grace, SIGKILL.
func killGroup(pgid int, grace time.Duration) {
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	for i := 0; i < 40 && groupAlive(pgid); i++ {
		time.Sleep(50 * time.Millisecond)
	}
}

// Keepalive refreshes the VM's activity clock (warm pools) and prints
// status like `status` does.
func Keepalive(w io.Writer, root string) error {
	touchActivity(root)
	return PrintStatus(w, root)
}

// Drain marks the VM draining (no new sessions; it powers off once the
// running ones finish) and prints status. Used by the pool manager to retire
// a warm instance ahead of max_lifetime.
func Drain(w io.Writer, root, reason string) error {
	markDraining(root, reason)
	return PrintStatus(w, root)
}
