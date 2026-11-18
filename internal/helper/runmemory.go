package helper

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
// with its run permit: its expected peak, if it runs in the pool.
func (s *Session) planRunMemory(e protocol.Exec) {
	alone := s.acq.Exclusive || len(s.tokens) > 1 || s.maxSlots <= 1
	s.runMem = runMemRecord{
		Pooled:    e.Container != nil && !alone && memOption(e.Container.Options) == "" && poolDir() != "",
		ReserveMB: max(e.MemoryMB, 0),
	}
}

// memoryWait says why this session's command can't start yet for memory,
// or "" when it can: called under the run gate, with permit (the one it
// would take) free. Anything it can't read lets the command start, as a
// run never fails for the pool (see memoryArgs).
func (s *Session) memoryWait(permit int) string {
	if !s.runMem.Pooled {
		return ""
	}
	pool := poolDir()
	limit, ok := cgroupLimit(pool)
	if !ok {
		return ""
	}
	used, ok := cgroupUsed(pool)
	if !ok {
		return ""
	}
	share := max(limit/int64(max(s.maxSlots, 1))>>20, 1)
	others := 0
	var growth int64
	for j := 1; j <= s.maxSlots; j++ {
		if j == permit || !isHeld(s.layout.RunLock(j)) {
			continue
		}
		var r runMemRecord
		if err := readJSON(s.layout.RunMem(j), &r); err == nil && !r.Pooled {
			continue
		}
		// A permit without a record (an older helper's) reserves the fair
		// share, as does a command whose peak is unknown.
		others++
		reserve := r.ReserveMB
		if reserve <= 0 {
			reserve = share
		}
		var u int64
		if r.Cgroup != "" && strings.HasPrefix(r.Cgroup, pool+"/") {
			if b, ok := cgroupUsed(r.Cgroup); ok {
				u = b >> 20
			}
		}
		growth += max(reserve-u, 0)
	}
	if others == 0 {
		return ""
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
