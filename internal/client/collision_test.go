package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// foldsNames reports whether dir's filesystem opens a under the name b.
func foldsNames(t *testing.T, dir, a, b string) bool {
	t.Helper()
	p := filepath.Join(dir, a)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	ai, _ := os.Lstat(p)
	bi, err := os.Lstat(filepath.Join(dir, b))
	return err == nil && os.SameFile(ai, bi)
}

type collisionRun struct {
	t      *testing.T
	ws     *Workspace
	m      *manifest.Manifest
	remote map[string]version
	files  map[string]remoteFile
}

func newCollisionRun(t *testing.T) *collisionRun {
	return &collisionRun{
		t:      t,
		ws:     &Workspace{Root: t.TempDir(), GitDir: t.TempDir()},
		m:      &manifest.Manifest{ByPath: map[string]protocol.Entry{}},
		remote: map[string]version{},
		files:  map[string]remoteFile{},
	}
}

// local writes a file that was uploaded as base and is on disk as now.
func (c *collisionRun) local(p, base, now string) {
	c.t.Helper()
	full := filepath.Join(c.ws.Root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(now), 0o644); err != nil {
		c.t.Fatal(err)
	}
	c.m.ByPath[p] = protocol.Entry{Path: p, ID: fsid.Bytes([]byte(base)), Mode: fsid.ModeFile}
}

// wrote records that the remote command left p holding data.
func (c *collisionRun) wrote(p, data string) {
	rf := remoteFile{data: []byte(data), mode: fsid.ModeFile, id: fsid.Bytes([]byte(data))}
	c.remote[p] = version{true, rf.id, rf.mode}
	c.files[p] = rf
}

func (c *collisionRun) apply() WritebackResult {
	c.t.Helper()
	var wb WritebackResult
	if err := applyWriteback(context.Background(), c.ws, c.m, c.remote, c.files, nil, "run-1", &wb); err != nil {
		c.t.Fatal(err)
	}
	return wb
}

func (c *collisionRun) content(p string) string {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.ws.Root, filepath.FromSlash(p)))
	if err != nil {
		c.t.Fatal(err)
	}
	return string(b)
}

// stashed is the conflict recorded for p.
func stashed(t *testing.T, wb WritebackResult, p string) Conflict {
	t.Helper()
	for _, c := range wb.Stashed {
		if c.Path == p {
			return c
		}
	}
	t.Fatalf("no conflict for %q in %+v", p, wb.Stashed)
	return Conflict{}
}

const (
	nfc = "src/n\u00e9.txt"  // U+00E9, one code point
	nfd = "src/ne\u0301.txt" // e + combining acute
)

func TestRemoteNamesCollidingLocallyAreReportedAsCollisions(t *testing.T) {
	c := newCollisionRun(t)
	// Case-sensitive APFS still ignores normalisation: test what folds.
	cs := foldsNames(t, c.ws.Root, "x.txt", "X.txt")
	norm := foldsNames(t, c.ws.Root, "\u00e9.txt", "e\u0301.txt")
	if !cs && !norm {
		t.Skip("this filesystem tells every name apart: nothing collides")
	}
	if cs {
		c.wrote("src/Dup.txt", "A")
		c.wrote("src/DUP.txt", "B")
	}
	if norm {
		c.wrote(nfc, "1")
		c.wrote(nfd, "2")
	}
	wb := c.apply()

	if cs {
		// Byte order applies src/DUP.txt first; src/Dup.txt then names that file.
		d := stashed(t, wb, "src/Dup.txt")
		if d.With != "src/DUP.txt" || d.Local != filepath.Join(c.ws.Root, "src", "DUP.txt") {
			t.Fatalf("collision %+v", d)
		}
		if b, err := os.ReadFile(d.Remote); err != nil || string(b) != "A" {
			t.Fatalf("stashed remote src/Dup.txt: %q, %v", b, err)
		}
		if got := c.content("src/DUP.txt"); got != "B" {
			t.Fatalf("src/DUP.txt holds %q", got)
		}
	}
	if norm {
		n := stashed(t, wb, nfc)
		if n.With != nfd {
			t.Fatalf("normalisation collision %+v", n)
		}
		if got := c.content(nfd); got != "2" {
			t.Fatalf("%s holds %q", nfd, got)
		}
	}
	for _, s := range wb.Stashed {
		if s.With == "" {
			t.Fatalf("a collision reported as a local edit: %+v", s)
		}
	}

	rec := &recordingReporter{}
	reportConflicts(rec, wb.Stashed, "run-1")
	for i, w := range rec.warnings {
		s := wb.Stashed[i]
		if !strings.HasPrefix(w, "SYNC_COLLISION: ") || !strings.Contains(w, s.Path) || !strings.Contains(w, "kept "+s.With) || strings.Contains(w, "changed locally") {
			t.Fatalf("warning %q for %+v", w, s)
		}
		if rec.events[i]["with"] != s.With {
			t.Fatalf("event %v", rec.events[i])
		}
	}
	if norm && !strings.Contains(strings.Join(rec.warnings, "\n"), `"src/ne\u0301.txt"`) {
		t.Fatalf("names differing only in normalisation are not spelled out: %v", rec.warnings)
	}
}

func TestRemoteNameCollidingWithAnUnchangedTrackedFile(t *testing.T) {
	c := newCollisionRun(t)
	if !foldsNames(t, c.ws.Root, "x.txt", "X.txt") {
		t.Skip("case-sensitive filesystem: the names don't collide")
	}
	c.local("README.md", "tracked", "tracked")
	c.wrote("readme.md", "created")
	wb := c.apply()
	if s := stashed(t, wb, "readme.md"); s.With != "README.md" {
		t.Fatalf("collision %+v", s)
	}
	if got := c.content("README.md"); got != "tracked" {
		t.Fatalf("README.md holds %q", got)
	}
}

func TestRemoteNameCollidingWithAChangedTrackedFile(t *testing.T) {
	c := newCollisionRun(t)
	if !foldsNames(t, c.ws.Root, "x.txt", "X.txt") {
		t.Skip("case-sensitive filesystem: the names don't collide")
	}
	// README.md sorts first and is checked before readme.md is applied.
	c.local("readme.md", "old", "old")
	c.wrote("readme.md", "new")
	c.wrote("README.md", "created")
	wb := c.apply()
	if s := stashed(t, wb, "README.md"); s.With != "readme.md" {
		t.Fatalf("collision %+v", s)
	}
	if got := c.content("readme.md"); got != "new" {
		t.Fatalf("readme.md holds %q", got)
	}
	if len(wb.Applied) != 1 || wb.Applied[0] != "readme.md" {
		t.Fatalf("applied %v", wb.Applied)
	}
}

func TestLocalEditIsStillAConflict(t *testing.T) {
	c := newCollisionRun(t)
	c.local("src/a.txt", "base", "edited")
	c.wrote("src/a.txt", "remote")
	wb := c.apply()
	if s := stashed(t, wb, "src/a.txt"); s.With != "" {
		t.Fatalf("a local edit reported as a collision: %+v", s)
	}
	if got := c.content("src/a.txt"); got != "edited" {
		t.Fatalf("src/a.txt holds %q", got)
	}
}

func TestOnDiskName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Src", "File.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := onDiskName(root, "Src/File.txt"); !ok || got != "Src/File.txt" {
		t.Fatalf("exact name: %q, %v", got, ok)
	}
	if _, ok := onDiskName(root, "Src/missing.txt"); ok {
		t.Fatal("a missing file has an on-disk name")
	}
	if !foldsNames(t, root, "x.txt", "X.txt") {
		if _, ok := onDiskName(root, "src/file.txt"); ok {
			t.Fatal("case-sensitive filesystem: src/file.txt does not exist")
		}
		return
	}
	if got, ok := onDiskName(root, "src/file.txt"); !ok || got != "Src/File.txt" {
		t.Fatalf("folded name: %q, %v", got, ok)
	}
}

func TestEveryCollidingRemoteVersionIsStashed(t *testing.T) {
	c := newCollisionRun(t)
	if !foldsNames(t, c.ws.Root, "x.txt", "X.txt") {
		t.Skip("case-sensitive filesystem: the names don't collide")
	}
	// You edited README.md during the run (a conflict) while the run also
	// wrote readme.md and Readme.md: three stashed versions, one stash name.
	c.local("README.md", "base", "yours")
	c.wrote("README.md", "remote README")
	c.wrote("Readme.md", "remote Readme")
	c.wrote("readme.md", "remote readme")
	wb := c.apply()
	if len(wb.Stashed) != 3 || len(wb.Conflicts) != 3 {
		t.Fatalf("stashed %+v", wb.Stashed)
	}
	if s := stashed(t, wb, "README.md"); s.With != "" {
		t.Fatalf("your edit reported as a collision: %+v", s)
	}
	for _, p := range []string{"Readme.md", "readme.md"} {
		if s := stashed(t, wb, p); s.With != "README.md" {
			t.Fatalf("collision %+v", s)
		}
	}
	seen := map[string]bool{}
	for _, s := range wb.Stashed {
		b, err := os.ReadFile(s.Remote)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "remote "+strings.TrimSuffix(s.Path, ".md") {
			t.Fatalf("%s's stash at %s holds %q", s.Path, s.Remote, b)
		}
		seen[string(b)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("stashed versions overwrote each other: %v", seen)
	}
	if got := c.content("README.md"); got != "yours" {
		t.Fatalf("README.md holds %q", got)
	}
}

func TestIdenticalCollidingVersionsAreStillReported(t *testing.T) {
	c := newCollisionRun(t)
	if !foldsNames(t, c.ws.Root, "x.txt", "X.txt") {
		t.Skip("case-sensitive filesystem: the names don't collide")
	}
	c.wrote("Same.txt", "same")
	c.wrote("SAME.txt", "same")
	wb := c.apply()
	if s := stashed(t, wb, "Same.txt"); s.With != "SAME.txt" {
		t.Fatalf("collision %+v", s)
	}
	if len(wb.Applied) != 1 || wb.Applied[0] != "SAME.txt" {
		t.Fatalf("applied %v", wb.Applied)
	}
}

func TestOnDiskNameWithHardLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Skip("no hard links here")
	}
	if got, ok := onDiskName(root, "b.txt"); !ok || got != "b.txt" {
		t.Fatalf("exact name: %q, %v", got, ok)
	}
	if !foldsNames(t, root, "x.txt", "X.txt") {
		return
	}
	// A.txt folds to a.txt, not to its hard link b.txt.
	if got, ok := onDiskName(root, "A.txt"); !ok || got != "a.txt" {
		t.Fatalf("folded name: %q, %v", got, ok)
	}
}

func TestStashNeverOverwritesAnEarlierVersion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conflicts", "run-1")
	file := func(data string) (version, remoteFile) {
		rf := remoteFile{data: []byte(data), mode: fsid.ModeFile, id: fsid.Bytes([]byte(data))}
		return version{true, rf.id, rf.mode}, rf
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// a deleted remotely, then a.deleted changed remotely: the marker's
	// name is taken by the time a.deleted's version is stashed.
	mark, err := stashConflict(dir, "a", version{}, remoteFile{})
	if err != nil {
		t.Fatal(err)
	}
	w, rf := file("remote a.deleted")
	got, err := stashConflict(dir, "a.deleted", w, rf)
	if err != nil {
		t.Fatal(err)
	}
	if got == mark || read(got) != "remote a.deleted" || !strings.HasPrefix(read(mark), "deleted") {
		t.Fatalf("marker %s (%q), version %s (%q)", mark, read(mark), got, read(got))
	}
	// And the reverse: a deletion marker whose name a stashed file has.
	w, rf = file("remote b.deleted")
	first, err := stashConflict(dir, "b.deleted", w, rf)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stashConflict(dir, "b", version{}, remoteFile{})
	if err != nil {
		t.Fatal(err)
	}
	if second == first || read(first) != "remote b.deleted" || !strings.HasPrefix(read(second), "deleted") {
		t.Fatalf("version %s (%q), marker %s (%q)", first, read(first), second, read(second))
	}
	// The same path twice keeps both.
	w, rf = file("one")
	one, err := stashConflict(dir, "src/c", w, rf)
	if err != nil {
		t.Fatal(err)
	}
	w, rf = file("two")
	two, err := stashConflict(dir, "src/c", w, rf)
	if err != nil {
		t.Fatal(err)
	}
	if one == two || read(one) != "one" || read(two) != "two" {
		t.Fatalf("%s (%q), %s (%q)", one, read(one), two, read(two))
	}
}

func TestStashFallbackForADeepPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conflicts", "run-1")
	p := strings.Repeat("d/", 120) + strings.Repeat("x", 120) + ".txt"
	var got []string
	for _, data := range []string{"one", "two"} {
		rf := remoteFile{data: []byte(data), mode: fsid.ModeFile, id: fsid.Bytes([]byte(data))}
		s, err := stashConflict(dir, p, version{true, rf.id, rf.mode}, rf)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(s); err != nil || string(b) != data {
			t.Fatalf("%s: %q, %v", s, b, err)
		}
		got = append(got, s)
	}
	if got[0] == got[1] || len(filepath.Base(got[1])) > 255 {
		t.Fatalf("stashed at %v", got)
	}
	if _, err := stashConflict(dir, p, version{}, remoteFile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := stashConflict(dir, p, version{}, remoteFile{}); err != nil {
		t.Fatalf("second deletion marker: %v", err)
	}
}

func TestCollisionUnderAnotherDirectorySpelling(t *testing.T) {
	c := newCollisionRun(t)
	if !foldsNames(t, c.ws.Root, "x.txt", "X.txt") {
		t.Skip("case-sensitive filesystem: the names don't collide")
	}
	// d/A lands in the existing D/ as D/A, a spelling no path has; d/a is
	// then that file.
	c.local("D/keep", "k", "k")
	c.wrote("d/A", "upper")
	c.wrote("d/a", "lower")
	// SRC/x lands in the existing src/ as src/x: src/x's own name.
	c.local("src/keep", "k", "k")
	c.wrote("SRC/x", "upper")
	c.wrote("src/x", "lower")
	wb := c.apply()
	if s := stashed(t, wb, "d/a"); s.With != "d/A" {
		t.Fatalf("collision %+v", s)
	}
	if s := stashed(t, wb, "src/x"); s.With != "SRC/x" {
		t.Fatalf("collision %+v", s)
	}
	if got := c.content("D/A"); got != "upper" {
		t.Fatalf("D/A holds %q", got)
	}
}
