package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
)

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestWriteLocalKeepsLocalPermissions(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		before os.FileMode
		remote uint32
		want   os.FileMode
	}{
		{"private file modified remotely", 0o600, fsid.ModeFile, 0o600},
		{"group-writable file", 0o664, fsid.ModeFile, 0o664},
		{"made executable remotely", 0o600, fsid.ModeExec, 0o700},
		{"made executable, world-readable", 0o644, fsid.ModeExec, 0o755},
		{"executable bit cleared remotely", 0o750, fsid.ModeFile, 0o640},
		{"still executable", 0o700, fsid.ModeExec, 0o700},
	}
	for i, c := range cases {
		name := filepath.Join("f", string(rune('a'+i)))
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, c.before); err != nil {
			t.Fatal(err)
		}
		if err := writeLocal(dir, filepath.ToSlash(name), remoteFile{data: []byte("new\n"), mode: c.remote}, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if b, _ := os.ReadFile(full); string(b) != "new\n" {
			t.Fatalf("%s: content %q", c.name, b)
		}
		if got := perm(t, full); got != c.want {
			t.Errorf("%s: %o, want %o", c.name, got, c.want)
		}
	}
}

func TestWriteLocalNewFilesRespectUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := writeLocal(dir, "new.txt", remoteFile{data: []byte("x"), mode: fsid.ModeFile}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeLocal(dir, "new.sh", remoteFile{data: []byte("x"), mode: fsid.ModeExec}, nil); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, filepath.Join(dir, "new.txt")); got != 0o600 {
		t.Errorf("new file: %o, want 0600 under umask 077", got)
	}
	if got := perm(t, filepath.Join(dir, "new.sh")); got != 0o700 {
		t.Errorf("new executable: %o, want 0700 under umask 077", got)
	}
}

func TestWriteLocalHonoursAChmodDuringStaging(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, ".npmrc")
	if err := os.WriteFile(full, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, 0o644); err != nil {
		t.Fatal(err)
	}
	var staged os.FileMode
	stagedHook = func() {
		// While the replacement is staged it is private, and the user
		// tightens the original.
		matches, _ := filepath.Glob(filepath.Join(dir, ".spotexec-tmp-*"))
		if len(matches) == 1 {
			staged = perm(t, matches[0])
		}
		_ = os.Chmod(full, 0o600)
	}
	defer func() { stagedHook = nil }()
	if err := writeLocal(dir, ".npmrc", remoteFile{data: []byte("new\n"), mode: fsid.ModeFile}, nil); err != nil {
		t.Fatal(err)
	}
	if staged != 0o600 {
		t.Errorf("staged copy was %o, want 0600", staged)
	}
	if got := perm(t, full); got != 0o600 {
		t.Errorf("after write-back %o, want the tightened 0600", got)
	}
}

func TestWritebackLockWaitIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "writeback.lock")
	unlock, err := lockFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Another sx (here: another open file description) waits for it.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = lockFile(ctx, p)
	if err == nil {
		t.Fatal("took a lock that is held")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("waited %s past the deadline", waited)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Fatalf("error doesn't name the holder (%s): %v", want, err)
	}
	unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	unlock2, err := lockFile(ctx2, p)
	if err != nil {
		t.Fatalf("lock not free after release: %v", err)
	}
	unlock2()
	// A deadline that has already passed never takes the lock.
	done, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if u, err := lockFile(done, p); err == nil {
		u()
		t.Fatal("took the lock with an expired deadline")
	}
}

func TestConflictStashIsDiffable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	stash := filepath.Join(t.TempDir(), "conflicts", "run-1")
	local := filepath.Join(root, "src", "a b.txt")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wb := &WritebackResult{ConflictDir: stash}
	rf := remoteFile{data: []byte("remote\n"), mode: fsid.ModeFile}
	if err := wb.conflict(root, "src/a b.txt", version{true, fsid.Bytes(rf.data), rf.mode}, rf); err != nil {
		t.Fatal(err)
	}
	if err := wb.conflict(root, "src/gone.txt", version{}, remoteFile{}); err != nil {
		t.Fatal(err)
	}
	if len(wb.Stashed) != 2 || len(wb.Conflicts) != 2 {
		t.Fatalf("stashed %v", wb.Stashed)
	}
	c := wb.Stashed[0]
	if c.Path != "src/a b.txt" || c.Local != local || c.Deleted {
		t.Fatalf("conflict %+v", c)
	}
	if b, err := os.ReadFile(c.Remote); err != nil || string(b) != "remote\n" {
		t.Fatalf("stashed copy at %s: %q, %v", c.Remote, b, err)
	}
	if d := wb.Stashed[1]; !d.Deleted || !strings.HasSuffix(d.Remote, ".deleted") {
		t.Fatalf("deletion conflict %+v", d)
	}
	if _, err := os.Stat(wb.Stashed[1].Remote); err != nil {
		t.Fatalf("deletion marker: %v", err)
	}
	// The suggested commands run as printed and show the difference.
	diff := func(c Conflict, want ...string) {
		t.Helper()
		out, _ := exec.Command("sh", "-c", conflictDiff(c)).CombinedOutput()
		for _, w := range want {
			if !strings.Contains(string(out), w) {
				t.Fatalf("%s: no %q in:\n%s", conflictDiff(c), w, out)
			}
		}
	}
	diff(c, "-local edit", "+remote")
	// Deleted remotely: your copy against nothing.
	gone := filepath.Join(root, "src", "gone.txt")
	if err := os.WriteFile(gone, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff(wb.Stashed[1], "-kept")
	// Deleted locally: nothing against the remote version.
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	diff(c, "+remote")
}

func TestConflictsAreReported(t *testing.T) {
	rec := &recordingReporter{}
	stashed := []Conflict{
		{Path: "a.txt", Local: "/w/a.txt", Remote: "/g/spotexec/conflicts/r/a.txt"},
		{Path: "b.txt", Local: "/w/b.txt", Remote: "/g/spotexec/conflicts/r/b.txt.deleted", Deleted: true},
	}
	reportConflicts(rec, stashed, "r")
	if len(rec.warnings) != 2 || len(rec.events) != 2 {
		t.Fatalf("warnings %v, events %v", rec.warnings, rec.events)
	}
	for i, ev := range rec.events {
		if ev["path"] != stashed[i].Path || ev["local"] != stashed[i].Local || ev["remote"] != stashed[i].Remote || ev["diff"] == "" {
			t.Fatalf("event %v", ev)
		}
		if !strings.Contains(rec.warnings[i], ev["diff"].(string)) {
			t.Fatalf("warning without its diff command: %s", rec.warnings[i])
		}
	}
}

type recordingReporter struct {
	warnings []string
	events   []map[string]any // conflict events
	named    []string         // every event, by name
	fields   []map[string]any // and its fields
}

func (r *recordingReporter) Event(name string, f map[string]any) {
	if name == "conflict" {
		r.events = append(r.events, f)
	}
	r.named = append(r.named, name)
	r.fields = append(r.fields, f)
}
func (r *recordingReporter) Status(string, ...any) {}
func (r *recordingReporter) Clear()                {}
func (r *recordingReporter) Warn(code, format string, args ...any) {
	r.warnings = append(r.warnings, code+": "+fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Done(string, bool)     {}
func (r *recordingReporter) PartDone(string, bool) {}
func (r *recordingReporter) IsJSON() bool          { return true }
