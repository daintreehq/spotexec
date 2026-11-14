package protocol

import (
	"fmt"
	"strings"
)

// IsObjectID reports whether s is a lowercase hex SHA-1 Git object ID.
func IsObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidBranchRef checks a branch ref name (refs/heads/...) by git's rules
// (git check-ref-format), which also makes it a safe relative path inside
// a .git directory.
func ValidBranchRef(r string) error {
	bad := fmt.Errorf("invalid branch ref %q", r)
	rest, ok := strings.CutPrefix(r, "refs/heads/")
	if !ok || rest == "" || strings.Contains(r, "..") || strings.Contains(r, "@{") ||
		strings.HasSuffix(r, "/") || strings.HasSuffix(r, ".") {
		return bad
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return bad
		}
	}
	for _, comp := range strings.Split(r, "/") {
		if comp == "" || strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return bad
		}
	}
	return nil
}
