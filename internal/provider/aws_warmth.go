package provider

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// What makes a region warm (#141) is account-wide: a bake of the runner
// there, the project's archive in the region's dependency cache bucket, and
// the project's image in the region's ECR repository. Run-cost ranking
// checks each of these in every region it prices, so every machine sharing
// the account sees the same warmth, and a region that ran VMs for another
// project, or before the lockfile or Dockerfile changed, isn't counted warm
// for what it doesn't hold. Lookups are cached in the placement state for a
// few minutes, so a launch rarely waits on them.

const (
	// evidenceTTL is how long a lookup's answer is used.
	evidenceTTL = 10 * time.Minute
	// evidenceRetry is how long a failed lookup is left before it is
	// tried again (until then the part counts as not checked).
	evidenceRetry = time.Minute
	// warmthBudget bounds the lookups of one price table: past it, the
	// parts not yet looked up count as not checked.
	warmthBudget = 15 * time.Second
)

// Project is what a launch checks regions for: the project it is for.
// Without one (the pool manager's launches, `spotexec prices` outside a
// project) the project's parts are judged by where VMs of the runner ran.
type Project struct {
	RepoID string
	// Deps: the run installs dependencies. DepsKey is their cache key when
	// it can be known before a VM exists (on the host); for an image built
	// from a Dockerfile the key depends on that region's image, so
	// DepsKeyFor makes it from the image's ID. Neither set: not checked.
	Deps       bool
	DepsKey    string
	DepsKeyFor func(imageID string) (string, error)
	// Image: the run builds the project's image from its Dockerfile, with
	// content tag ImageTag ("" = unknown: not checked).
	Image    bool
	ImageTag string
}

type projectKey struct{}

// WithProject returns ctx carrying the project launches under it are for.
func WithProject(ctx context.Context, p *Project) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, projectKey{}, p)
}

func projectOf(ctx context.Context) *Project {
	p, _ := ctx.Value(projectKey{}).(*Project)
	return p
}

// repoOf is the repository ID of the project in ctx ("" without one).
func repoOf(ctx context.Context) string {
	if p := projectOf(ctx); p != nil {
		return p.RepoID
	}
	return ""
}

type partState int

const (
	partUnknown partState = iota // not checked: VM history decides the project's parts
	partHas                      // the region holds it
	partLacks                    // the region doesn't hold it
	partUnused                   // the run doesn't need it
)

// partSeen is what was found for one part of a cold start in a region.
type partSeen struct {
	state partState
	why   string // overrides the default description ("" = the default)
}

// regionWarmth is what a region holds of a cold start's parts.
type regionWarmth struct {
	boot, deps, image partSeen
}

// evidence is one cached lookup.
type evidence struct {
	Has   bool      `json:"has,omitempty"`
	ID    string    `json:"id,omitempty"`    // an image's ID
	Until time.Time `json:"until,omitempty"` // a bake: when launches stop using it
	Why   string    `json:"why,omitempty"`   // a bake that isn't used
	Err   string    `json:"err,omitempty"`   // the lookup failed
	At    time.Time `json:"at"`
}

func (e evidence) fresh(now time.Time) bool {
	ttl := evidenceTTL
	if e.Err != "" {
		ttl = evidenceRetry
	}
	return now.Sub(e.At) < ttl && !e.At.After(now.Add(time.Hour))
}

func (e evidence) seen(has, lacks string, now time.Time) partSeen {
	switch {
	case e.Err != "":
		return partSeen{partUnknown, "lookup failed: " + e.Err}
	case e.Has && (e.Until.IsZero() || now.Before(e.Until)):
		return partSeen{partHas, has}
	case e.Has:
		return partSeen{partLacks, "the bake of the runner here is too old to use"}
	case e.Why != "":
		return partSeen{partLacks, e.Why}
	}
	return partSeen{partLacks, lacks}
}

// warmth looks up what each region in t holds of a cold start's parts, for
// the project in ctx (if any). Only run-cost ranking across several regions
// weighs it.
func (g *AWSGlobal) warmth(ctx context.Context, t PriceTable, refresh bool) map[string]regionWarmth {
	if g.rankOrder() != config.LaunchRunCost || g.oneRegion() || len(t.Prices) == 0 {
		return nil
	}
	var regions []string
	for _, p := range t.Prices {
		if !contains(regions, p.Region) {
			regions = append(regions, p.Region)
		}
	}
	sort.Strings(regions)
	started := time.Now() // against ForgetBakeWarmth, which runs on the wall clock
	now := g.clock()
	lctx, cancel := context.WithTimeout(ctx, warmthBudget)
	defer cancel()
	// Answers belong to the account, not the profile's name: the same
	// profile (or none) can point at another account. Unknown: nothing is
	// kept. Asked in a priced region: the home region may not be enabled.
	scope := ""
	if acct, err := g.Regional(regions[0]).accountID(lctx); err == nil {
		scope = g.r.Profile + "|" + acct
	}
	cached := map[string]evidence{}
	_ = g.withState(func(st *placementState) {
		if st.EvidenceProfile != scope {
			st.Evidence, st.EvidenceProfile = nil, scope
		}
		if refresh || scope == "" {
			return
		}
		for k, e := range st.Evidence {
			if e.fresh(now) {
				cached[k] = e
			}
		}
	})
	var mu sync.Mutex
	looked := map[string]evidence{}
	live := func(key string, fn func() evidence) evidence {
		if e, ok := cached[key]; ok {
			return e
		}
		e := fn()
		e.At = now
		mu.Lock()
		looked[key] = e
		mu.Unlock()
		return e
	}
	// First what is known without asking, for every region: a region the
	// budget runs out before keeps its cached answers and unused parts.
	cachedOnly := func(key string, _ func() evidence) evidence {
		if e, ok := cached[key]; ok {
			return e
		}
		return evidence{Err: "not looked up in time"}
	}
	out := map[string]regionWarmth{}
	for _, rg := range regions {
		out[rg] = g.regionWarmth(lctx, rg, cachedOnly, now)
	}
	_ = eachRegion(lctx, regions, func(ctx context.Context, rg string) error {
		w := g.regionWarmth(ctx, rg, live, now)
		mu.Lock()
		out[rg] = w
		mu.Unlock()
		return nil
	})
	// A lookup cut short by the budget or a cancelled launch says nothing
	// about the region: only answers are kept.
	_ = g.withState(func(st *placementState) {
		if scope == "" || st.EvidenceProfile != scope {
			return
		}
		for k, e := range looked {
			if e.Err != "" && lctx.Err() != nil {
				continue
			}
			if strings.HasPrefix(k, "bake|") && st.BakesForgotAt.After(started) {
				continue // a bake was made or removed while this looked
			}
			if st.Evidence == nil {
				st.Evidence = map[string]evidence{}
			}
			if cur, ok := st.Evidence[k]; !ok || !cur.At.After(e.At) {
				st.Evidence[k] = e
			}
		}
	})
	return out
}

// lookupFn answers one lookup by key, from the cache or by calling fn.
type lookupFn func(key string, fn func() evidence) evidence

func failedLookup(err error) evidence { return evidence{Err: firstLineOf([]byte(err.Error()))} }

// regionWarmth is what region rg holds for the project in ctx.
func (g *AWSGlobal) regionWarmth(ctx context.Context, rg string, lookup lookupFn, now time.Time) regionWarmth {
	proj := projectOf(ctx)
	a := g.Regional(rg)
	var w regionWarmth
	if g.r.AMI != "" && g.r.AMI != "auto" {
		w.boot = partSeen{partLacks, "the runner's ami is pinned: no bake is used"}
	} else {
		e := lookup(strings.Join([]string{"bake", rg, g.r.Name, a.BakeHash(), a.ec2Arch()}, "|"), func() evidence {
			b, err := a.findBake(ctx)
			switch {
			case err != nil:
				return failedLookup(err)
			case b == nil:
				return evidence{}
			case b.Expired:
				return evidence{Why: "the bake of the runner here is too old to use (" + b.Stale + ")"}
			case b.CheckErr != "":
				// Whether a newer base retires it is unknown: ask again soon.
				return evidence{Err: b.CheckErr}
			}
			return evidence{Has: true, ID: b.ID, Until: b.usableUntil()}
		})
		w.boot = e.seen("a bake of the runner", "no bake of the runner", now)
	}
	switch {
	case proj == nil:
	case !proj.Image:
		w.image = partSeen{state: partUnused}
	case proj.ImageTag != "":
		e := lookup(strings.Join([]string{"image", rg, ecrName(proj.RepoID), proj.ImageTag}, "|"), func() evidence {
			id, err := a.projectImage(ctx, proj.RepoID, proj.ImageTag)
			if err != nil {
				return failedLookup(err)
			}
			return evidence{Has: id != "", ID: id}
		})
		w.image = e.seen("the project's image", "no image of the project", now)
		if proj.Deps && proj.DepsKey == "" && proj.DepsKeyFor != nil {
			switch w.image.state {
			case partHas:
				if key, err := proj.DepsKeyFor(e.ID); err == nil {
					w.deps = g.depsSeen(ctx, a, lookup, proj.RepoID, key, now)
				}
			case partLacks:
				// The image built here will be new, and so will the
				// dependency archive keyed by it.
				w.deps = partSeen{partLacks, "no image of the project, so no dependency archive for it"}
			}
		}
	}
	switch {
	case proj == nil:
	case !proj.Deps:
		w.deps = partSeen{state: partUnused}
	case proj.DepsKey != "":
		w.deps = g.depsSeen(ctx, a, lookup, proj.RepoID, proj.DepsKey, now)
	}
	return w
}

func (g *AWSGlobal) depsSeen(ctx context.Context, a *AWSSpot, lookup lookupFn, repoID, key string, now time.Time) partSeen {
	e := lookup(strings.Join([]string{"deps", a.r.Region, g.r.CacheBucket, repoID, key}, "|"), func() evidence {
		has, err := a.hasCacheEntry(ctx, repoID, key)
		if err != nil {
			return failedLookup(err)
		}
		return evidence{Has: has}
	})
	lacks := "no dependency archive for the project"
	switch g.r.CacheBucket {
	case "off", "none", "false":
		lacks = "the dependency cache is off (cache_bucket)"
	}
	return e.seen("the project's dependency archive", lacks, now)
}

// ForgetBakeWarmth drops what placement found of r's bakes, after a bake
// was made or removed, so the next ranking looks again.
func ForgetBakeWarmth(r *config.Runner) {
	if !r.MultiRegion() {
		return
	}
	g, err := NewAWSPricer(r)
	if err != nil {
		return
	}
	_ = g.withState(func(st *placementState) {
		st.BakesForgotAt = time.Now()
		for k := range st.Evidence {
			if strings.HasPrefix(k, "bake|") {
				delete(st.Evidence, k)
			}
		}
	})
}
