package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
)

func resolveIn(t *testing.T, dir string) *client.Workspace {
	t.Helper()
	ws, err := client.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// init and container init never mark the home directory or /, and need a
// confirmation (--yes when not interactive) for a non-Git directory.
func TestCheckProjectDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{home, "/"} {
		err := checkProjectDir(resolveIn(t, d), true)
		if err == nil || !strings.Contains(err.Error(), "home directory") {
			t.Errorf("%s: want a refusal even with --yes, got %v", d, err)
		}
	}
	ssh := filepath.Join(home, ".ssh")
	os.MkdirAll(ssh, 0o700)
	if err := checkProjectDir(resolveIn(t, ssh), true); err == nil {
		t.Error("~/.ssh accepted as a project")
	}
	keys := filepath.Join(home, "keys")
	os.MkdirAll(keys, 0o700)
	os.Symlink(keys, filepath.Join(home, ".gnupg"))
	if ws := resolveIn(t, filepath.Join(home, ".gnupg")); ws.Marked || checkProjectDir(ws, true) == nil {
		t.Error("a symlinked ~/.gnupg accepted as a project")
	}
	// A leftover ~/.spotexec.toml doesn't change that.
	os.WriteFile(filepath.Join(home, ".spotexec.toml"), nil, 0o644)
	if err := checkProjectDir(resolveIn(t, home), true); err == nil {
		t.Error("home with a .spotexec.toml accepted")
	}

	plain := filepath.Join(home, "notes")
	os.MkdirAll(plain, 0o755)
	ws := resolveIn(t, plain)
	if ws.Marked {
		t.Fatalf("plain dir %s resolved as marked", ws.Root)
	}
	// Tests run without a terminal on stdin: refuse, pointing at --yes.
	if err := checkProjectDir(ws, false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-git without --yes: %v", err)
	}
	if err := checkProjectDir(ws, true); err != nil {
		t.Errorf("non-git with --yes: %v", err)
	}

	repo := filepath.Join(home, "repo")
	os.MkdirAll(repo, 0o755)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := checkProjectDir(resolveIn(t, repo), false); err != nil {
		t.Errorf("git repo: %v", err)
	}
}
