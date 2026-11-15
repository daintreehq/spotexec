package helper

import (
	"slices"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestNetworkArgs(t *testing.T) {
	for _, c := range []struct {
		network string
		options []string
		want    []string
	}{
		{"", nil, []string{"--network", "host"}},
		{"host", []string{"--shm-size=1g"}, []string{"--network", "host"}},
		{"private", nil, []string{"--network", "bridge"}},
		{"private", []string{"--label", "x"}, []string{"--network", "bridge"}},
		// The user's own network flag wins, in any spelling.
		{"", []string{"--network", "none"}, nil},
		{"", []string{"--network=mynet"}, nil},
		{"", []string{"--net=bridge"}, nil},
		{"private", []string{"--net", "host"}, nil},
		// Boolean flags take no value, so the network flag after one counts.
		{"", []string{"--use-api-socket", "--network", "mynet"}, nil},
		{"private", []string{"--init", "--net", "none"}, nil},
		// A value that looks like the flag isn't the flag.
		{"", []string{"--label", "--network"}, []string{"--network", "host"}},
	} {
		got := networkArgs(&protocol.ContainerRun{Network: c.network, Options: c.options})
		if !slices.Equal(got, c.want) {
			t.Errorf("network %q, options %q: got %q, want %q", c.network, c.options, got, c.want)
		}
	}
}
