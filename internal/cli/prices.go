package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

// cmdPrices shows where an aws-spot runner's next instance would launch:
// the current Spot price of each instance type in each zone of its regions.
func cmdPrices() *cobra.Command {
	var asJSON, refresh bool
	var limit int
	c := &cobra.Command{
		Use:   "prices [runner]",
		Short: "Show current Spot prices across an AWS runner's regions, cheapest (where the next VM launches) first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGlobal()
			if err != nil {
				return err
			}
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
			rc := *r
			if !rc.MultiRegion() {
				rc.Regions = []string{r.Region} // price its one region the same way
			}
			p, err := provider.NewAWSGlobal(&rc)
			if err != nil {
				return err
			}
			g := p.(*provider.AWSGlobal)
			t, err := g.Prices(cmd.Context(), refresh)
			if err != nil && len(t.Prices) == 0 {
				return err
			}
			cool := g.Cooldowns(t.Prices)
			maxPrice, _ := strconv.ParseFloat(r.MaxPrice, 64)
			if asJSON {
				type row struct {
					provider.SpotPrice
					Skipped string `json:"skipped,omitempty"`
				}
				rows := make([]row, 0, len(t.Prices))
				for _, sp := range t.Prices {
					why := cool[sp.Region+"/"+sp.Zone+"/"+sp.Type]
					if why == "" && maxPrice > 0 && sp.USD() > maxPrice {
						why = "above max_price"
					}
					rows = append(rows, row{sp, why})
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"runner": r.Name, "fetched_at": t.At, "regions": t.Regions, "unpriced": t.Unpriced, "prices": rows})
			}
			age := "just now"
			if t.Cached {
				age = ui.Dur(time.Since(t.At).Round(time.Second)) + " ago; --refresh to update"
			}
			fmt.Printf("%s: Linux Spot prices for %s in %d region(s) (fetched %s)\n", r.Name, strings.Join(r.InstanceTypes, ", "), len(t.Regions), age)
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "\tREGION\tZONE\tTYPE\tUSD/HOUR\t")
			next := r.MultiRegion() // a single-region runner tries instance_types in order instead
			for i, sp := range t.Prices {
				if limit > 0 && i == limit {
					fmt.Fprintf(tw, "\t… %d more (--limit 0 for all)\t\t\t\t\n", len(t.Prices)-limit)
					break
				}
				mark, note := " ", ""
				if why := cool[sp.Region+"/"+sp.Zone+"/"+sp.Type]; why != "" {
					note = "skipped for now: " + why
				} else if maxPrice > 0 && sp.USD() > maxPrice {
					note = "above max_price " + r.MaxPrice
				} else if next {
					mark, note, next = "→", "next launch", false
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", mark, sp.Region, sp.Zone, sp.Type, sp.Price, note)
			}
			tw.Flush()
			if len(t.Unpriced) > 0 {
				var keys []string
				for k := range t.Unpriced {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				fmt.Printf("\nNot used (no price):\n")
				for _, k := range keys {
					fmt.Printf("  %s: %s\n", k, t.Unpriced[k])
				}
			}
			if !r.MultiRegion() {
				fmt.Printf("\n%s uses only %s and tries instance_types in order. To launch wherever Spot is cheapest, set regions = [\"all\"] (or a list) instead of region.\n", r.Name, r.Region)
			}
			fmt.Println("\nPrices are Spot compute only; the disk and public IPv4 address add a little per hour, about the same everywhere.")
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVar(&refresh, "refresh", false, "fetch fresh prices instead of using the 15-minute cache")
	c.Flags().IntVar(&limit, "limit", 20, "rows to show (0: all)")
	return c
}
