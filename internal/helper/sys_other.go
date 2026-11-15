//go:build !linux

package helper

import (
	"encoding/binary"
	"io/fs"
	"syscall"
	"time"
)

// protectControlPlane needs /proc and Linux's OOM killer: a no-op here.
func protectControlPlane() {}

func resetOwnOOMScore() {}

// hostMemTotal is the machine's memory in bytes (hw.memsize), or 0.
func hostMemTotal() int64 {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil || len(s) == 0 || len(s) > 8 {
		return 0
	}
	b := make([]byte, 8) // Sysctl drops trailing NUL bytes: the high zero bytes here
	copy(b, s)
	return int64(binary.LittleEndian.Uint64(b))
}

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
