package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// The cost model (docs/POOL.md, "Cost-aware placement"): with enough
// history, the retained VMs a job may go to are compared by what placing it
// there costs, rather than by the fixed preference order alone:
//
//	score = extra VM time it causes × the VM's price + its expected lease × wait_cost
//
// The extra VM time is how much later the VM can idle out because of this
// job: an idle VM that has been idle a while costs the job's whole lease
// plus the idle time already run down; a busy VM whose jobs outlast this
// one costs nothing. The lease (acquire to release) is learned per task
// and per how warm the VM is for it, so a cold sync shows up as a longer
// lease. Without prices for every candidate, or samples for this task and
// the jobs already there, placement keeps the fixed order (score_mode
// "heuristic"): a missing figure is unknown, never zero.

const (
	// modelSamples: lease durations kept per key (the newest).
	modelSamples = 16
	// modelMin: samples a key needs before it is used.
	modelMin = 3
	// modelKeys: keys kept per fleet (the most recently used).
	modelKeys = 96
	// modelAge: a key unused this long is forgotten.
	modelAge = 14 * 24 * time.Hour
	// pricesEvery: how often the fleet's prices are looked up.
	pricesEvery = 15 * time.Minute
)

// durStat is a key's recent lease durations, in seconds.
type durStat struct {
	S  []float64 `json:"s"`
	At time.Time `json:"at"`
}

// TaskKey names a job for the duration model: its repository and command,
// hashed (the model never stores a command line).
func TaskKey(repo, command string) string {
	h := sha256.Sum256([]byte(repo + "\x00" + command))
	return hex.EncodeToString(h[:8])
}

func modelKey(task string, cache int) string {
	if cache < 0 {
		return task + "|*"
	}
	return task + "|" + string(rune('0'+cache))
}

// observe records a lease of task, placed at the given cache warmth.
func (s *schedState) observe(task string, cache int, d time.Duration, now time.Time) {
	if task == "" || d <= 0 {
		return
	}
	if s.Model == nil {
		s.Model = map[string]*durStat{}
	}
	keys := []string{modelKey(task, -1)}
	if cache >= 0 {
		keys = append(keys, modelKey(task, cache))
	}
	for _, k := range keys {
		st := s.Model[k]
		if st == nil {
			st = &durStat{}
			s.Model[k] = st
		}
		st.S = append(st.S, d.Seconds())
		if len(st.S) > modelSamples {
			st.S = st.S[len(st.S)-modelSamples:]
		}
		st.At = now
	}
	for k, st := range s.Model {
		if now.Sub(st.At) > modelAge {
			delete(s.Model, k)
		}
	}
	// At most modelKeys tasks, each with all its warmth levels: the task
	// used least recently goes whole.
	for {
		last := map[string]time.Time{}
		for k, st := range s.Model {
			t, _, _ := strings.Cut(k, "|")
			if st.At.After(last[t]) {
				last[t] = st.At
			}
		}
		if len(last) <= modelKeys {
			return
		}
		oldest, ot := "", time.Time{}
		for t, at := range last {
			if oldest == "" || at.Before(ot) {
				oldest, ot = t, at
			}
		}
		for k := range s.Model {
			if t, _, _ := strings.Cut(k, "|"); t == oldest {
				delete(s.Model, k)
			}
		}
	}
}

// predict is a task's expected lease at a cache warmth: the median of its
// samples there, else of all its samples; and their 90th percentile (for
// fitting under a deadline). ok is false without enough samples.
func (s *schedState) predict(task string, cache int) (mid, p90 time.Duration, ok bool) {
	if task == "" {
		return 0, 0, false
	}
	for _, k := range []string{modelKey(task, cache), modelKey(task, -1)} {
		st := s.Model[k]
		if st == nil || len(st.S) < modelMin {
			continue
		}
		v := append([]float64(nil), st.S...)
		sort.Float64s(v)
		q := func(f float64) time.Duration {
			i := int(math.Ceil(f*float64(len(v)))) - 1
			return time.Duration(v[max(0, min(i, len(v)-1))] * float64(time.Second))
		}
		return q(0.5), q(0.9), true
	}
	return 0, 0, false
}

// cacheLevel is how warm a VM is for a job: 2 with its worktree's files, 1
// with its repository's caches, 0 neither.
func cacheLevel(v *vmSched, c claim, now time.Time) int {
	switch {
	case v.warmFor(c.Worktree, now):
		return 2
	case v.hasRepo(c.Repo, now):
		return 1
	}
	return 0
}

// hourly is a VM's price per hour: the recorded Spot price, else the
// runner's hourly_cost.
func (p *Pool) hourly(v *vmSched, now time.Time) (float64, bool) {
	if usd, ok := v.price(now); ok {
		return usd, true
	}
	if p.R.HourlyCost > 0 {
		return p.R.HourlyCost, true
	}
	return 0, false
}

// expiry is when a VM would idle out if nothing more were placed on it
// (zero: not before the horizon, for a warm VM the pool manager keeps).
// ok is false when a job on it has no predicted release.
func (p *Pool) expiry(s *schedState, w vmView, now time.Time) (time.Time, bool) {
	for _, id := range s.Warm {
		if id == w.in.ID {
			return time.Time{}, true
		}
	}
	idle := p.R.IdleTimeout.Duration
	last := w.v.lastUseful()
	if w.in.LaunchedAt.After(last) {
		last = w.in.LaunchedAt
	}
	e := last.Add(idle)
	if e.Before(now) {
		e = now // overdue: its watchdog hasn't run yet
	}
	for _, a := range s.Attempts {
		if a.VM != w.in.ID || !a.live() {
			continue
		}
		end := a.Expect
		if end.IsZero() {
			return time.Time{}, false
		}
		if end.Before(now) {
			end = now // running late: at least until now
		}
		if t := end.Add(idle); t.After(e) {
			e = t
		}
	}
	return e, true
}

// scored is a candidate's measured cost.
type scored struct {
	cost float64 // USD
	late bool    // its lease may outlast the VM's lifetime
}

// score is the measured cost of placing a job (claim c) on VM w: the VM
// time it adds times its price, plus the expected lease times wait_cost.
// ok is false when a figure it needs is unknown.
func (p *Pool) score(s *schedState, w vmView, c claim, now time.Time) (scored, bool) {
	price, ok := p.hourly(w.v, now)
	if !ok {
		return scored{}, false
	}
	mid, p90, ok := s.predict(c.Task, cacheLevel(w.v, c, now))
	if !ok {
		return scored{}, false
	}
	e0, ok := p.expiry(s, w, now)
	if !ok {
		return scored{}, false
	}
	added := 0.0
	if !e0.IsZero() {
		if e1 := now.Add(mid + p.R.IdleTimeout.Duration); e1.After(e0) {
			added = e1.Sub(e0).Seconds()
		}
	}
	out := scored{cost: price/3600*added + p.R.WaitCostPerHour()/3600*mid.Seconds()}
	if lt := p.R.MaxLifetime.Duration; lt > 0 && !w.in.LaunchedAt.IsZero() {
		out.late = now.Add(p90).After(w.in.LaunchedAt.Add(lt - LifetimeDrainAhead(p.R)))
	}
	return out, true
}

// tieBand is the cost difference too small to act on: about a second of
// waiting, plus a second of the VM.
func (p *Pool) tieBand(price float64) float64 {
	return (p.R.WaitCostPerHour() + price) / 3600
}

// --- prices ---------------------------------------------------------------

// fleetPricer prices a fleet's VMs (provider.InstancePricer; tests
// substitute it).
type fleetPricer interface {
	Prices(ctx context.Context, insts []provider.Instance) map[string]float64
}

// pricesForAnyFleet lets tests price a fleet that isn't AWS's.
var pricesForAnyFleet = false

var newFleetPricer = func(r *config.Runner) (fleetPricer, error) {
	return provider.NewInstancePricer(r)
}

// RefreshPrices records the current Spot price of the fleet's VMs, at most
// every pricesEvery across all processes (one lookup per region). Prices
// are looked up outside the pool lock.
func (p *Pool) RefreshPrices(ctx context.Context) {
	if p.R.Type != config.TypeAWSSpot || p.R.SchedPolicy() != config.PolicyConsolidate {
		return
	}
	if _, aws := p.Fleet.(provider.Reclaimer); !aws && !pricesForAnyFleet {
		return // a stand-in fleet (tests): nothing to look up
	}
	var insts []provider.Instance
	_ = p.withCtx(ctx, func(st *poolState) error {
		s := st.sched()
		now := time.Now()
		if len(st.Instances) == 0 || (now.Sub(s.PricesAt) < pricesEvery && !s.PricesAt.After(now)) {
			return nil
		}
		// Claimed: other processes skip it meanwhile. Until the answer is
		// in, it counts as a lookup a minute ago at most-every-15m, so one
		// that fails (or a process that exits) is retried in a minute.
		s.PricesAt = now.Add(-pricesEvery + time.Minute)
		insts = append(insts, st.Instances...)
		return nil
	})
	if len(insts) == 0 {
		return
	}
	pr, err := newFleetPricer(p.R)
	if err != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	prices := pr.Prices(pctx, insts)
	if len(prices) == 0 {
		return
	}
	_ = p.withCtx(ctx, func(st *poolState) error {
		s := st.sched()
		now := time.Now()
		s.PricesAt = now
		for id, usd := range prices {
			if v := s.VMs[id]; v != nil && usd > 0 {
				v.Price, v.PriceAt = usd, now
			}
		}
		return nil
	})
}
