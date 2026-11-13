package dockerignore

import "testing"

func TestExcluded(t *testing.T) {
	m, err := New([]byte("# comment\nnode_modules\ndist/\n!dist/keep.txt\n*\n!package.json\n!src\n**/*.log\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"package.json":        false,
		"src/index.ts":        false,
		"README.md":           true,
		"node_modules/x/a.js": true,
		"src/debug.log":       true,
		".dockerignore":       false,
	}
	for p, want := range cases {
		if got := m.Excluded(p); got != want {
			t.Errorf("%s: excluded=%v, want %v", p, got, want)
		}
	}
	none, _ := New(nil)
	if none.Excluded("anything") {
		t.Error("empty rules excluded a path")
	}
}

// BuildKit walks directories carrying parent results: a repeated pattern for
// an already-excluded parent doesn't re-exclude a re-included child.
func TestWalkSemantics(t *testing.T) {
	m, _ := New([]byte("foo\n!foo/bar\nfoo\n"))
	if m.Excluded("foo/bar") {
		t.Error("foo/bar should be kept, as in a BuildKit walk")
	}
	if !m.Excluded("foo/baz") {
		t.Error("foo/baz should be excluded")
	}
}
