package client

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Only macOS gets caffeinate, holding the idle-sleep assertion (not the
// display's, not a closed lid's) for as long as sx lives.
func TestAwakeCmd(t *testing.T) {
	for _, goos := range []string{"linux", "freebsd", "windows"} {
		if cmd := awakeCmd(goos, 42); cmd != nil {
			t.Errorf("%s: %v", goos, cmd.Args)
		}
	}
	if _, err := os.Stat(caffeinate); err != nil {
		if awakeCmd("darwin", 42) != nil {
			t.Fatal("a command without caffeinate")
		}
		t.Skip("no caffeinate here")
	}
	cmd := awakeCmd("darwin", 42)
	if cmd == nil {
		t.Fatal("no command on macOS")
	}
	if want := []string{caffeinate, "-i", "-w", "42"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("args %v, want %v", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("caffeinate shares sx's process group: a Ctrl-C would end it mid-cleanup")
	}
}

// Overlapping runs (the parts of a task) share one assertion, which ends
// with the last of them; a release called twice counts once.
func TestKeepAwakeNests(t *testing.T) {
	old := startAwake
	t.Cleanup(func() { startAwake = old })
	var starts, stops int
	startAwake = func() func() {
		starts++
		return func() { stops++ }
	}
	a := KeepAwake()
	b := KeepAwake()
	if starts != 1 {
		t.Fatalf("%d assertions for two overlapping holds", starts)
	}
	a()
	a()
	if stops != 0 {
		t.Fatal("ended while a hold remained")
	}
	b()
	if stops != 1 {
		t.Fatalf("%d ends after the last release, want 1", stops)
	}
	KeepAwake()()
	if starts != 2 || stops != 2 {
		t.Fatalf("starts %d, stops %d after a later hold", starts, stops)
	}
}

// The held process runs until it is stopped, and stopping reaps it; one
// that can't start, or isn't there, ends nothing.
func TestHoldAwake(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	stop := holdAwake(cmd)
	if cmd.Process == nil || cmd.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("not running")
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	start := time.Now()
	stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stop took %s: it waited for the process instead of ending it", took)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("not killed and reaped by stop: %v", cmd.ProcessState)
	}
	holdAwake(nil)()
	holdAwake(exec.Command("/nonexistent/caffeinate"))()
}

// On macOS, caffeinate holds the assertion for this process until it is
// stopped; the opt-out starts nothing.
func TestStartAwakeOnMac(t *testing.T) {
	if awakeCmd(runtime.GOOS, os.Getpid()) == nil {
		t.Skip("no caffeinate here")
	}
	pattern := "caffeinate -i -w " + strconv.Itoa(os.Getpid()) + "$"
	running := func() bool { return exec.Command("pgrep", "-f", pattern).Run() == nil }
	t.Setenv("SPOTEXEC_KEEP_AWAKE", "False")
	stop := startAwake()
	if running() {
		t.Fatal("caffeinate started despite SPOTEXEC_KEEP_AWAKE=0")
	}
	stop()
	t.Setenv("SPOTEXEC_KEEP_AWAKE", "")
	stop = startAwake()
	if !running() {
		stop()
		t.Fatal("caffeinate isn't running")
	}
	stop()
	if running() {
		t.Fatal("caffeinate still running after stop")
	}
}
