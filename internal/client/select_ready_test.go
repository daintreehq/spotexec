package client

import (
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/provider"
)

// VMs this machine already reached are probed before ones still needing
// the helper uploaded; otherwise the order (newest first, from the pool)
// is kept.
func TestReadyFirst(t *testing.T) {
	old := helperInstalled
	t.Cleanup(func() { helperInstalled = old })
	helperInstalled = func(t provider.Target, _ string) bool { return strings.HasPrefix(t.Host, "ready") }
	cands := []provider.Instance{
		{ID: "a", Target: provider.Target{Host: "new-a"}},
		{ID: "b", Target: provider.Target{Host: "ready-b"}},
		{ID: "c", Target: provider.Target{Host: "new-c"}},
		{ID: "d", Target: provider.Target{Host: "ready-d"}},
	}
	readyFirst(cands, "")
	var got []string
	for _, in := range cands {
		got = append(got, in.ID)
	}
	if strings.Join(got, "") != "bdac" {
		t.Fatalf("order %v, want b d a c", got)
	}
}
