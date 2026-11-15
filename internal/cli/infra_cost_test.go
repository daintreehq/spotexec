package cli

import (
	"context"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// fakePricer prices from a fixed table, or blocks until its deadline.
type fakePricer struct {
	usd   map[string]float64
	block bool
}

func (f *fakePricer) Prices(ctx context.Context, insts []provider.Instance) map[string]float64 {
	if f.block {
		<-ctx.Done()
		return nil
	}
	return f.usd
}

// A VM's Spot price replaces hourly_cost, which stays only for VMs with no
// known price; every runner is priced at once under one deadline.
func TestPriceFleets(t *testing.T) {
	old := pricesTimeout
	pricesTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pricesTimeout = old })
	aws := &config.Runner{Name: "aws", Type: config.TypeAWSSpot, HourlyCost: 0.12}
	slow1 := &config.Runner{Name: "slow1", Type: config.TypeAWSSpot, HourlyCost: 0.5}
	slow2 := &config.Runner{Name: "slow2", Type: config.TypeAWSSpot}
	pricers := map[string]instancePricer{
		"aws":   &fakePricer{usd: map[string]float64{"i-1": 0.331}},
		"slow1": &fakePricer{block: true},
		"slow2": &fakePricer{block: true},
	}
	fp := newFleetPricer()
	fp.newPricer = func(r *config.Runner) (instancePricer, error) { return pricers[r.Name], nil }
	row := func(r *config.Runner, id string) runnerStatus {
		c, from := fallbackCost(r)
		return runnerStatus{r: r, Fleet: r.Name, Cost: c, CostFrom: from, Info: provider.Info{InstanceID: id}}
	}
	rows := []runnerStatus{row(aws, "i-1"), row(aws, "i-2"), row(slow1, "i-3"), row(slow2, "i-4")}
	fleets := []fleetInstances{
		{aws, []provider.Instance{{ID: "i-1"}, {ID: "i-2"}}},
		{slow1, []provider.Instance{{ID: "i-3"}}},
		{slow2, []provider.Instance{{ID: "i-4"}}},
	}
	start := time.Now()
	fp.priceFleets(context.Background(), rows, fleets)
	if d := time.Since(start); d > 3*pricesTimeout/2 {
		t.Fatalf("pricing took %s: the runners' deadlines added up", d)
	}
	want := []struct {
		cost float64
		from string
	}{{0.331, costSpot}, {0.12, costSetting}, {0.5, costSetting}, {0, ""}}
	for i, w := range want {
		if rows[i].Cost != w.cost || rows[i].CostFrom != w.from {
			t.Errorf("row %d: %v %q, want %v %q", i, rows[i].Cost, rows[i].CostFrom, w.cost, w.from)
		}
	}
	if len(fp.byRunner) != 3 {
		t.Fatalf("%d pricers kept, want one per runner", len(fp.byRunner))
	}
}

// Only aws-spot runners are priced from AWS; other types never make a
// pricer.
func TestPriceFleetsSkipsOtherTypes(t *testing.T) {
	fp := newFleetPricer()
	made := 0
	fp.newPricer = func(r *config.Runner) (instancePricer, error) {
		made++
		return &fakePricer{usd: map[string]float64{"x": 1}}, nil
	}
	for _, typ := range []string{config.TypeSim, config.TypeSSH} {
		r := &config.Runner{Name: typ, Type: typ}
		rows := []runnerStatus{{r: r, Fleet: typ, Info: provider.Info{InstanceID: "x"}}}
		fp.priceFleets(context.Background(), rows, []fleetInstances{{r, []provider.Instance{{ID: "x"}}}})
		if rows[0].CostFrom != "" {
			t.Fatalf("%s runner priced: %+v", typ, rows[0])
		}
	}
	if made != 0 {
		t.Fatalf("made %d pricer(s) for runners that aren't aws-spot", made)
	}
}

func TestBurnLine(t *testing.T) {
	aws := &config.Runner{Name: "aws", Type: config.TypeAWSSpot}
	ssh := &config.Runner{Name: "box", Type: config.TypeSSH, HourlyCost: 1}
	row := func(r *config.Runner, st provider.State, cost float64, from string) runnerStatus {
		return runnerStatus{r: r, Cost: cost, CostFrom: from, Info: provider.Info{State: st}}
	}
	for _, tc := range []struct {
		name string
		rows []runnerStatus
		want string
	}{
		{"spot only", []runnerStatus{row(aws, provider.StateRunning, 0.129, costSpot), row(aws, provider.StateRunning, 0.331, costSpot)},
			"estimated burn: $0.460/hour (from Spot prices)"},
		{"mixed sources", []runnerStatus{row(aws, provider.StateRunning, 0.129, costSpot), row(aws, provider.StateRunning, 0.12, costSetting)},
			"estimated burn: $0.249/hour (from Spot prices and hourly_cost)"},
		{"setting only", []runnerStatus{row(aws, provider.StateRunning, 0.12, costSetting)},
			"estimated burn: $0.120/hour (from hourly_cost)"},
		{"unpriced VM", []runnerStatus{row(aws, provider.StateRunning, 0.129, costSpot), row(aws, provider.StateRunning, 0, "")},
			"estimated burn: $0.129/hour (from Spot prices), not counting 1 running VM(s) with no known price"},
		{"pending and static runners left out", []runnerStatus{row(aws, provider.StatePending, 0.2, costSpot), row(ssh, provider.StateRunning, 1, costSetting), row(aws, provider.StateRunning, 0.1, costSpot)},
			"estimated burn: $0.100/hour (from Spot prices)"},
		{"nothing priced", []runnerStatus{row(aws, provider.StateRunning, 0, "")}, ""},
		{"nothing running", nil, ""},
	} {
		if got := burnLine(tc.rows); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
