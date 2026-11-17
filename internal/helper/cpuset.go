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

// Jobs sharing a runner share its CPUs: a project container may use any of
// them, and when jobs contend, the kernel divides CPU time between their
// containers by cgroup weight, equal for each, among those that can use it
// at that moment. So a job runs on every idle CPU, and no slot count leaves
// CPUs that no job may use. The cost is that nproc,
// os.availableParallelism() and GOMAXPROCS see the whole VM in every
// container; SPOTEXEC_CPUS is each job's share, for sizing worker pools.
//
// container.cpu_pinning restores a fixed partition, for timing-sensitive
// work: the container is pinned with --cpuset-cpus to a block of the CPUs of
// its own, so the CPU count it sees matches its share. nproc and the rest
// size their pools from the affinity mask, which only a cpuset changes; a
// CFS quota (--cpus) caps CPU time but leaves them all seeing the whole VM.
// The job holding run permit or token t gets the t-th of max_slots blocks,
// which together cover every CPU: CPUs / slots each, plus one more for the
// first CPUs % slots blocks. With more slots than CPUs, each gets one CPU,
// wrapping.

// cpuOptions are the docker run flags that set a CPU limit, or move the
// container to a cgroup whose CPUs we can't see: if container.options sets
// one, the user decides and spotexec pins nothing.
var cpuOptions = []string{"--cpuset-cpus", "--cpus", "--cpu-quota", "--cpu-period", "--cgroup-parent"}

// cpuOption returns the CPU flag opts sets, or "".
func cpuOption(opts []string) string {
	for _, f := range dockerFlags(opts) {
		if slices.Contains(cpuOptions, f) {
			return f
		}
	}
	return ""
}

// dockerBoolFlags are the docker run flags that take no value (as a
// separate argument): every other flag consumes the next argument unless
// it is written --flag=value.
var dockerBoolFlags = []string{"--rm", "--init", "--privileged", "--read-only", "--tty", "--interactive", "--detach",
	"--oom-kill-disable", "--no-healthcheck", "--publish-all", "--quiet", "--sig-proxy", "--help", "--disable-content-trust", "--use-api-socket", "-t", "-i", "-d", "-P", "-q"}

// dockerFlags lists the flags a docker run argument list sets, by name
// (--memory, -m), skipping the values flags consume, so a value such as
// `--label -metrics` is never read as a flag.
func dockerFlags(opts []string) []string {
	var flags []string
	for i := 0; i < len(opts); i++ {
		o := opts[i]
		switch {
		case strings.HasPrefix(o, "--") && len(o) > 2:
			name, _, hasValue := strings.Cut(o, "=")
			flags = append(flags, name)
			if !hasValue && !slices.Contains(dockerBoolFlags, name) {
				i++
			}
		case len(o) > 1 && o[0] == '-':
			// A cluster of shorthands (-it, -m4g, -itm 4g): booleans, then
			// at most one that takes a value, attached or next.
			for j := 1; j < len(o); j++ {
				f := "-" + o[j:j+1]
				flags = append(flags, f)
				if !slices.Contains(dockerBoolFlags, f) {
					if j == len(o)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return flags
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
	if n <= 1 {
		return "", "" // the job gets every CPU anyway
	}
	if slots >= n {
		// One CPU each, wrapping. Tokens are taken lowest first, so jobs
		// only share a CPU once every CPU has one.
		c := (max(token, 1) - 1) % n
		return formatCPUList(h.cpus[c : c+1]), ""
	}
	// slots blocks covering every CPU: q each, and the first r one more.
	q, r := n/slots, n%slots
	b := (max(token, 1) - 1) % slots
	start := b*q + min(b, r)
	size := q
	if b < r {
		size++
	}
	return formatCPUList(h.cpus[start : start+size]), ""
}

var (
	dockerCPUsRe    = regexp.MustCompile(`(?m)^\s*CPUs:\s*(\d+)\s*$`)
	dockerContextRe = regexp.MustCompile(`(?m)^\s*Context:\s*(\S+)\s*$`)
)

// dockerRemote says why the docker daemon may be on another machine than
// this one, or "".
func dockerRemote(info string) string {
	if dh := os.Getenv("DOCKER_HOST"); dh != "" && !strings.HasPrefix(dh, "unix://") {
		return "DOCKER_HOST points at another machine"
	}
	if m := dockerContextRe.FindStringSubmatch(info); m != nil && m[1] != "default" && m[1] != "rootless" {
		return "docker uses the " + m[1] + " context"
	}
	return ""
}

// readCPUHost gathers cpuHost for this runner from the probe's docker info.
func readCPUHost(info string) cpuHost {
	h := cpuHost{numCPU: runtime.NumCPU()}
	if m := dockerCPUsRe.FindStringSubmatch(info); m != nil {
		h.docker, _ = strconv.Atoi(m[1])
	}
	// Rootless Docker without cgroups drops resource limits silently.
	h.noCpuset = strings.Contains(info, "No cpuset support") || strings.Contains(info, "rootless-mode without cgroups")
	h.remote = dockerRemote(info)
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
// container to its slot's CPUs, when container.cpu_pinning asks for it. It
// never fails a run: when the cpuset can't be trusted to be valid, nothing
// is pinned and a container_cpus event (seen with --json or --events-file)
// says why.
func (s *Session) cpusetArgs(run *protocol.ContainerRun, d dockerRunner) []string {
	exclusive := s.acq.Exclusive || len(s.tokens) > 1
	if !run.CPUPinning || exclusive || s.maxSlots <= 1 || cpuOption(run.Options) != "" {
		return nil // skip reading the host: nothing would be pinned
	}
	h := readCPUHost(d.info)
	// A command's CPUs follow its run permit: at most max_slots run at
	// once, while tokens go up to twice that. Anything else (a dependency
	// install) goes by its token.
	s.relMu.Lock()
	idx := s.runIdx
	s.relMu.Unlock()
	if idx == 0 {
		idx = s.tokenIdxAt(0)
	}
	set, note := planCPUSet(h, idx, s.maxSlots, exclusive, run.Options)
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
