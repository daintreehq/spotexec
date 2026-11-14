package ignore

import "testing"

func TestAnchoredTarget(t *testing.T) {
	m := New("/target/", ".git", ".spotexec-tmp-*")
	for p, want := range map[string]bool{
		"target/debug/x":            true,
		"internal/target/x_test.go": false,
		".git":                      true,
		"sub/.git":                  true,
		".spotexec-tmp-123":         true,
	} {
		if got := m.Match(p, false); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	if !m.Match("target", true) {
		t.Error("top-level target dir should match")
	}
}

func TestMatchBelow(t *testing.T) {
	m := New("/target/", ".spotexec", "node_modules/")
	for _, c := range []struct {
		p    string
		skip int
		want bool
	}{
		{".spotexec/Dockerfile", 1, false},
		{".spotexec/Dockerfile", 0, true},
		{".spotexec/node_modules/x", 1, true},
		{"docker/target/x", 1, false},
		{"target/x", 0, true},
		{"a/.spotexec/b/c", 1, true},
		{"a/.spotexec/b/c", 2, false},
	} {
		if got := m.MatchBelow(c.p, false, c.skip); got != c.want {
			t.Errorf("MatchBelow(%q, %d) = %v, want %v", c.p, c.skip, got, c.want)
		}
	}
}
