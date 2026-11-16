package helper

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// onSpotexecVM reports whether this host is one of spotexec's own VMs,
// whose cloud-init made LifecycleDir as root. /var/tmp is world-writable,
// so on a shared SSH runner any account can create that path (or a
// symlink at it): only a real directory owned by root that other users
// can't write to counts. /var/tmp is sticky, so once root owns the entry
// nobody else can rename or replace it, and only root and the runner
// user's group (cloud-init's chgrp; on spotexec's VMs that user has
// passwordless sudo anyway) can add or replace files inside it. That is
// the trust boundary: files inside are not checked one by one.
//
// Everything the helper reads from or writes to LifecycleDir (boot state,
// the lifecycle lock, draining, activity, the simulator's notices) and
// everything it does only on its own VMs (sudo, the memory pool) goes
// through this check, never a bare os.Stat.
func onSpotexecVM() bool { return trustedDir(LifecycleDir, 0) }

// trustedDir reports whether path is a directory itself (not a symlink to
// one) owned by uid owner and not writable by everyone.
func trustedDir(path string, owner uint32) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o002 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == owner
}

// simulated reports whether this VM is a Docker fleet simulator instance,
// whose notices come from files in SimDir instead of IMDS. The simulator
// makes SimDir, and writes every notice in it, as root.
func simulated() bool { return onSpotexecVM() && trustedDir(SimDir, 0) }

// lifecycleFile reads a file cloud-init keeps in LifecycleDir. Off
// spotexec's VMs there is none, and nothing there is opened.
func lifecycleFile(name string) ([]byte, error) {
	if !onSpotexecVM() {
		return nil, fs.ErrNotExist
	}
	return os.ReadFile(filepath.Join(LifecycleDir, name))
}

// hasLifecycleLock reports whether this VM has the lifecycle lock the idle
// watchdog takes exclusively before it shuts the VM down.
func hasLifecycleLock() bool {
	if !onSpotexecVM() {
		return false
	}
	_, err := os.Stat(LifecycleLock)
	return err == nil
}
