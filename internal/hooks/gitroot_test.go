package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The hook's worktree and main checkout are the checkout asked for when a
// sibling's name differs only by a trailing space.
func TestGitRootKeepsTrailingSpace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot end in a space")
	}
	parent := t.TempDir()
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		parent = r
	}
	for _, name := range []string{"checkout", "checkout "} {
		dir := filepath.Join(parent, name)
		if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}
	spaced := filepath.Join(parent, "checkout ")
	if got := gitRoot(filepath.Join(spaced, "pkg")); got != spaced {
		t.Errorf("gitRoot = %q, want %q", got, spaced)
	}
	if got := mainCheckout(filepath.Join(spaced, "pkg")); got != spaced {
		t.Errorf("mainCheckout = %q, want %q", got, spaced)
	}
	if !cdsInside("cd pkg && go test ./...", spaced) {
		t.Error("cd into the checkout's own pkg counted as leaving it")
	}
	if cdsInside("cd '"+filepath.Join(parent, "checkout")+"' && go test ./...", spaced) {
		t.Error("cd into the sibling checkout counted as staying inside")
	}
}
