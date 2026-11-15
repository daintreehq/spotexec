package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// wbFixture is a worktree and the manifest a run uploaded from it.
type wbFixture struct {
	ws *Workspace
	m  *manifest.Manifest
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newWBFixture makes a worktree holding committed (written, then committed
// when git is set) and local (written after the commit) files, and the
// manifest of every file in it.
func newWBFixture(t *testing.T, git bool, committed, local map[string]string) wbFixture {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if git {
		gitRun(t, root, "init", "-q")
	}
	for p, b := range committed {
		writeFile(t, root, p, b)
	}
	head := ""
	if git && len(committed) > 0 {
		gitRun(t, root, "add", "-A")
		gitRun(t, root, "commit", "-qm", "base")
		head = gitRun(t, root, "rev-parse", "HEAD")
	}
	for p, b := range local {
		writeFile(t, root, p, b)
	}
	ws, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Root: root, Git: git, Head: head, ByPath: map[string]protocol.Entry{}}
	err = filepath.WalkDir(root, func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, full)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == ".spotexec" {
				return filepath.SkipDir
			}
			return nil
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

// apply writes back a run that deleted dels and rewrote writes.
func (f wbFixture) apply(t *testing.T, dels []string, writes map[string]string) WritebackResult {
	t.Helper()
	remote := map[string]version{}
	files := map[string]remoteFile{}
	for _, p := range dels {
		remote[p] = version{}
	}
	for p, b := range writes {
		rf := remoteFile{data: []byte(b), id: fsid.Bytes([]byte(b)), mode: fsid.ModeFile}
		remote[p] = version{true, rf.id, rf.mode}
		files[p] = rf
	}
	var wb WritebackResult
	if err := applyWriteback(context.Background(), f.ws, f.m, remote, files, "run-1", &wb); err != nil {
		t.Fatal(err)
	}
	return wb
}

func gone(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s was not deleted", p)
		}
	}
}

func readOr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestWritebackBacksUpUncommittedWork(t *testing.T) {
	f := newWBFixture(t, true,
		map[string]string{"src/clean.go": "clean\n", "src/edited.go": "committed\n"},
		map[string]string{"src/edited.go": "my edit\n", "notes/todo.txt": "only here\n"})
	// git checkout -- . ; git clean -fd ; rm src/clean.go
	wb := f.apply(t, []string{"notes/todo.txt", "src/clean.go"}, map[string]string{"src/edited.go": "committed\n"})

	if len(wb.Deleted) != 2 || len(wb.Applied) != 1 {
		t.Fatalf("applied %v, deleted %v", wb.Applied, wb.Deleted)
	}
	if wb.BackupDir == "" {
		t.Fatal("no backup dir reported")
	}
	if got := readOr(filepath.Join(wb.BackupDir, "notes", "todo.txt")); got != "only here\n" {
		t.Errorf("deleted untracked file: backup %q", got)
	}
	if got := readOr(filepath.Join(wb.BackupDir, "src", "edited.go")); got != "my edit\n" {
		t.Errorf("reverted edit: backup %q", got)
	}
	if _, err := os.Lstat(filepath.Join(wb.BackupDir, "src", "clean.go")); err == nil {
		t.Error("a file with a committed copy was backed up")
	}
	if got := strings.Join(wb.BackedUp, ","); got != "notes/todo.txt,src/edited.go" {
		t.Errorf("BackedUp = %q", got)
	}
	gone(t, f.ws.Root, "notes/todo.txt", "src/clean.go")
	if !strings.HasPrefix(wb.BackupDir, f.ws.GitDir) {
		t.Errorf("backup %s is not under the git dir %s", wb.BackupDir, f.ws.GitDir)
	}
	if got := readOr(filepath.Join(f.ws.Root, "src", "edited.go")); got != "committed\n" {
		t.Errorf("the remote revert was not applied: %q", got)
	}
}

func TestWritebackBacksUpEverythingOutsideGit(t *testing.T) {
	f := newWBFixture(t, false, nil, map[string]string{".spotexec.toml": "[sync]\n", "a.txt": "a\n"})
	wb := f.apply(t, []string{"a.txt"}, nil)
	if got := readOr(filepath.Join(wb.BackupDir, "a.txt")); got != "a\n" {
		t.Errorf("backup %q", got)
	}
	gone(t, f.ws.Root, "a.txt")
	if !strings.HasPrefix(wb.BackupDir, filepath.Join(f.ws.Root, ".spotexec")+string(filepath.Separator)) {
		t.Errorf("backup dir %s", wb.BackupDir)
	}
}

func TestWritebackWithNothingToSaveMakesNoBackup(t *testing.T) {
	f := newWBFixture(t, true, map[string]string{"a.txt": "a\n", "b.txt": "b\n"}, nil)
	wb := f.apply(t, []string{"a.txt"}, map[string]string{"b.txt": "new\n"})
	if wb.BackupDir != "" || len(wb.BackedUp) != 0 {
		t.Errorf("backed up %v to %q", wb.BackedUp, wb.BackupDir)
	}
	gone(t, f.ws.Root, "a.txt")
	if got := readOr(filepath.Join(f.ws.Root, "b.txt")); got != "new\n" {
		t.Errorf("b.txt = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(f.ws.GitDir, "spotexec", "backup")); err == nil {
		t.Error("an empty backup directory was made")
	}
}

func TestWritebackBacksUpStagedAndModeOnlyChanges(t *testing.T) {
	f := newWBFixture(t, true, map[string]string{"run.sh": "echo\n", "keep.txt": "k\n"}, map[string]string{"staged.txt": "staged\n"})
	gitRun(t, f.ws.Root, "add", "staged.txt")
	if err := os.Chmod(filepath.Join(f.ws.Root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	id, mode, size, err := fsid.Path(filepath.Join(f.ws.Root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	f.m.ByPath["run.sh"] = protocol.Entry{Path: "run.sh", ID: id, Mode: mode, Size: size}
	wb := f.apply(t, []string{"staged.txt", "run.sh"}, nil)
	if got := strings.Join(wb.BackedUp, ","); got != "run.sh,staged.txt" {
		t.Fatalf("BackedUp = %q", got)
	}
	if got := perm(t, filepath.Join(wb.BackupDir, "run.sh")); got != 0o755 {
		t.Errorf("backup of an executable is %o", got)
	}
	gone(t, f.ws.Root, "staged.txt", "run.sh")
	if out := gitRun(t, f.ws.Root, "diff", "--cached", "--name-only"); out != "staged.txt" {
		t.Errorf("the index changed: %q", out)
	}
}

func TestWritebackBacksUpSymlinks(t *testing.T) {
	f := newWBFixture(t, true, map[string]string{"a.txt": "a\n"}, nil)
	if err := os.Symlink("a.txt", filepath.Join(f.ws.Root, "link")); err != nil {
		t.Fatal(err)
	}
	id, mode, size, err := fsid.Path(filepath.Join(f.ws.Root, "link"))
	if err != nil {
		t.Fatal(err)
	}
	f.m.ByPath["link"] = protocol.Entry{Path: "link", ID: id, Mode: mode, Size: size}
	wb := f.apply(t, []string{"link"}, nil)
	if got, err := os.Readlink(filepath.Join(wb.BackupDir, "link")); err != nil || got != "a.txt" {
		t.Errorf("symlink backup: %q, %v", got, err)
	}
	gone(t, f.ws.Root, "link")
}

func TestWritebackHoldsBackProjectConfigDeletion(t *testing.T) {
	f := newWBFixture(t, true, map[string]string{
		".spotexec.toml": "[sync]\n", ".gitignore": "x\n", "sub/.gitignore": "y\n",
		"a": "a", "b": "b", "c": "c", "d": "d", "e": "e", "f": "f",
	}, nil)
	wb := f.apply(t, []string{".spotexec.toml", ".gitignore", "sub/.gitignore", "a"}, nil)
	if got := strings.Join(wb.Held, ","); got != ".gitignore,.spotexec.toml" {
		t.Fatalf("held %q", got)
	}
	if got := strings.Join(wb.Deleted, ","); got != "sub/.gitignore,a" {
		t.Errorf("deleted %q", got)
	}
	for _, p := range []string{".spotexec.toml", ".gitignore"} {
		if _, err := os.Lstat(filepath.Join(f.ws.Root, p)); err != nil {
			t.Errorf("%s was deleted: %v", p, err)
		}
	}
	if !strings.Contains(wb.HeldWhy, ".spotexec.toml") {
		t.Errorf("reason %q", wb.HeldWhy)
	}
}

func TestWritebackMassDeletionGuard(t *testing.T) {
	for _, c := range []struct {
		files, dels int
		held        bool
	}{
		{10, 5, true},
		{11, 5, false},
		{8, 4, false}, // fewer than the minimum
		{25, 25, true},
	} {
		committed := map[string]string{}
		var dels []string
		for i := 0; i < c.files; i++ {
			p := fmt.Sprintf("src/f%02d.txt", i)
			committed[p] = p
			if i < c.dels {
				dels = append(dels, p)
			}
		}
		f := newWBFixture(t, true, committed, nil)
		wb := f.apply(t, dels, nil)
		held, deleted := strings.Join(wb.Held, ","), strings.Join(wb.Deleted, ",")
		if c.held && (held != strings.Join(dels, ",") || deleted != "") || !c.held && (held != "" || deleted != strings.Join(dels, ",")) {
			t.Errorf("%d of %d: held %q, deleted %q", c.dels, c.files, held, deleted)
		}
		if c.held {
			for _, p := range dels {
				if _, err := os.Lstat(filepath.Join(f.ws.Root, p)); err != nil {
					t.Errorf("%d of %d: %s deleted", c.dels, c.files, p)
				}
			}
		} else {
			gone(t, f.ws.Root, dels...)
		}
	}
}

func TestReportSafety(t *testing.T) {
	rec := &recordingReporter{}
	reportSafety(rec, WritebackResult{BackedUp: []string{"a"}, BackupDir: "/g/spotexec/backup/r", Held: []string{".gitignore"}, HeldWhy: "the run deleted the project's own .gitignore"}, "r")
	if len(rec.warnings) != 2 || !strings.Contains(rec.warnings[0], "/g/spotexec/backup/r") || !strings.HasPrefix(rec.warnings[1], "WRITEBACK_HELD: the run deleted the project's own .gitignore") {
		t.Fatalf("warnings %q", rec.warnings)
	}
	if strings.Join(rec.named, ",") != "backup,held_deletions" {
		t.Fatalf("events %v", rec.named)
	}
	if b := rec.fields[0]; b["dir"] != "/g/spotexec/backup/r" || fmt.Sprint(b["files"]) != "[a]" {
		t.Errorf("backup event %v", b)
	}
	if h := rec.fields[1]; fmt.Sprint(h["files"]) != "[.gitignore]" || h["reason"] != "the run deleted the project's own .gitignore" {
		t.Errorf("held_deletions event %v", h)
	}
	rec = &recordingReporter{}
	reportSafety(rec, WritebackResult{}, "r")
	if len(rec.warnings)+len(rec.named) != 0 {
		t.Errorf("reported %v %v for a write-back with nothing to say", rec.warnings, rec.named)
	}
}

func TestWritebackBacksUpBeforeTheFirstCommit(t *testing.T) {
	f := newWBFixture(t, true, nil, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	gitRun(t, f.ws.Root, "add", "a.txt")
	wb := f.apply(t, []string{"a.txt"}, nil)
	if got := readOr(filepath.Join(wb.BackupDir, "a.txt")); got != "a\n" {
		t.Errorf("backup %q", got)
	}
	gone(t, f.ws.Root, "a.txt")
}

// A backup is only made of the exact version write-back saw: anything else
// means the file changed during write-back, and nothing is saved or deleted.
func TestBackupLocalRefusesAChangedFile(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	writeFile(t, root, "a.txt", "now\n")
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    string
		want version
	}{
		{"a.txt", version{true, fsid.Bytes([]byte("then\n")), fsid.ModeFile}},
		{"a.txt", version{true, fsid.Bytes([]byte("now\n")), fsid.ModeExec}},
		{"a.txt", version{true, fsid.Bytes([]byte("a.txt")), fsid.ModeSymlink}},
		{"link", version{true, fsid.Bytes([]byte("b.txt")), fsid.ModeSymlink}},
		{"link", version{true, fsid.Bytes([]byte("now\n")), fsid.ModeFile}},
		{"missing", version{true, fsid.Bytes([]byte("now\n")), fsid.ModeFile}},
	} {
		if _, err := backupLocal(root, dir, c.p, c.want); !errors.Is(err, errChangedUnderUs) {
			t.Errorf("%s as %v: %v, want errChangedUnderUs", c.p, c.want, err)
		}
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left %v in the backup", left)
	}
	got, err := backupLocal(root, dir, "a.txt", version{true, fsid.Bytes([]byte("now\n")), fsid.ModeFile})
	if err != nil || readOr(got) != "now\n" {
		t.Fatalf("backup %s: %v", got, err)
	}
	// A second backup to the same name goes beside the first.
	again, err := backupLocal(root, dir, "a.txt", version{true, fsid.Bytes([]byte("now\n")), fsid.ModeFile})
	if err != nil || again == got || readOr(again) != "now\n" {
		t.Fatalf("second backup %s: %v", again, err)
	}
}
