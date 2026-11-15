package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

type statusRep struct {
	nopRep
	status []string
}

func (r *statusRep) Status(format string, args ...any) {
	r.status = append(r.status, fmt.Sprintf(format, args...))
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
	last := rep.status[len(rep.status)-1]
	if last != "aws-spot#3 · installing dependencies (npm ci) — added 1438 packages in 1m" {
		t.Fatalf("status = %q", last)
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
