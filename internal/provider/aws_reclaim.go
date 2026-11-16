package provider

import (
	"fmt"
	"strings"
	"time"
)

// Spot interruptions: a zone and type whose VMs AWS reclaims ranks last for
// new launches for a while, so a fleet moves away from a pool that keeps
// interrupting it. Unlike a capacity cooldown it is not skipped: when every
// other option fails, a launch still goes there.
const (
	reclaimHoldMin = 30 * time.Minute // first hold-back after an interruption notice
	reclaimHoldMax = 4 * time.Hour
)

// Reclaimer is implemented by fleets that weigh Spot interruptions in where
// they launch next.
type Reclaimer interface {
	// Reclaimed records that in got a Spot interruption notice.
	Reclaimed(in Instance)
}

var (
	_ Reclaimer = (*AWSGlobal)(nil)
	_ Reclaimer = (*AWSSpot)(nil)
)

const reclaimPrefix = "reclaimed:"

// reclaimKey names a zone/type's hold-back in placementState.Cooldown.
func reclaimKey(p SpotPrice) string { return reclaimPrefix + poolKey(p) }

// Reclaimed holds in's zone and type back for reclaimHoldMin, doubling (up
// to reclaimHoldMax) for each further VM reclaimed there while the last
// hold is remembered (up to reclaimHoldMax after it ends). A VM reclaimed
// within earlyReclaim of its launch also counts in the pool's failure
// history, which outlasts the hold.
func (a *AWSSpot) Reclaimed(in Instance) { a.ranker().Reclaimed(in) }

// Reclaimed implements Reclaimer.
func (g *AWSGlobal) Reclaimed(in Instance) {
	if in.Zone == "" || in.Type == "" {
		return
	}
	if in.Region == "" {
		in.Region = g.r.Region
	}
	pool := SpotPrice{Region: in.Region, Zone: in.Zone, Type: in.Type}
	key := reclaimKey(pool)
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		for id, at := range st.Reclaimed {
			if now.Sub(at) > reclaimHoldMax {
				delete(st.Reclaimed, id)
			}
		}
		if _, seen := st.Reclaimed[in.ID]; seen {
			return // the same notice, from another run on that VM or the pool manager
		}
		if st.Reclaimed == nil {
			st.Reclaimed = map[string]time.Time{}
		}
		st.Reclaimed[in.ID] = now
		step := reclaimHoldMin
		if c, ok := st.Cooldown[key]; ok && c.Step > 0 && now.Before(c.Until.Add(reclaimHoldMax)) {
			step = min(2*c.Step, reclaimHoldMax)
		}
		st.Cooldown[key] = cooldown{Until: now.Add(step), Step: step,
			Why: fmt.Sprintf("Spot interruption notice to %s at %s", in.ID, now.Local().Format("15:04"))}
		// An unknown or future launch time says nothing about how soon.
		if up := now.Sub(in.LaunchedAt); !in.LaunchedAt.IsZero() && up >= 0 && up <= earlyReclaim {
			recordFailure(st, pool, false, now)
		}
	})
}

// heldBack returns the hold-back of each listed price whose zone and type
// recently had a VM reclaimed.
func (g *AWSGlobal) heldBack(ps []SpotPrice) map[string]cooldown {
	out := map[string]cooldown{}
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		for _, p := range ps {
			if c, ok := st.Cooldown[reclaimKey(p)]; ok && now.Before(c.Until) {
				out[poolKey(p)] = c
			}
		}
	})
	return out
}

// HeldBack says, for each listed price ranked last for now after a Spot
// interruption, why and until when.
func (g *AWSGlobal) HeldBack(ps []SpotPrice) map[string]string {
	out := map[string]string{}
	for k, c := range g.heldBack(ps) {
		out[k] = fmt.Sprintf("%s; ranked last until %s", c.Why, c.Until.Local().Format("15:04"))
	}
	return out
}

// heldLast returns the indexes of ps in launch order: those not held back
// first, in their order, then the held-back ones.
func heldLast(ps []SpotPrice, held map[string]cooldown) []int {
	var order, last []int
	for i, p := range ps {
		if _, ok := held[poolKey(p)]; ok {
			last = append(last, i)
		} else {
			order = append(order, i)
		}
	}
	return append(order, last...)
}

// coolMemory is how long after a cooldown ends its backoff step is kept.
func coolMemory(key string) time.Duration {
	if strings.HasPrefix(key, reclaimPrefix) {
		return reclaimHoldMax
	}
	return poolCoolMax
}
