package client

import "testing"

func TestReservedPath(t *testing.T) {
	for p, want := range map[string]bool{
		".git/hooks/pre-commit":    true,
		".GIT/hooks/pre-commit":    true,
		"sub/.Git/config":          true,
		".spotexec":                true,
		".spotexec/conflicts/r1/x": true,
		".SpotExec/Conflicts/r1/x": true,
		".spotexec/WRITEBACK.lock": true,
		".spotexec/backup/r1/x":    true,
		".spotexec/Backup/r1/x":    true,
		".spotexec/Dockerfile":     false,
		".spotexec/setup.sh":       false,
		"src/git/x":                false,
		".gitignore":               false,
		"a/.github/workflows":      false,
	} {
		if got := reservedPath(p, false); got != want {
			t.Errorf("reservedPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSpotexecConfigPath(t *testing.T) {
	for p, want := range map[string]bool{
		".spotexec.toml":         true,
		".SpotExec.TOML":         true,
		"sub/dir/.spotexec.toml": true,
		".spotexecignore":        true,
		"sub/.SPOTEXECIGNORE":    true,
		".spotexec":              true,
		".spotexec/Dockerfile":   true,
		".SpotExec/setup.sh":     true,
		"sub/.spotexec/x/y":      true,
		".spotexec.toml.bak":     false,
		"spotexec.toml":          false,
		"spotexec/Dockerfile":    false,
		"docker/Dockerfile":      false,
		"a/.spotexec.toml/b":     true,
		".spotexecignore/x":      true,
		"x.spotexec/y":           false,
	} {
		if got := spotexecConfigPath(p); got != want {
			t.Errorf("spotexecConfigPath(%q) = %v, want %v", p, got, want)
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
