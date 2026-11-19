package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A checkout Git can't inspect is not a plain directory: automatic sync
// refuses it with Git's own error instead of walking it, which would ignore
// .git/info/exclude and the global excludes. sync.source = "directory"
// still walks it, as the user chose.
func TestGitFailureRefusesAutomaticWalk(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string // in the auto-mode error
	}{
		{"unusable gitfile", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, ".git"), "gitdir: "+filepath.Join(root, "missing")+"\n")
		}, "not a git repository"},
		{"invalid gitfile", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, ".git"), "garbage\n")
		}, "invalid gitfile format"},
		{"ownership refusal", func(t *testing.T, root string) {
			gitIn(t, root, "init", "-q")
			t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
		}, "dubious ownership"},
		{"git missing", func(t *testing.T, root string) {
			gitIn(t, root, "init", "-q")
			t.Setenv("PATH", t.TempDir())
		}, "git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, ".spotexec.toml", "src/a.txt")
			tc.setup(t, root)
			_, err := Build(ctx, Options{Root: root})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `sync.source = "directory"`) {
				t.Fatalf("auto Build err = %v; want Git's error (%q) and the directory-mode hint", err, tc.want)
			}
			m, err := Build(ctx, Options{Root: root, Source: "directory"})
			if err != nil {
				t.Fatalf("directory Build: %v", err)
			}
			if m.Git || paths(m) != ".spotexec.toml src/a.txt" {
				t.Errorf("directory Build = git %v, %q", m.Git, paths(m))
			}
		})
	}
}

// A project in a subdirectory of a checkout Git can't inspect is refused
// too: the checkout's .git is above the root Build is given.
func TestGitFailureAboveRootRefusesAutomaticWalk(t *testing.T) {
	isolateGit(t)
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git"), "garbage\n")
	sub := filepath.Join(repo, "sub")
	writeTree(t, sub, ".spotexec.toml", "a.txt")
	if _, err := Build(context.Background(), Options{Root: sub}); err == nil || !strings.Contains(err.Error(), "invalid gitfile format") {
		t.Fatalf("Build err = %v; want Git's error", err)
	}
}

// Without a .git anywhere, a missing git binary is no reason to refuse: the
// directory is plain.
func TestPlainDirectoryWalksWithoutGit(t *testing.T) {
	isolateGit(t)
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	writeTree(t, root, ".spotexec.toml", "src/a.txt")
	m, err := Build(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if m.Git || paths(m) != ".spotexec.toml src/a.txt" {
		t.Errorf("Build = git %v, %q", m.Git, paths(m))
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
