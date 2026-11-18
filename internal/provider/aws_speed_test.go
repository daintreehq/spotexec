package provider

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// runs records each duration (seconds) as one run of workload on itype.
func runs(st *placementState, now time.Time, workload, itype string, secs ...float64) {
	for i, s := range secs {
		recordSpeed(st, workload, itype, time.Duration(s*float64(time.Second)), now.Add(time.Duration(i)*time.Second))
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// exactFit draws types toward their generation's speed too weakly to
// matter, for tests of the fit itself.
func exactFit(t *testing.T) {
	old := speedStray
	speedStray = 1e4
	t.Cleanup(func() { speedStray = old })
}

// The issue's typecheck (#279): r8a's runs took a median 72 s, m7a's 103 s,
// so m7a runs at 0.69x r8a's speed, against r8a (the newer generation).
func TestMeasuredSpeedsOneWorkload(t *testing.T) {
	exactFit(t)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	runs(st, now, "typecheck", "r8a.2xlarge", 80, 72, 64, 72, 75)
	runs(st, now, "typecheck", "m7a.2xlarge", 101, 105, 112, 73, 103)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	m7a, r8a := got["m7a.2xlarge"], got["r8a.2xlarge"]
	if m7a.Generation != 8 || m7a.Group != "m7a.2xlarge" || !approx(m7a.Relative, 72.0/103) || m7a.Runs != 5 || m7a.Workload != 1 {
		t.Fatalf("m7a %+v", m7a)
	}
	if r8a.Generation != 8 || r8a.Group != "m7a.2xlarge" || !approx(r8a.Relative, 1) || r8a.Runs != 5 {
		t.Fatalf("r8a %+v", r8a)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
}

// Too little to compare is no measurement: fewer than speedMinRuns runs on
// a type, a workload run on one type only, or runs older than speedFor.
func TestMeasuredSpeedsNeedsComparableRuns(t *testing.T) {
	exactFit(t)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	runs(st, now, "w1", "r8a.2xlarge", 70, 70, 70, 70, 70)
	runs(st, now, "w1", "m7a.2xlarge", 100, 100, 100, 100) // four runs
	runs(st, now, "w2", "m6a.2xlarge", 90, 90, 90, 90, 90)
	if got := measuredSpeeds(st.Speed, now); got != nil {
		t.Fatalf("got %+v", got)
	}
	runs(st, now, "w1", "m7a.2xlarge", 100)
	if got := measuredSpeeds(st.Speed, now); len(got) != 2 || !approx(got["m7a.2xlarge"].Relative, 0.7) {
		t.Fatalf("got %+v", got)
	}
	if got := measuredSpeeds(st.Speed, now.Add(speedFor+time.Hour)); got != nil {
		t.Fatalf("expired: %+v", got)
	}
}

// Types compared through different workloads line up (A against B in one,
// B against C in another), each workload counting the same however often
// it ran; types never sharing a workload with them are a group of their
// own, against its own newest generation.
func TestMeasuredSpeedsChainsAndGroups(t *testing.T) {
	exactFit(t)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	// w1: m6a takes twice as long as m8a. w2: m7a 1.5x m8a... but measured
	// against m6a: m6a 60, m7a 45 (so m7a is 1.5x m8a by way of m6a).
	runs(st, now, "w1", "m8a.2xlarge", 30, 30, 30, 30, 30)
	runs(st, now, "w1", "m6a.2xlarge", 60, 60, 60, 60, 60)
	runs(st, now, "w2", "m6a.2xlarge", 600, 600, 600, 600, 600, 600, 600, 600, 600)
	runs(st, now, "w2", "m7a.2xlarge", 450, 450, 450, 450, 450)
	// Another group: c7i and c6i, c6i 0.8x.
	runs(st, now, "w3", "c6i.2xlarge", 50, 50, 50, 50, 50)
	runs(st, now, "w3", "c7i.2xlarge", 40, 40, 40, 40, 40)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	want := map[string]TypeSpeed{
		"m8a.2xlarge": {Relative: 1, Generation: 8, Group: "m6a.2xlarge", Runs: 5, Workload: 1},
		"m6a.2xlarge": {Relative: 0.5, Generation: 8, Group: "m6a.2xlarge", Runs: 5 + 9, Workload: 2},
		"m7a.2xlarge": {Relative: 30.0 / 45, Generation: 8, Group: "m6a.2xlarge", Runs: 5, Workload: 1},
		"c7i.2xlarge": {Relative: 1, Generation: 7, Group: "c6i.2xlarge", Runs: 5, Workload: 1},
		"c6i.2xlarge": {Relative: 0.8, Generation: 7, Group: "c6i.2xlarge", Runs: 5, Workload: 1},
	}
	checkSpeeds(t, got, want)
}

func checkSpeeds(t *testing.T, got, want map[string]TypeSpeed) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for it, w := range want {
		g := got[it]
		if !approx(g.Relative, w.Relative) || g.Generation != w.Generation || g.Group != w.Group || g.Runs != w.Runs || g.Workload != w.Workload {
			t.Errorf("%s: %+v, want %+v", it, g, w)
		}
	}
}

// A long chain of comparisons resolves exactly: eight types, with many
// workloads at the ends and one per link between, A-D running twice as
// fast as E-H.
func TestMeasuredSpeedsLongChain(t *testing.T) {
	exactFit(t)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	types := []string{"a1.x", "b1.x", "c1.x", "d1.x", "e1.x", "f1.x", "g1.x", "h1.x"}
	secs := func(i int) float64 { return float64(10 * (1 + i/4)) }
	n := 0
	link := func(i, times int) {
		for k := 0; k < times; k++ {
			w := fmt.Sprintf("w%02d", n)
			n++
			runs(st, now, w, types[i], secs(i), secs(i), secs(i), secs(i), secs(i))
			runs(st, now, w, types[i+1], secs(i+1), secs(i+1), secs(i+1), secs(i+1), secs(i+1))
		}
	}
	link(0, 29)
	for i := 1; i < 6; i++ {
		link(i, 1)
	}
	link(6, 29)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	// All generation 1: centred on all eight, which average sqrt(1*0.5).
	for i, it := range types {
		want := 1 / math.Sqrt(0.5)
		if i >= 4 {
			want /= 2
		}
		if g := got[it]; !approx(g.Relative, want) {
			t.Errorf("%s: %+v, want %v", it, g, want)
		}
	}
}

// Each workload weighs by how sure its medians are: two whose runs agree
// as closely and as often, one comparing m7a with m8a at 0.5 and another
// at 0.8, meet at their geometric mean; runs that scatter widely weigh
// little against runs that agree.
func TestMeasuredSpeedsWorkloadsWeighByAgreement(t *testing.T) {
	exactFit(t)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	runs(st, now, "steady", "m8a.2xlarge", 10, 10, 10, 10, 10)
	runs(st, now, "steady", "m7a.2xlarge", 20, 20, 20, 20, 20)
	runs(st, now, "also", "m8a.2xlarge", 40, 40, 40, 40, 40)
	runs(st, now, "also", "m7a.2xlarge", 50, 50, 50, 50, 50)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	if g := got["m7a.2xlarge"]; !approx(g.Relative, math.Sqrt(0.5*0.8)) || g.Workload != 2 || g.Runs != 10 {
		t.Fatalf("m7a %+v, want %v", g, math.Sqrt(0.4))
	}
	st = &placementState{}
	runs(st, now, "steady", "m8a.2xlarge", 10, 10, 10, 10, 10)
	runs(st, now, "steady", "m7a.2xlarge", 20, 20, 20, 20, 20)
	runs(st, now, "scattered", "m8a.2xlarge", 20, 30, 40, 60, 80)
	runs(st, now, "scattered", "m7a.2xlarge", 25, 35, 50, 75, 100)
	got = measuredSpeeds(st.Speed, now.Add(time.Minute))
	if g := got["m7a.2xlarge"]; g.Relative <= 0.5 || g.Relative > 0.51 {
		t.Fatalf("m7a %+v, want just over 0.5", g)
	}
}

// The reference is the group's newest generation, centred on its types:
// more runs of one of them don't move it, and an older type may measure
// faster than a newer one.
func TestMeasuredSpeedsReference(t *testing.T) {
	exactFit(t)
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	runs(st, now, "w", "m7a.2xlarge", 100, 100, 100, 100, 100)
	runs(st, now, "w", "r7a.2xlarge", 110, 110, 110, 110, 110, 110)
	runs(st, now, "w", "m6i.2xlarge", 80, 80, 80, 80, 80, 80, 80)
	mean := math.Sqrt(100 * 110)
	want := map[string]float64{"m7a.2xlarge": mean / 100, "r7a.2xlarge": mean / 110, "m6i.2xlarge": mean / 80}
	for round := 0; round < 2; round++ {
		got := measuredSpeeds(st.Speed, now.Add(time.Minute))
		for it, rel := range want {
			if g := got[it]; g.Generation != 7 || !approx(g.Relative, rel) {
				t.Errorf("round %d, %s: %+v, want %v", round, it, g, rel)
			}
		}
		runs(st, now, "w", "m7a.2xlarge", 100, 100, 100)
	}
}

// What is kept stays bounded: the latest speedSamples runs of a workload
// on a type, speedWorkloads workloads and speedCells pairs, the least
// recently run forgotten first, and nothing past speedFor.
func TestRecordSpeedBounds(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	for i := 0; i < speedSamples+5; i++ {
		recordSpeed(st, "w", "m7a.2xlarge", time.Duration(i+1)*time.Second, now.Add(time.Duration(i)*time.Second))
	}
	ss := st.Speed["w"]["m7a.2xlarge"]
	if len(ss) != speedSamples || ss[0].MS != 6000 || ss[len(ss)-1].MS != int64(speedSamples+5)*1000 {
		t.Fatalf("samples %+v", ss)
	}
	at := now.Add(time.Hour)
	for i := 0; i < speedWorkloads+3; i++ {
		if i == speedWorkloads {
			// The first by name, run again, and then once more by a clock
			// set back: its latest run counts, and the least recently run
			// go.
			recordSpeed(st, "w000", "m7a.2xlarge", time.Second, at.Add(time.Duration(i)*time.Second))
			recordSpeed(st, "w000", "m7a.2xlarge", time.Second, at.Add(-time.Minute))
		}
		recordSpeed(st, fmt.Sprintf("w%03d", i), "m7a.2xlarge", time.Second, at.Add(time.Duration(i)*time.Second))
	}
	if len(st.Speed) != speedWorkloads || st.Speed["w"] != nil || st.Speed["w000"] == nil || st.Speed["w001"] != nil || st.Speed["w003"] != nil || st.Speed["w004"] == nil {
		t.Fatalf("%d workloads; w %v, w000 %v, w001 %v, w004 %v", len(st.Speed), st.Speed["w"] != nil, st.Speed["w000"] != nil, st.Speed["w001"] != nil, st.Speed["w004"] != nil)
	}
	delete(st.Speed, "w000")
	at = at.Add(time.Hour)
	types := []string{"m6a", "m7a", "m8a", "r7a", "r8a"}
	for i := 0; i < speedWorkloads; i++ {
		for j, it := range types {
			recordSpeed(st, fmt.Sprintf("w%03d", i+3), it+".2xlarge", time.Second, at.Add(time.Duration(i*len(types)+j)*time.Second))
		}
	}
	cells := 0
	for _, w := range st.Speed {
		cells += len(w)
	}
	if cells != speedCells || len(st.Speed) > speedWorkloads {
		t.Fatalf("%d cells in %d workloads", cells, len(st.Speed))
	}
	// The newest pair stays; the oldest went.
	last := fmt.Sprintf("w%03d", speedWorkloads+2)
	if st.Speed[last]["r8a.2xlarge"] == nil || st.Speed["w003"] != nil {
		t.Fatalf("last %v, first %v", st.Speed[last], st.Speed["w003"])
	}
	forgetOld(st, at.Add(speedFor+time.Hour))
	if len(st.Speed) != 0 {
		t.Fatalf("expired: %d left", len(st.Speed))
	}
}

// Only vcpus runners keep run times: a fixed instance_types list isn't
// weighed for speed.
func TestRecordRunSpeedOnlyVCPUsRunners(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	listed := &config.Runner{Name: "listed", Type: config.TypeAWSSpot, InstanceTypes: []string{"m7a.2xlarge"}}
	if err := RecordRunSpeed(listed, "w", "m7a.2xlarge", time.Minute); err != nil {
		t.Fatal(err)
	}
	g := &AWSGlobal{r: listed, dir: RunnerStateDir(listed.Name)}
	_ = g.withState(func(st *placementState) {
		if st.Speed != nil {
			t.Fatalf("recorded for a listed runner: %+v", st.Speed)
		}
	})
	vc := &config.Runner{Name: "vc", Type: config.TypeAWSSpot, VCPUs: 8}
	for _, bad := range []struct {
		w, it string
		d     time.Duration
	}{{"", "m7a.2xlarge", time.Minute}, {"w", "", time.Minute}, {"w", "m7a.2xlarge", 0}} {
		if err := RecordRunSpeed(vc, bad.w, bad.it, bad.d); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordRunSpeed(vc, "w", "m7a.2xlarge", time.Minute); err != nil {
		t.Fatal(err)
	}
	g = &AWSGlobal{r: vc, dir: RunnerStateDir(vc.Name)}
	_ = g.withState(func(st *placementState) {
		if len(st.Speed) != 1 || len(st.Speed["w"]["m7a.2xlarge"]) != 1 || st.Speed["w"]["m7a.2xlarge"][0].MS != 60000 {
			t.Fatalf("speed %+v", st.Speed)
		}
	})
}

// Measured speeds replace the generation assumption in the ranking: an
// older type the runner's runs found as fast as the newer one, run after
// run, measures nearly so and wins on price, and the launch says its speed
// was measured; a type never measured keeps its speed by generation.
func TestRunCostMeasuredSpeedRanks(t *testing.T) {
	prices := map[string][][3]string{"r-one-1": {
		{"r-one-1a", "m7a.2xlarge", "0.0600"},
		{"r-one-1b", "m6i.2xlarge", "0.0500"},
		{"r-one-1c", "r7a.2xlarge", "0.0700"},
	}}
	f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()}, prices: prices}
	a := newSingleEnv(t, f, func(r *config.Runner) { reqRunner(r); r.LaunchOrder = config.LaunchRunCost })
	for i := 0; i < speedMinRuns; i++ {
		for _, it := range []string{"m7a.2xlarge", "m6i.2xlarge"} {
			if err := RecordRunSpeed(a.r, "typecheck", it, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	c := in.Placement.Cost
	if in.Type != "m6i.2xlarge" || c == nil || c.SpeedFrom != SpeedMeasured || c.RelSpeed < 0.99 || c.Speed > 0.01 {
		t.Fatalf("launched %s, cost %+v", in.Type, c)
	}

	g := a.ranker()
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	k := g.Rank(tbl)
	if s := k.Model.MeasuredSpeeds["m6i.2xlarge"]; s.Relative < 0.99 || s.Generation != 7 || s.Group != "m6i.2xlarge" || s.Runs != speedMinRuns {
		t.Fatalf("model %+v", k.Model.MeasuredSpeeds)
	}
	for _, p := range k.Prices {
		c, _ := k.CostOf(p)
		want, rel := SpeedMeasured, 1.0
		switch p.Type {
		case "r7a.2xlarge":
			want = SpeedByGeneration
		case "m6i.2xlarge":
			rel = k.Model.MeasuredSpeeds["m6i.2xlarge"].Relative
		}
		if c.SpeedFrom != want || c.RelSpeed != rel {
			t.Errorf("%s: %+v", p.Type, c)
		}
	}

	// Measured slower than the generation says (0.69x), m6i now loses to
	// m7a, and says why.
	for i := 0; i < speedMinRuns; i++ {
		if err := RecordRunSpeed(a.r, "tests", "m7a.2xlarge", 69*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := RecordRunSpeed(a.r, "tests", "m6i.2xlarge", 100*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	k = g.Rank(tbl)
	if k.Prices[0].Type != "m7a.2xlarge" {
		t.Fatalf("order %v", k.Prices)
	}
	m6i := k.Cost[poolKey(tbl.Prices[0])]
	for _, p := range tbl.Prices {
		if p.Type == "m6i.2xlarge" {
			m6i = k.Cost[poolKey(p)]
		}
	}
	if m6i.SpeedFrom != SpeedMeasured || !strings.Contains(m6i.Factors(), "slower type (83% speed, measured)") {
		t.Fatalf("m6i %+v: %s", m6i, m6i.Factors())
	}
}

// A speed is marked measured only against a fastest option measured with
// it: against a newer type never measured, the ratio is partly assumed.
// On a tie for fastest, the measured type is the one compared against.
func TestRunCostsMeasuredLabel(t *testing.T) {
	m := RunCostModel{WaitCost: 1, HorizonMinutes: 60, GenSpeedup: genSpeedup, RoundTrips: runRoundTrips, SpeedByGen: true,
		MeasuredSpeeds: map[string]TypeSpeed{
			"m6a.2xlarge": {Relative: 0.69, Generation: 7, Group: "m6a.2xlarge"},
			"m7a.2xlarge": {Relative: 1, Generation: 7, Group: "m6a.2xlarge"},
		}}
	opts := func(types ...string) []SpotPrice {
		var ps []SpotPrice
		for _, it := range types {
			ps = append(ps, SpotPrice{Region: "r", Zone: "a", Type: it, Price: "0.10"})
		}
		return ps
	}
	c := runCosts(m, opts("m8a.2xlarge", "m7a.2xlarge", "m6a.2xlarge"), placementEvidence{now: time.Now()})
	for it, want := range map[string]RunCost{
		"m8a.2xlarge": {RelSpeed: 1, SpeedFrom: SpeedByGeneration},
		"m7a.2xlarge": {RelSpeed: 1 / 1.1, SpeedFrom: SpeedByGeneration},
		"m6a.2xlarge": {RelSpeed: 0.69 / 1.1, SpeedFrom: SpeedByGeneration},
	} {
		if got := c["r/a/"+it]; !approx(got.RelSpeed, want.RelSpeed) || got.SpeedFrom != want.SpeedFrom {
			t.Errorf("against m8a, %s: %+v", it, got)
		}
	}
	c = runCosts(m, opts("c7a.2xlarge", "m6a.2xlarge", "m7a.2xlarge"), placementEvidence{now: time.Now()})
	for it, want := range map[string]RunCost{
		"c7a.2xlarge": {RelSpeed: 1, SpeedFrom: SpeedByGeneration},
		"m7a.2xlarge": {RelSpeed: 1, SpeedFrom: SpeedMeasured},
		"m6a.2xlarge": {RelSpeed: 0.69, SpeedFrom: SpeedMeasured},
	} {
		if got := c["r/a/"+it]; !approx(got.RelSpeed, want.RelSpeed) || got.SpeedFrom != want.SpeedFrom {
			t.Errorf("tie, %s: %+v", it, got)
		}
	}
}

// The issue's typecheck (#296): r7a's nine kept runs were bimodal (~45 s on
// a quiet VM, 147-183 s beside five other jobs), m7a's busier throughout,
// and their medians said m7a ran at 49% of r7a's speed, the same
// generation and core count. Runs that scattered barely move either type
// from its generation's speed, and the extra time charged to m7a stays
// within a few times its Spot price, not 14 times.
func TestMeasuredSpeedsScatteredRunsStayNearGeneration(t *testing.T) {
	now := time.Date(2026, 11, 17, 22, 45, 0, 0, time.UTC)
	st := &placementState{}
	runs(st, now, "typecheck", "m7a.2xlarge", 63, 103, 108, 110, 121, 125, 131, 144, 161)
	runs(st, now, "typecheck", "r7a.2xlarge", 44, 44, 47, 49, 59, 65, 147, 155, 183)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	ratio := got["m7a.2xlarge"].Relative / got["r7a.2xlarge"].Relative
	if ratio < 0.8 || ratio > 1 {
		t.Fatalf("m7a at %.3f of r7a: %+v", ratio, got)
	}
	m := RunCostModel{WaitCost: 1, HorizonMinutes: 60, GenSpeedup: genSpeedup, RoundTrips: runRoundTrips, SpeedByGen: true, MeasuredSpeeds: got}
	ps := []SpotPrice{
		{Region: "eu-north-1", Zone: "eu-north-1b", Type: "r7a.2xlarge", Price: "0.0792"},
		{Region: "eu-north-1", Zone: "eu-north-1c", Type: "m7a.2xlarge", Price: "0.0787"},
	}
	c := runCosts(m, ps, placementEvidence{now: now})
	slow := c[poolKey(ps[1])]
	if slow.SpeedFrom != SpeedMeasured || slow.Speed > 3*slow.Compute {
		t.Fatalf("m7a charged %+v", slow)
	}
	t.Logf("m7a at %.3f of r7a, +$%.3f/h", ratio, slow.Speed)
}

// Two types that run a workload equally fast across many runs measure
// about the same, whichever of them happened to be busy when: the same
// mix of quiet and busy runs, in different orders.
func TestMeasuredSpeedsSameMixMeasuresTheSame(t *testing.T) {
	now := time.Date(2026, 11, 17, 22, 45, 0, 0, time.UTC)
	mix := []float64{44, 47, 49, 59, 65, 103, 108, 110, 121, 125, 131, 144, 147, 155, 161, 183}
	var a, b []float64
	for i := 0; i < speedSamples; i++ {
		a = append(a, mix[i%len(mix)])
		b = append(b, mix[(i*7+3)%len(mix)])
	}
	st := &placementState{}
	runs(st, now, "typecheck", "m7a.2xlarge", a...)
	runs(st, now, "typecheck", "r7a.2xlarge", b...)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	if ratio := got["m7a.2xlarge"].Relative / got["r7a.2xlarge"].Relative; ratio < 0.95 || ratio > 1/0.95 {
		t.Fatalf("m7a at %.3f of r7a: %+v", ratio, got)
	}
}

// Consistent runs still replace the generation assumption: m7a at 0.69x
// r8a's speed (#279), run after run, measures close to that, where the
// assumption says 0.91x; the same five runs scattered as widely as the
// issue's leave it nearer the assumption.
func TestMeasuredSpeedsConsistentRunsOverrideGeneration(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	st := &placementState{}
	r8a := []float64{72, 70, 74, 71, 73, 72, 70, 74, 71, 73}
	m7a := []float64{104, 101, 105, 103, 102, 104, 101, 105, 103, 102}
	runs(st, now, "typecheck", "r8a.2xlarge", r8a...)
	runs(st, now, "typecheck", "m7a.2xlarge", m7a...)
	got := measuredSpeeds(st.Speed, now.Add(time.Minute))
	if g := got["m7a.2xlarge"]; g.Relative < 0.69 || g.Relative > 0.72 {
		t.Fatalf("consistent m7a %+v", g)
	}
	st = &placementState{}
	runs(st, now, "typecheck", "r8a.2xlarge", 45, 72, 150, 60, 180)
	runs(st, now, "typecheck", "m7a.2xlarge", 63, 103, 110, 161, 144)
	got = measuredSpeeds(st.Speed, now.Add(time.Minute))
	if g := got["m7a.2xlarge"]; g.Relative < 0.85 || g.Relative > 1 {
		t.Fatalf("scattered m7a %+v", g)
	}
}
