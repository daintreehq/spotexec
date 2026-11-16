package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
)

// spooled is a fetched regular file holding data, spooled as a fetch would.
func spooled(t testing.TB, data string, mode uint32) remoteFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spooled")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return remoteFile{spool: p, size: int64(len(data)), mode: mode, id: fsid.Bytes([]byte(data))}
}
