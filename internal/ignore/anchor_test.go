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
