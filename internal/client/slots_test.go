package client

import (
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// Capacity planning counts automatic slots the way the helper does, within
// the same ceiling as an explicit max_slots.
func TestSlotsOfRespectsCeiling(t *testing.T) {
	for typ, want := range map[string]int{"c7a.large": 1, "c7a.4xlarge": 4, "c7a.48xlarge": protocol.MaxSlots} {
		if got := slotsOf(&config.Runner{Type: config.TypeAWSSpot, InstanceTypes: []string{typ}}); got != want {
			t.Errorf("slotsOf(%s) = %d, want %d", typ, got, want)
		}
	}
}

// A vcpus runner's VMs all have exactly vcpus vCPUs, whichever type each
// one gets.
func TestSlotsOfRequirements(t *testing.T) {
	for vcpus, want := range map[int]int{8: 2, 32: 8} {
		if got := slotsOf(&config.Runner{Type: config.TypeAWSSpot, VCPUs: vcpus}); got != want {
			t.Errorf("slotsOf(vcpus = %d) = %d, want %d", vcpus, got, want)
		}
	}
	if got := slotsOf(&config.Runner{Type: config.TypeAWSSpot, VCPUs: 8, MaxSlots: 3}); got != 3 {
		t.Errorf("max_slots ignored: %d", got)
	}
}
