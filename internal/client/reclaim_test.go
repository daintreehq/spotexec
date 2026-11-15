package client

import (
	"testing"

	"github.com/daintreehq/spotexec/internal/provider"
)

type reclaimFleet struct {
	provider.Provider
	got []provider.Instance
}

func (f *reclaimFleet) Reclaimed(in provider.Instance) { f.got = append(f.got, in) }

// A Spot interruption notice is passed to the fleet with the VM's zone and
// type, so its replacement launches elsewhere.
func TestNoteReclaimed(t *testing.T) {
	f := &reclaimFleet{}
	in := provider.Instance{ID: "i-1", Region: "us-east-2", Zone: "us-east-2a", Type: "c7a.2xlarge"}
	noteReclaimed(&Lease{Provider: f, InstanceID: in.ID, Instance: in})
	if len(f.got) != 1 || f.got[0].ID != in.ID || f.got[0].Zone != in.Zone || f.got[0].Type != in.Type {
		t.Fatalf("reported %+v", f.got)
	}
	// A lease without its VM's details reports nothing.
	noteReclaimed(&Lease{Provider: f, InstanceID: "i-2"})
	if len(f.got) != 1 {
		t.Fatalf("reported %+v", f.got)
	}
	// Nor does a provider that doesn't weigh interruptions.
	noteReclaimed(&Lease{Provider: &provider.SSH{}, InstanceID: in.ID, Instance: in})
}
