package fsid

import (
	"path/filepath"
	"testing"
)

func TestCleanRel(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"a.txt", "a.txt"},
		{"src/a.txt", filepath.Join("src", "a.txt")},
		{"src/.gitignore", filepath.Join("src", ".gitignore")},
		{"src/.github/x", filepath.Join("src", ".github", "x")},
	} {
		got, err := CleanRel(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", "/a", "a//b", "a/", "./a", "a/./b", "..", "a/../b", "a/..",
		".git", ".git/config", "sub/.GIT/hooks/x", "a\x00b",
	} {
		if got, err := CleanRel(in); err == nil {
			t.Errorf("CleanRel(%q) = %q, want an error", in, got)
		}
	}
}

// A backslash is an ordinary filename byte on macOS and Linux, so paths
// holding one sync like any other; only where it separates paths (Windows)
// could it hide a "..", and there it is refused.
func TestCleanRelBackslash(t *testing.T) {
	for _, in := range []string{`src/back\slash.txt`, `loc\al.txt`, `a\..\b`, `\`, `dir\/x`} {
		got, err := CleanRel(in)
		if filepath.Separator == '\\' {
			if err == nil {
				t.Errorf("CleanRel(%q) = %q, want an error where \\ separates paths", in, got)
			}
			continue
		}
		if err != nil || got != in {
			t.Errorf("CleanRel(%q) = %q, %v; want it unchanged", in, got, err)
		}
	}
}

func TestSafeJoinBackslash(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip(`\ separates paths here`)
	}
	root := t.TempDir()
	got, err := SafeJoin(root, `src/back\slash.txt`)
	if err != nil || got != filepath.Join(root, "src", `back\slash.txt`) {
		t.Errorf("SafeJoin = %q, %v", got, err)
	}
}
