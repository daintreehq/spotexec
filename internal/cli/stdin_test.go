package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/spotexec/internal/ui"
)

func TestUnreadInputPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if unreadInput(r) {
		t.Fatal("an open, empty pipe has no input")
	}
	if _, err := w.Write([]byte("x y\n")); err != nil {
		t.Fatal(err)
	}
	if !unreadInput(r) {
		t.Fatal("a pipe holding bytes has input")
	}
	// Looking must not consume it.
	_ = w.Close()
	if !unreadInput(r) {
		t.Fatal("bytes left by a closed writer are still input")
	}
	if b, _ := io.ReadAll(r); string(b) != "x y\n" {
		t.Fatalf("the input was consumed: read %q", b)
	}
	if unreadInput(r) {
		t.Fatal("a drained pipe has no input")
	}
}

func TestUnreadInputClosedEmptyPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = w.Close() // `true | sx -- cat`
	if unreadInput(r) {
		t.Fatal("a pipe whose writer wrote nothing has no input")
	}
}

func TestUnreadInputFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	full := filepath.Join(dir, "full")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(empty)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if unreadInput(f) {
		t.Fatal("an empty file has no input")
	}
	g, err := os.Open(full)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if !unreadInput(g) {
		t.Fatal("a file with bytes past its offset has input")
	}
	if _, err := g.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if unreadInput(g) {
		t.Fatal("a file read to its end has no input")
	}
}

func TestUnreadInputDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if unreadInput(f) {
		t.Fatal("/dev/null has no input")
	}
}

func TestUnreadInputSocket(t *testing.T) {
	a, b := socketPair(t)
	if unreadInput(a) {
		t.Fatal("an empty socket has no input")
	}
	if _, err := b.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if !unreadInput(a) {
		t.Fatal("a socket holding bytes has input")
	}
}

func socketPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	fa, err := s.(*net.UnixConn).File()
	if err != nil {
		t.Fatal(err)
	}
	fb, err := c.(*net.UnixConn).File()
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = c.Close()
	t.Cleanup(func() { _ = fa.Close(); _ = fb.Close() })
	return fa, fb
}

type warnRecorder struct {
	ui.Reporter
	warnings []string
}

func (w *warnRecorder) Warn(code, format string, args ...any) {
	w.warnings = append(w.warnings, code+": "+fmt.Sprintf(format, args...))
}

func TestStdinNoticeWarnsOnce(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	rec := &warnRecorder{}
	n := &stdinNotice{in: r}
	n.check(rec) // before the run: the writer hasn't written yet
	if len(rec.warnings) != 0 {
		t.Fatalf("warned with nothing piped: %v", rec.warnings)
	}
	if _, err := w.Write([]byte("diff")); err != nil {
		t.Fatal(err)
	}
	n.check(rec) // after the run
	n.check(rec)
	if len(rec.warnings) != 1 || rec.warnings[0][:len("STDIN_IGNORED")] != "STDIN_IGNORED" {
		t.Fatalf("want one STDIN_IGNORED warning, got %v", rec.warnings)
	}
}

func TestStdinNoticeWithoutCommand(t *testing.T) {
	if n := newStdinNotice(""); n != nil {
		t.Fatal("a sync has no command to feed, so nothing to warn about")
	}
	var n *stdinNotice
	n.check(&warnRecorder{}) // nil never warns, never panics
}
