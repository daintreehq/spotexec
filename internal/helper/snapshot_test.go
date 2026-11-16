package helper

import (
	"reflect"
	"sort"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Skip entries anchored at the root (a leading "/" or "./") skip only that
// path, as the manifest anchors /target/ and cache_dirs; a bare name skips
// the directory at any depth (#156). Files a run creates in a nested
// target/, build/ or dist/ source directory, or in .spotexec/, are observed.
func TestSnapshotSkipAnchoring(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.workspace, "go.mod", "module x\n")
	s.loadStored()
	skip := []string{".git", "node_modules", "/target", "/build", "./dist", "/out/", "/gen/*"}
	pre := s.takeSnapshot(skip)

	skipped := []string{
		"target/debug/app",
		"build/out.o",
		"dist/app.js",
		"out/bundle.js",
		"gen/a/b.txt",
		"node_modules/dep/index.js",
		"pkg/node_modules/dep/index.js",
	}
	observed := []string{
		".spotexec/Dockerfile.new",
		"cmd/build/main.go",
		"gen/top.txt",
		"internal/target/gen.go",
		"web/dist/app.js",
		"web/out/bundle.js",
	}
	for _, p := range append(append([]string{}, skipped...), observed...) {
		writeFile(t, s.workspace, p, "made remotely\n")
	}

	_, _, created, _, _, _ := s.diff(pre, s.takeSnapshot(skip))
	var got []string
	for _, c := range created {
		got = append(got, c.Path)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, observed) {
		t.Fatalf("created = %v, want %v", got, observed)
	}
}
