package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A runner config that can't be read must stop a revoke before anything
// is touched: it's where the machine's VMs are searched for.
func TestRevokeNeedsReadableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
	t.Setenv("SPOTEXEC_CONFIG", "")
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("version = 1\n[runners.aws\n"), 0o600)
	err := revokeMachine(context.Background(), "", "studio-01", true)
	if err == nil || !strings.Contains(err.Error(), "reading the runner config") {
		t.Fatalf("err = %v", err)
	}
}
