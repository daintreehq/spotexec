package helper

import (
	"errors"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

const gb = 1 << 30

func TestMemShare(t *testing.T) {
	for _, c := range []struct {
		name      string
		total     int64
		slots     int
		exclusive bool
		wantMiB   int64
	}{
		// c7a.2xlarge (15.6 GiB usable), 4 slots: 1 GiB-ish reserve.
		{"16 GiB, 4 slots", 16 * gb, 4, false, (16*gb - gb) / 4 >> 20},
		{"16 GiB, exclusive", 16 * gb, 4, true, (16*gb - gb) >> 20},
		{"16 GiB, one slot", 16 * gb, 1, false, (16*gb - gb) >> 20},
		{"4 GiB: the 512 MiB floor on the reserve", 4 * gb, 2, false, (4*gb - 512<<20) / 2 >> 20},
		{"64 GiB: the 2 GiB cap on the reserve", 64 * gb, 8, false, (64*gb - 2*gb) / 8 >> 20},
		{"more slots than memory: 256 MiB floor", 2 * gb, 44, false, 256},
		{"unknown memory", 0, 4, false, 0},
	} {
		if got := memShare(c.total, c.slots, c.exclusive) >> 20; got != c.wantMiB {
			t.Errorf("%s: %d MiB, want %d", c.name, got, c.wantMiB)
		}
	}
	// Every slot's share together, plus the reserve, fits in the machine.
	for _, total := range []int64{4 * gb, 8 * gb, 15*gb + 600<<20, 32 * gb, 128 * gb} {
		for slots := 1; slots <= 16; slots++ {
			if s := memShare(total, slots, false); s > minMemShare && s*int64(slots)+memReserve(total) > total {
				t.Errorf("%d GiB, %d slots: %d MiB each overcommits", total>>30, slots, s>>20)
			}
		}
	}
}

func TestMemOption(t *testing.T) {
	for _, o := range [][]string{{"--memory=4g"}, {"--memory", "4g"}, {"-m", "4g"}, {"-m4g"}, {"-itm", "4g"}, {"--memory-swap=-1"},
		{"--memory-reservation=1g"}, {"--oom-kill-disable"}, {"--oom-score-adj=500"}, {"--kernel-memory=1g"},
		{"--rm", "-m", "1g"}, {"--label", "x", "--memory", "1g"}} {
		if memOption(o) == "" {
			t.Errorf("%v: not seen as a memory option", o)
		}
	}
	for _, o := range [][]string{nil, {"--shm-size=1g"}, {"--cpus=2"}, {"-it"}, {"--mount", "type=tmpfs"}, {"-e", "M=1"},
		{"--label", "-metrics"}, {"--label", "--memory=4g"}, {"-e", "-m"}, {"-eM=1", "--rm"}, {"--memory-swappiness=0"}} {
		if f := memOption(o); f != "" {
			t.Errorf("%v: seen as %s", o, f)
		}
	}
}

func memHostOf(total int64) memHost {
	return memHost{total: total, docker: total, driver: "systemd", v2: true, ours: true}
}

func TestPoolLimit(t *testing.T) {
	// About a c7a.2xlarge: under 16 GiB, so 1/16 reserved.
	total := int64(15)*gb + 600<<20
	if got := poolLimit(total); got != (total-total/16)&^(1<<20-1) {
		t.Errorf("15.6 GiB: %d MiB", got>>20)
	}
	if got := poolLimit(64 * gb); got != 62*gb {
		t.Errorf("64 GiB: %d MiB, want 2 GiB reserved", got>>20)
	}
}

func TestPlanMemory(t *testing.T) {
	if l, note := planMemory(memHostOf(16*gb), false, nil); l != poolLimit(16*gb) || note != "" {
		t.Errorf("shared slot: (%d, %q)", l, note)
	}
	docker := memHostOf(16 * gb)
	docker.docker = 16*gb - 200<<20 // Docker rounds what it prints
	if l, note := planMemory(docker, false, nil); l == 0 || note != "" {
		t.Errorf("docker's rounding refused: (%d, %q)", l, note)
	}
	// Alone on the machine, or with the user's own limit: no limit, nothing to note.
	for name, c := range map[string]struct {
		alone bool
		opts  []string
	}{"exclusive or one slot": {true, nil}, "user -m": {false, []string{"-m", "8g"}}, "user cgroup parent": {false, []string{"--cgroup-parent=/x"}}} {
		if l, note := planMemory(memHostOf(16*gb), c.alone, c.opts); l != 0 || note != "" {
			t.Errorf("%s: (%d, %q)", name, l, note)
		}
	}
	unknown := memHost{why: "the runner isn't Linux"}
	desktop := memHostOf(16 * gb)
	desktop.docker = 8 * gb
	noInfo := memHostOf(16 * gb)
	noInfo.docker = 0
	noSupport := memHostOf(16 * gb)
	noSupport.noLimit = true
	remote := memHostOf(16 * gb)
	remote.remote = "DOCKER_HOST points at another machine"
	notOurs := memHostOf(16 * gb)
	notOurs.ours = false
	v1 := memHostOf(16 * gb)
	v1.v2 = false
	driver := memHostOf(16 * gb)
	driver.driver = ""
	for _, c := range []struct {
		name string
		h    memHost
		note string
	}{
		{"unknown", unknown, "isn't Linux"},
		{"docker elsewhere", desktop, "docker reports 8.0 GiB of memory, the host 16.0 GiB"},
		{"docker silent", noInfo, "doesn't say how much memory"},
		{"no support", noSupport, "no memory limit support"},
		{"remote", remote, "DOCKER_HOST"},
		{"someone else's machine", notOurs, "not a spotexec VM"},
		{"cgroup v1", v1, "cgroup v2"},
		{"unknown driver", driver, "cgroup driver"},
	} {
		if l, note := planMemory(c.h, false, nil); l != 0 || !strings.Contains(note, c.note) {
			t.Errorf("%s: (%d, %q), want no limit and a note containing %q", c.name, l, note, c.note)
		}
	}
}

func TestReadMemHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	info := " Context:    default\n CPUs: 8\n Total Memory: 15.33GiB\n Cgroup Driver: systemd\n Cgroup Version: 2\n"
	h := readMemHost(info)
	v := 15.33
	if want := int64(v * gb); h.docker != want {
		t.Errorf("docker %d, want %d", h.docker, want)
	}
	if h.noLimit || h.remote != "" || h.driver != "systemd" || !h.v2 {
		t.Errorf("%+v", h)
	}
	if h := readMemHost(strings.Replace(info, "Version: 2", "Version: 1", 1)); h.v2 {
		t.Error("cgroup v1 read as v2")
	}
	if h := readMemHost(info + "WARNING: No memory limit support\n"); !h.noLimit {
		t.Errorf("warning not seen: %+v", h)
	}
	if h := readMemHost(info + "WARNING: Running in rootless-mode without cgroups.\n"); !h.noLimit {
		t.Errorf("rootless without cgroups not seen: %+v", h)
	}
	if h := readMemHost(" Total Memory: 512MiB\n"); h.docker != 512<<20 {
		t.Errorf("MiB: %d", h.docker)
	}
	if runtime.GOOS == "linux" && h.total <= 0 {
		t.Errorf("no memory size on Linux: %s", h.why)
	}
	if runtime.GOOS != "linux" && (h.total != 0 || h.why == "") {
		t.Errorf("off Linux: total %d, why %q", h.total, h.why)
	}
}

func TestPSIAvg10(t *testing.T) {
	psi := "some avg10=31.50 avg60=10.00 avg300=2.00 total=123\nfull avg10=12.25 avg60=4.00 avg300=1.00 total=99\n"
	if v := psiAvg10(psi, "full"); v != 12.25 {
		t.Errorf("full %v", v)
	}
	if v := psiAvg10(psi, "some"); v != 31.5 {
		t.Errorf("some %v", v)
	}
	if v := psiAvg10("", "full"); v != 0 {
		t.Errorf("empty %v", v)
	}
}

// A job alone on the machine (--exclusive, a single slot) or with its own
// memory options gets no memory constraint at all, on any OS: nothing that
// a runtime could size itself from (Node's heap), and no pool either.
func TestMemoryArgsAloneHasNoConstraint(t *testing.T) {
	stubPool(t, true, nil)
	out := &syncBuf{}
	for name, s := range map[string]*Session{
		"--exclusive":   {acq: protocol.Acquire{Exclusive: true}, maxSlots: 4, tokens: []*flock{nil, nil, nil, nil}, tokenIdx: 1},
		"one slot":      {maxSlots: 1, tokens: []*flock{nil}, tokenIdx: 1},
		"user --memory": {maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 2},
	} {
		s.c = protocol.NewConn(strings.NewReader(""), out)
		run := &protocol.ContainerRun{}
		if name == "user --memory" {
			run.Options = []string{"--memory=2g"}
		}
		if got := s.memoryArgs(run, dockerRunner{info: linuxInfo()}); got != nil || s.memLimit != 0 {
			t.Errorf("%s: %v, limit %d", name, got, s.memLimit)
		}
	}
	if out.b.Len() != 0 {
		t.Fatalf("events for runs that weren't due a limit: %s", out.b.String())
	}
}

// stubPool makes the host look like a spotexec VM (or not) and replaces
// the cgroup setup, which needs root.
func stubPool(t *testing.T, ours bool, err error) *[]int64 {
	oldVM, oldPool := spotexecVM, ensurePool
	t.Cleanup(func() { spotexecVM, ensurePool = oldVM, oldPool })
	var calls []int64
	spotexecVM = func() bool { return ours }
	ensurePool = func(_, driver string, limit int64) (string, error) {
		calls = append(calls, limit)
		if err != nil {
			return "", err
		}
		if driver == "systemd" {
			return "spotexec.slice", nil
		}
		return "/spotexec", nil
	}
	return &calls
}

func linuxInfo() string {
	return " Context:    default\n Total Memory: " + strconv.FormatFloat(float64(hostMemTotal())/gb, 'f', 4, 64) + "GiB\n Cgroup Driver: systemd\n Cgroup Version: 2\n"
}

// A shared slot's container goes in the pool, and gets no limit of its own.
func TestMemoryArgsSharedSlotUsesPool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/meminfo")
	}
	t.Setenv("DOCKER_HOST", "")
	calls := stubPool(t, true, nil)
	s := &Session{maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 2}
	got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{info: linuxInfo()})
	if !slices.Equal(got, []string{"--cgroup-parent", "spotexec.slice"}) {
		t.Fatalf("got %v, want only the pool's --cgroup-parent", got)
	}
	if want := poolLimit(hostMemTotal()); s.memLimit != want || len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("limit %d, pool set to %v; want %d", s.memLimit, *calls, want)
	}
	if got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{info: strings.Replace(linuxInfo(), "systemd", "cgroupfs", 1)}); !slices.Equal(got, []string{"--cgroup-parent", "/spotexec"}) {
		t.Fatalf("cgroupfs: %v", got)
	}
}

// A pool that can't be set up, or a host that isn't ours, leaves the run
// unlimited, noted once.
func TestMemoryArgsFailOpen(t *testing.T) {
	stubPool(t, true, errors.New("sudo: a password is required"))
	out := &syncBuf{}
	s := &Session{c: protocol.NewConn(strings.NewReader(""), out), maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 1}
	for range 2 {
		if got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{info: linuxInfo()}); got != nil || s.memLimit != 0 {
			t.Fatalf("got %v, limit %d", got, s.memLimit)
		}
	}
	if n := strings.Count(out.b.String(), "container_memory"); n != 1 {
		t.Fatalf("%d container_memory events, want 1: %s", n, out.b.String())
	}
}

// Commands start by undoing the helper's lowered OOM score, before
// anything else (a profile could fork a long-lived process).
func TestWrapScriptResetsOOMScore(t *testing.T) {
	old := oomLowered.Load()
	t.Cleanup(func() { oomLowered.Store(old) })
	oomLowered.Store(false)
	if w := wrapScript("true", "/w", ""); strings.Contains(w, "oom_score_adj") {
		t.Fatalf("reset without a lowered score: %q", w)
	}
	oomLowered.Store(true)
	if w := wrapScript("true", "/w", ""); !strings.HasPrefix(w, childOOMReset) {
		t.Fatalf("reset isn't first: %q", w)
	}
}

// A client that seems silent only because the helper itself was stalled
// (heartbeats still queued in the pipe) isn't taken for gone.
func TestClientGoneAfterOwnStall(t *testing.T) {
	s := newSession()
	s.lastSeen.Store(s.now() - int64(2*ClientTimeout))
	if !s.clientGone() {
		t.Fatal("silent client not gone")
	}
	s.stalledAt.Store(s.now())
	if s.clientGone() {
		t.Fatal("client taken for gone right after the helper's own stall")
	}
	s.stalledAt.Store(s.now() - int64(stallGrace+time.Second))
	if !s.clientGone() {
		t.Fatal("an old stall still excuses the client")
	}
	// Woken before the stall sampler after a stall: its tick is overdue.
	s.stallTick.Store(s.now() - int64(10*time.Second))
	if s.clientGone() {
		t.Fatal("client taken for gone before the stall sampler caught up")
	}
	// Stall after stall can't excuse a dead client forever.
	s.stalledAt.Store(s.now())
	s.lastSeen.Store(s.now() - int64(ClientTimeout+stallCap+time.Second))
	if !s.clientGone() {
		t.Fatal("stalls excuse a client silent past the cap")
	}
}

// The stall sampler notices the helper going unscheduled.
func TestWatchStalls(t *testing.T) {
	s := newSession()
	done := make(chan struct{})
	defer close(done)
	go s.watchStalls(done)
	for deadline := time.Now().Add(3 * time.Second); s.stallTick.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if s.recentlyStalled() {
		t.Fatal("stalled without a stall")
	}
}

// Liveness is measured on the session's monotonic clock: its timestamps are
// offsets from the session's start, not wall-clock instants, so NTP
// stepping a just-booted VM's clock neither ends a live client's run nor
// keeps a dead client's going.
func TestLivenessUsesTheMonotonicClock(t *testing.T) {
	s := newSession()
	if seen := time.Duration(s.lastSeen.Load()); seen <= 0 || seen > time.Minute {
		t.Fatalf("lastSeen = %v: not an offset from the session's start", seen)
	}
	// The session's start carries a monotonic reading (time.Since then
	// ignores the wall clock); one rebuilt from wall-clock nanoseconds
	// wouldn't.
	if !strings.Contains(s.born.String(), " m=") {
		t.Fatalf("born %v has no monotonic reading", s.born)
	}
	if s.clientGone() {
		t.Fatal("a client just heard from is gone")
	}
	now := s.lastSeen.Load()
	if s.silentForAt(now+int64(ClientTimeout), ClientTimeout) {
		t.Fatal("silent at the limit")
	}
	if !s.silentForAt(now+int64(ClientTimeout+time.Second), ClientTimeout) {
		t.Fatal("not silent past the limit")
	}
	// No stall yet: a fresh session's first seconds aren't excused as one.
	if s.recentlyStalledAt(now + int64(time.Second)) {
		t.Fatal("a fresh session counts as stalled")
	}
	s.stalledAt.Store(now)
	if !s.recentlyStalledAt(now+int64(stallGrace-time.Second)) || s.recentlyStalledAt(now+int64(stallGrace+time.Second)) {
		t.Fatal("stall grace misjudged")
	}
}

// /dev/shm is half the job's fair share of the daemon's memory, or 1 GiB
// when that's unknown.
func TestShmSize(t *testing.T) {
	for _, c := range []struct {
		name      string
		total     int64
		slots     int
		exclusive bool
		wantMiB   int64
	}{
		{"16 GiB, 4 slots", 16 * gb, 4, false, 1920},
		{"16 GiB, exclusive", 16 * gb, 4, true, 7680},
		{"16 GiB, one slot", 16 * gb, 1, false, 7680},
		{"4 GiB, 2 slots", 4 * gb, 2, false, 896},
		{"an odd share rounds down to a MiB", 15*gb + 601<<20, 3, false, (15*gb + 601<<20 - (15*gb+601<<20)/16) / 3 / 2 >> 20},
		{"more slots than memory: half the 256 MiB share floor", 2 * gb, 44, false, 128},
		{"unknown memory", 0, 4, false, 1024},
	} {
		got := shmSize(c.total, c.slots, c.exclusive)
		if got>>20 != c.wantMiB || got&(1<<20-1) != 0 {
			t.Errorf("%s: %d bytes, want %d MiB", c.name, got, c.wantMiB)
		}
	}
}

// A project container gets a /dev/shm sized from the daemon's memory
// unless container.options sets its own, or its own IPC.
func TestShmArgs(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	s := &Session{maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 2}
	d := dockerRunner{info: " Context:    default\n Total Memory: 16GiB\n"}
	want := []string{"--shm-size", strconv.FormatInt(shmSize(16*gb, 4, false), 10)}
	for _, o := range [][]string{nil, {"--label", "--shm-size"}, {"--label", "--ipc=host"}, {"-e", "--ipc"},
		{"--memory=4g"}, {"--cgroup-parent", "x"}, {"--cpus=2"}, {"--network", "none"},
		{"--mount", "type=tmpfs,target=/dev/shm"}, {"-v", "/dev/shm:/dev/shm"}, {"-it", "--rm"}} {
		if got := s.shmArgs(&protocol.ContainerRun{Options: o}, d); !slices.Equal(got, want) {
			t.Errorf("%v: %v, want %v", o, got, want)
		}
	}
	for _, o := range [][]string{{"--shm-size=1g"}, {"--shm-size", "1g"}, {"--ipc=host"}, {"--ipc", "private"},
		{"--ipc=shareable"}, {"--ipc", "container:x"}, {"--ipc=none"}, {"--use-api-socket", "--ipc", "host"}, {"-it", "--shm-size=2g"}} {
		if got := s.shmArgs(&protocol.ContainerRun{Options: o}, d); got != nil {
			t.Errorf("%v: %v, want none", o, got)
		}
	}
	// A job alone on the machine (--exclusive, or holding more than one
	// slot's token) gets half of all it can use.
	for name, s := range map[string]*Session{
		"--exclusive": {acq: protocol.Acquire{Exclusive: true}, maxSlots: 4, tokens: []*flock{nil}},
		"two tokens":  {maxSlots: 4, tokens: []*flock{nil, nil}},
		"one slot":    {maxSlots: 1, tokens: []*flock{nil}},
	} {
		if got := s.shmArgs(&protocol.ContainerRun{}, d); got[1] != strconv.FormatInt(shmSize(16*gb, 1, false), 10) {
			t.Errorf("%s: %v", name, got)
		}
	}
	// Without the daemon's memory, a local Linux daemon is sized from the
	// runner's; any other gets the fallback.
	wantLocal := int64(shmFallback)
	if runtime.GOOS == "linux" {
		wantLocal = shmSize(hostMemTotal(), 4, false)
	}
	if got := s.shmArgs(&protocol.ContainerRun{}, dockerRunner{info: " Context:    default\n"}); got[1] != strconv.FormatInt(wantLocal, 10) {
		t.Errorf("local daemon, memory unknown: %v, want %d", got, wantLocal)
	}
	// A remote daemon: the runner's memory isn't Docker's.
	if got := s.shmArgs(&protocol.ContainerRun{}, dockerRunner{info: " Context:    build-box\n"}); got[1] != strconv.FormatInt(shmFallback, 10) {
		t.Errorf("remote context, memory unknown: %v", got)
	}
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	if got := s.shmArgs(&protocol.ContainerRun{}, dockerRunner{}); got[1] != strconv.FormatInt(shmFallback, 10) {
		t.Errorf("remote daemon, memory unknown: %v", got)
	}
	if got := s.shmArgs(&protocol.ContainerRun{}, d); !slices.Equal(got, want) {
		t.Errorf("remote daemon that reports its memory: %v, want %v", got, want)
	}
}
