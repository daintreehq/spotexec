package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A created file whose name looks like git pathspec magic must not make
// check-ignore fail (it would block write-back and every later sync).
func TestCheckIgnoredLiteralNames(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := Build(ctx, Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	ign, err := m.CheckIgnored(ctx, []string{":(literal)extra.txt", ":(exclude)x", "dist/app.js", "src/a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !ign["dist/app.js"] || ign["src/a.go"] || ign[":(literal)extra.txt"] || ign[":(exclude)x"] {
		t.Fatalf("ignored = %v", ign)
	}
}

// A path beyond a local symlink (git refuses it with exit 128) is answered
// on its own, as not ignored, instead of failing the whole batch.
func TestCheckIgnoredPathBeyondSymlink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "docs")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := Build(ctx, Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	ign, err := m.CheckIgnored(ctx, []string{"docs/evil.txt", "docs/sub/x.log", "dist/app.js", "src/a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !ign["dist/app.js"] || ign["src/a.go"] || ign["docs/evil.txt"] || ign["docs/sub/x.log"] {
		t.Fatalf("ignored = %v", ign)
	}
	// Nothing left for git to judge.
	if ign, err := m.CheckIgnored(ctx, []string{"docs/evil.txt"}); err != nil || len(ign) != 0 {
		t.Fatalf("only a blocked path: %v, %v", ign, err)
	}
}
