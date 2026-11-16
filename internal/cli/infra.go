package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/cloudinit"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/daemon"
	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/helperbin"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// loadGlobal loads the runner set: global runners plus any declared by the
// project in the current directory.
func loadGlobal() (*config.Config, error) {
	if l, err := load(""); err == nil {
		return l.cfg, nil
	}
	g, err := config.LoadGlobal()
	if err != nil {
		return nil, err
	}
	p, _ := config.LoadProject("")
	return &config.Config{Global: g, Project: p}, nil
}

// selectRunners returns the named runners, or all configured ones.
func selectRunners(cfg *config.Config, names []string) ([]*config.Runner, error) {
	if len(names) == 0 {
		names = cfg.RunnerNames()
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no runners configured in %s (run `spotexec setup` to create one)", cfg.Global.Path)
	}
	var out []*config.Runner
	for _, n := range names {
		r, err := cfg.Runner(n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

// runnerStatus is one row of `spotexec status` (a static runner, or one
// instance of a fleet).
type runnerStatus struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Info   provider.Info  `json:"info"`
	Helper *helper.Status `json:"helper,omitempty"`
	Error  string         `json:"error,omitempty"`
	Cost   float64        `json:"hourly_cost,omitempty"`
	// CostFrom says where Cost came from: "spot" (the current Spot price of
	// the VM's type in its zone) or "hourly_cost" (the runner's setting).
	CostFrom string `json:"hourly_cost_from,omitempty"`
	Fleet    string `json:"fleet,omitempty"`
	// Mismatch says why a fleet VM no longer satisfies the runner's
	// current requirements: it takes no new runs and idles out.
	Mismatch string `json:"mismatch,omitempty"`
	// Role is a fleet VM's placement role (retained, standby, draining),
	// and why.
	Role       string `json:"role,omitempty"`
	RoleReason string `json:"role_reason,omitempty"`
	// LaunchHolds: the regions a fleet launches nothing into for now, and
	// why (on the fleet's first row).
	LaunchHolds map[string]provider.RegionHold `json:"launch_holds,omitempty"`
	r           *config.Runner
	// reclaimed records a Spot interruption notice the VM reports.
	reclaimed func()
	// answered: this status request reached the VM over SSH.
	answered bool
}

const (
	costSpot    = "spot"
	costSetting = "hourly_cost"
)

// instancePricer is provider.InstancePricer, as fleetPricer uses it.
type instancePricer interface {
	Prices(ctx context.Context, insts []provider.Instance) map[string]float64
}

// fleetPricer prices aws-spot fleet VMs for status and monitor, keeping
// one pricer per runner so monitor's refreshes reuse its quotes.
type fleetPricer struct {
	newPricer func(*config.Runner) (instancePricer, error)
	byRunner  map[string]instancePricer
}

func newFleetPricer() *fleetPricer {
	return &fleetPricer{byRunner: map[string]instancePricer{}, newPricer: func(r *config.Runner) (instancePricer, error) {
		p, err := provider.NewInstancePricer(r)
		if err != nil {
			return nil, err
		}
		return p, nil
	}}
}

// pricesTimeout bounds one refresh's price lookups, for every runner
// together, so a slow or unreachable pricing API delays status by at most
// this much.
var pricesTimeout = 10 * time.Second

// fleetInstances is one fleet's listed instances, to be priced.
type fleetInstances struct {
	r     *config.Runner
	insts []provider.Instance
}

// priceFleets sets the cost of each fleet row whose VM has a known Spot
// price, pricing every runner at once under one deadline. Rows it can't
// price keep their hourly_cost fallback; pricing never fails the status.
func (fp *fleetPricer) priceFleets(ctx context.Context, rows []runnerStatus, fleets []fleetInstances) {
	if fp == nil {
		return
	}
	type job struct {
		p     instancePricer
		f     fleetInstances
		found map[string]float64
	}
	var jobs []*job
	for _, f := range fleets {
		if f.r.Type != config.TypeAWSSpot || len(f.insts) == 0 {
			continue
		}
		p := fp.byRunner[f.r.Name]
		if p == nil {
			var err error
			if p, err = fp.newPricer(f.r); err != nil {
				continue
			}
			fp.byRunner[f.r.Name] = p
		}
		jobs = append(jobs, &job{p: p, f: f})
	}
	if len(jobs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pricesTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j.found = j.p.Prices(ctx, j.f.insts)
		}()
	}
	wg.Wait()
	spot := map[string]map[string]float64{} // runner -> instance ID -> USD/hour
	for _, j := range jobs {
		spot[j.f.r.Name] = j.found
	}
	for i := range rows {
		st := &rows[i]
		if st.Fleet == "" || st.Info.InstanceID == "" {
			continue
		}
		if usd, ok := spot[st.Fleet][st.Info.InstanceID]; ok {
			st.Cost, st.CostFrom = usd, costSpot
		}
	}
}

// fallbackCost is what a VM or machine costs per hour when its Spot price
// isn't known: the runner's hourly_cost, if set.
func fallbackCost(r *config.Runner) (float64, string) {
	if r.HourlyCost > 0 {
		return r.HourlyCost, costSetting
	}
	return 0, ""
}

// fleetMismatches says which of a fleet's VMs no longer satisfy the
// runner's current requirements, and why (none when that can't be told).
func fleetMismatches(ctx context.Context, f provider.Fleet, insts []provider.Instance) map[string]string {
	m, ok := f.(provider.Matcher)
	if !ok || len(insts) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return m.Mismatches(ctx, insts)
}

func gatherStatus(ctx context.Context, rs []*config.Runner, fp *fleetPricer) []runnerStatus {
	var rows []runnerStatus
	var fleets []fleetInstances
	// Fleets that keep region holds: their pool, and their first row.
	type holder struct {
		rh    provider.RegionHolder
		pool  *client.Pool
		first int
	}
	var holders []holder
	for _, r := range rs {
		p, err := provider.New(r)
		if err != nil {
			rows = append(rows, runnerStatus{Name: r.Name, Type: r.Type, Error: err.Error(), r: r})
			continue
		}
		if f, ok := p.(provider.Fleet); ok {
			insts, err := f.List(ctx)
			if err != nil {
				rows = append(rows, runnerStatus{Name: r.Name, Type: r.Type, Error: err.Error(), r: r, Info: provider.Info{State: provider.StateUnknown}})
				continue
			}
			first := len(rows)
			if len(insts) == 0 {
				detail := "0 instances; scales on demand"
				if r.Capped() {
					detail = fmt.Sprintf("0 of max %d instances; scales on demand", r.MaxInstances)
				}
				rows = append(rows, runnerStatus{Name: r.Name, Type: r.Type, r: r, Fleet: r.Name,
					Info: provider.Info{State: provider.StateAbsent, Detail: detail}})
			}
			fleets = append(fleets, fleetInstances{r, insts})
			mismatch := fleetMismatches(ctx, f, insts)
			pool := client.NewPool(r, f)
			roles := pool.Roles()
			for _, in := range insts {
				cost, from := fallbackCost(r)
				st := runnerStatus{Name: r.Name + "/" + in.ID, Type: r.Type, Cost: cost, CostFrom: from, r: r, Fleet: r.Name, Mismatch: mismatch[in.ID],
					Info: provider.Info{State: in.State, Target: in.Target, InstanceID: in.ID, InstanceType: in.Type, LaunchedAt: in.LaunchedAt, Spot: in.Spot, Region: in.Region}}
				if r.SchedPolicy() == config.PolicyConsolidate { // shadow roles are hypothetical
					st.Role, st.RoleReason = roles[in.ID].Role, roles[in.ID].Reason
				}
				if rc, ok := f.(provider.Reclaimer); ok {
					st.reclaimed = func() { rc.Reclaimed(in) }
				}
				rows = append(rows, st)
			}
			if rh, ok := f.(provider.RegionHolder); ok && len(rows) > first {
				holders = append(holders, holder{rh, pool, first})
			}
			continue
		}
		info, err := p.Status(ctx)
		cost, from := fallbackCost(r)
		st := runnerStatus{Name: r.Name, Type: r.Type, Cost: cost, CostFrom: from, Info: info, r: r}
		if err != nil {
			st.Error = err.Error()
		}
		rows = append(rows, st)
	}
	var wg sync.WaitGroup
	// Prices are looked up while the helpers are asked for their status;
	// each writes only its own fields of a row.
	wg.Add(1)
	go func() {
		defer wg.Done()
		fp.priceFleets(ctx, rows, fleets)
	}()
	for i := range rows {
		st := &rows[i]
		if st.Error != "" || st.Info.State != provider.StateRunning {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := transport.New(st.Info.Target, st.r.WorkRoot)
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if err := c.EnsureMaster(cctx); err != nil {
				st.Error = err.Error()
				return
			}
			b, err := c.HelperCmd(cctx, "status")
			// Reached it, even if its helper failed (an existing SSH master
			// may answer for a connection that is gone: only the command
			// says, and one cut short by the deadline says nothing).
			st.answered = err == nil || (!transport.IsConnError(err) && cctx.Err() == nil)
			if err != nil {
				st.Error = err.Error()
				return
			}
			var hs helper.Status
			if err := json.Unmarshal(b, &hs); err != nil {
				st.Error = "bad status from helper: " + err.Error()
				return
			}
			st.Helper = &hs
			// Asking marks the VM draining, so later asks (the pool
			// manager's) no longer see the notice: record it here too.
			if hs.SpotNotice != "" && st.reclaimed != nil {
				st.reclaimed()
			}
		}()
	}
	wg.Wait()
	// A VM that answered ends its region's hold; the holds left are shown.
	for _, h := range holders {
		for i := h.first; i < len(rows) && rows[i].Fleet == rows[h.first].Fleet; i++ {
			if rows[i].answered {
				h.pool.Reached(provider.Instance{ID: rows[i].Info.InstanceID, Region: rows[i].Info.Region})
			}
		}
		if holds, _ := h.rh.RegionHolds(); len(holds) > 0 {
			rows[h.first].LaunchHolds = holds
		}
	}
	return rows
}

func printStatus(w *os.File, sts []runnerStatus, cfg *config.Config) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNNER\tTYPE\tSTATE\tHOST\tSLOTS\tLOAD\tMEM FREE\tIDLE\tUSD/HOUR")
	for _, s := range sts {
		state := string(s.Info.State)
		if s.Error != "" && state == "" {
			state = "error"
		}
		host := s.Info.Target.String()
		if host == "" && s.Info.Detail != "" {
			host = s.Info.Detail
		}
		if s.Info.InstanceType != "" {
			host += " (" + s.Info.InstanceType + ")"
		}
		slots, load, mem, idle := "-", "-", "-", "-"
		if h := s.Helper; h != nil {
			n := s.r.MaxSlots
			if n == 0 {
				n = min(max(h.CPUs/4, 1), protocol.MaxSlots)
			}
			slots = fmt.Sprintf("%d/%d", len(h.Holders), n)
			if h.CPUs > 0 && h.Load1 > 0 {
				load = fmt.Sprintf("%.1f/%d", h.Load1, h.CPUs)
			}
			if h.MemTotalMB > 0 {
				mem = fmt.Sprintf("%.1f/%.0fG", float64(h.MemAvailMB)/1024, float64(h.MemTotalMB)/1024)
			}
			if h.IdleS >= 0 {
				idle = ui.Dur(time.Duration(h.IdleS) * time.Second)
				if h.IdleTimeout != "" {
					idle += " of " + h.IdleTimeout
				}
			}
			if h.Draining {
				state = "draining"
			}
		}
		if s.Mismatch != "" && state != "draining" {
			state = "outdated" // explained below the table
		} else if s.Role == "standby" && state == string(provider.StateRunning) {
			state = "standby" // explained below the table
		}
		typ := s.Type
		if s.r != nil && s.Info.Region != "" && (s.r.MultiRegion() || s.Info.Region != s.r.Region) {
			typ += " " + s.Info.Region // which region each instance landed in
		}
		cost := "-"
		if s.CostFrom == costSpot {
			cost = fmt.Sprintf("%.4f", s.Cost)
		} else if s.CostFrom == costSetting {
			cost = fmt.Sprintf("%.4f (hourly_cost)", s.Cost)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, typ, state, host, slots, load, mem, idle, cost)
	}
	tw.Flush()
	for _, s := range sts {
		var held []string
		for rg := range s.LaunchHolds {
			held = append(held, rg)
		}
		sort.Strings(held)
		for _, rg := range held {
			fmt.Fprintf(w, "  %s: no launches into %s: %s\n", s.Fleet, rg, s.LaunchHolds[rg].Why)
		}
		if s.Error != "" {
			fmt.Fprintf(w, "  %s: %s\n", s.Name, s.Error)
		}
		if s.Mismatch != "" {
			fmt.Fprintf(w, "  %s: doesn't match the current config (%s): takes no new runs and terminates when idle\n", s.Name, s.Mismatch)
		} else if s.Role == "standby" {
			fmt.Fprintf(w, "  %s: standby (%s): its runs finish, new ones go to the retained VMs unless they're full, and it terminates when idle\n", s.Name, s.RoleReason)
		}
		if s.Helper == nil {
			continue
		}
		for _, h := range s.Helper.Holders {
			age := time.Since(time.UnixMilli(h.StartedAt)).Round(time.Second)
			fmt.Fprintf(w, "  %s slot %d: %s  (%s, %s pid %d, %s)\n", s.Name, h.Token, h.Client.Command, age, h.Client.Host, h.Client.PID, h.Client.Worktree)
		}
		for _, h := range s.Helper.Waiters {
			age := time.Since(time.UnixMilli(h.StartedAt)).Round(time.Second)
			fmt.Fprintf(w, "  %s waiting for workspace slot %d: %s  (%s, %s pid %d, %s)\n", s.Name, h.Slot, h.Client.Command, age, h.Client.Host, h.Client.PID, h.Client.Worktree)
		}
	}
}

func cmdStatus() *cobra.Command {
	var asJSON, everyone bool
	c := &cobra.Command{
		Use:   "status [runner...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Show runner states, active slots, lock holders and idle timers",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			if everyone {
				res := searchAccount(cmd.Context(), cfg)
				quotas := spotQuotas(cmd.Context(), cfg)
				if asJSON {
					if res.Instances == nil {
						res.Instances = []provider.AccountInstance{}
					}
					if err := json.NewEncoder(os.Stdout).Encode(accountStatus{AccountSearch: res, SpotQuotas: quotas}); err != nil {
						return err
					}
					return res.Err()
				}
				printAccountInstances(res.Instances)
				printSearched(res)
				printSpotQuotas(os.Stdout, quotas)
				return res.Err()
			}
			rs, err := selectRunners(cfg, args)
			if err != nil {
				return err
			}
			sts := gatherStatus(cmd.Context(), rs, newFleetPricer())
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(sts)
			}
			printStatus(os.Stdout, sts, cfg)
			if pid := daemon.Running(); pid != 0 {
				fmt.Printf("\npool manager: running (pid %d) · `spotexec daemon status` for details\n", pid)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVarP(&everyone, "all-machines", "A", false, "every spotexec VM (bake builders and stopped VMs too), whichever machine launched it, in the regions and profiles your AWS runners use, and each region's Spot quota use")
	return c
}

func cmdMonitor() *cobra.Command {
	var interval time.Duration
	c := &cobra.Command{
		Use:   "monitor [runner...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Live dashboard of runner load, active slots and estimated burn rate",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			rs, err := selectRunners(cfg, args)
			if err != nil {
				return err
			}
			ctx, _, stop := signals()
			defer stop()
			fp := newFleetPricer()
			for {
				sts := gatherStatus(ctx, rs, fp)
				fmt.Print("\x1b[H\x1b[2J")
				fmt.Printf("spotexec monitor · %s · every %s · Ctrl-C to quit\n\n", time.Now().Format("15:04:05"), interval)
				printStatus(os.Stdout, sts, cfg)
				if line := burnLine(sts); line != "" {
					fmt.Printf("\n%s\n", line)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	c.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval")
	return c
}

// burnLine sums what the running ephemeral VMs cost per hour, saying
// where the figures came from and how many VMs had none.
func burnLine(sts []runnerStatus) string {
	burn, unpriced := 0.0, 0
	from := map[string]bool{}
	for _, s := range sts {
		if s.Info.State != provider.StateRunning || s.r == nil || !s.r.Ephemeral() {
			continue
		}
		if s.CostFrom == "" {
			unpriced++
			continue
		}
		burn += s.Cost
		from[s.CostFrom] = true
	}
	if len(from) == 0 {
		return ""
	}
	src := "Spot prices"
	switch {
	case from[costSpot] && from[costSetting]:
		src = "Spot prices and hourly_cost"
	case from[costSetting]:
		src = "hourly_cost"
	}
	line := fmt.Sprintf("estimated burn: $%.3f/hour (from %s)", burn, src)
	if unpriced > 0 {
		line += fmt.Sprintf(", not counting %d running VM(s) with no known price", unpriced)
	}
	return line
}

func firstEphemeral(cfg *config.Config) (*config.Runner, error) {
	// The same pool sx would use here (SPOTEXEC_POOL, the project's pool,
	// then default_pool), so `down` stops what `sx` started.
	rs, err := cfg.Pool(nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Ephemeral() {
			return r, nil
		}
	}
	for _, n := range cfg.RunnerNames() {
		if r := cfg.Global.Runners[n]; r.Ephemeral() {
			return r, nil
		}
	}
	return nil, errors.New("no ephemeral (aws-spot) runner configured")
}

func cmdUp() *cobra.Command {
	var count int
	c := &cobra.Command{
		Use:   "up [runner] [--count N]",
		Short: "Pre-warm fleet instances now (otherwise they launch on demand)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			var r *config.Runner
			if len(args) == 1 {
				r, err = cfg.Runner(args[0])
			} else {
				r, err = firstEphemeral(cfg)
			}
			if err != nil {
				return err
			}
			p, err := provider.New(r)
			if err != nil {
				return err
			}
			rep, _ := ui.New(ui.ModeAuto, os.Stderr, "")
			ctx, _, stop := signals()
			defer stop()
			f, ok := p.(provider.Fleet)
			if !ok {
				info, err := p.Up(ctx, nil)
				if err != nil {
					return err
				}
				c := transport.New(info.Target, r.WorkRoot)
				if err := c.EnsureMaster(ctx); err != nil {
					return err
				}
				if _, err := c.EnsureHelper(ctx, false); err != nil {
					return err
				}
				rep.Done(fmt.Sprintf("%s ready at %s", r.Name, info.Target), true)
				return nil
			}
			if count < 1 {
				count = 1
			}
			// Prewarming goes through the pool ledger, so it respects
			// max_instances (when set) and concurrent sx processes see the
			// launches.
			pool := client.NewPool(r, f)
			ids, err := pool.Reserve(ctx, count)
			if err != nil {
				return err
			}
			if len(ids) < count {
				rep.Warn("MAX_INSTANCES", "%s: launching %d of %d requested (max_instances = %d)", r.Name, len(ids), count, r.MaxInstances)
			}
			if len(ids) == 0 {
				return nil
			}
			count = len(ids)
			type res struct {
				in  provider.Instance
				err error
			}
			ch := make(chan res, count)
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-ctx.Done():
					rep.Warn("INTERRUPTED", "terminating the instance(s) being launched")
				case <-done:
				}
			}()
			for _, id := range ids {
				go func() {
					// A VM that never gets ready (or is interrupted with
					// Ctrl-C) is terminated and recorded as a failed boot.
					in, err := pool.Prewarm(ctx, id, func(f string, a ...any) {
						rep.Event("provision", map[string]any{"runner": r.Name, "message": fmt.Sprintf(f, a...)})
					}, func(ctx context.Context, in provider.Instance) error {
						return client.WaitReady(ctx, r, in.Target, rep, r.BootTimeout.Duration)
					}, func(ctx context.Context, in provider.Instance) bool {
						return client.BootFinished(ctx, r, in.Target)
					})
					if err == nil {
						// Ready, so other runs may already use it: a failed
						// helper install leaves it to idle out.
						if _, herr := transport.New(in.Target, r.WorkRoot).EnsureHelper(ctx, false); herr != nil {
							err = fmt.Errorf("%s: installing the helper: %w (the VM is ready and ends itself after idle_timeout)", in.ID, herr)
						}
					}
					ch <- res{in, err}
				}()
			}
			var errs []string
			bootFailed := false
			for i := 0; i < count; i++ {
				x := <-ch
				if x.err != nil {
					errs = append(errs, x.err.Error())
					bootFailed = bootFailed || errors.Is(x.err, client.ErrBootstrapFailed)
					continue
				}
				rep.Event("instance_ready", map[string]any{"runner": r.Name, "instance_id": x.in.ID, "host": x.in.Target.String()})
			}
			if len(errs) > 0 {
				rep.Clear()
				if ctx.Err() != nil {
					return fmt.Errorf("interrupted: %s", strings.Join(errs, "; "))
				}
				if bootFailed {
					errs = append(errs, fmt.Sprintf("fix bootstrap_script, then run `spotexec down %s` (until then, or for an hour, runs won't launch new VMs)", r.Name))
				}
				return errors.New(strings.Join(errs, "; "))
			}
			rep.Done(fmt.Sprintf("%s: %d instance(s) ready (each terminates after %s idle)", r.Name, count, r.IdleTimeout.Duration), true)
			return nil
		},
	}
	c.Flags().IntVar(&count, "count", 1, "number of instances to launch")
	return c
}

func cmdDown() *cobra.Command {
	var all, everyone, yes bool
	var machine string
	c := &cobra.Command{
		Use:   "down [runner...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Terminate ephemeral runner(s) now",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			if everyone || machine != "" {
				return downAccount(cmd.Context(), cfg, machine, yes)
			}
			var rs []*config.Runner
			switch {
			case all:
				rs, err = selectRunners(cfg, nil)
			case len(args) > 0:
				rs, err = selectRunners(cfg, args)
			default:
				var r *config.Runner
				r, err = firstEphemeral(cfg)
				rs = []*config.Runner{r}
			}
			if err != nil {
				return err
			}
			var errs []string
			for _, r := range rs {
				if !r.Ephemeral() {
					if len(args) > 0 {
						errs = append(errs, fmt.Sprintf("%s is a static ssh runner: there is nothing to terminate", r.Name))
					}
					continue
				}
				p, err := provider.New(r)
				if err != nil {
					errs = append(errs, err.Error())
					continue
				}
				if err := p.Down(cmd.Context(), func(f string, a ...any) { fmt.Fprintf(os.Stderr, "%s: %s\n", r.Name, fmt.Sprintf(f, a...)) }); err != nil {
					errs = append(errs, fmt.Sprintf("%s: %v", r.Name, err))
					continue
				}
				if f, ok := p.(provider.Fleet); ok {
					client.NewPool(r, f).Reset()
				}
				fmt.Printf("%s: terminated\n", r.Name)
				if r.MinInstances > 0 && daemon.Running() != 0 {
					fmt.Printf("%s: the pool manager will relaunch %d warm instance(s); `spotexec daemon stop` to prevent that\n", r.Name, r.MinInstances)
				}
			}
			if len(errs) > 0 {
				return errors.New(strings.Join(errs, "; "))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "terminate every ephemeral runner")
	c.Flags().BoolVarP(&everyone, "all-machines", "A", false, "terminate spotexec VMs (bake builders and stopped VMs too) launched by any machine, in the regions and profiles your AWS runners use (with --machine: only that one's)")
	c.Flags().StringVar(&machine, "machine", "", "only VMs launched by the machine named `NAME` in status -A")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before terminating other machines' VMs")
	return c
}

func cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [runner...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Verify SSH, ControlMaster, helper binaries, config and cloud credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			ok := true
			check := func(name string, good bool, msg string) {
				mark := "\x1b[32m✓\x1b[0m"
				if !good {
					mark = "\x1b[31m✗\x1b[0m"
					ok = false
				}
				fmt.Printf("%s %-28s %s\n", mark, name, msg)
			}
			warn := func(name, msg string) { fmt.Printf("\x1b[33m!\x1b[0m %-28s %s\n", name, msg) }
			for _, bin := range []string{"ssh", "git"} {
				p, err := exec.LookPath(bin)
				check(bin, err == nil, p)
			}
			avail := helperbin.Available()
			check("embedded helpers", len(avail) > 0, strings.Join(avail, ", "))
			if d := os.Getenv("SPOTEXEC_HELPER_DIR"); d != "" {
				check("SPOTEXEC_HELPER_DIR", true, d)
			}
			cfg, err := loadGlobal()
			if err != nil {
				check("global config", false, err.Error())
				return exitWith(1)
			}
			check("global config", true, cfg.Global.Path)
			if l, err := load(""); err == nil && !l.ws.Marked {
				check("project", true, "none here (cd into a Git worktree, or run `spotexec init` to mark a directory)")
			} else if err == nil {
				src := "(defaults; no .spotexec.toml)"
				if l.cfg.Project.Path != "" {
					src = l.cfg.Project.Path
				}
				check("project config", true, src)
				for _, n := range l.cfg.UnavailableNames() {
					warn("runner "+n, l.cfg.Unavailable[n].Error())
				}
				check("workspace", true, fmt.Sprintf("%s (repo id %s)", l.ws.Root, l.ws.RepoID))
				doctorContainer(l, args, check, warn)
				doctorDeps(cmd.Context(), l, check)
			} else {
				check("project config", false, err.Error())
			}
			rs, err := selectRunners(cfg, args)
			if err != nil {
				check("runners", false, err.Error())
				return exitWith(1)
			}
			for _, r := range rs {
				p, err := provider.New(r)
				if err != nil {
					check(r.Name, false, err.Error())
					continue
				}
				for _, c := range p.Doctor(cmd.Context()) {
					check(r.Name+": "+c.Name, c.OK, c.Msg)
				}
				info, err := p.Status(cmd.Context())
				if err != nil || info.State != provider.StateRunning {
					msg := string(info.State)
					if err != nil {
						msg = err.Error()
					}
					if r.Ephemeral() && err == nil {
						check(r.Name+": instance", true, msg+" (boots on demand)")
					} else {
						check(r.Name+": reachable", false, msg+" "+info.Detail)
					}
					continue
				}
				start := time.Now()
				_, s, err := client.Connect(cmd.Context(), r, info.Target)
				if err != nil {
					check(r.Name+": helper session", false, err.Error())
					continue
				}
				check(r.Name+": helper session", true, fmt.Sprintf("%s/%s, %d CPUs, helper %s, %s", s.Hello.OS, s.Hello.Arch, s.Hello.CPUs, s.Hello.Version, ui.Dur(time.Since(start))))
				s.Close()
			}
			warm := 0
			for _, r := range cfg.Global.Runners {
				warm += r.MinInstances
			}
			if pid := daemon.Running(); pid != 0 {
				check("pool manager", true, fmt.Sprintf("running (pid %d)", pid))
			} else if warm > 0 {
				check("pool manager", false, "min_instances is set but the daemon isn't running, so nothing is kept warm (`spotexec daemon start`)")
			}
			if !ok {
				return exitWith(1)
			}
			return nil
		},
	}
}

// doctorDeps fails deps.lockfiles that every synced run's dependency install
// would fail on, such as a literal entry missing here (as `spotexec
// supported` does).
func doctorDeps(ctx context.Context, l *loaded, check func(string, bool, string)) {
	d := l.cfg.Project.Deps
	mode := l.cfg.Project.Sync.Mode
	if v := os.Getenv("SPOTEXEC_SYNC"); v != "" {
		mode = v
	}
	if d.Install == "" || mode == "none" {
		return
	}
	if err := client.CheckDeps(ctx, l.cfg.Project, l.ws); err != nil {
		check("deps lockfiles", false, err.Error())
		return
	}
	check("deps lockfiles", true, strings.Join(d.Lockfiles, ", "))
}

// doctorContainer reports the project's container: the image it runs in,
// or, for a project with a toolchain but no container, that AWS VMs (among
// the runners doctor checks, else the project's pool) boot without it.
func doctorContainer(l *loaded, runners []string, check func(string, bool, string), warn func(string, string)) {
	c := l.cfg.Project.Container
	switch {
	case c.Image != "":
		check("container", true, "image "+c.Image)
	case c.Dockerfile != "":
		if _, err := os.Stat(filepath.Join(l.ws.Root, filepath.FromSlash(c.Dockerfile))); err != nil {
			check("container", false, c.Dockerfile+" is missing (`spotexec container init` writes it)")
			return
		}
		check("container", true, c.Dockerfile+" (the first sx builds it; `spotexec container build` builds it now)")
	default:
		_, stack, _, _ := scaffoldDockerfile(l.ws.Root)
		rs, err := l.cfg.Pool(nil)
		if len(runners) > 0 {
			rs, err = selectRunners(l.cfg, runners)
		}
		ephemeral := false
		if err == nil {
			for _, r := range rs {
				ephemeral = ephemeral || r.Ephemeral()
			}
		}
		if stack != "" && ephemeral {
			warn("container", "none, but this is a "+stack+" project and AWS VMs boot without it: run `spotexec container init`, or install it with bootstrap_script")
			return
		}
		check("container", true, "none (commands use the runner's own toolchain)")
	}
}

// remoteHelper runs a helper admin command on each named runner that is up:
// every running instance of a fleet, or one instance named "<runner>/<id>"
// (as `spotexec status` prints them). With single set (unlocking a slot
// number), a fleet with several instances must name the instance.
func remoteHelper(cmd *cobra.Command, names []string, single bool, args ...string) error {
	cfg, err := loadGlobal()
	if err != nil {
		return err
	}
	want := map[string]map[string]bool{} // runner -> instance ids (empty = all)
	allOf := map[string]bool{}
	var runnerNames []string
	for _, n := range names {
		rn, id, hasID := strings.Cut(n, "/")
		if hasID && (id == "" || strings.Contains(id, "/")) {
			return fmt.Errorf("invalid target %q (use <runner> or <runner>/<instance-id>)", n)
		}
		if _, seen := want[rn]; !seen {
			want[rn] = map[string]bool{}
			runnerNames = append(runnerNames, rn)
		}
		if id != "" {
			want[rn][id] = true
		} else {
			allOf[rn] = true // the bare runner name means every instance
		}
	}
	rs, err := selectRunners(cfg, runnerNames)
	if err != nil {
		return err
	}
	var failed []string
	type target struct {
		label string
		t     provider.Target
		r     *config.Runner
	}
	var targets []target
	run := func(label string, t provider.Target, r *config.Runner) {
		c := transport.New(t, r.WorkRoot)
		if err := c.EnsureMaster(cmd.Context()); err != nil {
			fmt.Printf("%s: %v\n", label, err)
			failed = append(failed, label)
			return
		}
		out, err := c.HelperCmd(cmd.Context(), args...)
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				fmt.Printf("%s: %s\n", label, line)
			}
		}
		if err != nil {
			fmt.Printf("%s: %v\n", label, err)
			failed = append(failed, label)
		}
	}
	for _, r := range rs {
		p, err := provider.New(r)
		if err != nil {
			return err
		}
		if f, ok := p.(provider.Fleet); ok {
			insts, err := f.List(cmd.Context())
			if err != nil {
				fmt.Printf("%s: %v\n", r.Name, err)
				failed = append(failed, r.Name)
				continue
			}
			ids := want[r.Name]
			found := map[string]bool{}
			n := 0
			for _, in := range insts {
				if in.State == provider.StateRunning && in.Target.Host != "" && (allOf[r.Name] || len(ids) == 0 || ids[in.ID]) {
					targets = append(targets, target{r.Name + "/" + in.ID, in.Target, r})
					found[in.ID] = true
					n++
				}
			}
			for id := range ids {
				if !found[id] {
					fmt.Printf("%s/%s: not running\n", r.Name, id)
					failed = append(failed, r.Name+"/"+id)
				}
			}
			if n == 0 && len(ids) == 0 {
				fmt.Printf("%s: no running instances, skipped\n", r.Name)
			}
			continue
		}
		if len(want[r.Name]) > 0 {
			return fmt.Errorf("%s is a single machine: name it without an instance id", r.Name)
		}
		info, err := p.Status(cmd.Context())
		if err != nil || info.State != provider.StateRunning {
			fmt.Printf("%s: not running, skipped\n", r.Name)
			continue
		}
		targets = append(targets, target{r.Name, info.Target, r})
	}
	// Unlocking one slot number must name exactly one machine: decide
	// before touching anything.
	if single && len(targets) > 1 {
		var labels []string
		for _, t := range targets {
			labels = append(labels, t.label)
		}
		return fmt.Errorf("slot numbers are per machine and %d are running (%s): name one", len(targets), strings.Join(labels, ", "))
	}
	for _, t := range targets {
		run(t.label, t.t, t.r)
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed on %s", strings.Join(failed, ", "))
	}
	return nil
}

func cmdUnlock() *cobra.Command {
	var slot int
	var all bool
	c := &cobra.Command{
		Use:   "unlock [runner[/instance-id]] (--slot N | --all)",
		Short: "Reclaim a wedged slot (kills its command and helper)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (slot == 0) == !all {
				return errors.New("pass either --slot N or --all")
			}
			a := []string{"unlock"}
			if all {
				a = append(a, "--all")
			} else {
				a = append(a, "--token", fmt.Sprint(slot))
			}
			return remoteHelper(cmd, args, !all, a...)
		},
	}
	c.Flags().IntVar(&slot, "slot", 0, "slot number (as shown by status)")
	c.Flags().BoolVar(&all, "all", false, "every held slot")
	return c
}

func cmdPrune() *cobra.Command {
	var olderThan string
	var dry bool
	c := &cobra.Command{
		Use:   "prune [runner[/instance-id]...]",
		Args:  cobra.ArbitraryArgs,
		Short: "Remove old dependency caches and idle slot workspaces on runners",
		RunE: func(cmd *cobra.Command, args []string) error {
			age, err := parseAge(olderThan)
			if err != nil {
				return err
			}
			a := []string{"prune", "--older-than", age.String()}
			if dry {
				a = append(a, "--dry-run")
			}
			return remoteHelper(cmd, args, false, a...)
		},
	}
	c.Flags().StringVar(&olderThan, "older-than", "7d", "remove items unused for this long (e.g. 7d, 36h)")
	c.Flags().BoolVar(&dry, "dry-run", false, "only print what would be removed")
	return c
}

func cmdSync() *cobra.Command {
	var f runFlags
	c := &cobra.Command{
		Use:   "sync",
		Args:  noArgs,
		Short: "Pre-sync the worktree (and dependencies) to a slot without running anything",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(f.dir)
			if err != nil {
				return err
			}
			return exitWith(f.execute(l, runSpec{display: "(sync)", syncOnly: true, sync: "in"}))
		},
	}
	f.register(c.Flags())
	return c
}

func cmdSyncPlan() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "sync-plan",
		Args:  noArgs,
		Short: "Dry-run: show what would be synced (file count, bytes, largest files, exclusions, dependency key files)",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load("")
			if err != nil {
				return err
			}
			p := l.cfg.Project
			t := time.Now()
			m, err := manifest.Build(cmd.Context(), client.ManifestOptions(l.ws.Root, p))
			if err != nil {
				return err
			}
			var lockfiles, unmatched []string
			if p.Deps.Install != "" {
				if lockfiles, unmatched, err = client.DepsLockfiles(m, p.Deps.Lockfiles); err != nil {
					return err
				}
			}
			if asJSON {
				out := map[string]any{
					"root": m.Root, "git": m.Git, "files": len(m.Entries), "bytes": m.Bytes,
					"fingerprint": m.Fingerprint, "excluded": m.Excluded, "repo_id": l.ws.RepoID,
				}
				if p.Deps.Install != "" {
					out["deps_lockfiles"], out["deps_unmatched"] = lockfiles, unmatched
				}
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			fmt.Printf("root:         %s (%s)\n", m.Root, map[bool]string{true: "git working set", false: "directory walk"}[m.Git])
			fmt.Printf("repo id:      %s\n", l.ws.RepoID)
			fmt.Printf("files:        %d (%s)\n", len(m.Entries), ui.Bytes(m.Bytes))
			fmt.Printf("fingerprint:  %s\n", m.Fingerprint)
			fmt.Printf("scan time:    %s\n", ui.Dur(time.Since(t)))
			if len(m.Excluded) > 0 {
				var parts []string
				for k, v := range m.Excluded {
					parts = append(parts, fmt.Sprintf("%s=%d", k, v))
				}
				sort.Strings(parts)
				fmt.Printf("excluded:     %s\n", strings.Join(parts, ", "))
			}
			ents := append([]struct {
				p string
				s int64
			}{}, nil...)
			for _, e := range m.Entries {
				ents = append(ents, struct {
					p string
					s int64
				}{e.Path, e.Size})
			}
			sort.Slice(ents, func(i, j int) bool { return ents[i].s > ents[j].s })
			if len(ents) > 0 {
				fmt.Println("largest:")
				for i := 0; i < len(ents) && i < 10; i++ {
					fmt.Printf("  %10s  %s\n", ui.Bytes(ents[i].s), ents[i].p)
				}
			}
			if p.Deps.Install != "" {
				fmt.Printf("deps key:     %d lockfiles\n", len(lockfiles))
				for _, f := range lockfiles {
					fmt.Printf("  %s\n", f)
				}
				for _, pat := range unmatched {
					fmt.Printf("  ! %s matches no synced file\n", pat)
				}
			}
			switch {
			case len(m.Entries) > client.AbortFiles || m.Bytes > client.AbortBytes:
				fmt.Println("\n✗ exceeds the large-sync limit; add [sync].exclude rules or use --force-large-sync")
			case len(m.Entries) > client.WarnFiles || m.Bytes > client.WarnBytes:
				fmt.Println("\n! large sync; consider [sync].exclude rules")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

// pullMode picks pull's output mode: --json (either way) beats
// SPOTEXEC_JSON, read as a run reads it, and --json=false beats
// SPOTEXEC_AGENT=1.
func pullMode(asJSON, jsonSet bool) (ui.Mode, error) {
	if jsonSet {
		if asJSON {
			return ui.ModeJSON, nil
		}
		if os.Getenv("SPOTEXEC_AGENT") == "1" {
			return ui.ModePlain, nil
		}
		return ui.ModeAuto, nil
	}
	envJSON, err := envBool("SPOTEXEC_JSON")
	if err != nil {
		return ui.ModeAuto, err
	}
	if envJSON {
		return ui.ModeJSON, nil
	}
	return ui.ModeAuto, nil
}

func cmdPull() *cobra.Command {
	var dest string
	var held, asJSON bool
	c := &cobra.Command{
		Use:   "pull <remote-glob>... | --held [<glob>...]",
		Short: "Pull gitignored artifacts (e.g. coverage/), or held write-back files, from the last-used slot",
		Args: func(cmd *cobra.Command, args []string) error {
			if held {
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := pullMode(asJSON, cmd.Flags().Changed("json"))
			if err != nil {
				return err
			}
			l, err := load("")
			if err != nil {
				return err
			}
			lr, err := client.LoadLastRun(l.ws)
			if err != nil {
				return errors.New("no previous run from this worktree; run something first")
			}
			heldFiles := client.LoadHeld(l.ws, *lr)
			if held {
				if args, heldFiles, err = client.HeldPatterns(heldFiles, args); err != nil {
					return err
				}
			}
			r, err := l.cfg.Runner(lr.Runner)
			if err != nil {
				return err
			}
			ctx, _, stop := signals()
			defer stop()
			rep, _ := ui.New(mode, os.Stderr, "")
			host, _ := os.Hostname()
			defer client.KeepAwake()() // released after the lease
			lease, err := client.AcquireSlot(ctx, l.cfg, l.ws, r, lr, rep, host, client.PullDisplay, false, 0, false, "")
			if err != nil {
				return err
			}
			defer lease.Close()
			root := l.ws.Root
			if dest != "" {
				root = dest // created by PullArtifacts once validated
			}
			n, err := client.PullArtifacts(ctx, lease.Sess, l.ws, root, args, heldFiles, held, rep, "")
			if err != nil {
				return err
			}
			rep.Event("pull", map[string]any{"patterns": args, "files": n})
			rep.Done(fmt.Sprintf("pulled %d file%s from %s", n, plural(n), r.Name), true)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "NDJSON events on stderr (warnings, links pulled out of the worktree, the pull)")
	c.Flags().StringVar(&dest, "dest", "", "write under this directory instead of the worktree root")
	c.Flags().BoolVar(&held, "held", false, "pull the new files the last run's write-back held in the slot (over the write-back limit); globs narrow them")
	return c
}

func cmdCloudInit() *cobra.Command {
	return &cobra.Command{
		Use:    "cloud-init [runner]",
		Short:  "Print the first-boot user-data (idle watchdog + key) for an ephemeral runner",
		Hidden: false,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			var r *config.Runner
			if len(args) == 1 {
				r, err = cfg.Runner(args[0])
			} else {
				r, err = firstEphemeral(cfg)
			}
			if err != nil {
				return err
			}
			pub, err := provider.EnsureKey()
			if err != nil {
				return err
			}
			user := r.User
			if user == "" {
				user = "ubuntu"
			}
			s, err := cloudinit.Render(cloudinit.Options{User: user, PublicKey: pub, IdleTimeout: r.IdleTimeout.Duration, MaxLifetime: r.MaxLifetime.Duration, BootstrapScript: r.BootstrapScript, UserBootstrap: r.UserBootstrap})
			if err != nil {
				return err
			}
			fmt.Print(s)
			return nil
		},
	}
}

// parseAge is time.ParseDuration plus a days unit ("7d").
func parseAge(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var d int
		if _, err := fmt.Sscanf(n, "%d", &d); err != nil || d < 0 || d > 36500 || fmt.Sprint(d) != n {
			return 0, fmt.Errorf("invalid age %q", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (e.g. 7d or 36h)", s)
	}
	return d, nil
}

// The account-wide discovery and termination (tests replace them).
var (
	findAccountInstances     = provider.FindAccountInstances
	terminateAccountInstance = provider.TerminateAccountInstance
	enabledRegions           = provider.EnabledRegions
)

// searchAccount finds every spotexec VM in the scopes the AWS runners use;
// a region list that couldn't be fetched is reported as a failed scope.
func searchAccount(ctx context.Context, cfg *config.Config) *provider.AccountSearch {
	scopes, failed := accountScopes(ctx, cfg)
	res := findAccountInstances(ctx, scopes)
	res.Failed = append(res.Failed, failed...)
	return res
}

// accountScopes is every (region, profile) the AWS runners use, including
// every region a multi-region runner launches in or may still hold VMs in:
// "the account" for status -A and down -A means these.
func accountScopes(ctx context.Context, cfg *config.Config) ([]provider.AccountScope, []provider.AccountScopeError) {
	var out []provider.AccountScope
	var failed []provider.AccountScopeError
	seen := map[provider.AccountScope]bool{}
	for _, r := range cfg.Global.Runners {
		if r.Type != config.TypeAWSSpot {
			continue
		}
		// Every region the runner launches in or may still hold VMs in:
		// its region, its list of regions (or, for "all", every region
		// enabled in the account), and any its placement state records.
		regions := append([]string{r.Region}, r.Regions...)
		if r.MultiRegion() {
			all, err := enabledRegions(ctx, r.Profile, r.Region, r.Regions...)
			switch {
			case err != nil && r.AnyRegion():
				failed = append(failed, provider.AccountScopeError{
					AccountScope: provider.AccountScope{Region: config.AllRegions, Profile: r.Profile},
					Err:          fmt.Sprintf("runner %s uses every region, but the account's regions couldn't be listed: %v", r.Name, err),
				})
			case err == nil && r.AnyRegion():
				regions = all
			case err == nil:
				// Listed regions the account hasn't enabled can't hold VMs
				// (and some never answer).
				regions = provider.FilterEnabled(r.Regions, all)
			}
		}
		regions = append(regions, provider.KnownRegions(r)...)
		for _, rg := range regions {
			sc := provider.AccountScope{Region: rg, Profile: r.Profile}
			if rg == "" || rg == config.AllRegions || seen[sc] {
				continue
			}
			seen[sc] = true
			out = append(out, sc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].Profile < out[j].Profile
	})
	return out, failed
}

// printSearched says where the search looked, and what it couldn't list.
func printSearched(res *provider.AccountSearch) {
	if len(res.Searched) == 0 && len(res.Failed) == 0 {
		fmt.Println("no AWS runners configured, so no account to search")
		return
	}
	// One group per account and profile: a fleet with regions = ["all"]
	// is searched in every region with the same identity.
	type ident struct{ account, profile string }
	var order []ident
	regions := map[ident][]string{}
	for _, sc := range res.Searched {
		id := ident{sc.Account, sc.Profile}
		if _, ok := regions[id]; !ok {
			order = append(order, id)
		}
		regions[id] = append(regions[id], sc.Region)
	}
	var where []string
	for _, id := range order {
		label := provider.AccountScope{Region: strings.Join(regions[id], ", "), Account: id.account, Profile: id.profile}.String()
		where = append(where, label)
	}
	if len(where) > 0 {
		fmt.Printf("searched %s (the regions your AWS runners use)\n", strings.Join(where, "; "))
	}
	// Regions that couldn't be listed come back as the command's error.
}

func machineName(in provider.AccountInstance) string {
	h := in.Host
	if h == "" {
		h = "owner " + strings.Split(in.Owner, "-")[0]
	}
	if in.Mine {
		h += " (this)"
	}
	return h
}

func printAccountInstances(ins []provider.AccountInstance) {
	if len(ins) == 0 {
		fmt.Println("no spotexec VMs in the account")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tRUNNER\tINSTANCE\tTYPE\tMARKET\tSTATE\tUP\tREGION\tACCOUNT")
	per := map[string]int{}
	for _, in := range ins {
		per[machineName(in)]++
		runner := in.Runner
		if in.Builder {
			runner += " (bake builder)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", machineName(in), runner, in.ID, in.Type, in.Market, in.State, ui.Dur(time.Since(in.Launched).Round(time.Minute)), in.Region, in.Account)
	}
	tw.Flush()
	fmt.Printf("\n%d VM(s) across %d machine(s). Stop one machine's VMs: spotexec down -A --machine <name>\n", len(ins), len(per))
}

// downAccount terminates spotexec VMs (fleet VMs and bake builders, in any
// state) launched by any machine (or by one), each through the profile that
// found it. It fails when any region couldn't be listed or any VM
// terminated, so "done" means done.
func downAccount(ctx context.Context, cfg *config.Config, machine string, yes bool) error {
	res := searchAccount(ctx, cfg)
	short := strings.Split(machine, ".")[0]
	var pick []provider.AccountInstance
	for _, in := range res.Instances {
		if in.State == "shutting-down" {
			continue // already terminating
		}
		if machine == "" || strings.EqualFold(in.Host, machine) || strings.EqualFold(in.Host, short) || strings.HasPrefix(in.Owner, machine) {
			pick = append(pick, in)
		}
	}
	if len(pick) == 0 {
		fmt.Println("no matching spotexec VMs")
		printSearched(res)
		return res.Err()
	}
	printSearched(res)
	others := 0
	for _, in := range pick {
		if !in.Mine {
			others++
		}
	}
	if others > 0 && !yes {
		if !ui.IsTerminal(os.Stdin) {
			if others == 1 {
				return errors.New("one of these VMs belongs to another machine; re-run with --yes to terminate it")
			}
			return fmt.Errorf("%d of these VMs belong to other machines; re-run with --yes to terminate them", others)
		}
		printAccountInstances(pick)
		fmt.Printf("Terminate these %d VM(s), %d of them other machines'? Commands running on them fail. [y/N] ", len(pick), others)
		var a string
		fmt.Scanln(&a)
		if a = strings.ToLower(strings.TrimSpace(a)); a != "y" && a != "yes" {
			return exitWith(1)
		}
	}
	var errs []string
	for _, in := range pick {
		if err := terminateAccountInstance(ctx, in); err != nil {
			errs = append(errs, fmt.Sprintf("couldn't terminate %s in %s: %v", in.ID, in.Region, err))
			continue
		}
		fmt.Printf("terminated %s (%s, %s, %s)\n", in.ID, machineName(in), in.Runner, in.Region)
	}
	if err := res.Err(); err != nil {
		errs = append(errs, err.Error()+" (VMs there were not terminated)")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
