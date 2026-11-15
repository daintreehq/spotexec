package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestWriteLocalKeepsLocalPermissions(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		before os.FileMode
		remote uint32
		want   os.FileMode
	}{
		{"private file modified remotely", 0o600, fsid.ModeFile, 0o600},
		{"group-writable file", 0o664, fsid.ModeFile, 0o664},
		{"made executable remotely", 0o600, fsid.ModeExec, 0o700},
		{"made executable, world-readable", 0o644, fsid.ModeExec, 0o755},
		{"executable bit cleared remotely", 0o750, fsid.ModeFile, 0o640},
		{"still executable", 0o700, fsid.ModeExec, 0o700},
	}
	for i, c := range cases {
		name := filepath.Join("f", string(rune('a'+i)))
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, c.before); err != nil {
			t.Fatal(err)
		}
		if err := writeLocal(dir, filepath.ToSlash(name), remoteFile{data: []byte("new\n"), mode: c.remote}, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if b, _ := os.ReadFile(full); string(b) != "new\n" {
			t.Fatalf("%s: content %q", c.name, b)
		}
		if got := perm(t, full); got != c.want {
			t.Errorf("%s: %o, want %o", c.name, got, c.want)
		}
	}
}

func TestWriteLocalNewFilesRespectUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := writeLocal(dir, "new.txt", remoteFile{data: []byte("x"), mode: fsid.ModeFile}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeLocal(dir, "new.sh", remoteFile{data: []byte("x"), mode: fsid.ModeExec}, nil); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, filepath.Join(dir, "new.txt")); got != 0o600 {
		t.Errorf("new file: %o, want 0600 under umask 077", got)
	}
	if got := perm(t, filepath.Join(dir, "new.sh")); got != 0o700 {
		t.Errorf("new executable: %o, want 0700 under umask 077", got)
	}
}

func TestWriteLocalHonoursAChmodDuringStaging(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, ".npmrc")
	if err := os.WriteFile(full, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, 0o644); err != nil {
		t.Fatal(err)
	}
	var staged os.FileMode
	stagedHook = func() {
		// While the replacement is staged it is private, and the user
		// tightens the original.
		matches, _ := filepath.Glob(filepath.Join(dir, ".spotexec-tmp-*"))
		if len(matches) == 1 {
			staged = perm(t, matches[0])
		}
		_ = os.Chmod(full, 0o600)
	}
	defer func() { stagedHook = nil }()
	if err := writeLocal(dir, ".npmrc", remoteFile{data: []byte("new\n"), mode: fsid.ModeFile}, nil); err != nil {
		t.Fatal(err)
	}
	if staged != 0o600 {
		t.Errorf("staged copy was %o, want 0600", staged)
	}
	if got := perm(t, full); got != 0o600 {
		t.Errorf("after write-back %o, want the tightened 0600", got)
	}
}

func TestWritebackLockWaitIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "writeback.lock")
	unlock, err := lockFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Another sx (here: another open file description) waits for it.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = lockFile(ctx, p)
	if err == nil {
		t.Fatal("took a lock that is held")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("waited %s past the deadline", waited)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Fatalf("error doesn't name the holder (%s): %v", want, err)
	}
	unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	unlock2, err := lockFile(ctx2, p)
	if err != nil {
		t.Fatalf("lock not free after release: %v", err)
	}
	unlock2()
	// A deadline that has already passed never takes the lock.
	done, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if u, err := lockFile(done, p); err == nil {
		u()
		t.Fatal("took the lock with an expired deadline")
	}
}

func TestConflictStashIsDiffable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	stash := filepath.Join(t.TempDir(), "conflicts", "run-1")
	local := filepath.Join(root, "src", "a b.txt")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wb := &WritebackResult{ConflictDir: stash}
	rf := remoteFile{data: []byte("remote\n"), mode: fsid.ModeFile}
	if err := wb.conflict(root, "src/a b.txt", version{true, fsid.Bytes(rf.data), rf.mode}, rf, ""); err != nil {
		t.Fatal(err)
	}
	if err := wb.conflict(root, "src/gone.txt", version{}, remoteFile{}, ""); err != nil {
		t.Fatal(err)
	}
	if len(wb.Stashed) != 2 || len(wb.Conflicts) != 2 {
		t.Fatalf("stashed %v", wb.Stashed)
	}
	c := wb.Stashed[0]
	if c.Path != "src/a b.txt" || c.Local != local || c.Deleted {
		t.Fatalf("conflict %+v", c)
	}
	if b, err := os.ReadFile(c.Remote); err != nil || string(b) != "remote\n" {
		t.Fatalf("stashed copy at %s: %q, %v", c.Remote, b, err)
	}
	if d := wb.Stashed[1]; !d.Deleted || !strings.HasSuffix(d.Remote, ".deleted") {
		t.Fatalf("deletion conflict %+v", d)
	}
	if _, err := os.Stat(wb.Stashed[1].Remote); err != nil {
		t.Fatalf("deletion marker: %v", err)
	}
	// The suggested commands run as printed and show the difference.
	diff := func(c Conflict, want ...string) {
		t.Helper()
		out, _ := exec.Command("sh", "-c", conflictDiff(c)).CombinedOutput()
		for _, w := range want {
			if !strings.Contains(string(out), w) {
				t.Fatalf("%s: no %q in:\n%s", conflictDiff(c), w, out)
			}
		}
	}
	diff(c, "-local edit", "+remote")
	// Deleted remotely: your copy against nothing.
	gone := filepath.Join(root, "src", "gone.txt")
	if err := os.WriteFile(gone, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff(wb.Stashed[1], "-kept")
	// Deleted locally: nothing against the remote version.
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	diff(c, "+remote")
}

func TestConflictsAreReported(t *testing.T) {
	rec := &recordingReporter{}
	stashed := []Conflict{
		{Path: "a.txt", Local: "/w/a.txt", Remote: "/g/spotexec/conflicts/r/a.txt"},
		{Path: "b.txt", Local: "/w/b.txt", Remote: "/g/spotexec/conflicts/r/b.txt.deleted", Deleted: true},
	}
	reportConflicts(rec, stashed, "r")
	if len(rec.warnings) != 2 || len(rec.events) != 2 {
		t.Fatalf("warnings %v, events %v", rec.warnings, rec.events)
	}
	for i, ev := range rec.events {
		if ev["path"] != stashed[i].Path || ev["local"] != stashed[i].Local || ev["remote"] != stashed[i].Remote || ev["diff"] == "" {
			t.Fatalf("event %v", ev)
		}
		if !strings.Contains(rec.warnings[i], ev["diff"].(string)) {
			t.Fatalf("warning without its diff command: %s", rec.warnings[i])
		}
	}
}

type recordingReporter struct {
	warnings []string
	events   []map[string]any // conflict events
	named    []string         // every event, by name
	fields   []map[string]any // and its fields
}

func (r *recordingReporter) Event(name string, f map[string]any) {
	if name == "conflict" {
		r.events = append(r.events, f)
	}
	r.named = append(r.named, name)
	r.fields = append(r.fields, f)
}

// byName returns the fields of every event named name, in order.
func (r *recordingReporter) byName(name string) []map[string]any {
	var out []map[string]any
	for i, n := range r.named {
		if n == name {
			out = append(out, r.fields[i])
		}
	}
	return out
}

// latest returns the fields of the last event named name, or nil.
func (r *recordingReporter) latest(name string) map[string]any {
	evs := r.byName(name)
	if len(evs) == 0 {
		return nil
	}
	return evs[len(evs)-1]
}

func (r *recordingReporter) Status(string, ...any) {}
func (r *recordingReporter) Clear()                {}
func (r *recordingReporter) Warn(code, format string, args ...any) {
	r.warnings = append(r.warnings, code+": "+fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Done(string, bool)     {}
func (r *recordingReporter) PartDone(string, bool) {}
func (r *recordingReporter) IsJSON() bool          { return true }

// A file the run created with a backslash in its name is written back under
// that name, not refused or split into directories (#92).
func TestWriteLocalBackslashName(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip(`\ separates paths here`)
	}
	dir := t.TempDir()
	if err := writeLocal(dir, `src/back\slash.txt`, remoteFile{data: []byte("w"), mode: fsid.ModeFile}, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "src", `back\slash.txt`)); err != nil || string(b) != "w" {
		t.Errorf("content %q, %v", b, err)
	}
}

// A path beyond a local symlink is stashed and reported as such: never as a
// local edit, and never diffed through the symlink, out of the worktree.
func TestSymlinkParentConflict(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write := func(full, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(outside, "evil.txt"), "outside\n")
	write(filepath.Join(outside, "old.txt"), "outside\n")
	write(filepath.Join(root, "src", "a.txt"), "local edit\n")
	write(filepath.Join(root, "gen"), "a file, not a directory\n")
	// Below a real directory: every ancestor counts, not just the first.
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "nested", "docs")); err != nil {
		t.Fatal(err)
	}
	wb := &WritebackResult{ConflictDir: filepath.Join(t.TempDir(), "conflicts", "run-1")}
	rf := remoteFile{data: []byte("remote\n"), mode: fsid.ModeFile}
	want := version{true, fsid.Bytes(rf.data), rf.mode}
	for _, p := range []string{"nested/docs/evil.txt", "nested/docs/old.txt", "src/a.txt", "gen/x.txt"} {
		w, f := want, rf
		if p == "nested/docs/old.txt" {
			w, f = version{}, remoteFile{} // deleted by the remote command
		}
		if err := wb.conflict(root, p, w, f, ""); err != nil {
			t.Fatal(err)
		}
	}
	c, del, plain, file := wb.Stashed[0], wb.Stashed[1], wb.Stashed[2], wb.Stashed[3]
	if c.Reason != ReasonSymlinkParent || del.Reason != ReasonSymlinkParent || !del.Deleted || plain.Reason != "" || file.Reason != "" {
		t.Fatalf("reasons %q %q %q %q", c.Reason, del.Reason, plain.Reason, file.Reason)
	}
	if b, err := os.ReadFile(c.Remote); err != nil || string(b) != "remote\n" {
		t.Fatalf("stashed copy at %s: %q, %v", c.Remote, b, err)
	}
	for _, n := range []string{"evil.txt", "old.txt"} {
		if b, _ := os.ReadFile(filepath.Join(outside, n)); string(b) != "outside\n" {
			t.Fatalf("%s beyond the symlink changed: %q", n, b)
		}
	}
	nul := transport.ShellQuote(os.DevNull)
	if d := conflictDiff(c); strings.Contains(d, c.Local) || !strings.Contains(d, "-- "+nul+" ") {
		t.Fatalf("diff reads through the symlink: %s", d)
	}
	if d := conflictDiff(del); d != "git diff --no-index -- "+nul+" "+nul {
		t.Fatalf("deletion diff reads through the symlink: %s", d)
	}
	if d := conflictDiff(plain); !strings.Contains(d, transport.ShellQuote(plain.Local)) {
		t.Fatalf("plain conflict diff without the local file: %s", d)
	}

	rec := &recordingReporter{}
	reportConflicts(rec, wb.Stashed, "run-1")
	if len(rec.warnings) != 4 || len(rec.events) != 4 {
		t.Fatalf("warnings %v, events %v", rec.warnings, rec.events)
	}
	for i, w := range rec.warnings[:2] {
		if !strings.Contains(w, wb.Stashed[i].Path) || !strings.Contains(w, "symlink") || strings.Contains(w, "changed locally") {
			t.Fatalf("warning %d: %s", i, w)
		}
		if rec.events[i]["reason"] != ReasonSymlinkParent {
			t.Fatalf("event %d: %v", i, rec.events[i])
		}
	}
	if !strings.Contains(rec.warnings[0], "not written back") || !strings.Contains(rec.warnings[0], conflictDiff(c)) {
		t.Fatalf("warning without the stashed copy: %s", rec.warnings[0])
	}
	if !strings.Contains(rec.warnings[1], "deleted by the remote command but not here") {
		t.Fatalf("deletion warning: %s", rec.warnings[1])
	}
	for i := 2; i < 4; i++ {
		if !strings.Contains(rec.warnings[i], "changed locally") {
			t.Fatalf("plain conflict: %s", rec.warnings[i])
		}
		if _, ok := rec.events[i]["reason"]; ok {
			t.Fatalf("plain conflict event has a reason: %v", rec.events[i])
		}
	}
}

// A created path git couldn't judge (beyond a local symlink) is asked about
// again once a deletion removed the symlink: a gitignored output stays remote.
func TestNowIgnoredAfterSymlinkRemoved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "dist")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := manifest.Build(ctx, manifest.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writes := []string{"dist/app.log", "dist/app.js", "src/other.log"}
	unjudged := map[string]bool{"dist/app.log": true, "dist/app.js": true}
	if got := nowIgnored(ctx, m, root, writes, unjudged); len(got) != 0 {
		t.Fatalf("still beyond the symlink: %v", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	// Only what was unjudged: src/other.log was already answered.
	if got := nowIgnored(ctx, m, root, writes, unjudged); !reflect.DeepEqual(got, []string{"dist/app.log"}) {
		t.Fatalf("now ignored = %v", got)
	}
}

func TestLinkEscapes(t *testing.T) {
	cases := []struct {
		path, target string
		want         bool
	}{
		{"src/abs_link", "/etc/passwd", true},
		{"src/rel_link", "../../../../../../etc/hosts", true},
		{"docs", "/tmp", true},
		{"key", "/Users/me/.ssh/id_ed25519", true},
		{"src/up", "../..", true},
		{"src/parent", "..", false},
		{"link", "a.txt", false},
		{"src/link", "../README.md", false},
		{"src/deep/link", "../../src/x", false},
		{"src/odd", "./../../x", true},
		{"src/back", "../src/../../x", true},
		{"src/dots", "..x", false},
	}
	for _, c := range cases {
		if got := linkEscapes(c.path, c.target); got != c.want {
			t.Errorf("%s -> %s: %v, want %v", c.path, c.target, got, c.want)
		}
	}
}

func TestOutsideLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := []Link{
		{"src/inside", "a.txt"},          // stays in the worktree
		{"src/dangling", "missing"},      // nowhere yet, but inside
		{"src/abs", "/etc/passwd"},       // absolute
		{"src/climb", "../../x"},         // above the root by its text
		{"out", outside},                 // absolute, to a directory
		{"src/via", "../out/secret"},     // inside by its text, outside through out
		{"src/replaced", "/etc/hosts"},   // reported by its text even if replaced since
		{"src/gone", "a.txt"},            // removed since: nothing to judge
		{"src/swapped", "../out/secret"}, // replaced since by a link inside: gone
		{"src/pending", "../out/later"},  // through out to a file that doesn't exist yet
		{"src/dotdot", "../out/../x"},    // inside by its text; out/.. is outside's parent
	}
	for _, l := range links {
		if l.Path == "src/gone" {
			continue
		}
		if err := os.Symlink(l.Target, filepath.Join(root, filepath.FromSlash(l.Path))); err != nil {
			t.Fatal(err)
		}
	}
	for p, target := range map[string]string{"src/replaced": "a.txt", "src/swapped": "a.txt"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	got := outsideLinks(root, links)
	var paths []string
	for _, l := range got {
		paths = append(paths, l.Path)
	}
	want := "src/abs,src/climb,out,src/via,src/replaced,src/pending,src/dotdot"
	if strings.Join(paths, ",") != want {
		t.Fatalf("outside links %v, want %s", paths, want)
	}
}

func TestOutsideLinksAreReported(t *testing.T) {
	rec := &recordingReporter{}
	reportOutsideLinks(rec, nil, "r")
	if len(rec.warnings) != 0 || len(rec.named) != 0 {
		t.Fatalf("no links, but %v %v", rec.warnings, rec.named)
	}
	links := []Link{{"src/abs_link", "/etc/passwd"}, {"docs", "/tmp"}, {"ev\x1bil", "/a\x9b2Jb"}, {"src/up", "../../x"}}
	reportOutsideLinks(rec, links, "r")
	if len(rec.warnings) != 4 {
		t.Fatalf("warnings %v", rec.warnings)
	}
	for i, w := range rec.warnings[:2] {
		if !strings.HasPrefix(w, "WRITEBACK_SYMLINK: ") || !strings.Contains(w, links[i].Path+" a symlink to "+links[i].Target+", an absolute path;") {
			t.Fatalf("warning doesn't name the link and its target: %s", w)
		}
	}
	if w := rec.warnings[2]; strings.ContainsRune(w, 0x1b) || strings.Contains(w, "\x9b") || !strings.Contains(w, `"ev\x1bil"`) || !strings.Contains(w, `"/a\x9b2Jb"`) {
		t.Fatalf("control characters must reach the terminal escaped: %q", w)
	}
	if w := rec.warnings[3]; !strings.Contains(w, "src/up a symlink to ../../x, outside the worktree;") {
		t.Fatalf("a relative link out is described as absolute: %s", w)
	}
	evs := rec.byName("outside_symlink")
	if len(evs) != 4 {
		t.Fatalf("events %v", rec.named)
	}
	for i, ev := range evs {
		if ev["path"] != links[i].Path || ev["target"] != links[i].Target || ev["run_id"] != "r" {
			t.Fatalf("event %v for %v", ev, links[i])
		}
	}
}

// The issue's cases, applied: links created, a tracked file and a tracked
// link turned into links out of the worktree, a tracked directory replaced
// by one. All land, and exactly the ones leading out are reported.
func TestWritebackReportsLinksOutOfTheWorktree(t *testing.T) {
	root := t.TempDir()
	write := func(p, data string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/a.txt", "a\n")
	write("src/tracked.cfg", "cfg\n")
	write("src/edited.txt", "edited locally\n")
	write("docs/x.md", "x\n")
	if err := os.Symlink("a.txt", filepath.Join(root, "src", "link")); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{ByPath: map[string]protocol.Entry{}}
	for p, data := range map[string]string{"src/a.txt": "a\n", "src/tracked.cfg": "cfg\n", "src/edited.txt": "before\n", "docs/x.md": "x\n"} {
		m.ByPath[p] = protocol.Entry{Path: p, ID: fsid.Bytes([]byte(data)), Mode: fsid.ModeFile}
	}
	m.ByPath["src/link"] = protocol.Entry{Path: "src/link", ID: fsid.Bytes([]byte("a.txt")), Mode: fsid.ModeSymlink}

	remote := map[string]version{"docs/x.md": {}}
	files := map[string]remoteFile{}
	link := func(p, target string) {
		rf := remoteFile{link: target, mode: fsid.ModeSymlink, id: fsid.Bytes([]byte(target))}
		files[p], remote[p] = rf, version{true, rf.id, rf.mode}
	}
	link("src/abs_link", "/etc/passwd")
	link("src/rel_link", "../../../../../../etc/hosts")
	link("docs", "/tmp")
	link("src/tracked.cfg", "/home/me/.ssh/id_ed25519")
	link("src/link", "/etc/hosts")
	link("src/ok", "a.txt")               // stays inside: applied, not reported
	link("src/edited.txt", "/etc/shadow") // edited locally: a conflict, not applied

	wb := WritebackResult{}
	if err := applyWriteback(context.Background(), &Workspace{Root: root}, m, remote, files, nil, "r", &wb); err != nil {
		t.Fatal(err)
	}
	for p, rf := range files {
		if p == "src/edited.txt" {
			continue
		}
		if got, err := os.Readlink(filepath.Join(root, filepath.FromSlash(p))); err != nil || got != rf.link {
			t.Errorf("%s: %q %v, want a link to %s", p, got, err, rf.link)
		}
	}
	if len(wb.Conflicts) != 1 || wb.Conflicts[0] != "src/edited.txt" {
		t.Fatalf("conflicts %v", wb.Conflicts)
	}
	var got []string
	for _, l := range wb.OutsideLinks {
		got = append(got, l.Path+" -> "+l.Target)
	}
	want := "docs -> /tmp, src/abs_link -> /etc/passwd, src/link -> /etc/hosts, src/rel_link -> ../../../../../../etc/hosts, src/tracked.cfg -> /home/me/.ssh/id_ed25519"
	if strings.Join(got, ", ") != want {
		t.Fatalf("reported %v\nwant %s", got, want)
	}
}

// A worktree reached under another spelling (here a link to it; on macOS
// also a case variant) is still the worktree.
func TestWithinOtherSpelling(t *testing.T) {
	root := resolveExisting(t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !within(filepath.Join(alias, "src", "missing"), root, info, true) {
		t.Error("a path under another spelling of the root is outside")
	}
	if within(filepath.Dir(root), root, info, true) || within(filepath.Join(filepath.Dir(alias), "x"), root, info, true) {
		t.Error("a path outside the root is inside")
	}
}

// A link applied before write-back fails is still reported.
func TestWritebackReportsLinksBeforeAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	ro := filepath.Join(root, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	m := &manifest.Manifest{ByPath: map[string]protocol.Entry{}}
	link := remoteFile{link: "/etc/passwd", mode: fsid.ModeSymlink, id: fsid.Bytes([]byte("/etc/passwd"))}
	file := remoteFile{data: []byte("x"), mode: fsid.ModeFile, id: fsid.Bytes([]byte("x"))}
	remote := map[string]version{"a_link": {true, link.id, link.mode}, "ro/new.txt": {true, file.id, file.mode}}
	files := map[string]remoteFile{"a_link": link, "ro/new.txt": file}
	wb := WritebackResult{}
	if err := applyWriteback(context.Background(), &Workspace{Root: root}, m, remote, files, nil, "r", &wb); err == nil {
		t.Fatal("want the write into a read-only directory to fail")
	}
	if len(wb.OutsideLinks) != 1 || wb.OutsideLinks[0] != (Link{"a_link", "/etc/passwd"}) {
		t.Fatalf("outside links %v", wb.OutsideLinks)
	}
}
