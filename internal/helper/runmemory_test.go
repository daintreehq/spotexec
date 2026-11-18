package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

const mib = int64(1) << 20

// fakePool makes a memory pool cgroup capped at limitMB, using usedMB (no
// page cache), and points the helper at it.
func fakePool(t *testing.T, limitMB, usedMB int64) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spotexec.slice")
	setCgroup(t, dir, usedMB, 0)
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(fmt.Sprint(limitMB*mib)), 0o644); err != nil {
		t.Fatal(err)
	}
	old := poolCgroups
	poolCgroups = []string{dir}
	t.Cleanup(func() { poolCgroups = old })
	return dir
}

// setCgroup writes a cgroup's memory.current and memory.stat: usedMB that
// can't be reclaimed, plus cacheMB of page cache.
func setCgroup(t *testing.T, dir string, usedMB, cacheMB int64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cur := (usedMB + cacheMB) * mib
	stat := fmt.Sprintf("anon %d\nfile %d\nkernel 0\nshmem %d\n", usedMB*mib, cacheMB*mib, 0)
	for f, v := range map[string]string{"memory.current": fmt.Sprint(cur), "memory.stat": stat} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// pooledJobs acquires n jobs on a runner of slots slots at root, each
// planning a command in a container that needs memMB at its peak.
func pooledJobs(t *testing.T, root string, n, slots int, memMB int64) []*Session {
	t.Helper()
	var out []*Session
	for i := 0; i < n; i++ {
		s := testSession(t, root)
		if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: slots, Overflow: true}); err != nil || s.slotLock == nil {
			t.Fatalf("job %d not admitted: %v", i+1, err)
		}
		s.planRunMemory(protocol.Exec{Container: &protocol.ContainerRun{Image: "img"}, MemoryMB: memMB})
		out = append(out, s)
	}
	return out
}

func TestCgroupUsedLeavesPageCacheOut(t *testing.T) {
	dir := t.TempDir()
	setCgroup(t, dir, 3000, 5000)
	if got, ok := cgroupUsed(dir); !ok || got != 3000*mib {
		t.Fatalf("used %d MiB (%v), want 3000", got/mib, ok)
	}
	// /dev/shm files are in file, but can't be reclaimed: they count.
	os.WriteFile(filepath.Join(dir, "memory.stat"), []byte(fmt.Sprintf("file %d\nshmem %d\n", 5000*mib, 1000*mib)), 0o644)
	if got, _ := cgroupUsed(dir); got != 4000*mib {
		t.Fatalf("used %d MiB with 1000 MiB of shmem, want 4000", got/mib)
	}
	if _, ok := cgroupUsed(filepath.Join(dir, "gone")); ok {
		t.Fatal("a missing cgroup read as used")
	}
}

// #294: four typechecks that each reach 8 GiB, on a VM whose pool is
// 28.7 GiB, started while the VM was idle. With their peaks known, three
// start (24 GiB reserved), and the fourth waits for memory, though slots
// are free, until one ends.
func TestRunPermitWaitsForMemory(t *testing.T) {
	root := t.TempDir()
	fakePool(t, 29388, 1024)
	jobs := pooledJobs(t, root, 4, 6, 8192)
	for _, s := range jobs[:3] {
		if canceled, err := s.takeRun(); err != nil || canceled {
			t.Fatalf("one of the first three didn't start: %v", err)
		}
	}
	d := jobs[3]
	if why := d.memoryWait(4); !strings.Contains(why, "waiting for memory") || !strings.Contains(why, "8.0 GiB") {
		t.Fatalf("fourth job's wait: %q", why)
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.takeRun()
		done <- err
	}()
	eventually(t, "the fourth job to wait", func() bool { return holderOf(t, root, d.tokenIdx).Phase == protocol.PhaseRunWait })
	time.Sleep(3 * runPoll)
	if h := holderOf(t, root, d.tokenIdx); h.Phase != protocol.PhaseRunWait {
		d.frames <- cancelFrame(t)
		<-done
		t.Fatalf("fourth job started into a pool it would overflow: %+v", h)
	}
	freed := jobs[0].runIdx
	jobs[0].releaseRun()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		d.frames <- cancelFrame(t)
		<-done
		t.Fatal("the fourth job didn't start once one ended")
	}
	var r runMemRecord
	if d.runIdx != freed || readJSON(d.layout.RunMem(freed), &r) != nil || r.ReserveMB != 8192 || !r.Pooled {
		t.Fatalf("fourth job holds permit %d (freed %d), reserving %+v", d.runIdx, freed, r)
	}
}

// A running command's reservation shrinks by what it already uses, and
// memory used outside any command (a dependency install, a command over
// its estimate) counts too.
func TestMemoryWaitCountsGrowthNotUse(t *testing.T) {
	root := t.TempDir()
	pool := fakePool(t, 20000, 0)
	jobs := pooledJobs(t, root, 2, 4, 8000)
	a, b := jobs[0], jobs[1]
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}
	// a will grow to 8000: 12000 left, b fits.
	if why := b.memoryWait(2); why != "" {
		t.Fatalf("b refused with 12000 MiB left: %s", why)
	}
	// a already uses 6000 of its 8000 (the pool too): still 12000 left.
	cg := filepath.Join(pool, "docker-"+strings.Repeat("a", 64)+".scope")
	setCgroup(t, cg, 6000, 4000)
	setCgroup(t, pool, 6000, 4000)
	writeJSON(a.layout.RunMem(a.runIdx), runMemRecord{Pooled: true, ReserveMB: 8000, Cgroup: cg})
	if why := b.memoryWait(2); why != "" {
		t.Fatalf("b refused with a's use counted twice: %s", why)
	}
	// Something else in the pool (a dependency install) uses 5000 more.
	setCgroup(t, pool, 11000, 4000)
	if why := b.memoryWait(2); why == "" {
		t.Fatal("b admitted with 7000 MiB left")
	}
	// A cgroup outside the pool is never read: a's use then doesn't count
	// against its reservation, and 6000 used plus 8000 to come leave b
	// too little.
	writeJSON(a.layout.RunMem(a.runIdx), runMemRecord{Pooled: true, ReserveMB: 8000, Cgroup: filepath.Join(t.TempDir(), "elsewhere")})
	setCgroup(t, pool, 6000, 0)
	if why := b.memoryWait(2); why == "" {
		t.Fatal("b admitted on a cgroup outside the pool")
	}
}

// A command with no other command running starts however much it needs;
// one with an unknown peak reserves the fair share; a command outside the
// pool, or on a VM without one, takes a permit as before.
func TestMemoryWaitBounds(t *testing.T) {
	root := t.TempDir()
	fakePool(t, 16000, 0)
	jobs := pooledJobs(t, root, 3, 4, 0)
	a, b, c := jobs[0], jobs[1], jobs[2]
	a.runMem.ReserveMB = 50000
	if why := a.memoryWait(1); why != "" {
		t.Fatalf("a lone command waited: %s", why)
	}
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}
	if why := b.memoryWait(2); why == "" {
		t.Fatal("b started beside a command expected to take more than the pool")
	}
	b.planRunMemory(protocol.Exec{Shell: "true"})
	if b.runMem.Pooled || b.memoryWait(2) != "" {
		t.Fatalf("a command outside the pool waited for memory: %+v", b.runMem)
	}
	if _, err := b.takeRun(); err != nil {
		t.Fatal(err)
	}
	a.releaseRun()
	// Unknown peaks: 4000 each (16000 ÷ 4 slots); b reserves nothing.
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}
	a.runMem.ReserveMB = 0
	writeJSON(a.layout.RunMem(a.runIdx), a.runMem)
	if why := c.memoryWait(3); why != "" {
		t.Fatalf("c refused beside one unknown command: %s", why)
	}
	poolCgroups = []string{filepath.Join(t.TempDir(), "none")}
	c.planRunMemory(protocol.Exec{Container: &protocol.ContainerRun{Image: "img"}, MemoryMB: 99999})
	if c.runMem.Pooled || c.memoryWait(3) != "" {
		t.Fatal("a VM without a pool held a command back")
	}
}

// The sampler finds the container's cgroup from the ID docker writes,
// records it with the permit, and reports the most it used.
func TestWatchMemory(t *testing.T) {
	root := t.TempDir()
	pool := fakePool(t, 16000, 0)
	old := memSampleEvery
	memSampleEvery = 10 * time.Millisecond
	t.Cleanup(func() { memSampleEvery = old })
	s := pooledJobs(t, root, 1, 2, 1000)[0]
	if _, err := s.takeRun(); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 64)
	cid := filepath.Join(t.TempDir(), "c.cid")
	stop := s.watchMemory(cid, s.runIdx)
	os.WriteFile(cid, []byte(id), 0o644)
	cg := filepath.Join(pool, id) // cgroupfs layout
	setCgroup(t, cg, 700, 3000)
	var r runMemRecord
	eventually(t, "the cgroup to be recorded", func() bool {
		return readJSON(s.layout.RunMem(s.runIdx), &r) == nil && r.Cgroup == cg
	})
	if !r.Pooled || r.ReserveMB != 1000 {
		t.Fatalf("record %+v", r)
	}
	setCgroup(t, cg, 1500, 0)
	time.Sleep(50 * time.Millisecond)
	setCgroup(t, cg, 200, 0)
	if got := stop(); got != 1500 {
		t.Fatalf("peak %d MiB, want 1500", got)
	}
	s.releaseRun()
	if _, err := os.Stat(s.layout.RunMem(1)); !os.IsNotExist(err) {
		t.Fatalf("the permit's memory record outlived it: %v", err)
	}
	// An ID that isn't one is never looked up.
	os.WriteFile(cid, []byte("../../etc"), 0o644)
	if d := containerCgroup(cid); d != "" {
		t.Fatalf("resolved %q", d)
	}
}
