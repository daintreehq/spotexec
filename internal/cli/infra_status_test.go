package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
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
		{Name: "idle", r: &config.Runner{MaxSlots: 3}, Info: running, Helper: &helper.Status{CPUs: 8, Holders: []protocol.Holder{}}},
		{Name: "older", r: &config.Runner{}, Info: running, Helper: &helper.Status{CPUs: 8, Holders: three[:1]}},
	}, nil)
	f.Close()
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	for i, want := range []string{"3/4", "0/3", "1/2"} {
		if !strings.Contains(lines[i+1], " "+want+" ") {
			t.Fatalf("row %d: want slots %s:\n%s", i+1, want, b)
		}
	}
}

type regionListerFleet struct {
	provider.Fleet
	insts    []provider.Instance
	unlisted map[string]error
}

func (f regionListerFleet) ListReachable(context.Context) ([]provider.Instance, map[string]error, error) {
	return f.insts, f.unlisted, nil
}

// One region that can't be listed shows as its own unknown row, with why,
// beside the instances of the regions that could be.
func TestStatusShowsReachableRegions(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "spot", Type: config.TypeAWSSpot, Region: "us-east-2", Regions: []string{"us-east-2", "ap-south-1", "eu-north-1"}}
	f := regionListerFleet{
		insts: []provider.Instance{{ID: "i-ok", State: provider.StatePending, Region: "us-east-2"}},
		unlisted: map[string]error{"ap-south-1": errors.New("no answer within 15s"),
			"eu-north-1": errors.New("UnauthorizedOperation")},
	}
	defer func(n func(*config.Runner) (provider.Provider, error)) { newProvider = n }(newProvider)
	newProvider = func(*config.Runner) (provider.Provider, error) { return f, nil }
	rows := gatherStatus(context.Background(), []*config.Runner{r}, nil)
	var names []string
	for _, row := range rows {
		names = append(names, row.Name+"="+string(row.Info.State))
	}
	if got := strings.Join(names, " "); got != "spot/ap-south-1=unknown spot/eu-north-1=unknown spot/i-ok=pending" {
		t.Fatalf("rows %s", got)
	}
	if rows[0].Info.Region != "ap-south-1" || rows[0].Info.InstanceID != "" {
		t.Fatalf("region row %+v", rows[0])
	}
	path := filepath.Join(t.TempDir(), "out")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	printStatus(out, rows, nil)
	out.Close()
	b, _ := os.ReadFile(path)
	text := string(b)
	lines := strings.Split(text, "\n")
	if !strings.Contains(lines[1], "aws-spot ap-south-1") || !strings.Contains(lines[1], "unknown") || !strings.Contains(lines[1], "instances not listed") {
		t.Fatalf("region row:\n%s", text)
	}
	if !strings.Contains(lines[3], "spot/i-ok") {
		t.Fatalf("instance row:\n%s", text)
	}
	if !strings.Contains(text, "spot/ap-south-1: couldn't list ap-south-1: no answer within 15s") {
		t.Fatalf("no reason:\n%s", text)
	}
	// Nothing listed and regions missing: those regions, not "0 instances".
	f.insts = nil
	newProvider = func(*config.Runner) (provider.Provider, error) { return f, nil }
	rows = gatherStatus(context.Background(), []*config.Runner{r}, nil)
	names = nil
	for _, row := range rows {
		names = append(names, row.Name+"="+string(row.Info.State)+" "+row.Error)
	}
	want := "spot/ap-south-1=unknown couldn't list ap-south-1: no answer within 15s|spot/eu-north-1=unknown couldn't list eu-north-1: UnauthorizedOperation"
	if got := strings.Join(names, "|"); got != want {
		t.Fatalf("rows %s", got)
	}
}

// A fleet in one region keeps its strict listing.
func TestListFleetPlain(t *testing.T) {
	boom := errors.New("boom")
	if _, unlisted, err := listFleet(context.Background(), listOnlyFleet{err: boom}); err != boom || unlisted != nil {
		t.Fatalf("%v %v", unlisted, err)
	}
	insts, unlisted, err := listFleet(context.Background(), listOnlyFleet{insts: []provider.Instance{{ID: "i-one"}}})
	if err != nil || unlisted != nil || len(insts) != 1 || insts[0].ID != "i-one" {
		t.Fatalf("%v %v %v", insts, unlisted, err)
	}
}

type listOnlyFleet struct {
	provider.Fleet
	insts []provider.Instance
	err   error
}

func (f listOnlyFleet) List(context.Context) ([]provider.Instance, error) { return f.insts, f.err }

// Status reads a consolidating fleet's roles, but doesn't wait out a pool
// manager that holds the pool's lock through a slow listing; a fleet under
// another policy has no roles to show.
func TestFleetRoles(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "spot", Type: config.TypeAWSSpot} // consolidate by default
	dir := provider.RunnerStateDir(r.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sched := `{"version":1,"vms":{"i-a":{"role":"standby","reason":"surplus"}}}`
	if err := os.WriteFile(filepath.Join(dir, "sched.json"), []byte(sched), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fleetRoles(context.Background(), r, client.NewPool(r, regionListerFleet{})); got["i-a"].Role != "standby" || got["i-a"].Reason != "surplus" {
		t.Fatalf("roles %v", got)
	}
	shadow := *r
	shadow.Scheduling.Policy = config.PolicyShadow
	if got := fleetRoles(context.Background(), &shadow, client.NewPool(&shadow, regionListerFleet{})); len(got) != 0 {
		t.Fatalf("shadow roles shown: %v", got)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "pool.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { rolesTimeout = d }(rolesTimeout)
	rolesTimeout = 100 * time.Millisecond
	start := time.Now()
	if got := fleetRoles(context.Background(), r, client.NewPool(r, regionListerFleet{})); len(got) != 0 {
		t.Fatalf("roles %v", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %s for the pool lock", d)
	}
}
