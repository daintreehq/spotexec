// Package config loads the global runner config (~/.config/spotexec/config.toml)
// and the per-repo project config (.spotexec.toml), applies SPOTEXEC_* env
// overrides, and fills defaults.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Duration is a time.Duration that decodes from TOML strings like "5m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Runner types.
const (
	TypeSSH     = "ssh"
	TypeAWSSpot = "aws-spot"
	TypeSim     = "sim" // local Docker-based simulation of a Spot fleet
)

// Runner is one entry under [runners.<name>].
type Runner struct {
	Name    string `toml:"-"`
	Type    string `toml:"type"`
	Extends string `toml:"extends"` // project runners: inherit a global runner, then override

	// Common
	MaxSlots        int      `toml:"max_slots"`
	Priority        int      `toml:"priority"`
	WorkRoot        string   `toml:"work_root"`
	RemotePathSetup string   `toml:"remote_path_setup"`
	IdentityFile    string   `toml:"identity_file"`
	SSHOptions      []string `toml:"ssh_options"`
	HourlyCost      float64  `toml:"hourly_cost"`

	// ssh
	Hosts []string `toml:"hosts"`
	Host  string   `toml:"host"`

	// ephemeral
	IdleTimeout     Duration `toml:"idle_timeout"`
	MaxLifetime     Duration `toml:"max_lifetime"`
	BootstrapScript string   `toml:"bootstrap_script"`
	UserBootstrap   string   `toml:"user_bootstrap_script"`
	BootTimeout     Duration `toml:"boot_timeout"`

	// aws-spot
	Region           string   `toml:"region"`
	Regions          []string `toml:"regions"` // several regions, or ["all"]: launch wherever Spot is cheapest
	Profile          string   `toml:"profile"`
	InstanceTypes    []string `toml:"instance_types"`
	MaxPrice         string   `toml:"max_price"`
	AMI              string   `toml:"ami"`
	Arch             string   `toml:"arch"` // amd64|arm64 (derived from instance types if empty)
	SubnetID         string   `toml:"subnet_id"`
	SecurityGroupIDs []string `toml:"security_group_ids"`
	SSHCIDR          string   `toml:"ssh_cidr"`
	VolumeGB         int      `toml:"volume_gb"`
	User             string   `toml:"user"`
	OnDemandFallback bool     `toml:"on_demand_fallback"`
	CacheBucket      string   `toml:"cache_bucket"` // shared dependency cache: "auto" (default), a bucket name, or "off"

	// fleets (aws-spot, sim)
	MaxInstances int `toml:"max_instances"`
	MinInstances int `toml:"min_instances"` // kept warm by `spotexec daemon` (default 0)

	// sim
	Image     string   `toml:"image"`
	BootDelay Duration `toml:"boot_delay"`
}

// AllRegions is the regions value meaning every enabled region.
const AllRegions = "all"

// MultiRegion reports whether an aws-spot runner places instances across
// several regions (regions = [...] with more than one, or ["all"]).
func (r *Runner) MultiRegion() bool { return r.Type == TypeAWSSpot && len(r.Regions) > 0 }

// AnyRegion reports whether the runner may launch in every enabled region.
func (r *Runner) AnyRegion() bool {
	return r.MultiRegion() && len(r.Regions) == 1 && r.Regions[0] == AllRegions
}

// Ephemeral reports whether the runner is provisioned on demand.
func (r *Runner) Ephemeral() bool { return r.Type == TypeAWSSpot || r.Type == TypeSim }

// Global is ~/.config/spotexec/config.toml.
type Global struct {
	Version       int                `toml:"version"`
	DefaultPool   []string           `toml:"default_pool"`
	LocalFallback bool               `toml:"local_fallback"`
	Runners       map[string]*Runner `toml:"runners"`
	Path          string             `toml:"-"`
}

// Sync is [sync] in .spotexec.toml.
type Sync struct {
	Mode          string   `toml:"mode"`      // auto|in|none
	Writeback     string   `toml:"writeback"` // always|on-success|never
	Exclude       []string `toml:"exclude"`
	PullArtifacts []string `toml:"pull_artifacts"`
	GitSeed       bool     `toml:"git_seed"`
	Source        string   `toml:"source"` // git|directory (auto when empty)
}

// Deps is [deps].
type Deps struct {
	Lockfiles []string `toml:"lockfiles"`
	Install   string   `toml:"install"`
	CacheDirs []string `toml:"cache_dirs"`
	Hydrate   string   `toml:"hydrate"` // auto|reflink|copy|hardlink
	Timeout   Duration `toml:"timeout"`
}

// Task is [tasks.<name>].
type Task struct {
	Run       string   `toml:"run"`
	Parallel  []string `toml:"parallel"`
	Shards    int      `toml:"shards"`
	FailFast  bool     `toml:"fail_fast"`
	Timeout   Duration `toml:"timeout"`
	Exclusive bool     `toml:"exclusive"`
	Sync      string   `toml:"sync"`
	Pool      []string `toml:"pool"`
	Priority  int      `toml:"priority"`
}

// MaxShards bounds a task's shards: past it, a typo would launch a fleet.
const MaxShards = 64

// Describe says what a task runs, for task lists and completions.
func (t *Task) Describe() string {
	switch {
	case len(t.Parallel) > 0:
		return "parallel: " + strings.Join(t.Parallel, ", ")
	case t.Shards == 1:
		return "1 shard: " + t.Run
	case t.Shards > 1:
		return fmt.Sprintf("%d shards: %s", t.Shards, t.Run)
	}
	return t.Run
}

// Container is [container]: run every command (and the dependency install)
// inside this image on the runner.
type Container struct {
	Image      string   `toml:"image"`      // pull this image (e.g. "node:22-bookworm")
	Dockerfile string   `toml:"dockerfile"` // or build this Dockerfile (repo-relative)
	Context    string   `toml:"context"`    // build context (default: the Dockerfile's directory)
	Options    []string `toml:"options"`    // extra `docker run` arguments
	User       string   `toml:"user"`       // "runner" (default: files stay owned by the runner user) or "root"
	Timeout    Duration `toml:"timeout"`    // pull/build limit (default 20m)
}

// Enabled reports whether a container is configured.
func (c Container) Enabled() bool { return c.Image != "" || c.Dockerfile != "" }

// Hook is [hook]: tunes the agent command-interception hook.
type Hook struct {
	Patterns      []string `toml:"patterns"`        // extra regexes that should be offloaded
	Exclude       []string `toml:"exclude"`         // regexes that must stay local
	WhenLoadAbove float64  `toml:"when_load_above"` // only offload when local load/CPU exceeds this
	Mode          string   `toml:"mode"`            // rewrite (default) | block
	Disabled      bool     `toml:"disabled"`
}

// Project is .spotexec.toml.
type Project struct {
	Version      int               `toml:"version"`
	Timeout      Duration          `toml:"timeout"`
	Grace        Duration          `toml:"grace_period"`
	QueueTimeout Duration          `toml:"queue_timeout"`
	EnvAllow     []string          `toml:"env_allow"`
	Env          map[string]string `toml:"env"`
	Pool         []string          `toml:"pool"`
	Sync         Sync              `toml:"sync"`
	Deps         Deps              `toml:"deps"`
	Container    Container         `toml:"container"`
	Tasks        map[string]*Task  `toml:"tasks"`
	Hook         Hook              `toml:"hook"`
	// Runners defined (or extended) by the project; merged over the global
	// runners by Config.MergeProjectRunners.
	Runners map[string]toml.Primitive `toml:"runners"`
	Path    string                    `toml:"-"`
	md      toml.MetaData
}

// Config is the merged view used by commands.
type Config struct {
	Global  *Global
	Project *Project
}

// Dir returns the global config directory (~/.config/spotexec, honoring
// SPOTEXEC_CONFIG_DIR and XDG_CONFIG_HOME).
func Dir() string {
	if d := os.Getenv("SPOTEXEC_CONFIG_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "spotexec")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "spotexec")
}

// StateDir holds runtime state (instance records, known_hosts, locks).
func StateDir() string {
	if d := os.Getenv("SPOTEXEC_STATE_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "spotexec")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "spotexec")
}

// GlobalPath is the global config file path.
func GlobalPath() string {
	if p := os.Getenv("SPOTEXEC_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.toml")
}

// LoadGlobal reads the global config; a missing file yields an empty config.
func LoadGlobal() (*Global, error) {
	g := &Global{Runners: map[string]*Runner{}, Path: GlobalPath()}
	if _, err := os.Stat(g.Path); errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	md, err := toml.DecodeFile(g.Path, g)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", g.Path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys: %v", g.Path, und)
	}
	if g.Runners == nil {
		g.Runners = map[string]*Runner{}
	}
	for name, r := range g.Runners {
		r.Name = name
		if md.IsDefined("runners", name, "region") && md.IsDefined("runners", name, "regions") {
			return nil, fmt.Errorf("%s: runner %q: set region or regions, not both", g.Path, name)
		}
		if err := r.applyDefaults(); err != nil {
			return nil, fmt.Errorf("%s: runner %q: %w", g.Path, name, err)
		}
	}
	for _, n := range g.DefaultPool {
		if _, ok := g.Runners[n]; !ok {
			return nil, fmt.Errorf("%s: default_pool references unknown runner %q", g.Path, n)
		}
	}
	return g, nil
}

func (r *Runner) applyDefaults() error {
	switch r.Type {
	case TypeSSH:
		if r.Host != "" && (len(r.Hosts) == 0 || r.Hosts[0] != r.Host) {
			r.Hosts = append([]string{r.Host}, r.Hosts...)
		}
		if len(r.Hosts) == 0 {
			return errors.New("ssh runner needs host or hosts")
		}
	case TypeAWSSpot:
		if err := r.normalizeRegions(); err != nil {
			return err
		}
		if r.Region == "" {
			r.Region = firstNonEmpty(os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), "us-east-1")
		}
		if strings.HasPrefix(r.Region, "cn-") {
			return fmt.Errorf("region %q: the AWS China regions aren't supported", r.Region)
		}
		if len(r.InstanceTypes) == 0 {
			r.InstanceTypes = []string{"c7a.2xlarge", "m7a.2xlarge", "c6a.2xlarge"} // the starter template's 8-vCPU types
		}
		if r.Arch == "" {
			r.Arch = archForInstanceType(r.InstanceTypes[0])
		}
		if r.User == "" {
			r.User = "ubuntu"
		}
		if r.VolumeGB == 0 {
			r.VolumeGB = 64
		}
		if r.SSHCIDR == "" {
			r.SSHCIDR = "0.0.0.0/0"
		}
		if r.IdleTimeout.Duration == 0 {
			r.IdleTimeout.Duration = 10 * time.Minute
		}
		if r.MaxLifetime.Duration == 0 {
			r.MaxLifetime.Duration = 12 * time.Hour
		}
		if r.BootTimeout.Duration == 0 {
			r.BootTimeout.Duration = 15 * time.Minute
		}
	case TypeSim:
		if r.Image == "" {
			r.Image = "spotexec-sim"
		}
		if r.User == "" {
			r.User = "dev"
		}
		if r.IdleTimeout.Duration == 0 {
			r.IdleTimeout.Duration = 10 * time.Minute
		}
		if r.MaxLifetime.Duration == 0 {
			r.MaxLifetime.Duration = 2 * time.Hour
		}
		if r.BootTimeout.Duration == 0 {
			r.BootTimeout.Duration = 2 * time.Minute
		}
	case "":
		return errors.New("missing type (ssh, aws-spot or sim)")
	default:
		return fmt.Errorf("unsupported type %q (supported: ssh, aws-spot, sim)", r.Type)
	}
	if r.Ephemeral() {
		if r.MaxInstances == 0 {
			r.MaxInstances = 4
		}
		if r.MaxInstances < 0 {
			return errors.New("max_instances must be >= 1")
		}
		if r.MinInstances < 0 || r.MinInstances > r.MaxInstances {
			return errors.New("min_instances must be between 0 and max_instances")
		}
	}
	if r.WorkRoot == "" {
		r.WorkRoot = "~/.cache/spotexec"
	}
	return r.validate()
}

// validate rejects values that would pass silently and break later.
func (r *Runner) validate() error {
	if r.Name != "" && !runnerNameRe.MatchString(r.Name) {
		return fmt.Errorf("invalid runner name %q (letters, digits, '.', '_' and '-')", r.Name)
	}
	for _, h := range r.Hosts {
		// A leading '-' would be read by ssh as an option.
		if h == "" || strings.HasPrefix(h, "-") || strings.ContainsAny(h, " \t\r\n\x00") {
			return fmt.Errorf("invalid host %q", h)
		}
	}
	if r.MaxSlots < 0 || r.MaxSlots > protocol.MaxSlots {
		return fmt.Errorf("max_slots must be between 1 and %d (got %d)", protocol.MaxSlots, r.MaxSlots)
	}
	for name, d := range map[string]Duration{"idle_timeout": r.IdleTimeout, "max_lifetime": r.MaxLifetime, "boot_timeout": r.BootTimeout, "boot_delay": r.BootDelay} {
		if d.Duration < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if !r.Ephemeral() {
		return nil
	}
	for name, d := range map[string]Duration{"idle_timeout": r.IdleTimeout, "max_lifetime": r.MaxLifetime, "boot_timeout": r.BootTimeout} {
		if d.Duration < time.Second {
			return fmt.Errorf("%s must be at least 1s (got %s)", name, d.Duration)
		}
	}
	if r.MaxLifetime.Duration <= r.BootTimeout.Duration {
		return fmt.Errorf("max_lifetime (%s) must be longer than boot_timeout (%s): the VM would stop mid-setup", r.MaxLifetime.Duration, r.BootTimeout.Duration)
	}
	if !userRe.MatchString(r.User) {
		return fmt.Errorf("invalid user %q", r.User)
	}
	if r.Type == TypeAWSSpot {
		if !regionRe.MatchString(r.Region) {
			return fmt.Errorf("invalid region %q", r.Region)
		}
		if r.Arch != "amd64" && r.Arch != "arm64" {
			return fmt.Errorf("arch must be amd64 or arm64 (got %q)", r.Arch)
		}
		for _, t := range r.InstanceTypes {
			if archForInstanceType(t) != r.Arch {
				return fmt.Errorf("instance_types mixes architectures (%s is not %s): use one runner per architecture", t, r.Arch)
			}
		}
		if r.VolumeGB < 8 {
			return fmt.Errorf("volume_gb must be at least 8 (got %d)", r.VolumeGB)
		}
	}
	return nil
}

// normalizeRegions validates regions and folds a single concrete region
// into region (so it behaves exactly like a single-region runner). For
// several regions, region becomes the home region: the first listed, or
// for "all" the environment default, used for account-wide lookups.
func (r *Runner) normalizeRegions() error {
	if len(r.Regions) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, rg := range r.Regions {
		switch {
		case rg == AllRegions:
			if len(r.Regions) > 1 {
				return errors.New(`regions: "all" can't be combined with named regions`)
			}
		case !regionRe.MatchString(rg):
			return fmt.Errorf("regions: invalid region %q", rg)
		case seen[rg]:
			return fmt.Errorf("regions: %q is listed twice", rg)
		case strings.HasPrefix(rg, "cn-"):
			return fmt.Errorf("regions: %q: the AWS China regions aren't supported", rg)
		}
		seen[rg] = true
	}
	// One set of credentials works in one partition: GovCloud regions can't
	// share a list with the others.
	gov := 0
	for _, rg := range r.Regions {
		if strings.HasPrefix(rg, "us-gov-") {
			gov++
		}
	}
	if gov > 0 && gov < len(r.Regions) {
		return errors.New("regions: GovCloud (us-gov-*) and other regions can't be mixed: they need separate AWS accounts; define a runner for each")
	}
	if len(r.Regions) == 1 && r.Regions[0] != AllRegions {
		if r.Region != "" && r.Region != r.Regions[0] {
			return errors.New("set region or regions, not both")
		}
		r.Region, r.Regions = r.Regions[0], nil
		return nil
	}
	// Network and image IDs only exist in one region: a multi-region runner
	// uses each region's default VPC, its own security group and the
	// region's Ubuntu image (or bake).
	if r.SubnetID != "" {
		return errors.New("subnet_id is specific to one region; remove it to use several regions (each region's default VPC is used)")
	}
	if len(r.SecurityGroupIDs) > 0 {
		return errors.New("security_group_ids are specific to one region; remove them to use several regions (spotexec manages a group per region)")
	}
	if r.AMI != "" && r.AMI != "auto" && r.AMI != "base" {
		return fmt.Errorf("ami %q is specific to one region; use \"auto\" or \"base\" with several regions", r.AMI)
	}
	if b := r.CacheBucket; b != "" && b != "auto" && b != "off" {
		return fmt.Errorf("cache_bucket %q lives in one region; use \"auto\" (a bucket per region) or \"off\" with several regions", b)
	}
	if r.Region == "" && r.Regions[0] != AllRegions {
		r.Region = r.Regions[0]
	}
	return nil
}

// archForInstanceType guesses the CPU architecture of an EC2 instance type:
// Graviton families carry a "g" after the generation digit (c7g, m7gd, t4g).
func archForInstanceType(t string) string {
	fam, _, _ := strings.Cut(t, ".")
	if fam == "a1" || strings.HasPrefix(fam, "mac2") {
		return "arm64" // Graviton 1 and Apple silicon Macs
	}
	for i := 0; i < len(fam); i++ {
		if fam[i] >= '0' && fam[i] <= '9' {
			rest := fam[i+1:]
			if strings.HasPrefix(rest, "g") {
				return "arm64"
			}
			return "amd64"
		}
	}
	return "amd64"
}

// ValidRegion reports whether s looks like an AWS region name.
func ValidRegion(s string) bool { return regionRe.MatchString(s) }

// ArchForInstanceType is exported for providers.
func ArchForInstanceType(t string) string { return archForInstanceType(t) }

// CredentialDirs never sync from untracked or non-Git sources, and no
// directory inside one is ever a project.
var CredentialDirs = []string{".aws", ".ssh", ".gnupg"}

// NeverProject reports whether dir is the home directory, the filesystem
// root, or inside a credential directory (~/.ssh, even reached through a
// symlink). None of them is ever a project: syncing them would upload
// ~/.aws, ~/.ssh and the like, so a .spotexec.toml (or Git worktree) there
// is ignored and `spotexec init` refuses to write one. Directories are
// compared by identity, so another spelling of home (/Users/ME on a
// case-insensitive disk) is still home.
func NeverProject(dir string) bool {
	d := filepath.Clean(dir)
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	if d == filepath.Dir(d) {
		return true
	}
	for _, seg := range strings.Split(filepath.ToSlash(d), "/") {
		for _, c := range CredentialDirs {
			if strings.EqualFold(seg, c) {
				return true
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	hi, herr := os.Stat(home)
	var creds []os.FileInfo
	for _, c := range CredentialDirs {
		if fi, err := os.Stat(filepath.Join(home, c)); err == nil && fi.IsDir() {
			creds = append(creds, fi)
		}
	}
	for a := d; ; a = filepath.Dir(a) {
		if fi, err := os.Stat(a); err == nil {
			if a == d && herr == nil && os.SameFile(fi, hi) {
				return true
			}
			for _, c := range creds {
				if os.SameFile(fi, c) {
					return true
				}
			}
		}
		if a == filepath.Dir(a) {
			return false
		}
	}
}

// FindProject walks up from dir to find .spotexec.toml, stopping at root.
// One in the home directory or / does not count (see NeverProject).
func FindProject(dir, root string) string {
	for d := dir; ; d = filepath.Dir(d) {
		p := filepath.Join(d, ".spotexec.toml")
		if _, err := os.Stat(p); err == nil && !NeverProject(d) {
			return p
		}
		if d == root || d == filepath.Dir(d) {
			return ""
		}
	}
}

// LoadProject reads .spotexec.toml at path (empty path = defaults only).
func LoadProject(path string) (*Project, error) {
	p := &Project{Path: path}
	if path != "" {
		md, err := toml.DecodeFile(path, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var und []toml.Key
		for _, k := range md.Undecoded() {
			if len(k) > 0 && k[0] == "runners" {
				continue // decoded later, by MergeProjectRunners
			}
			und = append(und, k)
		}
		if len(und) > 0 {
			return nil, fmt.Errorf("%s: unknown keys: %v", path, und)
		}
		p.md = md
	}
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func (p *Project) applyDefaults() {
	if p.Timeout.Duration == 0 {
		p.Timeout.Duration = 5 * time.Minute
	}
	if p.Grace.Duration == 0 {
		p.Grace.Duration = 5 * time.Second
	}
	if p.QueueTimeout.Duration == 0 {
		p.QueueTimeout.Duration = 10 * time.Minute
	}
	if p.Sync.Mode == "" {
		p.Sync.Mode = "auto"
	}
	if p.Sync.Writeback == "" {
		p.Sync.Writeback = "always"
	}
	if p.Deps.Hydrate == "" {
		p.Deps.Hydrate = "auto"
	}
	if p.Deps.Timeout.Duration == 0 {
		p.Deps.Timeout.Duration = 20 * time.Minute
	}
	if p.Container.Timeout.Duration == 0 {
		p.Container.Timeout.Duration = 20 * time.Minute
	}
	if p.Tasks == nil {
		p.Tasks = map[string]*Task{}
	}
}

func (p *Project) validate() error {
	if !oneOf(p.Sync.Mode, "auto", "in", "none") {
		return fmt.Errorf("sync.mode must be auto, in or none (got %q)", p.Sync.Mode)
	}
	if !oneOf(p.Sync.Writeback, "always", "on-success", "never") {
		return fmt.Errorf("sync.writeback must be always, on-success or never (got %q)", p.Sync.Writeback)
	}
	if !oneOf(p.Deps.Hydrate, "auto", "reflink", "copy", "hardlink") {
		return fmt.Errorf("deps.hydrate must be auto, reflink, copy or hardlink (got %q)", p.Deps.Hydrate)
	}
	if p.Container.Image != "" && p.Container.Dockerfile != "" {
		return errors.New("container: set image or dockerfile, not both")
	}
	if p.Container.User != "" && p.Container.User != "runner" && p.Container.User != "root" {
		return errors.New(`container.user must be "runner" or "root"`)
	}
	if p.Deps.Install != "" && len(p.Deps.CacheDirs) == 0 {
		return errors.New("deps.install requires deps.cache_dirs")
	}
	if p.Deps.Install != "" && len(p.Deps.Lockfiles) == 0 {
		return errors.New("deps.install requires deps.lockfiles (the cache key)")
	}
	if !oneOf(p.Sync.Source, "", "git", "directory") {
		return fmt.Errorf("sync.source must be git or directory (got %q)", p.Sync.Source)
	}
	if !oneOf(p.Hook.Mode, "", "rewrite", "block") {
		return fmt.Errorf("hook.mode must be rewrite or block (got %q)", p.Hook.Mode)
	}
	for name, t := range p.Tasks {
		if t.Run != "" && embeddedArgs(t.Run) {
			return fmt.Errorf("task %q: use {args} as a separate word (or \"{args}\"), not inside a quoted string: extra arguments are passed as \"$@\"", name)
		}
		if (t.Run == "") == (len(t.Parallel) == 0) {
			return fmt.Errorf("task %q must set exactly one of run or parallel", name)
		}
		if t.Shards != 0 || p.md.IsDefined("tasks", name, "shards") {
			if len(t.Parallel) > 0 {
				return fmt.Errorf("task %q: shards applies to a run task, not a parallel one (shard the tasks it lists instead)", name)
			}
			if t.Shards < 1 || t.Shards > MaxShards {
				return fmt.Errorf("task %q: shards must be between 1 and %d (got %d)", name, MaxShards, t.Shards)
			}
		} else if strings.Contains(t.Run, "{shard}") || strings.Contains(t.Run, "{shards}") {
			return fmt.Errorf("task %q: {shard} and {shards} need shards = N on the task", name)
		}
		for _, sub := range t.Parallel {
			st, ok := p.Tasks[sub]
			if !ok {
				return fmt.Errorf("task %q: parallel references unknown task %q", name, sub)
			}
			if len(st.Parallel) > 0 {
				return fmt.Errorf("task %q: nested parallel task %q is not supported", name, sub)
			}
		}
		if t.Sync != "" && !oneOf(t.Sync, "auto", "in", "none") {
			return fmt.Errorf("task %q: sync must be auto, in or none", name)
		}
	}
	return nil
}

// globalOnlyKeys are runner settings a project (.spotexec.toml) may not set.
var globalOnlyKeys = []string{
	"type", "host", "hosts", "ssh_options", "identity_file", "work_root",
	"profile", "region", "subnet_id", "security_group_ids", "ssh_cidr",
	"user", "cache_bucket", "regions",
}

var (
	runnerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	regionRe     = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	userRe       = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// MergeProjectRunners decodes runners declared in .spotexec.toml and adds
// them to the global set (a project runner with the same name as a global
// one overrides it). `extends = "name"` starts from a copy of a global
// runner, so a project can keep your account/region settings and change only
// sizes, slots or bootstrap.
func (c *Config) MergeProjectRunners() error {
	p := c.Project
	if p == nil {
		return nil
	}
	// Extends resolve against the user's own runners only, in a fixed order.
	global := map[string]*Runner{}
	for k, v := range c.Global.Runners {
		global[k] = v
	}
	names := make([]string, 0, len(p.Runners))
	for name := range p.Runners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prim := p.Runners[name]
		if !runnerNameRe.MatchString(name) {
			return fmt.Errorf("%s: invalid runner name %q (letters, digits, '.', '_' and '-')", p.Path, name)
		}
		// A repository is not trusted to choose how this machine connects, which
		// credentials or account are used, or which network is opened: those
		// stay in the global config. Sizing, scheduling and the VM's own setup
		// may be overridden.
		for _, key := range globalOnlyKeys {
			if p.md.IsDefined("runners", name, key) {
				return fmt.Errorf("%s: runner %q: %q can only be set in the global config (%s)", p.Path, name, key, GlobalPath())
			}
		}
		var probe Runner
		if err := p.md.PrimitiveDecode(prim, &probe); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		base := Runner{}
		from := probe.Extends
		if from == "" {
			from = name
		}
		g, ok := global[from]
		if !ok {
			if probe.Extends != "" {
				return fmt.Errorf("%s: runner %q extends unknown runner %q", p.Path, name, probe.Extends)
			}
			return fmt.Errorf("%s: runner %q must extend one of your global runners (extends = \"...\")", p.Path, name)
		}
		base = *g
		base.Hosts = append([]string(nil), g.Hosts...)
		base.InstanceTypes = append([]string(nil), g.InstanceTypes...)
		base.SSHOptions = append([]string(nil), g.SSHOptions...)
		base.SecurityGroupIDs = append([]string(nil), g.SecurityGroupIDs...)
		base.Regions = append([]string(nil), g.Regions...)
		if p.md.IsDefined("runners", name, "instance_types") && !p.md.IsDefined("runners", name, "arch") {
			base.Arch = "" // re-derive from the new instance types
		}
		if err := p.md.PrimitiveDecode(prim, &base); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		base.Name, base.Extends = name, ""
		if err := base.applyDefaults(); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		c.Global.Runners[name] = &base
	}
	if und := p.md.Undecoded(); len(und) > 0 {
		var keys []string
		for _, k := range und {
			if len(k) > 0 && k[0] == "runners" {
				keys = append(keys, k.String())
			}
		}
		if len(keys) > 0 {
			return fmt.Errorf("%s: unknown keys: %s", p.Path, strings.Join(keys, ", "))
		}
	}
	for _, n := range p.Pool {
		if _, ok := c.Global.Runners[n]; !ok {
			return fmt.Errorf("%s: pool references unknown runner %q", p.Path, n)
		}
	}
	return nil
}

// TaskNames returns sorted task names.
func (p *Project) TaskNames() []string {
	var names []string
	for n := range p.Tasks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Pool resolves the ordered list of runners to use.
func (c *Config) Pool(override []string) ([]*Runner, error) {
	names := override
	if len(names) == 0 {
		names = splitList(os.Getenv("SPOTEXEC_POOL"))
	}
	if len(names) == 0 && c.Project != nil {
		names = c.Project.Pool
	}
	if len(names) == 0 {
		names = c.Global.DefaultPool
	}
	if len(names) == 0 {
		names = c.RunnerNames() // sorted: every process sees the same order
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no runners configured; add one to %s (or run `spotexec setup`)", c.Global.Path)
	}
	var out []*Runner
	for _, n := range names {
		r, ok := c.Global.Runners[n]
		if !ok {
			return nil, fmt.Errorf("unknown runner %q (configured: %s)", n, strings.Join(c.RunnerNames(), ", "))
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

// RunnerNames lists configured runner names.
func (c *Config) RunnerNames() []string {
	var n []string
	for k := range c.Global.Runners {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Runner returns a runner by name.
func (c *Config) Runner(name string) (*Runner, error) {
	r, ok := c.Global.Runners[name]
	if !ok {
		return nil, fmt.Errorf("unknown runner %q (configured: %s)", name, strings.Join(c.RunnerNames(), ", "))
	}
	return r, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func oneOf(s string, opts ...string) bool {
	for _, o := range opts {
		if s == o {
			return true
		}
	}
	return false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ExpandHome expands a leading ~/ in a local path.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// embeddedArgs reports a {args} that is not a shell word of its own (for
// example inside "pytest {args}", or escaped), where "$@" can't be
// substituted safely. {args}, "{args}" and '{args}' are fine when they stand
// alone between word boundaries outside quotes; comments are skipped.
func embeddedArgs(run string) bool {
	const ph = "{args}"
	bound := func(c byte) bool { return strings.IndexByte(" \t\n;&|()", c) >= 0 }
	inS, inD, atBound := false, false, true
	for i := 0; i < len(run); i++ {
		c := run[i]
		if !inS && !inD {
			if atBound && c == '#' { // a comment: to the end of the line
				end := strings.IndexByte(run[i:], '\n')
				if end < 0 {
					end = len(run) - i
				}
				if strings.Contains(run[i:i+end], ph) {
					return true // tasks don't strip comments: "$@" would land in it
				}
				i += end
				atBound = true
				continue
			}
			if atBound {
				matched := false
				for _, form := range []string{ph, `"` + ph + `"`, "'" + ph + "'"} {
					end := i + len(form)
					if strings.HasPrefix(run[i:], form) && (end == len(run) || bound(run[end])) {
						i, matched = end-1, true
						break
					}
				}
				if matched {
					atBound = false
					continue
				}
			}
		}
		if strings.HasPrefix(run[i:], ph) {
			return true // inside quotes, or glued to other text
		}
		switch {
		case c == '\\' && !inS:
			if strings.HasPrefix(run[i+1:], ph) {
				return true // an escaped placeholder
			}
			if i+1 < len(run) && run[i+1] == '\n' {
				i++ // a line continuation: still between words
				atBound = true
				continue
			}
			i++
			atBound = false
			continue
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		}
		atBound = !inS && !inD && bound(c)
	}
	return false
}
