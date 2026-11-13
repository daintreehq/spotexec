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
