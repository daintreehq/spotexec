package client

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// A Mac that idle-sleeps mid-run stops answering its runners, and a runner
// ends a run after transport.HelperPatience without word from its client
// (#142). While a run is under way sx holds macOS's idle-sleep assertion
// through caffeinate: -i blocks idle sleep only (the display still sleeps,
// and so does a closed lid), and -w ends caffeinate when sx exits, however
// it exits. SPOTEXEC_KEEP_AWAKE=0 (or false, no) turns it off.
const caffeinate = "/usr/bin/caffeinate"

// awakeCmd is the command that keeps this machine awake while pid lives
// (nil where there is none: anything but macOS, or no caffeinate).
func awakeCmd(goos string, pid int) *exec.Cmd {
	if goos != "darwin" {
		return nil
	}
	if _, err := os.Stat(caffeinate); err != nil {
		return nil
	}
	cmd := exec.Command(caffeinate, "-i", "-w", strconv.Itoa(pid))
	// Its own process group: a Ctrl-C at the terminal doesn't end it while
	// sx is still cleaning up the run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// startAwake starts holding the assertion, best effort, and returns what
// ends it (tests replace it).
var startAwake = func() (stop func()) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SPOTEXEC_KEEP_AWAKE"))) {
	case "0", "false", "no":
		return func() {}
	}
	return holdAwake(awakeCmd(runtime.GOOS, os.Getpid()))
}

// holdAwake starts cmd (nil: nothing to start) and returns what ends it.
func holdAwake(cmd *exec.Cmd) (stop func()) {
	if cmd == nil || cmd.Start() != nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() // reaps it, even if it ends first
	return func() {
		_ = cmd.Process.Kill()
		<-done
	}
}

var awake struct {
	sync.Mutex
	n    int    // holds outstanding
	stop func() // ends the assertion while n > 0
}

// KeepAwake keeps this machine from idle-sleeping until the returned
// release is called (once is enough; more calls do nothing). Holds nest:
// the parts of a task share one assertion, ended by the last release.
func KeepAwake() (release func()) {
	awake.Lock()
	defer awake.Unlock()
	if awake.n++; awake.n == 1 {
		awake.stop = startAwake()
	}
	return sync.OnceFunc(func() {
		awake.Lock()
		defer awake.Unlock()
		if awake.n--; awake.n == 0 {
			awake.stop()
			awake.stop = nil
		}
	})
}
