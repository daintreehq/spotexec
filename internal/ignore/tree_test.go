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

// A trailing ** excludes what is inside a directory, not the directory,
// so a deeper file's negation re-includes a file in it, as in Git.
func TestTrailingDoubleStar(t *testing.T) {
	m := New("service/**", "!service/keep.txt")
	for p, want := range map[string]bool{"service/a.txt": true, "service/keep.txt": false, "service/sub/x": true} {
		if got := m.Match(p, false); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	if m.Match("service", true) {
		t.Error("service/** excluded service itself")
	}

	root := t.TempDir()
	writeFiles(t, root, map[string]string{".gitignore": "service/**\n", "service/.gitignore": "!keep.txt\n"})
	tr := NewTree(root, ".gitignore")
	for p, want := range map[string]bool{"service/keep.txt": false, "service/a.txt": true, "service/sub/keep.txt": true} {
		if got, err := tr.Match(p, false); err != nil || got != want {
			t.Errorf("Tree.Match(%q) = %v, %v, want %v", p, got, err, want)
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
			if _, err := NewTree(root, ".gitignore").Match("sub/a.txt", false); err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("Match error = %v, want one naming %s", err, p)
			}
			if err := New().AddFile(p); err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("AddFile error = %v, want one naming %s", err, p)
			}
		})
	}
}

// What lies beyond a symlinked directory is not the project's: its ignore
// files are not read. A symlinked ignore file itself is followed, and a
// dangling one is absent.
func TestTreeSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFiles(t, outside, map[string]string{".gitignore": "*.txt\n", "rules": "*.log\n"})
	writeFiles(t, root, map[string]string{"real/a.log": ""})
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "rules"), filepath.Join(root, "real", ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	tr := NewTree(root, ".gitignore")
	if got, err := tr.Match("link/a.txt", false); err != nil || got {
		t.Errorf("Match(link/a.txt) = %v, %v", got, err)
	}
	if got, err := tr.Match("real/a.log", false); err != nil || !got {
		t.Errorf("Match(real/a.log) = %v, %v", got, err)
	}
}
