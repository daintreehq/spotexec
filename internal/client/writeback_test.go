package client

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

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
