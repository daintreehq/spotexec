package helper

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// fakeDocker is a docker that works and has no containers; run executes
// the command after -c directly (as this user: a root test makes that the
// real fix-up), or does nothing with noop.
func fakeDocker(t *testing.T, noop bool) string {
	t.Helper()
	bin := t.TempDir()
	run := `while [ $# -gt 0 ] && [ "$1" != -c ]; do shift; done; shift; exec /bin/sh -c "$1"`
	if noop {
		run = "exit 0"
	}
	script := "#!/bin/sh\ncase \"$1\" in\nrun) " + run + ";;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// reclaimSession is a slot with a root run's marker. path is PATH (by
// default a docker that changes nothing, so the files must already be
// ours).
func reclaimSession(t *testing.T, path ...string) *Session {
	t.Helper()
	if len(path) == 0 {
		path = []string{fakeDocker(t, true), "/usr/bin", "/bin"}
	}
	t.Setenv("PATH", strings.Join(path, ":"))
	slot := t.TempDir()
	s := &Session{slotDir: slot, workspace: filepath.Join(slot, "workspace")}
	if err := os.MkdirAll(filepath.Join(s.workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.workspace, "src", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, rootRunMarker), []byte("node:22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

// An aborted root container run leaves its marker: the next holder checks
// the files, and clears the marker once they are all the runner user's.
func TestReclaimFilesClearsMarkerWhenOwned(t *testing.T) {
	s := reclaimSession(t)
	if err := s.reclaimFiles(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.slotDir, rootRunMarker)); !os.IsNotExist(err) {
		t.Fatal("marker kept after the files checked out")
	}
	if err := s.reclaimFiles(1); err != nil { // no marker: nothing to do
		t.Fatal(err)
	}
}

// Without docker, the root run's container can't be checked on: the slot
// isn't handed out.
func TestReclaimFilesNeedsDocker(t *testing.T) {
	s := reclaimSession(t, t.TempDir())
	if err := s.reclaimFiles(2); err == nil || !strings.Contains(err.Error(), "slot 2") {
		t.Fatalf("want the slot refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.slotDir, rootRunMarker)); err != nil {
		t.Fatal("marker dropped")
	}
}

// Files that can't be given back keep the slot from being handed out.
func TestReclaimFilesRefusesSlotItCantFix(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	s := reclaimSession(t)
	locked := filepath.Join(s.workspace, "src")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	err := s.reclaimFiles(3)
	if err == nil || !strings.Contains(err.Error(), "slot 3") {
		t.Fatalf("want the slot refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.slotDir, rootRunMarker)); err != nil {
		t.Fatal("marker dropped although the files are still not ours")
	}
}

// A directory whose entries can be listed but not inspected doesn't pass
// for "all ours".
func TestForeignFileReportsUncheckableEntries(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can inspect anything")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o444); err != nil { // readable, not searchable
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })
	if err := foreignFile(dir); err == nil {
		t.Fatal("unchecked files passed as ours")
	}
}

// Files a root container left behind are given back through docker (here a
// fake that runs the fix-up directly, as root): the real chown, then the
// check, then the marker goes.
func TestReclaimFilesGivesRootFilesBack(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getuid() != 0 {
		t.Skip("needs root on Linux to make files someone else's")
	}
	s := reclaimSession(t, fakeDocker(t, false), "/usr/bin", "/bin")
	var paths []string
	_ = filepath.Walk(s.workspace, func(p string, _ os.FileInfo, _ error) error { paths = append(paths, p); return nil })
	for _, p := range paths {
		if err := os.Lchown(p, 4242, 4242); err != nil {
			t.Fatal(err)
		}
	}
	if foreignFile(s.workspace) == nil {
		t.Fatal("setup: files still ours")
	}
	if err := s.reclaimFiles(1); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != 0 {
			t.Fatalf("%s still owned by %d:%d", p, st.Uid, st.Gid)
		}
	}
	if _, err := os.Stat(filepath.Join(s.slotDir, rootRunMarker)); !os.IsNotExist(err) {
		t.Fatal("marker kept")
	}
}
