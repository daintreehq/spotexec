package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/helper"
)

// `status --json` is an object with a format version, the runners' rows
// under "runners" (an empty list when there are none), so a script can
// tell the shape it reads.
func TestStatusJSONIsVersioned(t *testing.T) {
	var b bytes.Buffer
	if err := writeStatusJSON(&b, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(b.String()); got != `{"format_version":1,"runners":[]}` {
		t.Errorf("no runners: %s", got)
	}
	b.Reset()
	rows := []runnerStatus{{Name: "aws", Type: "aws-spot", Helper: &helper.Status{}, Cost: 0.5, CostFrom: "spot"}}
	if err := writeStatusJSON(&b, rows); err != nil {
		t.Fatal(err)
	}
	var got struct {
		FormatVersion *int `json:"format_version"`
		Runners       []struct {
			Name     string          `json:"name"`
			Helper   json.RawMessage `json:"helper"`
			Cost     float64         `json:"hourly_cost"`
			CostFrom string          `json:"hourly_cost_from"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FormatVersion == nil || *got.FormatVersion != statusFormat || len(got.Runners) != 1 || got.Runners[0].Name != "aws" ||
		len(got.Runners[0].Helper) == 0 || got.Runners[0].Cost != 0.5 || got.Runners[0].CostFrom != "spot" {
		t.Errorf("json: %s", b.String())
	}
}

// `daemon status --json` says its format version, also when no daemon
// runs.
func TestDaemonStatusJSONIsVersioned(t *testing.T) {
	project(t, "")
	var code int
	out := captureStdout(t, func() { code, _ = runMain(t, "spotexec", "daemon", "status", "--json") })
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %q", code, err, out)
	}
	if got["format_version"] != float64(daemonStatusFormat) || got["running"] != false || got["status"] != nil {
		t.Errorf("json: %s", out)
	}
}

// `sync-plan --json` says its format version beside the plan.
func TestSyncPlanJSONIsVersioned(t *testing.T) {
	dir := project(t, "")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var code int
	var stderr string
	out := captureStdout(t, func() { code, stderr = runMain(t, "spotexec", "sync-plan", "--json") })
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %q %s", code, err, out, stderr)
	}
	if got["format_version"] != float64(syncPlanFormat) || got["files"] == nil || got["fingerprint"] == nil {
		t.Errorf("json: %s", out)
	}
}
