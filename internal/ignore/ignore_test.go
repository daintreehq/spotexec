package ignore

import "testing"

func TestMatch(t *testing.T) {
	m := New(".env*", "*.pem", "tmp/", "/build", "docs/**/*.md", "!docs/keep.md", "a/b")
	cases := []struct {
		p    string
		dir  bool
		want bool
	}{
		{".env", false, true},
		{"sub/.env.local", false, true},
		{"x.pem", false, true},
		{"tmp", true, true},
		{"tmp", false, false},
		{"tmp/x.txt", false, true},
		{"src/tmp/x.txt", false, true},
		{"build", true, true},
		{"src/build", true, false},
		{"docs/a/b.md", false, true},
		{"docs/b.md", false, true},
		{"docs/keep.md", false, false},
		{"a/b", false, true},
		{"x/a/b", false, false},
		{"src/main.go", false, false},
	}
	for _, c := range cases {
		if got := m.Match(c.p, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.p, c.dir, got, c.want)
		}
	}
}
