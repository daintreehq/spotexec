package manifest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
)

// A Dockerfile in a project subdirectory (its directory the build context)
// doesn't force-sync what Git ignores there: secrets, build outputs and a
// nested Rust target/ stay local, Git or not. The Dockerfile itself syncs.
func TestSubdirContainerRespectsGitignore(t *testing.T) {
	include := []string{"services/api/Dockerfile", "services/api/Dockerfile.dockerignore", "services/api", "services/api"}
	files := []string{
		"services/api/Dockerfile", "services/api/main.go", "services/api/secrets.json",
		"services/api/dist/app.js", "services/api/target/debug/api",
	}
	gitignore := "services/api/secrets.json\ndist/\ntarget/\n"
	want := ".gitignore services/api/Dockerfile services/api/main.go"
	for _, git := range []bool{false, true} {
		root := t.TempDir()
		writeTree(t, root, files...)
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(gitignore), 0o644); err != nil {
			t.Fatal(err)
		}
		if git {
			gitIn(t, root, "init", "-q")
		}
		m, err := Build(context.Background(), Options{Root: root, Include: include})
		if err != nil {
			t.Fatal(err)
		}
		if m.Git != git {
			t.Fatalf("git = %v, want %v", m.Git, git)
		}
		if got := paths(m); got != want {
			t.Errorf("git %v: synced %q, want %q", git, got, want)
		}
	}
}

// The same for a container.context in a subdirectory while the Dockerfile
// lives under .spotexec/: the context follows the usual rules, the
// Dockerfile's own directory still syncs whole.
func TestSubdirContextRespectsGitignore(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	writeTree(t, root, ".spotexec/Dockerfile", ".spotexec/setup.sh", "app/main.go", "app/.env", "app/build/out.bin")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".spotexec/\n.env\nbuild/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Build(context.Background(), Options{Root: root, Include: []string{".spotexec/Dockerfile", ".spotexec/Dockerfile.dockerignore", ".spotexec", "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(m), ".gitignore .spotexec/Dockerfile .spotexec/setup.sh app/main.go"; got != want {
		t.Errorf("synced %q, want %q", got, want)
	}
}

// A context inside a built-in excluded directory (the root target/) still
// syncs its source, Git or not, but not what Git ignores there or a
// built-in excluded directory inside it.
func TestContextUnderBuiltinExcludeRespectsGitignore(t *testing.T) {
	include := []string{"target/api/Dockerfile", "target/api/Dockerfile.dockerignore", "target/api/.dockerignore", "target/api", "target/api"}
	files := []string{"target/api/Dockerfile", "target/api/main.go", "target/api/secret.json", "target/api/node_modules/m.js", "target/debug/bin"}
	want := ".gitignore target/api/Dockerfile target/api/main.go"
	for _, source := range []string{"", "directory"} {
		for _, git := range []bool{false, true} {
			if source == "directory" && !git {
				continue
			}
			root := t.TempDir()
			writeTree(t, root, files...)
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.json\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if git {
				gitIn(t, root, "init", "-q")
			}
			m, err := Build(context.Background(), Options{Root: root, Source: source, Include: include})
			if err != nil {
				t.Fatal(err)
			}
			if got := paths(m); got != want {
				t.Errorf("git %v source %q: synced %q, want %q", git, source, got, want)
			}
		}
	}
}

// A .gitattributes applies before it is staged, and even when Git ignores
// it, to every file below it: a clean filter it names makes the index blob
// differ from the worktree bytes, so the manifest must hash the worktree.
// So does the global attributes file, wherever Git finds it.
func TestUnstagedGitattributesDisablesIndexIDs(t *testing.T) {
	for _, tc := range []struct {
		name, attrs, gitignore string
		config                 string // core.attributesFile; ~ is the temp home
		xdg                    bool   // XDG_CONFIG_HOME set
		gitDir                 string // a separate git dir in the temp home
	}{
		{name: "untracked", attrs: ".gitattributes", xdg: true},
		{name: "ancestor", attrs: "sub/.gitattributes", xdg: true},
		{name: "ignored", attrs: "sub/.gitattributes", gitignore: ".gitattributes\n", xdg: true},
		{name: "xdg", attrs: "~xdg/git/attributes", xdg: true},
		{name: "home", attrs: "~/.config/git/attributes"},
		{name: "configured", attrs: "~/attrs", config: "~/attrs", xdg: true},
		{name: "configured-relative", attrs: "conf/attrs", config: "conf/attrs", xdg: true},
		// A name ending in a space is that file, not its trimmed sibling.
		{name: "configured-trailing-space", attrs: "conf/attrs ", config: "conf/attrs ", xdg: true},
		{name: "common-dir-trailing-space", attrs: "~/meta /info/attributes", xdg: true, gitDir: "meta "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.HasSuffix(tc.name, "-trailing-space") {
				t.Skip("Windows paths cannot end in a space")
			}
			root, home := t.TempDir(), t.TempDir()
			xdg := filepath.Join(home, "xdg")
			// Only this test's git configuration applies.
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("XDG_CONFIG_HOME", "")
			if tc.xdg {
				t.Setenv("XDG_CONFIG_HOME", xdg)
			}
			abs := func(p string) string {
				switch {
				case strings.HasPrefix(p, "~xdg/"):
					return filepath.Join(xdg, filepath.FromSlash(p[5:]))
				case strings.HasPrefix(p, "~/"):
					return filepath.Join(home, filepath.FromSlash(p[2:]))
				}
				return filepath.Join(root, filepath.FromSlash(p))
			}
			if tc.gitDir != "" {
				gitIn(t, root, "init", "-q", "--separate-git-dir", filepath.Join(home, tc.gitDir))
			} else {
				gitIn(t, root, "init", "-q")
			}
			gitIn(t, root, "config", "core.autocrlf", "false")
			gitIn(t, root, "config", "filter.up.clean", "tr a-z A-Z")
			gitIn(t, root, "config", "filter.up.smudge", "cat")
			if tc.config != "" {
				c := tc.config
				if strings.HasPrefix(c, "~/") {
					c = abs(c)
				}
				gitIn(t, root, "config", "core.attributesFile", c)
			}
			writeTree(t, root, "sub/deep/a.txt")
			attrs := abs(tc.attrs)
			if err := os.MkdirAll(filepath.Dir(attrs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(attrs, []byte("*.txt filter=up\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.gitignore != "" {
				if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(tc.gitignore), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitIn(t, root, "add", "--", "sub/deep/a.txt")
			want := fsid.Bytes([]byte("sub/deep/a.txt"))
			staged, err := lsStage(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if staged["sub/deep/a.txt"].id == want {
				t.Fatal("the filter didn't apply: the index blob is the worktree bytes")
			}
			// Run from elsewhere: a relative core.attributesFile is the
			// worktree root's, not the caller's.
			t.Chdir(t.TempDir())
			m, err := Build(context.Background(), Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if got := m.ByPath["sub/deep/a.txt"].ID; got != want {
				t.Fatalf("manifest ID %s, want worktree bytes' ID %s", got, want)
			}
		})
	}
}
