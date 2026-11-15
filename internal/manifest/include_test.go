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
		".spotexec/backup/run1/src/a.txt", ".spotexec/Backup/run2/b.txt",
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
		// Without the container, .spotexec/ is an ordinary directory:
		// only spotexec's own state there stays out.
		m, err = Build(context.Background(), Options{Root: root, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		if got := paths(m); got != want {
			t.Errorf("source %q without include: synced %q, want %q", source, got, want)
		}
	}
}

// Project files under .spotexec/ (a [setup] script, helpers) sync like any
// other, Git or not; spotexec's own state there (write-back lock, conflicts,
// backups), at the root or in a nested project, never does.
func TestSpotexecDirSyncsExceptState(t *testing.T) {
	files := []string{
		"src/a.txt", ".spotexec/prep.sh", ".spotexec/lib/helper.sh",
		".spotexec/conflicts/run1/src/a.txt", ".spotexec/backup/run1/src/a.txt", ".spotexec/writeback.lock",
		".SpotExec/Backup/run2/b.txt",
		"sub/.spotexec/prep.sh", "sub/.spotexec/backup/run1/x", "sub/.spotexec/conflicts/run1/x", "sub/.spotexec/writeback.lock",
	}
	want := ".spotexec/lib/helper.sh .spotexec/prep.sh src/a.txt sub/.spotexec/prep.sh"
	for _, git := range []bool{false, true} {
		root := t.TempDir()
		writeTree(t, root, files...)
		if git {
			gitIn(t, root, "init", "-q")
		}
		m, err := Build(context.Background(), Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if m.Git != git {
			t.Fatalf("git = %v, want %v", m.Git, git)
		}
		if got := paths(m); got != want {
			t.Errorf("git %v: synced %q, want %q", git, got, want)
		}
		ign, err := m.CheckIgnored(context.Background(), []string{".spotexec/new.sh", ".spotexec/backup/r/x", "sub/.spotexec/conflicts/r/x", ".SpotExec/Backup/r/y", "Sub/.SPOTEXEC/WriteBack.lock"})
		if err != nil {
			t.Fatal(err)
		}
		for p, want := range map[string]bool{".spotexec/new.sh": false, ".spotexec/backup/r/x": true, "sub/.spotexec/conflicts/r/x": true, ".SpotExec/Backup/r/y": true, "Sub/.SPOTEXEC/WriteBack.lock": true} {
			if ign[p] != want {
				t.Errorf("git %v: CheckIgnored(%q) = %v, want %v", git, p, ign[p], want)
			}
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
	if got, want := paths(m), ".gitignore .spotexec/Dockerfile .spotexec/setup.sh docker/Dockerfile docker/files/x.conf src/a.txt"; got != want {
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
	if got := paths(m); got != ".spotexec/Dockerfile" {
		t.Errorf("synced %q, want only .spotexec/Dockerfile (never spotexec state)", got)
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

// Credential directories never sync from untracked or non-Git sources,
// whatever the config says; a file Git tracks is the user's choice.
func TestCredentialDirsNeverSync(t *testing.T) {
	creds := []string{".aws/credentials", ".ssh/id_ed25519", ".gnupg/private-keys-v1.d/k.key", "sub/.ssh/id_rsa", ".SSH/id_rsa"}
	root := t.TempDir()
	writeTree(t, root, append([]string{"src/a.txt"}, creds...)...)
	// Even an include root (the container's context) can't lift them.
	m, err := Build(context.Background(), Options{Root: root, Include: []string{".ssh", ".aws/credentials"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(m); got != "src/a.txt" {
		t.Errorf("non-git: synced %q", got)
	}
	ign, err := m.CheckIgnored(context.Background(), []string{".ssh/new_key", "src/new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !ign[".ssh/new_key"] || ign["src/new.txt"] {
		t.Errorf("CheckIgnored = %v", ign)
	}

	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", "-f", "--", "sub/.ssh/id_rsa")
	m, err = Build(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(m); got != "src/a.txt sub/.ssh/id_rsa" {
		t.Errorf("git: synced %q, want the tracked file and src/a.txt only", got)
	}
}

// A .spotexec.toml in the home directory doesn't make it a project.
func TestRootIgnoresHomeMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTree(t, home, ".spotexec.toml", "sub/x.txt", "proj/.spotexec.toml")
	sub := filepath.Join(home, "sub")
	if r, git := Root(sub); git || r != sub {
		t.Errorf("Root(sub) = %s, %v; want sub itself", r, git)
	}
	proj := filepath.Join(home, "proj")
	if r, _ := Root(filepath.Join(proj)); r != proj {
		t.Errorf("Root(proj) = %s", r)
	}
}

// A project rooted in (or below) a credential directory syncs nothing
// untracked, whatever the options say.
func TestCredentialRootSyncsNothing(t *testing.T) {
	for _, sub := range []string{".ssh", ".aws/sso", ".GnuPG"} {
		root := filepath.Join(t.TempDir(), sub)
		writeTree(t, root, "id_ed25519", "keys/x")
		for _, source := range []string{"", "directory"} {
			m, err := Build(context.Background(), Options{Root: root, Source: source, Include: []string{"id_ed25519", "keys"}})
			if err != nil {
				t.Fatal(err)
			}
			if got := paths(m); got != "" {
				t.Errorf("%s (source %q): synced %q", sub, source, got)
			}
		}
	}
}

// The same through a symlinked ~/.ssh.
func TestSymlinkedCredentialRootSyncsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	keys := filepath.Join(home, "privatekeys")
	writeTree(t, keys, "id_ed25519")
	if err := os.Symlink(keys, filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	m, err := Build(context.Background(), Options{Root: keys, Include: []string{"id_ed25519"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(m); got != "" {
		t.Errorf("synced %q", got)
	}
}
