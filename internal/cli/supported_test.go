package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/manifest"
)

// supportedEnv isolates the global config; runners is its body after version.
func supportedEnv(t *testing.T, runners string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SPOTEXEC_CONFIG_DIR", filepath.Join(home, "spotexec"))
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(home, "spotexec", "config.toml"))
	t.Setenv("SPOTEXEC_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("SPOTEXEC_POOL", "")
	t.Setenv("SPOTEXEC_RUNNER", "")
	t.Setenv("SPOTEXEC_SYNC", "")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CEILING_DIRECTORIES"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if runners != "" {
		os.MkdirAll(filepath.Join(home, "spotexec"), 0o755)
		if err := os.WriteFile(filepath.Join(home, "spotexec", "config.toml"), []byte("version = 1\n"+runners), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const sshRunner = "[runners.box]\ntype = \"ssh\"\nhost = \"me@box\"\n"

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

// outsideGit is a temporary directory Git won't find a repository above.
func outsideGit(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(d))
	if _, isGit := manifest.Root(d); isGit {
		t.Fatalf("%s is inside a Git worktree", d)
	}
	if config.NeverProject(d) {
		t.Skipf("%s is a directory spotexec never syncs", d)
	}
	return d
}

func putFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSupported(t *testing.T) {
	cases := []struct {
		name    string
		runners string
		setup   func(t *testing.T) string // returns the directory to check
		want    bool
	}{
		{"project and runner", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return d
		}, true},
		{"subdirectory of a project", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			sub := filepath.Join(d, "pkg", "api")
			os.MkdirAll(sub, 0o755)
			return sub
		}, true},
		{"nested project config", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, "api", ".spotexec.toml"), "version = 1\n")
			return filepath.Join(d, "api")
		}, true},
		{"outside git", sshRunner, func(t *testing.T) string {
			d := outsideGit(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return d
		}, true},
		{"git sync source outside git", sshRunner, func(t *testing.T) string {
			d := outsideGit(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n[sync]\nsource = \"git\"\n")
			return d
		}, false},
		{"git sync source outside git, sync.mode none", sshRunner, func(t *testing.T) string {
			d := outsideGit(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n[sync]\nsource = \"git\"\nmode = \"none\"\n")
			return d
		}, true},
		{"git sync source outside git, SPOTEXEC_SYNC=none", sshRunner, func(t *testing.T) string {
			t.Setenv("SPOTEXEC_SYNC", "none")
			d := outsideGit(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n[sync]\nsource = \"git\"\n")
			return d
		}, true},
		{"SPOTEXEC_SYNC is not a sync mode", sshRunner, func(t *testing.T) string {
			t.Setenv("SPOTEXEC_SYNC", "bogus")
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return d
		}, false},
		{"symlink into a project", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			sub := filepath.Join(d, "pkg", "api")
			os.MkdirAll(sub, 0o755)
			link := filepath.Join(t.TempDir(), "api")
			if err := os.Symlink(sub, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, true},
		{"nearest config wins: broken nested", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			putFile(t, filepath.Join(d, "api", ".spotexec.toml"), "version = 1\nnot_a_key = true\n")
			return filepath.Join(d, "api")
		}, false},
		{"nearest config wins: broken root", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\nnot_a_key = true\n")
			putFile(t, filepath.Join(d, "api", ".spotexec.toml"), "version = 1\n")
			return filepath.Join(d, "api")
		}, true},
		{"home directory is the repository", sshRunner, func(t *testing.T) string {
			home := os.Getenv("HOME")
			if out, err := exec.Command("git", "init", "-q", home).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			putFile(t, filepath.Join(home, "notes", ".spotexec.toml"), "version = 1\n")
			return filepath.Join(home, "notes")
		}, false},
		{"project runner extends a global one", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\npool = [\"fast\"]\n[runners.fast]\nextends = \"box\"\n")
			return d
		}, true},
		{"project sets a global-only runner key", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n[runners.box]\nhost = \"other\"\n")
			return d
		}, false},
		{"SPOTEXEC_RUNNER names an unknown runner", sshRunner, func(t *testing.T) string {
			t.Setenv("SPOTEXEC_RUNNER", "nope")
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return d
		}, false},
		{"malformed global config", "[runners.box\n", func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return d
		}, false},
		{"no project config", sshRunner, func(t *testing.T) string { return initRepo(t) }, false},
		{"config above the repository root", sshRunner, func(t *testing.T) string {
			parent := t.TempDir()
			putFile(t, filepath.Join(parent, ".spotexec.toml"), "version = 1\n")
			repo := filepath.Join(parent, "repo")
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			return repo
		}, false},
		{"broken project config", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\nnot_a_key = true\n")
			return d
		}, false},
		{"no runners", "", func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return d
		}, false},
		{"pool names an unknown runner", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\npool = [\"nope\"]\n")
			return d
		}, false},
		{"missing directory in a project", sshRunner, func(t *testing.T) string {
			d := initRepo(t)
			putFile(t, filepath.Join(d, ".spotexec.toml"), "version = 1\n")
			return filepath.Join(d, "gone")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supportedEnv(t, tc.runners)
			if got := supported(tc.setup(t)); got != tc.want {
				t.Fatalf("supported = %v, want %v", got, tc.want)
			}
		})
	}
}

// The command prints exactly true or false and exits 0 or 1.
func TestSupportedCommand(t *testing.T) {
	supportedEnv(t, sshRunner)
	yes := initRepo(t)
	putFile(t, filepath.Join(yes, ".spotexec.toml"), "version = 1\n")
	no := initRepo(t)
	run := func(args ...string) (int, string) {
		t.Helper()
		r, w, _ := os.Pipe()
		er, ew, _ := os.Pipe()
		oldOut, oldErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = w, ew
		code := Main(args)
		os.Stdout, os.Stderr = oldOut, oldErr
		w.Close()
		ew.Close()
		b, _ := io.ReadAll(r)
		eb, _ := io.ReadAll(er)
		r.Close()
		er.Close()
		if len(eb) > 0 {
			t.Errorf("spotexec %v wrote to stderr: %q", args, eb)
		}
		return code, string(b)
	}
	if code, out := run("supported", "-C", yes); code != 0 || out != "true\n" {
		t.Errorf("supported -C yes = %d %q, want 0 \"true\\n\"", code, out)
	}
	if code, out := run("supported", "--dir", no); code != 1 || out != "false\n" {
		t.Errorf("supported --dir no = %d %q, want 1 \"false\\n\"", code, out)
	}
	if code, out := run("supported", "-C", filepath.Join(yes, "gone")); code != 1 || out != "false\n" {
		t.Errorf("supported -C missing = %d %q, want 1 \"false\\n\"", code, out)
	}
	t.Chdir(yes)
	if code, out := run("supported"); code != 0 || out != "true\n" {
		t.Errorf("supported in yes = %d %q, want 0 \"true\\n\"", code, out)
	}
}
