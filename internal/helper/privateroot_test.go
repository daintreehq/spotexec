package helper

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// helloSession is a Session whose replies land in out.
func helloSession(t *testing.T) (*Session, *syncBuf) {
	t.Helper()
	out := &syncBuf{}
	s := &Session{c: protocol.NewConn(strings.NewReader(""), out), frames: make(chan protocol.Frame, 1), born: time.Now()}
	t.Cleanup(s.release)
	return s, out
}

// openDir makes a directory with exactly mode m.
func openDir(t *testing.T, path string, m fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, m); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, m); err != nil { // past the umask
		t.Fatal(err)
	}
}

// stubChmod replaces the root's chmod with fn for the test, recording the
// directories and modes it was called with.
func stubChmod(t *testing.T, fn func(f *os.File, m fs.FileMode) error) *[]string {
	t.Helper()
	var calls []string
	chmodRoot = func(f *os.File, m fs.FileMode) error {
		calls = append(calls, f.Name()+" "+m.String())
		return fn(f, m)
	}
	t.Cleanup(func() { chmodRoot = (*os.File).Chmod })
	return &calls
}

func refused(t *testing.T, err error) string {
	t.Helper()
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeWorkRoot {
		t.Fatalf("hello: %v, want a %s error", err, protocol.CodeWorkRoot)
	}
	return pe.msg
}

// A shared work root the runner can write to but not chmod (#312): the
// session is refused before HelloOK, so no workspace is ever accepted under it.
func TestHelloRefusesRootItCannotMakePrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	openDir(t, root, 0o755)
	calls := stubChmod(t, func(f *os.File, _ fs.FileMode) error {
		return &fs.PathError{Op: "chmod", Path: f.Name(), Err: syscall.EPERM}
	})

	s, out := helloSession(t)
	msg := refused(t, s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root}))
	for _, want := range []string{root, "0755", "operation not permitted", "work_root"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q doesn't mention %q", msg, want)
		}
	}
	if len(*calls) != 1 || !strings.HasSuffix((*calls)[0], " -rwx------") {
		t.Fatalf("chmod calls %q, want one to 0700", *calls)
	}
	if n := len(sent(out)); n != 0 {
		t.Fatalf("sent %d bytes before refusing", n)
	}
	if s.layout.Root != "" {
		t.Fatalf("layout set to %q on a refused hello", s.layout.Root)
	}
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err == nil || !strings.Contains(err.Error(), "hello required") {
		t.Fatalf("acquire after the refused hello: %v", err)
	}
	if ents, err := os.ReadDir(root); err != nil || len(ents) != 0 {
		t.Fatalf("refused root holds %v (%v)", ents, err)
	}
}

// A chmod that reports success without making the root 0700 (a filesystem
// that ignores or rewrites modes) is caught by reading the mode back.
func TestHelloChecksTheModeNotTheChmod(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	openDir(t, root, 0o777)
	stubChmod(t, func(f *os.File, _ fs.FileMode) error { return f.Chmod(0o755) })

	s, out := helloSession(t)
	msg := refused(t, s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root}))
	if !strings.Contains(msg, "0755") || strings.Contains(msg, "failed") {
		t.Fatalf("error %q, want the mode read back and no chmod failure", msg)
	}
	if n := len(sent(out)); n != 0 {
		t.Fatalf("sent %d bytes before refusing", n)
	}
}

// A root the runner owns that is private but not 0700 (0500: it can't
// create its slots there) is refused when chmod fails, not left for acquire
// to trip over.
func TestHelloRequires0700(t *testing.T) {
	root := filepath.Join(t.TempDir(), "readonly")
	openDir(t, root, 0o500)
	stubChmod(t, func(f *os.File, _ fs.FileMode) error {
		return &fs.PathError{Op: "chmod", Path: f.Name(), Err: syscall.EPERM}
	})
	s, _ := helloSession(t)
	if msg := refused(t, s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root})); !strings.Contains(msg, "0500") {
		t.Fatalf("error %q doesn't give the mode", msg)
	}
}

// Open roots the runner owns are tightened and accepted: an existing one, a
// new one, and one reached through a symlink (judged, and reported, by its
// target). One already 0700 is left alone.
func TestHelloMakesOwnedRootPrivate(t *testing.T) {
	dir := t.TempDir()
	open, target, private := filepath.Join(dir, "open"), filepath.Join(dir, "target"), filepath.Join(dir, "private")
	openDir(t, open, 0o755)
	openDir(t, target, 0o755)
	openDir(t, private, 0o700)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	calls := stubChmod(t, (*os.File).Chmod)
	for _, tc := range []struct {
		root   string
		chmods int
	}{{open, 1}, {filepath.Join(dir, "new", "root"), 0}, {link, 1}, {private, 0}} {
		*calls = nil
		s, out := helloSession(t)
		if err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: tc.root}); err != nil {
			t.Fatalf("hello on %s: %v", tc.root, err)
		}
		var ok protocol.HelloOK
		replies(t, out, protocol.TypeHelloOK, &ok)
		if ok.WorkRoot != s.layout.Root || ok.Proto != protocol.Version {
			t.Fatalf("hello on %s answered %+v", tc.root, ok)
		}
		if tc.root == link && s.layout.Root != canon {
			t.Fatalf("root through a symlink is %q, want %q", s.layout.Root, canon)
		}
		if len(*calls) != tc.chmods {
			t.Fatalf("hello on %s: chmod calls %q, want %d", tc.root, *calls, tc.chmods)
		}
		fi, err := os.Stat(s.layout.Root)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("root %s left at %#o", s.layout.Root, fi.Mode().Perm())
		}
	}
}

// A session keeps the root its first hello named: a second would split its
// locks and workspace between two roots.
func TestHelloOnlyOnce(t *testing.T) {
	s, _ := helloSession(t)
	first := t.TempDir()
	if err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: first}); err != nil {
		t.Fatal(err)
	}
	err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: t.TempDir()})
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeBadRequest {
		t.Fatalf("second hello: %v", err)
	}
	if want, _ := filepath.EvalSymlinks(first); s.layout.Root != want {
		t.Fatalf("root moved to %q", s.layout.Root)
	}
}

// A root owned by another user is refused whatever its mode, and a helper
// running as root leaves it as it was.
func TestHelloRefusesRootOwnedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give the directory to another user")
	}
	for _, m := range []fs.FileMode{0o755, 0o700} {
		root := filepath.Join(t.TempDir(), "theirs")
		openDir(t, root, m)
		if err := os.Chown(root, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		s, _ := helloSession(t)
		if msg := refused(t, s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root})); !strings.Contains(msg, "uid 65534") {
			t.Fatalf("%#o: error %q doesn't name the owner", m, msg)
		}
		if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != m {
			t.Fatalf("%#o: another user's root was changed: %v %v", m, fi.Mode(), err)
		}
	}
}
