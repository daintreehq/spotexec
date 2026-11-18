package helper

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Memory at the run permit (#294). Since jobs share every CPU, a few
// memory-heavy commands can fit a VM's CPUs while together they don't fit
// its memory pool: four typechecks, each idle at the start and 7-8 GiB two
// minutes later, all started within a minute and the kernel killed one.
// Memory free when a command starts says little about memory free a minute
// later, so each command's peak is measured (the most its container was
// seen using in the pool), the client remembers it per workload and sends
// it with the next run of it (Exec.MemoryMB), and a command reserves it
// with its run permit. A command starts only when its own expected peak
// fits in what the pool has left once every running command has grown to
// its own:
//
//	need <= pool.max - pool.used - Σ max(0, reservedᵢ - usedᵢ)
//
// Used memory leaves page cache out (the kernel reclaims it before it
// kills anything), so a pool full of cache from earlier runs isn't full.
// A command without a measured peak (its first run, or a command line that
// differs every time) reserves the fair share (the pool ÷ max_slots), as
// SPOTEXEC_MEMORY tells it to stay within. A command with no other command
// running always starts, however much it needs: nothing waiting would give
// it more room. Commands outside the pool (on the runner itself, or in a
// container with memory options of its own) take permits as before.

var (
	// poolCgroups are where the memory pool's cgroup is, for Docker's
	// systemd and cgroupfs drivers (see ensurePool).
	poolCgroups = []string{"/sys/fs/cgroup/spotexec.slice", "/sys/fs/cgroup/spotexec"}
	// memSampleEvery is how often a running command's memory is sampled.
	memSampleEvery = 2 * time.Second
	// poolMemTotal is the machine's memory, for the pool's planned cap
	// before its cgroup exists.
	poolMemTotal = hostMemTotal
)

// runMemRecord is what a run permit's holder reserves in the memory pool
// (Layout.RunMem).
type runMemRecord struct {
	// Pooled: the command runs in the memory pool. One outside it (an
	// older helper's, or a command on the runner itself) reserves nothing
	// there.
	Pooled bool `json:"pooled"`
	// ReserveMB is its expected peak (Exec.MemoryMB); 0 when unknown: the
	// fair share then.
	ReserveMB int64 `json:"reserve_mb,omitempty"`
	// Cgroup is its running container's cgroup, once known, for what it
	// already uses of its reservation.
	Cgroup string `json:"cgroup,omitempty"`
	// PID is the helper that wrote it: a record whose helper is gone was
	// left by one that died, and says nothing of the permit's holder.
	PID int `json:"pid,omitempty"`
}

// poolDir is the memory pool's cgroup, or "" while there is none (not a
// spotexec VM, or no job has run in the pool yet).
func poolDir() string {
	for _, d := range poolCgroups {
		if _, err := os.Stat(filepath.Join(d, "memory.max")); err == nil {
			return d
		}
	}
	return ""
}

// cgroupLimit is a cgroup's memory.max in bytes; false when it has none
// or it can't be read.
func cgroupLimit(dir string) (int64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil && v > 0
}

// cgroupUsed is the memory a cgroup uses in bytes, page cache left out:
// memory.current less memory.stat's file (which includes shmem: /dev/shm
// and tmpfs files, which can't be reclaimed without swap, so they count).
// false when it can't be read (the cgroup is gone).
func cgroupUsed(dir string) (int64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	cur, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	f, err := os.Open(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return cur, true
	}
	defer f.Close()
	var file, shmem int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), " ")
		switch k {
		case "file":
			file, _ = strconv.ParseInt(v, 10, 64)
		case "shmem":
			shmem, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return max(cur-file+shmem, 0), true
}

var containerIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// containerCgroup is the cgroup of the container whose ID docker wrote to
// cidFile, in the memory pool; "" until it exists.
func containerCgroup(cidFile string) string {
	b, err := os.ReadFile(cidFile)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(b))
	pool := poolDir()
	if !containerIDRe.MatchString(id) || pool == "" {
		return ""
	}
	for _, d := range []string{filepath.Join(pool, "docker-"+id+".scope"), filepath.Join(pool, id)} {
		if _, err := os.Stat(filepath.Join(d, "memory.current")); err == nil {
			return d
		}
	}
	return ""
}

// planRunMemory records what e's command will reserve in the memory pool
// with its run permit: its expected peak, if it runs in the pool (the pool
// is set up on spotexec's own Linux VMs, by the first container to run in
// it, so it may not exist yet).
func (s *Session) planRunMemory(e protocol.Exec) {
	alone := s.acq.Exclusive || len(s.tokens) > 1 || s.maxSlots <= 1
	s.runMem = runMemRecord{
		Pooled:    e.Container != nil && !alone && memOption(e.Container.Options) == "" && (poolDir() != "" || poolPlanned()),
		ReserveMB: max(e.MemoryMB, 0),
		PID:       os.Getpid(),
	}
}

// poolPlanned reports whether this runner will have a memory pool once a
// job runs in it: one of spotexec's own Linux VMs (see planMemory).
var poolPlanned = func() bool { return runtime.GOOS == "linux" && spotexecVM() }

// memGateBusy is memoryWait's (and tryRun's) answer when another job held
// the run gate: nothing was learned.
const memGateBusy = "\x00busy"

// memoryWait says why this session's command can't start yet for memory,
// or "" when it can: called under the run gate, with permit (the one it
// would take) free. Anything it can't read lets the command start, as a
// run never fails for the pool (see memoryArgs).
func (s *Session) memoryWait(permit int) string {
	if !s.runMem.Pooled {
		return ""
	}
	pool := poolDir()
	var limit int64
	if pool != "" {
		l, ok := cgroupLimit(pool)
		if !ok {
			return ""
		}
		limit = l
	} else if total := poolMemTotal(); total > 0 {
		limit = poolLimit(total) // not set up yet: nothing runs in it
	} else {
		return ""
	}
	share := max(limit/int64(max(s.maxSlots, 1))>>20, 1)
	others := 0
	var growth int64
	// Each command's use is read before the pool's: one growing meanwhile
	// then counts in the pool's use too, never as room.
	for j := 1; j <= s.maxSlots; j++ {
		if j == permit || !isHeld(s.layout.RunLock(j)) {
			continue
		}
		var r runMemRecord
		if err := readJSON(s.layout.RunMem(j), &r); err != nil || (r.PID != 0 && !helperAlive(r.PID)) {
			// An older helper's permit, or a record a helper that died
			// left: its command reserves the fair share.
			r = runMemRecord{Pooled: true}
		}
		if !r.Pooled {
			continue
		}
		others++
		reserve := r.ReserveMB
		if reserve <= 0 {
			reserve = share
		}
		var u int64
		if pool != "" && r.Cgroup != "" && strings.HasPrefix(r.Cgroup, pool+"/") {
			if b, ok := cgroupUsed(r.Cgroup); ok {
				u = b >> 20
			}
		}
		growth += max(reserve-u, 0)
	}
	// A job of an older helper holding its slot for its whole lease runs
	// its command without a permit: the fair share too.
	for _, h := range readHolders(s.layout) {
		if !h.Phased && h.HelperPID != 0 {
			others++
			growth += share
		}
	}
	if others == 0 {
		return ""
	}
	var used int64
	if pool != "" {
		u, ok := cgroupUsed(pool)
		if !ok {
			return ""
		}
		used = u
	}
	need := s.runMem.ReserveMB
	if need <= 0 {
		need = share
	}
	room := (limit-used)>>20 - growth
	if need <= room {
		return ""
	}
	return fmt.Sprintf("waiting for memory: the command needs about %s, and the %d command(s) running leave %s of the %s memory pool once they reach their usual peaks",
		gib(need<<20), others, gib(max(room, 0)<<20), gib(limit))
}

// helperAlive reports whether process pid still runs.
func helperAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// watchMemory samples the memory of the container whose ID docker writes
// to cidFile, in the pool, until the returned stop is called, which
// returns the most it was seen using (MiB; 0 if never seen). Its cgroup is
// recorded with run permit permit, once known, so other jobs deciding
// whether to start see how much of its reservation it already uses.
func (s *Session) watchMemory(cidFile string, permit int) (stop func() int64) {
	quit, done := make(chan struct{}), make(chan int64, 1)
	rec := s.runMem
	go func() {
		var dir string
		var peak int64
		sample := func() {
			if dir == "" {
				if dir = containerCgroup(cidFile); dir != "" && permit > 0 {
					rec.Cgroup = dir
					_ = writeJSON(s.layout.RunMem(permit), rec)
				}
			}
			if dir != "" {
				if u, ok := cgroupUsed(dir); ok {
					peak = max(peak, u)
				}
			}
		}
		t := time.NewTicker(memSampleEvery)
		defer t.Stop()
		sample()
		for {
			select {
			case <-quit:
				sample()
				done <- peak >> 20
				return
			case <-t.C:
				sample()
			}
		}
	}()
	return func() int64 {
		close(quit)
		return <-done
	}
}
