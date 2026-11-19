package ignore

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// In a .gitignore a trailing ** excludes what is inside a directory, not
// the directory, so a deeper file's negation re-includes a file in it, as
// in Git. .spotexecignore and [sync].exclude keep excluding the directory
// itself, as they always have.
func TestTrailingDoubleStar(t *testing.T) {
	user := New("service/**", "!service/keep.txt", "private/**/")
	for p, want := range map[string]bool{"service/a.txt": true, "service/keep.txt": true, "private/key.pem": true} {
		if got := user.Match(p, false); got != want {
			t.Errorf("user Match(%q) = %v, want %v", p, got, want)
		}
	}
	if !user.Match("service", true) {
		t.Error("user service/** no longer excludes service itself")
	}

	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".gitignore":         "service/**\nprivate/**/\n",
		"service/.gitignore": "!keep.txt\n!sub/\n",
	})
	tr := NewTree(root, ".gitignore")
	for p, want := range map[string]bool{
		"service/keep.txt": false, "service/a.txt": true, "service/x/keep.txt": true,
		"private/key.pem": false, "private/sub/key.pem": true,
	} {
		if got, err := tr.Match(p, false); err != nil || got != want {
			t.Errorf("Tree.Match(%q) = %v, %v, want %v", p, got, err, want)
		}
	}
	// Re-included as a directory, but its files are still inside service/**.
	if got, err := tr.Match("service/sub", true); err != nil || got {
		t.Errorf("Tree.Match(service/sub/) = %v, %v", got, err)
	}
	if got, err := tr.Match("service/sub/x", false); err != nil || !got {
		t.Errorf("Tree.Match(service/sub/x) = %v, %v", got, err)
	}
}

// In a .gitignore, Git's negated class [!...] means "not", as [^...]
// does; only a "!" first in a class negates it. .spotexecignore and
// [sync].exclude keep reading "!" as a member.
func TestBracketNegation(t *testing.T) {
	m := &Matcher{git: true}
	m.Add("keys/[!x].pem", "\\[!y].txt", "lit/[[!].pem", "dir[!a]/*.key")
	for p, want := range map[string]bool{
		"keys/a.pem": true, "keys/x.pem": false, "keys/!.pem": true, "keys/ab.pem": false,
		"[!y].txt": true, "a.txt": false,
		"lit/!.pem": true, "lit/[.pem": true, "lit/a.pem": false,
		"dirb/k.key": true, "dira/k.key": false,
	} {
		if got := m.Match(p, false); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	user := New("keys/[!x].pem")
	for p, want := range map[string]bool{"keys/x.pem": true, "keys/!.pem": true, "keys/a.pem": false} {
		if got := user.Match(p, false); got != want {
			t.Errorf("user Match(%q) = %v, want %v", p, got, want)
		}
	}
}

// A trailing slash asks about a directory, and only the ignore files above
// it apply: one inside it that can't be read doesn't matter.
func TestTreeTrailingSlash(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{".gitignore": "out/\n", "service/out/.gitignore/x": ""})
	tr := NewTree(root, ".gitignore")
	if got, err := tr.Match("service/out/", false); err != nil || !got {
		t.Fatalf("Match(service/out/) = %v, %v", got, err)
	}
	if got, err := tr.Match("service/", false); err != nil || got {
		t.Fatalf("Match(service/) = %v, %v", got, err)
	}
	// A trailing slash is the directory itself: an unreadable file inside
	// it doesn't apply to it.
	writeFiles(t, root, map[string]string{"open/.gitignore/x": ""})
	if got, err := tr.Match("open/", false); err != nil || got {
		t.Fatalf("Match(open/) = %v, %v", got, err)
	}
	// Below an excluded directory nothing is re-included, so its own
	// file is never needed.
	if got, err := NewTree(root, ".gitignore").Match("service/out/x", false); err != nil || !got {
		t.Fatalf("Match(service/out/x) = %v, %v", got, err)
	}
	// Below a directory that isn't excluded, its unreadable file fails.
	writeFiles(t, root, map[string]string{"keep/.gitignore/x": ""})
	if _, err := NewTree(root, ".gitignore").Match("keep/x", false); err == nil || !strings.Contains(err.Error(), filepath.Join(root, "keep", ".gitignore")) {
		t.Fatalf("Match(keep/x) error = %v", err)
	}
}

// An ignore file that exists but can't be read whole fails with its path:
// a FIFO (without hanging), an over-long line, a directory.
func TestTreeReadErrors(t *testing.T) {
	cases := map[string]func(t *testing.T, p string){
		"fifo": func(t *testing.T, p string) {
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Skip(err)
			}
		},
		"long line": func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("a\n"+strings.Repeat("x", 1<<20)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, make := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"sub/a.txt": ""})
			p := filepath.Join(root, "sub", ".gitignore")
			make(t, p)
			tr := NewTree(root, ".gitignore")
			for range 2 { // never cached as "no file"
				if _, err := tr.Match("sub/a.txt", false); err == nil || !strings.Contains(err.Error(), p) {
					t.Fatalf("Match error = %v, want one naming %s", err, p)
				}
			}
			if err := New().AddFile(p); err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("AddFile error = %v, want one naming %s", err, p)
			}
		})
	}
}

// What lies beyond a symlinked directory is not the project's: its ignore
// files are not read. A symlinked .gitignore below the root is skipped, as
// Git skips it; the root's is followed, as spotexec always has; a dangling
// one is absent.
func TestTreeSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFiles(t, outside, map[string]string{".gitignore": "*.txt\n", "a.txt": "", "rules": "*.log\n", "rootrules": "*.tmp\n"})
	writeFiles(t, root, map[string]string{"real/a.log": "", "real/b.tmp": ""})
	for link, target := range map[string]string{
		"link":            outside,
		"real/.gitignore": filepath.Join(outside, "rules"),
		".gitignore":      filepath.Join(outside, "rootrules"),
		"gone/.gitignore": filepath.Join(outside, "missing"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	tr := NewTree(root, ".gitignore")
	for p, want := range map[string]bool{"link/a.txt": false, "real/a.log": false, "real/b.tmp": true, "gone/x": false} {
		if got, err := tr.Match(p, false); err != nil || got != want {
			t.Errorf("Match(%q) = %v, %v, want %v", p, got, err, want)
		}
	}

	dangling := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(dangling, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if got, err := NewTree(dangling, ".gitignore").Match("x.txt", false); err != nil || got {
		t.Errorf("dangling root: %v, %v", got, err)
	}
}
