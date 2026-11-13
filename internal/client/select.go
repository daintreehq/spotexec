package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/protocol"
	"github.com/spotexec/spotexec/internal/provider"
	"github.com/spotexec/spotexec/internal/transport"
	"github.com/spotexec/spotexec/internal/ui"
)

// Lease is an acquired slot on a runner, held until Close.
type Lease struct {
	Runner     *config.Runner
	Provider   provider.Provider
	Target     provider.Target
	Client     *transport.Client
	Sess       *transport.Session
	Acq        protocol.Acquired
	InstanceID string // fleet instance, when the runner is a fleet
}

// Close releases the slot.
func (l *Lease) Close() {
	if l != nil && l.Sess != nil {
		l.Sess.Close()
	}
}

// AcquireRequest bundles what selection needs.
type AcquireRequest struct {
	Pool         []*config.Runner
	Base         protocol.Acquire // RepoID, WorktreeKey, Fingerprint, Client, Exclusive
	QueueTimeout time.Duration
	PreferSlot   int
	Rep          ui.Reporter
	NoBoot       bool            // never launch fleet instances
	Exclude      map[string]bool // fleet instance IDs to avoid (e.g. one being reclaimed)
}

// Connect opens a helper session on a known target.
func Connect(ctx context.Context, r *config.Runner, t provider.Target) (*transport.Client, *transport.Session, error) {
	c := transport.New(t, r.WorkRoot)
	s, err := c.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, s, nil
}

type member struct {
	r     *config.Runner
	p     provider.Provider
	pool  *Pool // nil for static runners
	t     provider.Target
	tErr  error
	ready bool
}

// Acquire finds a free slot anywhere in the pool, scaling fleets out as
// needed (docs/POOL.md):
//
//  1. probe static runners and running fleet instances without blocking
//     (busiest-first, so idle instances can scale in);
//  2. if everything is full, register as a waiter and launch another fleet
//     instance when waiting demand exceeds the capacity already launching;
//  3. poll (with jitter) until a slot frees up or queue_timeout passes.
func Acquire(ctx context.Context, req AcquireRequest) (*Lease, error) {
	queue := req.QueueTimeout
	if queue <= 0 {
		queue = 10 * time.Minute
	}
	deadline := time.Now().Add(queue)
	start := time.Now()
	var members []*member
	var errs []string
	for _, r := range req.Pool {
		p, err := provider.New(r)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		m := &member{r: r, p: p}
		if f, ok := p.(provider.Fleet); ok {
			m.pool = NewPool(r, f)
		}
		members = append(members, m)
	}
	waiters := map[string]*Waiter{}
	defer func() {
		for _, w := range waiters {
			w.Done()
		}
	}()
	provisioning := map[string]bool{}
	var lastHolders []protocol.Holder
	queued := false
	for round := 0; ; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reachable := 0
		roundErrs := []string{}
		for _, m := range members {
			if m.pool == nil {
				lease, holders, err := m.probeStatic(ctx, req)
				if lease != nil {
					return lease, nil
				}
				if err != nil {
					roundErrs = append(roundErrs, fmt.Sprintf("%s: %v", m.r.Name, err))
					continue
				}
				reachable++
				lastHolders = holders
				continue
			}
			cands, err := m.pool.Candidates(ctx, req.Exclude)
			if err != nil {
				roundErrs = append(roundErrs, fmt.Sprintf("%s: %v", m.r.Name, err))
				continue
			}
			reachable++ // a fleet can always grow (or is growing)
			for _, in := range cands {
				lease, holders, out := m.probeInstance(ctx, req, in)
				m.pool.Observe(in.ID, out, in.LaunchedAt)
				if lease != nil {
					return lease, nil
				}
				if out == OutBusy {
					lastHolders = holders
				}
			}
		}
		if reachable == 0 {
			errs = append(errs, roundErrs...)
			return nil, fmt.Errorf("no runner available:\n  %s", strings.Join(errs, "\n  "))
		}
		if !queued {
			queued = true
			req.Rep.Event("queued", map[string]any{"holders": len(lastHolders)})
		}
		// No fleet to grow: queue inside the first reachable static runner's
		// helper instead of polling (FIFO-ish fairness, and --exclusive takes
		// the host gate without being starved by newcomers).
		if !hasFleet(members) && len(members) == 1 && members[0].ready {
			m := members[0]
			left := time.Until(deadline)
			if left <= 0 {
				return nil, fmt.Errorf("%s: no free slot within %s", protocol.CodeQueueTimeout, queue)
			}
			req.Rep.Status("all slots busy on %s (%s); queued", m.r.Name, describeHolders(lastHolders))
			rq := req
			rq.QueueTimeout = left
			lease, _, err := tryAcquire(ctx, rq, m.r, m.p, m.t, true)
			return lease, err
		}
		// Everything is full: signal demand and grow the first fleet that can.
		status := ""
		for _, m := range members {
			if m.pool == nil {
				continue
			}
			if waiters[m.r.Name] == nil {
				weight := 1
				if req.Base.Exclusive {
					weight = m.pool.SlotsPerInstance()
				}
				w, err := m.pool.Register(weight)
				if err != nil {
					return nil, err
				}
				waiters[m.r.Name] = w
			}
			if req.NoBoot {
				continue
			}
			d, err := m.pool.Plan(ctx)
			if err != nil {
				status = fmt.Sprintf("%s: %v", m.r.Name, err)
				continue
			}
			if d.Launch && !provisioning[d.ID] {
				provisioning[d.ID] = true
				req.Rep.Event("scale_out", map[string]any{"runner": m.r.Name, "launch_id": d.ID, "demand": d.Demand, "instances": d.Live})
				go m.provision(ctx, d.ID, req.Rep)
			}
			if status == "" && (d.Pending > 0 || d.Launch) {
				status = fmt.Sprintf("%d slot(s) wanted on %s; %d instance(s) starting", d.Demand, m.r.Name, d.Pending)
			} else if status == "" {
				status = fmt.Sprintf("%s: %s", m.r.Name, d.Reason)
			}
			if d.Launch || d.Pending > 0 {
				break // growing this fleet covers the demand; don't grow lower-priority ones too
			}
		}
		if status == "" {
			status = "all slots busy (" + describeHolders(lastHolders) + ")"
		}
		req.Rep.Status("queued %s · %s", ui.Dur(time.Since(start)), status)
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: no free slot within %s (%s)", protocol.CodeQueueTimeout, queue, status)
		}
		// Jittered poll interval: spreads probes from many waiting agents.
		sleep := 1200*time.Millisecond + time.Duration(rand.IntN(1300))*time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

func hasFleet(ms []*member) bool {
	for _, m := range ms {
		if m.pool != nil {
			return true
		}
	}
	return false
}

func (m *member) probeStatic(ctx context.Context, req AcquireRequest) (*Lease, []protocol.Holder, error) {
	if !m.ready {
		info, err := m.p.Up(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		m.t, m.ready = info.Target, true
	}
	req.Rep.Status("connecting to %s (%s)", m.r.Name, m.t)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lease, holders, err := tryAcquire(cctx, req, m.r, m.p, m.t, false)
	if err != nil && transport.IsConnError(err) {
		m.ready = false
		m.p.Forget()
	}
	return lease, holders, err
}

func (m *member) probeInstance(ctx context.Context, req AcquireRequest, in provider.Instance) (*Lease, []protocol.Holder, Outcome) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lease, holders, err := tryAcquire(cctx, req, m.r, m.p, in.Target, false)
	switch {
	case lease != nil:
		lease.InstanceID = in.ID
		req.Rep.Event("placed", map[string]any{"runner": m.r.Name, "instance_id": in.ID, "slot": lease.Acq.Token})
		return lease, nil, OutAcquired
	case err == nil:
		return nil, holders, OutBusy
	case isDraining(err):
		return nil, nil, OutDraining
	case isCode(err, protocol.CodeBooting):
		return nil, nil, OutBooting
	}
	return nil, nil, OutUnreachable
}

func (m *member) provision(ctx context.Context, id string, rep ui.Reporter) {
	logf := func(format string, args ...any) {
		rep.Event("provision", map[string]any{"runner": m.r.Name, "launch_id": id, "message": fmt.Sprintf(format, args...)})
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.r.BootTimeout.Duration+time.Minute)
	defer cancel()
	in, err := m.pool.Provision(pctx, id, logf)
	if err != nil {
		rep.Warn("LAUNCH_FAILED", "%s: %v", m.r.Name, firstLine(err.Error()))
		return
	}
	rep.Event("instance_running", map[string]any{"runner": m.r.Name, "instance_id": in.ID, "host": in.Target.String()})
}

func isCode(err error, code string) bool {
	var re *transport.RemoteError
	return errors.As(err, &re) && re.Code == code
}

// EnsureSpare makes sure the pool has a free slot somewhere other than the
// excluded instances, launching one if needed, without keeping it: used when
// an instance announces its reclamation, so a replacement is warm by the
// time the doomed run needs it.
func EnsureSpare(ctx context.Context, req AcquireRequest) {
	req.Rep = quietReporter{req.Rep}
	l, err := Acquire(ctx, req)
	if err == nil {
		l.Close()
	}
}

// quietReporter forwards events but not spinner/status output.
type quietReporter struct{ ui.Reporter }

func (q quietReporter) Status(string, ...any) {}
func (q quietReporter) Clear()                {}

func isDraining(err error) bool {
	var re *transport.RemoteError
	return errors.As(err, &re) && re.Code == protocol.CodeDraining
}

func describeHolders(h []protocol.Holder) string {
	if len(h) == 0 {
		return "no holders"
	}
	var parts []string
	for i, x := range h {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(h)-3))
			break
		}
		age := time.Since(time.UnixMilli(x.StartedAt)).Round(time.Second)
		cmd := x.Client.Command
		if len(cmd) > 24 {
			cmd = cmd[:23] + "…"
		}
		parts = append(parts, fmt.Sprintf("#%d %s %s", x.Token, cmd, age))
	}
	return strings.Join(parts, ", ")
}

func tryAcquire(ctx context.Context, req AcquireRequest, r *config.Runner, p provider.Provider, t provider.Target, wait bool) (*Lease, []protocol.Holder, error) {
	start := time.Now()
	c, s, err := Connect(ctx, r, t)
	if err != nil {
		return nil, nil, err
	}
	connectMS := time.Since(start).Milliseconds()
	a := req.Base
	a.MaxSlots = r.MaxSlots
	a.Wait = wait
	a.QueueMS = req.QueueTimeout.Milliseconds()
	a.PreferSlot = req.PreferSlot
	a.SpotWatch = r.Ephemeral()
	if err := s.Conn.Send(protocol.TypeAcquire, a); err != nil {
		s.Close()
		return nil, nil, err
	}
	for {
		env, err := s.Expect(ctx, protocol.TypeAcquired, protocol.TypeBusy, protocol.TypeWaiting)
		if err != nil {
			s.Close()
			return nil, nil, err
		}
		switch env.Type {
		case protocol.TypeBusy:
			var b protocol.Busy
			_ = env.Unmarshal(&b)
			s.Close()
			return nil, b.Holders, nil
		case protocol.TypeWaiting:
			var w protocol.Waiting
			_ = env.Unmarshal(&w)
			req.Rep.Status("waiting for a slot on %s (%s) %s", r.Name, describeHolders(w.Holders), ui.Dur(time.Since(start)))
			continue
		}
		var acq protocol.Acquired
		if err := env.Unmarshal(&acq); err != nil {
			s.Close()
			return nil, nil, err
		}
		req.Rep.Event("slot_acquired", map[string]any{
			"runner": r.Name, "slot": acq.Token, "workspace_slot": acq.Slot, "connect_ms": connectMS,
			"waited_ms": acq.WaitedMS, "host": t.String(),
		})
		return &Lease{Runner: r, Provider: p, Target: t, Client: c, Sess: s, Acq: acq}, nil, nil
	}
}

// WaitReady polls until SSH accepts our key and /var/tmp/spotexec/ready exists.
func WaitReady(ctx context.Context, r *config.Runner, t provider.Target, rep ui.Reporter, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	c := transport.New(t, r.WorkRoot)
	var last error
	start := time.Now()
	for time.Now().Before(deadline) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := c.Run(cctx, "cat /var/tmp/spotexec/ready 2>/dev/null || echo waiting", nil)
		cancel()
		if err == nil {
			s := strings.TrimSpace(string(out))
			switch {
			case s == "0":
				return nil
			case s == "waiting":
				rep.Status("%s: running first-boot setup %s", r.Name, ui.Dur(time.Since(start)))
			default:
				tail, _ := c.Run(ctx, "tail -n 20 /var/log/spotexec-bootstrap.log 2>/dev/null", nil)
				rep.Warn("BOOTSTRAP_FAILED", "%s: bootstrap_script exited %s; continuing anyway. Last log lines:\n%s", r.Name, s, strings.TrimSpace(string(tail)))
				return nil
			}
		} else {
			last = err
			rep.Status("%s: waiting for SSH at %s %s", r.Name, t.Host, ui.Dur(time.Since(start)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("runner %s not ready after %s: %v", r.Name, timeout, last)
}

// AcquireSlot re-acquires the exact workspace slot of a previous run (for
// exec and pull), waiting if it is busy. It fails unless the slot still holds
// this worktree's files.
func AcquireSlot(ctx context.Context, cfg *config.Config, ws *Workspace, r *config.Runner, lr *LastRun, rep ui.Reporter, host, display string, exclusive bool, queue time.Duration) (*Lease, error) {
	p, err := provider.New(r)
	if err != nil {
		return nil, err
	}
	if queue <= 0 {
		queue = cfg.Project.QueueTimeout.Duration
	}
	req := AcquireRequest{
		Base: protocol.Acquire{RepoID: ws.RepoID, WorktreeKey: ws.WorktreeKey, Exclusive: exclusive, KeepOwner: true,
			Client: protocol.ClientInfo{Host: host, PID: os.Getpid(), Worktree: ws.Root, Command: display}},
		QueueTimeout: queue, PreferSlot: lr.Slot, Rep: rep,
	}
	lease, _, err := tryAcquire(ctx, req, r, p, lr.Target, true)
	if err != nil {
		return nil, err
	}
	lease.InstanceID = lr.InstanceID
	if lease.Acq.Slot != lr.Slot {
		lease.Close()
		return nil, fmt.Errorf("slot %d on %s is in use by another run; try again shortly", lr.Slot, r.Name)
	}
	if lease.Acq.PrevWorktreeKey != ws.WorktreeKey {
		lease.Close()
		return nil, fmt.Errorf("slot %d on %s now holds another worktree's files; use `spotexec run` (which syncs) instead", lr.Slot, r.Name)
	}
	return lease, nil
}
