package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spotexec/spotexec/internal/fsid"
)

// With core.autocrlf=input git reports a CRLF file as clean while its index
// ID is the LF blob; the manifest must carry the ID of the worktree bytes.
func TestAutocrlfInputHashesWorktree(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "core.autocrlf", "input")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	p := filepath.Join(dir, ".dockerignore")
	if err := os.WriteFile(p, []byte("src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "i")
	crlf := []byte("src\r\n")
	if err := os.WriteFile(p, crlf, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Build(context.Background(), Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.ByPath[".dockerignore"].ID, fsid.Bytes(crlf); got != want {
		t.Fatalf("manifest ID %s, want worktree bytes' ID %s", got, want)
	}
}
