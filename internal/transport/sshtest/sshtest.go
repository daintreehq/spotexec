// Package sshtest is a fake `ssh` for tests that drive the real transport.
//
// The fake is the test binary itself, linked into PATH as "ssh" (call Main
// first thing in TestMain). It keeps its control master in a state
// directory (the master's pid in `master`), logs every dial and stop, and
// plays the remote end: uname, the helper upload, and the RPC handshake.
// Files in the state directory change its behavior:
//
//   - wedged-<pid>: the master with that pid has a dead connection (the
//     laptop slept): `-O check` still succeeds, but sessions through it hang;
//   - uname-fails: `uname` fails at once;
//   - upload-hangs: the helper upload never finishes;
//   - rpc-silent: after the handshake the helper goes silent, reading and
//     writing nothing (a wedged helper or half-open channel);
//   - rpc-pressure: after the handshake the helper sends one heartbeat
//     reporting memory pressure, then goes silent (a VM out of memory);
//   - rpc-heartbeat: after the handshake the helper sends only heartbeats
//     and never answers (alive, but slow);
//   - hold-fds: another process keeps the session's stdin, stdout and
//     stderr open after the fake is killed, as a ControlMaster does for a
//     stuck channel (its pid goes in hold-fds.pid; it exits within 30s).
package sshtest

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/helperbin"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// Main runs the fake and exits when the test binary was invoked as ssh.
func Main() {
	if filepath.Base(os.Args[0]) == "ssh" {
		os.Exit(run(os.Args[1:]))
	}
}

// Fake is an installed fake ssh.
type Fake struct{ State string }

// Install puts the fake first in PATH for the test, with a short fresh HOME
// (control sockets live under it) and a fake linux/amd64 helper.
func Install(t testing.TB) *Fake {
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
	home, err := os.MkdirTemp("/tmp", "sxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	return &Fake{State: st}
}

// Set creates a behavior file (see the package doc) with content val.
func (f *Fake) Set(name, val string) {
	_ = os.WriteFile(filepath.Join(f.State, name), []byte(val), 0o600)
}

// Release ends the process hold-fds started, if any.
func (f *Fake) Release() {
	if b, err := os.ReadFile(filepath.Join(f.State, "hold-fds.pid")); err == nil {
		if pid, err := strconv.Atoi(string(b)); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// Log is what the fake did with masters: "dial <pid>", "stop <pid>"...
func (f *Fake) Log() string {
	b, _ := os.ReadFile(filepath.Join(f.State, "log"))
	return string(b)
}

func run(args []string) int {
	st := os.Getenv("FAKE_SSH_STATE")
	is := func(name string) bool {
		_, err := os.Stat(filepath.Join(st, name))
		return err == nil
	}
	logf := func(format string, a ...any) {
		f, _ := os.OpenFile(filepath.Join(st, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintf(f, format+"\n", a...)
		f.Close()
	}
	master := func() string {
		b, _ := os.ReadFile(filepath.Join(st, "master"))
		return strings.TrimSpace(string(b))
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
	for _, a := range args {
		if a == "-N" {
			n := 1
			for is(fmt.Sprintf("dialed-%d", n)) {
				n++
			}
			_ = os.WriteFile(filepath.Join(st, fmt.Sprintf("dialed-%d", n)), nil, 0o600)
			_ = os.WriteFile(filepath.Join(st, "master"), []byte(fmt.Sprint(100+n)), 0o600)
			logf("dial %d", 100+n)
			return 0
		}
	}
	if pid := master(); pid != "" && is("wedged-"+pid) {
		time.Sleep(time.Minute) // nothing gets through
		return 255
	}
	cmd := args[len(args)-1]
	switch {
	case strings.Contains(cmd, "uname -sm"):
		if is("uname-fails") {
			fmt.Fprintln(os.Stderr, "uname: boom")
			return 1
		}
		fmt.Println("Linux x86_64")
	case strings.Contains(cmd, "test -x"):
		// not installed: upload
	case strings.HasSuffix(cmd, " rpc") || strings.Contains(cmd, " rpc'"):
		return rpc()
	default: // the helper upload
		if is("upload-hangs") {
			time.Sleep(time.Hour)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	return 0
}

func rpc() int {
	st := os.Getenv("FAKE_SSH_STATE")
	conn := protocol.NewConn(os.Stdin, os.Stdout)
	if _, err := conn.ReadFrame(); err != nil {
		return 1
	}
	if err := conn.Send(protocol.TypeHelloOK, protocol.HelloOK{Proto: protocol.Version, OS: "linux", Arch: "amd64", CPUs: 4}); err != nil {
		return 1
	}
	if _, err := os.Stat(filepath.Join(st, "hold-fds")); err == nil {
		hold := exec.Command("sleep", "30")
		hold.Stdin, hold.Stdout, hold.Stderr = os.Stdin, os.Stdout, os.Stderr
		if hold.Start() == nil {
			_ = os.WriteFile(filepath.Join(st, "hold-fds.pid"), []byte(strconv.Itoa(hold.Process.Pid)), 0o600)
		}
	}
	if _, err := os.Stat(filepath.Join(st, "rpc-pressure")); err == nil {
		_ = conn.Send(protocol.TypeHeartbeat, protocol.Heartbeat{MemTotalMB: 16000, MemAvailMB: 120})
		time.Sleep(time.Hour) // until killed
	}
	if _, err := os.Stat(filepath.Join(st, "rpc-silent")); err == nil {
		time.Sleep(time.Hour) // until killed
	}
	if _, err := os.Stat(filepath.Join(st, "rpc-heartbeat")); err == nil {
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); os.Exit(0) }()
		for {
			time.Sleep(50 * time.Millisecond)
			if conn.Send(protocol.TypeHeartbeat, nil) != nil {
				return 0
			}
		}
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}
