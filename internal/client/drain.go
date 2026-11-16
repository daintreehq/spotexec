package client

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
)

// Early drain (docs/POOL.md, "Early drain"): a surplus (standby) VM ends
// once it has been idle for result_retention instead of idle_timeout.
// Each drain is committed in the ledger first (BeginDrain), so the VM is
// never placed on again, and then handed to the VM (spotexec-helper
// schedule-drain): its own watchdog ends it once idle that long with no
// session holding it. Nothing on this machine has to stay running for it,
// which is why an sx process can do this as its job ends, and why the pool
// manager is not needed for it. A VM booted before scheduled drains is
// asked to drain now instead (try-drain), and asked again later while its
// results are still kept.

// DrainResult is one VM's answer to an early drain.
type DrainResult struct {
	ID     string
	Result string        // a helper TryDrain result (helper.Drain*)
	Idle   time.Duration // how long it had been idle (try-drain only; -1 unknown)
}

// drainHelperReady reports whether the VM can be asked without installing
// a helper there, which would touch its idle clock (tests substitute it).
var drainHelperReady = func(r *config.Runner, in provider.Instance) bool {
	return transport.New(in.Target, r.WorkRoot).HelperInstalled()
}

// drainCmd runs a helper subcommand on the VM, never installing one
// (tests substitute it).
var drainCmd = func(ctx context.Context, r *config.Runner, in provider.Instance, args ...string) ([]byte, error) {
	c := transport.New(in.Target, r.WorkRoot)
	if err := c.EnsureMaster(ctx); err != nil {
		return nil, err
	}
	return c.HelperCmdInstalled(ctx, args...)
}

// drainCallTimeout bounds one VM's answer.
const drainCallTimeout = 25 * time.Second

// EarlyDrain commits and sends the early drains due among running: standby
// VMs that may go (BeginDrain), and committed ones not yet handed over.
// It returns the VMs that answered. Nothing outlives the call.
func (p *Pool) EarlyDrain(ctx context.Context, running []provider.Instance) []DrainResult {
	if !p.R.EarlyDrain() || p.R.SchedPolicy() != config.PolicyConsolidate || !p.R.Ephemeral() {
		return nil
	}
	due := map[string]bool{}
	_ = p.with(func(st *poolState) error {
		for id, v := range st.sched().VMs {
			due[id] = v.Role == roleStandby || (v.Role == roleDrainRequested && !v.DrainScheduled)
		}
		return nil
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var out []DrainResult
	for _, in := range running {
		if !due[in.ID] || !drainHelperReady(p.R, in) {
			continue
		}
		op, ok, err := p.BeginDrain(ctx, in.ID)
		if err != nil || !ok {
			continue
		}
		wg.Add(1)
		go func(in provider.Instance, op string) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, drainCallTimeout)
			defer cancel()
			res, ok := p.askDrain(cctx, in, op)
			if !ok {
				return // asked again later, with the same operation
			}
			p.FinishDrain(in.ID, op, res.Result, res.Detail)
			idle := time.Duration(-1)
			if res.IdleS >= 0 {
				idle = time.Duration(res.IdleS) * time.Second
			}
			mu.Lock()
			out = append(out, DrainResult{ID: in.ID, Result: res.Result, Idle: idle})
			mu.Unlock()
		}(in, op)
	}
	wg.Wait()
	return out
}

// askDrain hands the drain to the VM, or on a VM whose watchdog predates
// that, asks it to drain now if it has kept its results long enough.
func (p *Pool) askDrain(ctx context.Context, in provider.Instance, op string) (helper.TryDrainResult, bool) {
	ret := p.R.ResultRetention().String()
	res, ok := p.drainAnswer(ctx, in, op, "schedule-drain", "--op", op, "--min-idle", ret)
	if ok && res.Result == helper.DrainUnsupported {
		res, ok = p.drainAnswer(ctx, in, op, "try-drain", "--op", op, "--min-idle", ret, "--reason", "surplus capacity")
	}
	return res, ok
}

func (p *Pool) drainAnswer(ctx context.Context, in provider.Instance, op string, args ...string) (helper.TryDrainResult, bool) {
	var res helper.TryDrainResult
	out, err := drainCmd(ctx, p.R, in, args...)
	if err != nil || json.Unmarshal(out, &res) != nil || res.Op != op {
		return res, false
	}
	return res, true
}

// leaseDrainBudget bounds the early-drain pass at the end of a job: it
// only sends what the VMs then carry out themselves.
const leaseDrainBudget = 5 * time.Second

// DrainSurplus is the early-drain pass an sx process runs as its lease
// ends, over the VMs last listed: a standby VM is usually busy, or still
// keeping its results, while jobs come and go, and the last job to end on
// it may be the last sx process to run for a while.
func (p *Pool) DrainSurplus() {
	if !p.R.EarlyDrain() || p.R.SchedPolicy() != config.PolicyConsolidate || !p.R.Ephemeral() {
		return
	}
	var listed []provider.Instance
	_ = p.with(func(st *poolState) error {
		for _, in := range st.Instances {
			if in.State == provider.StateRunning {
				listed = append(listed, in)
			}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), leaseDrainBudget)
	defer cancel()
	p.EarlyDrain(ctx, listed)
}
