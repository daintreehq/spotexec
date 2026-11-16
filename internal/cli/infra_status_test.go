package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// A fleet VM that no longer matches the runner's config is marked in
// `spotexec status`, with why.
func TestStatusMarksMismatchedVM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	printStatus(f, []runnerStatus{
		{Name: "spot/i-old", Type: "aws-spot", Fleet: "spot", Mismatch: "c6a.2xlarge doesn't have 8 vCPUs, at least 32 GiB",
			Info: provider.Info{State: provider.StateRunning, InstanceID: "i-old", InstanceType: "c6a.2xlarge"}},
		{Name: "spot/i-new", Type: "aws-spot", Fleet: "spot",
			Info: provider.Info{State: provider.StateRunning, InstanceID: "i-new", InstanceType: "r6a.2xlarge"}},
	}, nil)
	f.Close()
	b, _ := os.ReadFile(path)
	out := string(b)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[1], "outdated") || strings.Contains(lines[2], "outdated") {
		t.Fatalf("state column:\n%s", out)
	}
	if !strings.Contains(out, "spot/i-old: doesn't match the current config (c6a.2xlarge doesn't have 8 vCPUs, at least 32 GiB)") {
		t.Fatalf("no reason:\n%s", out)
	}
}

type matcherFleet struct {
	provider.Fleet
	got map[string]string
}

func (m matcherFleet) Mismatches(context.Context, []provider.Instance) map[string]string {
	return m.got
}

// Status asks a fleet that can judge its VMs, and not one that can't.
func TestFleetMismatches(t *testing.T) {
	insts := []provider.Instance{{ID: "i-old"}}
	want := map[string]string{"i-old": "c6a.2xlarge is not in instance_types"}
	if got := fleetMismatches(context.Background(), matcherFleet{got: want}, insts); got["i-old"] != want["i-old"] {
		t.Fatalf("%q", got)
	}
	var plain provider.Fleet = matcherFleet{}.Fleet
	if got := fleetMismatches(context.Background(), plain, insts); got != nil {
		t.Fatalf("%q", got)
	}
}

// The SLOTS column shows the slot count the VM's running jobs agreed on,
// not the viewer's max_slots; an idle VM, or one whose helper doesn't
// report it, falls back to the viewer's setting.
func TestStatusSlotsShowVMCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	running := provider.Info{State: provider.StateRunning}
	three := []protocol.Holder{{Token: 1}, {Token: 2}, {Token: 3}}
	printStatus(f, []runnerStatus{
		{Name: "busy", r: &config.Runner{MaxSlots: 2}, Info: running, Helper: &helper.Status{CPUs: 8, Holders: three, SlotCount: 4}},
		{Name: "idle", r: &config.Runner{MaxSlots: 2}, Info: running, Helper: &helper.Status{CPUs: 8, Holders: []protocol.Holder{}}},
		{Name: "older", r: &config.Runner{}, Info: running, Helper: &helper.Status{CPUs: 8, Holders: three[:1]}},
	}, nil)
	f.Close()
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	for i, want := range []string{"3/4", "0/2", "1/2"} {
		if !strings.Contains(lines[i+1], " "+want+" ") {
			t.Fatalf("row %d: want slots %s:\n%s", i+1, want, b)
		}
	}
}
