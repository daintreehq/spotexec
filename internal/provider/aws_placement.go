package provider

import (
	"context"
	"fmt"
	"math"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// Run-cost placement (launch_order = "run_cost", the default): each option
// is scored by what an hour of work there is expected to cost, in USD: the
// Spot bill plus the waiting (valued at the runner's wait_cost) that a
// slower type, a cold region or a farther region adds. Fixed assumptions are
// below, and `spotexec prices --json` reports them with every score.
const (
	// runHorizon is the work each option is scored on: an hour of the
	// fastest option's work. A cold start is paid once per VM, so this is
	// also how long a saving has to make up for one.
	runHorizon = time.Hour
	// The time a region adds while it holds none of what a run reuses:
	// Daintree's 17-part test run took 7.1 minutes the first time in a
	// region and 3.5 the next. It is split by what saves each part (an
	// estimate: the dependency install is the largest). The deps and image
	// parts are only the defaults before the project's own builds and
	// installs have been measured (see RecordColdWork).
	//
	// coldBoot: booting without a bake of the runner there runs its
	// bootstrap on the VM.
	coldBoot = time.Minute
	// coldDeps: without the project's archive in the region's dependency
	// cache, the install runs (default).
	coldDeps = 2 * time.Minute
	// coldImage: without the project's image in the region's registry,
	// it is built (default).
	coldImage = time.Minute
	// warmFor is how long a region counts as warm after a VM of this
	// runner last ran there, where the project's caches can't be checked:
	// well inside the 14 days the S3 dependency cache keeps an archive.
	warmFor = 7 * 24 * time.Hour
	// warmAfter is how long a VM must have been up before it counts as
	// evidence: past its first-boot setup and first runs, which fill the
	// caches. A VM just running, or one that failed to boot, says nothing.
	warmAfter = 10 * time.Minute
	// genSpeedup is the speed per vCPU each instance generation adds
	// (m7a.2xlarge ran the same shards about 10% faster than m6a.2xlarge).
	// Only weighed for vcpus runners, whose types span generations.
	genSpeedup = 1.10
	// runRoundTrips is the round trips from this machine to the VM in an
	// hour of work: some 30 runs of ~10 SSH round trips each.
	runRoundTrips = 300
	// rttFor is how long a measured round trip to a region is used.
	rttFor = 7 * 24 * time.Hour
	// rttStep rounds measured round trips, so jitter doesn't reorder
	// regions at the same distance.
	rttStep = 10 * time.Millisecond
)

// RunCost is how launch_order = "run_cost" scored one option: the expected
// cost in USD of an hour of the fastest option's work there, and what each
// factor added to it.
type RunCost struct {
	Score    float64 `json:"score"`
	Compute  float64 `json:"compute"`  // the Spot bill for an hour at this price
	Speed    float64 `json:"speed"`    // a slower type: the extra VM time and waiting (vcpus runners)
	Cold     float64 `json:"cold"`     // a cold start: the sum of ColdParts
	Distance float64 `json:"distance"` // round trips to a farther region
	Failures float64 `json:"failures"` // the zone and type's recent failures (see FailureHistory)
	// FailedHere is the history behind Failures (nil: none lately).
	FailedHere *FailureHistory `json:"failure_history,omitempty"`
	// ColdParts says which parts of a cold start the region pays, and why
	// (several regions only).
	ColdParts *ColdParts `json:"cold_parts,omitempty"`
	// RelSpeed is the type's speed per vCPU against the fastest option's
	// (1: as fast), from its instance generation.
	RelSpeed float64   `json:"relative_speed"`
	Warm     bool      `json:"warm"`                // no part of a cold start is paid
	WarmSeen time.Time `json:"warm_seen,omitempty"` // last time a VM of this runner ran in the region, when that decided a part
	RTTMS    int       `json:"rtt_ms,omitempty"`    // TCP connect to the region's EC2 endpoint (0: not measured)
}

// ColdParts is what a cold start in a region is made of: booting without a
// bake, installing dependencies without the cache's archive, and building
// the container image instead of pulling it.
type ColdParts struct {
	Boot  ColdPart `json:"boot"`
	Deps  ColdPart `json:"dependencies"`
	Image ColdPart `json:"image"`
}

// ColdPart is one part of a cold start in one region.
type ColdPart struct {
	Cost float64 `json:"cost"` // USD added to the score (0: warm)
	Warm bool    `json:"warm"` // the region has what saves it, or the run doesn't need it
	Why  string  `json:"why"`  // what decided it
}

// Paid names the parts the region pays ("" when none).
func (p ColdParts) Paid() string {
	var out []string
	for _, c := range []struct {
		name string
		part ColdPart
	}{{"boot", p.Boot}, {"deps", p.Deps}, {"image", p.Image}} {
		if !c.part.Warm {
			out = append(out, c.name)
		}
	}
	return strings.Join(out, ",")
}

// RunCostModel is the assumptions behind every RunCost.
type RunCostModel struct {
	WaitCost       float64 `json:"wait_cost"`          // USD per hour of waiting (wait_cost)
	HorizonMinutes float64 `json:"horizon_minutes"`    // the work scored: an hour of the fastest option's
	ColdMinutes    float64 `json:"cold_minutes"`       // what a cold region adds: the sum of the parts below
	BootMinutes    float64 `json:"cold_boot_minutes"`  // booting without a bake
	DepsMinutes    float64 `json:"cold_deps_minutes"`  // installing dependencies without the cache
	ImageMinutes   float64 `json:"cold_image_minutes"` // building the container image
	WarmDays       float64 `json:"warm_days"`          // how long a region stays warm after a VM ran there (parts not checked)
	WarmAfterMin   float64 `json:"warm_after_minutes"` // how long that VM must have been up
	GenSpeedup     float64 `json:"gen_speedup"`        // speed per vCPU each generation adds (vcpus runners)
	RoundTrips     int     `json:"round_trips"`        // client round trips in that work
	// FailureMinutes is what each recent failure of a zone and type adds,
	// as minutes of the VM and waiting; FailureHalfLifeHours is how long a
	// failure takes to count half as much.
	FailureMinutes       float64 `json:"failure_minutes"`
	FailureHalfLifeHours float64 `json:"failure_half_life_hours"`
	SpeedByGen           bool    `json:"speed_by_generation"`
	Regions              bool    `json:"regions"` // warmth and distance weighed (several regions)
	// DepsFrom and ImageFrom say where DepsMinutes and ImageMinutes came
	// from: the project's measured installs and builds, or the defaults.
	DepsFrom  ColdEstimate `json:"cold_deps_estimate"`
	ImageFrom ColdEstimate `json:"cold_image_estimate"`
}

// Ranking is the order a launch tries a price table's options in.
type Ranking struct {
	// Order is how the options were ranked: "run_cost", "cheapest" (the
	// Spot price alone) or "listed" (instance_types in order).
	Order  string
	Prices []SpotPrice // launch order; those held back after a Spot interruption last
	// Cost: each option's score by pool key (Order "run_cost" only).
	Cost  map[string]RunCost
	Model *RunCostModel
	held  map[string]cooldown
	// ranked: the order before held-back options were moved last.
	ranked []SpotPrice
}

// HeldBack reports whether p ranks last for now after a Spot interruption.
func (k Ranking) HeldBack(p SpotPrice) bool {
	_, ok := k.held[poolKey(p)]
	return ok
}

// CostOf returns p's run-cost score, if it has one.
func (k Ranking) CostOf(p SpotPrice) (RunCost, bool) {
	c, ok := k.Cost[poolKey(p)]
	return c, ok
}

// rankOrder is the launch order a runner's options are ranked by. A runner
// in one region with a fixed list ranks by price, exactly as before
// run_cost: every option shares the region's warmth and distance, and
// types the user listed aren't second-guessed on speed.
func (g *AWSGlobal) rankOrder() string {
	switch g.r.LaunchOrder {
	case config.LaunchListed, config.LaunchCheapest:
		return g.r.LaunchOrder
	}
	if g.oneRegion() && !g.r.ByRequirements() {
		return config.LaunchCheapest
	}
	return config.LaunchRunCost
}

// placementEvidence is what run-cost ranking reads from local state, and
// what the price table found in each region's account-wide caches.
type placementEvidence struct {
	now   time.Time
	warm  map[string]time.Time     // region -> last VM of this runner seen running there
	rtt   map[string]time.Duration // region -> measured round trip
	found map[string]regionWarmth  // region -> what its caches hold (see warmth)
	fails map[string]poolFailures  // pool key -> its failure history, decayed to now
}

// Rank orders t's options for the next launch: by run cost (or price, or
// the listed order), then those held back after a Spot interruption last.
// Cooldowns and max_price are left to the caller: they skip, not reorder.
func (g *AWSGlobal) Rank(t PriceTable) Ranking {
	ev := placementEvidence{now: g.clock(), warm: map[string]time.Time{}, rtt: map[string]time.Duration{}, found: t.warmth}
	held := map[string]cooldown{}
	var work *coldWork
	_ = g.withState(func(st *placementState) {
		if t.repoID != "" {
			work = st.ColdWork[t.repoID]
		}
		for _, p := range t.Prices {
			if c, ok := st.Cooldown[reclaimKey(p)]; ok && ev.now.Before(c.Until) {
				held[poolKey(p)] = c
			}
		}
		ev.fails = failureEvidence(st, t.Prices, ev.now)
		if st.WarmProfile == g.r.Profile {
			for rg, at := range st.Warm {
				ev.warm[rg] = at
			}
		}
		for rg, s := range st.RTT {
			if ev.now.Sub(s.At) < rttFor {
				ev.rtt[rg] = time.Duration(s.MS) * time.Millisecond
			}
		}
	})
	k := Ranking{Order: g.rankOrder(), held: held}
	ps := append([]SpotPrice(nil), t.Prices...)
	if k.Order == config.LaunchRunCost {
		k.Model = g.costModel(work, ev.now)
		k.Cost = runCosts(*k.Model, ps, ev)
		idx := make([]int, len(ps))
		for i := range idx {
			idx[i] = i
		}
		// Equal scores keep the price table's order (price, then the
		// instance_types order, region, zone, type).
		sort.SliceStable(idx, func(a, b int) bool {
			return k.Cost[poolKey(ps[idx[a]])].Score < k.Cost[poolKey(ps[idx[b]])].Score
		})
		ranked := make([]SpotPrice, len(ps))
		for i, j := range idx {
			ranked[i] = ps[j]
		}
		ps = ranked
	}
	k.ranked = ps
	for _, i := range heldLast(ps, held) {
		k.Prices = append(k.Prices, ps[i])
	}
	return k
}

// costModel is the model for a launch of the project whose builds and
// installs work holds (nil: none measured, or outside a project).
func (g *AWSGlobal) costModel(work *coldWork, now time.Time) *RunCostModel {
	deps, depsFrom := work.estimate(ColdDeps, coldDeps, now)
	image, imageFrom := work.estimate(ColdImage, coldImage, now)
	return &RunCostModel{
		WaitCost:             g.r.WaitCostPerHour(),
		HorizonMinutes:       runHorizon.Minutes(),
		ColdMinutes:          (coldBoot + deps + image).Minutes(),
		BootMinutes:          coldBoot.Minutes(),
		DepsMinutes:          deps.Minutes(),
		ImageMinutes:         image.Minutes(),
		DepsFrom:             depsFrom,
		ImageFrom:            imageFrom,
		WarmDays:             warmFor.Hours() / 24,
		WarmAfterMin:         warmAfter.Minutes(),
		GenSpeedup:           genSpeedup,
		RoundTrips:           runRoundTrips,
		FailureMinutes:       failCost.Minutes(),
		FailureHalfLifeHours: failHalfLife.Hours(),
		SpeedByGen:           g.r.ByRequirements(),
		Regions:              !g.oneRegion(),
	}
}

// runCosts scores each option (see RunCost).
func runCosts(m RunCostModel, ps []SpotPrice, ev placementEvidence) map[string]RunCost {
	maxGen := 0
	if m.SpeedByGen {
		for _, p := range ps {
			maxGen = max(maxGen, typeGeneration(p.Type))
		}
	}
	minRTT := time.Duration(-1)
	if m.Regions {
		for _, p := range ps {
			if d, ok := ev.rtt[p.Region]; ok && (minRTT < 0 || d < minRTT) {
				minRTT = d
			}
		}
	}
	h := m.HorizonMinutes / 60
	out := make(map[string]RunCost, len(ps))
	for _, p := range ps {
		price, wait := p.USD(), m.WaitCost
		c := RunCost{RelSpeed: 1}
		if gen := typeGeneration(p.Type); m.SpeedByGen && gen > 0 {
			// Unknown generations count as fastest rather than guessing.
			c.RelSpeed = math.Pow(m.GenSpeedup, float64(gen-maxGen))
		}
		// An hour of the fastest option's work takes 1/RelSpeed hours here:
		// the extra time costs the VM and the wait.
		c.Compute = price * h
		c.Speed = (price + wait) * h * (1/c.RelSpeed - 1)
		if m.Regions {
			seen, ok := ev.warm[p.Region]
			// A time ahead of the clock is no evidence of anything.
			ran := ok && ev.now.Sub(seen) < warmFor && !seen.After(ev.now.Add(time.Hour))
			parts, usedHistory := coldParts(m, price+wait, ev.found[p.Region], ran)
			c.ColdParts = &parts
			c.Cold = parts.Boot.Cost + parts.Deps.Cost + parts.Image.Cost
			c.Warm = parts.Boot.Warm && parts.Deps.Warm && parts.Image.Warm
			if ran && usedHistory {
				c.WarmSeen = seen
			}
			if d, ok := ev.rtt[p.Region]; ok {
				c.RTTMS = int(d / time.Millisecond)
				c.Distance = (price + wait) * float64(m.RoundTrips) * (d - minRTT).Hours()
			}
		}
		if f, ok := ev.fails[poolKey(p)]; ok {
			c.FailedHere = &FailureHistory{Capacity: f.Capacity, Reclaims: f.Reclaims, LastCapacity: f.LastCapacity, LastReclaim: f.LastReclaim}
			c.Failures = (price + wait) * f.weight() * m.FailureMinutes / 60
		}
		c.Score = c.Compute + c.Speed + c.Cold + c.Distance + c.Failures
		out[poolKey(p)] = c
	}
	return out
}

// coldParts prices each part of a cold start in one region, at perHour (the
// VM and the waiting): checked parts by what the region holds, the
// project's parts that couldn't be checked by whether a VM of the runner
// ran there lately (ran). usedHistory: ran decided a part.
func coldParts(m RunCostModel, perHour float64, w regionWarmth, ran bool) (p ColdParts, usedHistory bool) {
	part := func(s partSeen, minutes float64, has, lacks, unused string, history bool) ColdPart {
		c := ColdPart{Why: s.why}
		switch s.state {
		case partHas:
			c.Warm = true
			if c.Why == "" {
				c.Why = has
			}
		case partUnused:
			c.Warm = true
			if c.Why == "" {
				c.Why = unused
			}
		case partLacks:
			if c.Why == "" {
				c.Why = lacks
			}
		default:
			if history {
				usedHistory = true
				c.Warm = ran
				if c.Why != "" {
					c.Why += "; "
				}
				if ran {
					c.Why += "not checked: a VM of the runner ran here lately"
				} else {
					c.Why += "not checked: no VM of the runner ran here lately"
				}
			} else if c.Why == "" {
				c.Why = "not checked"
			}
		}
		if !c.Warm {
			c.Cost = perHour * minutes / 60
		}
		return c
	}
	// Without a bake every VM runs the bootstrap: where VMs ran says
	// nothing about it.
	p.Boot = part(w.boot, m.BootMinutes, "a bake of the runner", "no bake of the runner", "", false)
	p.Deps = part(w.deps, m.DepsMinutes, "the project's dependency archive", "no dependency archive for the project", "no dependencies to install", true)
	p.Image = part(w.image, m.ImageMinutes, "the project's image", "no image of the project", "no image to build", true)
	return p, usedHistory
}

// Factors lists what the score added to the Spot bill, for messages
// ("" when nothing was).
func (c RunCost) Factors() string {
	var parts []string
	add := func(v float64, what string) {
		if v >= 0.0005 {
			parts = append(parts, fmt.Sprintf("+$%.3f %s", v, what))
		}
	}
	add(c.Speed, fmt.Sprintf("slower type (%.0f%% speed)", c.RelSpeed*100))
	cold := "cold region"
	if c.ColdParts != nil {
		if paid := c.ColdParts.Paid(); paid != "" {
			cold += " (" + paid + ")"
		}
	}
	add(c.Cold, cold)
	add(c.Distance, fmt.Sprintf("distance (%d ms)", c.RTTMS))
	if c.FailedHere != nil {
		add(c.Failures, "failed here lately ("+c.FailedHere.String()+")")
	}
	return strings.Join(parts, ", ")
}

// Summary is one line on c: its score and what made it up.
func (c RunCost) Summary() string {
	s := fmt.Sprintf("$%.3f per hour of work: $%.3f Spot", c.Score, c.Compute)
	if f := c.Factors(); f != "" {
		s += ", " + f
	}
	if c.Warm {
		s += "; warm region"
	}
	return s
}

// Placement says where a launch went and why, for events and logs.
type Placement struct {
	Order string `json:"order"` // run_cost | cheapest | listed | on_demand
	Rank  int    `json:"rank"`  // 1: the first choice
	Of    int    `json:"of"`    // options ranked
	Price string `json:"price,omitempty"`
	// Cost is the winning option's score (run_cost).
	Cost *RunCost `json:"run_cost,omitempty"`
	Why  string   `json:"why"`
}

// passedOver counts why a launch passed over the options ranked before the
// one it launched, for its explanation.
type passedOver struct {
	order []string
	n     map[string]int
}

func (p *passedOver) add(why string) {
	if p.n == nil {
		p.n = map[string]int{}
	}
	if p.n[why] == 0 {
		p.order = append(p.order, why)
	}
	p.n[why]++
}

func (p passedOver) String() string {
	var parts []string
	for _, why := range p.order {
		parts = append(parts, fmt.Sprintf("%d %s", p.n[why], why))
	}
	return strings.Join(parts, ", ")
}

// placementWhy explains why the option at rank won: what put it first, or
// what happened to the ones ranked before it (ahead). Against the cheapest
// option it says what that one would have added.
func placementWhy(t PriceTable, k Ranking, rank int, ahead passedOver) string {
	c := k.Prices[rank]
	where := plural(len(t.Prices), "option") + " in " + plural(len(t.Regions)-len(t.Unpriced), "region")
	held := k.heldAhead(c) // better options held back: rank is launch order, not score order
	var s string
	switch k.Order {
	case config.LaunchRunCost:
		s = "lowest expected run cost of " + where
		if rank > 0 || held > 0 {
			s = fmt.Sprintf("#%d in launch order (by run cost) of %s", rank+1, where)
		}
	case config.LaunchListed:
		s = fmt.Sprintf("#%d in instance_types order of %s", rank+1, where)
	default:
		s = "cheapest of " + where
		if rank > 0 || held > 0 {
			s = fmt.Sprintf("#%d in launch order (by price) of %s", rank+1, where)
		}
	}
	if rank > 0 {
		s += "; passed over: " + ahead.String()
	}
	if k.HeldBack(c) {
		s += "; itself held back after a Spot interruption"
	} else if held > 0 {
		s += fmt.Sprintf("; %s held back after a Spot interruption would otherwise come first", plural(held, "option"))
	}
	if k.Order != config.LaunchRunCost {
		return s
	}
	rc, ok := k.CostOf(c)
	if !ok {
		return s
	}
	s += " (" + rc.Summary() + ")"
	if cheap := cheapestOf(t.Prices); cheap != c && cheap.USD() < c.USD() {
		if cc, ok := k.CostOf(cheap); ok {
			why := cc.Factors()
			if why == "" {
				why = "no added cost"
			}
			s += fmt.Sprintf("; the cheapest, %s in %s at $%s/h, scores $%.3f (%s)", cheap.Type, cheap.Zone, cheap.Price, cc.Score, why)
		}
	}
	return s
}

// heldAhead counts the options held back after a Spot interruption that
// would otherwise rank ahead of p.
func (k Ranking) heldAhead(p SpotPrice) int {
	n := 0
	pos := -1
	for i, q := range k.ranked {
		if q == p {
			pos = i
			break
		}
	}
	for i, q := range k.ranked {
		if i < pos && k.HeldBack(q) {
			n++
		}
	}
	return n
}

// ---- round trips ----

// observedRTT is the latest TCP connect this process made to each region's
// EC2 endpoint, and when (a reused connection makes none).
var observedRTT = struct {
	sync.Mutex
	m map[string]rttObservation
}{m: map[string]rttObservation{}}

type rttObservation struct {
	d  time.Duration
	at time.Time
}

// traceConnect times the TCP connects of one request to region's EC2
// endpoint: a connect is one round trip. Through a proxy it times the proxy
// instead, the same for every region, so distance then weighs nothing.
func traceConnect(ctx context.Context, region string) context.Context {
	var mu sync.Mutex
	starts := map[string]time.Time{}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			mu.Lock()
			starts[network+" "+addr] = time.Now()
			mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			mu.Lock()
			start, ok := starts[network+" "+addr]
			mu.Unlock()
			if err != nil || !ok {
				return
			}
			now := time.Now()
			observedRTT.Lock()
			if cur, ok := observedRTT.m[region]; !ok || now.After(cur.at) {
				observedRTT.m[region] = rttObservation{d: now.Sub(start), at: now}
			}
			observedRTT.Unlock()
		},
	})
}

// rttSample is a region's measured round trip, kept in placement.json.
type rttSample struct {
	MS int       `json:"ms"`
	At time.Time `json:"at"`
}

// saveRTTs records the round trips this process measured to regions, each
// as of when it was measured (a newer saved one stays).
func saveRTTs(st *placementState, regions []string) {
	observedRTT.Lock()
	defer observedRTT.Unlock()
	for _, rg := range regions {
		o, ok := observedRTT.m[rg]
		if !ok || !o.at.After(st.RTT[rg].At) {
			continue
		}
		if st.RTT == nil {
			st.RTT = map[string]rttSample{}
		}
		st.RTT[rg] = rttSample{MS: int(o.d.Round(rttStep) / time.Millisecond), At: o.at}
	}
}

// sawRunning records that VMs of this runner have been running in their
// regions for warmAfter: the evidence a region holds its caches.
func (g *AWSGlobal) sawRunning(st *placementState, ins []Instance, now time.Time) {
	if st.WarmProfile != g.r.Profile {
		st.Warm, st.WarmProfile = nil, g.r.Profile // another account's caches
	}
	for _, in := range ins {
		if in.State != StateRunning || in.Region == "" || in.LaunchedAt.IsZero() || now.Sub(in.LaunchedAt) < warmAfter {
			continue
		}
		if st.Warm == nil {
			st.Warm = map[string]time.Time{}
		}
		if now.After(st.Warm[in.Region]) {
			st.Warm[in.Region] = now
		}
	}
}

// forgetOld drops warmth and round trips too old to use.
func forgetOld(st *placementState, now time.Time) {
	for rg, at := range st.Warm {
		if now.Sub(at) > warmFor || at.After(now.Add(time.Hour)) {
			delete(st.Warm, rg)
		}
	}
	for rg, s := range st.RTT {
		if now.Sub(s.At) > rttFor || s.At.After(now.Add(time.Hour)) {
			delete(st.RTT, rg)
		}
	}
	for k, e := range st.Evidence {
		if !e.fresh(now) {
			delete(st.Evidence, k)
		}
	}
	forgetOldColdWork(st, now)
	forgetOldFailures(st, now)
}
