package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

func quotaErr() error {
	return &provider.QuotaError{Region: "us-east-2", Spot: true, Code: provider.QuotaCodeSpotStandard, VCPUs: 8, Desired: 32,
		Detail: "c7a.2xlarge: MaxSpotInstanceCountExceeded"}
}

// planOnce registers one unit of demand and plans.
func planOnce(t *testing.T, p *Pool) LaunchDecision {
	t.Helper()
	w, err := p.Register(1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Done()
	d, err := p.Plan(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestQuotaRefusalHoldsLaunchesWhileVMsRun(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	ids, _ := p.Reserve(ctx, 1)
	in, err := p.Provision(ctx, ids[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Observe(in.ID, OutBusy, in.LaunchedAt) // serving, every slot taken
	f.fail = quotaErr()
	ids, _ = p.Reserve(ctx, 1)
	if _, err := p.Provision(ctx, ids[0], nil); err == nil {
		t.Fatal("expected a quota refusal")
	}
	// Past the ordinary launch backoff, the quota still holds launches back:
	// the run queues for the VM that is running.
	_ = p.with(func(st *poolState) error {
		st.BackoffUntil = time.Time{}
		st.QuotaAt = time.Now().Add(-2 * launchBackoff)
		st.ListedAt = time.Time{}
		return nil
	})
	d := planOnce(t, p)
	if d.Launch || d.QuotaErr == "" || d.Live != 1 || !strings.Contains(d.Reason, "quota") {
		t.Fatalf("launch planned despite the quota: %+v", d)
	}
	if ids, _ := p.EnsureMin(ctx, 2); len(ids) != 0 {
		t.Fatalf("warm pool launched despite the quota: %d", len(ids))
	}
	// One of our VMs ending frees quota: launches resume.
	f.insts = nil
	_ = p.with(func(st *poolState) error { st.ListedAt = time.Time{}; return nil })
	if d := planOnce(t, p); !d.Launch || d.QuotaErr != "" {
		t.Fatalf("launch not retried after a VM ended: %+v", d)
	}
}

func TestQuotaRefusalWithNoVMsHoldsOnlyWaitingRuns(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	p.since = time.Now().Add(-time.Minute) // waiting when the refusal comes
	f.fail = quotaErr()
	ids, _ := p.Reserve(ctx, 1)
	_, _ = p.Provision(ctx, ids[0], nil)
	_ = p.with(func(st *poolState) error { st.BackoffUntil = time.Time{}; return nil })
	if d := planOnce(t, p); d.QuotaErr == "" || d.Launch || d.Live != 0 || d.Pending != 0 {
		t.Fatalf("refusal not visible to waiting runs: %+v", d)
	}
	// Still held well past the ordinary backoff (no retry loop while
	// another runner serves the waiting run).
	_ = p.with(func(st *poolState) error { st.QuotaAt = time.Now().Add(-2 * launchBackoff); return nil })
	if d := planOnce(t, p); d.QuotaErr == "" || d.Launch {
		t.Fatalf("waiting run retried the launch: %+v", d)
	}
	// A run that starts later tries once more (the quota may have been
	// raised meanwhile).
	later := NewPool(p.R, f)
	later.since = time.Now()
	if d := planOnce(t, later); d.QuotaErr != "" || !d.Launch {
		t.Fatalf("a new run is still blocked: %+v", d)
	}
}

func TestQuotaClearedBySuccessfulLaunch(t *testing.T) {
	ctx := context.Background()
	p, f := testPool(t, 4)
	f.fail = quotaErr()
	ids, _ := p.Reserve(ctx, 1)
	_, _ = p.Provision(ctx, ids[0], nil)
	f.fail = nil
	ids, _ = p.Reserve(ctx, 1)
	if _, err := p.Provision(ctx, ids[0], nil); err != nil {
		t.Fatal(err)
	}
	_ = p.with(func(st *poolState) error {
		if st.QuotaErr != "" {
			t.Fatal("quota refusal kept after a successful launch")
		}
		return nil
	})
}

type nopRep struct{}

func (nopRep) Event(string, map[string]any) {}
func (nopRep) Status(string, ...any)        {}
func (nopRep) Clear()                       {}
func (nopRep) Warn(string, string, ...any)  {}
func (nopRep) Done(string, bool)            {}
func (nopRep) PartDone(string, bool)        {}
func (nopRep) IsJSON() bool                 { return false }

var _ ui.Reporter = nopRep{}

// With no VM running and nothing else to serve the run, a quota refusal
// fails the run at once, saying how to raise the quota.
func TestAcquireFailsPromptlyOnQuotaWithNoVMs(t *testing.T) {
	_, f := testPool(t, 4)
	f.fail = quotaErr()
	r := f.r
	r.Type = config.TypeAWSSpot
	old := newProvider
	newProvider = func(*config.Runner) (provider.Provider, error) { return f, nil }
	t.Cleanup(func() { newProvider = old })

	start := time.Now()
	_, err := Acquire(context.Background(), AcquireRequest{Pool: []*config.Runner{r}, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err == nil {
		t.Fatal("acquire succeeded with no capacity")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s to fail: should be prompt", took)
	}
	for _, want := range []string{"us-east-2", "8 vCPUs", "--quota-code L-34B43A08 --desired-value 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// downStatic is a static runner that never answers.
type downStatic struct{ r *config.Runner }

func (d downStatic) Runner() *config.Runner { return d.r }
func (d downStatic) Status(context.Context) (provider.Info, error) {
	return provider.Info{}, errors.New("unreachable")
}
func (d downStatic) Up(context.Context, provider.Logger) (provider.Info, error) {
	return provider.Info{}, errors.New("ssh: connect to host: connection refused")
}
func (d downStatic) Down(context.Context, provider.Logger) error { return nil }
func (d downStatic) Doctor(context.Context) []provider.Check     { return nil }
func (d downStatic) Forget()                                     {}

// A static runner in the pool that doesn't answer can't serve the run
// either: the quota refusal is still reported promptly.
func TestAcquireQuotaWithUnreachableStaticRunner(t *testing.T) {
	_, f := testPool(t, 4)
	f.fail = quotaErr()
	f.r.Type = config.TypeAWSSpot
	st := &config.Runner{Name: "box", Type: config.TypeSSH, Host: "box"}
	old := newProvider
	newProvider = func(r *config.Runner) (provider.Provider, error) {
		if r == st {
			return downStatic{st}, nil
		}
		return f, nil
	}
	t.Cleanup(func() { newProvider = old })
	start := time.Now()
	_, err := Acquire(context.Background(), AcquireRequest{Pool: []*config.Runner{st, f.r}, QueueTimeout: time.Minute, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "request-service-quota-increase") {
		t.Fatalf("want the quota error, got %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s to fail: should be prompt", took)
	}
}

// A static runner this run hasn't probed yet (it is far back in the queue)
// may well serve it later: the quota refusal must not fail the run.
func TestAcquireQuotaWaitsForUnprobedStaticRunner(t *testing.T) {
	_, f := testPool(t, 4)
	f.fail = quotaErr()
	f.r.Type = config.TypeAWSSpot
	st := &config.Runner{Name: "box", Type: config.TypeSSH, Host: "box", MaxSlots: 1}
	old := newProvider
	newProvider = func(r *config.Runner) (provider.Provider, error) {
		if r == st {
			return downStatic{st}, nil // never reached: not at the head
		}
		return f, nil
	}
	t.Cleanup(func() { newProvider = old })
	pool := []*config.Runner{st, f.r}
	q := QueueFor(pool)
	for i := 0; i < 3; i++ {
		tk, err := q.Join(10, false, "ahead")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tk.Leave)
	}
	_, err := Acquire(context.Background(), AcquireRequest{Pool: pool, QueueTimeout: 3 * time.Second, Rep: nopRep{}})
	WaitLaunches(5 * time.Second)
	if err == nil || strings.Contains(err.Error(), "quota") {
		t.Fatalf("want a queue timeout (the static runner may serve), got %v", err)
	}
}

func TestParseReadyProbe(t *testing.T) {
	for out, want := range map[string][2]string{
		"0\n":                                {"0", ""},
		"waiting\n":                          {"waiting", ""},
		"waiting\nSetting up git (1:2.43)\n": {"waiting", "Setting up git (1:2.43)"},
		"3\n":                                {"3", ""},
	} {
		if s, l := parseReadyProbe(out); s != want[0] || l != want[1] {
			t.Errorf("%q: %q %q", out, s, l)
		}
	}
}

type eventRep struct {
	nopRep
	events []map[string]any
}

func (e *eventRep) Event(name string, f map[string]any) {
	if name == "boot_progress" {
		e.events = append(e.events, f)
	}
}

func TestBootProgress(t *testing.T) {
	m := &member{r: &config.Runner{Name: "aws"}}
	in := provider.Instance{ID: "i-1", LaunchedAt: time.Now().Add(-42 * time.Second)}
	rep := &eventRep{}
	booting := func(line string) error {
		return &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeBooting, Message: "booting", Detail: line}}
	}
	if bootingNow([]*member{m}) != "" {
		t.Fatal("nothing seen booting yet")
	}
	m.sawBooting(rep, in, booting("spotexec: running bootstrap_script"))
	got := bootingNow([]*member{m})
	if got != "aws: running first-boot setup 42.0s"+ui.DetailSep+"spotexec: running bootstrap_script" {
		t.Fatalf("status %q", got)
	}
	m.sawBooting(rep, in, booting("Setting up git")) // within 5s: no second event
	if len(rep.events) != 1 || rep.events[0]["line"] != "spotexec: running bootstrap_script" || rep.events[0]["instance_id"] != "i-1" {
		t.Fatalf("events %v", rep.events)
	}
	if !strings.HasSuffix(bootingNow([]*member{m}), ui.DetailSep+"Setting up git") {
		t.Fatal("status not updated to the latest line")
	}
	m.boot.seen = time.Now().Add(-30 * time.Second) // a slow probe of another VM since
	if bootingNow([]*member{m}) == "" {
		t.Fatal("progress dropped while another probe was slow")
	}
	m.boot.seen = time.Now().Add(-time.Minute) // no word for a minute: no stale boot status
	if bootingNow([]*member{m}) != "" {
		t.Fatal("stale boot status")
	}
}
