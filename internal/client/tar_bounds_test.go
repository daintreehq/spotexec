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

func TestReadTarRefusesMoreThanItsLimit(t *testing.T) {
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

// A runner that reports a 1-byte file and sends a big one fails the
// write-back before reading the file, and leaves no spool behind.
func TestWritebackRefusesAFileLargerThanReported(t *testing.T) {
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	m := manifestOf(t, dir)
	big := strings.Repeat("A", 8<<20)
	r := protocol.Result{Created: []protocol.Entry{{Path: "bomb.bin", ID: fsid.Bytes([]byte("x")), Mode: fsid.ModeFile, Size: 1}}}
	fetch := func(protocol.Fetch) (io.Reader, error) { return tarOf(t, map[string]string{"bomb.bin": big}), nil }
	_, err := writeback(context.Background(), fetch, ws, m, r, "run1", map[string]bool{}, nil, WritebackLimits{}, &recordingReporter{})
	if err == nil || !strings.Contains(err.Error(), "larger than the") {
		t.Fatalf("got %v, want a size refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "bomb.bin")); !os.IsNotExist(err) {
		t.Fatalf("bomb.bin was written (%v)", err)
	}
	assertNoSpools(t, ws)
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
