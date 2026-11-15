package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
