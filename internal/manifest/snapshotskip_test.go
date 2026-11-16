package manifest

import (
	"reflect"
	"testing"
)

// The post-run walk skips what the manifest leaves out, anchored the same
// way: target and cache_dirs only at the root, .spotexec/ not at all (#156).
func TestSnapshotSkipAnchorsRootDirs(t *testing.T) {
	got := SnapshotSkip([]string{"build", "./dist", "/out/", "packages/*/node_modules", "", "./"})
	want := append(append([]string{}, HeavyDirs...), "/build", "/dist", "/out", "/packages/*/node_modules")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SnapshotSkip = %v, want %v", got, want)
	}
	hasRootTarget := false
	for _, k := range got {
		if k == "target" || k == ".spotexec" || k == "/.spotexec" {
			t.Fatalf("%q skipped: %v", k, got)
		}
		hasRootTarget = hasRootTarget || k == "/target"
	}
	if !hasRootTarget {
		t.Fatalf("/target not skipped: %v", got)
	}
	if len(HeavyDirs) == 0 || &got[0] == &HeavyDirs[0] {
		t.Fatal("SnapshotSkip must copy HeavyDirs, not append to it")
	}
}
