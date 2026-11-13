package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/spotexec/spotexec/internal/client"
	"github.com/spotexec/spotexec/internal/cloudinit"
	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/helper"
	"github.com/spotexec/spotexec/internal/helperbin"
	"github.com/spotexec/spotexec/internal/manifest"
	"github.com/spotexec/spotexec/internal/provider"
	"github.com/spotexec/spotexec/internal/transport"
	"github.com/spotexec/spotexec/internal/ui"
)

func loadGlobal() (*config.Config, error) {
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
		return nil, fmt.Errorf("no runners configured in %s (run `spotexec init --global` for a starting point)", cfg.Global.Path)
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
	Fleet  string         `json:"fleet,omitempty"`
	r      *config.Runner
}

func gatherStatus(ctx context.Context, rs []*config.Runner) []runnerStatus {
	var rows []runnerStatus
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
			if len(insts) == 0 {
				rows = append(rows, runnerStatus{Name: r.Name, Type: r.Type, r: r, Fleet: r.Name,
					Info: provider.Info{State: provider.StateAbsent, Detail: fmt.Sprintf("0 of max %d instances; scales on demand", r.MaxInstances)}})
			}
			for _, in := range insts {
				rows = append(rows, runnerStatus{Name: r.Name + "/" + in.ID, Type: r.Type, Cost: r.HourlyCost, r: r, Fleet: r.Name,
					Info: provider.Info{State: in.State, Target: in.Target, InstanceID: in.ID, InstanceType: in.Type, LaunchedAt: in.LaunchedAt, Spot: in.Spot}})
			}
			continue
		}
		info, err := p.Status(ctx)
		st := runnerStatus{Name: r.Name, Type: r.Type, Cost: r.HourlyCost, Info: info, r: r}
		if err != nil {
			st.Error = err.Error()
		}
		rows = append(rows, st)
	}
	var wg sync.WaitGroup
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
		}()
	}
	wg.Wait()
	return rows
}

func printStatus(w *os.File, sts []runnerStatus, cfg *config.Config) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNNER\tTYPE\tSTATE\tHOST\tSLOTS\tLOAD\tMEM FREE\tIDLE")
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
			max := s.r.MaxSlots
			if max == 0 {
				max = h.CPUs / 4
				if max < 1 {
					max = 1
				}
			}
			slots = fmt.Sprintf("%d/%d", len(h.Holders), max)
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Type, state, host, slots, load, mem, idle)
	}
	tw.Flush()
	for _, s := range sts {
		if s.Error != "" {
			fmt.Fprintf(w, "  %s: %s\n", s.Name, s.Error)
		}
		if s.Helper == nil {
			continue
		}
		for _, h := range s.Helper.Holders {
			age := time.Since(time.UnixMilli(h.StartedAt)).Round(time.Second)
			fmt.Fprintf(w, "  %s slot %d: %s  (%s, %s pid %d, %s)\n", s.Name, h.Token, h.Client.Command, age, h.Client.Host, h.Client.PID, h.Client.Worktree)
		}
	}
}

func cmdStatus() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status [runner...]",
		Short: "Show runner states, active slots, lock holders and idle timers",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			rs, err := selectRunners(cfg, args)
			if err != nil {
				return err
			}
			sts := gatherStatus(cmd.Context(), rs)
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(sts)
			}
			printStatus(os.Stdout, sts, cfg)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

func cmdMonitor() *cobra.Command {
	var interval time.Duration
	c := &cobra.Command{
		Use:   "monitor [runner...]",
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
			for {
				sts := gatherStatus(ctx, rs)
				fmt.Print("\x1b[H\x1b[2J")
				fmt.Printf("spotexec monitor · %s · every %s · Ctrl-C to quit\n\n", time.Now().Format("15:04:05"), interval)
				printStatus(os.Stdout, sts, cfg)
				burn := 0.0
				for _, s := range sts {
					if s.Info.State == provider.StateRunning && s.r.Ephemeral() {
						burn += s.Cost
					}
				}
				if burn > 0 {
					fmt.Printf("\nestimated burn: $%.3f/hour (from hourly_cost)\n", burn)
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

func firstEphemeral(cfg *config.Config) (*config.Runner, error) {
	rs, err := selectRunners(cfg, cfg.Global.DefaultPool)
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
			// max_instances and concurrent sx processes see the launches.
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
			for _, id := range ids {
				go func() {
					in, err := pool.Provision(ctx, id, func(f string, a ...any) {
						rep.Event("provision", map[string]any{"runner": r.Name, "message": fmt.Sprintf(f, a...)})
					})
					if err == nil {
						err = client.WaitReady(ctx, r, in.Target, rep, r.BootTimeout.Duration)
					}
					if err == nil {
						_, err = transport.New(in.Target, r.WorkRoot).EnsureHelper(ctx, false)
					}
					ch <- res{in, err}
				}()
			}
			var errs []string
			for i := 0; i < count; i++ {
				x := <-ch
				if x.err != nil {
					errs = append(errs, x.err.Error())
					continue
				}
				rep.Event("instance_ready", map[string]any{"runner": r.Name, "instance_id": x.in.ID, "host": x.in.Target.String()})
			}
			if len(errs) > 0 {
				rep.Clear()
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
	var all bool
	c := &cobra.Command{
		Use:   "down [runner...]",
		Short: "Terminate ephemeral runner(s) now",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
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
				fmt.Printf("%s: terminated\n", r.Name)
			}
			if len(errs) > 0 {
				return errors.New(strings.Join(errs, "; "))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "terminate every ephemeral runner")
	return c
}

func cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [runner...]",
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
			if l, err := load(""); err == nil {
				src := "(defaults; no .spotexec.toml)"
				if l.cfg.Project.Path != "" {
					src = l.cfg.Project.Path
				}
				check("project config", true, src)
				check("workspace", true, fmt.Sprintf("%s (repo id %s)", l.ws.Root, l.ws.RepoID))
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
			if !ok {
				return exitWith(1)
			}
			return nil
		},
	}
}

// remoteHelper runs a helper admin command on each named runner that is up.
func remoteHelper(cmd *cobra.Command, names []string, args ...string) error {
	cfg, err := loadGlobal()
	if err != nil {
		return err
	}
	rs, err := selectRunners(cfg, names)
	if err != nil {
		return err
	}
	for _, r := range rs {
		p, err := provider.New(r)
		if err != nil {
			return err
		}
		info, err := p.Status(cmd.Context())
		if err != nil || info.State != provider.StateRunning {
			fmt.Printf("%s: not running, skipped\n", r.Name)
			continue
		}
		c := transport.New(info.Target, r.WorkRoot)
		if err := c.EnsureMaster(cmd.Context()); err != nil {
			fmt.Printf("%s: %v\n", r.Name, err)
			continue
		}
		out, err := c.HelperCmd(cmd.Context(), args...)
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				fmt.Printf("%s: %s\n", r.Name, line)
			}
		}
		if err != nil {
			fmt.Printf("%s: %v\n", r.Name, err)
		}
	}
	return nil
}

func cmdUnlock() *cobra.Command {
	var slot int
	var all bool
	c := &cobra.Command{
		Use:   "unlock [runner] (--slot N | --all)",
		Short: "Reclaim a wedged slot (kills its command and helper)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if slot == 0 && !all {
				return errors.New("pass --slot N or --all")
			}
			a := []string{"unlock"}
			if all {
				a = append(a, "--all")
			} else {
				a = append(a, "--token", fmt.Sprint(slot))
			}
			return remoteHelper(cmd, args, a...)
		},
	}
	c.Flags().IntVar(&slot, "slot", 0, "slot number (as shown by status)")
	c.Flags().BoolVar(&all, "all", false, "every held slot")
	return c
}

func cmdPrune() *cobra.Command {
	var olderThan time.Duration
	var dry bool
	c := &cobra.Command{
		Use:   "prune [runner...]",
		Short: "Remove old dependency caches and idle slot workspaces on runners",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := []string{"prune", "--older-than", olderThan.String()}
			if dry {
				a = append(a, "--dry-run")
			}
			return remoteHelper(cmd, args, a...)
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 7*24*time.Hour, "remove items unused for this long")
	c.Flags().BoolVar(&dry, "dry-run", false, "only print what would be removed")
	return c
}

func cmdSync() *cobra.Command {
	var f runFlags
	c := &cobra.Command{
		Use:   "sync",
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
		Short: "Dry-run: show what would be synced (file count, bytes, largest files, exclusions)",
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load("")
			if err != nil {
				return err
			}
			p := l.cfg.Project
			t := time.Now()
			m, err := manifest.Build(cmd.Context(), manifest.Options{Root: l.ws.Root, Exclude: p.Sync.Exclude, Source: p.Sync.Source, Protect: p.Deps.CacheDirs})
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"root": m.Root, "git": m.Git, "files": len(m.Entries), "bytes": m.Bytes,
					"fingerprint": m.Fingerprint, "excluded": m.Excluded, "repo_id": l.ws.RepoID,
				})
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

func cmdPull() *cobra.Command {
	var dest string
	c := &cobra.Command{
		Use:   "pull <remote-glob>... ",
		Short: "Pull gitignored artifacts (e.g. coverage/) from the last-used slot",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load("")
			if err != nil {
				return err
			}
			lr, err := client.LoadLastRun(l.ws)
			if err != nil {
				return errors.New("no previous run from this worktree; run something first")
			}
			r, err := l.cfg.Runner(lr.Runner)
			if err != nil {
				return err
			}
			ctx, _, stop := signals()
			defer stop()
			rep, _ := ui.New(ui.ModeAuto, os.Stderr, "")
			host, _ := os.Hostname()
			lease, err := client.AcquireSlot(ctx, l.cfg, l.ws, r, lr, rep, host, "(pull)", false, 0)
			if err != nil {
				return err
			}
			defer lease.Close()
			root := l.ws.Root
			if dest != "" {
				root = dest
				if err := os.MkdirAll(root, 0o755); err != nil {
					return err
				}
			}
			n, err := client.PullArtifacts(ctx, lease.Sess, root, args)
			if err != nil {
				return err
			}
			rep.Done(fmt.Sprintf("pulled %d file%s from %s", n, plural(n), r.Name), true)
			return nil
		},
	}
	c.Flags().StringVar(&dest, "dest", "", "write under this directory instead of the worktree root")
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
