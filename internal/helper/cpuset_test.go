package helper

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestParseFormatCPUList(t *testing.T) {
	for in, want := range map[string]string{
		"0-7\n":       "0-7",
		"0":           "0",
		"0-3,8-11":    "0-3,8-11",
		"0,2,4":       "0,2,4",
		"3,0-2":       "0-3",
		"0-1,1-2,5":   "0-2,5",
		" 0-15\n":     "0-15",
		"2-3,6,10-11": "2-3,6,10-11",
	} {
		cpus, err := parseCPUList(in)
		if err != nil {
			t.Fatalf("parseCPUList(%q): %v", in, err)
		}
		if got := formatCPUList(cpus); got != want {
			t.Errorf("parseCPUList(%q) formats as %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "\n", "a", "3-1", "-1", "0-", "0,,1", "0-99999"} {
		if _, err := parseCPUList(bad); err == nil {
			t.Errorf("parseCPUList(%q) accepted", bad)
		}
	}
}

func seq(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func host(n int) cpuHost { return cpuHost{cpus: seq(n), numCPU: n, docker: n} }

func TestPlanCPUSetRanges(t *testing.T) {
	cases := []struct {
		name         string
		h            cpuHost
		token, slots int
		want         string
	}{
		{"8 vCPUs 4 slots, first", host(8), 1, 4, "0-1"},
		{"8 vCPUs 4 slots, last", host(8), 4, 4, "6-7"},
		{"8 vCPUs 2 slots", host(8), 2, 2, "4-7"},
		{"uneven: 10 vCPUs 4 slots, first gets a leftover CPU", host(10), 1, 4, "0-2"},
		{"uneven: 10 vCPUs 4 slots, last gets 8-9", host(10), 4, 4, "8-9"},
		{"uneven: 6 vCPUs 4 slots, second gets 2-3", host(6), 2, 4, "2-3"},
		{"uneven: 6 vCPUs 4 slots, last gets 5", host(6), 4, 4, "5"},
		{"uneven: 8 vCPUs 6 slots, last gets 7", host(8), 6, 6, "7"},
		{"uneven: 8 vCPUs 6 slots, token 7 wraps to the first", host(8), 7, 6, "0-1"},
		{"uneven: 8 vCPUs 6 slots, token 12 wraps to the last", host(8), 12, 6, "7"},
		{"uneven: 10 vCPUs 4 slots, token 6 wraps to the second", host(10), 6, 4, "3-5"},
		{"more slots than CPUs: one CPU each", host(4), 3, 8, "2"},
		{"more slots than CPUs wraps", host(4), 6, 8, "1"},
		{"token beyond this client's max_slots wraps", host(8), 6, 4, "2-3"},
		{"non-contiguous CPUs", cpuHost{cpus: []int{0, 1, 2, 3, 8, 9, 10, 11}, numCPU: 8, docker: 8}, 3, 4, "8-9"},
		{"exactly one CPU per slot", host(4), 4, 4, "3"},
		{"bad token treated as the first", host(8), 0, 4, "0-1"},
	}
	for _, c := range cases {
		set, note := planCPUSet(c.h, c.token, c.slots, false, nil)
		if set != c.want || note != "" {
			t.Errorf("%s: got (%q, %q), want %q", c.name, set, note, c.want)
		}
	}
}

// Concurrent jobs on one machine never share a CPU while there are at least
// as many CPUs as slots, each sees SPOTEXEC_CPUS of them or one more, and
// together they cover every CPU, whatever the slot count.
func TestPlanCPUSetDisjointAndCoversEveryCPU(t *testing.T) {
	for n := 2; n <= 48; n++ {
		for slots := 2; slots <= n; slots++ {
			seen := map[int]int{}
			share := max(1, n/slots)
			for tok := 1; tok <= slots; tok++ {
				set, _ := planCPUSet(host(n), tok, slots, false, nil)
				cpus, err := parseCPUList(set)
				if err != nil {
					t.Fatalf("n=%d slots=%d tok=%d: %q: %v", n, slots, tok, set, err)
				}
				if len(cpus) != share && len(cpus) != share+1 {
					t.Fatalf("n=%d slots=%d tok=%d: %d CPUs, SPOTEXEC_CPUS is %d", n, slots, tok, len(cpus), share)
				}
				for _, c := range cpus {
					if prev, ok := seen[c]; ok {
						t.Fatalf("n=%d slots=%d: CPU %d in tokens %d and %d", n, slots, c, prev, tok)
					}
					seen[c] = tok
				}
			}
			if len(seen) != n {
				t.Fatalf("n=%d slots=%d: %d of the CPUs used, want all", n, slots, len(seen))
			}
		}
	}
	// More slots than CPUs: still every CPU, one each.
	for n := 2; n <= 8; n++ {
		seen := map[string]bool{}
		for tok := 1; tok <= n+3; tok++ {
			set, _ := planCPUSet(host(n), tok, n+3, false, nil)
			if want := strconv.Itoa((tok - 1) % n); set != want {
				t.Fatalf("n=%d slots=%d tok=%d: %q, want CPU %s alone", n, n+3, tok, set, want)
			}
			seen[set] = true
		}
		if len(seen) != n {
			t.Fatalf("n=%d slots=%d: blocks %v, want each CPU alone", n, n+3, seen)
		}
	}
	// Tokens past the slot count wrap onto the same blocks.
	for n := 2; n <= 24; n++ {
		for slots := 2; slots <= n; slots++ {
			for tok := 1; tok <= slots; tok++ {
				a, _ := planCPUSet(host(n), tok, slots, false, nil)
				b, _ := planCPUSet(host(n), tok+slots, slots, false, nil)
				if a != b {
					t.Fatalf("n=%d slots=%d: token %d got %q, token %d got %q", n, slots, tok, a, tok+slots, b)
				}
			}
		}
	}
}

func TestPlanCPUSetNoPin(t *testing.T) {
	unknown := host(8)
	unknown.cpus, unknown.why = nil, "the runner isn't Linux"
	quiet := []struct {
		name      string
		h         cpuHost
		slots     int
		exclusive bool
		opts      []string
	}{
		{"exclusive gets every CPU", host(8), 4, true, nil},
		{"one slot gets every CPU", host(8), 1, false, nil},
		{"one CPU", host(1), 4, false, nil},
		{"user cpuset wins", host(8), 4, false, []string{"--shm-size=1g", "--cpuset-cpus=0-5"}},
		{"user cpuset wins, separate value", host(8), 4, false, []string{"--cpuset-cpus", "0-5"}},
		{"user --cpus wins", host(8), 4, false, []string{"--cpus", "4"}},
		{"user --cpu-quota wins", host(8), 4, false, []string{"--cpu-quota=200000"}},
		{"user cgroup parent", host(8), 4, false, []string{"--cgroup-parent=/batch"}},
		// Nothing to note when no pinning was due, even on an unknown host.
		{"exclusive on an unknown host", unknown, 4, true, nil},
		{"one slot on an unknown host", unknown, 1, false, nil},
		{"user option on an unknown host", unknown, 4, false, []string{"--cpus=2"}},
	}
	for _, c := range quiet {
		if set, note := planCPUSet(c.h, 2, c.slots, c.exclusive, c.opts); set != "" || note != "" {
			t.Errorf("%s: got (%q, %q), want nothing", c.name, set, note)
		}
	}
	// Fail open: no cpuset, and a note saying why.
	desktop := host(8)
	desktop.docker = 4
	noInfo := host(8)
	noInfo.docker = 0
	restricted := host(8)
	restricted.numCPU = 6
	noSupport := host(8)
	noSupport.noCpuset = true
	remote := host(8)
	remote.remote = "docker uses the build-box context"
	for _, c := range []struct {
		name string
		h    cpuHost
		note string
	}{
		{"unknown CPUs", unknown, "the runner isn't Linux"},
		{"docker sees other CPUs", desktop, "docker reports 4 CPUs, the host 8"},
		{"docker CPUs unknown", noInfo, "doesn't say how many CPUs"},
		{"restricted helper", restricted, "the helper may use 6 of the host's 8 CPUs"},
		{"no cpuset support", noSupport, "no cpuset support"},
		{"remote daemon", remote, "build-box context"},
	} {
		if set, note := planCPUSet(c.h, 2, 4, false, nil); set != "" || !strings.Contains(note, c.note) {
			t.Errorf("%s: got (%q, %q), want no cpuset and a note containing %q", c.name, set, note, c.note)
		}
	}
}

func TestCPUOption(t *testing.T) {
	for _, o := range [][]string{{"--cpus=2"}, {"--cpuset-cpus", "1"}, {"--cpu-period=1000"}, {"--cgroup-parent", "x"}} {
		if cpuOption(o) == "" {
			t.Errorf("%v: not seen as a CPU limit", o)
		}
	}
	for _, o := range [][]string{nil, {"--shm-size=1g"}, {"--cpu-shares=512"}, {"--cpuset-mems=0"}, {"--cpusx"}, {"--label", "--cpus=2"}, {"-e", "--cpus"}} {
		if f := cpuOption(o); f != "" {
			t.Errorf("%v: seen as %s", o, f)
		}
	}
}

func TestReadCPUHostParsesDockerInfo(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	info := "Client:\n Version:    27.5.1\n Context:    default\n\nServer:\n Containers: 3\n Kernel Version: 6.8.0\n CPUs: 8\n Total Memory: 15.3GiB\n"
	h := readCPUHost(info)
	if h.docker != 8 || h.noCpuset || h.remote != "" {
		t.Fatalf("docker=%d noCpuset=%v remote=%q, want 8, false, \"\"", h.docker, h.noCpuset, h.remote)
	}
	if h := readCPUHost(info + "WARNING: No cpuset support\n"); !h.noCpuset {
		t.Fatal("cpuset warning not seen")
	}
	if h := readCPUHost(info + "WARNING: Running in rootless-mode without cgroups. Systemd is required to enable cgroups in rootless-mode.\n"); !h.noCpuset {
		t.Fatal("rootless without cgroups not seen")
	}
	if h := readCPUHost("host:\n  cpus: 8\n"); h.docker != 0 { // podman's docker shim
		t.Fatalf("docker=%d from podman output, want 0 (unknown)", h.docker)
	}
	if h := readCPUHost(strings.Replace(info, "default", "rootless", 1)); h.remote != "" {
		t.Fatalf("rootless context seen as remote: %q", h.remote)
	}
	if h := readCPUHost(strings.Replace(info, "default", "build-box", 1)); h.remote == "" {
		t.Fatal("another context not seen")
	}
	t.Setenv("DOCKER_HOST", "ssh://build-box")
	if h := readCPUHost(info); h.remote == "" {
		t.Fatal("remote DOCKER_HOST not seen")
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	if h := readCPUHost(info); h.remote != "" {
		t.Fatalf("rootless socket seen as remote: %q", h.remote)
	}
	h = readCPUHost(info)
	if runtime.GOOS == "linux" && h.cpus == nil {
		t.Fatalf("no CPU list on Linux: %s", h.why)
	}
	if runtime.GOOS != "linux" && (h.cpus != nil || h.why == "") {
		t.Fatalf("off Linux: cpus %v, why %q; want none, and a reason", h.cpus, h.why)
	}
}

// Runs that get every CPU anyway, set their own limit, or didn't ask for
// pinning are never pinned and never noted, on any OS; a run that asked for
// pinning and can't be pinned is noted once.
func TestCpusetArgsQuiet(t *testing.T) {
	out := &syncBuf{}
	s := &Session{c: protocol.NewConn(strings.NewReader(""), out), maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 2}
	sent := func() string { out.mu.Lock(); defer out.mu.Unlock(); return out.b.String() }
	none := dockerRunner{} // docker info said nothing: pinning can't be trusted
	pin := func() *protocol.ContainerRun { return &protocol.ContainerRun{CPUPinning: true} }
	if got := s.cpusetArgs(&protocol.ContainerRun{}, none); got != nil {
		t.Fatalf("shared CPUs by default: got %v", got)
	}
	if got := s.cpusetArgs(&protocol.ContainerRun{CPUPinning: true, Options: []string{"--cpus=1"}}, none); got != nil {
		t.Fatalf("user --cpus: got %v", got)
	}
	s.acq.Exclusive = true
	if got := s.cpusetArgs(pin(), none); got != nil {
		t.Fatalf("exclusive: got %v", got)
	}
	s.acq.Exclusive, s.tokens = false, []*flock{nil, nil} // --exclusive holds every token
	if got := s.cpusetArgs(pin(), none); got != nil {
		t.Fatalf("every token: got %v", got)
	}
	s.tokens, s.maxSlots = []*flock{nil}, 1
	if got := s.cpusetArgs(pin(), none); got != nil {
		t.Fatalf("one slot: got %v", got)
	}
	if ev := sent(); ev != "" {
		t.Fatalf("events sent for runs that weren't due a cpuset: %s", ev)
	}
	s.maxSlots = 4
	for range 2 { // deps install, then the command
		if got := s.cpusetArgs(pin(), none); got != nil {
			t.Fatalf("unknown daemon CPUs: got %v", got)
		}
	}
	if n := strings.Count(sent(), "container_cpus"); n != 1 {
		t.Fatalf("%d container_cpus events, want 1: %s", n, sent())
	}
}

func TestCpusetArgsPinsOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the host's CPU list")
	}
	t.Setenv("DOCKER_HOST", "")
	h := readCPUHost("")
	if len(h.cpus) != runtime.NumCPU() || len(h.cpus) < 2 {
		t.Skipf("host CPU list %v doesn't match the %d CPUs this process may use", h.cpus, runtime.NumCPU())
	}
	d := dockerRunner{info: " Context:    default\n CPUs: " + strconv.Itoa(len(h.cpus)) + "\n"}
	s := &Session{maxSlots: len(h.cpus), tokens: []*flock{nil}, tokenIdx: 2}
	got := s.cpusetArgs(&protocol.ContainerRun{CPUPinning: true, Options: []string{"--shm-size=1g"}}, d)
	if want := []string{"--cpuset-cpus", formatCPUList(h.cpus[1:2])}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Without cpu_pinning the same job may use every CPU.
	if got := s.cpusetArgs(&protocol.ContainerRun{Options: []string{"--shm-size=1g"}}, d); got != nil {
		t.Fatalf("not asked to pin: got %v", got)
	}
}

// The dependency install gets SPOTEXEC_CPUS as commands do: the machine's
// share, or every CPU for a job alone on the runner.
func TestCPUShare(t *testing.T) {
	n := runtime.NumCPU()
	s := &Session{maxSlots: 4, tokens: []*flock{nil}}
	if got := s.cpuShare(); got != max(1, n/4) {
		t.Fatalf("4 slots: %d, want %d", got, max(1, n/4))
	}
	s.maxSlots = n * 2
	if got := s.cpuShare(); got != 1 {
		t.Fatalf("more slots than CPUs: %d, want 1", got)
	}
	s.maxSlots = 1
	if got := s.cpuShare(); got != n {
		t.Fatalf("one slot: %d, want %d", got, n)
	}
	s.maxSlots, s.acq.Exclusive = 4, true
	if got := s.cpuShare(); got != n {
		t.Fatalf("exclusive: %d, want %d", got, n)
	}
	s.acq.Exclusive, s.tokens = false, []*flock{nil, nil}
	if got := s.cpuShare(); got != n {
		t.Fatalf("every token: %d, want %d", got, n)
	}
}

// acquire records the slot count the cpuset is computed from.
func TestAcquireRecordsSlotCount(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 3}); err != nil {
		t.Fatal(err)
	}
	if s.maxSlots != 3 {
		t.Fatalf("maxSlots %d, want 3", s.maxSlots)
	}
}

// Jobs from checkouts with different max_slots, sharing one VM, are pinned
// to disjoint CPU blocks of one size.
func TestMixedMaxSlotsPinDisjointCPUs(t *testing.T) {
	root := t.TempDir()
	used := map[int]bool{}
	size := 0
	for _, n := range []int{2, 4, 4} {
		s := testSession(t, root)
		if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: n}); err != nil {
			t.Fatal(err)
		}
		if s.slotLock == nil {
			continue // the VM's slots are all taken
		}
		set, _ := planCPUSet(host(8), s.tokenIdx, s.maxSlots, false, nil)
		cpus, err := parseCPUList(set)
		if err != nil {
			t.Fatalf("token %d: %v", s.tokenIdx, err)
		}
		if size == 0 {
			size = len(cpus)
		}
		for _, c := range cpus {
			if used[c] || len(cpus) != size {
				t.Fatalf("token %d (%d slots) pinned to %s: overlaps, or not %d CPUs", s.tokenIdx, s.maxSlots, set, size)
			}
			used[c] = true
		}
	}
	if len(used) != 8 {
		t.Fatalf("pinned CPUs %v, want all 8 split between the runs", used)
	}
}
