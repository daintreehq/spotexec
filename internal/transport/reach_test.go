package transport

import (
	"errors"
	"os/exec"
	"testing"
)

// Only a failure to find a network path counts as unreachable: anything
// else ssh reports says nothing about the network.
func TestNetworkFailure(t *testing.T) {
	for stderr, want := range map[string]Reach{
		"ssh: connect to host 1.2.3.4 port 22: Operation timed out":    Unreachable,
		"ssh: connect to host 1.2.3.4 port 22: Connection timed out":   Unreachable,
		"ssh: connect to host 1.2.3.4 port 22: No route to host":       Unreachable,
		"ssh: connect to host 1.2.3.4 port 22: Network is unreachable": Unreachable,
		"ssh: connect to host 1.2.3.4 port 22: Connection refused":     ReachUnknown,
		"ec2-user@1.2.3.4: Permission denied (publickey).":             ReachUnknown,
		"Host key verification failed.":                                ReachUnknown,
		"":                                                             ReachUnknown,
	} {
		if got := networkFailure(stderr); got != want {
			t.Errorf("%q: %v, want %v", stderr, got, want)
		}
	}
}

// A probe ssh couldn't run, or that was killed, says nothing; the remote
// shell failing means the VM answered.
func TestProbeResult(t *testing.T) {
	if got := probeResult(errors.New("fork/exec ssh: too many open files"), ""); got != ReachUnknown {
		t.Errorf("ssh didn't start: %v", got)
	}
	run := func(script string) error { return exec.Command("sh", "-c", script).Run() }
	if got := probeResult(run("kill -9 $$"), ""); got != ReachUnknown {
		t.Errorf("killed: %v", got)
	}
	if got := probeResult(run("exit 1"), ""); got != Reachable {
		t.Errorf("remote shell failed: %v", got)
	}
	if got := probeResult(run("exit 255"), "ssh: connect to host h port 22: No route to host"); got != Unreachable {
		t.Errorf("no route: %v", got)
	}
	if got := probeResult(run("exit 255"), "Permission denied (publickey)."); got != ReachUnknown {
		t.Errorf("auth: %v", got)
	}
}
