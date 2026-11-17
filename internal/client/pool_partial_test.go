package client

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

// stallFleet is a fakeFleet spread over regions, some of whose APIs may
// not answer. Like AWSGlobal, its List fails as a whole while any region
// can't be listed, and ListReachable lists the rest.
type stallFleet struct {
	*fakeFleet
	stalled map[string]bool
	// hang: an unlisted region makes ListReachable wait for its context
	// instead of failing at once.
	hang bool
}

func (f *stallFleet) ListReachable(ctx context.Context) ([]provider.Instance, map[string]error, error) {
	all, _ := f.fakeFleet.List(ctx)
	var out []provider.Instance
	unlisted := map[string]error{}
	for _, in := range all {
		if f.stalled[in.Region] {
			continue
		}
		out = append(out, in)
	}
	for rg := range f.stalled {
		unlisted[rg] = errors.New("no answer within 15s: context deadline exceeded")
	}
	if len(unlisted) > 0 && f.hang {
		<-ctx.Done()
	}
	return out, unlisted, nil
}

func (f *stallFleet) List(ctx context.Context) ([]provider.Instance, error) {
	if len(f.stalled) > 0 {
		if f.hang {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, errors.New("ap-south-1: no answer")
	}
	return f.fakeFleet.List(ctx)
}

func listedIDs(t *testing.T, p *Pool, force bool) []string {
	t.Helper()
	v, err := p.Snapshot(context.Background(), force)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var ids []string
	for _, in := range v.Instances {
		ids = append(ids, in.ID)
	}
	slices.Sort(ids)
	return ids
}

func staleList(p *Pool) {
	_ = p.with(func(st *poolState) error { st.ListedAt = time.Now().Add(-listTTL - time.Second); return nil })
}

// Issue #241: one region's stalled API mustn't hide the regions that
// answered, nor make the pool forget the stalled region's VMs.
func TestRefreshUsesRegionsThatAnswered(t *testing.T) {
	p, f := testPool(t, 4)
	rf := &stallFleet{fakeFleet: f, stalled: map[string]bool{}}
	p.Fleet = rf
	host := provider.Target{Host: "127.0.0.1"}
	f.insts = []provider.Instance{
		{ID: "i-east", Region: "us-east-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now()},
		{ID: "i-south", Region: "ap-south-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now()},
	}
	if got := listedIDs(t, p, false); !slices.Equal(got, []string{"i-east", "i-south"}) {
		t.Fatalf("first listing: %v", got)
	}

	// ap-south-1 stops answering; a VM launches in us-east-1 meanwhile.
	rf.stalled["ap-south-1"] = true
	f.insts = append(f.insts, provider.Instance{ID: "i-new", Region: "us-east-1", State: provider.StateRunning,
		Target: host, LaunchedAt: time.Now()})
	staleList(p)
	if got := listedIDs(t, p, false); !slices.Equal(got, []string{"i-east", "i-new", "i-south"}) {
		t.Fatalf("partial listing: want the new VM seen and the stalled region's VM kept, got %v", got)
	}
}
