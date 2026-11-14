package client

import "testing"

func TestReservedPath(t *testing.T) {
	for p, want := range map[string]bool{
		".git/hooks/pre-commit": true,
		".GIT/hooks/pre-commit": true,
		"sub/.Git/config":       true,
		".spotexec/x":           true,
		".SpotExec/x":           true,
		"src/git/x":             false,
		".gitignore":            false,
		"a/.github/workflows":   false,
	} {
		if got := reservedPath(p, false); got != want {
			t.Errorf("reservedPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestPullMatches(t *testing.T) {
	pats := []string{"coverage", "./dist/", "reports/*.xml"}
	for p, want := range map[string]bool{
		"coverage/lcov.info":  true,
		"dist/app.js":         true,
		"reports/junit.xml":   true,
		"reports/x/junit.xml": false,
		"src/main.go":         false,
		"Makefile":            false,
		"coverage2/x":         false,
	} {
		if got := pullMatches(pats, p); got != want {
			t.Errorf("pullMatches(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestReservedSpotexecOnlyOutsideGit(t *testing.T) {
	if reservedPath(".spotexec/Dockerfile", true) {
		t.Error(".spotexec/ is ordinary in a git worktree")
	}
	if !reservedPath(".GIT/config", true) {
		t.Error(".git is always reserved")
	}
}
