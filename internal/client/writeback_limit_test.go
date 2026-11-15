package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestWritebackLimitsOver(t *testing.T) {
	l := WritebackLimits{MaxFiles: 1000, MaxBytes: 100 << 20}
	for _, c := range []struct {
		files int
		bytes int64
		want  bool
	}{
		{1000, 100 << 20, false}, // at the limit is fine
		{1001, 0, true},
		{0, 100<<20 + 1, true},
		{0, 0, false},
	} {
		if got := l.Over(c.files, c.bytes); got != c.want {
			t.Errorf("Over(%d, %d) = %v, want %v", c.files, c.bytes, got, c.want)
		}
	}
	if (WritebackLimits{}).Over(1<<30, 1<<40) {
		t.Error("zero limits must never be over")
	}
	if got := (WritebackLimits{MaxBytes: 5 << 20}).String(); got != "5.0 MB" {
		t.Errorf("String() = %q", got)
	}
	if got := l.String(); got != "1000 files or 100.0 MB" {
		t.Errorf("String() = %q", got)
	}
}

// The issue's flood: 20,000 tiny files in gen/ and one 200 MB binary both
// show up, a lone deep file is named in full, and a group is named by the
// deepest directory its files share.
func TestLargestPathsNamesBigFilesAndCrowdedDirs(t *testing.T) {
	var es []protocol.Entry
	for i := range 20000 {
		es = append(es, protocol.Entry{Path: fmt.Sprintf("gen/f%d.txt", i), Size: 6})
	}
	es = append(es,
		protocol.Entry{Path: "big.bin", Size: 200 << 20},
		protocol.Entry{Path: "packages/app/heap.heapsnapshot", Size: 50 << 20},
		protocol.Entry{Path: "out/cov/lcov/a/x.html", Size: 10},
		protocol.Entry{Path: "out/cov/lcov/b/y.html", Size: 10},
		protocol.Entry{Path: "out/cov/lcov/b/z.html", Size: 10},
		protocol.Entry{Path: "notes.txt", Size: 1},
	)
	got := largestPaths(es, 2)
	want := []pathTotal{
		{Path: "big.bin", Files: 1, Bytes: 200 << 20},
		{Path: "packages/app/heap.heapsnapshot", Files: 1, Bytes: 50 << 20},
		{Path: "gen/", Files: 20000, Bytes: 120000},
		{Path: "out/cov/lcov/", Files: 3, Bytes: 30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("largestPaths = %+v\nwant %+v", got, want)
	}
	if s := describeLargest(got[:1]); s != "big.bin (200.0 MB)" {
		t.Errorf("describe = %q", s)
	}
	if s := got[2].String(); s != "gen/ (20000 files, 117 KB)" {
		t.Errorf("describe = %q", s)
	}
}

// Count ranking matters: a crowded directory of tiny files shows up even
// below many bigger single files.
func TestLargestPathsRanksByCountToo(t *testing.T) {
	var es []protocol.Entry
	for i := range 10 {
		es = append(es, protocol.Entry{Path: fmt.Sprintf("blob%d.bin", i), Size: int64(1000 + i)})
	}
	for i := range 500 {
		es = append(es, protocol.Entry{Path: fmt.Sprintf("gen/f%d", i), Size: 1})
	}
	got := largestPaths(es, 2)
	want := []pathTotal{{"blob9.bin", 1, 1009}, {"blob8.bin", 1, 1008}, {"gen/", 500, 500}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("largestPaths = %+v", got)
	}
}

func TestCommonDir(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"gen/f1", "gen/f2", "gen"},
		{"gen", "gen/sub/f3", "gen"},
		{"pkg/a/x/1", "pkg/a/x/2", "pkg/a/x"},
		{"pkg/a/x", "pkg/a/y/2", "pkg/a"}, // pkg/a/x already a directory
		{"pkg/app", "pkg/apple/x", "pkg"},
	} {
		if got := commonDir(c.a, c.b); got != c.want {
			t.Errorf("commonDir(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// gitRepo makes a git worktree with files committed (as they would be
// synced) and returns its canonical root.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, s := range files {
		writeFile(t, dir, rel, s)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x", "--allow-empty"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// tarOf is a gzip'd tar of files, as the helper streams them.
func tarOf(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, s := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(s)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func entry(p, s string) protocol.Entry {
	return protocol.Entry{Path: p, ID: fsid.Bytes([]byte(s)), Mode: fsid.ModeFile, Size: int64(len(s))}
}

// floodRun: a run that changed one synced file and created five new ones,
// one of them big, plus a gitignored output.
func floodRun(t *testing.T) (*Workspace, *manifest.Manifest, protocol.Result, map[string]string) {
	t.Helper()
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n", ".gitignore": "dist/\n"})
	m, err := manifest.Build(context.Background(), manifest.Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	remote := map[string]string{
		"src/a.go":    "package a // formatted\n",
		"gen/f1.txt":  "1\n",
		"gen/f2.txt":  "2\n",
		"gen/f3.txt":  "3\n",
		"gen/f4.txt":  "4\n",
		"big.bin":     strings.Repeat("x", 4096),
		"dist/app.js": "built\n",
	}
	var r protocol.Result
	for p, s := range remote {
		if p == "src/a.go" {
			r.Changed = append(r.Changed, entry(p, s))
		} else {
			r.Created = append(r.Created, entry(p, s))
		}
	}
	return &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}, m, r, remote
}

// Over the limit in hold mode, no new file is fetched or accepted (they stay
// pending in the slot), while the change to a synced file still comes back.
func TestWritebackHoldsNewFilesOverTheLimit(t *testing.T) {
	ws, m, r, remote := floodRun(t)
	var asked []protocol.Fetch
	fetch := func(f protocol.Fetch) (io.Reader, error) {
		asked = append(asked, f)
		sent := map[string]string{}
		for _, p := range f.Paths {
			sent[p] = remote[p]
		}
		return tarOf(t, sent), nil
	}
	rep := &recordingReporter{}
	wb, err := writeback(context.Background(), fetch, ws, m, r, "run1", nil, nil, WritebackLimits{MaxFiles: 3, Hold: true}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !reflect.DeepEqual(asked[0].Paths, []string{"src/a.go"}) || len(asked[0].Accept) != 0 {
		t.Fatalf("fetched %+v, want only src/a.go and nothing accepted", asked)
	}
	var held []string
	for _, e := range wb.HeldNew {
		held = append(held, e.Path)
	}
	if len(held) != 5 || wb.HeldNewBytes != 4096+8 || strings.Contains(strings.Join(held, " "), "dist/") {
		t.Fatalf("held %v (%d bytes), want the five new files and not the ignored output", held, wb.HeldNewBytes)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Root, "src/a.go")); string(b) != remote["src/a.go"] {
		t.Errorf("synced file not written back: %q", b)
	}
	for _, p := range []string{"gen", "big.bin"} {
		if _, err := os.Lstat(filepath.Join(ws.Root, p)); err == nil {
			t.Errorf("%s came back although held", p)
		}
	}
	if ev := rep.latest("writeback_held"); ev == nil || ev["files"] != 5 || ev["bytes"] != int64(4096+8) || ev["run_id"] != "run1" {
		t.Errorf("writeback_held event = %v", ev)
	}
	if len(rep.warnings) != 1 || !strings.HasPrefix(rep.warnings[0], "WRITEBACK_HELD: left 5 files (4 KB) in the slot: over the write-back limit of 3 files. Largest: big.bin (4 KB), gen/ (4 files, 8 B)") || !strings.Contains(rep.warnings[0], "spotexec pull --held") {
		t.Fatalf("warnings = %q", rep.warnings)
	}
}

// Without hold, everything comes back, with a warning naming the largest
// paths, sent before the transfer starts.
func TestWritebackWarnsOverTheLimit(t *testing.T) {
	ws, m, r, remote := floodRun(t)
	rep := &recordingReporter{}
	var asked protocol.Fetch
	over := true
	fetch := func(f protocol.Fetch) (io.Reader, error) {
		if over && len(rep.warnings) == 0 {
			t.Error("fetched before warning")
		}
		asked = f
		sent := map[string]string{}
		for _, p := range f.Paths {
			sent[p] = remote[p]
		}
		return tarOf(t, sent), nil
	}
	wb, err := writeback(context.Background(), fetch, ws, m, r, "run1", nil, nil, WritebackLimits{MaxFiles: 3}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.HeldNew) != 0 || len(asked.Paths) != 6 || len(asked.Accept) != 5 {
		t.Fatalf("held %d, fetched %v, accepted %v", len(wb.HeldNew), asked.Paths, asked.Accept)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Root, "gen/f4.txt")); string(b) != "4\n" {
		t.Error("new file not written back")
	}
	if ev := rep.latest("large_writeback"); ev == nil || ev["files"] != 6 || ev["new_files"] != 5 || ev["changed_files"] != 1 {
		t.Errorf("large_writeback event = %v", ev)
	}
	if len(rep.warnings) != 1 || !strings.HasPrefix(rep.warnings[0], "LARGE_WRITEBACK: bringing back 6 files (4 KB: 5 new, 1 changed), over the write-back limit of 3 files. Largest: big.bin (4 KB), src/a.go (23 B), gen/ (4 files, 8 B); if they are outputs, add them to .gitignore") {
		t.Fatalf("warnings = %q", rep.warnings)
	}

	// Under the limits: silent.
	over = false
	ws, m, r, _ = floodRun(t)
	rep = &recordingReporter{}
	wb, err = writeback(context.Background(), fetch, ws, m, r, "run2", nil, nil, WritebackLimits{MaxFiles: 6, MaxBytes: 1 << 20, Hold: true}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.warnings) != 0 || len(wb.HeldNew) != 0 || len(asked.Accept) != 5 {
		t.Fatalf("under the limits: warned %q, held %d, accepted %v", rep.warnings, len(wb.HeldNew), asked.Accept)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Root, "big.bin")); len(b) != 4096 {
		t.Error("new file not written back under the limits")
	}
}

// The byte limit alone holds too.
func TestWritebackHoldsOverTheByteLimit(t *testing.T) {
	ws, m, r, remote := floodRun(t)
	var asked protocol.Fetch
	fetch := func(f protocol.Fetch) (io.Reader, error) {
		asked = f
		return tarOf(t, map[string]string{"src/a.go": remote["src/a.go"]}), nil
	}
	wb, err := writeback(context.Background(), fetch, ws, m, r, "run1", nil, nil, WritebackLimits{MaxFiles: 100, MaxBytes: 4000, Hold: true}, &recordingReporter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.HeldNew) != 5 || !reflect.DeepEqual(asked.Paths, []string{"src/a.go"}) || len(asked.Accept) != 0 {
		t.Fatalf("held %d, fetched %v, accepted %v", len(wb.HeldNew), asked.Paths, asked.Accept)
	}
}

// The new side of a case-only rename is never held: it is fetched and
// accepted with the deletion, so rename handling still pairs them.
func TestWritebackNeverHoldsCaseRenames(t *testing.T) {
	ws, m, r, remote := floodRun(t)
	r.Deleted = []string{"src/a.go"}
	r.Changed = nil
	r.Created = append(r.Created, entry("src/A.go", remote["src/a.go"]))
	var asked protocol.Fetch
	fetch := func(f protocol.Fetch) (io.Reader, error) {
		asked = f
		return tarOf(t, map[string]string{"src/A.go": remote["src/a.go"]}), nil
	}
	wb, err := writeback(context.Background(), fetch, ws, m, r, "run1", nil, nil, WritebackLimits{MaxFiles: 1, Hold: true}, &recordingReporter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.HeldNew) != 5 || !reflect.DeepEqual(asked.Accept, []string{"src/A.go"}) {
		t.Fatalf("held %d, accepted %v", len(wb.HeldNew), asked.Accept)
	}
	if _, err := os.Lstat(filepath.Join(ws.Root, "src/A.go")); err != nil {
		t.Errorf("renamed file lost: %v", err)
	}
}

// A synced file's changes are never held: a formatter rewriting many files
// over the limit warns, without the hint to hold, and still applies.
func TestWritebackNeverHoldsChangedFiles(t *testing.T) {
	ws, m, r, remote := floodRun(t)
	r.Created = nil
	rep := &recordingReporter{}
	fetch := func(f protocol.Fetch) (io.Reader, error) {
		return tarOf(t, map[string]string{"src/a.go": remote["src/a.go"]}), nil
	}
	wb, err := writeback(context.Background(), fetch, ws, m, r, "run1", nil, nil, WritebackLimits{MaxBytes: 10, Hold: true}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.HeldNew) != 0 || len(wb.Applied) != 1 {
		t.Fatalf("held %v, applied %v", wb.HeldNew, wb.Applied)
	}
	if len(rep.warnings) != 1 || !strings.HasPrefix(rep.warnings[0], "LARGE_WRITEBACK") || strings.Contains(rep.warnings[0], "hold") {
		t.Fatalf("warnings = %q", rep.warnings)
	}
}

func TestHeldRecord(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	ws := &Workspace{WorktreeKey: "wk"}
	lr := LastRun{Runner: "aws", InstanceID: "i-1/x", Slot: 2}
	es := []protocol.Entry{entry("gen/f1.txt", "1\n"), entry("a*b[1].txt", "2\n"), entry(`win\name.txt`, "3\n")}
	if LoadHeld(ws, lr) != nil {
		t.Fatal("held before anything was saved")
	}
	if err := saveHeld(ws, lr, "run1", es); err != nil {
		t.Fatal(err)
	}
	held := LoadHeld(ws, lr)
	if len(held) != 3 || held["gen/f1.txt"] != es[0] {
		t.Fatalf("loaded %v", held)
	}
	other := lr
	other.Slot = 3
	if LoadHeld(ws, other) != nil {
		t.Error("another slot's record was used")
	}
	pats, sel, err := HeldPatterns(held, nil)
	if err != nil || !reflect.DeepEqual(pats, []string{"a?b?1].txt", "gen/f1.txt", "win?name.txt"}) || len(sel) != 3 {
		t.Errorf("patterns %v, %v, %v", pats, sel, err)
	}
	if pats, sel, err := HeldPatterns(held, []string{"gen"}); err != nil || !reflect.DeepEqual(pats, []string{"gen/f1.txt"}) || !reflect.DeepEqual(sel, HeldFiles{"gen/f1.txt": es[0]}) {
		t.Errorf("narrowed patterns %v, %v, %v", pats, sel, err)
	}
	if _, _, err := HeldPatterns(held, []string{"nope"}); err == nil {
		t.Error("no match, no error")
	}
	for _, p := range pats {
		if err := fsid.CheckPattern(p); err != nil {
			t.Errorf("the helper would refuse pattern %q: %v", p, err)
		}
	}
	if !newPullMatcher(pats)(`win\name.txt`) {
		t.Error("a held name with a backslash isn't asked for")
	}
	if !newPullMatcher(pats)("a*b[1].txt") || newPullMatcher(pats)("gen/f2.txt") {
		t.Error("held patterns don't ask for the held files")
	}
	if !newPullMatcher(pats)("axb?1].txt") {
		t.Error("expected the ? patterns to reach other names too (applyPull's heldOnly filters them)")
	}
	caseTwin := lr
	caseTwin.Runner = "AWS"
	if heldPath(ws, caseTwin) == heldPath(ws, lr) || strings.EqualFold(heldPath(ws, caseTwin), heldPath(ws, lr)) {
		t.Error("runners differing in case share a record file")
	}
	clearHeld(ws, lr)
	if LoadHeld(ws, lr) != nil {
		t.Error("record survived the clear")
	}
	if _, _, err := HeldPatterns(nil, nil); err == nil {
		t.Error("nothing held, no error")
	}
}

// pull brings held files back although Git doesn't ignore them, but only
// into an empty place, only as they were held, never over a tracked file,
// and never outside their own path in the worktree.
func TestPullHeldFiles(t *testing.T) {
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	writeFile(t, dir, "gen/f2.txt", "mine\n") // made locally since the run
	slot := map[string]string{
		"gen/f1.txt": "1\n",
		"gen/f2.txt": "2\n",
		"gen/f3.txt": "changed in the slot since\n",
		"gen/f4.txt": "4\n",
		"src/a.go":   "remote\n",
	}
	held := HeldFiles{}
	for _, e := range []protocol.Entry{entry("gen/f1.txt", "1\n"), entry("gen/f2.txt", "2\n"), entry("gen/f3.txt", "3\n"), entry("src/a.go", "remote\n")} {
		held[e.Path] = e
	}
	listed := []string{"gen/f1.txt", "gen/f2.txt", "gen/f3.txt", "gen/f4.txt", "src/a.go"}
	pull := func(root string, held HeldFiles) int {
		t.Helper()
		n, err := applyPull(context.Background(), ws, root, []string{"gen", "src"}, held, false, listed, tarOf(t, slot))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		return string(b)
	}

	if n := pull(dir, nil); n != 0 || read("gen/f1.txt") != "" {
		t.Fatalf("pulled %d non-ignored file(s) with nothing held", n)
	}
	if n := pull(filepath.Join(dir, "out"), held); n != 0 {
		t.Fatalf("pulled %d held file(s) into another place in the worktree", n)
	}
	if n := pull(dir, held); n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	if read("gen/f1.txt") != "1\n" || read("gen/f2.txt") != "mine\n" || read("gen/f3.txt") != "" || read("gen/f4.txt") != "" || read("src/a.go") != "package a\n" {
		t.Fatalf("after pull: f1 %q f2 %q f3 %q f4 %q a.go %q", read("gen/f1.txt"), read("gen/f2.txt"), read("gen/f3.txt"), read("gen/f4.txt"), read("src/a.go"))
	}
	if n := pull(dir, held); n != 0 { // already here: nothing to do
		t.Fatalf("second pull wrote %d", n)
	}

	// Tracked, even when deleted locally: never recreated from a hold.
	if err := os.Remove(filepath.Join(dir, "src/a.go")); err != nil {
		t.Fatal(err)
	}
	if pull(dir, held); read("src/a.go") != "" {
		t.Error("a tracked file was recreated from a hold")
	}

	// The same bytes with another mode were not what was held.
	modeHeld := HeldFiles{"gen/f4.txt": {Path: "gen/f4.txt", ID: fsid.Bytes([]byte("4\n")), Mode: fsid.ModeExec, Size: 2}}
	if n := pull(dir, modeHeld); n != 0 || read("gen/f4.txt") != "" {
		t.Error("a held file whose mode changed came back")
	}
}

// Once ignored locally, a held path still only lands in an empty place, as
// it was held; and --held writes nothing else its patterns reach.
func TestPullHeldFilesStayGuarded(t *testing.T) {
	dir := gitRepo(t, map[string]string{".gitignore": "gen/\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	writeFile(t, dir, "gen/occupied.txt", "mine\n")
	held := HeldFiles{
		"gen/occupied.txt": entry("gen/occupied.txt", "held\n"), // as held, but something is there now
		"gen/stale.txt":    entry("gen/stale.txt", "held\n"),    // nothing there, but changed in the slot
		"gen/mode.txt":     {Path: "gen/mode.txt", ID: fsid.Bytes([]byte("m\n")), Mode: fsid.ModeExec, Size: 2},
		"gen/a?.txt":       entry("gen/a?.txt", "q\n"),
		"gen/ab.txt":       entry("gen/ab.txt", "ab\n"), // held, but not selected below
	}
	slot := map[string]string{"gen/occupied.txt": "held\n", "gen/stale.txt": "changed since\n", "gen/mode.txt": "m\n", "gen/ab.txt": "ab\n", "gen/a?.txt": "q\n", "gen/ac.txt": "unrelated\n"}
	listed := []string{"gen/a?.txt", "gen/ab.txt", "gen/ac.txt", "gen/mode.txt", "gen/occupied.txt", "gen/stale.txt"}
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		return string(b)
	}
	pull := func(globs ...string) int {
		t.Helper()
		pats, sel, err := HeldPatterns(held, globs)
		if err != nil {
			t.Fatal(err)
		}
		n, err := applyPull(context.Background(), ws, dir, pats, sel, true, listed, tarOf(t, slot))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := pull("gen/a[?].txt"); n != 1 || read("gen/a?.txt") != "q\n" || read("gen/ab.txt") != "" || read("gen/ac.txt") != "" {
		t.Fatalf("narrowed --held pulled %d: a? %q ab %q ac %q", n, read("gen/a?.txt"), read("gen/ab.txt"), read("gen/ac.txt"))
	}
	if n := pull(); n != 1 || read("gen/ab.txt") != "ab\n" || read("gen/ac.txt") != "" {
		t.Fatalf("--held pulled %d: ab %q ac %q", n, read("gen/ab.txt"), read("gen/ac.txt"))
	}
	if read("gen/occupied.txt") != "mine\n" || read("gen/stale.txt") != "" || read("gen/mode.txt") != "" {
		t.Fatalf("guards lapsed for ignored held files: occupied %q stale %q mode %q", read("gen/occupied.txt"), read("gen/stale.txt"), read("gen/mode.txt"))
	}
}
