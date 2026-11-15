package helper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// A file the run creates with a backslash in its name is reported, so it
// comes back like any other (#92).
func TestSnapshotReportsBackslashNames(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip(`\ separates paths here`)
	}
	o, _ := runExec(t, protocol.Exec{
		Shell:    `mkdir -p src && printf w > 'src/back\slash.txt' && printf x > 'plain.txt'`,
		Snapshot: true,
	}, "")
	if o.res.ExitCode != 0 {
		t.Fatalf("result %+v, stderr %q", o.res, o.stderr)
	}
	var created []string
	for _, c := range o.res.Created {
		created = append(created, c.Path)
	}
	if got := strings.Join(created, " "); !strings.Contains(got, `src/back\slash.txt`) || !strings.Contains(got, "plain.txt") {
		t.Errorf("created %q, want src/back\\slash.txt and plain.txt", got)
	}
}

// An uploaded file with a backslash in its name, or under a directory with
// one, lands in the workspace under that name (#92).
func TestExtractBackslashNames(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip(`\ separates paths here`)
	}
	local := t.TempDir()
	names := []string{`src/loc\al.txt`, `odd\dir/x.txt`}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	need := map[string]bool{}
	for _, n := range names {
		writeFile(t, local, n, n)
		if err := addTar(tw, filepath.Join(local, n), n); err != nil {
			t.Fatal(err)
		}
		need[n] = true
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
	if n, _, _, err := s.extract(&buf, need, nil); err != nil || n != len(names) {
		t.Fatalf("extract wrote %d, %v", n, err)
	}
	for _, n := range names {
		if b, err := os.ReadFile(filepath.Join(s.workspace, n)); err != nil || string(b) != n {
			t.Errorf("%s: %q, %v", n, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.workspace, "odd", "dir")); err == nil {
		t.Error(`odd\dir was split into directories`)
	}
}
