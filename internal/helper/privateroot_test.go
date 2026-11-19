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

// A shared work root the runner can write to but not chmod (#312): the
// session is refused before HelloOK, so no workspace is ever accepted under it.
func TestHelloRefusesRootItCannotMakePrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil { // past the umask
		t.Fatal(err)
	}
	chmodRoot = func(name string, _ fs.FileMode) error {
		return &fs.PathError{Op: "chmod", Path: name, Err: syscall.EPERM}
	}
	t.Cleanup(func() { chmodRoot = os.Chmod })

	s, out := helloSession(t)
	err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root})
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeBadRequest {
		t.Fatalf("hello on a root it can't make private: %v, want a %s error", err, protocol.CodeBadRequest)
	}
	for _, want := range []string{root, "0755", "operation not permitted", "work_root"} {
		if !strings.Contains(pe.msg, want) {
			t.Errorf("error %q doesn't mention %q", pe.msg, want)
		}
	}
	if n := len(sent(out)); n != 0 {
		t.Fatalf("sent %d bytes before refusing", n)
	}
	if s.layout.Root != "" {
		t.Fatalf("layout set to %q on a refused hello", s.layout.Root)
	}
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err == nil {
		t.Fatal("a workspace was accepted after the refused hello")
	}
	if ents, err := os.ReadDir(root); err != nil || len(ents) != 0 {
		t.Fatalf("refused root holds %v (%v)", ents, err)
	}
}

// A chmod that reports success without taking effect (a filesystem that
// ignores modes) is caught by checking the result, not the call.
func TestHelloChecksTheModeNotTheChmod(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	chmodRoot = func(string, fs.FileMode) error { return nil }
	t.Cleanup(func() { chmodRoot = os.Chmod })

	s, out := helloSession(t)
	err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root})
	if err == nil || !strings.Contains(err.Error(), "0777") {
		t.Fatalf("hello on a 0777 root: %v", err)
	}
	if n := len(sent(out)); n != 0 {
		t.Fatalf("sent %d bytes before refusing", n)
	}
}

// An existing open root the runner owns is tightened and accepted, as are a
// new one and one reached through a symlink.
func TestHelloMakesOwnedRootPrivate(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(open, link); err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(open)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{open, filepath.Join(dir, "new", "root"), link} {
		s, out := helloSession(t)
		if err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root}); err != nil {
			t.Fatalf("hello on %s: %v", root, err)
		}
		var ok protocol.HelloOK
		replies(t, out, protocol.TypeHelloOK, &ok)
		if ok.WorkRoot != s.layout.Root || ok.Proto != protocol.Version {
			t.Fatalf("hello on %s answered %+v", root, ok)
		}
		if root == link && s.layout.Root != canon {
			t.Fatalf("root through a symlink is %q, want %q", s.layout.Root, canon)
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

// A root owned by another user is refused, and a helper running as root
// leaves its mode alone.
func TestHelloRefusesRootOwnedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give the directory to another user")
	}
	root := filepath.Join(t.TempDir(), "theirs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	s, _ := helloSession(t)
	err := s.hello(protocol.Hello{Proto: protocol.Version, WorkRoot: root})
	if err == nil || !strings.Contains(err.Error(), "uid 65534") {
		t.Fatalf("hello on a root owned by another user: %v", err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("another user's root was changed: %v %v", fi.Mode(), err)
	}
}
