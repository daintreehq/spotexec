package transport

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/helperbin"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// The tests drive the real transport against a fake `ssh`: this test binary,
// linked into PATH as "ssh". It keeps its control master in a state
// directory (the master's pid in `master`), logs every dial and stop, and
// plays the remote end: uname, the helper upload, and the RPC handshake. A
// master marked `wedged-<pid>` is a master whose connection died (the laptop
// slept): `-O check` still succeeds, but sessions through it hang.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "ssh" {
		os.Exit(fakeSSH(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSSH(args []string) int {
	st := os.Getenv("FAKE_SSH_STATE")
	logf := func(format string, a ...any) {
		f, _ := os.OpenFile(filepath.Join(st, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintf(f, format+"\n", a...)
		f.Close()
	}
	master := func() string {
		b, _ := os.ReadFile(filepath.Join(st, "master"))
		return strings.TrimSpace(string(b))
	}
	has := func(a string) bool {
		for _, x := range args {
			if x == a {
				return true
			}
		}
		return false
	}
	for i, a := range args {
		if a != "-O" || i+1 >= len(args) {
			continue
		}
		pid := master()
		switch args[i+1] {
		case "check":
			if pid == "" {
				return 255
			}
			fmt.Fprintf(os.Stderr, "Master running (pid=%s)\r\n", pid)
			return 0
		case "stop", "exit":
			if pid != "" {
				logf("%s %s", args[i+1], pid)
				os.Remove(filepath.Join(st, "master"))
			}
			return 0
		}
	}
	if has("-N") {
		n := 1
		for ; ; n++ {
			if _, err := os.Stat(filepath.Join(st, fmt.Sprintf("dialed-%d", n))); err != nil {
				break
			}
		}
		os.WriteFile(filepath.Join(st, fmt.Sprintf("dialed-%d", n)), nil, 0o600)
		os.WriteFile(filepath.Join(st, "master"), []byte(fmt.Sprint(100+n)), 0o600)
		logf("dial %d", 100+n)
		return 0
	}
	if pid := master(); pid != "" {
		if _, err := os.Stat(filepath.Join(st, "wedged-"+pid)); err == nil {
			time.Sleep(time.Minute) // nothing gets through
			return 255
		}
	}
	cmd := args[len(args)-1]
	switch {
	case strings.Contains(cmd, "uname -sm"):
		if _, err := os.Stat(filepath.Join(st, "uname-fails")); err == nil {
			fmt.Fprintln(os.Stderr, "uname: boom")
			return 1
		}
		fmt.Println("Linux x86_64")
	case strings.Contains(cmd, "test -x"):
		// not installed: upload
	case strings.HasSuffix(cmd, " rpc") || strings.Contains(cmd, " rpc'"):
		conn := protocol.NewConn(os.Stdin, os.Stdout)
		if _, err := conn.ReadFrame(); err != nil {
			return 1
		}
		if err := conn.Send(protocol.TypeHelloOK, protocol.HelloOK{Proto: protocol.Version, OS: "linux"}); err != nil {
			return 1
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
	default: // the helper upload
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	return 0
}

type fakeEnv struct {
	state string
	c     *Client
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(bin, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	st := t.TempDir()
	t.Setenv("FAKE_SSH_STATE", st)
	t.Setenv("GORACE", "atexit_sleep_ms=0") // a -race fake would idle 1s on every exit
	helpers := t.TempDir()
	if err := os.WriteFile(filepath.Join(helpers, helperbin.Name("linux", "amd64")), []byte("fake helper"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_HELPER_DIR", helpers)
	home, err := os.MkdirTemp("/tmp", "sxt") // short: control sockets live under it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	ctlMu.Lock()
	ctlDir = ""
	ctlMu.Unlock()
	t.Cleanup(func() {
		ctlMu.Lock()
		ctlDir = ""
		ctlMu.Unlock()
	})
	short(t, &reusedHandshakeTimeout, 500*time.Millisecond)
	short(t, &handshakeTimeout, 2*time.Second)
	short(t, &probeTimeout, 3*time.Second)
	return &fakeEnv{state: st, c: New(provider.Target{Host: "box", User: "dev"}, "~/.cache/spotexec")}
}

func short(t *testing.T, v *time.Duration, d time.Duration) {
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func (e *fakeEnv) log() string {
	b, _ := os.ReadFile(filepath.Join(e.state, "log"))
	return string(b)
}

func (e *fakeEnv) set(name, val string) {
	os.WriteFile(filepath.Join(e.state, name), []byte(val), 0o600)
}

func (e *fakeEnv) open(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := e.c.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v\nssh log:\n%s", err, e.log())
	}
	s.Close()
}

// A reused master whose connection died is replaced: the run reconnects
// once and proceeds.
func TestOpenReplacesDeadMaster(t *testing.T) {
	e := newFakeEnv(t)
	e.open(t) // dials master 101 and installs the helper
	e.set("wedged-101", "")
	start := time.Now()
	e.open(t)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("recovery took %s", took)
	}
	if got, want := e.log(), "dial 101\nstop 101\ndial 102\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// Same, when the helper isn't known to be installed (a new CLI version):
// the install's first remote command finds the dead master out.
func TestOpenReplacesDeadMasterBeforeInstall(t *testing.T) {
	e := newFakeEnv(t)
	e.open(t)
	os.RemoveAll(filepath.Join(os.Getenv("HOME"), ".cache", "spotexec", "helpers"))
	e.set("wedged-101", "")
	e.open(t)
	if got, want := e.log(), "dial 101\nstop 101\ndial 102\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// A master just dialed that can't carry a session is a real failure: no
// second dial, but it is retired so the next attempt starts afresh.
func TestOpenFreshMasterFailureIsNotRetried(t *testing.T) {
	e := newFakeEnv(t)
	e.set("wedged-101", "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := e.c.Open(ctx)
	if !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
	if got, want := e.log(), "dial 101\nstop 101\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// A run that found the old master dead must not retire the replacement
// another run has already dialed.
func TestDropMasterKeepsReplacement(t *testing.T) {
	e := newFakeEnv(t)
	e.set("master", "102")
	e.c.dropMaster(101)
	if !e.c.MasterAlive(context.Background()) || e.log() != "" {
		t.Fatalf("replacement master retired: %q", e.log())
	}
	e.c.dropMaster(102)
	if e.c.MasterAlive(context.Background()) || e.log() != "stop 102\n" {
		t.Fatalf("dead master kept: %q", e.log())
	}
}

// A probe command that fails at once is not a dead connection: its own
// error is kept.
func TestProbeFailureKeepsItsError(t *testing.T) {
	e := newFakeEnv(t)
	e.set("uname-fails", "")
	_, err := e.c.EnsureHelper(context.Background(), false)
	if err == nil || IsConnError(err) || !strings.Contains(err.Error(), "uname: boom") {
		t.Fatalf("want the remote error, got %v", err)
	}
}
