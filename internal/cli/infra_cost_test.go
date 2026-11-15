package cli

import (
	"context"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// A VM's Spot price wins over hourly_cost, which only covers VMs with no
// known price.
func TestInstanceCost(t *testing.T) {
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot, HourlyCost: 0.12}
	spot := map[string]float64{"i-1": 0.331}
	if c, from := instanceCost(r, spot, "i-1"); c != 0.331 || from != costSpot {
		t.Fatalf("priced VM: %v %q", c, from)
	}
	if c, from := instanceCost(r, spot, "i-2"); c != 0.12 || from != costSetting {
		t.Fatalf("unpriced VM: %v %q", c, from)
	}
	r.HourlyCost = 0
	if c, from := instanceCost(r, spot, "i-2"); c != 0 || from != "" {
		t.Fatalf("no price at all: %v %q", c, from)
	}
}

// Only aws-spot runners are priced from AWS; other types never ask.
func TestFleetPricerSkipsOtherTypes(t *testing.T) {
	fp := newFleetPricer()
	for _, typ := range []string{config.TypeSim, config.TypeSSH} {
		r := &config.Runner{Name: typ, Type: typ}
		if got := fp.prices(context.Background(), r, []provider.Instance{{ID: "x", Spot: true, Region: "r", Zone: "z", Type: "t"}}); got != nil {
			t.Fatalf("%s runner priced: %v", typ, got)
		}
	}
	if len(fp.byRunner) != 0 {
		t.Fatal("made a pricer for a runner that isn't aws-spot")
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
