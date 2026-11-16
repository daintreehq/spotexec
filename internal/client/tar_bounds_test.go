package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

type tarEntry struct{ name, data string }

// rawTar is the uncompressed tar of entries, in order (repeats included).
func rawTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Size: int64(len(e.data)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// blobOf frames b as a helper sends a blob, ended with sendErr (nil: ok),
// and returns the reader the client reads it with.
func blobOf(t *testing.T, b []byte, sendErr error) io.Reader {
	t.Helper()
	var wire bytes.Buffer
	bw := &protocol.BlobWriter{C: protocol.NewConn(nil, &wire)}
	if _, err := bw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := bw.CloseWithError(sendErr); err != nil {
		t.Fatal(err)
	}
	c := protocol.NewConn(&wire, nil)
	return &protocol.BlobReader{Next: c.ReadFrame}
}

func readAll(t *testing.T, r io.Reader, limit int64) (map[string]string, error) {
	t.Helper()
	got := map[string]string{}
	err := readTar(r, limit, func(h *tar.Header, rd io.Reader) error {
		b, err := io.ReadAll(rd)
		got[h.Name] = string(b)
		return err
	})
	return got, err
}

func TestReadTarChecksTheGzipTrailer(t *testing.T) {
	good := gz(t, rawTar(t, []tarEntry{{name: "a", data: "hello"}}))
	if got, err := readAll(t, blobOf(t, good, nil), 0); err != nil || got["a"] != "hello" {
		t.Fatalf("intact archive: %v, %v", got, err)
	}
	// A corrupt CRC is only seen after the tar has ended.
	bad := append([]byte(nil), good...)
	bad[len(bad)-8] ^= 0xff
	if _, err := readAll(t, blobOf(t, bad, nil), 0); !errors.Is(err, gzip.ErrChecksum) {
		t.Fatalf("bad CRC: got %v, want a checksum error", err)
	}
	// Cut before the trailer.
	if _, err := readAll(t, blobOf(t, good[:len(good)-4], nil), 0); err == nil {
		t.Fatal("truncated gzip stream was accepted")
	}
}

func TestReadTarReportsTheSendersError(t *testing.T) {
	// The helper fails mid-file: half a gzip'd tar, then its error.
	full := gz(t, rawTar(t, []tarEntry{{name: "a", data: strings.Repeat("x", 1<<20)}}))
	_, err := readAll(t, blobOf(t, full[:len(full)/2], errors.New("read slot/a: input/output error")), 0)
	if err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("got %v, want the helper's error", err)
	}
	// It fails after a complete archive: still an error.
	_, err = readAll(t, blobOf(t, full, errors.New("late failure")), 0)
	if err == nil || !strings.Contains(err.Error(), "late failure") {
		t.Fatalf("got %v, want the helper's error", err)
	}
	// Nothing sent at all, then an error.
	_, err = readAll(t, blobOf(t, nil, errors.New("no such slot")), 0)
	if err == nil || !strings.Contains(err.Error(), "no such slot") {
		t.Fatalf("got %v, want the helper's error", err)
	}
}

// A complete gzip stream holding half a tar, then the helper's error: the
// tar reader's unexpected EOF gives way to the helper's message.
func TestReadTarPrefersTheSendersErrorToATruncatedTar(t *testing.T) {
	whole := rawTar(t, []tarEntry{{"a", strings.Repeat("x", 4096)}})
	half := gz(t, whole[:1024]) // the header and part of the body
	_, err := readAll(t, blobOf(t, half, errors.New("read slot/a: input/output error")), 0)
	if err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("got %v, want the helper's error", err)
	}
	// Without a sender error, the truncation itself is the error.
	if _, err := readAll(t, blobOf(t, half, nil), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want unexpected EOF", err)
	}
}

// tripwire counts reads that reach it.
type tripwire struct{ reads int }

func (w *tripwire) Read([]byte) (int, error) { w.reads++; return 0, io.EOF }

func TestReadTarRefusesMoreThanItsLimit(t *testing.T) {
	// Over the cap, nothing more is read from the sender: a live one could
	// keep sending forever.
	bomb := gz(t, rawTar(t, []tarEntry{{"zeros", string(make([]byte, 64<<20))}}))
	trip := &tripwire{}
	if err := readTar(io.MultiReader(bytes.NewReader(bomb), trip), 1<<20, func(*tar.Header, io.Reader) error { return nil }); !errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("got %v, want errArchiveTooLarge", err)
	}
	if trip.reads != 0 {
		t.Fatal("read on past the cap")
	}

	big := gz(t, rawTar(t, []tarEntry{{name: "skipped", data: strings.Repeat("0", 4<<20)}}))
	calls := 0
	err := readTar(bytes.NewReader(big), 1<<20, func(*tar.Header, io.Reader) error {
		calls++
		return nil // not read: the tar reader skips it, still within the cap
	})
	if !errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("got %v, want errArchiveTooLarge", err)
	}
	// Data after the end of the archive is refused, not decompressed.
	trailing := gz(t, append(rawTar(t, []tarEntry{{name: "a", data: "x"}}), make([]byte, 1<<20)...))
	if _, err := readAll(t, bytes.NewReader(trailing), 0); !errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("trailing data: got %v, want errArchiveTooLarge", err)
	}
}

// A runner that reports a 1-byte file and sends a bigger one fails the
// write-back on that file (well inside the archive's cap), before reading it,
// and leaves no spool behind.
func TestWritebackRefusesAFileLargerThanReported(t *testing.T) {
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	m := manifestOf(t, dir)
	r := protocol.Result{Created: []protocol.Entry{{Path: "bomb.bin", ID: fsid.Bytes([]byte("x")), Mode: fsid.ModeFile, Size: 1}}}
	fetch := func(protocol.Fetch) (io.Reader, error) {
		return tarOf(t, map[string]string{"bomb.bin": strings.Repeat("A", 100<<10)}), nil
	}
	_, err := writeback(context.Background(), fetch, ws, m, r, "run1", map[string]bool{}, nil, WritebackLimits{}, &recordingReporter{})
	if err == nil || !strings.Contains(err.Error(), "larger than the 1 B it reported") || errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("got %v, want the per-file size refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "bomb.bin")); !os.IsNotExist(err) {
		t.Fatalf("bomb.bin was written (%v)", err)
	}
	assertNoSpools(t, ws)
}

// A write-back archive that sends one path twice fails and applies nothing.
func TestWritebackRefusesARepeatedPath(t *testing.T) {
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	m := manifestOf(t, dir)
	r := protocol.Result{Created: []protocol.Entry{entry("gen/a", "one"), entry("gen/b", "two")}}
	fetch := func(protocol.Fetch) (io.Reader, error) {
		return bytes.NewReader(gz(t, rawTar(t, []tarEntry{{"gen/a", "one"}, {"gen/a", "two"}}))), nil
	}
	wb, err := writeback(context.Background(), fetch, ws, m, r, "run1", map[string]bool{}, nil, WritebackLimits{}, &recordingReporter{})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("got %v, want a repeated-path refusal", err)
	}
	if len(wb.Applied) != 0 {
		t.Fatalf("applied %v", wb.Applied)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen")); !os.IsNotExist(err) {
		t.Fatalf("gen/ was written (%v)", err)
	}
	assertNoSpools(t, ws)
}

// A spool still locked by its owner is never pruned, however old; one whose
// owner is gone is, once old.
func TestPruneSpoolsKeepsALiveSpool(t *testing.T) {
	ws := &Workspace{Root: t.TempDir(), GitDir: t.TempDir()}
	live, err := newSpool(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	dead, err := newSpool(ws)
	if err != nil {
		t.Fatal(err)
	}
	dead.lock.Close() // its process was killed
	old := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{live.dir, dead.dir} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	pruneSpools(filepath.Dir(live.dir), time.Now())
	if _, err := os.Stat(live.dir); err != nil {
		t.Fatalf("live spool pruned: %v", err)
	}
	if _, err := os.Stat(dead.dir); !os.IsNotExist(err) {
		t.Fatalf("abandoned spool kept (%v)", err)
	}
}

// A write-back that fetches files spools them on disk, applies them, and
// removes the spool; declining to proceed removes it too.
func TestWritebackSpoolIsRemoved(t *testing.T) {
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	m := manifestOf(t, dir)
	remote := map[string]string{"src/a.go": "package a // changed\n", "gen/out.txt": strings.Repeat("y", 300<<10)}
	r := protocol.Result{Changed: []protocol.Entry{entry("src/a.go", remote["src/a.go"])}, Created: []protocol.Entry{entry("gen/out.txt", remote["gen/out.txt"])}}
	fetch := func(protocol.Fetch) (io.Reader, error) { return tarOf(t, remote), nil }

	no := func() bool { return false }
	if _, err := writeback(context.Background(), fetch, ws, m, r, "run1", map[string]bool{}, no, WritebackLimits{}, &recordingReporter{}); err != nil {
		t.Fatal(err)
	}
	assertNoSpools(t, ws)

	wb, err := writeback(context.Background(), fetch, ws, m, r, "run2", map[string]bool{}, nil, WritebackLimits{}, &recordingReporter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.Applied) != 2 {
		t.Fatalf("applied %v", wb.Applied)
	}
	for p, s := range remote {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))); err != nil || string(b) != s {
			t.Errorf("%s: %d bytes, %v", p, len(b), err)
		}
	}
	if e := m.ByPath["gen/out.txt"]; e.Size != int64(len(remote["gen/out.txt"])) {
		t.Errorf("manifest size %d", e.Size)
	}
	assertNoSpools(t, ws)
}

// A pull whose archive turns out corrupt writes nothing, and a file the
// runner sends twice is written once.
func TestPullAppliesOnlyAVerifiedArchive(t *testing.T) {
	dir := gitRepo(t, map[string]string{".gitignore": "out/\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	archive := gz(t, rawTar(t, []tarEntry{{name: "out/a", data: "first"}, {name: "out/a", data: "second"}, {name: "out/b", data: "b"}}))
	listed := []string{"out/a", "out/b"}

	bad := append([]byte(nil), archive...)
	bad[len(bad)-8] ^= 0xff
	if _, err := applyPull(context.Background(), ws, dir, []string{"out"}, nil, false, listed, bytes.NewReader(bad), &recordingReporter{}, ""); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if _, err := os.Lstat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("a corrupt archive wrote files (%v)", err)
	}
	assertNoSpools(t, ws)

	n, err := applyPull(context.Background(), ws, dir, []string{"out"}, nil, false, listed, bytes.NewReader(archive), &recordingReporter{}, "")
	if err != nil || n != 2 {
		t.Fatalf("pulled %d, %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out", "a")); string(b) != "first" {
		t.Errorf("out/a = %q, want the first copy", b)
	}
	assertNoSpools(t, ws)
}

// hookWriter runs onHeader once, after the tar header has been written.
type hookWriter struct {
	w        io.Writer
	onHeader func()
}

func (h *hookWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	if h.onHeader != nil {
		h.onHeader()
		h.onHeader = nil
	}
	return n, err
}

// A file that grows after its header is sent is cut at the header's size
// (and hashed as sent); one that shrinks fails, as a re-run.
func TestAddLocalTarSendsWhatTheHeaderSays(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&hookWriter{w: &buf, onHeader: func() {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(" and more")
		f.Close()
	}})
	e, err := addLocalTar(tw, p, "f")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != fsid.Bytes([]byte("original")) || e.Size != 8 {
		t.Fatalf("entry %+v, want the 8 bytes sent", e)
	}
	tw = tar.NewWriter(&hookWriter{w: io.Discard, onHeader: func() { os.Truncate(p, 3) }})
	if _, err := addLocalTar(tw, p, "f"); err == nil || !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("shrunk file: got %v", err)
	}
}

// addLocalTar streams a file and returns the entry for exactly the bytes in
// the tar.
func TestAddLocalTarHashesWhatItSends(t *testing.T) {
	dir := t.TempDir()
	data := strings.Repeat("0123456789", 200<<10)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte(data), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	e, err := addLocalTar(tw, filepath.Join(dir, "f"), "f")
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if e.ID != fsid.Bytes([]byte(data)) || e.Size != int64(len(data)) || e.Mode != fsid.ModeExec {
		t.Fatalf("entry %+v", e)
	}
	tr := tar.NewReader(&buf)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(tr); string(b) != data {
		t.Fatal("tar content differs from the file")
	}
}

func assertNoSpools(t *testing.T, ws *Workspace) {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(conflictBase(ws), "spool"))
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("spools left behind: %v", names)
	}
}

func manifestOf(t *testing.T, dir string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Build(context.Background(), manifest.Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
