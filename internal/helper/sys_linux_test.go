//go:build linux

package helper

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// What a run left behind is reaped from its workspace, including a
// background process that changed directory (it still carries the run's
// SPOTEXEC_WORKSPACE), but nothing else.
func TestReapWorkspace(t *testing.T) {
	ws := t.TempDir()
	start := func(dir string, env ...string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}
	sub := filepath.Join(ws, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := start(sub)
	escaped := start("/", "SPOTEXEC_WORKSPACE="+ws)
	other := start("/", "SPOTEXEC_WORKSPACE="+ws+"-other")
	exited := make(map[int]chan struct{})
	for _, c := range []*exec.Cmd{inside, escaped, other} {
		ch := make(chan struct{})
		exited[c.Process.Pid] = ch
		go func(c *exec.Cmd) { _ = c.Wait(); close(ch) }(c)
	}
	time.Sleep(100 * time.Millisecond) // let them exec
	if left := reapWorkspace(ws, time.Second); len(left) != 0 {
		t.Fatalf("survivors: %v", left)
	}
	for _, c := range []*exec.Cmd{inside, escaped} {
		select {
		case <-exited[c.Process.Pid]:
		case <-time.After(5 * time.Second):
			t.Fatalf("pid %d still running", c.Process.Pid)
		}
	}
	select {
	case <-exited[other.Process.Pid]:
		t.Fatal("a process of another workspace was killed")
	case <-time.After(200 * time.Millisecond):
	}
}
