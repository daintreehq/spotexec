package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

type statusRep struct {
	nopRep
	mu     sync.Mutex
	status []string
}

func (r *statusRep) Status(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = append(r.status, fmt.Sprintf(format, args...))
}

func (r *statusRep) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status[len(r.status)-1]
}

// replies encodes what a helper sends in answer to a deps request.
func replies(t *testing.T, send func(c *protocol.Conn)) func(context.Context) (protocol.Frame, error) {
	t.Helper()
	var buf bytes.Buffer
	send(protocol.NewConn(nil, &buf))
	rd := protocol.NewConn(&buf, nil)
	return func(context.Context) (protocol.Frame, error) { return rd.ReadFrame() }
}

func TestConsumeDepsCapturesInstallOutput(t *testing.T) {
	old := statusEvery
	statusEvery = 0
	t.Cleanup(func() { statusEvery = old })
	dir := t.TempDir()
	next := replies(t, func(c *protocol.Conn) {
		_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_install", Message: "npm ci"})
		_ = c.Send(protocol.TypeStarted, protocol.Started{PID: 42})
		_ = c.WriteFrame(protocol.KindStdout, []byte("> husky\n.git can't be found\n"))
		_ = c.WriteFrame(protocol.KindStderr, []byte("31 vulnerabilities (28 moderate, 3 high)\n"))
		_ = c.WriteFrame(protocol.KindStdout, []byte("added 1438 packages in 1m\n"))
		_ = c.Send(protocol.TypeDepsResult, protocol.DepsResult{Key: "k", Built: true, MS: 61000})
	})
	rep := &statusRep{}
	var events []string
	log := newDepsLog(dir, "aws-spot#3")
	dr, err := consumeDeps(context.Background(), next, func(ev protocol.Event) { events = append(events, ev.Name) }, rep, "aws-spot#3", log)
	log.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !dr.Built || dr.Key != "k" {
		t.Fatalf("result = %+v", dr)
	}
	if len(events) != 1 || events[0] != "deps_install" {
		t.Fatalf("events = %v", events)
	}
	b, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	want := "> husky\n.git can't be found\n31 vulnerabilities (28 moderate, 3 high)\nadded 1438 packages in 1m\n"
	if string(b) != want {
		t.Fatalf("log = %q, want %q", b, want)
	}
	if filepath.Dir(log.Path()) != dir || !strings.Contains(filepath.Base(log.Path()), "aws-spot-3") {
		t.Fatalf("log path = %s", log.Path())
	}
	if last := rep.last(); last != "aws-spot#3 · installing dependencies (npm ci) — added 1438 packages in 1m" {
		t.Fatalf("status = %q", rep.last())
	}
}

func TestConsumeDepsFailureIsARemoteError(t *testing.T) {
	next := replies(t, func(c *protocol.Conn) {
		_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_install", Message: "npm ci"})
		_ = c.WriteFrame(protocol.KindStderr, []byte("npm ERR! code E404\n"))
		_ = c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeDepsFailed, Message: "install command exited 1"})
	})
	log := newDepsLog(t.TempDir(), "r#1")
	_, err := consumeDeps(context.Background(), next, func(protocol.Event) {}, nopRep{}, "r#1", log)
	log.Close()
	var re *transport.RemoteError
	if !errors.As(err, &re) || re.Code != protocol.CodeDepsFailed {
		t.Fatalf("err = %v", err)
	}
	if tail, cut := log.Tail(depsTailLines); tail != "npm ERR! code E404" || cut {
		t.Fatalf("tail = %q, %v", tail, cut)
	}
}

func TestDepsLogCacheHitLeavesNoFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	next := replies(t, func(c *protocol.Conn) {
		_ = c.Send(protocol.TypeDepsResult, protocol.DepsResult{Key: "k", Hydrated: true})
	})
	log := newDepsLog(dir, "r#1")
	if _, err := consumeDeps(context.Background(), next, func(protocol.Event) {}, nopRep{}, "r#1", log); err != nil {
		t.Fatal(err)
	}
	log.Close()
	if log.Path() != "" {
		t.Fatalf("path = %q", log.Path())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("logs dir created on a cache hit: %v", err)
	}
}

func TestDepsLogTailIsBounded(t *testing.T) {
	log := newDepsLog(t.TempDir(), "r")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(log, "line %d\n", i)
	}
	tail, cut := log.Tail(depsTailLines)
	lines := strings.Split(tail, "\n")
	if !cut || len(lines) != depsTailLines || lines[0] != "line 60" || lines[len(lines)-1] != "line 99" {
		t.Fatalf("tail: cut=%v %d lines, %q … %q", cut, len(lines), lines[0], lines[len(lines)-1])
	}

	// Past the byte bound, the partial line the buffer starts in is dropped.
	big := newDepsLog("", "r")
	big.Write([]byte(strings.Repeat("x", depsTailBytes) + "\nlast\n"))
	if tail, cut := big.Tail(depsTailLines); tail != "last" || !cut {
		t.Fatalf("tail = %.20q, %v", tail, cut)
	}
	if big.Path() != "" {
		t.Fatal("a log with no directory has no file")
	}
	if len(big.tail) > depsTailBytes {
		t.Fatalf("tail buffer holds %d bytes", len(big.tail))
	}
}

func TestPruneDepsLogsKeepsNewestAndActive(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * depsLogsActive)
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("20260101-00000%d-r-x.log", i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if i != 1 { // #1 is still being written
			_ = os.Chtimes(p, old, old)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600)
	pruneDepsLogs(dir, 2, time.Now().Add(-depsLogsActive))
	ents, _ := os.ReadDir(dir)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	want := "20260101-000001-r-x.log 20260101-000003-r-x.log 20260101-000004-r-x.log notes.txt"
	if strings.Join(got, " ") != want {
		t.Fatalf("left %v", got)
	}
}

func TestDepsErrorDetail(t *testing.T) {
	base := fmt.Errorf("dependencies: %w", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeDepsFailed, Message: "install command exited 1"}})
	de := &DepsError{Err: base, Tail: "\x1b[31mnpm ERR!\x1b[0m missing\r\n50%\r100%", Truncated: true, Log: "/state/logs/deps/a.log"}
	err := infra(de)
	if err.Error() != "dependencies: DEPS_FAILED: install command exited 1" {
		t.Fatalf("message = %q", err.Error())
	}
	var got *DepsError
	var re *transport.RemoteError
	if !errors.As(err, &got) || !errors.As(err, &re) {
		t.Fatal("DepsError hides what it wraps")
	}
	want := []string{"last 2 lines of the install's output:", "  npm ERR! missing", "  100%", "full log: /state/logs/deps/a.log"}
	if d := got.Detail(); strings.Join(d, "|") != strings.Join(want, "|") {
		t.Fatalf("detail = %q", d)
	}
}

func TestConsumeDepsCoalescesStatus(t *testing.T) {
	next := replies(t, func(c *protocol.Conn) {
		_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_install", Message: "npm ci"})
		for i := 0; i < 500; i++ {
			_ = c.WriteFrame(protocol.KindStdout, []byte(fmt.Sprintf("line %d\n", i)))
		}
		_ = c.Send(protocol.TypeDepsResult, protocol.DepsResult{Key: "k", Built: true})
	})
	rep := &statusRep{}
	log := newDepsLog(t.TempDir(), "r")
	if _, err := consumeDeps(context.Background(), next, func(protocol.Event) {}, rep, "r", log); err != nil {
		t.Fatal(err)
	}
	log.Close()
	if len(rep.status) > 10 {
		t.Fatalf("%d status updates for 500 quick lines", len(rep.status))
	}
	if b, _ := os.ReadFile(log.Path()); !strings.HasSuffix(string(b), "line 499\n") {
		t.Fatal("throttling the status lost output from the log")
	}
}

func TestDepsLogTailOnALineBoundary(t *testing.T) {
	// The bytes dropped from the tail end exactly on a line break: the
	// first line kept is whole and must be shown.
	keep := "important failure\n" + strings.Repeat("p", depsTailBytes-len("important failure\n")-1) + "\n"
	for _, writes := range [][]string{{"old\n", keep}, {"old\n" + keep}} {
		l := newDepsLog("", "r")
		for _, w := range writes {
			l.Write([]byte(w))
		}
		tail, cut := l.Tail(depsTailLines)
		if !cut || !strings.HasPrefix(tail, "important failure\n") {
			t.Fatalf("writes %d: tail starts %.30q (cut %v)", len(writes), tail, cut)
		}
		if cap(l.tail) > depsTailBytes {
			t.Fatalf("tail buffer grew to %d bytes", cap(l.tail))
		}
	}
	// Many writes, one bigger than the bound: the buffer stays bounded and
	// holds the newest output.
	l := newDepsLog("", "r")
	l.Write([]byte(strings.Repeat("a\n", depsTailBytes)))
	for i := 0; i < 50; i++ {
		fmt.Fprintf(l, "w%d\n", i)
	}
	if tail, _ := l.Tail(1); tail != "w49" || cap(l.tail) > depsTailBytes {
		t.Fatalf("tail %q, cap %d", tail, cap(l.tail))
	}
}

func TestDepsLogEmptyWriteAndUnwritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	l := newDepsLog(dir, "r")
	l.Write(nil)
	l.Write([]byte{})
	if _, err := os.Stat(dir); !os.IsNotExist(err) || l.Path() != "" {
		t.Fatal("an empty frame created a log")
	}

	// A logs directory that can't be made: no file, but the tail is kept.
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, nil, 0o600)
	l = newDepsLog(filepath.Join(file, "deps"), "r")
	if n, err := l.Write([]byte("npm ERR! boom\n")); n != 14 || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	l.Write([]byte("more\n"))
	l.Close()
	if tail, _ := l.Tail(depsTailLines); l.Path() != "" || tail != "npm ERR! boom\nmore" {
		t.Fatalf("path %q, tail %q", l.Path(), tail)
	}

	// The file failing part way: the path keeps what was written, and no
	// new file is opened.
	l = newDepsLog(t.TempDir(), "r")
	l.Write([]byte("first\n"))
	path := l.Path()
	l.f.Close() // later writes fail
	l.Write([]byte("second\n"))
	l.Write([]byte("third\n"))
	l.Close()
	if b, _ := os.ReadFile(path); string(b) != "first\n" || l.Path() != path {
		t.Fatalf("log %q at %q", b, l.Path())
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("%d logs after a failed write", len(ents))
	}
	if tail, _ := l.Tail(depsTailLines); tail != "first\nsecond\nthird" {
		t.Fatalf("tail %q", tail)
	}
}

func TestConsumeDepsShowsTheLineBeforeASilence(t *testing.T) {
	old := statusEvery
	statusEvery = 30 * time.Millisecond
	t.Cleanup(func() { statusEvery = old })
	frames := replies(t, func(c *protocol.Conn) {
		_ = c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_install", Message: "npm ci"})
		_ = c.WriteFrame(protocol.KindStdout, []byte("downloading\n"))
		_ = c.WriteFrame(protocol.KindStdout, []byte("building native extensions\n"))
		_ = c.Send(protocol.TypeDepsResult, protocol.DepsResult{Key: "k", Built: true})
	})
	rep := &statusRep{}
	var seen string
	n := 0
	next := func(ctx context.Context) (protocol.Frame, error) {
		n++
		if n == 4 { // a quiet compile before the result
			time.Sleep(5 * statusEvery)
			seen = rep.last()
		}
		return frames(ctx)
	}
	log := newDepsLog(t.TempDir(), "r")
	if _, err := consumeDeps(context.Background(), next, func(protocol.Event) {}, rep, "r", log); err != nil {
		t.Fatal(err)
	}
	log.Close()
	if !strings.HasSuffix(seen, "building native extensions") {
		t.Fatalf("status during the silence = %q", seen)
	}
	time.Sleep(3 * statusEvery)
	if got := rep.last(); !strings.HasSuffix(got, "building native extensions") {
		t.Fatalf("status changed after the phase ended: %q", got)
	}
}
