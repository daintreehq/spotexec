package transport

import "testing"

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
