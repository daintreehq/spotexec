package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// nestedIgnoreTree lays out a project whose .gitignore files sit at several
// depths: each applies below its own directory, anchored patterns are
// relative to it, a deeper file overrides a shallower one (negation
// included), and nothing re-includes a path under an excluded directory.
func nestedIgnoreTree(t *testing.T, root string) {
	t.Helper()
	writeTree(t, root,
		"local-only.txt", "anchored.txt", "a.log", "src/a.go",
		"service/local-only.txt", "service/keep.log", "service/other.log",
		"service/anchored.txt", "service/x/anchored.txt", "service/x/local-only.txt",
		"service/sub/local-only.txt", "service/sub/main.go",
		"gen/out.txt", "gen/keep.txt",
		"build/a.o", "service/build/a.o",
	)
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "*.log\n/build/\ngen/\n")
	write("service/.gitignore", "local-only.txt\n!keep.log\n/anchored.txt\n")
	write("service/sub/.gitignore", "!local-only.txt\n")
	write("gen/.gitignore", "!*\n")
}

// What Git itself syncs from that layout.
const nestedIgnoreWant = ".gitignore anchored.txt local-only.txt service/.gitignore service/build/a.o service/keep.log service/sub/.gitignore service/sub/local-only.txt service/sub/main.go service/x/anchored.txt src/a.go"

// A directory walk applies every .gitignore at the directory declaring it,
// as Git does: a non-Git project and a Git root walked with
// sync.source = "directory" alike.
func TestNestedGitignoreDirectoryWalk(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	for _, gitRoot := range []bool{false, true} {
		name := "non-git directory"
		if gitRoot {
			name = "source directory"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			nestedIgnoreTree(t, root)
			source := ""
			if gitRoot {
				if _, err := exec.LookPath("git"); err != nil {
					t.Skip("git not installed")
				}
				gitIn(t, root, "init", "-q")
				source = "directory"
			}
			m, err := Build(ctx, Options{Root: root, Source: source})
			if err != nil {
				t.Fatal(err)
			}
			if m.Git {
				t.Fatal("walked as git")
			}
			if got := paths(m); got != nestedIgnoreWant {
				t.Errorf("synced %q\nwant   %q", got, nestedIgnoreWant)
			}
		})
	}
}

// Write-back judges a run's new files by the same rules: what the walk
// would skip stays remote, including in directories that don't exist here.
// Git is the reference, asked about the same layout in a repository of its
// own; spotexec judges a non-Git root and a Git root walked as a directory.
func TestNestedGitignoreCheckIgnored(t *testing.T) {
	isolateGit(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	probe := []string{
		"local-only.txt", "new.log", "service/local-only.txt", "service/new/local-only.txt",
		"service/keep.log", "service/new.log", "service/anchored.txt", "service/x/anchored.txt",
		"service/sub/local-only.txt", "service/sub/deeper/local-only.txt",
		"gen/keep.txt", "gen/new/file", "build/x", "service/build/x", "fresh/dir/file.go",
	}
	ref := t.TempDir()
	nestedIgnoreTree(t, ref)
	gitIn(t, ref, "init", "-q", "--template=")
	cmd := exec.Command("git", "-C", ref, "check-ignore", "--no-index", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(probe, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git check-ignore: %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	sort.Strings(want)
	if len(want) < 5 {
		t.Fatalf("git ignored only %q: the reference is broken", want)
	}

	for _, gitRoot := range []bool{false, true} {
		root := t.TempDir()
		nestedIgnoreTree(t, root)
		source := ""
		if gitRoot {
			gitIn(t, root, "init", "-q", "--template=")
			source = "directory"
		}
		m, err := Build(ctx, Options{Root: root, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		ign, err := m.CheckIgnored(ctx, probe)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for p := range ign {
			got = append(got, p)
		}
		sort.Strings(got)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("git root %v: ignored %q\ngit    %q", gitRoot, got, want)
		}
		// Each check reads the ignore files as they are now.
		if err := os.WriteFile(filepath.Join(root, "service", ".gitignore"), []byte("!local-only.txt\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ign, err := m.CheckIgnored(ctx, []string{"service/local-only.txt"}); err != nil || ign["service/local-only.txt"] {
			t.Errorf("git root %v: after the rule changed: %v, %v", gitRoot, ign, err)
		}
	}
}

// An ignore file that exists but can't be read fails the walk and the
// write-back check with its path, instead of syncing what it would exclude.
func TestGitignoreReadErrorAborts(t *testing.T) {
	ctx := context.Background()
	for _, rel := range []string{".gitignore", "service/.gitignore"} {
		t.Run(rel+" is a directory", func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, "src/a.go", "service/local-only.txt", rel+"/x")
			_, err := Build(ctx, Options{Root: root})
			if err == nil || !strings.Contains(err.Error(), filepath.Join(root, filepath.FromSlash(rel))) {
				t.Fatalf("Build error = %v, want one naming %s", err, rel)
			}
		})
	}

	t.Run("replaced by a directory after the walk", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, "src/a.go", "service/main.go", "service/.gitignore")
		m, err := Build(ctx, Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		gi := filepath.Join(root, "service", ".gitignore")
		if err := os.Remove(gi); err != nil {
			t.Fatal(err)
		}
		writeTree(t, root, "service/.gitignore/x")
		if _, err := m.CheckIgnored(ctx, []string{"service/new.txt"}); err == nil || !strings.Contains(err.Error(), gi+":") {
			t.Fatalf("CheckIgnored error = %v, want one naming %s", err, gi)
		}
	})

	t.Run("unreadable after the walk", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		root := t.TempDir()
		writeTree(t, root, "src/a.go", "service/main.go")
		gi := filepath.Join(root, "service", ".gitignore")
		if err := os.WriteFile(gi, []byte("local-only.txt\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := Build(ctx, Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(gi, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(gi, 0o644) })
		if _, err := m.CheckIgnored(ctx, []string{"service/local-only.txt"}); err == nil || !strings.Contains(err.Error(), gi) {
			t.Fatalf("CheckIgnored error = %v, want one naming %s", err, gi)
		}
		// A path the file doesn't apply to is still judged.
		if ign, err := m.CheckIgnored(ctx, []string{"src/new.go"}); err != nil || len(ign) != 0 {
			t.Fatalf("CheckIgnored(src/new.go) = %v, %v", ign, err)
		}
		if _, err := Build(ctx, Options{Root: root}); err == nil || !strings.Contains(err.Error(), gi) {
			t.Fatalf("Build error = %v, want one naming %s", err, gi)
		}
	})
}
