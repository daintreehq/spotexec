package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
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
	holder := protocol.Holder{Slot: 2, Client: protocol.ClientInfo{PID: 4242, Worktree: "/src/app", RunID: "20260101-120000-ab12cd34ef56"}}
	rows := []runnerStatus{{Name: "aws", Type: "aws-spot", Helper: &helper.Status{Holders: []protocol.Holder{holder}}, Cost: 0.5, CostFrom: "spot"}}
	if err := writeStatusJSON(&b, rows); err != nil {
		t.Fatal(err)
	}
	var got struct {
		FormatVersion *int `json:"format_version"`
		Runners       []struct {
			Name   string `json:"name"`
			Helper *struct {
				Holders []struct {
					Slot   int `json:"slot"`
					Client struct {
						PID      int    `json:"pid"`
						Worktree string `json:"worktree"`
						RunID    string `json:"run_id"`
					} `json:"client"`
				} `json:"holders"`
			} `json:"helper"`
			Cost     float64 `json:"hourly_cost"`
			CostFrom string  `json:"hourly_cost_from"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FormatVersion == nil || *got.FormatVersion != 1 || len(got.Runners) != 1 || got.Runners[0].Name != "aws" ||
		got.Runners[0].Cost != 0.5 || got.Runners[0].CostFrom != "spot" {
		t.Fatalf("json: %s", b.String())
	}
	if h := got.Runners[0].Helper; h == nil || len(h.Holders) != 1 || h.Holders[0].Slot != 2 || h.Holders[0].Client.PID != 4242 ||
		h.Holders[0].Client.Worktree != "/src/app" || h.Holders[0].Client.RunID != "20260101-120000-ab12cd34ef56" {
		t.Errorf("holders: %s", b.String())
	}
}

// The status command itself writes that object, not the bare rows.
func TestStatusCommandJSONIsVersioned(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.spot]\ntype = \"aws-spot\"\nregion = \"us-east-2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	defer func(n func(*config.Runner) (provider.Provider, error)) { newProvider = n }(newProvider)
	newProvider = func(*config.Runner) (provider.Provider, error) { return regionListerFleet{}, nil }
	var code int
	var stderr string
	out := captureStdout(t, func() { code, stderr = runMain(t, "spotexec", "status", "--json") })
	var got struct {
		FormatVersion *int             `json:"format_version"`
		Runners       []map[string]any `json:"runners"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %q %s", code, err, out, stderr)
	}
	if got.FormatVersion == nil || *got.FormatVersion != 1 || got.Runners == nil {
		t.Errorf("json: %s", out)
	}
}

// `status -A --json` says its format version, and its instances are a
// list even when the account holds none.
func TestAccountStatusCommandJSONIsVersioned(t *testing.T) {
	project(t, "") // ssh runners only: no Spot quota to look up
	fakeAccountSearch(t, &provider.AccountSearch{}, nil)
	var code int
	var stderr string
	out := captureStdout(t, func() { code, stderr = runMain(t, "spotexec", "status", "-A", "--json") })
	var got struct {
		FormatVersion *int              `json:"format_version"`
		Instances     []json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %q %s", code, err, out, stderr)
	}
	if got.FormatVersion == nil || *got.FormatVersion != 1 || got.Instances == nil || !strings.HasPrefix(out, `{"format_version":1,`) {
		t.Errorf("json: %s", out)
	}
}

// `daemon status --json` says its format version, also when no daemon
// runs.
func TestDaemonStatusJSONIsVersioned(t *testing.T) {
	project(t, "")
	var code int
	out := captureStdout(t, func() { code, _ = runMain(t, "spotexec", "daemon", "status", "--json") })
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %q", code, err, out)
	}
	st, hasStatus := got["status"]
	if string(got["format_version"]) != "1" || string(got["running"]) != "false" || string(got["pid"]) != "0" || !hasStatus || string(st) != "null" {
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
	var got struct {
		FormatVersion *int   `json:"format_version"`
		Files         int    `json:"files"`
		Bytes         int64  `json:"bytes"`
		Fingerprint   string `json:"fingerprint"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("exit %d, %v: %q %s", code, err, out, stderr)
	}
	// .spotexec.toml (empty) and a.txt.
	if got.FormatVersion == nil || *got.FormatVersion != 1 || got.Files != 2 || got.Bytes != 3 || got.Fingerprint == "" {
		t.Errorf("json: %s", out)
	}
}
