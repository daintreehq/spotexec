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
//
// Jobs share a VM's CPUs, so a run's time also says how busy its VM was:
// the same typecheck took 45 s on a quiet r7a and 180 s beside five other
// jobs (#296). So a workload's runs on a type count by the VMs they ran
// on (runs on one VM share its load), and by how closely those VMs
// agree, and the fit leans on the generation assumption as far as they
// don't: a few VMs, or scattered ones, barely move a type from its
// generation's speed, and many consistent ones replace it.
const (
	// speedSamples bounds the runs kept of one workload on one type: enough
	// that busy and quiet VMs both feed them.
	speedSamples = 30
	// speedFor is how long a run counts.
	speedFor = 30 * 24 * time.Hour
	// SpeedMinRuns is the runs of a workload on a type before they count:
	// one run on a VM busy with others says little.
	SpeedMinRuns = 5
	// speedSpread is the spread (the standard deviation of log run time)
	// assumed from one VM's runs of a workload to another's, as a shared
	// VM's load moves them, before the VMs themselves show theirs: it
	// counts as speedSpreadVMs VMs' worth of evidence, so two VMs that
	// happen to agree aren't taken as proof.
	speedSpread    = 0.25
	speedSpreadVMs = 2.0
	// speedCap bounds how far measured speeds move an option's run cost
	// from what its generation alone says: three hours of its Spot price
	// per hour of work. Runs on busy VMs can make one type look slow on
	// every VM it ran on lately, which no spread reveals; a type truly
	// slower still pays several times its price for it, enough to pass
	// it over among similar types.
	speedCap = 3.0
	// speedWorkloads bounds the workloads remembered per runner, and
	// speedCells the workload and type pairs, so placement.json stays
	// small: the least recently run is forgotten first.
	speedWorkloads = 64
	speedCells     = 256
	// speedLockWait bounds how long a run waits to record its time: it is
	// skipped rather than holding the run up.
	speedLockWait = 500 * time.Millisecond
)

// speedStray is how far a type's speed is expected to stray from its
// generation's (log, a standard deviation): about one generation.
var speedStray = math.Log(genSpeedup)

// speedSample is one run's command time.
type speedSample struct {
	MS int64     `json:"ms"`
	At time.Time `json:"at"`
	// VM is the instance it ran on ("": unknown, as older clients kept
	// none: such runs count as one VM).
	VM string `json:"vm,omitempty"`
}

// TypeSpeed is a type's speed as measured from the runner's own runs.
type TypeSpeed struct {
	// Relative is its speed against the newest generation among the types
	// it was measured with (Generation; 0: none known), whose measured
	// types average 1.
	Relative   float64 `json:"relative"`
	Generation int     `json:"generation"`
	// Group names the types measured against each other, through shared
	// workloads: the first of them by name. Speeds in different groups
	// meet only through their generations.
	Group    string `json:"group"`
	Runs     int    `json:"runs"`      // the runs behind it
	Workload int    `json:"workloads"` // the workloads they were of
}

// RecordRunSpeed records how long a successful run's command took on a VM
// of instance type itype (instance vm), for runner r's next launches to
// weigh that type's speed by. workload names what ran (see
// client.SpeedWorkload): only runs of the same workload are compared. Only
// vcpus runners weigh speed.
func RecordRunSpeed(r *config.Runner, workload, itype, vm string, took time.Duration) error {
	if r == nil || r.Type != config.TypeAWSSpot || !r.ByRequirements() || workload == "" || itype == "" || took.Milliseconds() <= 0 {
		return nil
	}
	g := &AWSGlobal{r: r, dir: RunnerStateDir(r.Name)}
	return g.withStateWait(speedLockWait, func(st *placementState) { recordSpeed(st, workload, itype, vm, took, g.clock()) })
}

func recordSpeed(st *placementState, workload, itype, vm string, took time.Duration, now time.Time) {
	forgetOldSpeed(st, now)
	if st.Speed == nil {
		st.Speed = map[string]map[string][]speedSample{}
	}
	w := st.Speed[workload]
	if w == nil {
		w = map[string][]speedSample{}
		st.Speed[workload] = w
	}
	ss := append(w[itype], speedSample{MS: took.Milliseconds(), At: now, VM: vm})
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
			var at time.Time
			for _, s := range ss {
				if s.At.After(at) {
					at = s.At
				}
			}
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
// runs on a type count once there are SpeedMinRuns of them, as the
// geometric mean of each VM's (a mean, not a median, which jumps from
// quiet runs to busy ones when they are about as many), weighed by how
// sure it is: by the VMs' number and how little they spread (see
// speedEstimate). The means are fitted as a workload's
// own length times a type's own slowness, so types compared through
// different workloads still line up, and a workload run on only one type
// says nothing. Each type's slowness is also drawn toward its generation's
// (genSpeedup a generation, give or take about one), so a measurement
// moves a type only as far as its runs agree: a few scattered runs leave
// it near its generation's speed, and many consistent ones place it where
// they say. Types never linked through a shared workload are measured
// only against each other, each group against its newest generation,
// which keeps its speed by generation in the ranking: a reference that
// doesn't move when two close, noisy measurements swap places.
func measuredSpeeds(speed map[string]map[string][]speedSample, now time.Time) map[string]TypeSpeed {
	type cell struct {
		w, t string
		y    float64 // log of the typical time, in ms
		n    int     // runs
		wt   float64 // how sure y is: 1 / its variance
	}
	var cells []cell
	for _, wk := range sortedKeys(speed) {
		var row []cell
		for _, it := range sortedKeys(speed[wk]) {
			var ss, tagged []speedSample
			for _, s := range speed[wk][it] {
				if usableSpeed(s, now) {
					ss = append(ss, s)
					if s.VM != "" {
						tagged = append(tagged, s)
					}
				}
			}
			// Runs without a VM (older clients, which also drop the VMs
			// of runs they save) count only until enough runs say theirs:
			// the same VM may be among both.
			if len(tagged) >= SpeedMinRuns {
				ss = tagged
			}
			if len(ss) < SpeedMinRuns {
				continue
			}
			y, v := speedEstimate(ss)
			row = append(row, cell{w: wk, t: it, y: y, n: len(ss), wt: 1 / v})
		}
		if len(row) >= 2 {
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
	groups := map[string][]string{}
	for _, t := range sortedTypes(cells, func(c cell) string { return c.t }) {
		groups[find(t)] = append(groups[find(t)], t)
	}
	runs, works := map[string]int{}, map[string]int{}
	for _, c := range cells {
		runs[c.t] += c.n
		works[c.t]++
	}
	stray := speedStray
	out := map[string]TypeSpeed{}
	for _, ts := range groups {
		idx := make(map[string]int, len(ts))
		for i, t := range ts {
			idx[t] = i
		}
		newest := 0
		for _, t := range ts {
			newest = max(newest, typeGeneration(t))
		}
		// log(time) = a[workload] + b[type], fitted by weighted least
		// squares, with b drawn toward its generation's (a type of unknown
		// generation counts as the newest). With each workload's a
		// eliminated, b solves L b = r (L a weighted Laplacian of the
		// types, plus the draw on its diagonal; see fitTypes).
		n := len(ts)
		L := make([][]float64, n)
		for i := range L {
			L[i] = make([]float64, n)
		}
		r := make([]float64, n)
		for i, t := range ts {
			gen := typeGeneration(t)
			if gen == 0 {
				gen = newest
			}
			L[i][i] += 1 / (stray * stray)
			r[i] += -float64(gen) * math.Log(genSpeedup) / (stray * stray)
		}
		byW := map[string][]cell{}
		for _, c := range cells {
			if _, ok := idx[c.t]; ok {
				byW[c.w] = append(byW[c.w], c)
			}
		}
		for _, wk := range sortedKeys(byW) {
			row := byW[wk]
			W, Y := 0.0, 0.0
			for _, c := range row {
				W += c.wt
				Y += c.wt * c.y
			}
			for _, c := range row {
				i := idx[c.t]
				r[i] += c.wt*c.y - c.wt*Y/W
				for _, d := range row {
					L[i][idx[d.t]] -= c.wt * d.wt / W
				}
				L[i][i] += c.wt
			}
		}
		b := fitTypes(L, r)
		// Centred on the newest generation's types (all of them when none
		// has a known generation).
		mean, k := 0.0, 0
		for i, t := range ts {
			if typeGeneration(t) == newest {
				mean += b[i]
				k++
			}
		}
		mean /= float64(k)
		for i, t := range ts {
			out[t] = TypeSpeed{Relative: math.Exp(mean - b[i]), Generation: newest, Group: ts[0], Runs: runs[t], Workload: works[t]}
		}
	}
	return out
}

// speedEstimate is a workload's typical time on a type, from its runs
// there (log, in ms), and that estimate's variance. Runs on one VM share
// its load, so each VM counts once, as the mean of its runs' logs; the
// estimate is the VMs' mean, and its variance their spread over their
// number, the spread pooled with speedSpread (speedSpreadVMs VMs' worth)
// so a few VMs can't claim more agreement than they've shown.
func speedEstimate(ss []speedSample) (y, v float64) {
	sum, n := map[string]float64{}, map[string]int{}
	for _, s := range ss {
		sum[s.VM] += math.Log(float64(s.MS))
		n[s.VM]++
	}
	var means []float64
	for _, vm := range sortedKeys(sum) {
		means = append(means, sum[vm]/float64(n[vm]))
	}
	m := float64(len(means))
	for _, x := range means {
		y += x
	}
	y /= m
	dev := 0.0
	for _, x := range means {
		dev += (x - y) * (x - y)
	}
	spread := (speedSpreadVMs*speedSpread*speedSpread + dev) / (speedSpreadVMs + m - 1)
	return y, spread / m
}

// fitTypes solves L b = r for a group's types. The draw toward each type's
// generation makes L positive definite, so Gaussian elimination with
// partial pivoting solves it.
func fitTypes(L [][]float64, r []float64) []float64 {
	n := len(r)
	a := make([][]float64, n)
	for i := range a {
		a[i] = append(append([]float64(nil), L[i]...), r[i])
	}
	for col := 0; col < n; col++ {
		p := col
		for i := col + 1; i < n; i++ {
			if math.Abs(a[i][col]) > math.Abs(a[p][col]) {
				p = i
			}
		}
		a[col], a[p] = a[p], a[col]
		if a[col][col] == 0 {
			continue
		}
		for i := col + 1; i < n; i++ {
			f := a[i][col] / a[col][col]
			for j := col; j <= n; j++ {
				a[i][j] -= f * a[col][j]
			}
		}
	}
	b := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		v := a[i][n]
		for j := i + 1; j < n; j++ {
			v -= a[i][j] * b[j]
		}
		if a[i][i] != 0 {
			b[i] = v / a[i][i]
		}
	}
	return b
}

// sortedTypes is the distinct keys of cells, sorted.
func sortedTypes[C any](cells []C, key func(C) string) []string {
	seen := map[string]bool{}
	for _, c := range cells {
		seen[key(c)] = true
	}
	return sortedKeys(seen)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
