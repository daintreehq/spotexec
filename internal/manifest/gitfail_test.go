package manifest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// gitFatal is the first line git itself prints for rev-parse in dir, which
// differs between Git versions for the same fixture.
func gitFatal(t *testing.T, dir string) string {
	t.Helper()
	c := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := c.CombinedOutput()
	if err == nil {
		t.Fatalf("git rev-parse succeeded in %s: %s", dir, out)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// plainRoot is a fresh directory Git won't find a repository above.
func plainRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	return root
}

// A checkout Git can't inspect is not a plain directory: automatic sync
// refuses it with Git's own error instead of walking it, which would ignore
// .git/info/exclude and the global excludes. sync.source = "directory"
// still walks it, as the user chose, and sync.source = "git" keeps Git's
// error too.
func TestGitFailureRefusesAutomaticWalk(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  func(t *testing.T, root string, err error)
	}{
		{"unusable gitfile", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, ".git"), "gitdir: missing\n")
		}, nil},
		{"invalid gitfile", func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, ".git"), "garbage\n")
		}, nil},
		{"empty git dir", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"ownership refusal", func(t *testing.T, root string) {
			gitIn(t, root, "init", "-q")
			t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
			if f := gitFatal(t, root); !strings.Contains(f, "dubious ownership") {
				t.Skipf("this git doesn't simulate an ownership refusal: %s", f)
			}
		}, nil},
		{"not a work tree", func(t *testing.T, root string) {
			gitIn(t, root, "init", "-q")
			t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
			t.Setenv("GIT_IMPLICIT_WORK_TREE", "0")
		}, func(t *testing.T, root string, err error) {
			if err == nil || !strings.Contains(err.Error(), "does not treat it as a work tree") {
				t.Fatalf("auto Build err = %v; want the not-a-work-tree refusal", err)
			}
		}},
		{"GIT_DIR unreadable", func(t *testing.T, root string) {
			t.Setenv("GIT_DIR", filepath.Join(root, "missing"))
		}, nil},
		{"git missing", func(t *testing.T, root string) {
			if runtime.GOOS == "windows" {
				t.Skip("Windows also finds executables in the current directory")
			}
			gitIn(t, root, "init", "-q")
			t.Setenv("PATH", t.TempDir())
		}, func(t *testing.T, root string, err error) {
			if !errors.Is(err, exec.ErrNotFound) {
				t.Fatalf("auto Build err = %v; want git's lookup failure", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := plainRoot(t)
			writeTree(t, root, ".spotexec.toml", "src/a.txt")
			tc.setup(t, root)
			want := tc.want
			fatal := ""
			if want == nil {
				fatal = gitFatal(t, root)
				want = func(t *testing.T, root string, err error) {
					if err == nil || !strings.Contains(err.Error(), fatal) {
						t.Fatalf("Build err = %v; want Git's error %q", err, fatal)
					}
				}
			}
			_, err := Build(ctx, Options{Root: root})
			want(t, root, err)
			if !strings.Contains(err.Error(), `sync.source = "directory"`) {
				t.Errorf("auto Build err = %v; want the directory-mode hint", err)
			}
			_, err = Build(ctx, Options{Root: root, Source: "git"})
			if err == nil || !strings.Contains(err.Error(), `sync.source = "git"`) || !strings.Contains(err.Error(), fatal) {
				t.Errorf("git Build err = %v; want the strict-source refusal with Git's error %q", err, fatal)
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

// A canceled Build reports the cancellation, not an unreadable checkout.
func TestGitFailureCanceled(t *testing.T) {
	isolateGit(t)
	root := plainRoot(t)
	writeTree(t, root, ".spotexec.toml", "a.txt")
	gitIn(t, root, "init", "-q")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, Options{Root: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Build err = %v; want context.Canceled", err)
	}
}

// A project in a subdirectory of a checkout Git can't inspect is refused
// too: the checkout's .git is above the root Build is given.
func TestGitFailureAboveRootRefusesAutomaticWalk(t *testing.T) {
	isolateGit(t)
	repo := plainRoot(t)
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows also finds executables in the current directory")
	}
	isolateGit(t)
	root := plainRoot(t)
	t.Setenv("PATH", t.TempDir())
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

// From inside a checkout Git can't read, Root never widens to a project
// above it: the root is the nearest .spotexec.toml within the checkout,
// else the checkout itself, with Git's error.
func TestRootStopsAtUnreadableCheckout(t *testing.T) {
	isolateGit(t)
	parent := plainRoot(t)
	repo := filepath.Join(parent, "repo")
	writeTree(t, parent, ".spotexec.toml", "repo/sub/deep/x.txt")
	writeFile(t, filepath.Join(repo, ".git"), "garbage\n")
	deep := filepath.Join(repo, "sub", "deep")

	r, isGit, err := Root(deep)
	if r != repo || isGit || err == nil || !strings.Contains(err.Error(), "invalid gitfile format") {
		t.Fatalf("Root = %s, %v, %v; want the checkout and Git's error", r, isGit, err)
	}
	writeTree(t, repo, "sub/.spotexec.toml")
	sub := filepath.Join(repo, "sub")
	if r, isGit, err = Root(deep); r != sub || isGit || err == nil {
		t.Fatalf("Root = %s, %v, %v; want the project inside the checkout and Git's error", r, isGit, err)
	}
	// Outside the checkout nothing changes.
	if r, _, err = Root(parent); r != parent || err != nil {
		t.Fatalf("Root(parent) = %s, %v", r, err)
	}
}

// A repository Git is told to ignore (GIT_CEILING_DIRECTORIES, named
// through a symlink, which Git resolves) doesn't make a project below it an
// unreadable checkout: it walks as a plain directory.
func TestCeilingHidesOuterRepository(t *testing.T) {
	isolateGit(t)
	outer := plainRoot(t)
	gitIn(t, outer, "init", "-q")
	sub := filepath.Join(outer, "sub")
	writeTree(t, sub, ".spotexec.toml", "a.txt")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outer, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", link)
	r, isGit, err := Root(sub)
	if r != sub || isGit || err != nil {
		t.Fatalf("Root = %s, %v, %v; want the plain project", r, isGit, err)
	}
	m, err := Build(context.Background(), Options{Root: sub})
	if err != nil {
		t.Fatal(err)
	}
	if m.Git || paths(m) != ".spotexec.toml a.txt" {
		t.Errorf("Build = git %v, %q", m.Git, paths(m))
	}
	// After an empty entry Git takes ceilings as written: the symlink
	// then names nothing on the resolved path, and the repository counts.
	t.Setenv("GIT_CEILING_DIRECTORIES", string(filepath.ListSeparator)+link)
	writeFile(t, filepath.Join(outer, ".git", "HEAD"), "garbage\n")
	if _, _, err := Root(sub); err == nil {
		t.Fatalf("Root with an as-written ceiling: want the checkout's error")
	}
}
