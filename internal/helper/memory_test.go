package helper

import (
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

func memHostOf(total int64) memHost { return memHost{total: total, docker: total} }

func TestPlanMemory(t *testing.T) {
	if l, note := planMemory(memHostOf(16*gb), 4, false, nil); l != memShare(16*gb, 4, false) || note != "" {
		t.Errorf("plain: (%d, %q)", l, note)
	}
	docker := memHostOf(16 * gb)
	docker.docker = 16*gb - 200<<20 // Docker rounds what it prints
	if l, note := planMemory(docker, 4, false, nil); l == 0 || note != "" {
		t.Errorf("docker's rounding refused: (%d, %q)", l, note)
	}
	if l, note := planMemory(memHostOf(16*gb), 4, false, []string{"-m", "8g"}); l != 0 || note != "" {
		t.Errorf("user limit: (%d, %q)", l, note)
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
	} {
		if l, note := planMemory(c.h, 4, false, nil); l != 0 || !strings.Contains(note, c.note) {
			t.Errorf("%s: (%d, %q), want no limit and a note containing %q", c.name, l, note, c.note)
		}
	}
}

func TestReadMemHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	info := " Context:    default\n CPUs: 8\n Total Memory: 15.33GiB\n"
	h := readMemHost(info)
	v := 15.33
	if want := int64(v * gb); h.docker != want {
		t.Errorf("docker %d, want %d", h.docker, want)
	}
	if h.noLimit || h.noSwapLimit || h.remote != "" {
		t.Errorf("%+v", h)
	}
	if h := readMemHost(info + "WARNING: No memory limit support\nWARNING: No swap limit support\n"); !h.noLimit || !h.noSwapLimit {
		t.Errorf("warnings not seen: %+v", h)
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

// User memory options and unknown hosts set no limit; an unknown host is
// noted once per session.
func TestMemoryArgsQuiet(t *testing.T) {
	out := &syncBuf{}
	s := &Session{c: protocol.NewConn(strings.NewReader(""), out), maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 1}
	if got := s.memoryArgs(&protocol.ContainerRun{Options: []string{"--memory=2g"}}, dockerRunner{}); got != nil || s.memLimit != 0 {
		t.Fatalf("user --memory: %v, limit %d", got, s.memLimit)
	}
	if out.b.Len() != 0 {
		t.Fatalf("event for a user limit: %s", out.b.String())
	}
	for range 2 {
		if got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{}); got != nil {
			t.Fatalf("unknown daemon memory: %v", got)
		}
	}
	if n := strings.Count(out.b.String(), "container_memory"); n != 1 {
		t.Fatalf("%d container_memory events, want 1", n)
	}
}

func TestMemoryArgsOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/meminfo")
	}
	t.Setenv("DOCKER_HOST", "")
	total := hostMemTotal()
	info := " Context:    default\n Total Memory: " + strconv.FormatFloat(float64(total)/gb, 'f', 4, 64) + "GiB\n"
	s := &Session{maxSlots: 4, tokens: []*flock{nil}, tokenIdx: 1}
	v := strconv.FormatInt(memShare(total, 4, false)>>20, 10) + "m"
	if got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{info: info}); !slices.Equal(got, []string{"--memory", v, "--memory-swap", v}) {
		t.Fatalf("got %v, want --memory and --memory-swap %s", got, v)
	}
	if s.memLimit != memShare(total, 4, false) {
		t.Fatalf("limit recorded as %d", s.memLimit)
	}
	if got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{info: info + "WARNING: No swap limit support\n"}); !slices.Equal(got, []string{"--memory", v}) {
		t.Fatalf("without swap accounting: %v", got)
	}
	s.acq.Exclusive = true
	if got := s.memoryArgs(&protocol.ContainerRun{}, dockerRunner{info: info}); got[1] != strconv.FormatInt(memShare(total, 4, true)>>20, 10)+"m" {
		t.Fatalf("exclusive: %v", got)
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
	s := &Session{}
	s.lastSeen.Store(time.Now().Add(-2 * ClientTimeout).UnixNano())
	if !s.clientGone() {
		t.Fatal("silent client not gone")
	}
	s.stalledAt.Store(time.Now().UnixNano())
	if s.clientGone() {
		t.Fatal("client taken for gone right after the helper's own stall")
	}
	s.stalledAt.Store(time.Now().Add(-stallGrace - time.Second).UnixNano())
	if !s.clientGone() {
		t.Fatal("an old stall still excuses the client")
	}
	// Woken before the stall sampler after a stall: its tick is overdue.
	s.stallTick.Store(time.Now().Add(-10 * time.Second).UnixNano())
	if s.clientGone() {
		t.Fatal("client taken for gone before the stall sampler caught up")
	}
	// Stall after stall can't excuse a dead client forever.
	s.stalledAt.Store(time.Now().UnixNano())
	s.lastSeen.Store(time.Now().Add(-ClientTimeout - stallCap - time.Second).UnixNano())
	if !s.clientGone() {
		t.Fatal("stalls excuse a client silent past the cap")
	}
}

// The stall sampler notices the helper going unscheduled.
func TestWatchStalls(t *testing.T) {
	s := &Session{}
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
