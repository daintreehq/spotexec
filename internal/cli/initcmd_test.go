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

// The written region follows the profile setup signs in with, read from
// the AWS config file without the AWS CLI.
func TestWriteGlobalTemplateRegion(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "aws-config")
	os.WriteFile(cfg, []byte("[default]\nregion = us-west-2\n[profile work]\nsso_session = corp\nregion = eu-west-1\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("PATH", dir) // no aws CLI
	for _, c := range []struct{ profile, env, want string }{
		{"work", "", "eu-west-1"},
		{"", "work", "eu-west-1"},
		{"", "", "us-west-2"},
		{"nosuch", "", "us-east-1"},
	} {
		t.Setenv("AWS_PROFILE", c.env)
		p := filepath.Join(dir, "config.toml")
		os.Remove(p)
		region, why, err := writeGlobalTemplate(p, c.profile)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		if region != c.want || !strings.Contains(string(b), `region = "`+c.want+`"`) || why == "" {
			t.Errorf("profile %q (AWS_PROFILE %q): region %q (%s), want %q", c.profile, c.env, region, why, c.want)
		}
	}
}
