package helper

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// A project container is pinned to its slot's share of the machine's CPUs
// with --cpuset-cpus, so the CPU count it sees matches SPOTEXEC_CPUS. nproc,
// os.availableParallelism(), GOMAXPROCS, make -j, cargo and most test
// runners size their worker pools from the affinity mask, which only a
// cpuset changes; a CFS quota (--cpus) caps CPU time but leaves them all
// seeing the whole VM, so every slot would still start one worker per vCPU.
//
// The share is the same count as SPOTEXEC_CPUS: max(1, CPUs / slots). The
// job holding token t (SPOTEXEC_SLOT, unique on the host across repos) gets
// the t-th block of that many CPUs, so concurrent jobs never overlap until
// there are more slots than CPUs. CPUs left over by an uneven division stay
// unpinned, for the helper, sync and dockerd.

// cpuOptions are the docker run flags that set a CPU limit, or move the
// container to a cgroup whose CPUs we can't see: if container.options sets
// one, the user decides and spotexec pins nothing.
var cpuOptions = []string{"--cpuset-cpus", "--cpus", "--cpu-quota", "--cpu-period", "--cgroup-parent"}

// cpuOption returns the CPU flag opts sets, or "".
func cpuOption(opts []string) string {
	for _, o := range opts {
		for _, f := range cpuOptions {
			if o == f || strings.HasPrefix(o, f+"=") {
				return f
			}
		}
	}
	return ""
}

// parseCPUList parses a kernel CPU list such as "0-3,8-11".
func parseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty CPU list")
	}
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, fmt.Errorf("bad CPU list %q", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a || b-a > 4096 {
				return nil, fmt.Errorf("bad CPU list %q", s)
			}
		}
		for c := a; c <= b; c++ {
			cpus = append(cpus, c)
		}
	}
	slices.Sort(cpus)
	return slices.Compact(cpus), nil
}

// formatCPUList is parseCPUList's inverse for a sorted list: [0 1 2 5] is
// "0-2,5".
func formatCPUList(cpus []int) string {
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(cpus[i]))
		} else {
			parts = append(parts, strconv.Itoa(cpus[i])+"-"+strconv.Itoa(cpus[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// cpuHost is what decides whether a cpuset can be applied on this runner.
type cpuHost struct {
	cpus     []int  // CPUs dockerd accepts in a cpuset; nil when unknown
	why      string // why cpus is unknown
	numCPU   int    // CPUs the helper sees: what SPOTEXEC_CPUS was computed from
	docker   int    // CPUs the docker daemon reports; 0 when unknown
	noCpuset bool   // the daemon reports no cpuset support
	remote   string // why the daemon may be on another machine, or ""
}

// planCPUSet returns the --cpuset-cpus value for the job holding token on
// a machine with slots slots, or "" to pin nothing. note says why a job that
// should have been pinned wasn't; it is "" when pinning simply doesn't apply
// (the job gets every CPU anyway, or container.options sets a CPU limit).
func planCPUSet(h cpuHost, token, slots int, exclusive bool, opts []string) (set, note string) {
	if exclusive || slots <= 1 || cpuOption(opts) != "" {
		return "", ""
	}
	n := len(h.cpus)
	switch {
	case h.cpus == nil:
		return "", h.why
	case h.remote != "":
		return "", h.remote
	case h.noCpuset:
		return "", "the docker daemon has no cpuset support"
	case h.docker == 0:
		return "", "docker info doesn't say how many CPUs the daemon has"
	case h.docker != n:
		// Docker Desktop's VM, a remote context, or a daemon in a restricted
		// cgroup: its CPUs aren't the ones read here.
		return "", fmt.Sprintf("docker reports %d CPUs, the host %d", h.docker, n)
	case h.numCPU != n:
		return "", fmt.Sprintf("the helper may use %d of the host's %d CPUs", h.numCPU, n)
	}
	k := max(1, n/slots)
	if k >= n {
		return "", ""
	}
	// More slots than CPUs: one CPU each, wrapping. Tokens are taken lowest
	// first, so jobs only share a CPU once every CPU has one.
	b := (max(token, 1) - 1) % (n / k)
	return formatCPUList(h.cpus[b*k : b*k+k]), ""
}

var (
	dockerCPUsRe    = regexp.MustCompile(`(?m)^\s*CPUs:\s*(\d+)\s*$`)
	dockerContextRe = regexp.MustCompile(`(?m)^\s*Context:\s*(\S+)\s*$`)
)

// readCPUHost gathers cpuHost for this runner from the probe's docker info.
func readCPUHost(info string) cpuHost {
	h := cpuHost{numCPU: runtime.NumCPU()}
	if m := dockerCPUsRe.FindStringSubmatch(info); m != nil {
		h.docker, _ = strconv.Atoi(m[1])
	}
	// Rootless Docker without cgroups drops resource limits silently.
	h.noCpuset = strings.Contains(info, "No cpuset support") || strings.Contains(info, "rootless-mode without cgroups")
	if dh := os.Getenv("DOCKER_HOST"); dh != "" && !strings.HasPrefix(dh, "unix://") {
		h.remote = "DOCKER_HOST points at another machine"
	} else if m := dockerContextRe.FindStringSubmatch(info); m != nil && m[1] != "default" && m[1] != "rootless" {
		h.remote = "docker uses the " + m[1] + " context"
	}
	if runtime.GOOS != "linux" {
		h.why = "the runner isn't Linux, so its CPUs aren't Docker's"
		return h
	}
	// What dockerd validates a cpuset against: the root cgroup's CPUs
	// (cgroup v2, then v1), else every online CPU.
	for _, p := range []string{"/sys/fs/cgroup/cpuset.cpus.effective", "/sys/fs/cgroup/cpuset/cpuset.cpus", "/sys/devices/system/cpu/online"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		cpus, err := parseCPUList(string(b))
		if err != nil {
			h.why = err.Error()
			return h
		}
		h.cpus = cpus
		return h
	}
	h.why = "the host's CPU list can't be read"
	return h
}

// cpusetArgs returns the docker run arguments pinning this session's
// container to its slot's CPUs, if any. It never fails a run: when the
// cpuset can't be trusted to be valid, nothing is pinned and a
// container_cpus event (seen with --json or --events-file) says why.
func (s *Session) cpusetArgs(run *protocol.ContainerRun, d dockerRunner) []string {
	exclusive := s.acq.Exclusive || len(s.tokens) > 1
	if exclusive || s.maxSlots <= 1 || cpuOption(run.Options) != "" {
		return nil // skip reading the host: nothing would be pinned
	}
	h := readCPUHost(d.info)
	set, note := planCPUSet(h, s.tokenIdxAt(0), s.maxSlots, exclusive, run.Options)
	if set != "" {
		return []string{"--cpuset-cpus", set}
	}
	if note != "" && !s.cpusNoted {
		s.cpusNoted = true
		_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "container_cpus",
			Message: "not pinned to this slot's CPU share (" + note + "): commands see all of Docker's CPUs, so size worker pools with $SPOTEXEC_CPUS"})
	}
	return nil
}
