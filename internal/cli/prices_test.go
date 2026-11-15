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
		optionKey(warmOpt): {Score: 0.1, Compute: 0.1, RelSpeed: 1, Warm: true, WarmSeen: seen, RTTMS: 150},
		optionKey(coldOpt): {Score: 0.1691, Compute: 0.096, Cold: 0.0731, RelSpeed: 1, RTTMS: 150, ColdParts: &provider.ColdParts{
			Boot:  provider.ColdPart{Cost: 0.0183, Why: "no bake of the runner"},
			Deps:  provider.ColdPart{Cost: 0.0365, Why: "no dependency archive for the project"},
			Image: provider.ColdPart{Cost: 0.0183, Why: "no image of the project"}}},
		optionKey(slowOpt): {Score: 0.2862, Compute: 0.03, Speed: 0.103, Cold: 0.0687, Distance: 0.0845, RelSpeed: 0.9091, RTTMS: 440, ColdParts: &provider.ColdParts{
			Boot:  provider.ColdPart{Warm: true, Why: "a bake of the runner"},
			Deps:  provider.ColdPart{Cost: 0.0687, Why: "no dependency archive for the project"},
			Image: provider.ColdPart{Warm: true, Why: "the project's image"}}},
	}
	model = &provider.RunCostModel{WaitCost: 1, HorizonMinutes: 60, ColdMinutes: 4, BootMinutes: 1, DepsMinutes: 2, ImageMinutes: 1, WarmDays: 7, WarmAfterMin: 10, GenSpeedup: 1.1, RoundTrips: 300, SpeedByGen: true, Regions: true}
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
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "REGION ZONE TYPE USD/HOUR RUN COST SPEED COLD DISTANCE" {
		t.Fatalf("header: %q", lines[1])
	}
	for i, want := range []string{
		"→ us-east-1 us-east-1a m7a.2xlarge 0.100 0.100 - warm - next launch",
		"eu-north-1 eu-north-1b m7a.2xlarge 0.096 0.169 - +0.073 boot,deps,image -",
		"ap-south-2 ap-south-2a m6a.2xlarge 0.030 0.286 +0.103 +0.069 deps +0.085",
	} {
		if got := strings.Join(strings.Fields(lines[2+i]), " "); got != want {
			t.Errorf("row %d: %q, want %q", i+1, got, want)
		}
	}
	for _, want := range []string{`launch_order = "run_cost"`, "with waiting valued at $1/h (wait_cost)", "a slower type (each instance generation counts as 10% faster per vCPU)",
		"a cold region (COLD, in minutes: boot 1 without a bake of aws there, deps 2 without the project's archive in the region's cache, image 1 without the project's image in its registry; outside a project, deps and image count as warm where a VM of aws was up 10 minutes or more in the last 7 days)", `"cheapest" ranks by price alone`} {
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
	// --limit cuts the list after that many rows.
	b.Reset()
	runCostView().writeText(&b, 1)
	if out := b.String(); row(strings.Split(out, "\n"), "eu-north-1b") != "" || !strings.Contains(out, "… 2 more (--limit 0 for all)") {
		t.Fatalf("limited:\n%s", out)
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
	if got.Order != "run_cost" || !reflect.DeepEqual(got.Model, model) || len(got.Prices) != 3 {
		t.Fatalf("json: %s", b.String())
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
