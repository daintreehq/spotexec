package helper

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Memory on a shared runner. Without a limit, one job's spike exhausts the
// whole VM: it thrashes until the helper stops answering, and every slot's
// run is lost. But a limit on the container itself (docker run --memory)
// is visible inside it, and runtimes size themselves from it: Node reads
// cgroup memory.max (process.constrainedMemory()) and caps its default heap
// near 2 GiB under any limit, even 14 GiB, where it would take 4 GiB
// without one. So the limit goes one level up, where the container can't
// see it: every shared slot's container runs in one cgroup, the memory
// pool (docker run --cgroup-parent), capped at the machine's memory less a
// reserve for the runner itself. Jobs can still burst past their fair
// share; when together they reach the cap, the kernel kills the largest
// process among them (the job that spiked, in practice), inside the pool,
// while the helper, sshd and Docker keep their reserve. A job running alone
// on the machine (--exclusive, or a single slot) has no other job to be
// protected from and runs with no limit at all. SPOTEXEC_MEMORY is the fair
// share (pool ÷ slots), for sizing heaps and worker counts; nothing
// enforces it.

// memReserve is the memory kept back from jobs for everything else on the
// runner: the kernel, systemd and journald, sshd, dockerd and containerd
// (plus a shim per container), the helper and its tar/zstd hydration, and
// the docker clients. That is a few hundred MiB on a quiet VM and grows
// mildly with the number of slots, so: 1/16 of memory, at least 512 MiB,
// at most 2 GiB (1 GiB on a 16 GiB VM).
func memReserve(total int64) int64 {
	return min(max(total/16, 512<<20), 2<<30)
}

// controlOOMScore is the oom_score_adj protectControlPlane gives the
// helper: below dockerd's -500, so a runaway job (score 0) goes
// first, then Docker, and the session reporting on it last.
const controlOOMScore = -900

// oomLowered: protectControlPlane lowered the helper's score, so every
// command script starts with childOOMReset, putting the command back to the
// default score (raising it needs no privileges).
var oomLowered atomic.Bool

const childOOMReset = "echo 0 >/proc/self/oom_score_adj 2>/dev/null\n"

// protection runs protectControlPlane once per helper process, off the
// handshake's path (sudo can be slow); waitProtected holds a command back
// until it's done, so the command never inherits a lowered score without
// the reset.
var (
	protectOnce    sync.Once
	protectStarted atomic.Bool
	protectDone    = make(chan struct{})
)

func startProtection() {
	protectOnce.Do(func() {
		protectStarted.Store(true)
		go func() {
			defer close(protectDone)
			protectControlPlane()
		}()
	})
}

func waitProtected() {
	if !protectStarted.Load() {
		return
	}
	select {
	case <-protectDone:
	case <-time.After(5 * time.Second):
	}
}

// minMemShare: a share is never smaller than this, however many slots.
const minMemShare = 256 << 20

// memShare is one job's fair share of memory in bytes (SPOTEXEC_MEMORY),
// rounded down to a MiB: what the reserve leaves, divided among the slots
// (all of it with --exclusive or a single slot). 0 when total is unknown.
func memShare(total int64, slots int, exclusive bool) int64 {
	if total <= 0 {
		return 0
	}
	avail := total - memReserve(total)
	if !exclusive && slots > 1 {
		avail /= int64(slots)
	}
	return max(avail, minMemShare) &^ (1<<20 - 1)
}

// memOption returns the memory flag container.options sets, or "": then
// the user decides and spotexec sets no limit (a --cgroup-parent of their
// own would also take the container out of the pool).
func memOption(opts []string) string {
	for _, f := range dockerFlags(opts) {
		if slices.Contains(memOptions, f) {
			return f
		}
	}
	return ""
}

var memOptions = []string{"-m", "--memory", "--memory-swap", "--memory-reservation", "--kernel-memory", "--oom-kill-disable", "--oom-score-adj", "--cgroup-parent"}

// poolLimit is the memory pool's cap: the machine's memory less the
// reserve, rounded down to a MiB.
func poolLimit(total int64) int64 {
	return max(total-memReserve(total), minMemShare) &^ (1<<20 - 1)
}

// memHost is what decides whether the memory pool can be used.
type memHost struct {
	total   int64  // the runner's memory, bytes; 0 when unknown
	why     string // why total is unknown
	docker  int64  // memory the docker daemon reports, bytes; 0 when unknown
	noLimit bool   // the daemon reports no memory limit support
	remote  string // why the daemon may be on another machine, or ""
	driver  string // Docker's cgroup driver: systemd or cgroupfs
	v2      bool   // cgroup v2
	ours    bool   // a spotexec VM: the helper may set cgroups up with sudo
}

// planMemory returns the memory pool's cap (bytes) for a job, or 0 for no
// limit; note says why a job that should have been in the pool isn't.
// alone: the job has the machine to itself (--exclusive, one slot).
func planMemory(h memHost, alone bool, opts []string) (limit int64, note string) {
	if alone || memOption(opts) != "" {
		return 0, ""
	}
	switch {
	case h.total <= 0:
		return 0, h.why
	case h.remote != "":
		return 0, h.remote
	case !h.ours:
		return 0, "not a spotexec VM, so spotexec doesn't set up cgroups on it"
	case h.noLimit:
		return 0, "the docker daemon has no memory limit support"
	case !h.v2:
		return 0, "the runner doesn't use cgroup v2"
	case h.driver != "systemd" && h.driver != "cgroupfs":
		return 0, "docker info doesn't name a known cgroup driver"
	case h.docker <= 0:
		return 0, "docker info doesn't say how much memory the daemon has"
	case math.Abs(float64(h.docker-h.total)) > float64(h.total)/20:
		return 0, fmt.Sprintf("docker reports %s of memory, the host %s", gib(h.docker), gib(h.total))
	}
	return poolLimit(h.total), ""
}

var (
	dockerMemRe      = regexp.MustCompile(`(?m)^\s*Total Memory:\s*([0-9.]+)\s*([KMGTP]?i?B)\s*$`)
	dockerDriverRe   = regexp.MustCompile(`(?m)^\s*Cgroup Driver:\s*(\S+)\s*$`)
	dockerCgroupV2Re = regexp.MustCompile(`(?m)^\s*Cgroup Version:\s*2\s*$`)
)

// spotexecVM reports whether this is one of spotexec's own VMs.
var spotexecVM = func() bool {
	_, err := os.Stat(LifecycleDir)
	return err == nil
}

// readMemHost gathers memHost from the probe's docker info.
func readMemHost(info string) memHost {
	h := memHost{total: hostMemTotal()}
	if h.total <= 0 {
		h.why = "the runner's memory size can't be read"
	}
	if m := dockerMemRe.FindStringSubmatch(info); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}[m[2]]
		h.docker = int64(v * unit)
	}
	// Rootless Docker without cgroups accepts limits and ignores them.
	h.noLimit = strings.Contains(info, "No memory limit support") || strings.Contains(info, "rootless-mode without cgroups")
	h.remote = dockerRemote(info)
	if m := dockerDriverRe.FindStringSubmatch(info); m != nil {
		h.driver = m[1]
	}
	h.v2 = dockerCgroupV2Re.MatchString(info)
	h.ours = spotexecVM()
	if runtime.GOOS != "linux" {
		h.total, h.why = 0, "the runner isn't Linux, so its memory isn't Docker's"
	}
	return h
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// memoryArgs returns the docker run arguments that put this session's
// container in the memory pool, if it shares the machine, and records the
// pool's cap for the result. The container itself gets no limit, so what
// runs in it sees none. Like cpusetArgs it never fails a run: when the pool
// can't be set up or trusted, the container runs without it and a
// container_memory event says why.
func (s *Session) memoryArgs(run *protocol.ContainerRun, d dockerRunner) []string {
	s.memLimit = 0
	alone := s.acq.Exclusive || len(s.tokens) > 1 || s.maxSlots <= 1
	if alone || memOption(run.Options) != "" {
		return nil
	}
	h := readMemHost(d.info)
	limit, note := planMemory(h, alone, run.Options)
	if limit > 0 {
		parent, err := ensurePool(s.layout.Root, h.driver, limit)
		if err == nil {
			s.memLimit = limit
			return []string{"--cgroup-parent", parent}
		}
		note = "setting up the memory pool: " + err.Error()
	}
	if note != "" && !s.memNoted {
		s.memNoted = true
		_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "container_memory",
			Message: "no memory pool for this slot (" + note + "): one job can use up the runner's memory"})
	}
	return nil
}

// ensurePool makes sure the memory pool cgroup exists with its cap set to
// limit, for Docker's cgroup driver, and returns the --cgroup-parent that
// puts a container in it. Done once per VM, as root (sudo -n), under a lock
// shared by the VM's helpers; afterwards it only reads the cap back.
var ensurePool = func(root, driver string, limit int64) (string, error) {
	parent, dir := "/spotexec", "/sys/fs/cgroup/spotexec"
	if driver == "systemd" {
		parent, dir = "spotexec.slice", "/sys/fs/cgroup/spotexec.slice"
	}
	want := strconv.FormatInt(limit, 10)
	set := func() bool {
		b, err := os.ReadFile(dir + "/memory.max")
		return err == nil && strings.TrimSpace(string(b)) == want
	}
	if set() {
		return parent, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if root != "" {
		if err := os.MkdirAll(root, 0o755); err == nil {
			if l, err := lockExclusiveCtx(ctx, filepath.Join(root, "memory-pool.lock")); err == nil {
				defer l.Unlock()
			}
		}
	}
	if set() {
		return parent, nil
	}
	// No swap for the pool either, so a host with swap doesn't stretch it.
	script := "set -e; d=" + dir + "; mkdir -p $d; echo " + want + " >$d/memory.max; echo 0 >$d/memory.swap.max 2>/dev/null || true"
	if driver == "systemd" {
		// A slice of its own, which docker run --cgroup-parent places the
		// containers' scopes under.
		script = "set -e; mkdir -p /run/systemd/system; printf '[Unit]\\nDescription=spotexec jobs\\n\\n[Slice]\\nMemoryMax=" + want + "\\nMemorySwapMax=0\\n' >/run/systemd/system/spotexec.slice; " +
			"systemctl daemon-reload; systemctl start spotexec.slice; systemctl set-property --runtime spotexec.slice MemoryMax=" + want + " MemorySwapMax=0"
	}
	out, err := exec.CommandContext(ctx, "sudo", "-n", "sh", "-c", script).CombinedOutput()
	if !set() {
		if err != nil {
			return "", fmt.Errorf("%v: %s", err, firstLines(strings.TrimSpace(string(out)), 2))
		}
		return "", fmt.Errorf("%s/memory.max isn't %s afterwards", dir, want)
	}
	return parent, nil
}

// memoryShareMB is SPOTEXEC_MEMORY for this session's job, or 0.
func (s *Session) memoryShareMB() int64 {
	return memShare(hostMemTotal(), s.maxSlots, s.acq.Exclusive || len(s.tokens) > 1) >> 20
}

// containerOOMKilled reports whether Docker recorded an oom event (the
// kernel killed a process in the container's cgroup, for its memory limit,
// the memory pool's, or the whole machine running out) for the session's
// last container. Docker keeps recent
// events after a --rm container is gone, so this works after the run.
func (s *Session) containerOOMKilled() bool {
	c := s.lastContainer
	if c.name == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ts := func(t time.Time) string { return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond()) }
	out, err := runShell(ctx, c.d.cmd(strings.Join([]string{"events",
		"--since", ts(c.started.Add(-time.Second)), "--until", ts(time.Now()),
		"--filter", shq("container=" + c.name), "--filter", "event=oom", "--format", shq("{{.Action}}")}, " ")))
	return err == nil && strings.Contains(string(out), "oom")
}

// memStats reads the runner's memory for a heartbeat (Linux; zero
// elsewhere).
func memStats() protocol.Heartbeat {
	var hb protocol.Heartbeat
	if runtime.GOOS != "linux" {
		return hb
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), ":")
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			switch k {
			case "MemTotal":
				hb.MemTotalMB = kb >> 10
			case "MemAvailable":
				hb.MemAvailMB = kb >> 10
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		hb.PSIFull10 = psiAvg10(string(b), "full")
	}
	return hb
}

// psiAvg10 extracts a line's avg10 from a /proc/pressure file.
func psiAvg10(s, line string) float64 {
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, line+" ") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, _ := strconv.ParseFloat(v, 64)
				return x
			}
		}
	}
	return 0
}

// lockExclusiveCtx takes an exclusive lock on path, polling until ctx ends.
func lockExclusiveCtx(ctx context.Context, path string) (*flock, error) {
	for {
		l, ok, err := tryLock(path)
		if err != nil || ok {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
