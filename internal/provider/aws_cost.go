package provider

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// priceRetry is the least time between two lookups of one type in one
// region, so a lookup that fails, or finds no price for some zone, isn't
// repeated on every refresh.
const priceRetry = time.Minute

// InstancePricer prices a fleet's running VMs at the current Spot price of
// each one's instance type in its own zone, for burn estimates. A fleet can
// mix types (the cheapest listed type wins each launch, and vcpus runners
// take any qualifying type), so no single figure describes it.
//
// Quotes are kept for priceTTL, and only the regions holding VMs are asked,
// so a dashboard refreshing every few seconds costs one
// DescribeSpotPriceHistory per region every 15 minutes. Pricing never
// touches the placement state: it changes nothing about where VMs launch.
type InstancePricer struct {
	g     *AWSGlobal
	mu    sync.Mutex
	quote map[string]priceQuote // region/zone/type -> price
	asked map[string]time.Time  // region/type -> when it was last looked up
}

type priceQuote struct {
	usd float64
	at  time.Time
}

// NewInstancePricer returns a pricer for an aws-spot runner's VMs.
func NewInstancePricer(r *config.Runner) (*InstancePricer, error) {
	g, err := NewAWSPricer(r)
	if err != nil {
		return nil, err
	}
	return newInstancePricer(g), nil
}

func newInstancePricer(g *AWSGlobal) *InstancePricer {
	return &InstancePricer{g: g, quote: map[string]priceQuote{}, asked: map[string]time.Time{}}
}

// Prices returns the hourly Spot price in USD of each instance it can
// price, by instance ID. On-demand VMs (on_demand_fallback), instances
// missing their zone or type, and those whose price can't be read are left
// out: the caller decides what an unpriced VM costs.
func (p *InstancePricer) Prices(ctx context.Context, insts []Instance) map[string]float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.g.clock()
	need := map[string][]string{} // region -> types to look up
	for _, in := range insts {
		if !priceable(in) {
			continue
		}
		if q, ok := p.quote[in.Region+"/"+in.Zone+"/"+in.Type]; ok && now.Sub(q.at) < priceTTL {
			continue
		}
		if t, ok := p.asked[in.Region+"/"+in.Type]; ok && now.Sub(t) < priceRetry {
			continue
		}
		if !contains(need[in.Region], in.Type) {
			need[in.Region] = append(need[in.Region], in.Type)
		}
	}
	if len(need) > 0 {
		regions := make([]string, 0, len(need))
		for rg := range need {
			sort.Strings(need[rg])
			regions = append(regions, rg)
		}
		sort.Strings(regions)
		var mu sync.Mutex
		_ = eachRegion(ctx, regions, func(ctx context.Context, rg string) error {
			ps, err := p.g.Regional(rg).spotPrices(ctx, need[rg], now)
			mu.Lock()
			defer mu.Unlock()
			for _, t := range need[rg] {
				p.asked[rg+"/"+t] = now
			}
			for _, sp := range ps {
				if usd := sp.USD(); usd > 0 && contains(need[rg], sp.Type) {
					p.quote[rg+"/"+sp.Zone+"/"+sp.Type] = priceQuote{usd: usd, at: now}
				}
			}
			return err
		})
	}
	out := map[string]float64{}
	for _, in := range insts {
		if !priceable(in) {
			continue
		}
		// A quote past its TTL whose refresh failed is still the best
		// estimate there is, for as long as the lookup keeps failing.
		if q, ok := p.quote[in.Region+"/"+in.Zone+"/"+in.Type]; ok {
			out[in.ID] = q.usd
		}
	}
	return out
}

func priceable(in Instance) bool {
	return in.Spot && in.Region != "" && in.Zone != "" && in.Type != ""
}
