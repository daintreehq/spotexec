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
// user's group (cloud-init's chgrp) can add files inside it.
//
// Everything the helper reads from or writes to LifecycleDir (boot state,
// the lifecycle lock, draining, activity, the simulator's notices) and
// everything it does only on its own VMs (sudo, the memory pool) goes
// through this check, never a bare os.Stat.
func onSpotexecVM() bool { return trustedDir(LifecycleDir, 0) }

// trustedDir reports whether path is a directory itself (not a symlink to
// one) owned by uid owner and not writable by everyone.
func trustedDir(path string, owner uint32) bool {
	fi, ok := ownedDir(path, owner)
	return ok && fi.Mode().Perm()&0o002 == 0
}

// simulated reports whether this VM is a Docker fleet simulator instance,
// whose notices come from files in SimDir instead of IMDS. The simulator's
// entrypoint makes SimDir as root with mode 1777, so world-writable is
// fine here: it is only reachable inside a trusted LifecycleDir.
func simulated() bool {
	if !onSpotexecVM() {
		return false
	}
	_, ok := ownedDir(SimDir, 0)
	return ok
}

// ownedDir reports whether path is a directory itself (not a symlink to
// one) owned by uid owner.
func ownedDir(path string, owner uint32) (fs.FileInfo, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() {
		return nil, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return fi, ok && st.Uid == owner
}

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
