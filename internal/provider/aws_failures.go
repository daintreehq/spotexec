package provider

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Failure history: a zone and type that keeps failing (refusals for
// capacity, VMs reclaimed soon after launch) scores worse under
// launch_order = "run_cost" for longer, the more often it fails, with the
// penalty decaying over time. A cooldown only skips a dry pool for a few
// minutes, and the reclaim hold-back only for a while; without this, a pool
// whose type ranks first on speed is asked again every time they end.
const (
	// failHalfLife: a failure counts half as much after this long.
	failHalfLife = 2 * time.Hour
	// failCost is what one recent failure adds to an option's score, as
	// minutes of the VM and waiting: a ranking weight, not a measured cost.
	// Two recent capacity refusals outweigh one generation's speed.
	failCost = 3 * time.Minute
	// failMaxWeight caps a pool's decayed failures, so a pool that failed
	// all day comes back within a day of its last failure.
	failMaxWeight = 16
	// failForget: history decayed below this is dropped.
	failForget = 0.01
	// earlyReclaim: a VM reclaimed within this long of its launch counts as
	// a failure of its pool.
	earlyReclaim = time.Hour
)

// poolFailures is one zone and type's failure history in placement.json:
// its decayed failure counts as of At.
type poolFailures struct {
	Capacity     float64   `json:"capacity,omitempty"` // refusals for capacity
	Reclaims     float64   `json:"reclaims,omitempty"` // VMs reclaimed within earlyReclaim of launch
	At           time.Time `json:"at"`
	LastCapacity time.Time `json:"last_capacity,omitempty"`
	LastReclaim  time.Time `json:"last_reclaim,omitempty"`
}

// decayed is f as of now. A time ahead of At (a clock set back) decays
// nothing rather than growing the counts.
func (f poolFailures) decayed(now time.Time) poolFailures {
	if el := now.Sub(f.At); el > 0 {
		k := math.Exp2(-el.Hours() / failHalfLife.Hours())
		f.Capacity *= k
		f.Reclaims *= k
		f.At = now
	}
	return f
}

func (f poolFailures) weight() float64 { return f.Capacity + f.Reclaims }

// recordFailure adds one failure of p's pool to st: a capacity refusal, or
// (capacity false) a VM reclaimed soon after launch.
func recordFailure(st *placementState, p SpotPrice, capacity bool, now time.Time) {
	if st.Failures == nil {
		st.Failures = map[string]poolFailures{}
	}
	key := poolKey(p)
	f := st.Failures[key]
	if f.At.IsZero() {
		f.At = now
	}
	f = f.decayed(now)
	if capacity {
		f.Capacity++
		f.LastCapacity = now
	} else {
		f.Reclaims++
		f.LastReclaim = now
	}
	if w := f.weight(); w > failMaxWeight {
		f.Capacity *= failMaxWeight / w
		f.Reclaims *= failMaxWeight / w
	}
	st.Failures[key] = f
}

// forgetOldFailures drops failure history decayed to almost nothing.
func forgetOldFailures(st *placementState, now time.Time) {
	for k, f := range st.Failures {
		if f.decayed(now).weight() < failForget {
			delete(st.Failures, k)
		}
	}
}

// FailureHistory is why an option's score carries a failure penalty: its
// zone and type's recent failures, each counting half as much every
// failHalfLife.
type FailureHistory struct {
	Capacity     float64   `json:"capacity_refusals"` // decayed count of refusals for capacity
	Reclaims     float64   `json:"early_reclaims"`    // decayed count of VMs reclaimed soon after launch
	LastCapacity time.Time `json:"last_capacity_refusal,omitempty"`
	LastReclaim  time.Time `json:"last_early_reclaim,omitempty"`
}

// String says what failed there, as decayed counts ("1.8 capacity
// refusals, last 14:05").
func (h FailureHistory) String() string {
	var parts []string
	add := func(w float64, what string, last time.Time) {
		if w < failForget {
			return
		}
		s := fmt.Sprintf("%.1f %s", w, what)
		if !last.IsZero() {
			s += ", last " + last.Local().Format("15:04")
		}
		parts = append(parts, s)
	}
	add(h.Capacity, "capacity refusals", h.LastCapacity)
	add(h.Reclaims, "VMs reclaimed within "+shortDur(earlyReclaim)+" of launch", h.LastReclaim)
	return strings.Join(parts, "; ")
}

func shortDur(d time.Duration) string {
	if d%time.Hour == 0 {
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/time.Minute), "minute")
}

// failureEvidence copies each listed price's decayed failure history.
func failureEvidence(st *placementState, ps []SpotPrice, now time.Time) map[string]poolFailures {
	out := map[string]poolFailures{}
	for _, p := range ps {
		if f, ok := st.Failures[poolKey(p)]; ok {
			if f = f.decayed(now); f.weight() >= failForget {
				out[poolKey(p)] = f
			}
		}
	}
	return out
}
