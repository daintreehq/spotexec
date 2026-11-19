package client

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/manifest"
)

// isolateGit keeps the user's and system git configuration away from both
// the fixtures and the code under test.
func isolateGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// A checkout whose name ends in a space resolves to itself, never to the
// sibling Git would name if its output were trimmed: sync lists its files
// and write-back lands in it. Each keeps its git dir apart, under names that
// differ by the same space, so the git dir lookups are held to it too.
func TestResolveKeepsTrailingSpaceCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot end in a space")
	}
	isolateGit(t)
	parent := t.TempDir()
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		parent = r
	}
	plain, spaced := filepath.Join(parent, "checkout"), filepath.Join(parent, "checkout ")
	for dir, marker := range map[string]string{plain: "plain.txt", spaced: "spaced.txt"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := filepath.Join(parent, "meta"+strings.TrimPrefix(filepath.Base(dir), "checkout"))
		gitRun(t, dir, "init", "-q", "--separate-git-dir", meta)
		writeFile(t, dir, marker, marker+"\n")
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-qm", "base")
	}

	ws, err := Resolve(spaced)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != spaced || !ws.Git {
		t.Fatalf("Resolve root = %q (git %v), want %q", ws.Root, ws.Git, spaced)
	}
	for name, d := range map[string]string{"GitDir": ws.GitDir, "CommonGitDir": ws.CommonGitDir} {
		if want := filepath.Join(parent, "meta "); d != want {
			t.Errorf("%s = %q, want %q", name, d, want)
		}
	}
	other, err := Resolve(plain)
	if err != nil {
		t.Fatal(err)
	}
	if other.RepoID == ws.RepoID {
		t.Errorf("both checkouts share RepoID %s", ws.RepoID)
	}

	m, err := manifest.Build(context.Background(), manifest.Options{Root: ws.Root})
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	for _, e := range m.Entries {
		synced = append(synced, e.Path)
	}
	if got := strings.Join(synced, " "); got != "spaced.txt" {
		t.Errorf("synced %q, want spaced.txt only", got)
	}

	f := wbFixture{ws: ws, m: m}
	f.apply(t, nil, map[string]string{"out.txt": "result\n"})
	if got := readOr(filepath.Join(spaced, "out.txt")); got != "result\n" {
		t.Errorf("write-back into the requested checkout: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(plain, "out.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write-back reached the sibling checkout: %v", err)
	}
}

// The common git dir of a checkout whose name holds a newline is git's
// canonical absolute answer, not mistaken for old git echoing --path-format
// back. GIT_COMMON_DIR spells it with a "/./" that only the absolute form
// cleans away, so the fallback answer would differ.
func TestCommonGitDirKeepsNewlineInName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot hold a newline")
	}
	isolateGit(t)
	parent := t.TempDir()
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		parent = r
	}
	root := filepath.Join(parent, "check\nout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	if out, _ := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output(); strings.HasPrefix(string(out), "--path-format") {
		t.Skip("git before 2.31 has no --path-format")
	}
	t.Setenv("GIT_COMMON_DIR", root+"/./.git")
	want := filepath.Join(root, ".git")
	if got := commonGitDir(root, "fallback"); got != want {
		t.Errorf("commonGitDir = %q, want %q", got, want)
	}
}
