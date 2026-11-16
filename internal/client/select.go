package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
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
	// Instance: that VM as listed (its zone and type), when known.
	Instance provider.Instance
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
	OnlySlot     bool // wait for PreferSlot instead of taking another slot
	// PreferInstance is the VM this worktree ran on last: tried first among
	// its fleet's VMs, since its slot already holds the worktree's files and
	// build caches.
	PreferInstance string
	Rep            ui.Reporter
	NoBoot         bool            // never launch fleet instances
	Priority       int             // queue priority (higher first)
	Exclude        map[string]bool // fleet instance IDs to avoid (e.g. one being reclaimed)
	// Project: what a launch of a runner for this request looks for in
	// each region (nil, or a nil result: nothing in particular).
	Project func(r *config.Runner) *provider.Project
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

// newProvider builds a pool member's provider (tests substitute fakes).
var newProvider = provider.New

type member struct {
	fatal     error         // a problem no other instance of this runner would avoid
	down      bool          // a static runner whose last probe failed
	boot      *bootProgress // a VM of this fleet running its first-boot setup
	announced bool          // "connecting to" shown for this static runner
	r         *config.Runner
	p         provider.Provider
	pool      *Pool // nil for static runners
	t         provider.Target
	tErr      error
	ready     bool
}

// tryAcquireFn is tryAcquire (tests script the helpers' answers).
var tryAcquireFn = tryAcquire

// Acquire finds a free slot anywhere in the pool, scaling fleets out as
// needed (docs/POOL.md):
//
//  1. if nobody is queued, probe static runners and running fleet instances
//     without blocking (oldest instance first, so idle ones can scale in);
//  2. otherwise (or if everything is full) take a ticket in the pool's fair
//     queue (priority, then arrival) and signal demand to every fleet;
//  3. fleets launch instances while waiting demand exceeds the capacity
//     already launching, lower-priority fleets covering only the remainder;
//  4. only tickets near the head of the queue probe for slots, every
//     1.2-2.5s, until one frees up or queue_timeout passes.
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
		p, err := newProvider(r)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		m := &member{r: r, p: p}
		if f, ok := p.(provider.Fleet); ok {
			m.pool = NewPool(r, f)
			m.pool.since = start
		}
		members = append(members, m)
	}
	q := QueueFor(req.Pool)
	var ticket *Ticket
	defer func() { ticket.Leave() }()
	waiters := map[string]*Waiter{}
	var wmu sync.Mutex // waiters and ticket, shared with the heartbeat below
	defer func() {
		wmu.Lock()
		defer wmu.Unlock()
		for _, w := range waiters {
			w.Done()
		}
	}()
	// Keep the ticket and waiters fresh even while one round blocks for a
	// long time (slow probes, a slow cloud listing): stale ones are ignored
	// as stopped processes, which would reorder the queue and drop demand.
	beat, beatDone := make(chan struct{}), make(chan struct{})
	defer func() { close(beat); <-beatDone }() // joined before the ticket/waiters go
	go func() {
		defer close(beatDone)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-beat:
				return
			case <-t.C:
				wmu.Lock()
				ticket.Touch()
				for _, w := range waiters {
					w.Touch()
				}
				wmu.Unlock()
			}
		}
	}()
	join := func() error {
		if ticket != nil {
			return nil
		}
		t, err := q.Join(req.Priority, req.Base.Exclusive, req.Base.Client.Command)
		if err != nil {
			return err
		}
		wmu.Lock()
		ticket = t
		wmu.Unlock()
		return nil
	}
	// Everyone takes a ticket before probing: newcomers never overtake
	// waiting agents, and a burst of arrivals doesn't stampede SSH.
	if err := join(); err != nil {
		return nil, err
	}
	provisioning := map[string]bool{}
	var lastHolders []protocol.Holder
	lastPos := -1
	queued := false
	for round := 0; ; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// May this process probe now? Without a ticket (nobody waiting): yes.
		// With one: only within the first `window` tickets, where window is
		// the number of slots that could free up, and never past an
		// exclusive ticket ahead of us.
		window := 0
		fleetCands := map[*member][]provider.Instance{}
		reachable := 0
		staticUp := 0 // static runners not known to be down (busy counts as up)
		roundErrs := []string{}
		for _, m := range members {
			if m.fatal != nil {
				// Its setup is broken: no VM of it can serve (#162).
				roundErrs = append(roundErrs, m.fatalErr().Error())
				continue
			}
			if m.pool == nil {
				window += slotsOf(m.r)
				continue
			}
			cands, usable, err := m.pool.Candidates(ctx, req.Exclude)
			if err != nil {
				roundErrs = append(roundErrs, fmt.Sprintf("%s: %v", m.r.Name, err))
				continue
			}
			if !req.NoBoot || usable > 0 {
				reachable++ // a fleet can always grow (or is growing)
			}
			readyFirst(cands, m.r.WorkRoot)
			if req.PreferInstance != "" {
				for i, in := range cands {
					if in.ID == req.PreferInstance && i > 0 {
						cands = append([]provider.Instance{in}, append(append([]provider.Instance{}, cands[:i]...), cands[i+1:]...)...)
						break
					}
				}
			}
			fleetCands[m] = cands
			window += usable * m.pool.SlotsPerInstance()
		}
		mayProbe := true
		var qs QueueState
		for _, w := range waiters {
			w.Touch()
		}
		if ticket != nil {
			qs = q.State(ticket)
			mayProbe = qs.Position < max(1, window) && (!qs.ExclusiveHead || qs.Position == 0)
			if req.Base.Exclusive {
				mayProbe = qs.Position == 0 // a whole machine: only from the head
			}
			if qs.Position != lastPos {
				lastPos = qs.Position
				req.Rep.Event("queue_position", map[string]any{"position": qs.Position + 1, "queued": qs.Total, "priority": req.Priority})
			}
		}
		if mayProbe {
			for _, m := range members {
				if m.fatal != nil {
					continue
				}
				if m.pool == nil && req.Base.Exclusive && len(members) == 1 && ticket != nil && qs.Position == 0 && m.ready {
					// Head of the queue for a whole static machine: wait in its
					// helper, which holds the host gate against newcomers from
					// other machines or queues until we get it.
					rq := req
					rq.QueueTimeout = time.Until(deadline)
					if rq.QueueTimeout > 0 {
						lease, _, err := tryAcquire(ctx, rq, m.r, m.p, m.t, true)
						if err == nil {
							return lease, nil
						}
						roundErrs = append(roundErrs, fmt.Sprintf("%s: %v", m.r.Name, err))
						continue
					}
				}
				if m.pool == nil {
					lease, holders, err := m.probeStatic(ctx, req)
					if lease != nil {
						return lease, nil
					}
					m.down = err != nil
					if err != nil {
						roundErrs = append(roundErrs, fmt.Sprintf("%s: %v", m.r.Name, err))
						continue
					}
					reachable++
					lastHolders = holders
					continue
				}
				for _, in := range fleetCands[m] {
					lease, holders, out := m.probeInstance(ctx, req, in, deadline)
					if !(out == OutBusy && req.Base.Exclusive) {
						// An exclusive probe finding a partly busy instance must
						// not hide its free slots from everyone else.
						m.pool.Observe(in.ID, out, in.LaunchedAt)
					}
					if lease != nil {
						return lease, nil
					}
					if m.fatal != nil {
						if onlyFatal(members) {
							return nil, m.fatalErr()
						}
						// Another runner in the pool may still serve: stop
						// probing this one and fail over (#162).
						break
					}
					if out == OutBusy {
						lastHolders = holders
					}
					// Root and user bootstrap each get boot_timeout.
					if out == OutBooting && !in.LaunchedAt.IsZero() && time.Since(in.LaunchedAt) > 2*m.r.BootTimeout.Duration+2*time.Minute {
						// First-boot setup never finished (hung bootstrap):
						// the VM can't serve and may never idle out. Marked
						// draining only once the terminate request succeeded,
						// so a failed attempt is retried by the next prober.
						req.Rep.Warn("BOOT_STUCK", "%s: %s never finished first-boot setup; terminating it", m.r.Name, in.ID)
						tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
						if err := m.pool.Fleet.Terminate(tctx, in.ID); err == nil {
							m.pool.Observe(in.ID, OutDraining, in.LaunchedAt)
						}
						cancel()
					}
				}
			}
		} else {
			for _, m := range members {
				if m.pool == nil && m.ready {
					reachable++
				}
			}
		}
		// Only actual probe failures prove unavailability; a round where we
		// were not admitted to probe says nothing.
		if reachable == 0 && mayProbe {
			errs = append(errs, roundErrs...)
			return nil, fmt.Errorf("no runner available:\n  %s", strings.Join(errs, "\n  "))
		}
		if !queued {
			queued = true
			req.Rep.Event("queued", map[string]any{"holders": len(lastHolders)})
		}
		if err := join(); err != nil {
			return nil, err
		}
		// Out of time: give up before planning a launch nobody will use.
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: no free slot within %s", protocol.CodeQueueTimeout, queue)
		}
		// Everything is full: signal demand and grow fleets in priority order,
		// each covering only what the fleets before it are not launching.
		status := ""
		covered := 0
		blocked, fleets := 0, 0
		var blockErr error
		for _, m := range members {
			if m.pool == nil {
				continue
			}
			fleets++
			if m.fatal != nil {
				// Launches nothing, and its demand must not draw launches
				// from the fleets that can serve.
				blocked++
				blockErr = m.fatalErr()
				if w := waiters[m.r.Name]; w != nil {
					wmu.Lock()
					w.Done()
					delete(waiters, m.r.Name)
					wmu.Unlock()
				}
				continue
			}
			if req.NoBoot {
				continue // can't grow: don't signal demand others would launch for
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
				wmu.Lock()
				waiters[m.r.Name] = w
				wmu.Unlock()
			}
			if req.NoBoot {
				continue
			}
			d, err := m.pool.Plan(ctx, covered)
			if err != nil {
				status = fmt.Sprintf("%s: %v", m.r.Name, err)
				continue
			}
			if d.Live == 0 && d.Pending == 0 {
				// No VM to wait for, and none will be launched.
				if id := m.pool.BootFailure(); id != "" {
					blocked++
					blockErr = fmt.Errorf("%s: bootstrap_script failed on %s, so no new VMs are launched for an hour: fix bootstrap_script (the VM's /var/log/spotexec-bootstrap.log has the output), then `spotexec down %s` to clear it", m.r.Name, id, m.r.Name)
				} else if d.QuotaErr != "" {
					blocked++
					blockErr = fmt.Errorf("%s: %s", m.r.Name, d.QuotaErr)
				}
			}
			if d.Launch && !provisioning[d.ID] {
				provisioning[d.ID] = true
				req.Rep.Event("scale_out", map[string]any{"runner": m.r.Name, "launch_id": d.ID, "demand": d.Demand, "instances": d.Live})
				// Registered before the goroutine starts, so a process exiting
				// right now still waits for it (WaitLaunches).
				HoldLaunch()
				pctx := ctx
				if req.Project != nil {
					pctx = provider.WithProject(ctx, req.Project(m.r))
				}
				go m.provision(pctx, d.ID, req.Rep)
			}
			covered += d.Capacity
			if d.Pending > 0 || d.Launch {
				if status != "" {
					status += "; "
				}
				status += fmt.Sprintf("%d instance(s) starting on %s", d.Pending, m.r.Name)
			} else if status == "" && d.Demand > 0 {
				status = fmt.Sprintf("%s: %s", m.r.Name, d.Reason)
			}
		}
		for _, m := range members {
			if m.pool == nil && !m.down {
				staticUp++ // answered, or not probed yet (not at the queue's head)
			}
		}
		// Nothing can serve this request: every fleet can't launch (its
		// setup just failed, or the account's vCPU quota is full) and has no
		// VM to wait for, and no static runner answers. A static runner
		// that answers, even fully busy, is still worth waiting for.
		if blockErr != nil && blocked == fleets && staticUp == 0 {
			return nil, blockErr
		}
		if status == "" {
			status = "all slots busy (" + describeHolders(lastHolders) + ")"
		}
		pos := ""
		if ticket != nil {
			pos = fmt.Sprintf("#%d of %d · ", qs.Position+1, qs.Total)
		}
		if b := bootingNow(members); b != "" {
			req.Rep.Status("%s", b) // what the new VM is doing says more than the queue
		} else {
			req.Rep.Status("queued %s%s · %s", pos, ui.Dur(time.Since(start)), status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: no free slot within %s (%s)", protocol.CodeQueueTimeout, queue, status)
		}
		// Poll interval grows with queue position: the head probes most
		// often, so slots go to earlier (or higher-priority) tickets first.
		// Jitter spreads probes from many agents.
		sleep := 500*time.Millisecond + time.Duration(min(qs.Position, 10))*200*time.Millisecond +
			time.Duration(rand.IntN(400))*time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// fatalErr is why m can't serve, with how to fix it.
func (m *member) fatalErr() error {
	return fmt.Errorf("%s: %w (fix bootstrap_script, then `spotexec down %s`)", m.r.Name, m.fatal, m.r.Name)
}

// onlyFatal reports whether every member of the pool has failed in a way no
// other instance of it would avoid: nothing is left to fail over to.
func onlyFatal(members []*member) bool {
	for _, m := range members {
		if m.fatal == nil {
			return false
		}
	}
	return true
}

// slotsOf is a runner's slots per machine as the helper will compute it:
// max_slots, or (unset) vCPUs / 4, at least 1, at most protocol.MaxSlots.
// For AWS the vCPUs are known from the instance type; otherwise 4 until the
// runner reports.
func slotsOf(r *config.Runner) int {
	if r.MaxSlots > 0 {
		return r.MaxSlots
	}
	if n := provider.VMVCPUs(r); r.Type == config.TypeAWSSpot && n > 0 {
		return min(max(1, n/4), protocol.MaxSlots)
	}
	return 4
}

func (m *member) probeStatic(ctx context.Context, req AcquireRequest) (*Lease, []protocol.Holder, error) {
	if !m.ready {
		info, err := m.p.Up(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		m.t, m.ready = info.Target, true
	}
	if !m.announced {
		// Once: re-probing a busy machine every round would keep replacing
		// more useful progress (a VM's first boot) on the status line.
		m.announced = true
		req.Rep.Status("connecting to %s (%s)", m.r.Name, m.t)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lease, holders, err := tryAcquireFn(cctx, req, m.r, m.p, m.t, false)
	if err != nil && transport.IsConnError(err) {
		m.ready = false
		m.p.Forget()
	}
	return lease, holders, err
}

// helperInstalled reports whether this machine installed the helper on t
// (tests substitute it).
var helperInstalled = func(t provider.Target, workRoot string) bool {
	return transport.New(t, workRoot).HelperInstalled()
}

// readyFirst orders a fleet's candidates so VMs this machine already
// reached (the helper is installed) come first: a first contact uploads the
// helper, which can take a while, and a ready VM shouldn't wait on it.
func readyFirst(cands []provider.Instance, workRoot string) {
	ready := make(map[string]bool, len(cands))
	for _, in := range cands {
		ready[in.ID] = helperInstalled(in.Target, workRoot)
	}
	sort.SliceStable(cands, func(i, j int) bool { return ready[cands[i].ID] && !ready[cands[j].ID] })
}

func (m *member) probeInstance(ctx context.Context, req AcquireRequest, in provider.Instance, until time.Time) (*Lease, []protocol.Holder, Outcome) {
	// Room for Open to find a dead reused master (5s), dial afresh (up to
	// 10s) and still handshake: a reconnect must not read as unreachable.
	// The first contact also uploads the helper (a few MB): over a slow
	// link that alone can take most of 30s, and an upload cut short is
	// started over from nothing, so give it room to finish.
	d := 30 * time.Second
	if !helperInstalled(in.Target, m.r.WorkRoot) {
		// Never past the queue deadline, though, beyond the usual 30s.
		d = max(d, min(d+transport.HelperUploadBudget, time.Until(until)))
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	lease, holders, err := tryAcquireFn(cctx, req, m.r, m.p, in.Target, false)
	if m.boot != nil && m.boot.id == in.ID && !isCode(err, protocol.CodeBooting) {
		m.boot = nil // that VM is past its first boot (or gone)
	}
	switch {
	case lease != nil:
		lease.InstanceID, lease.Instance = in.ID, in
		req.Rep.Event("placed", map[string]any{"runner": m.r.Name, "instance_id": in.ID, "slot": lease.Acq.Token})
		return lease, nil, OutAcquired
	case err == nil:
		return nil, holders, OutBusy
	case isDraining(err):
		return nil, nil, OutDraining
	case isCode(err, protocol.CodeBooting):
		m.sawBooting(req.Rep, in, err)
		return nil, nil, OutBooting
	case isCode(err, protocol.CodeBootFailed):
		// The setup itself is broken: another VM would fail the same way.
		m.fatal = err
		return nil, nil, OutBootFailed
	}
	return nil, nil, OutUnreachable
}

// bootProgress is the latest word from a VM running its first-boot setup.
type bootProgress struct {
	id       string    // the VM
	launched time.Time // the VM's launch, for "how long it has been going"
	line     string    // the latest line of its bootstrap log
	seen     time.Time // when a probe last found it booting
	evented  time.Time // when it was last reported as an event
	evLine   string
}

// bootStatus is the status line for a booting VM: how long it has been
// going and, after ui.DetailSep, what it is doing.
func bootStatus(runner string, elapsed time.Duration, line string) string {
	s := fmt.Sprintf("%s: running first-boot setup %s", runner, ui.Dur(elapsed.Round(time.Second)))
	if line = ui.LogLine(line); line != "" {
		s += ui.DetailSep + line
	}
	return s
}

// sawBooting records a probe's RUNNER_BOOTING answer, and reports a changed
// bootstrap step as a boot_progress event (at most every 5s).
func (m *member) sawBooting(rep ui.Reporter, in provider.Instance, err error) {
	var re *transport.RemoteError
	errors.As(err, &re)
	b := m.boot
	if b == nil || b.id != in.ID {
		b = &bootProgress{id: in.ID}
		m.boot = b
	}
	b.launched, b.seen = in.LaunchedAt, time.Now()
	if re != nil {
		b.line = re.Detail
	}
	if line := ui.LogLine(b.line); line != b.evLine && time.Since(b.evented) >= 5*time.Second {
		b.evented, b.evLine = time.Now(), line
		rep.Event("boot_progress", map[string]any{"runner": m.r.Name, "instance_id": in.ID,
			"elapsed_ms": time.Since(in.LaunchedAt).Milliseconds(), "line": line})
	}
}

// bootingNow is the status line for a fleet VM still in its first-boot
// setup ("" when none is). It lasts until a probe of that VM says otherwise,
// so a slow probe of another VM doesn't blank it; only a minute without any
// word does.
func bootingNow(members []*member) string {
	for _, m := range members {
		if b := m.boot; b != nil && time.Since(b.seen) < time.Minute && !b.launched.IsZero() {
			return bootStatus(m.r.Name, time.Since(b.launched), b.line)
		}
	}
	return ""
}

func (m *member) provision(ctx context.Context, id string, rep ui.Reporter) {
	logf := func(format string, args ...any) {
		rep.Event("provision", map[string]any{"runner": m.r.Name, "launch_id": id, "message": fmt.Sprintf(format, args...)})
	}
	if ctx.Err() != nil {
		m.pool.Cancel(id) // interrupted before the launch request: don't send it
		ReleaseLaunch()
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.r.BootTimeout.Duration+time.Minute)
	defer cancel()
	launched := func(in provider.Instance) {
		// Where the launch went and why that option won, as soon as it's
		// known (the scale_out event comes before the choice).
		rep.Event("instance_launched", placed(map[string]any{"runner": m.r.Name, "launch_id": id, "instance_id": in.ID}, in))
	}
	in, err := m.pool.ProvisionHeldThen(pctx, id, logf, launched)
	if err != nil {
		rep.Warn("LAUNCH_FAILED", "%s: %v", m.r.Name, firstLine(err.Error()))
		return
	}
	rep.Event("instance_running", placed(map[string]any{"runner": m.r.Name, "launch_id": id, "instance_id": in.ID, "host": in.Target.String()}, in))
}

// placed adds where an instance is, and why it launched there, to an
// event's fields.
func placed(ev map[string]any, in provider.Instance) map[string]any {
	for k, v := range map[string]string{"region": in.Region, "zone": in.Zone, "type": in.Type} {
		if v != "" {
			ev[k] = v
		}
	}
	if in.Placement != nil {
		ev["placement"] = in.Placement
	}
	return ev
}

func isCode(err error, code string) bool {
	var re *transport.RemoteError
	return errors.As(err, &re) && re.Code == code
}

// EnsureSpare makes sure the pool has room for req somewhere other than the
// excluded instances (a free slot, or a whole idle VM for an exclusive
// request), launching a VM if needed, without keeping it: used when an
// instance announces its reclamation, so a replacement is warm by the time
// the doomed run needs it.
func EnsureSpare(ctx context.Context, req AcquireRequest) {
	req.Rep = quietReporter{req.Rep}
	req.Base.KeepOwner = true // only probing for room, not using the slot
	l, err := Acquire(ctx, req)
	if err == nil {
		l.Close()
	}
}

// quietReporter forwards events but not spinner/status output.
type quietReporter struct{ ui.Reporter }

func (q quietReporter) Status(string, ...any) {}
func (q quietReporter) Clear()                {}

// Event drops what would describe the spare's throwaway slot as if it were
// the run's own (placed, slot_acquired, queue progress); launches and
// warnings still come through.
func (q quietReporter) Event(name string, f map[string]any) {
	switch name {
	case "placed", "slot_acquired", "queued", "queue_position":
		return
	}
	q.Reporter.Event(name, f)
}

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

// queueGrace is how long past the queue deadline tryAcquire waits for the
// helper's own verdict.
var queueGrace = time.Minute

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
	a.OnlySlot = req.OnlySlot
	a.SpotWatch = r.Ephemeral()
	if err := s.Conn.Send(protocol.TypeAcquire, a); err != nil {
		s.Close()
		return nil, nil, err
	}
	// The helper enforces the queue deadline; this backstop (with room for
	// its reply) covers a helper that heartbeats but never answers.
	qctx := ctx
	if wait && req.QueueTimeout > 0 {
		var cancel context.CancelFunc
		qctx, cancel = context.WithTimeout(ctx, req.QueueTimeout+queueGrace)
		defer cancel()
	}
	for {
		env, err := s.Expect(qctx, protocol.TypeAcquired, protocol.TypeBusy, protocol.TypeWaiting)
		if err != nil && qctx.Err() != nil && ctx.Err() == nil {
			s.Abort()
			return nil, nil, &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeQueueTimeout,
				Message: fmt.Sprintf("no slot on %s within %s (the runner never answered)", r.Name, req.QueueTimeout.Round(time.Second))}}
		}
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

// readyProbe prints the first-boot verdict, or "waiting" and the latest
// line of the bootstrap log.
const readyProbe = `cat /var/tmp/spotexec/ready 2>/dev/null || { echo waiting; tail -n 1 /var/log/spotexec-bootstrap.log 2>/dev/null; }`

func parseReadyProbe(out string) (state, line string) {
	state, line, _ = strings.Cut(strings.TrimLeft(out, "\n"), "\n")
	return strings.TrimSpace(state), strings.TrimSpace(line)
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
		// One round trip: the verdict, or "waiting" and the log's latest line.
		out, err := c.Run(cctx, readyProbe, nil)
		cancel()
		if err == nil {
			s, line := parseReadyProbe(string(out))
			switch {
			case s == "0":
				return nil
			case s == "waiting":
				rep.Status("%s", bootStatus(r.Name, time.Since(start), line))
			default:
				tail, _ := c.Run(ctx, "tail -n 20 /var/log/spotexec-bootstrap.log 2>/dev/null", nil)
				return &bootstrapError{fmt.Sprintf("%s: bootstrap_script exited %s; last log lines:\n%s", r.Name, s, strings.TrimSpace(string(tail)))}
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
// exec, pull and --same-slot), waiting if it is busy. Unless the caller is
// about to sync (syncing), it fails when the slot no longer holds this
// worktree's files.
func AcquireSlot(ctx context.Context, cfg *config.Config, ws *Workspace, r *config.Runner, lr *LastRun, rep ui.Reporter, host, display string, exclusive bool, queue time.Duration, syncing bool, fingerprint string) (*Lease, error) {
	p, err := provider.New(r)
	if err != nil {
		return nil, err
	}
	// A VM that has shut down would only fail after an SSH timeout: ask the
	// cloud first.
	var listed provider.Instance
	if f, ok := p.(provider.Fleet); ok && lr.InstanceID != "" {
		if live, err := f.List(ctx); err == nil {
			found := false
			for _, in := range live {
				if in.ID == lr.InstanceID {
					found, listed = true, in
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: %s, which held it, has shut down (idle_timeout or Spot reclaim)", ErrSlotGone, lr.InstanceID)
			}
		}
	}
	if queue <= 0 {
		queue = cfg.Project.QueueTimeout.Duration
	}
	req := AcquireRequest{
		Base: protocol.Acquire{RepoID: ws.RepoID, WorktreeKey: ws.WorktreeKey, Fingerprint: fingerprint, Exclusive: exclusive, KeepOwner: !syncing,
			ReadOnly: display == PullDisplay, // still allowed on a VM near max_lifetime
			Client:   protocol.ClientInfo{Host: host, PID: os.Getpid(), Worktree: ws.Root, Command: display}},
		QueueTimeout: queue, PreferSlot: lr.Slot, OnlySlot: true, Rep: rep,
	}
	lease, _, err := tryAcquire(ctx, req, r, p, lr.Target, true)
	if err != nil {
		return nil, err
	}
	lease.InstanceID, lease.Instance = lr.InstanceID, listed
	if lease.Acq.Slot != lr.Slot {
		lease.Close()
		return nil, fmt.Errorf("slot %d on %s is in use by another run; try again shortly", lr.Slot, r.Name)
	}
	if !syncing && lease.Acq.PrevWorktreeKey != ws.WorktreeKey {
		lease.Close()
		return nil, fmt.Errorf("slot %d on %s now holds another worktree's files; run `sx --same-slot <command>` (which syncs) instead", lr.Slot, r.Name)
	}
	return lease, nil
}

// PullDisplay is AcquireSlot's display for `spotexec pull`, which only
// copies files back.
const PullDisplay = "(pull)"

// ErrSlotGone means the slot a worktree last used no longer exists.
var ErrSlotGone = errors.New("the slot is gone")
