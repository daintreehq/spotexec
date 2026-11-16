package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/provider"
)

const heldWhy = "unreachable from this client since 13:42 (2 VMs there don't answer SSH: i-1, i-2): no launches there until a VM there answers, or until 13:52"

// `spotexec status` says which regions a fleet launches nothing into, and
// why, once per fleet (#225).
func TestStatusShowsRegionHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	printStatus(f, []runnerStatus{
		{Name: "spot/i-1", Type: "aws-spot", Fleet: "spot", LaunchHolds: map[string]provider.RegionHold{"ap-south-1": {Why: heldWhy}},
			Info: provider.Info{State: provider.StateRunning, InstanceID: "i-1", Region: "ap-south-1"}},
		{Name: "spot/i-2", Type: "aws-spot", Fleet: "spot",
			Info: provider.Info{State: provider.StateRunning, InstanceID: "i-2", Region: "ap-south-1"}},
	}, nil)
	f.Close()
	b, _ := os.ReadFile(path)
	out := string(b)
	if strings.Count(out, "spot: no launches into ap-south-1: "+heldWhy) != 1 {
		t.Fatalf("hold not shown once:\n%s", out)
	}
}

// `spotexec prices` marks a held region's options and says why below the
// table, even when the limit hides its rows.
func TestPricesShowRegionHolds(t *testing.T) {
	v := runCostView()
	v.unreach = map[string]provider.RegionHold{"ap-south-2": {Why: heldWhy}}
	var b bytes.Buffer
	v.writeText(&b, 20)
	out := b.String()
	if !strings.Contains(row(strings.Split(out, "\n"), "ap-south-2a"), "skipped for now: this client can't reach ap-south-2 (below)") {
		t.Fatalf("row not marked:\n%s", out)
	}
	if !strings.Contains(out, "No launches for now:\n  ap-south-2: "+heldWhy) {
		t.Fatalf("no summary:\n%s", out)
	}
	b.Reset()
	v.writeText(&b, 1)
	if !strings.Contains(b.String(), "ap-south-2: "+heldWhy) {
		t.Fatalf("summary hidden by --limit:\n%s", b.String())
	}
	b.Reset()
	if err := v.writeJSON(&b); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Holds map[string]provider.RegionHold `json:"launch_holds"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || got.Holds["ap-south-2"].Why != heldWhy {
		t.Fatalf("json holds %+v, %v", got.Holds, err)
	}
}
