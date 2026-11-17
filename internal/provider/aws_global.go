package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

const (
	priceTTL      = 15 * time.Minute // Spot prices reused for new placements
	regionsTTL    = 24 * time.Hour   // the account's enabled regions
	maxAttempts   = 12               // candidate launches tried per Launch
	regionWorkers = 6                // regions queried at once
	poolCoolMin   = time.Minute      // first cooldown of a capacity-starved zone/type
	poolCoolMax   = 10 * time.Minute
	regionCool    = 10 * time.Minute // a region that failed for quota or setup reasons
)

// AWSGlobal is one logical aws-spot fleet spread over several regions
// (regions = [...] or ["all"]). Each region is served by an ordinary
// regional AWSSpot; AWSGlobal decides where new instances go (by
// launch_order: by default the lowest expected cost of the run, see Rank;
// falling back down the ranking on capacity or quota errors) and routes per-instance
// operations to the region that holds the instance. max_instances is
// enforced by the client pool across all regions together.
//
// Local state (placement.json, guarded by placement.lock) remembers which
// regions may hold instances, so listings only query those, plus cached
// prices, enabled regions and capacity cooldowns.
type AWSGlobal struct {
	r     *config.Runner
	creds *awsCredResolver
	dir   string

	mu       sync.Mutex
	regional map[string]*AWSSpot

	// legacy: a single-region runner that still has instances in regions
	// it used before (its config moved from regions to region). Launches
	// behave exactly as for a single-region runner; listings and teardown
	// still cover the old regions until they are empty.
	legacy bool
	// single: the launch placement of a plain single-region runner (see
	// AWSSpot.ranker): one region, its subnet_id and security groups as
	// configured, and nothing recorded about where instances are.
	single bool

	// tests
	endpoints func(region string) map[string]string
	now       func() time.Time
}

// NewAWSGlobal constructs a multi-region provider.
func NewAWSGlobal(r *config.Runner) (Provider, error) {
	if err := checkAWSRunner(r); err != nil {
		return nil, err
	}
	return &AWSGlobal{r: r, creds: &awsCredResolver{profile: r.Profile}, dir: RunnerStateDir(r.Name), regional: map[string]*AWSSpot{}}, nil
}

// NewAWSPricer returns the placement of an aws-spot runner's launches, for
// `spotexec prices`: the runner's own regions, or its one region.
func NewAWSPricer(r *config.Runner) (*AWSGlobal, error) {
	if r.MultiRegion() {
		p, err := NewAWSGlobal(r)
		if err != nil {
			return nil, err
		}
		return p.(*AWSGlobal), nil
	}
	if err := checkAWSRunner(r); err != nil {
		return nil, err
	}
	return newAWSSpot(r, &awsCredResolver{profile: r.Profile}).ranker(), nil
}

// oneRegion reports placement within the runner's one region.
func (g *AWSGlobal) oneRegion() bool { return g.legacy || g.single }

// heldElsewhere reports whether a single-region runner's placement state
// (left from when it used several regions) says it may still have
// instances in another region.
func heldElsewhere(r *config.Runner) bool {
	b, err := os.ReadFile(filepath.Join(RunnerStateDir(r.Name), "placement.json"))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist) // unreadable: keep looking everywhere
	}
	var st placementState
	if json.Unmarshal(b, &st) != nil {
		return true
	}
	for rg := range st.Active {
		if rg != r.Region {
			return true
		}
	}
	return false
}

// KnownRegions lists the regions a runner's local placement state says
// may hold its instances (no API calls).
func KnownRegions(r *config.Runner) []string {
	b, err := os.ReadFile(filepath.Join(RunnerStateDir(r.Name), "placement.json"))
	if err != nil {
		return nil
	}
	var st placementState
	_ = json.Unmarshal(b, &st)
	var out []string
	for rg := range st.Active {
		out = append(out, rg)
	}
	sort.Strings(out)
	return out
}

// EnabledRegions lists the account's enabled regions using profile's
// credentials, asking from home.
// It also asks from us-east-1 and the regions in also when home doesn't
// answer (it may not be enabled itself).
func EnabledRegions(ctx context.Context, profile, home string, also ...string) ([]string, error) {
	creds := &awsCredResolver{profile: profile}
	regional := func(rg string) *AWSSpot { return newAWSSpot(&config.Runner{Region: rg}, creds) }
	return enabledRegionsVia(ctx, regional, discoveryHomes(home, also))
}

// FilterEnabled keeps the regions in want that are in enabled.
func FilterEnabled(want, enabled []string) []string {
	var out []string
	for _, rg := range want {
		if contains(enabled, rg) {
			out = append(out, rg)
		}
	}
	return out
}

// forgetDiscovery is called when a runner is used in plain single-region
// mode, which launches without recording anything here: if it goes back to
// several regions, its first listing must look everywhere again.
func forgetDiscovery(r *config.Runner) {
	dir := RunnerStateDir(r.Name)
	b, err := os.ReadFile(filepath.Join(dir, "placement.json"))
	if err != nil {
		return
	}
	var st placementState
	if json.Unmarshal(b, &st) != nil || (!st.Discovered && st.RegionsAt.IsZero()) {
		return
	}
	g := &AWSGlobal{r: r, dir: dir}
	// The enabled regions too: the region list filtered through them must
	// not hide one used meanwhile.
	_ = g.withState(func(st *placementState) { st.Discovered, st.RegionsAt = false, time.Time{} })
}

// RegionalAWS returns a provider for one region of an aws-spot runner,
// whatever regions it is configured for (e.g. to clear images left in a
// region it no longer uses).
func RegionalAWS(r *config.Runner, region string) (*AWSSpot, error) {
	if err := checkAWSRunner(r); err != nil {
		return nil, err
	}
	rc := *r
	rc.Region, rc.Regions = region, nil
	return newAWSSpot(&rc, &awsCredResolver{profile: r.Profile}), nil
}

// newLegacyAWSGlobal serves a single-region runner that still has instances
// in regions it used before: launches as before, in r.Region only, while
// listings and teardown also cover the old regions until they are empty.
func newLegacyAWSGlobal(r *config.Runner) (Provider, error) {
	rc := *r
	rc.Regions = []string{r.Region}
	p, err := NewAWSGlobal(&rc)
	if err != nil {
		return nil, err
	}
	p.(*AWSGlobal).legacy = true
	return p, nil
}

func (g *AWSGlobal) Runner() *config.Runner { return g.r }

func (g *AWSGlobal) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// Regional returns the provider for one region of this runner.
func (g *AWSGlobal) Regional(region string) *AWSSpot {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a := g.regional[region]; a != nil {
		return a
	}
	rc := *g.r
	rc.Region, rc.Regions = region, nil
	a := newAWSSpot(&rc, g.creds)
	a.dir = g.dir // its caches live with the runner's placement state
	a.placed = !g.oneRegion()
	a.launching = g.mark(region)
	if g.endpoints != nil {
		a.api.endpoints = g.endpoints(region)
	}
	g.regional[region] = a
	return a
}

// Use returns the provider for region and records that the region may now
// hold this runner's instances (e.g. an image-bake builder).
func (g *AWSGlobal) Use(region string) (*AWSSpot, error) {
	return g.Regional(region), g.launching(region)
}

// home is the region used for account-wide lookups (DescribeRegions).
func (g *AWSGlobal) home() *AWSSpot { return g.Regional(g.r.Region) }

// Where describes the regions the runner may use, for messages.
func (g *AWSGlobal) Where() string {
	if g.oneRegion() {
		return g.r.Region
	}
	return RunnerWhere(g.r)
}

// RunnerWhere describes the regions an aws-spot runner's config lets it
// launch in, for messages: its one region, its list, or all enabled
// regions less exclude_regions.
func RunnerWhere(r *config.Runner) string {
	switch {
	case r.AnyRegion() && len(r.ExcludeRegions) > 0:
		return "all enabled regions except " + strings.Join(r.ExcludeRegions, ", ")
	case r.AnyRegion():
		return "all enabled regions"
	case r.MultiRegion():
		return strings.Join(r.Regions, ", ")
	}
	return r.Region
}

// ---- local placement state ----

type placedAt struct {
	Region string    `json:"region"`
	Until  time.Time `json:"until"`
}

type cooldown struct {
	Until time.Time     `json:"until"`
	Step  time.Duration `json:"step"`
	Why   string        `json:"why,omitempty"`
	Setup bool          `json:"setup,omitempty"` // region-wide and not Spot-specific: no on-demand there either
	// Transient: preparing a launch there failed for a reason that may
	// clear (throttling, a network error), not how the region is set up.
	Transient bool `json:"transient,omitempty"`
	// A region the client can't reach (see reach.go): when that began,
	// and when a VM last launched there to try it again.
	Since time.Time `json:"since,omitempty"`
	Trial time.Time `json:"trial,omitempty"`
	// unreach: a coolingDown result that is such a hold (not saved).
	unreach bool
}

// quotaSkip is a quota refusal seen during one launch: the region has no
// room for a VM of vcpus or more under that quota group (vcpus 0: a count
// limit, every type). Smaller types, and types under another quota, may
// still fit. It lasts for that launch only: across launches the client
// pool holds launches back until a VM ends (and frees quota) or a few
// minutes pass, which a longer local skip would override.
type quotaSkip struct {
	vcpus int
	group string
}

func (q quotaSkip) covers(itype string) bool {
	return q.vcpus == 0 || (quotaGroup(itype) == q.group && vcpusOf(itype) >= q.vcpus)
}

// quotaGroup names the vCPU quota an instance type's Spot launches count
// against: the standard quota, or (approximately) one per other family.
func quotaGroup(itype string) string {
	if c := quotaCodeFor(itype, true); c != "" {
		return c
	}
	fam, _, _ := strings.Cut(itype, ".")
	if i := strings.IndexAny(fam, "0123456789"); i > 0 {
		fam = fam[:i] // g5 -> g, inf2 -> inf
	}
	return "family:" + fam
}

type placementState struct {
	Regions   []string          `json:"regions,omitempty"` // enabled regions (regions = ["all"])
	RegionsAt time.Time         `json:"regions_at,omitempty"`
	Prices    []SpotPrice       `json:"prices,omitempty"`
	PricedAt  time.Time         `json:"priced_at,omitempty"`
	PricedFor string            `json:"priced_for,omitempty"`
	PricedMem map[string]int    `json:"priced_mem,omitempty"` // vcpus runners: instance type -> memory (MiB)
	Unpriced  map[string]string `json:"unpriced,omitempty"`   // region -> why it has no price
	// Active: region -> may hold our instances until. An expired entry is
	// only dropped once a listing of that region finds nothing, so a VM
	// whose watchdog failed is never forgotten.
	Active map[string]time.Time `json:"active,omitempty"`
	// Busy: region -> when a launch was last sent there or an instance
	// last seen there. A region the config no longer launches in is
	// dropped from Active once it's confirmed empty and this has settled.
	Busy      map[string]time.Time `json:"busy,omitempty"`
	Where     map[string]placedAt  `json:"where,omitempty"`     // instance ID -> region (a routing cache)
	Cooldown  map[string]cooldown  `json:"cooldown,omitempty"`  // "region", "zone:<zone>", "region/zone/type" or "reclaimed:region/zone/type"
	Reclaimed map[string]time.Time `json:"reclaimed,omitempty"` // instance ID -> when its Spot interruption was recorded
	// Failures: "region/zone/type" -> its recent capacity refusals and
	// early reclaims, decaying (see recordFailure).
	Failures map[string]poolFailures `json:"failures,omitempty"`
	// Warm: region -> when a VM of this runner was last seen there, running
	// for warmAfter or more (under profile WarmProfile), so its caches are
	// likely there too.
	Warm        map[string]time.Time `json:"warm,omitempty"`
	WarmProfile string               `json:"warm_profile,omitempty"`
	RTT         map[string]rttSample `json:"rtt,omitempty"` // region -> measured round trip
	// Evidence: what lookups found in each region's account-wide caches
	// (bakes, the dependency cache, images), for the profile and account
	// EvidenceProfile (see warmth). BakesForgotAt: when a bake was last
	// made or removed here; bake lookups begun before it are dropped.
	Evidence        map[string]evidence `json:"evidence,omitempty"`
	EvidenceProfile string              `json:"evidence_profile,omitempty"`
	BakesForgotAt   time.Time           `json:"bakes_forgot_at,omitempty"`
	// ColdWork: repository ID -> how long the project's image builds and
	// dependency installs took on VMs of this runner (see RecordColdWork).
	ColdWork map[string]*coldWork `json:"cold_work,omitempty"`
	// Discovered: every managed region has been listed once. Until then
	// (first use, an upgrade from single-region, a lost state file) listings
	// cover them all rather than trusting Active.
	Discovered bool `json:"discovered,omitempty"`
}

// withState runs fn on placement.json under placement.lock and saves it.
// Never call the cloud inside fn.
func (g *AWSGlobal) withState(fn func(st *placementState)) error {
	return g.withStateWait(0, fn)
}

// errStateBusy: placement.lock stayed held past withStateWait's wait.
var errStateBusy = errors.New("placement state busy")

// withStateWait is withState for what may be skipped: it waits at most
// wait for placement.lock (0: as long as it takes), else errStateBusy.
func (g *AWSGlobal) withStateWait(wait time.Duration, fn func(st *placementState)) error {
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(g.dir, "placement.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if wait <= 0 {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
	} else {
		deadline := time.Now().Add(wait)
		for {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				return err
			}
			if time.Now().After(deadline) {
				return errStateBusy
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	path := filepath.Join(g.dir, "placement.json")
	st := &placementState{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Unreadable state is an error, never an empty fleet: it records
		// where billable instances may be.
		if err := json.Unmarshal(b, st); err != nil {
			return fmt.Errorf("%s is corrupt (%v); delete it to rediscover instances in every region", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if st.Active == nil {
		st.Active = map[string]time.Time{}
	}
	if st.Busy == nil {
		st.Busy = map[string]time.Time{}
	}
	if st.Where == nil {
		st.Where = map[string]placedAt{}
	}
	if st.Cooldown == nil {
		st.Cooldown = map[string]cooldown{}
	}
	fn(st)
	now := g.clock()
	forgetOld(st, now)
	for k, v := range st.Where {
		if now.After(v.Until) {
			delete(st.Where, k)
		}
	}
	for k, v := range st.Cooldown {
		if !v.Trial.IsZero() && now.Before(v.Trial.Add(g.trialFor())) {
			continue // a trial still booting keeps its region's hold
		}
		end := v.Until
		if v.Trial.After(end) {
			end = v.Trial
		}
		if now.After(end.Add(coolMemory(k))) {
			delete(st.Cooldown, k) // forget the backoff step too, eventually
		}
	}
	b, _ = json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp" + fmt.Sprint(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// holdUntil is how long a region may hold an instance launched now: its
// watchdog stops it by max_lifetime.
func (g *AWSGlobal) holdUntil(from time.Time) time.Time {
	return from.Add(g.r.MaxLifetime.Duration + time.Hour)
}

// markActive records that region may hold instances until at least until.
// A launch must not be sent if this fails.
func (g *AWSGlobal) markActive(region string, until time.Time) error {
	return g.withState(func(st *placementState) {
		if until.After(st.Active[region]) {
			st.Active[region] = until
		}
	})
}

// launching records that something is about to be launched in region now:
// it may hold an instance until holdUntil, and is kept listed meanwhile.
func (g *AWSGlobal) launching(region string) error {
	return g.withState(func(st *placementState) {
		now := g.clock()
		if until := g.holdUntil(now); until.After(st.Active[region]) {
			st.Active[region] = until
		}
		if now.After(st.Busy[region]) {
			st.Busy[region] = now
		}
	})
}

// launchSettle is how long after a launch (or a sighting) an empty listing
// of its region isn't believed: EC2's listings are eventually consistent,
// and another process's view may be newer than this one's.
var launchSettle = 10 * time.Minute

// configured reports whether the runner's config launches in region.
// Unlike PlacementRegions it never asks the cloud: a region the account
// hasn't enabled still counts, as it holds nothing.
func (g *AWSGlobal) configured(region string) bool {
	switch {
	case g.oneRegion():
		return region == g.r.Region
	case g.r.AnyRegion():
		return !slices.Contains(g.r.ExcludeRegions, region)
	}
	return slices.Contains(g.r.Regions, region)
}

func (g *AWSGlobal) remember(ins []Instance) error {
	if len(ins) == 0 {
		return nil
	}
	return g.withState(func(st *placementState) {
		g.sawRunning(st, ins, g.clock())
		for _, in := range ins {
			launched := in.LaunchedAt
			if launched.IsZero() {
				launched = g.clock()
			}
			// A VM still alive past its lifetime (a failed watchdog) stays
			// tracked while it is seen.
			until := g.holdUntil(launched)
			if seen := g.clock().Add(time.Hour); seen.After(until) {
				until = seen
			}
			st.Where[in.ID] = placedAt{Region: in.Region, Until: until}
			if until.After(st.Active[in.Region]) {
				st.Active[in.Region] = until
			}
			if now := g.clock(); now.After(st.Busy[in.Region]) {
				st.Busy[in.Region] = now // seen now, however old
			}
		}
	})
}

// activeRegions are the regions that may hold this runner's instances.
func (g *AWSGlobal) activeRegions() ([]string, error) {
	var out []string
	err := g.withState(func(st *placementState) {
		for r := range st.Active {
			out = append(out, r)
		}
	})
	sort.Strings(out)
	return out, err
}

// PlacementRegions are the regions new instances may go to.
func (g *AWSGlobal) PlacementRegions(ctx context.Context) ([]string, error) {
	if g.oneRegion() {
		return []string{g.r.Region}, nil
	}
	enabled, err := g.enabledRegions(ctx)
	if !g.r.AnyRegion() {
		if err != nil {
			// Can't tell which are enabled: use the list as configured.
			return append([]string(nil), g.r.Regions...), nil
		}
		return FilterEnabled(g.r.Regions, enabled), nil
	}
	var out []string
	for _, rg := range enabled {
		if !contains(g.r.ExcludeRegions, rg) {
			out = append(out, rg)
		}
	}
	return out, err
}

// enabledRegionsTimeout bounds one DescribeRegions attempt: some regions
// that aren't enabled never answer, so asking from one would otherwise
// stall until the HTTP client gives up.
var enabledRegionsTimeout = 10 * time.Second

// enabledRegions lists the account's enabled regions, cached for a day.
func (g *AWSGlobal) enabledRegions(ctx context.Context) ([]string, error) {
	var cached []string
	_ = g.withState(func(st *placementState) {
		if g.clock().Sub(st.RegionsAt) < regionsTTL {
			cached = append(cached, st.Regions...)
		}
	})
	if len(cached) > 0 {
		return cached, nil
	}
	regions, err := enabledRegionsVia(ctx, g.Regional, discoveryHomes(g.r.Region, g.r.Regions))
	if err != nil {
		return nil, err
	}
	_ = g.withState(func(st *placementState) { st.Regions, st.RegionsAt = regions, g.clock() })
	return regions, nil
}

// discoveryHomes are the regions to ask for the account's enabled regions,
// in order: the home region, then us-east-1 (which no account can disable;
// not for GovCloud, a separate partition), then the other listed regions in
// case us-east-1 is denied by policy. At most four.
func discoveryHomes(home string, listed []string) []string {
	var out []string
	add := func(rg string) {
		if rg != "" && rg != config.AllRegions && !contains(out, rg) && len(out) < 4 {
			out = append(out, rg)
		}
	}
	add(home)
	if !strings.HasPrefix(home, "us-gov-") {
		add("us-east-1")
	}
	for _, rg := range listed {
		add(rg)
	}
	return out
}

// enabledRegionsVia asks each home in turn. A region that isn't enabled
// answers AuthFailure (a *CredentialError) or not at all, so neither stops
// the search; only every home failing does.
func enabledRegionsVia(ctx context.Context, regional func(string) *AWSSpot, homes []string) ([]string, error) {
	var errs []error
	for _, home := range homes {
		actx, cancel := context.WithTimeout(ctx, enabledRegionsTimeout)
		regions, err := regional(home).enabledRegions(actx)
		cancel()
		if err == nil {
			return regions, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("from %s: %w", home, err))
	}
	err := errors.Join(errs...)
	return nil, fmt.Errorf("list enabled regions: %w%s", err, permHint(err))
}

// notEnabled lists configured regions the account hasn't enabled (or that
// don't exist), which placement leaves out.
func (g *AWSGlobal) notEnabled(placement []string) []string {
	if g.oneRegion() || g.r.AnyRegion() {
		return nil
	}
	var out []string
	for _, rg := range g.r.Regions {
		if !contains(placement, rg) {
			out = append(out, rg)
		}
	}
	return out
}

// excluded lists the regions exclude_regions leaves out of regions = ["all"].
func (g *AWSGlobal) excluded() []string {
	if g.oneRegion() || !g.r.AnyRegion() {
		return nil
	}
	return g.r.ExcludeRegions
}

const excludedWhy = "left out by exclude_regions"

const noneOfferedWhy = "none of instance_types is offered as Spot here"

// noneOffered says a region offers none of the runner's types as Spot.
func noneOffered(r *config.Runner) string {
	if r.ByRequirements() {
		return "no type with " + describeRequirements(r) + " is offered as Spot here"
	}
	return noneOfferedWhy
}

// lastingUnpriced reports reasons a region has no price that won't clear
// by themselves: not enabled, or none of the runner's types offered there.
func lastingUnpriced(why string) bool {
	return why == notEnabledWhy || why == excludedWhy || strings.Contains(why, "offered as Spot here") || strings.HasPrefix(why, "no instance type in ")
}

// widenHint says how to give a runner more options to launch.
func widenHint(r *config.Runner) string {
	if r.ByRequirements() {
		return "lower min_memory_gib or min_generation"
	}
	return "add instance_types"
}

// shortOfCapacity reports the refusals that mean AWS has no Spot capacity
// for that type and zone right now (not a price cap or an unsupported type).
func shortOfCapacity(code string) bool {
	switch code {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientHostCapacity":
		return true
	}
	return false
}

const notEnabledWhy = "not enabled in this AWS account (or not a region): enable it in the AWS console under Account > AWS Regions, or remove it from regions"

// errHeld marks a region that still holds something billable.
var errHeld = errors.New("region still holds instances")

// managedRegions are the placement regions plus any region that may still
// hold an instance (e.g. one since removed from the config).
func (g *AWSGlobal) managedRegions(ctx context.Context) ([]string, error) {
	pr, err := g.PlacementRegions(ctx)
	if ex := g.excluded(); len(ex) > 0 && err == nil {
		// A VM launched before its region was excluded is still ours.
		if enabled, eerr := g.enabledRegions(ctx); eerr == nil {
			pr = union(pr, FilterEnabled(ex, enabled))
		}
	}
	ar, aerr := g.activeRegions()
	return union(pr, ar), errors.Join(err, aerr)
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// eachRegion runs fn for every region, a few at a time, and returns the
// failures by region.
func eachRegion(ctx context.Context, regions []string, fn func(ctx context.Context, region string) error) map[string]error {
	var mu sync.Mutex
	errs := map[string]error{}
	sem := make(chan struct{}, regionWorkers)
	var wg sync.WaitGroup
	for _, rg := range regions {
		wg.Add(1)
		go func(rg string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				errs[rg] = ctx.Err()
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			if err := fn(ctx, rg); err != nil {
				mu.Lock()
				errs[rg] = err
				mu.Unlock()
			}
		}(rg)
	}
	wg.Wait()
	return errs
}

// joinRegionErrors folds per-region failures into one error, keeping a
// credential failure findable with errors.As.
func joinRegionErrors(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	var keys []string
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var list []error
	for _, k := range keys {
		list = append(list, fmt.Errorf("%s: %w", k, errs[k]))
	}
	return errors.Join(list...)
}

func credentialFailures(errs map[string]error) int {
	n := 0
	for _, err := range errs {
		var ce *CredentialError
		if errors.As(err, &ce) {
			n++
		}
	}
	return n
}

func credentialFailure(errs map[string]error) error {
	for _, err := range errs {
		var ce *CredentialError
		if errors.As(err, &ce) {
			return err
		}
	}
	return nil
}

// ---- prices ----

// PriceTable is the ranked Spot prices of a runner's instance types.
type PriceTable struct {
	Prices   []SpotPrice       `json:"prices"` // in launch order: cheapest first (launch_order = "listed": by type, then price)
	At       time.Time         `json:"at"`     // when they were fetched
	Regions  []string          `json:"regions"`
	Unpriced map[string]string `json:"unpriced,omitempty"` // region -> why it has no price
	Cached   bool              `json:"cached"`
	// MemoryMiB: for vcpus runners, the memory of each eligible type priced.
	MemoryMiB map[string]int `json:"memory_mib,omitempty"`
	// warmth: what each region holds of a cold start's parts, for the
	// project the table was looked up for (run_cost across regions).
	warmth map[string]regionWarmth
	// repoID: that project's repository ID ("" outside a project), whose
	// measured builds and installs price the cold start.
	repoID string
}

func (g *AWSGlobal) priceKey(regions []string) string {
	k := g.r.Profile + "|" + strings.Join(g.r.InstanceTypes, ",") + "|" + strings.Join(regions, ",") + "|" + g.r.LaunchOrder
	if g.r.ByRequirements() {
		k += "|" + typesKey(g.r)
	}
	if g.r.SubnetID != "" {
		k += "|" + g.r.SubnetID
	}
	return k
}

// orderPrices puts prices in launch order: cheapest first, with the
// instance_types order breaking ties; launch_order = "listed" tries the
// types in the listed order, each in its cheapest zone first.
func orderPrices(r *config.Runner, ps []SpotPrice) {
	idx := func(t string) int {
		if i := listedIndex(r, t); i >= 0 {
			return i
		}
		return len(r.InstanceTypes)
	}
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		ia, ib := idx(a.Type), idx(b.Type)
		if r.LaunchOrder == config.LaunchListed && ia != ib {
			return ia < ib
		}
		if fa, fb := a.USD(), b.USD(); fa != fb {
			return fa < fb
		}
		if ia != ib {
			return ia < ib
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if a.Zone != b.Zone {
			return a.Zone < b.Zone
		}
		return a.Type < b.Type
	})
}

// Prices returns the current Spot prices across the placement regions,
// from a cache younger than 15 minutes unless refresh is set. Regions that
// could not be priced are listed in Unpriced, never treated as cheap.
func (g *AWSGlobal) Prices(ctx context.Context, refresh bool) (PriceTable, error) {
	if refresh {
		// Also recheck which regions are enabled: one may just have been.
		_ = g.withState(func(st *placementState) { st.RegionsAt = time.Time{} })
	}
	regions, err := g.PlacementRegions(ctx)
	if err != nil {
		return PriceTable{}, err
	}
	off := g.notEnabled(regions)
	key := g.priceKey(regions) + "|off:" + strings.Join(off, ",") + "|excluded:" + strings.Join(g.excluded(), ",")
	off = union(off, g.excluded())
	var t PriceTable
	if !refresh {
		_ = g.withState(func(st *placementState) {
			if st.PricedFor == key && g.clock().Sub(st.PricedAt) < priceTTL && len(st.Prices) > 0 {
				t = PriceTable{Prices: st.Prices, At: st.PricedAt, Regions: union(regions, off), Unpriced: st.Unpriced, Cached: true, MemoryMiB: st.PricedMem}
			}
		})
		if t.Cached {
			t.warmth = g.warmth(ctx, t, false)
			t.repoID = repoOf(ctx)
			return t, nil
		}
	}
	now := g.clock()
	// A pinned subnet_id fixes the zone: only its prices are options.
	zone := ""
	if g.r.SubnetID != "" {
		if zone, err = g.home().subnetZone(ctx); err != nil {
			return PriceTable{}, err
		}
	}
	var mu sync.Mutex
	var all []SpotPrice
	var mem map[string]int
	if g.r.ByRequirements() {
		mem = map[string]int{}
	}
	errs := eachRegion(ctx, regions, func(ctx context.Context, rg string) error {
		a := g.Regional(rg)
		types := g.r.InstanceTypes
		if g.r.ByRequirements() {
			ts, err := a.EligibleTypes(ctx, refresh)
			if err != nil {
				return err
			}
			types = nil
			mu.Lock()
			for _, t := range ts {
				types = append(types, t.Type)
				mem[t.Type] = t.MemoryMiB
			}
			mu.Unlock()
		}
		ps, err := a.spotPrices(ctx, types, now)
		if err != nil {
			return err
		}
		var keep []SpotPrice
		for _, p := range ps {
			if contains(types, p.Type) && (zone == "" || p.Zone == zone) {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			if zone != "" {
				return fmt.Errorf("%s in %s (the zone of subnet_id %s)", noneOffered(g.r), zone, g.r.SubnetID)
			}
			return errors.New(noneOffered(g.r))
		}
		mu.Lock()
		all = append(all, keep...)
		mu.Unlock()
		return nil
	})
	// A region that isn't enabled answers AuthFailure too: credentials are
	// only to blame when every region rejected them.
	if err := credentialFailure(errs); err != nil && credentialFailures(errs) == len(regions) {
		return PriceTable{}, err
	}
	if ctx.Err() != nil {
		return PriceTable{}, ctx.Err()
	}
	orderPrices(g.r, all)
	t = PriceTable{Prices: all, At: now, Regions: union(regions, off), Unpriced: map[string]string{}, MemoryMiB: mem}
	for rg, err := range errs {
		t.Unpriced[rg] = firstLineOf([]byte(err.Error()))
	}
	for _, rg := range off {
		t.Unpriced[rg] = notEnabledWhy
		if contains(g.excluded(), rg) {
			t.Unpriced[rg] = excludedWhy
		}
	}
	if len(all) == 0 {
		what := strings.Join(g.r.InstanceTypes, ", ")
		if g.r.ByRequirements() {
			what = describeRequirements(g.r)
		}
		err := fmt.Errorf("no Spot price for %s in %s: %s", what, g.Where(), summarize(t.Unpriced, 3))
		return t, fmt.Errorf("%w%s", err, permHint(err))
	}
	_ = g.withState(func(st *placementState) {
		st.Prices, st.PricedAt, st.PricedFor, st.Unpriced, st.PricedMem = all, now, key, t.Unpriced, mem
		saveRTTs(st, regions)
	})
	t.warmth = g.warmth(ctx, t, refresh)
	t.repoID = repoOf(ctx)
	return t, nil
}

// summarize lists up to n "region: reason" entries.
func summarize(m map[string]string, n int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for i, k := range keys {
		if i == n {
			parts = append(parts, fmt.Sprintf("and %d more", len(keys)-n))
			break
		}
		parts = append(parts, k+": "+m[k])
	}
	return strings.Join(parts, "; ")
}

func poolKey(p SpotPrice) string { return p.Region + "/" + p.Zone + "/" + p.Type }

// CoolingDown reports why a price's zone/type or region is skipped for
// now ("" when it is not).
func (g *AWSGlobal) coolingDown(st *placementState, p SpotPrice, now time.Time) (cooldown, bool) {
	if c, ok := st.Cooldown[p.Region]; ok && now.Before(c.Until) {
		return c, true
	}
	if c, ok := unreachHeld(st.Cooldown, unreachKey(p.Region), now, g.trialFor()); ok {
		c.Why, c.unreach = unreachWhy(c, now), true
		return c, true
	}
	if c, ok := st.Cooldown[g.zoneKey(p.Zone)]; ok && now.Before(c.Until) {
		return c, true
	}
	if c, ok := st.Cooldown[poolKey(p)]; ok && now.Before(c.Until) {
		return c, true
	}
	return cooldown{}, false
}

// Cooldowns returns why each listed price is currently skipped.
func (g *AWSGlobal) Cooldowns(ps []SpotPrice) map[string]string {
	out := map[string]string{}
	for k, c := range g.cooldowns(ps) {
		out[k] = c.Why
	}
	return out
}

// cooldowns returns the cooldown each listed price is currently skipped for.
func (g *AWSGlobal) cooldowns(ps []SpotPrice) map[string]cooldown {
	out := map[string]cooldown{}
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		for _, p := range ps {
			if c, ok := g.coolingDown(st, p, now); ok {
				out[poolKey(p)] = c
			}
		}
	})
	return out
}

// refusedAtCap returns, by region/type, the listed options EC2 refused at
// max_price (SpotMaxPriceTooLow) whose pool is still cooling down for it,
// whatever region or zone cooldown may hide that.
func (g *AWSGlobal) refusedAtCap(ps []SpotPrice) map[string]bool {
	out := map[string]bool{}
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		for _, p := range ps {
			if c, ok := st.Cooldown[poolKey(p)]; ok && now.Before(c.Until) && strings.Contains(c.Why, "SpotMaxPriceTooLow") {
				out[p.Region+"/"+p.Type] = true
			}
		}
	})
	return out
}

// coolPool skips p's zone and type for a while after a refusal (why: its
// code); a refusal for capacity also counts in its failure history.
func (g *AWSGlobal) coolPool(p SpotPrice, why string) {
	_ = g.withState(func(st *placementState) {
		now := g.clock()
		c := st.Cooldown[poolKey(p)]
		step := poolCoolMin
		if c.Step > 0 {
			step = min(2*c.Step, poolCoolMax)
		}
		st.Cooldown[poolKey(p)] = cooldown{Until: now.Add(step), Step: step, Why: why}
		if shortOfCapacity(why) {
			recordFailure(st, p, true, now)
		}
	})
}

// zoneKey names a zone's cooldown. Zone cooldowns are about its subnet
// (none by default, or full), so a pinned subnet_id gets its own: another
// subnet in the zone starts afresh.
func (g *AWSGlobal) zoneKey(zone string) string {
	if g.r.SubnetID != "" {
		return "zone:" + zone + "/" + g.r.SubnetID
	}
	return "zone:" + zone
}

// coolZone skips every type in one availability zone for a while.
func (g *AWSGlobal) coolZone(zone, why string, setup bool) {
	_ = g.withState(func(st *placementState) {
		st.Cooldown[g.zoneKey(zone)] = cooldown{Until: g.clock().Add(regionCool), Step: regionCool, Why: why, Setup: setup}
	})
}

func (g *AWSGlobal) coolRegion(region, why string, setup bool) {
	g.coolRegionAs(region, cooldown{Why: why, Setup: setup})
}

func (g *AWSGlobal) coolRegionAs(region string, c cooldown) {
	c.Until, c.Step = g.clock().Add(regionCool), regionCool
	_ = g.withState(func(st *placementState) { st.Cooldown[region] = c })
}

// setupFailure reports region failures that come from how the account or
// region is set up (no default VPC, opt-in, permissions): they won't clear
// by themselves, unlike throttling or a network error while preparing.
func setupFailure(err error) bool {
	if regionWide(awsCode(err)) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no default VPC") || strings.Contains(msg, "UnauthorizedOperation") ||
		strings.Contains(msg, "AccessDenied") || strings.Contains(msg, "OptInRequired")
}

// brokenRegions are regions cooling down for reasons on-demand shares.
func (g *AWSGlobal) brokenRegions() map[string]bool {
	out := map[string]bool{}
	now := g.clock()
	_ = g.withState(func(st *placementState) {
		for k, c := range st.Cooldown {
			if c.Setup && now.Before(c.Until) {
				out[k] = true
			}
		}
	})
	return out
}

func (g *AWSGlobal) clearCool(p SpotPrice) {
	_ = g.withState(func(st *placementState) { delete(st.Cooldown, poolKey(p)) })
}

// regionWide reports AWS errors that will repeat for every type and zone of
// the region (quota, opt-in, permissions scoped by region).
func regionWide(code string) bool {
	switch code {
	case "VcpuLimitExceeded", "MaxSpotInstanceCountExceeded", "InstanceLimitExceeded",
		"OptInRequired", "UnauthorizedOperation", "PendingVerification", "Blocked":
		return true
	}
	return false
}

// spotOnly reports region-wide errors that concern Spot alone: on-demand
// may still launch there.
func spotOnly(code string) bool {
	return code == "MaxSpotInstanceCountExceeded" || code == "VcpuLimitExceeded"
}

// zoneWide reports errors confined to one zone's subnet.
func zoneWide(code string) bool {
	return code == "InsufficientFreeAddressesInSubnet"
}

// ---- Fleet ----

// List returns live instances in every region that may hold them. If any
// such region can't be listed the whole listing fails, so the pool keeps
// its last view instead of believing those instances gone.
func (g *AWSGlobal) List(ctx context.Context) ([]Instance, error) {
	out, unlisted, err := g.list(ctx, 0)
	if err != nil {
		return nil, err
	}
	if len(unlisted) > 0 {
		return nil, joinRegionErrors(unlisted)
	}
	return out, nil
}

// regionListTimeout bounds how long ListReachable waits on one region.
var regionListTimeout = 15 * time.Second

// ListReachable lists every region List would, giving each at most
// regionListTimeout, and returns what it found with the regions it
// couldn't list. A region that failed is never forgotten: while any did,
// the listing only records the instances it saw.
func (g *AWSGlobal) ListReachable(ctx context.Context) ([]Instance, map[string]error, error) {
	return g.list(ctx, regionListTimeout)
}

// list lists the regions that may hold instances. With each zero, it stops
// at the first sign of a region it couldn't list and returns those
// regions' errors with nothing else. With each set, it bounds every
// region's queries by it and goes on with the regions it listed.
func (g *AWSGlobal) list(ctx context.Context, each time.Duration) ([]Instance, map[string]error, error) {
	bound := func(ctx context.Context) (context.Context, context.CancelFunc) {
		if each <= 0 {
			return ctx, func() {}
		}
		return context.WithTimeout(ctx, each)
	}
	slow := func(ctx context.Context, err error) error {
		if each > 0 && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("no answer within %s: %w", each, err)
		}
		return err
	}
	var regions []string
	discovered := false
	start := g.clock()
	if err := g.withState(func(st *placementState) {
		discovered = st.Discovered
		for r := range st.Active {
			regions = append(regions, r)
		}
	}); err != nil {
		return nil, nil, err
	}
	if g.legacy {
		regions = union(regions, []string{g.r.Region})
	}
	if !discovered {
		// No record yet of where instances may be: look everywhere once.
		dctx, cancel := bound(ctx)
		all, err := g.managedRegions(dctx)
		cancel()
		if err != nil {
			return nil, nil, slow(ctx, err)
		}
		regions = union(regions, all)
	}
	var mu sync.Mutex
	var out []Instance
	empty := map[string]bool{}
	unlisted := eachRegion(ctx, regions, func(pctx context.Context, rg string) error {
		ctx, cancel := bound(pctx)
		defer cancel()
		ins, err := g.Regional(rg).List(ctx)
		if err != nil {
			return slow(pctx, err)
		}
		mu.Lock()
		out = append(out, ins...)
		if len(ins) == 0 {
			empty[rg] = true
		}
		mu.Unlock()
		return nil
	})
	if len(unlisted) > 0 && each <= 0 {
		return nil, unlisted, nil
	}
	if err := g.remember(out); err != nil {
		return nil, nil, err
	}
	if len(unlisted) > 0 {
		// Forgetting or discovering regions needs every region's answer.
		sortInstances(out)
		return out, unlisted, nil
	}
	// The listing above has only schedulable VMs. Before forgetting a
	// region (or, while discovering, passing over one), make sure it holds
	// nothing else that bills: stopped fleet VMs or bake builders.
	now := g.clock()
	// settled: the region's last launch (or sighting) came long enough
	// before this listing began for the listing to show what it made.
	settled := func(st *placementState, rg string) bool {
		at, ok := st.Busy[rg]
		return ok && !at.After(start.Add(-launchSettle))
	}
	// gone: a region forgettable once it's confirmed to hold nothing. It's
	// past the time anything launched there could live, or no longer in
	// the config with its last activity settled.
	gone := func(st *placementState, rg string) bool {
		until, ok := st.Active[rg]
		if !ok {
			return false
		}
		return now.After(until) || (!g.configured(rg) && settled(st, rg))
	}
	var check []string
	if err := g.withState(func(st *placementState) {
		for rg := range empty {
			_, ok := st.Active[rg]
			if ok && !g.configured(rg) {
				if _, known := st.Busy[rg]; !known {
					// No record of its last activity (state from before
					// one was kept): count from now.
					st.Busy[rg] = now
				}
			}
			if gone(st, rg) || (!ok && !discovered) {
				check = append(check, rg)
			}
		}
	}); err != nil {
		return nil, nil, err
	}
	// A region that holds something, or that couldn't be checked, stays
	// listed until it's clear.
	held := map[string]bool{}
	cleared := map[string]bool{}
	for _, rg := range check {
		cleared[rg] = true
	}
	for rg := range eachRegion(ctx, check, func(ctx context.Context, rg string) error {
		ctx, cancel := bound(ctx)
		defer cancel()
		if n, err := g.Regional(rg).leftovers(ctx); err != nil {
			return err
		} else if n > 0 {
			return errHeld
		}
		return nil
	}) {
		delete(cleared, rg)
		held[rg] = true
	}
	if err := g.withState(func(st *placementState) {
		for rg := range held {
			if _, ok := st.Active[rg]; !ok {
				st.Active[rg] = now.Add(time.Hour) // listed again, then dropped once clear
			}
		}
		st.Discovered = true
		for rg := range cleared {
			// Confirmed empty, and nothing launched there since (a launch
			// meanwhile moves its hold and last launch on): stop listing it.
			if gone(st, rg) {
				delete(st.Active, rg)
				delete(st.Busy, rg)
			}
		}
	}); err != nil {
		return nil, nil, err
	}
	sortInstances(out)
	return out, nil, nil
}

// sortInstances orders a listing by state, then newest first.
func sortInstances(out []Instance) {
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := stateRank(string(out[i].State)), stateRank(string(out[j].State))
		if ri != rj {
			return ri < rj
		}
		return out[i].LaunchedAt.After(out[j].LaunchedAt)
	})
}

// Launch starts one instance at the first option in launch order (see Rank)
// that it can get.
func (g *AWSGlobal) Launch(ctx context.Context, log Logger) (Instance, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	// One provisioning lock for the runner (all regions share its state
	// directory): launches share it with each other, and Down and Up take
	// it alone.
	unlock, err := g.home().lockLaunch(ctx)
	if err != nil {
		return Instance{}, err
	}
	defer unlock()
	return g.launch(ctx, log)
}

// mark is the beforeSend of a launch in region: record, just before the
// request, that the region may hold an instance, so listings find one the
// request creates even if its response is lost. (A plain single-region
// runner always lists its one region.)
func (g *AWSGlobal) mark(region string) func() error {
	if g.single {
		return nil
	}
	return func() error { return g.launching(region) }
}

// record remembers where a new instance is.
func (g *AWSGlobal) record(in Instance) {
	if !g.single {
		_ = g.remember([]Instance{in})
	}
}

// onDemandTypes is the order to try on-demand in region: the types by
// their Spot rank there (a stand-in for the on-demand price, which spotexec
// doesn't look up), then any usable type without a Spot price there.
//
// With max_price, a type whose Spot price is above it in any of the
// region's zones is left out, and so is one EC2 refused at the cap
// (atCap, by region/type: the cached price may be stale): a Spot price is
// never above the on-demand price, so that type would launch above the
// cap. Types without a Spot price are left out too, their price unknown.
func (g *AWSGlobal) onDemandTypes(ctx context.Context, t PriceTable, region string, atCap map[string]bool) []string {
	maxPrice := g.r.MaxPriceUSD()
	over := map[string]bool{}
	for _, p := range t.Prices {
		if p.Region == region && (atCap[region+"/"+p.Type] || (maxPrice > 0 && p.USD() > maxPrice)) {
			over[p.Type] = true
		}
	}
	var out []string
	for _, p := range t.Prices {
		if p.Region == region && !over[p.Type] && !contains(out, p.Type) {
			out = append(out, p.Type)
		}
	}
	if maxPrice > 0 {
		return out
	}
	rest, _ := g.Regional(region).launchTypes(ctx) // vcpus: the cached eligible list
	for _, it := range rest {
		if !contains(out, it) {
			out = append(out, it)
		}
	}
	return out
}

func (g *AWSGlobal) launch(ctx context.Context, log Logger) (Instance, error) {
	t, err := g.Prices(ctx, false)
	if err != nil {
		var ce *CredentialError
		if !g.oneRegion() || g.r.ByRequirements() || ctx.Err() != nil || errors.As(err, &ce) {
			return Instance{}, err
		}
		// In one region with a fixed list, prices only order the attempts
		// (and an older IAM policy may not allow reading them): try the
		// types in the listed order instead.
		why := fmt.Sprintf("can't compare Spot prices (%s); trying instance_types in order", firstLineOfErr(err))
		held, lapsed := g.unreachRegions()
		if held[g.r.Region] || (lapsed[g.r.Region] && !g.claimTrial(g.r.Region)) {
			return Instance{}, fmt.Errorf("%s is %s", g.r.Region, g.unreachDetail(g.r.Region))
		}
		log("%s", why)
		a := g.home()
		inst, err := a.launchOrUnbaked(ctx, log, launchSpec{beforeSend: g.mark(a.r.Region)})
		if err != nil {
			if lapsed[g.r.Region] && !errors.Is(err, ErrLaunchAmbiguous) {
				g.dropTrial(g.r.Region) // nothing launched there to try it
			}
			return Instance{}, err
		}
		in := a.instance(inst)
		g.record(in)
		in.Placement = &Placement{Order: config.LaunchListed, Why: why}
		return in, nil
	}
	cool := g.cooldowns(t.Prices)
	// An explicit max_price is AWS's cap on the Spot bid: options already
	// above it would only be refused.
	maxPrice := g.r.MaxPriceUSD()
	skipRegion := map[string]string{}
	skipQuota := map[string][]quotaSkip{} // region -> what its quota refused during this launch
	skipZone := map[string]bool{}
	atCap := g.refusedAtCap(t.Prices) // region/type refused at max_price (SpotMaxPriceTooLow)
	broken := g.brokenRegions()       // regions whose setup failed (no on-demand there either)
	// Regions this client can't reach take no launch (cool has them); one
	// whose hold lapsed takes one, to try it again.
	unreachHeldNow, lapsed := g.unreachRegions()
	var unreachSkipped []string // held regions passed over, for the error
	var tried []string
	// When quota is all that stopped every option (others can't work as
	// configured: no network there, or above max_price), the error is a
	// *QuotaError, so the client pool holds launches until a VM ends or a
	// few minutes pass instead of retrying, and the pool manager doesn't
	// count it against the fleet's health.
	var quota *QuotaError    // the cheapest option's quota refusal
	otherFailure := false    // something that may clear by itself: options left untried
	var capacity []SpotPrice // options refused for capacity (may return, unless quota covers them too)
	quotaSkipped := func(c SpotPrice) bool {
		for _, q := range skipQuota[c.Region] {
			if q.covers(c.Type) {
				return true
			}
		}
		return false
	}
	attempts := 0
	var ahead passedOver // what happened to the options before the one launched
	// In launch order (by run cost unless launch_order says otherwise),
	// options whose VMs were recently reclaimed last.
	k := g.Rank(t)
	for rank, c := range k.Prices {
		switch {
		case skipRegion[c.Region] != "":
			ahead.add("in a region skipped")
			continue
		case skipZone[c.Zone]:
			ahead.add("in a zone skipped")
			continue
		case quotaSkipped(c):
			ahead.add("over the region's quota")
			continue
		case maxPrice > 0 && c.USD() > maxPrice:
			ahead.add("above max_price")
			continue
		}
		if cd, ok := cool[poolKey(c)]; ok && cd.unreach {
			ahead.add("in a region this client can't reach")
			otherFailure = true // ends when a VM there answers
			if !contains(unreachSkipped, c.Region) {
				unreachSkipped = append(unreachSkipped, c.Region)
			}
			continue
		}
		if cd, ok := cool[poolKey(c)]; ok {
			ahead.add("cooling down")
			switch {
			case cd.Transient:
				otherFailure = true // may have cleared since
			case cd.Setup: // no network there as set up: neutral
			case strings.Contains(cd.Why, "InsufficientFreeAddressesInSubnet"):
				otherFailure = true // the zone's subnet is full, for every size
			case shortOfCapacity(cd.Why):
				capacity = append(capacity, c)
			}
			// Unsupported here, or refused at max_price: neutral.
			continue
		}
		if attempts == maxAttempts {
			otherFailure = true
			break
		}
		trial := false
		if lapsed[c.Region] {
			// Its hold lapsed: this launch tries it again, unless another
			// one already is.
			if !g.claimTrial(c.Region) {
				ahead.add("in a region this client can't reach")
				otherFailure = true
				continue
			}
			trial, lapsed[c.Region] = true, false
		}
		a := g.Regional(c.Region)
		attempts++
		inst, err := a.launchOrUnbaked(ctx, log, launchSpec{itype: c.Type, zone: c.Zone, beforeSend: g.mark(c.Region)})
		if err != nil && trial && !errors.Is(err, ErrLaunchAmbiguous) {
			g.dropTrial(c.Region) // nothing there to try it
			lapsed[c.Region] = true
		}
		if err == nil {
			g.clearCool(c)
			in := a.instance(inst)
			if in.Zone == "" {
				in.Zone = c.Zone
			}
			g.record(in)
			why := placementWhy(t, k, rank, ahead)
			in.Placement = &Placement{Order: k.Order, Rank: rank + 1, Of: len(k.Prices), Price: c.Price, Why: why}
			if rc, ok := k.CostOf(c); ok {
				in.Placement.Cost = &rc
			}
			log("launched spot %s instance %s in %s at $%s/h (%s)", c.Type, in.ID, c.Zone, c.Price, why)
			return in, nil
		}
		var ce *CredentialError
		var re *regionError
		var ze *zoneError
		var qe *QuotaError // the pre-launch check: no room in this region's Spot quota
		code := awsCode(err)
		switch {
		case errors.Is(err, ErrLaunchAmbiguous), errors.As(err, &ce):
			// The instance may exist in this region: never launch a
			// replacement elsewhere on a guess. (Checked before
			// cancellation, which can make a sent request ambiguous.)
			return Instance{}, fmt.Errorf("%s: %w", c.Region, err)
		case ctx.Err() != nil:
			return Instance{}, ctx.Err()
		case errors.As(err, &ze), zoneWide(code):
			why := firstLineOf([]byte(err.Error()))
			skipZone[c.Zone] = true
			setup := ze != nil // no default subnet there: the network as set up
			g.coolZone(c.Zone, why, setup)
			log("skipping %s: %s", c.Zone, why)
			if !setup {
				otherFailure = true // no free addresses: may clear, for every size
			}
			if code == "" {
				attempts-- // nothing was sent
				code = "no default subnet"
			}
		case errors.As(err, &qe), classifyAWSError(err) == errQuota:
			// No room in this region's quota for this size: smaller types,
			// and types under another quota, may still fit here.
			why := firstLineOf([]byte(firstLineOfErr(err))) // a quota error goes on to say how to raise it
			q := quotaSkip{vcpus: vcpusOf(c.Type), group: quotaGroup(c.Type)}
			switch {
			case qe != nil:
				q.vcpus = qe.VCPUs
				attempts-- // the pre-launch check: nothing was sent
				code = "Spot vCPU quota"
			case code == "InstanceLimitExceeded":
				// A count limit on instances: no type fits, and on-demand
				// is refused there too.
				q.vcpus = 0
				broken[c.Region] = true
			}
			skipQuota[c.Region] = append(skipQuota[c.Region], q)
			if quota == nil {
				quota = qe
				if quota == nil {
					quota = &QuotaError{Region: c.Region, Spot: true, VCPUs: vcpusOf(c.Type), Detail: code}
					if q.vcpus > 0 { // a vCPU quota (not an instance count limit): say which
						quota.Code = quotaCodeFor(c.Type, true)
					}
				}
			}
			if q.vcpus > 0 {
				log("skipping %d-vCPU and larger types in %s: %s", q.vcpus, c.Region, why)
			} else {
				log("skipping %s: %s", c.Region, why)
			}
		case g.oneRegion() && (errors.As(err, &re) || regionWide(code)):
			// No other region to try, and the next launch should ask again
			// rather than wait out a cooldown.
			if re != nil {
				return Instance{}, err
			}
			return Instance{}, fmt.Errorf("launch %s: %w", c.Type, err)
		case errors.As(err, &re), regionWide(code):
			why := firstLineOf([]byte(firstLineOfErr(err)))
			skipRegion[c.Region] = why
			broken[c.Region] = true // no on-demand there either, this launch
			if setupFailure(err) {
				g.coolRegion(c.Region, why, true)
			} else {
				otherFailure = true // a transient failure: not a quota verdict
				g.coolRegionAs(c.Region, cooldown{Why: why, Transient: true})
			}
			log("skipping %s: %s", c.Region, why)
			if re != nil {
				attempts-- // failed before any launch request
			}
		case classifyAWSError(err) == errNextType:
			g.coolPool(c, code)
			log("skipping %s in %s: %s", c.Type, c.Zone, code)
			if code == "SpotMaxPriceTooLow" {
				atCap[c.Region+"/"+c.Type] = true
			}
			if shortOfCapacity(code) {
				capacity = append(capacity, c) // (not Unsupported or a max price: those won't change)
			}
		default:
			// The same request would fail everywhere (bad bootstrap, image,
			// parameters): don't burn through every region.
			if g.oneRegion() {
				return Instance{}, fmt.Errorf("launch %s: %w", c.Type, err)
			}
			return Instance{}, fmt.Errorf("%s: %w", c.Region, err)
		}
		tried = append(tried, c.Type+"@"+c.Zone+": "+code)
		ahead.add("refused (" + code + ")")
	}
	// A region that couldn't be priced just now (a timeout, throttling)
	// might have had room.
	for _, why := range t.Unpriced {
		if !lastingUnpriced(why) {
			otherFailure = true
		}
	}
	// A capacity refusal counts against "quota only" unless its region's
	// quota refused that size too (it couldn't have launched anyway).
	for _, c := range capacity {
		if !quotaSkipped(c) {
			otherFailure = true
		}
	}
	if g.r.OnDemandFallback {
		// Only regions with a type that may launch under max_price: with
		// every Spot price above it, on-demand is above it too, and the
		// launch ends in the PriceCapError below instead.
		// (Capped, onDemandTypes reads only the price table.)
		var regions []string
		for _, c := range t.Prices {
			if !broken[c.Region] && !unreachHeldNow[c.Region] && !contains(regions, c.Region) && (maxPrice == 0 || len(g.onDemandTypes(ctx, t, c.Region, atCap)) > 0) {
				regions = append(regions, c.Region)
			}
		}
		if len(regions) > 2 {
			otherFailure = true // regions left untried: not a quota verdict
		}
		for _, rg := range regions[:min(len(regions), 2)] {
			if lapsed[rg] && !g.claimTrial(rg) {
				continue // another launch is trying it again
			}
			log("no spot capacity; falling back to on-demand in %s", rg)
			a := g.Regional(rg)
			inst, err := a.launchOrUnbaked(ctx, log, launchSpec{onDemand: true, types: g.onDemandTypes(ctx, t, rg, atCap), beforeSend: g.mark(rg)})
			if err == nil {
				in := a.instance(inst)
				g.record(in)
				in.Placement = &Placement{Order: "on_demand", Why: "on_demand_fallback: no Spot option could launch"}
				return in, nil
			}
			if lapsed[rg] && !errors.Is(err, ErrLaunchAmbiguous) {
				g.dropTrial(rg) // nothing launched there to try it
			}
			var ce *CredentialError
			if errors.Is(err, ErrLaunchAmbiguous) || errors.As(err, &ce) || ctx.Err() != nil {
				return Instance{}, fmt.Errorf("%s: %w", rg, err)
			}
			var oq *QuotaError
			if !errors.As(err, &oq) {
				otherFailure = true
			} else if quota == nil {
				quota = oq
			}
			tried = append(tried, rg+" on-demand: "+firstLineOf([]byte(err.Error())))
		}
	}
	if quota != nil && !otherFailure {
		if quota.Code != "" && quota.Usage == nil {
			if quota.Desired == 0 {
				// Suggest a quota above the current one, as a single region does.
				if q, err := g.Regional(quota.Region).quotaValue(ctx, quota.Code); err == nil {
					quota.Desired = q + quota.VCPUs
				}
			}
			// The whole fleet may need to fit in this one region.
			quota.Desired = max(quota.Desired, quota.VCPUs, g.r.MaxInstances*VMVCPUs(g.r))
		}
		if len(tried) > 1 {
			return Instance{}, fmt.Errorf("no option in %s could launch (%s); quota stopped the cheapest: %w", g.Where(), strings.Join(tried, ", "), quota)
		}
		return Instance{}, quota
	}
	// (Prices[0] is first in launch order, not always the cheapest.)
	if cheapest := cheapestOf(t.Prices); len(tried) == 0 && maxPrice > 0 && len(t.Prices) > 0 && cheapest.USD() > maxPrice {
		pe := &PriceCapError{Where: g.Where(), MaxPrice: g.r.MaxPrice.String(), Cheapest: cheapest}
		if otherFailure { // e.g. a region unpriced just now: may be under the cap
			return Instance{}, errors.New(pe.Error())
		}
		return Instance{}, pe
	}
	if len(tried) == 0 && len(unreachSkipped) > 0 {
		var why []string
		for _, rg := range unreachSkipped {
			why = append(why, rg+" is "+g.unreachDetail(rg))
		}
		rest := ""
		if len(unreachSkipped) < len(t.Regions)-len(t.Unpriced) {
			rest = "; the other regions' options are cooling down or above max_price"
		}
		return Instance{}, fmt.Errorf("no option in %s can launch: %s%s (`spotexec prices %s` shows them)", g.Where(), strings.Join(why, "; "), rest, g.r.Name)
	}
	if len(tried) == 0 {
		return Instance{}, fmt.Errorf("every Spot option in %s is cooling down after recent capacity or quota failures; retry in a few minutes (`spotexec prices %s` shows them)", g.Where(), g.r.Name)
	}
	hint := ", or set on_demand_fallback = true"
	if g.r.OnDemandFallback {
		hint = ""
	}
	return Instance{}, fmt.Errorf("no Spot capacity in the %d cheapest options (%s); %s%s", len(tried), strings.Join(tried, ", "), widenHint(g.r), hint)
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// regionOf finds the region holding instance id: from local state, else by
// asking each region that could hold it.
func (g *AWSGlobal) regionOf(ctx context.Context, id string) (string, error) {
	var rg string
	serr := g.withState(func(st *placementState) { rg = st.Where[id].Region })
	if rg != "" {
		return rg, nil
	}
	regions, merr := g.managedRegions(ctx)
	if merr != nil && len(regions) == 0 {
		return "", merr
	}
	var mu sync.Mutex
	found := ""
	var at time.Time
	errs := eachRegion(ctx, regions, func(ctx context.Context, r string) error {
		b, err := g.Regional(r).api.ec2(ctx, "DescribeInstances", url.Values{"InstanceId.1": {id}})
		if err != nil {
			if awsCode(err) == "InvalidInstanceID.NotFound" || awsCode(err) == "InvalidInstanceID.Malformed" {
				return nil
			}
			return err
		}
		ins, _, err := parseDescribeInstances(b)
		if err == nil && len(ins) > 0 {
			mu.Lock()
			found, at = r, ins[0].launched()
			mu.Unlock()
		}
		return err
	})
	if found != "" {
		_ = g.remember([]Instance{{ID: id, Region: found, LaunchedAt: at}})
		return found, nil
	}
	if err := credentialFailure(errs); err != nil {
		return "", err
	}
	if len(errs) > 0 {
		return "", fmt.Errorf("can't find instance %s: %w", id, joinRegionErrors(errs))
	}
	if err := errors.Join(serr, merr); err != nil {
		// The search may have missed regions only the state knows about.
		return "", fmt.Errorf("can't find instance %s: %w", id, err)
	}
	return "", fmt.Errorf("instance %s is not in any of %s's regions", id, g.r.Name)
}

// For returns the regional provider holding instance id.
func (g *AWSGlobal) For(ctx context.Context, id string) (Provider, error) {
	rg, err := g.regionOf(ctx, id)
	if err != nil {
		return nil, err
	}
	return g.Regional(rg), nil
}

// WaitRunning waits for an instance in whichever region holds it.
func (g *AWSGlobal) WaitRunning(ctx context.Context, id string, log Logger) (Instance, error) {
	rg, err := g.regionOf(ctx, id)
	if err != nil {
		return Instance{}, err
	}
	return g.Regional(rg).WaitRunning(ctx, id, log)
}

// Terminate ends one instance (only if it carries our tags).
func (g *AWSGlobal) Terminate(ctx context.Context, id string) error {
	rg, err := g.regionOf(ctx, id)
	if err != nil {
		return err
	}
	return g.Regional(rg).Terminate(ctx, id)
}

// ---- Provider ----

// Status reports the best-placed instance across the active regions.
func (g *AWSGlobal) Status(ctx context.Context) (Info, error) {
	regions, err := g.activeRegions()
	if err != nil {
		return Info{State: StateUnknown, Region: g.Where(), Detail: err.Error()}, err
	}
	if len(regions) == 0 {
		return Info{State: StateAbsent, Region: g.Where()}, nil
	}
	var mu sync.Mutex
	var infos []Info
	errs := eachRegion(ctx, regions, func(ctx context.Context, rg string) error {
		info, err := g.Regional(rg).Status(ctx)
		if err != nil {
			return err
		}
		mu.Lock()
		infos = append(infos, info)
		mu.Unlock()
		return nil
	})
	rank := func(s State) int {
		switch s {
		case StateRunning:
			return 0
		case StatePending:
			return 1
		case StateStopping:
			return 2
		}
		return 3
	}
	sort.SliceStable(infos, func(i, j int) bool { return rank(infos[i].State) < rank(infos[j].State) })
	if len(errs) > 0 {
		err := joinRegionErrors(errs)
		if len(infos) == 0 || infos[0].State == StateAbsent {
			return Info{State: StateUnknown, Region: g.Where(), Detail: err.Error()}, err
		}
	}
	if len(infos) == 0 || infos[0].State == StateAbsent {
		return Info{State: StateAbsent, Region: g.Where()}, nil
	}
	return infos[0], nil
}

// Up returns a running instance, launching one where Spot is cheapest if
// there is none (spotexec up).
func (g *AWSGlobal) Up(ctx context.Context, log Logger) (Info, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	unlock, err := g.home().lockUp(ctx, log)
	if err != nil {
		return Info{}, err
	}
	defer unlock()
	ins, err := g.List(ctx)
	if err != nil {
		return Info{}, err
	}
	if len(ins) > 0 {
		return g.Regional(ins[0].Region).waitRunning(ctx, ec2Instance{InstanceID: ins[0].ID, State: string(ins[0].State)}, log)
	}
	in, err := g.launch(ctx, log)
	if err != nil {
		return Info{}, err
	}
	info, err := g.Regional(in.Region).waitRunning(ctx, ec2Instance{InstanceID: in.ID, State: "pending", InstanceType: in.Type}, log)
	info.Launched = true
	if info.InstanceID == "" {
		info.InstanceID = in.ID
	}
	return info, err
}

// Down terminates this runner's instances in every region it uses or may
// still hold instances in.
func (g *AWSGlobal) Down(ctx context.Context, log Logger) error {
	if log == nil {
		log = func(string, ...any) {}
	}
	regions, _ := g.managedRegions(ctx)
	g.sweepBuilders(ctx, regions, log)
	unlock, err := g.home().lockUp(ctx, log)
	if err != nil {
		return err
	}
	defer unlock()
	// Again under the lock: a launch that held it may have just added a
	// region.
	regions, rerr := g.managedRegions(ctx)
	var mu sync.Mutex
	total := 0
	errs := eachRegion(ctx, regions, func(ctx context.Context, rg string) error {
		n, err := g.Regional(rg).downLocked(ctx, func(f string, a ...any) {
			if !strings.HasPrefix(f, "no running instances") {
				log(f+" in %s", append(a, rg)...)
			}
		})
		mu.Lock()
		total += n
		mu.Unlock()
		return err
	})
	if total == 0 && len(errs) == 0 {
		log("no running instances for %s in %d region(s)", g.r.Name, len(regions))
	}
	if rerr != nil {
		errs[g.r.Region] = rerr
	}
	return joinRegionErrors(errs)
}

// SweepBuilders ends leftover image-bake builders in the regions that may
// hold them (the pool manager calls this periodically).
func (g *AWSGlobal) SweepBuilders(ctx context.Context, log Logger) {
	regions, _ := g.activeRegions()
	g.sweepBuilders(ctx, regions, log)
}

func (g *AWSGlobal) sweepBuilders(ctx context.Context, regions []string, log Logger) {
	eachRegion(ctx, regions, func(ctx context.Context, rg string) error {
		g.Regional(rg).SweepBuilders(ctx, log) // skips a region with a bake running
		return nil
	})
}

// RegionalProviders returns a provider for every region this runner uses
// or may hold instances in (for account-wide listings).
func (g *AWSGlobal) RegionalProviders(ctx context.Context) ([]*AWSSpot, error) {
	regions, err := g.managedRegions(ctx)
	out := make([]*AWSSpot, 0, len(regions))
	for _, rg := range regions {
		out = append(out, g.Regional(rg))
	}
	return out, err
}

// Forget is a no-op: instance state lives in the cloud.
func (g *AWSGlobal) Forget() {}

// Doctor checks credentials, prices across the regions, and the quota and
// network of the cheapest few regions.
func (g *AWSGlobal) Doctor(ctx context.Context) []Check {
	var out []Check
	h := g.home()
	creds, err := g.creds.get(ctx)
	if err != nil {
		return append(out, Check{Name: "aws credentials", Msg: err.Error()})
	}
	out = append(out, Check{Name: "aws credentials", OK: true, Msg: creds.Source})
	regions, rerr := g.PlacementRegions(ctx)
	if len(regions) > 0 {
		h = g.Regional(regions[0]) // the home region may not be enabled
	}
	if b, err := h.api.query(ctx, "sts", stsAPIVersion, "GetCallerIdentity", nil); err != nil {
		out = append(out, Check{Name: "aws identity", Msg: err.Error()})
	} else {
		out = append(out, Check{Name: "aws identity", OK: true,
			Msg: "account " + xmlFirst(b, "GetCallerIdentityResult>Account") + " · " + xmlFirst(b, "GetCallerIdentityResult>Arn")})
	}
	if rerr != nil {
		return append(out, Check{Name: "regions", Msg: rerr.Error()})
	}
	msg := fmt.Sprintf("%d: %s", len(regions), strings.Join(regions, ", "))
	if off := g.notEnabled(regions); len(off) > 0 {
		msg += fmt.Sprintf(" (left out, not enabled in this account: %s)", strings.Join(off, ", "))
	}
	if ex := g.excluded(); len(ex) > 0 {
		msg += fmt.Sprintf(" (left out by exclude_regions: %s)", strings.Join(ex, ", "))
	}
	out = append(out, Check{Name: "regions", OK: len(regions) > 0, Msg: msg})
	if len(regions) == 0 {
		return out
	}
	t, err := g.Prices(ctx, true)
	if err != nil {
		return append(out, Check{Name: "spot prices", Msg: err.Error()})
	}
	k := g.Rank(t)
	best := k.Prices[0]
	out = append(out, Check{Name: "spot prices", OK: true, Msg: g.nextLaunchIn(t, k)})
	if c, ok := rankingCheck(k); ok {
		out = append(out, c)
	}
	if len(t.Unpriced) > 0 {
		out = append(out, Check{Name: "unpriced regions", OK: true, Msg: "not used: " + summarize(t.Unpriced, 4)})
	}
	// Quota and network matter where launches will actually go: the
	// first few regions in launch order. A region the account has never
	// used often has a tiny Spot quota.
	need := vcpusOf(best.Type)
	for _, rg := range firstRegions(k) {
		a := g.Regional(rg)
		if vpc, err := a.vpcID(ctx); err != nil {
			msg := err.Error()
			if strings.HasPrefix(msg, "no default VPC") {
				msg = "launches skip this region: " + msg
			}
			out = append(out, Check{Name: "network " + rg, Msg: msg})
		} else {
			out = append(out, Check{Name: "network " + rg, OK: true, Msg: "default VPC " + vpc})
		}
		if q, err := a.spotQuota(ctx); err == nil {
			fleet := "no max_instances, so the quota is what limits the fleet"
			if g.r.Capped() {
				fleet = fmt.Sprintf("the fleet may use up to %d across all regions", g.r.MaxInstances*need)
			}
			out = append(out, Check{Name: "spot vCPU quota " + rg, OK: q >= need,
				Msg: fmt.Sprintf("%d vCPUs (one %s needs %d; %s)", q, best.Type, need, fleet)})
		}
	}
	out = append(out, Check{Name: "console", OK: true, Msg: fmt.Sprintf("instances can appear in any of these regions; filter by tag %s=%s (`spotexec status` shows each one's region)", tagRunner, g.r.Name)})
	return out
}

// launchRegionsChecked is how many regions, first in launch order, doctor
// and setup check the quota (and doctor the network) of: where launches
// will actually go.
const launchRegionsChecked = 3

// firstRegions lists the first few distinct regions in k's launch order.
func firstRegions(k Ranking) []string {
	var top []string
	for _, p := range k.Prices {
		if !contains(top, p.Region) {
			top = append(top, p.Region)
		}
		if len(top) == launchRegionsChecked {
			break
		}
	}
	return top
}

// LaunchRegions lists the first few regions in launch order, from fresh
// prices: those `spotexec doctor` checks the Spot quota of.
func (g *AWSGlobal) LaunchRegions(ctx context.Context) ([]string, error) {
	if g.oneRegion() {
		return []string{g.r.Region}, nil
	}
	t, err := g.Prices(ctx, true)
	if err != nil {
		return nil, err
	}
	return firstRegions(g.Rank(t)), nil
}

// LaunchRegionsWith is LaunchRegions for a multi-region runner, read with
// setup's admin session before the runner has a key of its own: profile
// ("" is the default chain), or with envOnly only the AWS_* variables
// passed from the machine running setup.
func LaunchRegionsWith(ctx context.Context, r *config.Runner, profile string, envOnly bool) ([]string, error) {
	p, err := NewAWSGlobal(r)
	if err != nil {
		return nil, err
	}
	return p.(*AWSGlobal).launchRegionsAs(ctx, &awsCredResolver{profile: profile, envOnly: envOnly})
}

// launchRegionsAs ranks g's regions with other credentials. It ranks as
// the runner would (its own profile names its warm regions), but against a
// copy of its placement state: what another session reads (its account's
// enabled regions, prices and instance types) must never be cached as the
// runner's.
func (g *AWSGlobal) launchRegionsAs(ctx context.Context, creds *awsCredResolver) ([]string, error) {
	tmp, err := os.MkdirTemp("", "spotexec-rank-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	switch b, err := os.ReadFile(filepath.Join(g.dir, "placement.json")); {
	case err == nil:
		if err := os.WriteFile(filepath.Join(tmp, "placement.json"), b, 0o600); err != nil {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	c := &AWSGlobal{r: g.r, creds: creds, dir: tmp, regional: map[string]*AWSSpot{}, legacy: g.legacy, single: g.single, endpoints: g.endpoints, now: g.now}
	return c.LaunchRegions(ctx)
}

// priceCheck is doctor's finding on where a single-region runner's next
// VM would go.
func (a *AWSSpot) priceCheck(ctx context.Context) Check {
	g := a.ranker()
	t, err := g.Prices(ctx, true)
	switch {
	case err == nil:
		k := g.Rank(t)
		msg := g.nextLaunchIn(t, k)
		if c, ok := rankingCheck(k); ok {
			msg += "; " + c.Msg
		}
		return Check{Name: "spot prices", OK: true, Msg: msg}
	case a.r.ByRequirements():
		return Check{Name: "spot prices", Msg: err.Error()}
	}
	return Check{Name: "spot prices", OK: true, Msg: "note: can't compare Spot prices (" + firstLineOfErr(err) + "), so launches try instance_types in the listed order"}
}

// NextLaunch is the option the next launch tries first: the first in
// launch order that isn't cooling down or above max_price, those held back
// after a Spot interruption last.
func (g *AWSGlobal) NextLaunch(t PriceTable) (SpotPrice, bool) {
	return g.nextIn(g.Rank(t))
}

func (g *AWSGlobal) nextIn(k Ranking) (SpotPrice, bool) {
	cool := g.cooldowns(k.Prices)
	maxPrice := g.r.MaxPriceUSD()
	for _, p := range k.Prices {
		if _, ok := cool[poolKey(p)]; ok || (maxPrice > 0 && p.USD() > maxPrice) {
			continue
		}
		return p, true
	}
	return SpotPrice{}, false
}

// nextLaunchIn describes where the next VM goes, for doctor.
func (g *AWSGlobal) nextLaunchIn(t PriceTable, k Ranking) string {
	what := plural(len(t.Prices), "option")
	if !g.oneRegion() {
		what += " in " + plural(len(t.Regions)-len(t.Unpriced), "region")
	}
	if g.r.ByRequirements() {
		what += fmt.Sprintf(", %s matching %s", plural(len(t.MemoryMiB), "instance type"), describeRequirements(g.r))
	}
	list := fmt.Sprintf("`spotexec prices %s` lists them", g.r.Name)
	p, ok := g.nextIn(k)
	if !ok {
		return fmt.Sprintf("every option is cooling down or above max_price (%s; %s)", what, list)
	}
	mem := ""
	if m := t.MemoryMiB[p.Type]; m > 0 {
		mem = fmt.Sprintf(" (%g GiB)", float64(m)/1024)
	}
	order := "cheapest first"
	switch k.Order {
	case config.LaunchListed:
		order = "instance_types in order, cheapest zone first"
	case config.LaunchRunCost:
		order = fmt.Sprintf("lowest expected run cost first, waiting valued at $%g/h", k.Model.WaitCost)
	}
	return fmt.Sprintf("next launch: %s%s in %s at $%s/h (%s; %s; %s)", p.Type, mem, p.Zone, p.Price, what, order, list)
}

// rankingCheck is doctor's view of the run-cost ranking: the first few
// options and what each factor added to their Spot price.
func rankingCheck(k Ranking) (Check, bool) {
	if k.Order != config.LaunchRunCost || len(k.Prices) == 0 {
		return Check{}, false
	}
	var parts []string
	for i, p := range k.Prices {
		if i == 3 {
			break
		}
		rc, _ := k.CostOf(p)
		s := fmt.Sprintf("%d. %s in %s at $%s/h: %s", i+1, p.Type, p.Zone, p.Price, rc.Summary())
		if k.HeldBack(p) {
			s += "; held back after a Spot interruption"
		}
		parts = append(parts, s)
	}
	return Check{Name: "launch ranking", OK: true, Msg: strings.Join(parts, " · ")}, true
}

func permHint(err error) string {
	if awsCode(err) == "UnauthorizedOperation" || strings.Contains(err.Error(), "UnauthorizedOperation") {
		return " (comparing Spot prices needs ec2:DescribeRegions, ec2:DescribeSpotPriceHistory and, for vcpus, ec2:DescribeInstanceTypes: re-run `spotexec setup` to update the policy)"
	}
	return ""
}
