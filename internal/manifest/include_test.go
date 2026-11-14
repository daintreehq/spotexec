package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func paths(m *Manifest) string {
	var ps []string
	for _, e := range m.Entries {
		ps = append(ps, e.Path)
	}
	sort.Strings(ps)
	return strings.Join(ps, " ")
}

// The container's Dockerfile and build context sync without Git: a
// non-Git project, and a Git one with sync.source = "directory".
func TestIncludeSyncsContainerInputsWithoutGit(t *testing.T) {
	include := []string{".spotexec/Dockerfile", ".spotexec/Dockerfile.dockerignore", ".spotexec"}
	files := []string{
		"src/a.txt", ".spotexec/Dockerfile", ".spotexec/Dockerfile.dockerignore", ".spotexec/entry.sh",
		".spotexec/node_modules/x.js", ".spotexec/conflicts/run1/src/a.txt", ".spotexec/writeback.lock",
		".spotexec/Conflicts/run2/b.txt", ".spotexec/WriteBack.lock",
		"node_modules/y.js",
	}
	want := ".spotexec/Dockerfile .spotexec/Dockerfile.dockerignore .spotexec/entry.sh src/a.txt"
	for _, source := range []string{"", "directory"} {
		root := t.TempDir()
		writeTree(t, root, files...)
		if source == "directory" {
			gitIn(t, root, "init", "-q")
		}
		m, err := Build(context.Background(), Options{Root: root, Source: source, Include: include})
		if err != nil {
			t.Fatal(err)
		}
		if m.Git {
			t.Fatalf("source %q: walked as git", source)
		}
		if got := paths(m); got != want {
			t.Errorf("source %q: synced %q, want %q", source, got, want)
		}
		// Without the container, .spotexec/ stays out.
		m, err = Build(context.Background(), Options{Root: root, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		if got := paths(m); got != "src/a.txt" {
			t.Errorf("source %q without include: synced %q", source, got)
		}
	}
}

// In a Git worktree an untracked Dockerfile under .spotexec/ syncs without
// `git add`, and gitignored build inputs are still sent (but nothing else
// that Git ignores).
func TestIncludeUntrackedAndIgnoredInGit(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	writeTree(t, root, "src/a.txt", ".spotexec/Dockerfile", ".spotexec/setup.sh", "docker/Dockerfile", "docker/files/x.conf", "out/big.bin")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("docker/\nout/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Build(context.Background(), Options{Root: root, Include: []string{".spotexec/Dockerfile", ".spotexec/Dockerfile.dockerignore", ".spotexec"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(m), ".gitignore .spotexec/Dockerfile .spotexec/setup.sh src/a.txt"; got != want {
		t.Errorf("synced %q, want %q", got, want)
	}
	m, err = Build(context.Background(), Options{Root: root, Include: []string{"docker/Dockerfile", "docker/Dockerfile.dockerignore", "docker"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(m), ".gitignore docker/Dockerfile docker/files/x.conf src/a.txt"; got != want {
		t.Errorf("synced %q, want %q", got, want)
	}
	// .spotexec/ itself gitignored: its build inputs still sync.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".spotexec/\ndocker/\nout/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err = Build(context.Background(), Options{Root: root, Include: []string{".spotexec/Dockerfile", ".spotexec/Dockerfile.dockerignore", ".spotexec"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(m), ".gitignore .spotexec/Dockerfile .spotexec/setup.sh src/a.txt"; got != want {
		t.Errorf("gitignored .spotexec/: synced %q, want %q", got, want)
	}
}

// Lifting an excluded directory keeps every other built-in pattern's
// meaning: /target/ stays anchored at the repository root.
func TestIncludeKeepsAnchoredBuiltins(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "docker/Dockerfile", "docker/target/templates/config", "docker/node_modules/m.js", "target/debug/bin")
	m, err := Build(context.Background(), Options{Root: root, Include: []string{"docker/Dockerfile", "docker"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(m), "docker/Dockerfile docker/target/templates/config"; got != want {
		t.Errorf("synced %q, want %q", got, want)
	}
}

// [sync].exclude still wins over Include, and so does NeverSync.
func TestIncludeRespectsExcludes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, ".spotexec/Dockerfile", ".spotexec/conflicts/x")
	m, err := Build(context.Background(), Options{Root: root, Exclude: []string{".spotexec/"}, Include: []string{".spotexec/Dockerfile", ".spotexec"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(m); got != "" {
		t.Errorf("synced %q despite [sync].exclude", got)
	}
	m, err = Build(context.Background(), Options{Root: root, Include: []string{".spotexec/conflicts/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(m); got != "" {
		t.Errorf("synced spotexec state %q", got)
	}
}

// Files a run creates in the build context come back like any other.
func TestCheckIgnoredHonoursInclude(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, ".spotexec/Dockerfile")
	m, err := Build(context.Background(), Options{Root: root, Include: []string{".spotexec/Dockerfile", ".spotexec"}})
	if err != nil {
		t.Fatal(err)
	}
	ign, err := m.CheckIgnored(context.Background(), []string{".spotexec/gen.sh", ".spotexec/conflicts/r/x", "node_modules/z", "src/new.go"})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{".spotexec/gen.sh": false, ".spotexec/conflicts/r/x": true, "node_modules/z": true, "src/new.go": false} {
		if ign[p] != want {
			t.Errorf("CheckIgnored(%q) = %v, want %v", p, ign[p], want)
		}
	}
}
