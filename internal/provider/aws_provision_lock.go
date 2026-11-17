package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The per-runner provisioning lock, in the runner's state directory:
//
//   - up.lock: launches hold it shared, Up and Down exclusively, so a
//     teardown never runs while a launch is about to create an instance,
//     and Up's "list, then launch only if there is none" stays atomic.
//     (Older spotexec binaries take it exclusively for everything.)
//   - up.slot.N.lock: a launch holds one of launchSlots of these, which
//     bounds the runner's RunInstances requests in flight at once.
//   - up.writer.lock and up.writer.pending: Up and Down take the writer
//     lock, then announce themselves with the marker before waiting for
//     up.lock, and new launches hold off while it's there, so a steady
//     stream of launches can't starve a teardown.
//
// The lock files stay in place: removing one that another process holds
// would let a third lock a new file at the same path.
const (
	launchSlots     = 4
	upLockFile      = "up.lock"
	upWriterLock    = "up.writer.lock"
	upWriterPending = "up.writer.pending"
	lockPoll        = 25 * time.Millisecond
)

// tryLockFile opens path and tries to flock it (how: LOCK_SH or LOCK_EX)
// without waiting. ok is false when another holder has it.
func tryLockFile(path string, how int) (release func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
		return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		f.Close()
		return nil, false, nil
	default:
		f.Close()
		return nil, false, err
	}
}

// waitLockFile flocks path (how: LOCK_SH or LOCK_EX), polling until it's
// free or ctx ends. onWait (if set) runs once, when it first has to wait.
func waitLockFile(ctx context.Context, path string, how int, onWait func()) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return pollLock(ctx, onWait, func() (func(), bool, error) { return tryLockFile(path, how) })
}

// pollLock calls try until it acquires (or fails), or ctx ends.
func pollLock(ctx context.Context, onWait func(), try func() (func(), bool, error)) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, ok, err := try()
		if err != nil {
			return nil, err
		}
		if ok {
			return release, nil
		}
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

func (a *AWSSpot) lockPath(name string) string { return filepath.Join(a.dir, name) }

// lockUp takes the per-runner provisioning lock exclusively (Up and Down),
// honoring ctx: it waits for the launches holding it to finish, while new
// ones hold off.
func (a *AWSSpot) lockUp(ctx context.Context, log Logger) (func(), error) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, err
	}
	logged := false
	onWait := func() { // once, across both waits
		if log != nil && !logged {
			log("waiting for another spotexec process that is provisioning %s", a.r.Name)
			logged = true
		}
	}
	releaseWriter, err := pollLock(ctx, onWait, func() (func(), bool, error) {
		return tryLockFile(a.lockPath(upWriterLock), syscall.LOCK_EX)
	})
	if err != nil {
		return nil, err
	}
	marker := a.lockPath(upWriterPending)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		releaseWriter()
		return nil, err
	}
	done := func() {
		// Removed while still holding the writer lock: a launch only clears
		// a marker whose writer is gone.
		_ = os.Remove(marker)
		releaseWriter()
	}
	releaseUp, err := pollLock(ctx, onWait, func() (func(), bool, error) {
		return tryLockFile(a.lockPath(upLockFile), syscall.LOCK_EX)
	})
	if err != nil {
		done()
		return nil, err
	}
	return func() { releaseUp(); done() }, nil
}

// lockLaunch takes the per-runner provisioning lock for one launch,
// honoring ctx: shared with other launches (at most launchSlots at once),
// never while Up or Down holds it or waits for it.
func (a *AWSSpot) lockLaunch(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, err
	}
	return pollLock(ctx, nil, a.tryLaunchLock)
}

// tryLaunchLock is one attempt at lockLaunch: a free slot, then up.lock
// shared, with no writer waiting before or after.
func (a *AWSSpot) tryLaunchLock() (func(), bool, error) {
	if pending, err := a.writerPending(); err != nil || pending {
		return nil, false, err
	}
	var releaseSlot func()
	for i := range launchSlots {
		release, ok, err := tryLockFile(a.lockPath(fmt.Sprintf("up.slot.%d.lock", i)), syscall.LOCK_EX)
		if err != nil {
			return nil, false, err
		}
		if ok {
			releaseSlot = release
			break
		}
	}
	if releaseSlot == nil {
		return nil, false, nil
	}
	releaseUp, ok, err := tryLockFile(a.lockPath(upLockFile), syscall.LOCK_SH)
	if err != nil || !ok {
		releaseSlot()
		return nil, false, err
	}
	// A writer that announced itself meanwhile goes first: checked again
	// while holding up.lock, so it can't miss this launch.
	if pending, err := a.writerPending(); err != nil || pending {
		releaseUp()
		releaseSlot()
		return nil, false, err
	}
	return func() { releaseUp(); releaseSlot() }, true, nil
}

// writerPending reports whether Up or Down is waiting for (or holds) the
// provisioning lock. It clears a marker left by one that died.
func (a *AWSSpot) writerPending() (bool, error) {
	marker := a.lockPath(upWriterPending)
	if _, err := os.Stat(marker); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	release, ok, err := tryLockFile(a.lockPath(upWriterLock), syscall.LOCK_EX)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil // its writer holds the lock
	}
	// Nobody holds the writer lock, so the marker's writer is gone.
	_ = os.Remove(marker)
	release()
	return false, nil
}
