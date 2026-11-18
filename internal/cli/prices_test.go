package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

var (
	warmOpt = provider.SpotPrice{Region: "us-east-1", Zone: "us-east-1a", Type: "m7a.2xlarge", Price: "0.100"}
	coldOpt = provider.SpotPrice{Region: "eu-north-1", Zone: "eu-north-1b", Type: "m7a.2xlarge", Price: "0.096"}
	slowOpt = provider.SpotPrice{Region: "ap-south-2", Zone: "ap-south-2a", Type: "m6a.2xlarge", Price: "0.030"}
	seen    = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	costs   = map[string]provider.RunCost{
		optionKey(warmOpt): {Score: 0.1, Compute: 0.1, RelSpeed: 1, SpeedFrom: provider.SpeedByGeneration, Warm: true, WarmSeen: seen, RTTMS: 150},
		optionKey(coldOpt): {Score: 0.1691, Compute: 0.096, Cold: 0.0731, RelSpeed: 1, SpeedFrom: provider.SpeedByGeneration, RTTMS: 150, ColdParts: &provider.ColdParts{
			Boot:  provider.ColdPart{Cost: 0.0183, Why: "no bake of the runner"},
			Deps:  provider.ColdPart{Cost: 0.0365, Why: "no dependency archive for the project"},
			Image: provider.ColdPart{Cost: 0.0183, Why: "no image of the project"}}},
		optionKey(slowOpt): {Score: 0.2518, Compute: 0.03, Speed: 0.103, Cold: 0.0343, Distance: 0.0845, RelSpeed: 0.9091, SpeedFrom: provider.SpeedByGeneration, RTTMS: 440, ColdParts: &provider.ColdParts{
			Boot:  provider.ColdPart{Warm: true, Why: "a bake of the runner"},
			Deps:  provider.ColdPart{Cost: 0.0343, Why: "no dependency archive for the project"},
			Image: provider.ColdPart{Warm: true, Why: "the project's image"}}},
	}
	model = &provider.RunCostModel{WaitCost: 1, HorizonMinutes: 60, ColdMinutes: 4, BootMinutes: 1, DepsMinutes: 2, ImageMinutes: 1, WarmDays: 7, WarmAfterMin: 10, GenSpeedup: 1.1, RoundTrips: 300, SpeedByGen: true, SpeedCap: 3, Regions: true,
		FailureMinutes: 3, FailureHalfLifeHours: 2,
		DepsFrom: provider.ColdEstimate{Source: "default"}, ImageFrom: provider.ColdEstimate{Source: "default"}}
)

func runCostView() pricesView {
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot, Regions: []string{"all"}, Region: "us-east-1",
		LaunchOrder: config.LaunchRunCost}
	k := provider.Ranking{Order: config.LaunchRunCost, Prices: []provider.SpotPrice{warmOpt, coldOpt, slowOpt}, Cost: costs, Model: model}
	t := provider.PriceTable{Prices: []provider.SpotPrice{slowOpt, coldOpt, warmOpt}, At: time.Now(), Regions: []string{"ap-south-2", "eu-north-1", "us-east-1"}}
	return pricesView{r: r, t: t, k: k, next: warmOpt, hasNext: true}
}

func row(lines []string, zone string) string {
	for _, l := range lines {
		if strings.Contains(l, zone) {
			return l
		}
	}
	return ""
}

// `spotexec prices` lists the options in launch order with what the
// ranking added to each, so the choice can be checked by eye.
func TestPricesShowRunCostBreakdown(t *testing.T) {
	var b bytes.Buffer
	runCostView().writeText(&b, 20)
	out := b.String()
	t.Log("\n" + out)
	lines := strings.Split(out, "\n")
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "REGION ZONE TYPE USD/HOUR RUN COST SPEED COLD DISTANCE FAILURES" {
		t.Fatalf("header: %q", lines[1])
	}
	for i, want := range []string{
		"→ us-east-1 us-east-1a m7a.2xlarge 0.100 0.100 - 100% assumed warm - - next launch",
		"eu-north-1 eu-north-1b m7a.2xlarge 0.096 0.169 - 100% assumed +0.073 boot,deps,image - -",
		"ap-south-2 ap-south-2a m6a.2xlarge 0.030 0.252 +0.103 91% assumed +0.034 deps +0.085 -",
	} {
		if got := strings.Join(strings.Fields(lines[2+i]), " "); got != want {
			t.Errorf("row %d: %q, want %q", i+1, got, want)
		}
	}
	for _, want := range []string{`launch_order = "run_cost"`, "with waiting valued at $1/h (wait_cost)", "a slower type (SPEED, assumed: each instance generation counts as 10% faster per vCPU; measured instead once the same command has run 5 times on each of two types)",
		"A zone and type that failed lately (refused for capacity, or a VM reclaimed within an hour of launch) adds 3 minutes of that for each failure (FAILURES), counting half as much every 2 hours.",
		"a cold region (COLD, in minutes: boot 1 without a bake of aws there, deps 2 (a default) without the project's archive in the region's cache, image 1 (a default) without the project's image in its registry; outside a project, deps and image count as warm where a VM of aws was up 10 minutes or more in the last 7 days)", `"cheapest" ranks by price alone`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// In a project, its caches were checked.
	b.Reset()
	v := runCostView()
	v.project = "app"
	v.writeText(&b, 20)
	if !strings.Contains(b.String(), "; deps and image checked for app)") {
		t.Errorf("project:\n%s", b.String())
	}
	// Its measured installs price the deps part, and the note says so.
	b.Reset()
	v = runCostView()
	v.project = "app"
	m := *model
	m.DepsMinutes, m.DepsFrom = 14.25, provider.ColdEstimate{Source: "measured", At: seen}
	m.ImageMinutes, m.ImageFrom = 60, provider.ColdEstimate{Source: "measured", At: seen, Capped: true}
	v.k.Model = &m
	v.writeText(&b, 20)
	day := seen.Local().Format("Jan 2")
	for _, want := range []string{"deps 14.3 (the project's longest install on aws lately, " + day + ") without the project's archive",
		"image 60 (capped: the project's longest image build on aws lately, " + day + ", took longer) without",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
	b.Reset()
	v.k.Model = model
	v.writeText(&b, 20)
	if !strings.Contains(b.String(), "deps 2 (a default: no install of the project on aws measured lately)") {
		t.Errorf("project default:\n%s", b.String())
	}
	// --limit cuts the list after that many rows.
	b.Reset()
	runCostView().writeText(&b, 1)
	if out := b.String(); row(strings.Split(out, "\n"), "eu-north-1b") != "" || !strings.Contains(out, "… 2 more (--limit 0 for all)") {
		t.Fatalf("limited:\n%s", out)
	}
}

// A zone and type that failed lately shows what that added (FAILURES) and
// why, beside any cooldown.
func TestPricesShowFailureHistory(t *testing.T) {
	last := time.Date(2026, 11, 15, 14, 5, 0, 0, time.Local)
	v := runCostView()
	v.k.Cost = map[string]provider.RunCost{}
	for k, c := range costs {
		v.k.Cost[k] = c
	}
	failed := v.k.Cost[optionKey(coldOpt)]
	failed.Failures, failed.Score = 0.1074, failed.Score+0.1074
	failed.FailedHere = &provider.FailureHistory{Capacity: 1.83, Reclaims: 0.5, LastCapacity: last, LastReclaim: last}
	v.k.Cost[optionKey(coldOpt)] = failed
	v.cool = map[string]string{optionKey(coldOpt): "InsufficientInstanceCapacity"}
	var b bytes.Buffer
	v.writeText(&b, 20)
	want := "eu-north-1 eu-north-1b m7a.2xlarge 0.096 0.276 - 100% assumed +0.073 boot,deps,image - +0.107 skipped for now: InsufficientInstanceCapacity; " +
		"failed here lately: 1.8 capacity refusals, last 14:05; 0.5 VMs reclaimed within 1 hour of launch, last 14:05"
	if got := strings.Join(strings.Fields(row(strings.Split(b.String(), "\n"), "eu-north-1b")), " "); got != want {
		t.Fatalf("row:\n%s\nwant:\n%s", got, want)
	}
	// Too little to show in USD (wait_cost 0, a cheap pool): still said.
	failed.Failures, failed.FailedHere = 0.0004, &provider.FailureHistory{Capacity: 1}
	v.k.Cost[optionKey(coldOpt)] = failed
	b.Reset()
	v.writeText(&b, 20)
	if got := row(strings.Split(b.String(), "\n"), "eu-north-1b"); !strings.Contains(got, "failed here lately: 1.0 capacity refusals") {
		t.Fatalf("small penalty row: %s", got)
	}
	failed.Failures, failed.FailedHere = 0.1074, &provider.FailureHistory{Capacity: 1.83, Reclaims: 0.5, LastCapacity: last, LastReclaim: last}
	v.k.Cost[optionKey(coldOpt)] = failed
	b.Reset()
	if err := v.writeJSON(&b); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"failures": 0.1074`, `"capacity_refusals": 1.83`, `"early_reclaims": 0.5`, `"last_capacity_refusal": "`, `"failure_minutes": 3`, `"failure_half_life_hours": 2`} {
		if !strings.Contains(b.String(), key) {
			t.Errorf("json missing %s:\n%s", key, b.String())
		}
	}
}

// --json carries the same breakdown, in launch order, with the model's
// assumptions.
func TestPricesJSONRunCost(t *testing.T) {
	var b bytes.Buffer
	if err := runCostView().writeJSON(&b); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Order  string                 `json:"launch_order"`
		Model  *provider.RunCostModel `json:"run_cost_model"`
		Prices []struct {
			provider.SpotPrice
			Next    bool
			RunCost *provider.RunCost `json:"run_cost"`
		}
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The parts' keys, as scripts read them.
	if out := b.String(); !strings.Contains(out, `"cold_parts": {`) || !strings.Contains(out, `"dependencies": {`) || !strings.Contains(out, `"cold_deps_minutes": 2`) ||
		!strings.Contains(out, `"cold_deps_estimate": {`) || !strings.Contains(out, `"source": "default"`) {
		t.Fatalf("json keys: %s", out)
	}
	if got.Order != "run_cost" || !reflect.DeepEqual(got.Model, model) || len(got.Prices) != 3 {
		t.Fatalf("json: %s", b.String())
	}
	// Measured parts say when the longest was taken and whether capped, in
	// the keys scripts read.
	v := runCostView()
	m := *model
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	m.DepsMinutes, m.DepsFrom = 14.25, provider.ColdEstimate{Source: "measured", At: at}
	m.ImageMinutes, m.ImageFrom = 60, provider.ColdEstimate{Source: "measured", At: at, Capped: true}
	v.k.Model = &m
	b.Reset()
	if err := v.writeJSON(&b); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Model map[string]any `json:"run_cost_model"`
	}
	if err := json.Unmarshal(b.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	wantDeps := map[string]any{"source": "measured", "at": "2026-11-02T09:30:00Z"}
	wantImage := map[string]any{"source": "measured", "at": "2026-11-02T09:30:00Z", "capped": true}
	if !reflect.DeepEqual(raw.Model["cold_deps_estimate"], wantDeps) || !reflect.DeepEqual(raw.Model["cold_image_estimate"], wantImage) ||
		raw.Model["cold_deps_minutes"] != 14.25 || raw.Model["cold_image_minutes"] != float64(60) {
		t.Fatalf("measured json: %v", raw.Model)
	}
	for i, want := range []provider.SpotPrice{warmOpt, coldOpt, slowOpt} {
		p := got.Prices[i]
		if p.Zone != want.Zone || p.Price != want.Price || p.Next != (i == 0) || p.RunCost == nil || !reflect.DeepEqual(*p.RunCost, costs[optionKey(want)]) {
			t.Errorf("row %d: %+v %+v, want %+v %+v", i, p.SpotPrice, p.RunCost, want, costs[optionKey(want)])
		}
	}
}

// Ranked by price alone (launch_order = "cheapest", or one region with a
// fixed list even under run_cost), the table has no run-cost columns.
func TestPricesCheapestHasNoBreakdown(t *testing.T) {
	v := runCostView()
	v.r.Regions, v.r.Region = nil, "us-east-1"
	v.k = provider.Ranking{Order: config.LaunchCheapest, Prices: v.t.Prices}
	v.next = v.t.Prices[0]
	var b bytes.Buffer
	v.writeText(&b, 20)
	out := b.String()
	if strings.Contains(out, "RUN COST") || strings.Contains(out, "run_cost") || !strings.Contains(strings.Split(out, "\n")[2], "ap-south-2a") {
		t.Fatalf("output:\n%s", out)
	}
	b.Reset()
	if err := v.writeJSON(&b); err != nil || strings.Contains(b.String(), "run_cost\"") || strings.Contains(b.String(), "run_cost_model") ||
		!strings.Contains(b.String(), `"launch_order": "cheapest"`) {
		t.Fatalf("json: %s (%v)", b.String(), err)
	}
}

// Speeds the runner's runs measured are marked as such, row by row, and
// the note names them, with the generation assumption for the rest.
func TestPricesShowMeasuredSpeed(t *testing.T) {
	v := runCostView()
	v.k.Cost = map[string]provider.RunCost{}
	for k, c := range costs {
		v.k.Cost[k] = c
	}
	slow := v.k.Cost[optionKey(slowOpt)]
	slow.RelSpeed, slow.SpeedFrom = 0.69, provider.SpeedMeasured
	v.k.Cost[optionKey(slowOpt)] = slow
	m := *model
	m.MeasuredSpeeds = map[string]provider.TypeSpeed{
		"m6a.2xlarge": {Relative: 0.69, Generation: 7, Group: "m6a.2xlarge", Runs: 27, Workload: 1},
		"m7a.2xlarge": {Relative: 1, Generation: 7, Group: "m6a.2xlarge", Runs: 66, Workload: 1},
		"c6i.2xlarge": {Relative: 0.8, Generation: 7, Group: "c6i.2xlarge", Runs: 3, Workload: 1},
		"c7i.2xlarge": {Relative: 1, Generation: 7, Group: "c6i.2xlarge", Runs: 3, Workload: 1},
	}
	v.k.Model = &m
	var b bytes.Buffer
	v.writeText(&b, 20)
	out := b.String()
	if got := strings.Join(strings.Fields(row(strings.Split(out, "\n"), "ap-south-2a")), " "); !strings.Contains(got, "+0.103 69% measured +0.034") {
		t.Errorf("row: %q", got)
	}
	slow.SpeedCapped = true
	v.k.Cost[optionKey(slowOpt)] = slow
	var capped bytes.Buffer
	v.writeText(&capped, 20)
	if got := strings.Join(strings.Fields(row(strings.Split(capped.String(), "\n"), "ap-south-2a")), " "); !strings.Contains(got, "+0.103 69% measured,capped +0.034") {
		t.Errorf("capped row: %q", got)
	}
	slow.SpeedCapped = false
	v.k.Cost[optionKey(slowOpt)] = slow
	want := "a slower type (SPEED, measured from the same commands' successful runs on each type lately, drawn toward the generation assumption as far as few VMs or scattered runs leave them unsure, and moving a type's charge at most 3 hours of its Spot price from that assumption: c6i.2xlarge 80%, c7i.2xlarge 100% of generation 7; m6a.2xlarge 69%, m7a.2xlarge 100% of generation 7; assumed for other types: each instance generation counts as 10% faster per vCPU)"
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
	if c := v.k.Cost[optionKey(slowOpt)]; !strings.Contains(c.Factors(), "+$0.103 slower type (69% speed, measured)") {
		t.Errorf("factors: %s", c.Factors())
	}
	// In JSON, under the keys scripts read.
	b.Reset()
	if err := v.writeJSON(&b); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model struct {
			Measured map[string]map[string]any `json:"measured_speeds"`
		} `json:"run_cost_model"`
		Prices []struct {
			Type    string         `json:"type"`
			RunCost map[string]any `json:"run_cost"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if s := got.Model.Measured["m6a.2xlarge"]; s["relative"] != 0.69 || s["generation"] != 7.0 || s["group"] != "m6a.2xlarge" || s["runs"] != 27.0 || s["workloads"] != 1.0 {
		t.Errorf("measured_speeds: %v", got.Model.Measured)
	}
	from := map[string]any{}
	for _, p := range got.Prices {
		if p.Type == "m6a.2xlarge" && p.RunCost["relative_speed"] != 0.69 {
			t.Errorf("relative_speed: %v", p.RunCost)
		}
		from[p.Type] = p.RunCost["speed_from"]
	}
	if from["m6a.2xlarge"] != "measured" || from["m7a.2xlarge"] != "generation" {
		t.Errorf("speed_from: %v", from)
	}
}
