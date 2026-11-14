//go:build !linux

package helper

import (
	"io/fs"
	"syscall"
	"time"
)

func inoCtime(fi fs.FileInfo) (uint64, int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
	}
	return 0, 0
}

func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// groupInDir cannot verify ownership without /proc, so orphan reaping is
// disabled off Linux rather than risk killing a recycled PGID.
func groupInDir(pgid int, dir string) bool { return false }

// reapWorkspace is a no-op without /proc.
func reapWorkspace(dir string, grace time.Duration) []int { return nil }
