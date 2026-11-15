package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// A linked worktree's backups and conflict copies live in the repository's
// common git dir, so a clean `git worktree remove` keeps them.
func TestLinkedWorktreeBackupsSurviveRemoval(t *testing.T) {
	for _, how := range []string{"remove", "prune"} {
		t.Run(how, func(t *testing.T) {
			main := newWBFixture(t, true, map[string]string{"src/a.go": "committed\n", "src/b.go": "b\n"}, nil)
			wt := filepath.Join(t.TempDir(), "wt")
			gitRun(t, main.ws.Root, "worktree", "add", "-q", wt)
			if r, err := filepath.EvalSymlinks(wt); err == nil {
				wt = r
			}
			writeFile(t, wt, "src/a.go", "my edit\n")
			writeFile(t, wt, "notes.txt", "only here\n")
			f := newWBFixtureAt(t, wt, main.m.Head)
			if f.ws.GitDir == f.ws.CommonGitDir {
				t.Fatalf("linked worktree git dir %s is the common dir", f.ws.GitDir)
			}
			// Edited locally during the run: the remote version is a conflict.
			writeFile(t, wt, "src/b.go", "edited meanwhile\n")
			// git clean -fd ; git checkout -- . ; and a change to src/b.go
			wb := f.apply(t, []string{"notes.txt"}, map[string]string{"src/a.go": "committed\n", "src/b.go": "remote\n"})
			if len(wb.Stashed) != 1 || wb.Stashed[0].Path != "src/b.go" {
				t.Fatalf("conflicts %+v", wb.Stashed)
			}
			common := filepath.Join(main.ws.GitDir, "spotexec") + string(filepath.Separator)
			if !strings.HasPrefix(wb.BackupDir, common) || !strings.HasPrefix(wb.Stashed[0].Remote, common) {
				t.Fatalf("backup %s / conflict %s are not under %s", wb.BackupDir, wb.Stashed[0].Remote, common)
			}
			gitRun(t, wt, "checkout", "--", "src/b.go")
			if out := gitRun(t, wt, "status", "--porcelain"); out != "" {
				t.Fatalf("worktree not clean:\n%s", out)
			}
			if how == "remove" {
				gitRun(t, main.ws.Root, "worktree", "remove", wt)
			} else {
				if err := os.RemoveAll(wt); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(f.ws.GitDir); err != nil {
					t.Fatalf("worktree metadata gone before prune: %v", err)
				}
				gitRun(t, main.ws.Root, "worktree", "prune", "--expire", "now")
			}
			if _, err := os.Stat(f.ws.GitDir); err == nil {
				t.Fatalf("worktree metadata %s still there", f.ws.GitDir)
			}
			if got := readOr(filepath.Join(wb.BackupDir, "notes.txt")); got != "only here\n" {
				t.Errorf("untracked file backup: %q", got)
			}
			if got := readOr(filepath.Join(wb.BackupDir, "src", "a.go")); got != "my edit\n" {
				t.Errorf("uncommitted edit backup: %q", got)
			}
			if got := readOr(wb.Stashed[0].Remote); got != "remote\n" {
				t.Errorf("conflict copy: %q", got)
			}
		})
	}
}

// newWBFixtureAt is newWBFixture for an existing worktree at root.
func newWBFixtureAt(t *testing.T, root, head string) wbFixture {
	t.Helper()
	ws, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Root: root, Git: true, Head: head, ByPath: map[string]protocol.Entry{}}
	err = filepath.WalkDir(root, func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, full)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".git" && d.IsDir():
			return filepath.SkipDir
		case rel == ".git" || d.IsDir():
			return nil // a linked worktree's .git is a file
		}
		id, mode, size, err := fsid.Path(full)
		if err != nil {
			return err
		}
		e := protocol.Entry{Path: rel, ID: id, Mode: mode, Size: size}
		m.Entries = append(m.Entries, e)
		m.ByPath[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return wbFixture{ws: ws, m: m}
}
