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
	// Drained only once the marker is really there: the watchdog acts on
	// it, and the caller never places on this VM again.
	if op != "" {
		if err := os.WriteFile(drainOpFile(root), []byte(op+"\n"), 0o644); err != nil {
			res.Result, res.Detail = DrainBusy, "recording the drain: "+err.Error()
			return res
		}
	}
	if err := os.WriteFile(drainingFile(root), []byte(reason+"\n"), 0o644); err != nil || !draining(root) {
		res.Result, res.Detail = DrainBusy, fmt.Sprintf("writing the drain marker: %v", err)
		return res
	}
	res.Result = DrainDone
	return res
}

// drainingFile is where markDraining writes.
func drainingFile(root string) string {
	if onSpotexecVM() {
		return DrainingFile
	}
	return filepath.Join(root, "draining")
}

// DrainScheduled: the VM's watchdog will end it once it has been idle for
// the operation's min-idle (schedule-drain).
const DrainScheduled = "scheduled"

// Scheduled drains: when an early drain is committed the VM is usually
// still keeping its results, and no process may be left on the client to
// ask again once they have been kept long enough. schedule-drain hands the
// wait to the VM instead: it records the least idle time in drain_after,
// and the watchdog (cloud-init) ends the VM once it has been idle that long
// with no session holding its lifecycle lock, exactly as it would at
// idle_timeout. The record only ever shortens the wait; it is written once
// and never withdrawn (the client has already stopped placing on the VM).

// drainAfterFile and featuresFile live beside the drain marker.
func drainAfterFile(root string) string {
	if onSpotexecVM() {
		return filepath.Join(LifecycleDir, "drain_after")
	}
	return filepath.Join(root, "drain_after")
}

func featuresFile(root string) string {
	if onSpotexecVM() {
		return filepath.Join(LifecycleDir, "features")
	}
	return filepath.Join(root, "features")
}

// watchdogReadsDrainAfter reports whether this VM's watchdog acts on
// drain_after (one booted before it existed has no features file). An
// error other than the file's absence is returned: it proves nothing.
func watchdogReadsDrainAfter(root string) (bool, error) {
	b, err := os.ReadFile(featuresFile(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, f := range strings.Fields(string(b)) {
		if f == "drain_after" {
			return true, nil
		}
	}
	return false, nil
}

// ScheduleDrain is `spotexec-helper schedule-drain`: it records that this
// VM ends once idle for minIdle, and prints a TryDrainResult.
func ScheduleDrain(w io.Writer, root, op string, minIdle time.Duration) error {
	return json.NewEncoder(w).Encode(scheduleDrain(root, op, minIdle, hasLifecycleLock()))
}

func scheduleDrain(root, op string, minIdle time.Duration, ephemeral bool) TryDrainResult {
	res := TryDrainResult{Op: op, IdleS: -1}
	if !ephemeral {
		res.Result = DrainUnsupported
		return res
	}
	// What is already in place first: "unsupported" must never be the
	// answer for a VM that has a drain scheduled (the client would put it
	// back into service).
	if draining(root) {
		if b, err := os.ReadFile(drainOpFile(root)); err == nil && op != "" && strings.TrimSpace(string(b)) == op {
			res.Result = DrainDone
		} else {
			res.Result = DrainAlready
		}
		return res
	}
	if _, err := os.Stat(drainAfterFile(root)); err == nil {
		// Already scheduled (a retry whose answer was lost, or another
		// operation): the first record stands.
		res.Result = DrainScheduled
		return res
	} else if !errors.Is(err, os.ErrNotExist) {
		res.Result, res.Detail = DrainBusy, "reading the drain schedule: "+err.Error()
		return res
	}
	if ok, err := watchdogReadsDrainAfter(root); err != nil {
		res.Result, res.Detail = DrainBusy, "reading the watchdog's features: "+err.Error()
		return res
	} else if !ok {
		res.Result, res.Detail = DrainUnsupported, "this VM's watchdog predates scheduled drains"
		return res
	}
	if minIdle < 0 {
		minIdle = 0
	}
	secs := int64((minIdle + time.Second - 1) / time.Second)
	if op != "" {
		if err := os.WriteFile(drainOpFile(root), []byte(op+"\n"), 0o644); err != nil {
			res.Result, res.Detail = DrainBusy, "recording the drain: "+err.Error()
			return res
		}
	}
	// Written whole, then renamed into place: the watchdog reads it
	// without a lock.
	p := drainAfterFile(root)
	tmp := fmt.Sprintf("%s.%d", p, os.Getpid())
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", secs)), 0o644); err != nil {
		res.Result, res.Detail = DrainBusy, "scheduling the drain: "+err.Error()
		return res
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		res.Result, res.Detail = DrainBusy, "scheduling the drain: "+err.Error()
		return res
	}
	res.Result = DrainScheduled
	return res
}
