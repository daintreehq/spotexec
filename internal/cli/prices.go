package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

// cmdPrices shows where an aws-spot runner's next instance would launch:
// the current Spot price of each instance type in each zone of its regions,
// in launch order.
func cmdPrices() *cobra.Command {
	var asJSON, refresh bool
	var limit int
	c := &cobra.Command{
		Use:   "prices [runner]",
		Short: "Show current Spot prices across an AWS runner's regions, in launch order (where the next VM launches first), with what the ranking added",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var r *config.Runner
			if len(args) > 0 {
				r, err = cfg.Runner(args[0])
			} else {
				r, err = firstEphemeral(cfg)
			}
			if err != nil {
				return err
			}
			if r.Type != config.TypeAWSSpot {
				return fmt.Errorf("runner %s is not an aws-spot runner", r.Name)
			}
			g, err := provider.NewAWSPricer(r)
			if err != nil {
				return err
			}
			// In a project, regions are checked for its own caches.
			project := ""
			if pj, name := pricesProject(ctx, r); pj != nil {
				ctx, project = provider.WithProject(ctx, pj), name
			}
			t, err := g.Prices(ctx, refresh)
			if err != nil && len(t.Prices) == 0 {
				return err
			}
			k := g.Rank(t)
			v := pricesView{r: r, t: t, k: k, cool: g.Cooldowns(t.Prices), held: g.HeldBack(t.Prices), project: project}
			v.unreach, _ = g.RegionHolds()
			v.next, v.hasNext = g.NextLaunch(t)
			if asJSON {
				return v.writeJSON(os.Stdout)
			}
			v.writeText(os.Stdout, limit)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVar(&refresh, "refresh", false, "fetch fresh prices instead of using the 15-minute cache")
	c.Flags().IntVar(&limit, "limit", 20, "rows to show (0: all)")
	return c
}

// pricesProject is the project in the current directory that a launch of
// r from here would check regions for, and its name (nil outside one, or
// when r doesn't launch across regions).
func pricesProject(ctx context.Context, r *config.Runner) (*provider.Project, string) {
	if !r.MultiRegion() || r.LaunchOrder != config.LaunchRunCost {
		return nil, "" // nothing weighs it
	}
	l, err := load("")
	if err != nil || l.cfg.Project == nil || !l.ws.Marked {
		return nil, ""
	}
	p := l.cfg.Project
	if p.Sync.Mode == "none" {
		return nil, "" // a run syncs nothing, so its launches check nothing
	}
	var m *manifest.Manifest
	if client.PlacementNeedsManifest(p) {
		if m, err = manifest.Build(ctx, client.ManifestOptions(l.ws.Root, p)); err != nil {
			return nil, ""
		}
	}
	pj := client.PlacementProject(p, l.ws, m, false, r)
	if pj == nil {
		return nil, ""
	}
	return pj, l.ws.RepoName
}

// pricesView is what `spotexec prices` shows.
type pricesView struct {
	project string // the project whose caches were checked ("": none)
	r       *config.Runner
	t       provider.PriceTable
	k       provider.Ranking // the options in launch order
	cool    map[string]string
	held    map[string]string              // held back after a Spot interruption
	unreach map[string]provider.RegionHold // regions this client can't reach: no launches there for now
	next    provider.SpotPrice
	hasNext bool
}

func optionKey(sp provider.SpotPrice) string { return sp.Region + "/" + sp.Zone + "/" + sp.Type }

func (v pricesView) writeJSON(w io.Writer) error {
	type row struct {
		provider.SpotPrice
		MemoryMiB int    `json:"memory_mib,omitempty"`
		Next      bool   `json:"next,omitempty"`
		Skipped   string `json:"skipped,omitempty"`
		HeldBack  string `json:"held_back,omitempty"`
		// RunCost: the option's score and what each factor added (USD for
		// an hour of work; launch_order = "run_cost").
		RunCost *provider.RunCost `json:"run_cost,omitempty"`
	}
	maxPrice := v.r.MaxPriceUSD()
	rows := make([]row, 0, len(v.k.Prices))
	for _, sp := range v.k.Prices {
		why := v.cool[optionKey(sp)]
		if why == "" && maxPrice > 0 && sp.USD() > maxPrice {
			why = "above max_price"
		}
		rw := row{SpotPrice: sp, MemoryMiB: v.t.MemoryMiB[sp.Type], Next: v.hasNext && sp == v.next, Skipped: why, HeldBack: v.held[optionKey(sp)]}
		if c, ok := v.k.CostOf(sp); ok {
			rw.RunCost = &c
		}
		rows = append(rows, rw)
	}
	out := map[string]any{"runner": v.r.Name, "fetched_at": v.t.At, "regions": v.t.Regions, "unpriced": v.t.Unpriced,
		"launch_order": v.k.Order, "prices": rows}
	if v.k.Model != nil {
		out["run_cost_model"] = v.k.Model
	}
	if len(v.unreach) > 0 {
		out["launch_holds"] = v.unreach
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// added formats what a factor added to an option's score ("-": nothing).
func added(usd float64) string {
	if usd < 0.0005 {
		return "-"
	}
	return fmt.Sprintf("+%.3f", usd)
}

func (v pricesView) writeText(w io.Writer, limit int) {
	r, t, k := v.r, v.t, v.k
	age := "just now"
	if t.Cached {
		age = ui.Dur(time.Since(t.At).Round(time.Second)) + " ago; --refresh to update"
	}
	what := strings.Join(r.InstanceTypes, ", ")
	if r.ByRequirements() {
		what = fmt.Sprintf("the %d instance type(s) with %s", len(t.MemoryMiB), provider.DescribeRequirements(r))
		if len(r.InstanceTypes) > 0 {
			what += " among " + strings.Join(r.InstanceTypes, ", ")
		}
	}
	fmt.Fprintf(w, "%s: Linux Spot prices for %s in %d region(s) (fetched %s)\n", r.Name, what, len(t.Regions), age)
	if r.SubnetID != "" && len(t.Prices) > 0 {
		fmt.Fprintf(w, "subnet_id %s is in %s, so only that zone is used.\n", r.SubnetID, t.Prices[0].Zone)
	}
	maxPrice := r.MaxPriceUSD()
	runCost := k.Order == config.LaunchRunCost
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "\tREGION\tZONE\tTYPE\t"
	if r.ByRequirements() {
		head += "MEMORY\t"
	}
	head += "USD/HOUR\t"
	if runCost {
		head += "RUN COST\tSPEED\tCOLD\tDISTANCE\tFAILURES\t"
	}
	fmt.Fprintln(tw, head)
	for i, sp := range k.Prices {
		if limit > 0 && i == limit {
			fmt.Fprintf(tw, "\t… %d more (--limit 0 for all)\n", len(k.Prices)-limit)
			break
		}
		mark, note := " ", ""
		if _, ok := v.unreach[sp.Region]; ok {
			note = "skipped for now: this client can't reach " + sp.Region + " (below)"
		} else if why := v.cool[optionKey(sp)]; why != "" {
			note = "skipped for now: " + why
		} else if maxPrice > 0 && sp.USD() > maxPrice {
			note = "above max_price " + r.MaxPrice.String()
		} else if v.hasNext && sp == v.next {
			mark, note = "→", "next launch"
			if why := v.held[optionKey(sp)]; why != "" {
				note += " (every other option is skipped; " + why + ")"
			}
		} else if why := v.held[optionKey(sp)]; why != "" {
			note = "held back: " + why
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t", mark, sp.Region, sp.Zone, sp.Type)
		if r.ByRequirements() {
			row += fmt.Sprintf("%g GiB\t", float64(t.MemoryMiB[sp.Type])/1024)
		}
		row += sp.Price + "\t"
		if c, ok := k.CostOf(sp); runCost && ok {
			cold := added(c.Cold)
			switch {
			case c.Warm:
				cold = "warm"
			case c.ColdParts != nil && c.ColdParts.Paid() != "":
				cold += " " + c.ColdParts.Paid()
			}
			speed := added(c.Speed)
			if c.SpeedFrom != "" {
				// How fast against the fastest option, and how it is known.
				how := "assumed"
				if c.SpeedFrom == provider.SpeedMeasured {
					how = "measured"
				}
				speed += fmt.Sprintf(" %.0f%% %s", c.RelSpeed*100, how)
			}
			row += fmt.Sprintf("%.3f\t%s\t%s\t%s\t%s\t", c.Score, speed, cold, added(c.Distance), added(c.Failures))
			if c.FailedHere != nil { // even when too little to show in USD
				failed := "failed here lately: " + c.FailedHere.String()
				if note != "" {
					failed = "; " + failed
				}
				note += failed
			}
		}
		fmt.Fprintln(tw, row+note)
	}
	tw.Flush()
	if len(v.unreach) > 0 {
		var keys []string
		for rg := range v.unreach {
			keys = append(keys, rg)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "\nNo launches for now:\n")
		for _, rg := range keys {
			fmt.Fprintf(w, "  %s: %s\n", rg, v.unreach[rg].Why)
		}
	}
	if len(t.Unpriced) > 0 {
		var keys []string
		for rg := range t.Unpriced {
			keys = append(keys, rg)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "\nNot used:\n")
		for _, rg := range keys {
			fmt.Fprintf(w, "  %s: %s\n", rg, t.Unpriced[rg])
		}
	}
	switch {
	case runCost:
		m := k.Model
		var factors []string
		if m.SpeedByGen {
			factors = append(factors, speedFactor(m))
		}
		if m.Regions {
			project := fmt.Sprintf("outside a project, deps and image count as warm where a VM of %s was up %g minutes or more in the last %g days", r.Name, m.WarmAfterMin, m.WarmDays)
			if v.project != "" {
				project = "deps and image checked for " + v.project
			}
			factors = append(factors, fmt.Sprintf("a cold region (COLD, in minutes: boot %g without a bake of %s there, deps %s without the project's archive in the region's cache, image %s without the project's image in its registry; %s)",
				m.BootMinutes, r.Name, coldMinutes(m.DepsMinutes, m.DepsFrom, "install", r.Name, v.project != ""), coldMinutes(m.ImageMinutes, m.ImageFrom, "image build", r.Name, v.project != ""), project),
				fmt.Sprintf("a farther region (%d round trips at the measured TCP connect time)", m.RoundTrips))
		}
		added := "nothing else differs between these options"
		switch n := len(factors); {
		case n == 1:
			added = "the VM time and waiting that " + factors[0] + " adds"
		case n > 1:
			added = "the VM time and waiting that " + strings.Join(factors[:n-1], ", ") + " or " + factors[n-1] + " adds"
		}
		fmt.Fprintf(w, "\nlaunch_order = \"run_cost\": ranked by the expected cost of an hour of work (RUN COST, USD): its Spot bill, plus %s, with waiting valued at $%g/h (wait_cost). A zone and type that failed lately (refused for capacity, or a VM reclaimed within an hour of launch) adds %g minutes of that for each failure (FAILURES), counting half as much every %g hours. VMs reclaimed in a zone rank last for a while. launch_order = \"cheapest\" ranks by price alone.\n",
			added, m.WaitCost, m.FailureMinutes, m.FailureHalfLifeHours)
	case r.LaunchOrder == config.LaunchListed:
		fmt.Fprintf(w, "\nlaunch_order = \"listed\": instance_types are tried in the listed order, each in its cheapest zone first.\n")
	}
	if !r.MultiRegion() {
		fmt.Fprintf(w, "\n%s uses only %s. To launch in whichever region costs least, set regions = [\"all\"] (or a list) instead of region.\n", r.Name, r.Region)
	}
	fmt.Fprintln(w, "\nPrices are Spot compute only; the disk and public IPv4 address add a little per hour, about the same everywhere.")
}

// speedFactor is the run-cost note's account of speed: the types the
// runner's runs measured, and the generation assumption for the rest.
func speedFactor(m *provider.RunCostModel) string {
	gen := fmt.Sprintf("each instance generation counts as %.0f%% faster per vCPU", (m.GenSpeedup-1)*100)
	if len(m.MeasuredSpeeds) == 0 {
		return fmt.Sprintf("a slower type (SPEED, assumed: %s; measured instead once the same command has run %d times on each of two types)", gen, provider.SpeedMinRuns)
	}
	// Each group of types measured against each other, against its
	// newest generation.
	byGroup := map[string][]string{}
	var groups []string
	for t, sp := range m.MeasuredSpeeds {
		if byGroup[sp.Group] == nil {
			groups = append(groups, sp.Group)
		}
		byGroup[sp.Group] = append(byGroup[sp.Group], t)
	}
	sort.Strings(groups)
	var parts []string
	for _, g := range groups {
		types := byGroup[g]
		sort.Strings(types)
		var ts []string
		for _, t := range types {
			ts = append(ts, fmt.Sprintf("%s %.0f%%", t, m.MeasuredSpeeds[t].Relative*100))
		}
		against := "the group's average"
		if gen := m.MeasuredSpeeds[types[0]].Generation; gen > 0 {
			against = fmt.Sprintf("generation %d", gen)
		}
		parts = append(parts, strings.Join(ts, ", ")+" of "+against)
	}
	capped := ""
	if m.SpeedCap > 0 {
		capped = fmt.Sprintf(", and moving a type's charge at most %g hours of its Spot price from that assumption", m.SpeedCap)
	}
	return fmt.Sprintf("a slower type (SPEED, measured from the same commands' successful runs on each type lately, drawn toward the generation assumption as far as few VMs or scattered runs leave them unsure%s: %s; assumed for other types: %s)", capped, strings.Join(parts, "; "), gen)
}

// coldMinutes is one part of a cold start as the run-cost note shows it:
// its minutes, and whether they come from the project's measured builds or
// installs (what, singular) on runner, or are the default.
func coldMinutes(minutes float64, e provider.ColdEstimate, what, runner string, inProject bool) string {
	m := strconv.FormatFloat(math.Round(minutes*10)/10, 'f', -1, 64)
	if e.Source != "measured" {
		if inProject {
			return fmt.Sprintf("%s (a default: no %s of the project on %s measured lately)", m, what, runner)
		}
		return m + " (a default)"
	}
	from := fmt.Sprintf("the project's longest %s on %s lately", what, runner)
	if !e.At.IsZero() {
		from += ", " + e.At.Local().Format("Jan 2")
	}
	if e.Capped {
		return fmt.Sprintf("%s (capped: %s, took longer)", m, from)
	}
	return fmt.Sprintf("%s (%s)", m, from)
}
