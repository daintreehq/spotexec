package helper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// A sync tar that carries one needed file twice fails at the repeat: it must
// not stand in for another needed file that never came.
func TestExtractRejectsARepeatedEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, s := range []string{"one", "two"} {
		if err := tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: int64(len(s)), Mode: 0o644}); err != nil {
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
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	s.loadStored()
	n, _, _, err := s.extract(&buf, map[string]bool{"a": true, "b": true}, nil)
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("extract wrote %d, err %v; want the repeat of a refused", n, err)
	}
}

// Overlapping pull patterns (pull_artifacts and --pull) list and send each
// file once, streamed whole.
func TestPullSendsEachFileOnce(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", 3<<20)
	writeFile(t, s.workspace, "dist/app.js", big)
	writeFile(t, s.workspace, "dist/sub/b.txt", "b")
	var wire bytes.Buffer
	s.c = protocol.NewConn(strings.NewReader(""), &wire)
	if err := s.pull(protocol.Pull{Patterns: []string{"dist", "dist/*", "dist/app.js", "dist"}}); err != nil {
		t.Fatal(err)
	}
	c := protocol.NewConn(&wire, nil)
	f, err := c.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	var need protocol.Need
	if err := env.Unmarshal(&need); err != nil {
		t.Fatal(err)
	}
	want := []string{"dist/app.js", "dist/sub/b.txt"}
	if !reflect.DeepEqual(need.Paths, want) {
		t.Fatalf("listed %v, want %v", need.Paths, want)
	}
	zr, err := gzip.NewReader(&protocol.BlobReader{Next: c.ReadFrame})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var sent []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "dist/app.js" && string(b) != big {
			t.Errorf("dist/app.js: %d bytes, want %d", len(b), len(big))
		}
		sent = append(sent, h.Name)
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

// addTar sends a file that vanished as nothing, not as an error.
func TestAddTarSkipsAVanishedFile(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := addTar(tw, filepath.Join(t.TempDir(), "gone"), "gone"); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tar.NewReader(&buf).Next(); err != io.EOF {
		t.Fatalf("an entry for a vanished file (%v)", err)
	}
}
