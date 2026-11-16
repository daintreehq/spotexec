package provider

import (
	"sort"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// How long a project's cold start takes varies by an order of magnitude
// between projects (#195): an image that compiles a tool from source takes
// 5-15 minutes to build, a large Node install 10-20. So run-cost placement
// prices the image and dependency parts of a cold start at how long this
// project's own builds and installs took on the runner, and falls back to
// coldImage and coldDeps only before any has been measured.
const (
	// coldWorkSamples bounds the measurements kept of a part (see
	// recordColdWork).
	coldWorkSamples = 16
	// coldWorkFor is how long a measurement is used.
	coldWorkFor = 30 * 24 * time.Hour
	// coldWorkCap bounds one part's estimate, whatever was measured.
	coldWorkCap = time.Hour
	// coldWorkProjects bounds the projects remembered per runner: the
	// least recently measured is forgotten first.
	coldWorkProjects = 64
	// coldWorkLockWait bounds how long a run waits to record one: it is
	// skipped rather than holding the run up.
	coldWorkLockWait = 2 * time.Second
)

// ColdWorkPart names the part of a cold start a measurement is of.
type ColdWorkPart string

const (
	ColdImage ColdWorkPart = "image" // building the project's container image
	ColdDeps  ColdWorkPart = "deps"  // installing the project's dependencies
)

// coldWork is what one project's builds and installs took on a runner: of
// each part, the measurements that can still be its longest in the last
// coldWorkFor, oldest (and longest) first, the latest last.
type coldWork struct {
	Image []coldSample `json:"image,omitempty"`
	Deps  []coldSample `json:"deps,omitempty"`
}

type coldSample struct {
	MS int64     `json:"ms"`
	At time.Time `json:"at"`
}

func (w *coldWork) samples(part ColdWorkPart) *[]coldSample {
	if part == ColdImage {
		return &w.Image
	}
	return &w.Deps
}

// latest is when the project was last measured.
func (w *coldWork) latest() time.Time {
	var at time.Time
	for _, ss := range [][]coldSample{w.Image, w.Deps} {
		for _, s := range ss {
			if s.At.After(at) {
				at = s.At
			}
		}
	}
	return at
}

// ColdEstimate is the time one part of a cold start is priced at, and
// where it came from.
type ColdEstimate struct {
	Source string    `json:"source"`           // "measured": this project's builds or installs; "default": none measured lately
	At     time.Time `json:"at,omitempty"`     // measured: when the one used (the longest) was taken
	Capped bool      `json:"capped,omitempty"` // measured: longer than coldWorkCap, so priced at it
}

// estimate is the time a part is priced at: the longest of its
// measurements in the last coldWorkFor (a build that reused a layer cache,
// or an install that reused a download cache, mustn't hide what a truly
// cold one takes), capped at coldWorkCap; else def.
func (w *coldWork) estimate(part ColdWorkPart, def time.Duration, now time.Time) (time.Duration, ColdEstimate) {
	var best coldSample
	if w != nil {
		for _, s := range *w.samples(part) {
			if usableSample(s, now) && s.MS > best.MS {
				best = s
			}
		}
	}
	if best.MS == 0 {
		return def, ColdEstimate{Source: "default"}
	}
	e := ColdEstimate{Source: "measured", At: best.At}
	if best.MS > coldWorkCap.Milliseconds() {
		e.Capped = true
		return coldWorkCap, e
	}
	return (time.Duration(best.MS) * time.Millisecond).Round(time.Second), e
}

func usableSample(s coldSample, now time.Time) bool {
	return s.MS > 0 && now.Sub(s.At) < coldWorkFor && !s.At.After(now.Add(time.Hour))
}

// RecordColdWork records how long a build of the project's image, or an
// install of its dependencies, took on a VM of runner r (only work that
// actually built or installed: not a pull, a restore or a reuse), so its
// next launch prices a region that lacks them by it. Only runners that
// place launches across regions weigh it.
func RecordColdWork(r *config.Runner, repoID string, part ColdWorkPart, took time.Duration) error {
	if r == nil || r.Type != config.TypeAWSSpot || !r.MultiRegion() || repoID == "" || took <= 0 {
		return nil
	}
	if part != ColdImage && part != ColdDeps {
		return nil
	}
	g := &AWSGlobal{r: r, dir: RunnerStateDir(r.Name)}
	return g.withStateWait(coldWorkLockWait, func(st *placementState) { recordColdWork(st, repoID, part, took, g.clock()) })
}

func recordColdWork(st *placementState, repoID string, part ColdWorkPart, took time.Duration, now time.Time) {
	if st.ColdWork == nil {
		st.ColdWork = map[string]*coldWork{}
	}
	// What has expired goes first: it counts for nothing below.
	forgetOldColdWork(st, now)
	w := st.ColdWork[repoID]
	if w == nil {
		w = &coldWork{}
		st.ColdWork[repoID] = w
	}
	// The longest in a sliding window: an earlier measurement no longer
	// than this one can never be the longest again (this one outlives it),
	// so it goes; what is left runs from the oldest and longest to this
	// one. A quick build that reused Docker's layer cache, or an install
	// that reused a download cache, so never pushes out a longer one before
	// it expires, and is still there to use once it has.
	ss := w.samples(part)
	kept := (*ss)[:0]
	for _, s := range *ss {
		// One stamped later, by a clock since set back, outlives this one.
		if s.MS > took.Milliseconds() || s.At.After(now) {
			kept = append(kept, s)
		}
	}
	*ss = append(kept, coldSample{MS: took.Milliseconds(), At: now})
	if n := len(*ss); n > coldWorkSamples {
		// A project faster nearly every run: give up the measurement
		// closest to the next one, which then stands in for it.
		drop := 0
		for i := 1; i < n-1; i++ {
			if gap(*ss, i) < gap(*ss, drop) {
				drop = i
			}
		}
		*ss = append((*ss)[:drop:drop], (*ss)[drop+1:]...)
	}
	if len(st.ColdWork) > coldWorkProjects {
		repos := make([]string, 0, len(st.ColdWork))
		for k := range st.ColdWork {
			repos = append(repos, k)
		}
		sort.Slice(repos, func(i, j int) bool {
			return st.ColdWork[repos[i]].latest().Before(st.ColdWork[repos[j]].latest())
		})
		for _, k := range repos[:len(repos)-coldWorkProjects] {
			delete(st.ColdWork, k)
		}
	}
}

// gap is how much longer ss[i] is than ss[i+1], as a ratio.
func gap(ss []coldSample, i int) float64 {
	return float64(ss[i].MS) / float64(max(ss[i+1].MS, 1))
}

// forgetOldColdWork drops measurements too old to use, and projects left
// with none.
func forgetOldColdWork(st *placementState, now time.Time) {
	for repo, w := range st.ColdWork {
		for _, part := range []ColdWorkPart{ColdImage, ColdDeps} {
			ss := w.samples(part)
			kept := (*ss)[:0]
			for _, s := range *ss {
				if usableSample(s, now) {
					kept = append(kept, s)
				}
			}
			*ss = kept
		}
		if len(w.Image) == 0 && len(w.Deps) == 0 {
			delete(st.ColdWork, repo)
		}
	}
}
