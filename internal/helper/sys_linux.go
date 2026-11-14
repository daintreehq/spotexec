//go:build linux

package helper

import (
	"bytes"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func inoCtime(fi fs.FileInfo) (uint64, int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, st.Ctim.Sec*1e9 + st.Ctim.Nsec
	}
	return 0, 0
}

func childSysProcAttr() *syscall.SysProcAttr {
	// Pdeathsig kills the direct child if the helper dies abruptly; the rest
	// of the group is reaped on the next acquire of this slot.
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// groupInDir checks (via /proc) that the group leader's cwd lies inside dir,
// so a recycled PGID belonging to an unrelated process is never killed.
func groupInDir(pgid int, dir string) bool {
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pgid) + "/cwd")
	if err != nil {
		// Leader gone; check any member of the group.
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if pg, err := syscall.Getpgid(pid); err == nil && pg == pgid {
				if c, err := os.Readlink("/proc/" + e.Name() + "/cwd"); err == nil && strings.HasPrefix(c, dir) {
					return true
				}
			}
		}
		return false
	}
	return strings.HasPrefix(cwd, dir)
}

// reapWorkspace kills processes (owned by us) left by runs in dir: those
// whose cwd is inside it, and those a run there started (its
// SPOTEXEC_WORKSPACE in their environment), which a background process
// keeps after changing directory. It returns the pids still alive after
// SIGKILL.
func reapWorkspace(dir string, grace time.Duration) []int {
	marker := []byte("SPOTEXEC_WORKSPACE=" + dir + "\x00")
	find := func() []int {
		var pids []int
		ents, _ := os.ReadDir("/proc")
		self := os.Getpid()
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == self {
				continue
			}
			c, err := os.Readlink("/proc/" + e.Name() + "/cwd")
			if err == nil && (c == dir || strings.HasPrefix(c, dir+"/")) {
				pids = append(pids, pid)
				continue
			}
			if env, err := os.ReadFile("/proc/" + e.Name() + "/environ"); err == nil && (bytes.HasPrefix(env, marker) || bytes.Contains(env, append([]byte{0}, marker...))) {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	pids := find()
	if len(pids) == 0 {
		return nil
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && len(find()) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range find() {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	for i := 0; i < 40; i++ {
		if pids = find(); len(pids) == 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pids
}
