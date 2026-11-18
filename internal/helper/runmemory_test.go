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
	old, oldPlanned := poolCgroups, poolPlanned
	poolCgroups, poolPlanned = []string{dir}, func() bool { return false }
	t.Cleanup(func() { poolCgroups, poolPlanned = old, oldPlanned })
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
	for _, f := range [][2]string{{"memory.current", fmt.Sprint(cur)}, {"memory.stat", stat}} {
		if err := os.WriteFile(filepath.Join(dir, f[0]), []byte(f[1]), 0o644); err != nil {
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
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	setCgroup(t, elsewhere, 6000, 0)
	writeJSON(a.layout.RunMem(a.runIdx), runMemRecord{Pooled: true, ReserveMB: 8000, Cgroup: elsewhere})
	setCgroup(t, pool, 6000, 0)
	if why := b.memoryWait(2); why == "" {
		t.Fatal("b admitted on a cgroup outside the pool")
	}
	// A record a dead helper left says nothing: a's permit reserves the
	// fair share (5000), whatever the record claims.
	writeJSON(a.layout.RunMem(a.runIdx), runMemRecord{Pooled: false, PID: 1 << 30})
	setCgroup(t, pool, 7500, 0)
	if why := b.memoryWait(2); why == "" {
		t.Fatal("b admitted on a dead helper's record: 7500 used + 5000 to come leave 7500")
	}
	setCgroup(t, pool, 7000, 0)
	if why := b.memoryWait(2); why != "" {
		t.Fatalf("b refused with exactly its 8000 left: %s", why)
	}
}

// A command with no other command running starts however much it needs;
// one with an unknown peak reserves the fair share; a command outside the
// pool takes a permit as before.
func TestMemoryWaitBounds(t *testing.T) {
	root := t.TempDir()
	pool := fakePool(t, 16000, 0)
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
	// Unknown peaks: 4000 each (16000 ÷ 4 slots); b, outside the pool,
	// reserves nothing. c fits while 8000 is left after a's 4000 to come,
	// not at 7999.
	a.runMem.ReserveMB = 0
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}
	setCgroup(t, pool, 4000, 0)
	if why := c.memoryWait(3); why != "" {
		t.Fatalf("c refused with 8000 left beside one unknown command: %s", why)
	}
	setCgroup(t, pool, 4001, 0)
	c.runMem.ReserveMB = 8000
	if why := c.memoryWait(3); why == "" {
		t.Fatal("c admitted with 7999 left")
	}
	c.runMem.ReserveMB = 0
	if why := c.memoryWait(3); why != "" {
		t.Fatalf("an unknown c (4000) refused with 7999 left: %s", why)
	}
}

// #294 on a fresh VM: no job has run in the pool yet, so its cgroup doesn't
// exist, but its commands will run in it. Its planned cap applies.
func TestMemoryWaitBeforeThePoolExists(t *testing.T) {
	root := t.TempDir()
	fakePool(t, 1, 0)
	poolCgroups = []string{filepath.Join(t.TempDir(), "spotexec.slice")}
	poolPlanned = func() bool { return true }
	oldTotal := poolMemTotal
	poolMemTotal = func() int64 { return 32 << 30 } // a 30 GiB pool
	t.Cleanup(func() { poolMemTotal = oldTotal })
	jobs := pooledJobs(t, root, 4, 6, 8192)
	for _, s := range jobs[:3] {
		if !s.runMem.Pooled {
			t.Fatal("a container job on a VM whose pool isn't set up yet planned outside it")
		}
		if _, err := s.takeRun(); err != nil {
			t.Fatal(err)
		}
	}
	if why := jobs[3].memoryWait(4); !strings.Contains(why, "waiting for memory") {
		t.Fatalf("the fourth 8 GiB job on a 30 GiB pool: %q", why)
	}
	poolPlanned = func() bool { return false }
	jobs[3].planRunMemory(protocol.Exec{Container: &protocol.ContainerRun{Image: "img"}, MemoryMB: 8192})
	if jobs[3].runMem.Pooled || jobs[3].memoryWait(4) != "" {
		t.Fatal("a runner that will never have a pool held a command back")
	}
}

// A job of an older helper, holding its slot for its whole lease, runs its
// command without a permit: it still reserves the fair share.
func TestMemoryWaitCountsOlderHelpersJobs(t *testing.T) {
	root := t.TempDir()
	fakePool(t, 16000, 0)
	l := Layout{Root: root}
	old, ok, err := tryLock(l.TokenLock(1))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer old.Unlock()
	if err := writeJSON(l.TokenHolder(1), protocol.Holder{Token: 1, RepoID: "old", HelperPID: 1}); err != nil {
		t.Fatal(err)
	}
	a := pooledJobs(t, root, 1, 4, 12001)[0]
	if why := a.memoryWait(2); why == "" {
		t.Fatal("12001 started beside an older job's 4000 in a 16000 pool")
	}
	a.runMem.ReserveMB = 12000
	if why := a.memoryWait(2); why != "" {
		t.Fatalf("12000 refused beside an older job's 4000 in a 16000 pool: %s", why)
	}
}

// The sampler finds the container's cgroup from the ID docker writes,
// records it with the permit, and reports the most it used.
func TestWatchMemory(t *testing.T) {
	root := t.TempDir()
	pool := fakePool(t, 16000, 0)
	old := memSampleEvery
	memSampleEvery = 5 * time.Millisecond
	t.Cleanup(func() { memSampleEvery = old })
	s := pooledJobs(t, root, 1, 2, 1000)[0]
	if _, err := s.takeRun(); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 64)
	cid := filepath.Join(t.TempDir(), "c.cid")
	stop := s.watchMemory(cid, s.runIdx)
	cg := filepath.Join(pool, id) // cgroupfs layout
	setCgroup(t, cg, 700, 0)
	os.WriteFile(cid, []byte(id), 0o644)
	var r runMemRecord
	eventually(t, "the cgroup to be recorded", func() bool {
		return readJSON(s.layout.RunMem(s.runIdx), &r) == nil && r.Cgroup == cg
	})
	if !r.Pooled || r.ReserveMB != 1000 || r.PID != os.Getpid() {
		t.Fatalf("record %+v", r)
	}
	setCgroup(t, cg, 1500, 0)
	time.Sleep(100 * time.Millisecond)
	setCgroup(t, cg, 200, 0)
	if got := stop(); got != 1500 {
		t.Fatalf("peak %d MiB, want 1500", got)
	}
	s.releaseRun()
	if _, err := os.Stat(s.layout.RunMem(1)); !os.IsNotExist(err) {
		t.Fatalf("the permit's memory record outlived it: %v", err)
	}
	// An ID that isn't a container's is never looked up, even where a
	// cgroup of that name exists.
	setCgroup(t, filepath.Join(pool, "notanid"), 1, 0)
	os.WriteFile(cid, []byte("notanid"), 0o644)
	if d := containerCgroup(cid); d != "" {
		t.Fatalf("resolved %q", d)
	}
}

// Session teardown (the client gone, the runner stopping) leaves no
// reservation behind with the permit it gives back.
func TestReleaseDropsTheMemoryRecord(t *testing.T) {
	root := t.TempDir()
	fakePool(t, 16000, 0)
	s := pooledJobs(t, root, 1, 2, 1000)[0]
	if _, err := s.takeRun(); err != nil {
		t.Fatal(err)
	}
	rec := s.layout.RunMem(s.runIdx)
	if _, err := os.Stat(rec); err != nil {
		t.Fatal(err)
	}
	s.release()
	if _, err := os.Stat(rec); !os.IsNotExist(err) {
		t.Fatalf("record after teardown: %v", err)
	}
}

// The pool's cgroup exists before its cap is written (cgroupfs: mkdir,
// then memory.max): its planned cap applies meanwhile.
func TestMemoryWaitWhileThePoolIsSetUp(t *testing.T) {
	root := t.TempDir()
	pool := fakePool(t, 1, 0)
	os.WriteFile(filepath.Join(pool, "memory.max"), []byte("max\n"), 0o644)
	poolPlanned = func() bool { return true }
	oldTotal := poolMemTotal
	poolMemTotal = func() int64 { return 32 << 30 }
	t.Cleanup(func() { poolMemTotal = oldTotal })
	jobs := pooledJobs(t, root, 4, 6, 8192)
	for _, s := range jobs[:3] {
		if _, err := s.takeRun(); err != nil {
			t.Fatal(err)
		}
	}
	if why := jobs[3].memoryWait(4); why == "" {
		t.Fatal("the fourth 8 GiB job started while the 30 GiB pool's cap was being set")
	}
}

// A reservation that can't be recorded refuses the permit (the next job
// couldn't count it), and leaves it free.
func TestUnrecordedReservationRefusesThePermit(t *testing.T) {
	root := t.TempDir()
	fakePool(t, 16000, 0)
	s := pooledJobs(t, root, 1, 2, 1000)[0]
	blocker := filepath.Join(s.layout.RunMem(1), "x")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.takeRun(); err == nil || s.run != nil {
		t.Fatalf("permit %d taken without its record (%v)", s.runIdx, err)
	}
	if isHeld(s.layout.RunLock(1)) {
		t.Fatal("the refused permit stayed held")
	}
	os.RemoveAll(s.layout.RunMem(1))
	if _, err := s.takeRun(); err != nil || s.runIdx != 1 {
		t.Fatalf("permit 1 afterwards: %d, %v", s.runIdx, err)
	}
}

// A sampler that finds its cgroup only after its permit went to another
// job leaves that job's record alone.
func TestLateSamplerKeepsOffAnotherJobsPermit(t *testing.T) {
	root := t.TempDir()
	pool := fakePool(t, 16000, 0)
	jobs := pooledJobs(t, root, 2, 2, 1000)
	a, b := jobs[0], jobs[1]
	if _, err := a.takeRun(); err != nil {
		t.Fatal(err)
	}
	cid := filepath.Join(t.TempDir(), "c.cid")
	stop := a.watchMemory(cid, a.runIdx)
	a.releaseRun()
	b.runMem.ReserveMB = 2222
	if _, err := b.takeRun(); err != nil || b.runIdx != 1 {
		t.Fatalf("b took permit %d: %v", b.runIdx, err)
	}
	id := strings.Repeat("c", 64)
	setCgroup(t, filepath.Join(pool, id), 100, 0)
	os.WriteFile(cid, []byte(id), 0o644)
	stop() // samples once more: finds the cgroup now
	var r runMemRecord
	if err := readJSON(b.layout.RunMem(1), &r); err != nil || r.ReserveMB != 2222 || r.Cgroup != "" {
		t.Fatalf("b's record %+v (%v)", r, err)
	}
}
