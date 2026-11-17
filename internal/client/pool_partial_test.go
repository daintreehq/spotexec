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

// A VM gone from a region that answered is gone; the stalled region's
// launches and the views every caller gets (forced or not) keep what was
// last known there, until the region answers again.
func TestPartialListingKeepsWhatItCantSee(t *testing.T) {
	p, f := testPool(t, 4)
	rf := &stallFleet{fakeFleet: f, stalled: map[string]bool{}}
	p.Fleet = rf
	host := provider.Target{Host: "127.0.0.1"}
	f.insts = []provider.Instance{
		{ID: "i-east", Region: "us-east-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now()},
		{ID: "i-south", Region: "ap-south-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now()},
	}
	listedIDs(t, p, false)
	// Launches whose VMs were never listed: one in the region that will
	// stall, one in a region that answers, and one whose provisioner died
	// before RunInstances returned. All are past every expiry.
	old := time.Now().Add(-p.R.BootTimeout.Duration - listLagGrace - time.Hour)
	_ = p.with(func(st *poolState) error {
		st.Launches = []launchRec{
			{ID: "l-south", InstanceID: "i-booting", Region: "ap-south-1", Started: old, PID: 1},
			{ID: "l-east", InstanceID: "i-vanished", Region: "us-east-1", Started: old, PID: 1},
			{ID: "l-orphan", Started: old, PID: -1},
			// From before launch records carried a region: its VM was
			// listed in us-east-1, so its absence there now is telling.
			{ID: "l-legacy", InstanceID: "i-east", Started: old, PID: 1},
		}
		st.Notes = map[string]*note{"i-gone-earlier": {Ready: true}}
		st.sched().VMs["i-gone-earlier"] = &vmSched{}
		st.sched().Warm = []string{"i-gone-earlier"}
		return nil
	})

	rf.stalled["ap-south-1"] = true
	f.insts = f.insts[1:] // i-east ends
	staleList(p)
	if got := listedIDs(t, p, true); !slices.Equal(got, []string{"i-south"}) {
		t.Fatalf("forced partial listing: %v", got)
	}
	var launches []string
	_ = p.with(func(st *poolState) error {
		for _, l := range st.Launches {
			launches = append(launches, l.ID)
		}
		if !slices.Equal(st.Unlisted, []string{"ap-south-1"}) {
			t.Errorf("unlisted regions not recorded: %v", st.Unlisted)
		}
		if st.Notes["i-gone-earlier"] == nil {
			t.Error("a note was forgotten while a region couldn't be listed")
		}
		if st.sched().VMs["i-gone-earlier"] == nil || !slices.Equal(st.sched().Warm, []string{"i-gone-earlier"}) {
			t.Error("scheduling records were forgotten while a region couldn't be listed")
		}
		return nil
	})
	if !slices.Equal(launches, []string{"l-south", "l-orphan"}) {
		t.Fatalf("launches kept while ap-south-1 is unlisted: want l-south and l-orphan, got %v", launches)
	}
	v, _ := p.Snapshot(context.Background(), false)
	if v.Launching != 2 || !slices.Equal(v.Unlisted, []string{"ap-south-1"}) {
		t.Fatalf("view: launching %d unlisted %v", v.Launching, v.Unlisted)
	}

	// The region answers again: what it no longer holds is gone.
	rf.stalled = map[string]bool{}
	f.insts = nil
	staleList(p)
	if got := listedIDs(t, p, false); len(got) != 0 {
		t.Fatalf("recovered listing: %v", got)
	}
	_ = p.with(func(st *poolState) error {
		if len(st.Launches) != 0 || len(st.Unlisted) != 0 || st.Notes["i-gone-earlier"] != nil ||
			st.sched().VMs["i-gone-earlier"] != nil || len(st.sched().Warm) != 0 {
			t.Errorf("not reconciled after a full listing: launches %v unlisted %v notes %v sched %v %v",
				st.Launches, st.Unlisted, st.Notes, st.sched().VMs, st.sched().Warm)
		}
		return nil
	})
}

// A region whose API never answers holds a refresh (and the pool lock)
// only for refreshBudget, and the regions that answered are still used.
func TestRefreshIsBoundedWhenARegionHangs(t *testing.T) {
	defer func(r, l time.Duration) { refreshBudget, listBudget = r, l }(refreshBudget, listBudget)
	refreshBudget, listBudget = 400*time.Millisecond, 200*time.Millisecond
	p, f := testPool(t, 4)
	m := &hangMatcher{}
	rf := &stallFleet{fakeFleet: f, stalled: map[string]bool{}, hang: true}
	p.Fleet = &matchingStallFleet{stallFleet: rf, hangMatcher: m}
	host := provider.Target{Host: "127.0.0.1"}
	f.insts = []provider.Instance{
		{ID: "i-south", Region: "ap-south-1", State: provider.StateRunning, Target: host, LaunchedAt: time.Now()},
	}
	listedIDs(t, p, false)

	rf.stalled["ap-south-1"] = true
	f.insts = append(f.insts, provider.Instance{ID: "i-new", Region: "us-east-1", State: provider.StateRunning,
		Target: host, LaunchedAt: time.Now()})
	staleList(p)
	m.called, m.deadline = false, time.Time{}
	start := time.Now()
	got := listedIDs(t, p, false)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("refresh took %s: not bounded", took)
	}
	if !m.called {
		t.Fatal("the VMs weren't judged against the config")
	}
	// Judging shares the refresh's deadline, after a listing that used up
	// its own: it gets no budget of its own on top.
	if limit := start.Add(refreshBudget + 100*time.Millisecond); m.deadline.IsZero() || m.deadline.After(limit) {
		t.Fatalf("judging's deadline %v is past the refresh's %v", m.deadline, limit)
	}
	if !slices.Equal(got, []string{"i-new", "i-south"}) {
		t.Fatalf("bounded partial listing: %v", got)
	}
}

// hangMatcher judges VMs only once its context ends, like a type lookup
// in a region that doesn't answer.
type hangMatcher struct {
	called   bool
	deadline time.Time
}

func (m *hangMatcher) Mismatches(ctx context.Context, _ []provider.Instance) map[string]string {
	m.called = true
	m.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return nil
}

type matchingStallFleet struct {
	*stallFleet
	*hangMatcher
}

// With no earlier listing there is nothing to keep for a region that
// doesn't answer, and expired credentials are said at once, as before.
func TestPartialListingErrors(t *testing.T) {
	p, f := testPool(t, 4)
	rf := &stallFleet{fakeFleet: f, stalled: map[string]bool{"ap-south-1": true}}
	p.Fleet = rf
	if _, err := p.Snapshot(context.Background(), false); err == nil {
		t.Fatal("a first listing missing a region was taken as the whole fleet")
	}

	p2, f2 := testPool(t, 4)
	cf := &credStallFleet{stallFleet: &stallFleet{fakeFleet: f2, stalled: map[string]bool{}}}
	p2.Fleet = cf
	listedIDs(t, p2, false)
	cf.stalled["us-east-1"] = true
	staleList(p2)
	_, err := p2.Snapshot(context.Background(), false)
	var ce *provider.CredentialError
	if !errors.As(err, &ce) {
		t.Fatalf("credential failure served a cached view: %v", err)
	}
}

type credStallFleet struct{ *stallFleet }

func (f *credStallFleet) ListReachable(ctx context.Context) ([]provider.Instance, map[string]error, error) {
	insts, unlisted, err := f.stallFleet.ListReachable(ctx)
	for rg := range unlisted {
		unlisted[rg] = &provider.CredentialError{Err: errors.New("token expired")}
	}
	return insts, unlisted, err
}

// A caller that gives up while the fleet is being listed gets its error,
// not a refreshed view to act on.
func TestRefreshReturnsCallerCancellation(t *testing.T) {
	p, f := testPool(t, 4)
	rf := &stallFleet{fakeFleet: f, stalled: map[string]bool{}, hang: true}
	p.Fleet = rf
	listedIDs(t, p, false)
	var listed time.Time
	_ = p.with(func(st *poolState) error { listed = st.ListedAt.Add(-listTTL - time.Second); st.ListedAt = listed; return nil })

	rf.stalled["ap-south-1"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Snapshot(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cancelled caller got %v", err)
	}
	_ = p.with(func(st *poolState) error {
		if !st.ListedAt.Equal(listed) {
			t.Errorf("a cancelled refresh moved the listing time: %v", st.ListedAt)
		}
		return nil
	})
}
