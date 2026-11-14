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
