package helper

import (
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
