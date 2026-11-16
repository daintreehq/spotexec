package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// configGuard follows config symlinks as the filesystem does.
func TestConfigGuardFollowsLinks(t *testing.T) {
	type tree struct {
		dirs  []string
		links map[string]string // path -> target; "@root" is replaced by the root as spelled
	}
	for name, tc := range map[string]struct {
		tree
		yes, no []string
	}{
		"file link":                      {tree{nil, map[string]string{".spotexec.toml": "conf/s.toml"}}, []string{"conf/s.toml", "conf/s.toml/x", "conf"}, []string{"conf/other.toml", "src/main.go"}},
		"dot-dot after a directory link": {tree{[]string{"nested/dir"}, map[string]string{".spotexec.toml": "cfg/../x.toml", "cfg": "nested/dir"}}, []string{"nested/x.toml", "cfg"}, []string{"x.toml", "nested/dir/a"}},
		"chain of directory links":       {tree{[]string{"real", "l1"}, map[string]string{"l1/.spotexec.toml": "../c1/x.toml", "c1": "bridge", "bridge": "real"}}, []string{"c1", "bridge", "real/x.toml"}, []string{"real/y.toml", "l1/other"}},
		"missing directory":              {tree{[]string{"real2"}, map[string]string{".spotexecignore": "c2/new/ign", "c2": "real2"}}, []string{"real2/new/ign", "real2/new", "c2"}, []string{"real2/other"}},
		"absolute target":                {tree{nil, map[string]string{"sub/.spotexec.toml": "@root/abs.toml"}}, []string{"abs.toml"}, []string{"sub/x"}},
		"link to the root":               {tree{nil, map[string]string{".spotexec": "."}}, []string{"setup.sh", "src/main.go"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir() // unresolved: /var on macOS is /private/var
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var links []string
			for l, target := range tc.links {
				full := filepath.Join(root, filepath.FromSlash(l))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(strings.ReplaceAll(target, "@root", root), full); err != nil {
					t.Fatal(err)
				}
				if spotexecConfigPath(l) {
					links = append(links, l)
				}
			}
			guard := configGuard(root, links)
			for _, p := range tc.yes {
				if !guard(p) {
					t.Errorf("%s not protected", p)
				}
			}
			for _, p := range tc.no {
				if guard(p) {
					t.Errorf("%s protected", p)
				}
			}
		})
	}
}

// Without a manifest (pull), nested config links are found in the worktree,
// Git or not.
func TestConfigLinksFindsNestedLinks(t *testing.T) {
	for _, git := range []bool{false, true} {
		dir := t.TempDir()
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../out/config.toml", filepath.Join(dir, "sub", ".spotexec.toml")); err != nil {
			t.Fatal(err)
		}
		if git {
			for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
		}
		ws := &Workspace{Root: dir, Git: git, Cwd: "."}
		if !configGuard(dir, configLinks(context.Background(), ws, nil, true))("out/config.toml") {
			t.Errorf("git %v: out/config.toml not protected", git)
		}
		if configGuard(dir, configLinks(context.Background(), ws, nil, false))("out/config.toml") {
			t.Errorf("git %v: found without looking", git)
		}
	}
}
