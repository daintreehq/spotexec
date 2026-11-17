package provider

import (
	"math"
	"sort"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// Measured speed (#279). Run-cost placement weighs how much slower an
// older instance type is (vcpus runners, whose types span generations).
// genSpeedup is only an assumption, and workloads differ widely: a
// TypeScript typecheck ran on m7a at 0.69x r8a's speed, where the
// assumption said 0.91x. So each successful run's command time is kept
// with the type it ran on, and once the same workload has run on two or
// more types, their speeds are taken from those runs instead.
//
// A VM serves whatever runs next on its runner, not only the project
// whose run launched it, so the runner's recent workloads are pooled: each
// is compared only with itself on other types (a workload is a project's
// command, so one typecheck is never compared with another project's
// tests).
const (
	// speedSamples bounds the runs kept of one workload on one type.
	speedSamples = 9
	// speedFor is how long a run counts.
	speedFor = 30 * 24 * time.Hour
	// speedMinRuns is the runs of a workload on a type before their
	// median counts: one run on a VM busy with others says little.
	speedMinRuns = 3
	// speedWorkloads bounds the workloads remembered per runner, and
	// speedCells the workload and type pairs, so placement.json stays
	// small: the least recently run is forgotten first.
	speedWorkloads = 64
	speedCells     = 256
	// speedLockWait bounds how long a run waits to record its time: it is
	// skipped rather than holding the run up.
	speedLockWait = 2 * time.Second
	// speedFitRounds bounds the rounds of the fit in measuredSpeeds.
	speedFitRounds = 500
)

// speedSample is one run's command time.
type speedSample struct {
	MS int64     `json:"ms"`
	At time.Time `json:"at"`
}

// TypeSpeed is a type's speed as measured from the runner's own runs.
type TypeSpeed struct {
	// Relative is its speed against Against, the newest generation of
	// the types it was measured with (1 for that one): Against's runs of
	// the same workloads took Relative times as long as its own.
	Relative float64 `json:"relative"`
	Against  string  `json:"against"`
	Runs     int     `json:"runs"`      // the runs behind it
	Workload int     `json:"workloads"` // the workloads they were of
}

// RecordRunSpeed records how long a successful run's command took on a VM
// of instance type itype, for runner r's next launches to weigh that
// type's speed by. workload names what ran (see client.SpeedWorkload): only
// runs of the same workload are compared. Only vcpus runners weigh speed.
func RecordRunSpeed(r *config.Runner, workload, itype string, took time.Duration) error {
	if r == nil || r.Type != config.TypeAWSSpot || !r.ByRequirements() || workload == "" || itype == "" || took <= 0 {
		return nil
	}
	g := &AWSGlobal{r: r, dir: RunnerStateDir(r.Name)}
	return g.withStateWait(speedLockWait, func(st *placementState) { recordSpeed(st, workload, itype, took, g.clock()) })
}

func recordSpeed(st *placementState, workload, itype string, took time.Duration, now time.Time) {
	forgetOldSpeed(st, now)
	if st.Speed == nil {
		st.Speed = map[string]map[string][]speedSample{}
	}
	w := st.Speed[workload]
	if w == nil {
		w = map[string][]speedSample{}
		st.Speed[workload] = w
	}
	ss := append(w[itype], speedSample{MS: took.Milliseconds(), At: now})
	if n := len(ss); n > speedSamples {
		ss = append([]speedSample(nil), ss[n-speedSamples:]...)
	}
	w[itype] = ss
	trimSpeed(st)
}

// trimSpeed forgets the least recently run workloads past speedWorkloads,
// then the least recently run types of a workload past speedCells.
func trimSpeed(st *placementState) {
	type cell struct {
		w, t string
		at   time.Time
	}
	var cells []cell
	latest := map[string]time.Time{}
	for wk, w := range st.Speed {
		for it, ss := range w {
			at := ss[len(ss)-1].At
			cells = append(cells, cell{wk, it, at})
			if at.After(latest[wk]) {
				latest[wk] = at
			}
		}
	}
	older := func(a, b time.Time, ka, kb string) bool {
		if !a.Equal(b) {
			return a.Before(b)
		}
		return ka < kb
	}
	if len(st.Speed) > speedWorkloads {
		keys := sortedKeys(st.Speed)
		sort.SliceStable(keys, func(i, j int) bool { return older(latest[keys[i]], latest[keys[j]], keys[i], keys[j]) })
		for _, k := range keys[:len(keys)-speedWorkloads] {
			delete(st.Speed, k)
		}
	}
	kept := cells[:0]
	for _, c := range cells {
		if st.Speed[c.w] != nil {
			kept = append(kept, c)
		}
	}
	if cells = kept; len(cells) <= speedCells {
		return
	}
	sort.Slice(cells, func(i, j int) bool {
		return older(cells[i].at, cells[j].at, cells[i].w+"/"+cells[i].t, cells[j].w+"/"+cells[j].t)
	})
	for _, c := range cells[:len(cells)-speedCells] {
		delete(st.Speed[c.w], c.t)
		if len(st.Speed[c.w]) == 0 {
			delete(st.Speed, c.w)
		}
	}
}

func usableSpeed(s speedSample, now time.Time) bool {
	return s.MS > 0 && now.Sub(s.At) < speedFor && !s.At.After(now.Add(time.Hour))
}

// forgetOldSpeed drops runs too old to count, and workloads and types
// left with none.
func forgetOldSpeed(st *placementState, now time.Time) {
	for wk, w := range st.Speed {
		for it, ss := range w {
			kept := ss[:0]
			for _, s := range ss {
				if usableSpeed(s, now) {
					kept = append(kept, s)
				}
			}
			if len(kept) == 0 {
				delete(w, it)
			} else {
				w[it] = kept
			}
		}
		if len(w) == 0 {
			delete(st.Speed, wk)
		}
	}
}

// measuredSpeeds is each type's speed as the runner's runs measured it
// (nil: no workload has run often enough on two types). Each workload's
// runs on a type count as their median (a run beside busy neighbours, or
// with more of the worktree's changes, mustn't sway it), once there are
// speedMinRuns of them. The medians are fitted as a workload's own length
// times a type's own slowness, each workload weighing the same however
// often it ran, so types compared through different workloads still line
// up, and a workload run on only one type says nothing. Types never linked
// through a shared workload are measured only against each other, each
// group against its newest generation (the most runs, then the name,
// breaking a tie): a reference that two close, noisy measurements
// swapping places doesn't move.
func measuredSpeeds(speed map[string]map[string][]speedSample, now time.Time) map[string]TypeSpeed {
	type cell struct {
		w, t string
		y    float64 // log of the median, in ms
		n    int     // runs
		wt   float64 // its share of its workload's runs
	}
	var cells []cell
	for _, wk := range sortedKeys(speed) {
		var row []cell
		for _, it := range sortedKeys(speed[wk]) {
			var ms []int64
			for _, s := range speed[wk][it] {
				if usableSpeed(s, now) {
					ms = append(ms, s.MS)
				}
			}
			if len(ms) < speedMinRuns {
				continue
			}
			row = append(row, cell{w: wk, t: it, y: math.Log(float64(median(ms))), n: len(ms)})
		}
		if len(row) >= 2 {
			total := 0
			for _, c := range row {
				total += c.n
			}
			for i := range row {
				row[i].wt = float64(row[i].n) / float64(total)
			}
			cells = append(cells, row...)
		}
	}
	if len(cells) == 0 {
		return nil
	}
	// Types linked by a shared workload, grouped.
	parent := map[string]string{}
	var find func(string) string
	find = func(t string) string {
		if p, ok := parent[t]; ok && p != t {
			r := find(p)
			parent[t] = r
			return r
		}
		parent[t] = t
		return t
	}
	first := map[string]string{} // workload -> its first type
	for _, c := range cells {
		find(c.t)
		if f, ok := first[c.w]; ok {
			if a, b := find(f), find(c.t); a != b {
				parent[max(a, b)] = min(a, b)
			}
		} else {
			first[c.w] = c.t
		}
	}
	// log(median) = a[workload] + b[type]: alternating weighted means
	// converge on the least-squares fit.
	a, b := map[string]float64{}, map[string]float64{}
	for r := 0; r < speedFitRounds; r++ {
		sum, wt := map[string]float64{}, map[string]float64{}
		for _, c := range cells {
			sum[c.w] += c.wt * (c.y - b[c.t])
			wt[c.w] += c.wt
		}
		for w := range sum {
			a[w] = sum[w] / wt[w]
		}
		sum, wt = map[string]float64{}, map[string]float64{}
		for _, c := range cells {
			sum[c.t] += c.wt * (c.y - a[c.w])
			wt[c.t] += c.wt
		}
		moved := 0.0
		for t := range sum {
			nb := sum[t] / wt[t]
			moved = max(moved, math.Abs(nb-b[t]))
			b[t] = nb
		}
		if r > 0 && moved < 1e-9 {
			break
		}
	}
	runs, works := map[string]int{}, map[string]int{}
	for _, c := range cells {
		runs[c.t] += c.n
		works[c.t]++
	}
	// Each group's reference: its newest generation (unknown: oldest),
	// then the most runs, then the name.
	ref := map[string]string{}
	for _, t := range sortedKeys(b) {
		g := find(t)
		f, ok := ref[g]
		if !ok {
			ref[g] = t
			continue
		}
		gt, gf := typeGeneration(t), typeGeneration(f)
		if gt > gf || gt == gf && runs[t] > runs[f] {
			ref[g] = t
		}
	}
	out := make(map[string]TypeSpeed, len(b))
	for t, bt := range b {
		f := ref[find(t)]
		out[t] = TypeSpeed{Relative: math.Exp(b[f] - bt), Against: f, Runs: runs[t], Workload: works[t]}
	}
	return out
}

func median(ms []int64) int64 {
	s := append([]int64(nil), ms...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
