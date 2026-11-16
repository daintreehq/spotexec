package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Early drain (docs/POOL.md, "Early drain"): the pool manager ends a
// surplus VM before its idle_timeout, but only through this operation,
// which decides under the lifecycle lock. A status reply saying "no
// holders" proves nothing a moment later; here, every acquire takes the
// lifecycle lock shared and checks the drain marker after it has it, so
// once the marker is written under the exclusive lock no new session can
// start, and the watchdog ends the VM at its next pass.

// TryDrain results.
const (
	DrainDone        = "drained"          // marked draining by this operation (or an earlier try with its ID)
	DrainBusy        = "busy"             // a session or exact-slot waiter holds the VM
	DrainProtected   = "protected"        // idle for less than the result-retention time
	DrainAlready     = "already_draining" // draining for another reason
	DrainUnsupported = "unsupported"      // not an ephemeral spotexec VM
)

// TryDrainResult is printed by `spotexec-helper try-drain`.
type TryDrainResult struct {
	Result string `json:"result"`
	Op     string `json:"op"`
	IdleS  int64  `json:"idle_s"` // -1 unknown
	Detail string `json:"detail,omitempty"`
}

// drainOpFile records which operation drained the VM, so a retry of the
// same operation (its answer lost) reads as done.
func drainOpFile(root string) string {
	if onSpotexecVM() {
		return filepath.Join(LifecycleDir, "drain_op")
	}
	return filepath.Join(root, "drain_op")
}

// TryDrainIfIdle marks the VM draining if, under its lifecycle lock held
// exclusively, no session holds it and it has been idle for at least
// minIdle (in this VM's own clock). It never waits behind work.
func TryDrainIfIdle(w io.Writer, root, op, reason string, minIdle time.Duration) error {
	res := tryDrain(root, op, reason, minIdle, LifecycleLock, ActivityFile, hasLifecycleLock())
	return json.NewEncoder(w).Encode(res)
}

func tryDrain(root, op, reason string, minIdle time.Duration, lockPath, activity string, ephemeral bool) TryDrainResult {
	res := TryDrainResult{Op: op, IdleS: -1}
	if !ephemeral {
		res.Result = DrainUnsupported
		return res
	}
	already := func() TryDrainResult {
		if b, err := os.ReadFile(drainOpFile(root)); err == nil && op != "" && strings.TrimSpace(string(b)) == op {
			res.Result = DrainDone
		} else {
			res.Result = DrainAlready
		}
		return res
	}
	if draining(root) {
		return already()
	}
	f, err := os.Open(lockPath)
	if err != nil {
		res.Result, res.Detail = DrainUnsupported, err.Error()
		return res
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		res.Result = DrainBusy
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			res.Detail = err.Error()
		}
		return res
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	// Rechecked with the lock held: nothing can start while we hold it.
	if draining(root) {
		return already()
	}
	l := Layout{Root: root}
	if h := readHolders(l); len(h) > 0 {
		res.Result, res.Detail = DrainBusy, fmt.Sprintf("%d session(s)", len(h))
		return res
	}
	if wt := readWaiters(l); len(wt) > 0 {
		res.Result, res.Detail = DrainBusy, fmt.Sprintf("%d waiting for a workspace slot", len(wt))
		return res
	}
	fi, err := os.Stat(activity)
	if err != nil {
		res.Result, res.Detail = DrainProtected, "no activity record"
		return res
	}
	idle := time.Since(fi.ModTime())
	if idle < 0 {
		idle = 0 // a clock stepped back: count it as just active
	}
	res.IdleS = int64(idle.Seconds())
	if idle < minIdle {
		res.Result = DrainProtected
		res.Detail = fmt.Sprintf("idle %s of the %s its results are kept", idle.Round(time.Second), minIdle)
		return res
	}
	if op != "" {
		_ = os.WriteFile(drainOpFile(root), []byte(op+"\n"), 0o644)
	}
	markDraining(root, reason)
	res.Result = DrainDone
	return res
}
