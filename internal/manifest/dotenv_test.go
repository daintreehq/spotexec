package manifest

import (
	"context"
	"os"
	"testing"
)

// isolateGit keeps the machine's Git config and ignore files (a global
// excludesFile, or ~/.config/git/ignore, listing .env) from hiding the
// files these tests need spotexec itself to exclude.
func isolateGit(t *testing.T) {
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_PARAMETERS", "GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_IMPLICIT_WORK_TREE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", os.DevNull)
}

// Untracked dotenv files never sync by default, before any project config
// exists: a fresh Git checkout and a non-Git directory alike. A dotenv
// template Git tracks (.env.example) syncs as every tracked file does.
func TestDotenvExcludedByDefault(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()

	t.Run("fresh git checkout", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, "src/a.txt", ".env.example")
		gitIn(t, root, "init", "-q")
		gitIn(t, root, "add", "-A")
		gitIn(t, root, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "init")
		writeTree(t, root, ".env", ".env.local", "pkg/.env.test", ".envrc", ".envs/local/db", "src/.ENV", "src/new.txt")
		m, err := Build(ctx, Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if !m.Git {
			t.Fatal("not walked as git")
		}
		if got, want := paths(m), ".env.example src/a.txt src/new.txt"; got != want {
			t.Errorf("synced %q, want %q", got, want)
		}
		if n := m.Excluded["builtin"]; n != 6 {
			t.Errorf("builtin excluded %d files, want 6 (%v)", n, m.Excluded)
		}
	})

	// Outside Git nothing is tracked, so no dotenv file syncs, the
	// template included: a non-Git directory (detected, not configured),
	// and a Git root walked with sync.source = "directory".
	for _, gitRoot := range []bool{false, true} {
		name := "non-git directory"
		if gitRoot {
			name = "source directory"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, "src/a.txt", ".env.example")
			source := ""
			if gitRoot {
				gitIn(t, root, "init", "-q")
				gitIn(t, root, "add", "-A")
				source = "directory"
			}
			writeTree(t, root, ".env", "pkg/.env.local", ".envs/local/db", "src/.Env")
			m, err := Build(ctx, Options{Root: root, Source: source})
			if err != nil {
				t.Fatal(err)
			}
			if m.Git {
				t.Fatal("walked as git")
			}
			if got, want := paths(m), "src/a.txt"; got != want {
				t.Errorf("synced %q, want %q", got, want)
			}
		})
	}
}
