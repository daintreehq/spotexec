package manifest

import (
	"context"
	"os"
	"path/filepath"
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

// An untracked .gitattributes applies before it is staged: a clean filter
// it names makes the index blob differ from the worktree bytes, so the
// manifest must hash the worktree.
func TestUntrackedGitattributesDisablesIndexIDs(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "config", "filter.up.clean", "tr a-z A-Z")
	gitIn(t, root, "config", "filter.up.smudge", "cat")
	writeTree(t, root, "a.txt")
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.txt filter=up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "--", "a.txt")
	m, err := Build(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.ByPath["a.txt"].ID, fsid.Bytes([]byte("a.txt")); got != want {
		t.Fatalf("manifest ID %s, want worktree bytes' ID %s", got, want)
	}
}
