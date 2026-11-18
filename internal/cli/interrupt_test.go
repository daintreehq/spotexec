package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
)

// The parts of a task interrupted on a remote runner report the
// interrupt, as a single run does, and not the error the interrupt ended
// them with (#289).
func TestInterruptedRemotePartsPrintNoError(t *testing.T) {
	dir := project(t, "[tasks.unit]\nrun = \"true\"\nshards = 2\n")
	sshtest.Install(t).Set("rpc-heartbeat", "") // the slot is never granted
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"dev@box\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	go func() {
		time.Sleep(2 * time.Second)
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	code, stderr := runMain(t, "sx", "-C", dir, "--queue-timeout", "30s", "unit")
	if code != client.ExitInterrupted {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "spotexec:") {
		t.Errorf("an interrupted part printed an error: %q", stderr)
	}
	assertTail(t, stderr,
		`^sx: unit \d/2 · interrupted · \S+ · exit 130$`,
		`^sx: unit \d/2 · interrupted · \S+ · exit 130$`,
		`^sx: unit · interrupted · \S+ · exit 130$`)
}
