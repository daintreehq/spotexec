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
	fake := sshtest.Install(t)
	fake.Set("rpc-heartbeat", "") // the slot is never granted
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"dev@box\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	// Interrupt once the parts are queued on the runner (it was dialed,
	// after the CLI took over SIGINT), and never after the run is over.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 400; i++ {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if strings.Contains(fake.Log(), "dial") {
				time.Sleep(500 * time.Millisecond)
				syscall.Kill(os.Getpid(), syscall.SIGINT)
				return
			}
		}
	}()
	code, stderr := runMain(t, "sx", "-C", dir, "--queue-timeout", "30s", "unit")
	close(done)
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
