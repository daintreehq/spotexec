package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Client reachability: a region whose VMs this client can't reach (by SSH,
// whatever the cloud's own health checks say) gets no new launches for a
// while, so waiting jobs are served from other regions instead of by VMs
// nobody can use. The client pool decides when a region is unreachable
// (see client.Pool.Observe); the fleet keeps the hold, which every process
// launching for the runner shares.
//
// A hold ends as soon as a VM in the region answers. Otherwise it lapses
// after unreachHoldMin, and then one VM may launch there to try it again
// (a trial): while that VM boots the region stays closed, and if it never
// answers either the region is held again, twice as long (up to
// unreachHoldMax).
const (
	unreachHoldMin = 10 * time.Minute
	unreachHoldMax = time.Hour
	unreachPrefix  = "unreachable:"
)

// RegionHolder is implemented by fleets that can keep new launches out of
// a region the client can't reach.
type RegionHolder interface {
	// HoldRegion keeps launches out of region: the client can't reach its
	// VMs (why says what it saw). Holding a held region again extends it.
	HoldRegion(region, why string)
	// ReachedRegion ends region's hold: the client reached a VM there.
	ReachedRegion(region string)
	// RegionHolds returns the regions held now, and whether the fleet has
	// a region left to launch into (open).
	RegionHolds() (holds map[string]RegionHold, open bool)
}

var (
	_ RegionHolder = (*AWSGlobal)(nil)
	_ RegionHolder = (*AWSSpot)(nil)
	_ RegionHolder = (*Sim)(nil)
)

// RegionHold is why and until when a region takes no launches.
type RegionHold struct {
	Why   string    `json:"why"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

func unreachKey(region string) string { return unreachPrefix + region }

// holdUnreachable holds key's region at now, or extends its hold.
func holdUnreachable(m map[string]cooldown, key, why string, now time.Time) {
	c, ok := m[key]
	switch {
	case ok && now.Before(c.Until):
		// Still held: for another step from now.
		c.Until, c.Why = now.Add(c.Step), why
	case ok && c.Step > 0:
		// Lapsed and unreachable again (its trial VM never answered, say):
		// twice as long.
		step := min(2*c.Step, unreachHoldMax)
		c = cooldown{Until: now.Add(step), Step: step, Why: why, Since: c.Since}
	default:
		c = cooldown{Until: now.Add(unreachHoldMin), Step: unreachHoldMin, Why: why, Since: now}
	}
	if c.Since.IsZero() {
		c.Since = now
	}
	m[key] = c
}

// unreachHeld reports whether key's region takes no launch now: it is
// held, or a trial VM launched there is still booting (trialFor: how long
// one may take to answer).
func unreachHeld(m map[string]cooldown, key string, now time.Time, trialFor time.Duration) (cooldown, bool) {
	c, ok := m[key]
	if !ok {
		return c, false
	}
	return c, now.Before(c.Until) || (!c.Trial.IsZero() && now.Before(c.Trial.Add(trialFor)))
}

// claimTrial reports whether a launch may go to key's region now, marking
// it the region's trial when its hold has lapsed (trial: the region then
// takes no other launch until that VM answers or trialFor passes). A region
// never held needs no trial.
func claimTrial(m map[string]cooldown, key string, now time.Time, trialFor time.Duration) (ok, trial bool) {
	c, found := m[key]
	if !found {
		return true, false
	}
	if _, held := unreachHeld(m, key, now, trialFor); held {
		return false, false
	}
	c.Trial = now
	m[key] = c
	return true, true
}

// unreachWhy says why a held region takes no launches, for prices, status
// and launch errors.
func unreachWhy(c cooldown, now time.Time) string {
	since := c.Since.Local().Format("15:04")
	if now.Before(c.Until) {
		return fmt.Sprintf("unreachable from this client since %s (%s): no launches there until a VM there answers, or until %s",
			since, c.Why, c.Until.Local().Format("15:04"))
	}
	return fmt.Sprintf("unreachable from this client since %s (%s): one VM launched at %s to try it again",
		since, c.Why, c.Trial.Local().Format("15:04"))
}

// regionHolds lists the held regions in m, and whether any of regions (the
// fleet's launch regions; none known: assumed open) is not held.
func regionHolds(m map[string]cooldown, regions []string, now time.Time, trialFor time.Duration) (map[string]RegionHold, bool) {
	out := map[string]RegionHold{}
	for k := range m {
		region, ok := strings.CutPrefix(k, unreachPrefix)
		if !ok {
			continue
		}
		if c, held := unreachHeld(m, k, now, trialFor); held {
			out[region] = RegionHold{Why: unreachWhy(c, now), Since: c.Since, Until: c.Until}
		}
	}
	open := len(regions) == 0
	for _, rg := range regions {
		if _, held := out[rg]; !held {
			open = true
		}
	}
	return out, open
}

// trialFor is how long a trial VM may take to answer before its region
// takes launches again (and, unanswered, is held again).
func trialFor(bootTimeout time.Duration) time.Duration {
	return bootTimeout + time.Minute
}

// ---- AWS ----

// HoldRegion implements RegionHolder.
func (g *AWSGlobal) HoldRegion(region, why string) {
	if region == "" {
		region = g.r.Region
	}
	now := g.clock()
	_ = g.withState(func(st *placementState) { holdUnreachable(st.Cooldown, unreachKey(region), why, now) })
}

// ReachedRegion implements RegionHolder.
func (g *AWSGlobal) ReachedRegion(region string) {
	if region == "" {
		region = g.r.Region
	}
	_ = g.withState(func(st *placementState) { delete(st.Cooldown, unreachKey(region)) })
}

// RegionHolds implements RegionHolder.
func (g *AWSGlobal) RegionHolds() (map[string]RegionHold, bool) {
	var holds map[string]RegionHold
	open := true
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		var regions []string
		switch {
		case g.oneRegion():
			regions = []string{g.r.Region}
		case g.r.AnyRegion():
			for _, rg := range st.Regions {
				if !contains(g.r.ExcludeRegions, rg) {
					regions = append(regions, rg)
				}
			}
		default:
			regions = g.r.Regions
		}
		holds, open = regionHolds(st.Cooldown, regions, now, g.trialFor())
	})
	if holds == nil {
		holds = map[string]RegionHold{}
	}
	return holds, open
}

func (g *AWSGlobal) trialFor() time.Duration { return trialFor(g.r.BootTimeout.Duration) }

// unreachRegions returns the regions that take no launch now because the
// client can't reach them, and those whose hold has lapsed (the next
// launch there is a trial).
func (g *AWSGlobal) unreachRegions() (held, lapsed map[string]bool) {
	held, lapsed = map[string]bool{}, map[string]bool{}
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		for k := range st.Cooldown {
			region, ok := strings.CutPrefix(k, unreachPrefix)
			if !ok {
				continue
			}
			if _, h := unreachHeld(st.Cooldown, k, now, g.trialFor()); h {
				held[region] = true
			} else {
				lapsed[region] = true
			}
		}
	})
	return held, lapsed
}

// claimTrial reports whether a launch may go to region, whose hold has
// lapsed, making it the region's trial.
func (g *AWSGlobal) claimTrial(region string) bool {
	ok := false
	now := g.clock()
	_ = g.withState(func(st *placementState) { ok, _ = claimTrial(st.Cooldown, unreachKey(region), now, g.trialFor()) })
	return ok
}

// dropTrial lets region take launches again after its trial launch failed
// (nothing launched there to try it).
func (g *AWSGlobal) dropTrial(region string) {
	_ = g.withState(func(st *placementState) {
		if c, ok := st.Cooldown[unreachKey(region)]; ok {
			c.Trial = time.Time{}
			st.Cooldown[unreachKey(region)] = c
		}
	})
}

// HoldRegion implements RegionHolder.
func (a *AWSSpot) HoldRegion(region, why string) { a.ranker().HoldRegion(region, why) }

// ReachedRegion implements RegionHolder.
func (a *AWSSpot) ReachedRegion(region string) { a.ranker().ReachedRegion(region) }

// RegionHolds implements RegionHolder.
func (a *AWSSpot) RegionHolds() (map[string]RegionHold, bool) { return a.ranker().RegionHolds() }

// ---- simulator ----

// simRegions are the simulated regions, in launch order
// (SPOTEXEC_SIM_REGIONS, comma-separated). Without them simulated VMs have
// no region.
func simRegions() []string {
	var out []string
	for _, rg := range strings.Split(os.Getenv("SPOTEXEC_SIM_REGIONS"), ",") {
		if rg = strings.TrimSpace(rg); rg != "" {
			out = append(out, rg)
		}
	}
	return out
}

// simPickRegion is the first of regions a launch may go to now (claiming
// its trial when its hold has lapsed), or why none may.
func simPickRegion(m map[string]cooldown, regions []string, now time.Time, trialFor time.Duration) (string, error) {
	var held []string
	for _, rg := range regions {
		if ok, _ := claimTrial(m, unreachKey(rg), now, trialFor); ok {
			return rg, nil
		}
		c, _ := unreachHeld(m, unreachKey(rg), now, trialFor)
		held = append(held, rg+": "+unreachWhy(c, now))
	}
	return "", fmt.Errorf("every simulated region is held back (%s)", strings.Join(held, "; "))
}

// withHolds runs fn on the simulator's region holds (sim-holds.json in the
// runner's state directory, under sim-holds.lock) and saves them.
func (s *Sim) withHolds(fn func(m map[string]cooldown)) error {
	dir := RunnerStateDir(s.r.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "sim-holds.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	path := filepath.Join(dir, "sim-holds.json")
	m := map[string]cooldown{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
		if m == nil {
			m = map[string]cooldown{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fn(m)
	now := time.Now()
	for k, c := range m {
		if now.After(c.Until.Add(unreachHoldMax)) && (c.Trial.IsZero() || now.After(c.Trial.Add(unreachHoldMax))) {
			delete(m, k)
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := path + ".tmp" + fmt.Sprint(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HoldRegion implements RegionHolder.
func (s *Sim) HoldRegion(region, why string) {
	now := time.Now()
	_ = s.withHolds(func(m map[string]cooldown) { holdUnreachable(m, unreachKey(region), why, now) })
}

// ReachedRegion implements RegionHolder.
func (s *Sim) ReachedRegion(region string) {
	_ = s.withHolds(func(m map[string]cooldown) { delete(m, unreachKey(region)) })
}

// RegionHolds implements RegionHolder.
func (s *Sim) RegionHolds() (map[string]RegionHold, bool) {
	var holds map[string]RegionHold
	open := true
	now := time.Now()
	_ = s.withHolds(func(m map[string]cooldown) {
		holds, open = regionHolds(m, simRegions(), now, trialFor(s.r.BootTimeout.Duration))
	})
	if holds == nil {
		holds = map[string]RegionHold{}
	}
	return holds, open
}

// simCutOffFile lists the simulated regions this client can't reach, one
// per line (`spotexec sim network`).
func (s *Sim) simCutOffFile() string {
	return filepath.Join(RunnerStateDir(s.r.Name), "sim-unreachable")
}

// cutOff returns the simulated regions this client can't reach.
func (s *Sim) cutOff() map[string]bool {
	out := map[string]bool{}
	b, _ := os.ReadFile(s.simCutOffFile())
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}

// SetReachable cuts the client off from a simulated region's VMs (their
// SSH target becomes a closed port), or restores it. The VMs themselves
// keep running and listing as healthy, like a network failure between the
// client and one region.
func (s *Sim) SetReachable(region string, reachable bool) error {
	cut := s.cutOff()
	if reachable {
		delete(cut, region)
	} else {
		cut[region] = true
	}
	var lines []string
	for rg := range cut {
		lines = append(lines, rg)
	}
	sort.Strings(lines)
	if err := os.MkdirAll(filepath.Dir(s.simCutOffFile()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.simCutOffFile(), []byte(strings.Join(lines, "\n")), 0o600)
}

// unreachableTarget is where a cut-off simulated VM's SSH goes: a port
// nothing listens on.
const unreachableTarget = 1

// unreachDetail says why region takes no launch now (it is held, or its
// trial VM is still booting).
func (g *AWSGlobal) unreachDetail(region string) string {
	why := "unreachable from this client"
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		if c, ok := st.Cooldown[unreachKey(region)]; ok {
			why = unreachWhy(c, now)
		}
	})
	return why
}
