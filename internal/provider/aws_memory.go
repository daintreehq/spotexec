package provider

import (
	"sort"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// Measured memory (#294). A VM's helper starts a command in its memory
// pool only when the command's expected peak fits beside what the running
// commands are expected to grow to, so each run's peak (what its container
// was seen using, page cache left out) is kept per workload, and the next
// run of the same workload is sent with the largest of its latest runs.
const (
	// memSamples bounds the runs kept of one workload: a workload that
	// needs less than it did (a leak fixed, an OOM kill recorded at the
	// pool's size) is sized by its new peaks after this many runs.
	memSamples = 5
	// memFor is how long a run counts.
	memFor = 30 * 24 * time.Hour
	// memWorkloads bounds the workloads remembered per runner: the least
	// recently run is forgotten first.
	memWorkloads = 128
	// memLockWait bounds how long a run waits to read or record a peak:
	// it is skipped rather than holding the run up.
	memLockWait = 500 * time.Millisecond
)

// memSample is one run's peak memory.
type memSample struct {
	MB int64     `json:"mb"`
	At time.Time `json:"at"`
}

// memoryRunner reports whether r's VMs have the memory pool (spotexec's own
// Linux VMs), so measuring and sending peaks is worth it.
func memoryRunner(r *config.Runner) bool {
	return r != nil && r.Type == config.TypeAWSSpot
}

// RecordRunMemory records the peak memory (MiB) a run of workload (see
// client.SpeedWorkload) used on a VM of runner r, for the next run of it to
// reserve. Best effort: skipped when the state stays busy.
func RecordRunMemory(r *config.Runner, workload string, mb int64) error {
	if !memoryRunner(r) || workload == "" || mb <= 0 {
		return nil
	}
	g := &AWSGlobal{r: r, dir: RunnerStateDir(r.Name)}
	return g.withStateWait(memLockWait, func(st *placementState) { recordMemory(st, workload, mb, g.clock()) })
}

// RunMemory is the memory (MiB) a run of workload is expected to need on a
// VM of runner r: the largest peak of its latest runs; 0 when none was
// measured lately (or the state stayed busy).
func RunMemory(r *config.Runner, workload string) int64 {
	if !memoryRunner(r) || workload == "" {
		return 0
	}
	g := &AWSGlobal{r: r, dir: RunnerStateDir(r.Name)}
	var mb int64
	_ = g.withStateWait(memLockWait, func(st *placementState) { mb = expectedMemory(st, workload, g.clock()) })
	return mb
}

func expectedMemory(st *placementState, workload string, now time.Time) int64 {
	var mb int64
	for _, s := range st.Memory[workload] {
		if usableMemory(s, now) {
			mb = max(mb, s.MB)
		}
	}
	return mb
}

func usableMemory(s memSample, now time.Time) bool {
	return s.MB > 0 && now.Sub(s.At) < memFor && !s.At.After(now.Add(time.Hour))
}

func recordMemory(st *placementState, workload string, mb int64, now time.Time) {
	for wk, ss := range st.Memory {
		kept := ss[:0]
		for _, s := range ss {
			if usableMemory(s, now) {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(st.Memory, wk)
		} else {
			st.Memory[wk] = kept
		}
	}
	if st.Memory == nil {
		st.Memory = map[string][]memSample{}
	}
	ss := append(st.Memory[workload], memSample{MB: mb, At: now})
	if n := len(ss); n > memSamples {
		ss = append([]memSample(nil), ss[n-memSamples:]...)
	}
	st.Memory[workload] = ss
	if len(st.Memory) <= memWorkloads {
		return
	}
	latest := func(k string) time.Time {
		ss := st.Memory[k]
		return ss[len(ss)-1].At
	}
	keys := sortedKeys(st.Memory)
	sort.SliceStable(keys, func(i, j int) bool { return latest(keys[i]).Before(latest(keys[j])) })
	for _, k := range keys[:len(keys)-memWorkloads] {
		delete(st.Memory, k)
	}
}
