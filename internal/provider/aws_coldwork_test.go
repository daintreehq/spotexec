package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// A region that holds the project's dependency archive keeps the launch
// against one a few cents an hour cheaper once the project's installs are
// known to be long (#195); with no measurement yet, the default holds.
func TestColdWorkPricesMeasuredInstall(t *testing.T) {
	g, f := twoRegionEnv(t, "0.150", "0.100", nil)
	f.cached = map[string]map[string]bool{"r-one-1": {"repo1/k1.tar": true}}
	pj := &Project{RepoID: "repo1", Deps: true, DepsKey: "k1"}

	k := rankFor(t, g, pj)
	if k.Prices[0].Region != "r-two-1" || k.Model.DepsMinutes != 2 || k.Model.DepsFrom.Source != "default" || k.Model.ImageFrom.Source != "default" {
		t.Fatalf("before any install: order %v, model %+v", k.Prices, *k.Model)
	}
	if c := costIn(t, k, "r-two-1"); !near(c.Cold, (0.100+1)*2/60) {
		t.Fatalf("default deps part: %+v", c)
	}

	// A quick install, then a long one (an install that reused a download
	// cache mustn't hide what a cold one takes): the longest is used.
	for _, d := range []time.Duration{90 * time.Second, 15 * time.Minute, 4 * time.Minute} {
		if err := RecordColdWork(g.r, "repo1", ColdDeps, d); err != nil {
			t.Fatal(err)
		}
	}
	k = rankFor(t, g, pj)
	m := k.Model
	if m.DepsMinutes != 15 || m.DepsFrom.Source != "measured" || m.DepsFrom.At.IsZero() || m.ColdMinutes != 1+15+1 {
		t.Fatalf("measured model %+v", *m)
	}
	if m.ImageMinutes != 1 || m.ImageFrom.Source != "default" {
		t.Fatalf("image unmeasured: %+v", *m)
	}
	if c := costIn(t, k, "r-two-1"); !near(c.ColdParts.Deps.Cost, (0.100+1)*15/60) || k.Prices[0].Region != "r-one-1" {
		t.Fatalf("order %v, cold %+v", k.Prices, c)
	}

	// Another project of the runner, and a launch outside any project, use
	// the defaults.
	if m := rankFor(t, g, &Project{RepoID: "repo2", Deps: true, DepsKey: "k1"}).Model; m.DepsMinutes != 2 || m.DepsFrom.Source != "default" {
		t.Fatalf("other project: %+v", *m)
	}
	if m := rankFor(t, g, nil).Model; m.DepsMinutes != 2 || m.DepsFrom.Source != "default" {
		t.Fatalf("no project: %+v", *m)
	}

	// Past coldWorkFor the measurements are forgotten.
	later := time.Now().Add(coldWorkFor + time.Hour)
	g.now = func() time.Time { return later }
	if m := rankFor(t, g, pj).Model; m.DepsFrom.Source != "default" {
		t.Fatalf("old measurements: %+v", *m)
	}
	b, err := os.ReadFile(filepath.Join(g.dir, "placement.json"))
	if err != nil || strings.Contains(string(b), "cold_work") {
		t.Fatalf("not forgotten: %s %v", b, err)
	}
}

// A project whose image takes long to build keeps its launch in the region
// that holds the image, against one a few cents cheaper; measured before the
// first price lookup, so a freshly fetched table carries the project.
func TestColdWorkPricesMeasuredImage(t *testing.T) {
	g, f := twoRegionEnv(t, "0.150", "0.100", nil)
	f.images = map[string]map[string]string{"r-one-1": {"t1": "sha256:one"}}
	pj := &Project{RepoID: "repo1", Image: true, ImageTag: "t1"}
	if err := RecordColdWork(g.r, "repo1", ColdImage, 12*time.Minute); err != nil {
		t.Fatal(err)
	}
	tbl, err := g.Prices(WithProject(context.Background(), pj), false)
	if err != nil || tbl.Cached {
		t.Fatalf("fresh table: cached %v, %v", tbl.Cached, err)
	}
	k := g.Rank(tbl)
	m := k.Model
	if m.ImageMinutes != 12 || m.ImageFrom.Source != "measured" || m.DepsFrom.Source != "default" {
		t.Fatalf("model %+v", *m)
	}
	two := costIn(t, k, "r-two-1")
	if two.ColdParts.Image.Warm || !near(two.ColdParts.Image.Cost, (0.100+1)*12/60) || k.Prices[0].Region != "r-one-1" {
		t.Fatalf("order %v, cold %+v", k.Prices, *two.ColdParts)
	}
	// With the default minute, the cheaper region would win.
	if k := rankFor(t, g, &Project{RepoID: "repo2", Image: true, ImageTag: "t1"}); k.Prices[0].Region != "r-two-1" {
		t.Fatalf("default order %v", k.Prices)
	}
}

func TestColdWorkEstimate(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	st := &placementState{}
	// A long cold build, then quick ones that reused the layer cache: the
	// long one stays the estimate, and when it was taken says so.
	recordColdWork(st, "r", ColdImage, 20*time.Minute, now.Add(-time.Hour))
	for i := 7; i >= 1; i-- {
		recordColdWork(st, "r", ColdImage, time.Duration(i)*10*time.Second, now.Add(time.Duration(8-i)*time.Second))
	}
	w := st.ColdWork["r"]
	if len(w.Image) > coldWorkSamples || w.Image[0].MS != 1200000 || w.Image[len(w.Image)-1].MS != 10000 || len(w.Deps) != 0 {
		t.Fatalf("kept %+v", w.Image)
	}
	d, e := w.estimate(ColdImage, coldImage, now)
	if d != 20*time.Minute || e.Source != "measured" || !e.At.Equal(now.Add(-time.Hour)) || e.Capped {
		t.Fatalf("estimate %v %+v", d, e)
	}
	if d, e := w.estimate(ColdDeps, coldDeps, now); d != 2*time.Minute || e.Source != "default" {
		t.Fatalf("deps %v %+v", d, e)
	}
	// A measurement at least as long replaces the earlier ones it outlives.
	recordColdWork(st, "r", ColdImage, 20*time.Minute, now.Add(10*time.Second))
	if len(w.Image) != 1 || !w.Image[0].At.Equal(now.Add(10*time.Second)) {
		t.Fatalf("after an equal build: %+v", w.Image)
	}

	// Five long builds a month back, then a shorter one today: once the long
	// ones expire, today's is the longest left, not the default.
	st = &placementState{}
	for i := 0; i < 5; i++ {
		recordColdWork(st, "r", ColdDeps, 20*time.Minute, now.Add(-29*day+time.Duration(i)*time.Minute))
	}
	recordColdWork(st, "r", ColdDeps, 15*time.Minute, now)
	w = st.ColdWork["r"]
	if d, _ := w.estimate(ColdDeps, coldDeps, now); d != 20*time.Minute {
		t.Fatalf("before expiry %v", d)
	}
	if d, e := w.estimate(ColdDeps, coldDeps, now.Add(2*day)); d != 15*time.Minute || !e.At.Equal(now) {
		t.Fatalf("after expiry %v %+v", d, e)
	}
	forgetOldColdWork(st, now.Add(2*day))
	if len(w.Deps) != 1 || w.Deps[0].MS != 900000 {
		t.Fatalf("forgot %+v", w.Deps)
	}
	// Expired measurements don't push out a new one either.
	st = &placementState{ColdWork: map[string]*coldWork{"r": {Deps: []coldSample{{MS: 1200000, At: now.Add(-31 * day)}}}}}
	recordColdWork(st, "r", ColdDeps, 15*time.Minute, now)
	if w := st.ColdWork["r"]; len(w.Deps) != 1 || w.Deps[0].MS != 900000 {
		t.Fatalf("after an expired one: %+v", w.Deps)
	}

	// A project faster every run keeps a bounded list, with the latest,
	// giving up the measurement closest to the next: a 40-minute install
	// after a month of 60 down to 45 is still the estimate once they
	// expire, and a quick one today doesn't push it out.
	st = &placementState{}
	for i := 0; i < coldWorkSamples; i++ {
		recordColdWork(st, "r", ColdDeps, time.Duration(60-i)*time.Minute, now.Add(-29*day+time.Duration(i)*time.Minute))
	}
	recordColdWork(st, "r", ColdDeps, 40*time.Minute, now.Add(-day))
	recordColdWork(st, "r", ColdDeps, time.Minute, now)
	w = st.ColdWork["r"]
	if n := len(w.Deps); n != coldWorkSamples || w.Deps[n-1].MS != 60000 || w.Deps[n-2].MS != 2400000 {
		t.Fatalf("bounded %+v", w.Deps)
	}
	if d, _ := w.estimate(ColdDeps, coldDeps, now); d != 58*time.Minute {
		t.Fatalf("bounded, now %v", d)
	}
	if d, _ := w.estimate(ColdDeps, coldDeps, now.Add(2*day)); d != 40*time.Minute {
		t.Fatalf("bounded, after expiry %v", d)
	}

	// The one given up is the closest to the next, wherever it is.
	st = &placementState{}
	for i, sec := range []int{600, 400, 399, 300, 250, 200, 170, 140, 120, 100, 80, 60, 50, 40, 30, 20, 10} {
		recordColdWork(st, "r", ColdDeps, time.Duration(sec)*time.Second, now.Add(time.Duration(i)*time.Minute))
	}
	if d := st.ColdWork["r"].Deps; len(d) != coldWorkSamples || d[0].MS != 600000 || d[1].MS != 399000 {
		t.Fatalf("gave up %+v", d)
	}

	// After the clock is set back, an equal measurement stamped earlier
	// doesn't push out one stamped later (which expires later).
	st = &placementState{}
	recordColdWork(st, "r", ColdDeps, 15*time.Minute, now.Add(30*time.Minute))
	recordColdWork(st, "r", ColdDeps, 15*time.Minute, now)
	if d, e := st.ColdWork["r"].estimate(ColdDeps, coldDeps, now.Add(30*day+15*time.Minute)); d != 15*time.Minute || !e.At.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("clock set back: %v %+v", d, e)
	}

	// Rounded to the second; an hour exactly isn't capped, past it is.
	st = &placementState{}
	recordColdWork(st, "r", ColdDeps, 5*time.Minute+1400*time.Millisecond, now)
	w = st.ColdWork["r"]
	if d, _ := w.estimate(ColdDeps, coldDeps, now); d != 5*time.Minute+time.Second {
		t.Fatalf("rounded %v", d)
	}
	recordColdWork(st, "r", ColdDeps, time.Hour, now)
	if d, e := w.estimate(ColdDeps, coldDeps, now); d != time.Hour || e.Capped {
		t.Fatalf("an hour %v %+v", d, e)
	}
	recordColdWork(st, "r", ColdDeps, 3*time.Hour, now)
	if d, e := w.estimate(ColdDeps, coldDeps, now); d != time.Hour || !e.Capped {
		t.Fatalf("capped %v %+v", d, e)
	}

	// Each part expires on its own; a project with neither is forgotten.
	st = &placementState{}
	recordColdWork(st, "r", ColdImage, time.Minute, now.Add(-29*day))
	recordColdWork(st, "r", ColdDeps, time.Minute, now)
	forgetOldColdWork(st, now.Add(2*day))
	if w := st.ColdWork["r"]; w == nil || len(w.Image) != 0 || len(w.Deps) != 1 {
		t.Fatalf("image forgotten, deps kept: %+v", w)
	}
	forgetOldColdWork(st, now.Add(31*day))
	if len(st.ColdWork) != 0 {
		t.Fatalf("all old: %+v", st.ColdWork)
	}

	// A measurement from the future is no evidence.
	st = &placementState{}
	recordColdWork(st, "r", ColdDeps, 9*time.Minute, now.Add(2*time.Hour))
	if d, e := st.ColdWork["r"].estimate(ColdDeps, coldDeps, now); d != coldDeps || e.Source != "default" {
		t.Fatalf("future %v %+v", d, e)
	}
	forgetOldColdWork(st, now)
	if len(st.ColdWork) != 0 {
		t.Fatalf("future kept: %+v", st.ColdWork)
	}
	// No record at all.
	var none *coldWork
	if d, e := none.estimate(ColdImage, coldImage, now); d != time.Minute || e.Source != "default" {
		t.Fatalf("nil %v %+v", d, e)
	}
}

// The projects remembered per runner are bounded: the least recently
// measured goes first, whichever part was measured.
func TestColdWorkForgetsLeastRecentProject(t *testing.T) {
	now := time.Now()
	st := &placementState{}
	name := func(i int) string { return fmt.Sprintf("repo%03d", i) }
	for i := 0; i < coldWorkProjects; i++ {
		recordColdWork(st, name(i), ColdDeps, time.Minute, now.Add(time.Duration(i)*time.Second))
	}
	// The first measured runs again, quicker: that still counts.
	recordColdWork(st, name(0), ColdDeps, time.Second, now.Add(time.Hour))
	recordColdWork(st, "new", ColdDeps, time.Minute, now.Add(2*time.Hour))
	if len(st.ColdWork) != coldWorkProjects || st.ColdWork[name(0)] == nil || st.ColdWork[name(0)].Deps[0].MS != 60000 ||
		st.ColdWork[name(1)] != nil || st.ColdWork["new"] == nil {
		t.Fatalf("%d projects: %s kept %v, %s kept %v", len(st.ColdWork), name(0), st.ColdWork[name(0)] != nil, name(1), st.ColdWork[name(1)] != nil)
	}
	// A project with nothing usable left goes before any that has.
	delete(st.ColdWork, "new")
	st.ColdWork["skewed"] = &coldWork{Deps: []coldSample{{MS: 60000, At: now.Add(5 * time.Hour)}}}
	recordColdWork(st, "newer", ColdDeps, time.Minute, now.Add(3*time.Hour))
	if len(st.ColdWork) != coldWorkProjects || st.ColdWork["skewed"] != nil || st.ColdWork[name(2)] == nil || st.ColdWork["newer"] == nil {
		t.Fatalf("%d projects, skewed %v, %s %v", len(st.ColdWork), st.ColdWork["skewed"] != nil, name(2), st.ColdWork[name(2)] != nil)
	}
}

// Recording waits only briefly for placement.lock: a run isn't held up by
// a process holding it, and a lock freed in time still records.
func TestRecordColdWorkSkipsBusyState(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "many", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{"us-east-1", "eu-north-1"}}
	g := &AWSGlobal{r: r, dir: RunnerStateDir(r.Name)}
	hold := func(d time.Duration) chan error {
		held, done := make(chan struct{}), make(chan error, 1)
		go func() {
			done <- g.withState(func(*placementState) {
				close(held)
				time.Sleep(d)
			})
		}()
		<-held
		return done
	}
	recorded := func() int {
		n := 0
		_ = g.withState(func(st *placementState) {
			if w := st.ColdWork["repo1"]; w != nil {
				n = len(w.Deps)
			}
		})
		return n
	}

	done := hold(coldWorkLockWait + time.Second)
	start := time.Now()
	err := RecordColdWork(r, "repo1", ColdDeps, time.Minute)
	took := time.Since(start)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, errStateBusy) || took < coldWorkLockWait || took > coldWorkLockWait+time.Second || recorded() != 0 {
		t.Fatalf("busy: %v after %v, %d recorded", err, took, recorded())
	}

	done = hold(300 * time.Millisecond)
	if err := RecordColdWork(r, "repo1", ColdDeps, time.Minute); err != nil {
		t.Fatalf("freed in time: %v", err)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if recorded() != 1 {
		t.Fatalf("freed in time, but not recorded")
	}
}

// Only runners that place across regions keep measurements; a runner in
// one region gets no placement state from them.
func TestRecordColdWorkOnlyAcrossRegions(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	one := &config.Runner{Name: "one", Type: config.TypeAWSSpot, Region: "us-east-1"}
	if err := RecordColdWork(one, "repo1", ColdImage, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(RunnerStateDir("one"), "placement.json")); !os.IsNotExist(err) {
		t.Fatalf("single-region runner got state: %v", err)
	}
	many := &config.Runner{Name: "many", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{"us-east-1", "eu-north-1"}}
	for _, bad := range []time.Duration{0, -time.Second} {
		if err := RecordColdWork(many, "repo1", ColdImage, bad); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordColdWork(many, "", ColdImage, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(RunnerStateDir("many"), "placement.json")); !os.IsNotExist(err) {
		t.Fatalf("nothing to record, yet state: %v", err)
	}
	if err := RecordColdWork(many, "repo1", ColdImage, 8*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(RunnerStateDir("many"), "placement.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st placementState
	if err := json.Unmarshal(b, &st); err != nil || len(st.ColdWork["repo1"].Image) != 1 || st.ColdWork["repo1"].Image[0].MS != 480000 {
		t.Fatalf("state %s, %v", b, err)
	}
}
