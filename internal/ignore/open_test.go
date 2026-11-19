package ignore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// openIgnoreFile follows a symlink only when asked to: with noFollow a
// link at the path (dangling or not) fails with ELOOP, on every OS, while
// a regular file opens either way.
func TestOpenIgnoreFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "rules")
	if err := os.WriteFile(real, []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link, dangling := filepath.Join(dir, "link"), filepath.Join(dir, "dangling")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	read := func(p string, noFollow bool) (string, error) {
		f, err := openIgnoreFile(p, noFollow)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		return string(b), err
	}
	for _, noFollow := range []bool{false, true} {
		if got, err := read(real, noFollow); err != nil || got != "*.log\n" {
			t.Errorf("regular file, noFollow=%v: %q, %v", noFollow, got, err)
		}
	}
	if got, err := read(link, false); err != nil || got != "*.log\n" {
		t.Errorf("followed link: %q, %v", got, err)
	}
	for _, p := range []string{link, dangling} {
		if _, err := read(p, true); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("%s with noFollow: %v, want ELOOP", filepath.Base(p), err)
		}
	}
	// A symlinked file below the root is no file to a Tree; the root's
	// own is followed.
	if lines, err := readLines(link, true); err != nil || lines != nil {
		t.Errorf("readLines(link, noFollow) = %q, %v", lines, err)
	}
	if lines, err := readLines(link, false); err != nil || len(lines) != 1 {
		t.Errorf("readLines(link) = %q, %v", lines, err)
	}
}
