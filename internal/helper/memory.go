package helper

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
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

// Each job gets a share of the runner's memory, as it does of its CPUs:
// SPOTEXEC_MEMORY (MiB), and in a project container a hard limit
// (docker run --memory, with --memory-swap equal to it so a host with swap
// doesn't stretch it). A job that outgrows its share is then OOM-killed
// inside its own cgroup, and the VM, the helper and the other slots carry
// on. Without the limit, one job's spike exhausts the whole VM: it thrashes
// until the helper stops answering, and every slot's run is lost.

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
// helper and its sshd: below dockerd's -500, so a runaway job (score 0) goes
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

// memShare is one job's memory in bytes, rounded down to a MiB: what the
// reserve leaves, divided among the slots (all of it with --exclusive or a
// single slot). 0 when total is unknown.
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
// the user decides and spotexec sets no limit.
func memOption(opts []string) string {
	for _, f := range dockerFlags(opts) {
		if slices.Contains(memOptions, f) {
			return f
		}
	}
	return ""
}

var memOptions = []string{"-m", "--memory", "--memory-swap", "--memory-reservation", "--kernel-memory", "--oom-kill-disable", "--oom-score-adj"}

// memHost is what decides whether a memory limit can be applied.
type memHost struct {
	total       int64  // the runner's memory, bytes; 0 when unknown
	why         string // why total is unknown
	docker      int64  // memory the docker daemon reports, bytes; 0 when unknown
	noLimit     bool   // the daemon reports no memory limit support
	noSwapLimit bool   // ...or no swap limit support (cgroup v1 without swapaccount)
	remote      string // why the daemon may be on another machine, or ""
}

// planMemory returns the --memory limit (bytes) for a job, or 0 to set
// none; note says why a job that should have been limited wasn't.
func planMemory(h memHost, slots int, exclusive bool, opts []string) (limit int64, note string) {
	if memOption(opts) != "" {
		return 0, ""
	}
	switch {
	case h.total <= 0:
		return 0, h.why
	case h.remote != "":
		return 0, h.remote
	case h.noLimit:
		return 0, "the docker daemon has no memory limit support"
	case h.docker <= 0:
		return 0, "docker info doesn't say how much memory the daemon has"
	case math.Abs(float64(h.docker-h.total)) > float64(h.total)/20:
		return 0, fmt.Sprintf("docker reports %s of memory, the host %s", gib(h.docker), gib(h.total))
	}
	return memShare(h.total, slots, exclusive), ""
}

var dockerMemRe = regexp.MustCompile(`(?m)^\s*Total Memory:\s*([0-9.]+)\s*([KMGTP]?i?B)\s*$`)

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
	h.noSwapLimit = strings.Contains(info, "No swap limit support")
	h.remote = dockerRemote(info)
	if runtime.GOOS != "linux" {
		h.total, h.why = 0, "the runner isn't Linux, so its memory isn't Docker's"
	}
	return h
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// memoryArgs returns the docker run arguments limiting this session's
// container to its slot's memory, if any, and records the limit for the
// result. Like cpusetArgs it never fails a run: when the limit can't be
// trusted to mean anything, none is set and a container_memory event says
// why.
func (s *Session) memoryArgs(run *protocol.ContainerRun, d dockerRunner) []string {
	s.memLimit = 0
	if memOption(run.Options) != "" {
		return nil
	}
	h := readMemHost(d.info)
	limit, note := planMemory(h, s.maxSlots, s.acq.Exclusive || len(s.tokens) > 1, run.Options)
	if limit > 0 {
		s.memLimit = limit
		v := strconv.FormatInt(limit>>20, 10) + "m"
		if h.noSwapLimit { // docker would only warn and ignore it
			return []string{"--memory", v}
		}
		return []string{"--memory", v, "--memory-swap", v}
	}
	if note != "" && !s.memNoted {
		s.memNoted = true
		_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "container_memory",
			Message: "no memory limit for this slot (" + note + "): one job can use up the runner's memory"})
	}
	return nil
}

// memoryShareMB is SPOTEXEC_MEMORY for this session's job, or 0.
func (s *Session) memoryShareMB() int64 {
	return memShare(hostMemTotal(), s.maxSlots, s.acq.Exclusive || len(s.tokens) > 1) >> 20
}

// containerOOMKilled reports whether Docker recorded an oom event (the
// kernel killed a process in the container's cgroup for exceeding its
// memory limit) for the session's last container. Docker keeps recent
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
