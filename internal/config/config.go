// Package config loads the global runner config (~/.config/spotexec/config.toml)
// and the per-repo project config (.spotexec.toml), applies SPOTEXEC_* env
// overrides, and fills defaults.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/daintreehq/spotexec/internal/depskey"
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

// Dollars is an amount in US dollars, written as a number (1.5) or a
// numeric string ("1.50", "$1.50"). Set tells an explicit 0 from unset.
type Dollars struct {
	V   float64
	Set bool
}

func (d *Dollars) UnmarshalText(b []byte) error {
	s := strings.TrimPrefix(strings.TrimSpace(string(b)), "$")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid amount %q (want US dollars, such as 1.50)", string(b))
	}
	d.V, d.Set = v, true
	return nil
}

// UnmarshalTOML takes unquoted numbers too.
func (d *Dollars) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		return d.UnmarshalText([]byte(v))
	case int64:
		d.V, d.Set = float64(v), true
		return nil
	case float64:
		d.V, d.Set = v, true
		return nil
	}
	return fmt.Errorf("invalid amount %v (want US dollars, such as 1.50)", v)
}

func (d Dollars) MarshalText() ([]byte, error) {
	if !d.Set {
		return nil, nil
	}
	return []byte(d.String()), nil
}

// String is the amount as a plain decimal ("0.4"), the form EC2 takes.
func (d Dollars) String() string { return strconv.FormatFloat(d.V, 'f', -1, 64) }

// Bytes is a size in bytes, written as a plain integer or with a binary unit:
// "512KB", "100MB", "1.5GB" (KiB, MiB and GiB spell the same).
type Bytes int64

func (b *Bytes) UnmarshalText(t []byte) error {
	v, err := ParseBytes(string(t))
	if err != nil {
		return err
	}
	*b = Bytes(v)
	return nil
}

// UnmarshalTOML takes unquoted numbers too: through UnmarshalText the
// decoder would format a float with six decimals, so 0.0000001 would read
// as 0, no limit.
func (b *Bytes) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		return b.UnmarshalText([]byte(v))
	case int64:
		return b.UnmarshalText([]byte(strconv.FormatInt(v, 10)))
	case float64:
		return b.UnmarshalText([]byte(strconv.FormatFloat(v, 'g', -1, 64)))
	}
	return fmt.Errorf("invalid size %v (want bytes, or a number with KB, MB or GB)", v)
}

// ParseBytes reads a size: a plain integer of bytes, or a number with a
// unit (B, KB, MB, GB; binary multiples, so "1MB" is 1,048,576 bytes).
func ParseBytes(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f*float64(mult) > math.MaxInt64/2 {
		return 0, fmt.Errorf("invalid size %q (want bytes, or a number with KB, MB or GB)", s)
	}
	// It would truncate to 0, which means no limit: only an explicit 0 may.
	// A tiny number (1e-400) parses as 0 too; its significand isn't zero.
	if n := f * float64(mult); (n > 0 && n < 1) || (n == 0 && !zeroSignificand(t)) {
		return 0, fmt.Errorf("invalid size %q: below one byte (0 means no limit)", s)
	}
	return int64(f * float64(mult)), nil
}

// zeroSignificand reports whether a number ParseFloat read as 0 is written
// as zero (0, -0.0, 0e5, 0x0p1) rather than underflowing. t is upper case.
func zeroSignificand(t string) bool {
	s := strings.TrimLeft(t, "+-")
	if h, ok := strings.CutPrefix(s, "0X"); ok {
		s, _, _ = strings.Cut(h, "P")
	} else {
		s, _, _ = strings.Cut(s, "E")
	}
	return strings.Trim(s, "0._") == ""
}

// Runner types.
const (
	TypeSSH     = "ssh"
	TypeAWSSpot = "aws-spot"
	TypeSim     = "sim" // local Docker-based simulation of a Spot fleet
)

// Runner is one entry under [runners.<name>].
type Runner struct {
	Name        string `toml:"-"`
	Type        string `toml:"type"`
	Extends     string `toml:"extends"`      // project runners: inherit a global runner, then override
	ExtendsType string `toml:"extends_type"` // project runners: or inherit any of your runners of this type

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
	Region  string   `toml:"region"`
	Regions []string `toml:"regions"` // several regions, or ["all"]: launch where the run is expected to cost least
	// ExcludeRegions: with regions = ["all"], enabled regions never to launch in.
	ExcludeRegions []string `toml:"exclude_regions"`
	Profile        string   `toml:"profile"`
	InstanceTypes  []string `toml:"instance_types"`
	// Requirements instead of (or narrowing) a type list: every current
	// instance type with exactly VCPUs vCPUs and at least MinMemoryGiB is a
	// candidate, and launches go to the cheapest.
	VCPUs                int      `toml:"vcpus"`
	MinMemoryGiB         float64  `toml:"min_memory_gib"`
	MinGeneration        int      `toml:"min_generation"`
	ExcludeInstanceTypes []string `toml:"exclude_instance_types"`
	LaunchOrder          string   `toml:"launch_order"` // run_cost (default) | cheapest | listed
	MaxPrice             Dollars  `toml:"max_price"`    // Spot bid cap in USD per hour; unset: the on-demand price
	AMI                  string   `toml:"ami"`
	Arch                 string   `toml:"arch"` // amd64|arm64 (derived from instance types if empty)
	SubnetID             string   `toml:"subnet_id"`
	SecurityGroupIDs     []string `toml:"security_group_ids"`
	SSHCIDR              string   `toml:"ssh_cidr"`
	VolumeGB             int      `toml:"volume_gb"`
	User                 string   `toml:"user"`
	OnDemandFallback     bool     `toml:"on_demand_fallback"`
	CacheBucket          string   `toml:"cache_bucket"` // shared dependency cache: "auto" (default), a bucket name, or "off"
	WaitCost             Dollars  `toml:"wait_cost"`    // USD an hour of waiting is worth against the Spot bill (run_cost); default DefaultWaitCost

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

// Launch orders.
const (
	// LaunchRunCost ranks by the expected cost of the work, counting the
	// waiting a cold region, a slower type or a distant region adds.
	LaunchRunCost  = "run_cost"
	LaunchCheapest = "cheapest" // the Spot price alone
	LaunchListed   = "listed"
)

// DefaultWaitCost is what an hour of waiting is worth, in USD, unless a
// runner sets wait_cost: more than an 8-vCPU Spot VM costs for an hour.
const DefaultWaitCost = 1.0

// MaxPriceUSD is the runner's max_price, or 0 when it isn't set (the bid
// is then capped at the on-demand price).
func (r *Runner) MaxPriceUSD() float64 {
	if r.MaxPrice.Set {
		return r.MaxPrice.V
	}
	return 0
}

// WaitCostPerHour is the runner's wait_cost, or DefaultWaitCost.
func (r *Runner) WaitCostPerHour() float64 {
	if r.WaitCost.Set {
		return r.WaitCost.V
	}
	return DefaultWaitCost
}

// DefaultMinGeneration is the oldest instance generation a vcpus-based
// runner considers: older ones are still "current" to AWS, but markedly
// slower per vCPU.
const DefaultMinGeneration = 6

// ByRequirements reports whether an aws-spot runner picks its instance
// types from vcpus (and min_memory_gib) rather than only a fixed list.
func (r *Runner) ByRequirements() bool { return r.Type == TypeAWSSpot && r.VCPUs > 0 }

// IsTypePattern reports whether an instance_types entry is a wildcard
// pattern (only meaningful with vcpus).
func IsTypePattern(t string) bool { return strings.Contains(t, "*") }

// MatchType reports whether an instance type matches a pattern in which *
// stands for any run of characters.
func MatchType(pattern, t string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == t
	}
	if !strings.HasPrefix(t, parts[0]) {
		return false
	}
	t = t[len(parts[0]):]
	for i, p := range parts[1:] {
		if i == len(parts)-2 {
			return strings.HasSuffix(t, p)
		}
		j := strings.Index(t, p)
		if j < 0 {
			return false
		}
		t = t[j+len(p):]
	}
	return true
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
	// A write-back bringing back more than WritebackMaxFiles files or
	// WritebackMaxBytes bytes is over the limit (0: no limit). Over it,
	// WritebackOver says what happens to the new files: warn brings them
	// back with a warning, hold leaves them in the slot for spotexec pull.
	WritebackMaxFiles int    `toml:"writeback_max_files"`
	WritebackMaxBytes Bytes  `toml:"writeback_max_bytes"`
	WritebackOver     string `toml:"writeback_over"` // warn|hold
}

// Write-back limit defaults.
const (
	DefaultWritebackMaxFiles = 1000
	DefaultWritebackMaxBytes = 100 << 20
)

// Deps is [deps].
type Deps struct {
	Lockfiles []string `toml:"lockfiles"`
	Install   string   `toml:"install"`
	CacheDirs []string `toml:"cache_dirs"`
	Hydrate   string   `toml:"hydrate"` // auto|reflink|copy|hardlink
	Timeout   Duration `toml:"timeout"`
}

// Setup is [setup]: a command run in the slot (and container) before every
// command, after the dependency restore, for steps that depend on source
// rather than lockfiles (building workspace packages, generating code).
type Setup struct {
	Run     string   `toml:"run"`
	Timeout Duration `toml:"timeout"` // default: the command's own timeout
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
	// Setup = false skips [setup] for this task (for a parallel task, for
	// every task it runs).
	Setup *bool `toml:"setup"`
	// Group prints each part of a parallel or sharded task as one block when
	// it finishes; nil leaves it to --group and the agent default.
	Group *bool `toml:"group"`
}

// SkipsSetup reports whether the task opts out of [setup].
func (t *Task) SkipsSetup() bool { return t.Setup != nil && !*t.Setup }

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
	Network    string   `toml:"network"`    // "host" (default: the runner's network) or "private" (a network of its own per run)
	User       string   `toml:"user"`       // "runner" (default: files stay owned by the runner user) or "root"
	Timeout    Duration `toml:"timeout"`    // pull/build limit (default 20m)
	// Docker lets commands in the container start containers of their own
	// (Testcontainers, docker compose) through a Docker socket scoped to the
	// run. It gives them root-equivalent access to the runner.
	Docker bool `toml:"docker"`
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
	Setup        Setup             `toml:"setup"`
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
	// Unavailable holds the project runners whose base isn't among this
	// user's runners, with the reason. They stay out of the runner set, so
	// only a command that selects one fails, with that reason.
	Unavailable map[string]error
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

// ConfigVersion is the config format this spotexec understands. A config
// without a version is read as this one.
const ConfigVersion = 1

// checkVersion rejects a config written for a format this spotexec doesn't
// know, rather than reading it under today's rules.
func checkVersion(md toml.MetaData, v int) error {
	if v == ConfigVersion {
		return nil
	}
	defined := false
	for _, k := range md.Keys() {
		// The decoder matches keys case-insensitively: Version is version.
		if len(k) == 1 && strings.EqualFold(k[0], "version") {
			defined = true
		}
	}
	if !defined {
		return nil
	}
	if v > ConfigVersion {
		return fmt.Errorf("version = %d isn't supported (supported: %d); upgrade spotexec to use this config", v, ConfigVersion)
	}
	return fmt.Errorf("version = %d isn't supported (supported: %d)", v, ConfigVersion)
}

// defined reports whether the file sets key as the decoder reads it: the
// decoder matches schema keys case-insensitively (TIMEOUT sets timeout),
// while MetaData.IsDefined is exact, so a check built on it could be
// sidestepped by spelling a key in capitals. The runner or task name under
// [runners] or [tasks] still matches exactly. Like IsDefined, a table counts
// as set when only keys inside it are (setup.timeout = "1m" sets [setup]).
func defined(md toml.MetaData, key ...string) bool {
	isName := func(i int) bool {
		return i == 1 && (strings.EqualFold(key[0], "runners") || strings.EqualFold(key[0], "tasks"))
	}
	for _, k := range md.Keys() {
		if len(k) < len(key) {
			continue
		}
		match := true
		for i := range key {
			if k[i] != key[i] && (isName(i) || !strings.EqualFold(k[i], key[i])) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// CheckVersion reads only the version of the config at path, for commands
// that edit the file without loading it.
func CheckVersion(path string) error {
	var v struct {
		Version int `toml:"version"`
	}
	md, err := toml.DecodeFile(path, &v)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := checkVersion(md, v.Version); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// LoadGlobal reads the global config; a missing file yields an empty config.
func LoadGlobal() (*Global, error) {
	path := GlobalPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Global{Runners: map[string]*Runner{}, Path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseGlobal(path, data)
}

// ParseGlobal reads a global config from data (the contents of the file at
// path), with the same defaults and validation as LoadGlobal.
func ParseGlobal(path string, data []byte) (*Global, error) {
	g := &Global{Runners: map[string]*Runner{}, Path: path}
	md, err := toml.Decode(string(data), g)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", g.Path, err)
	}
	if err := checkVersion(md, g.Version); err != nil {
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
		if defined(md, "runners", name, "extends_type") {
			return nil, fmt.Errorf("%s: runner %q: extends_type is for runners in a project's .spotexec.toml", g.Path, name)
		}
		if defined(md, "runners", name, "region") && defined(md, "runners", name, "regions") {
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
		if len(r.InstanceTypes) == 0 && r.VCPUs == 0 {
			r.InstanceTypes = []string{"c7a.2xlarge", "m7a.2xlarge", "c6a.2xlarge"} // the starter template's 8-vCPU types
		}
		if r.Arch == "" {
			r.Arch = "amd64"
			for _, t := range r.InstanceTypes {
				if !IsTypePattern(t) {
					r.Arch = archForInstanceType(t)
					break
				}
			}
		}
		if r.VCPUs > 0 && r.MinGeneration == 0 {
			r.MinGeneration = DefaultMinGeneration
		}
		if r.LaunchOrder == "" {
			r.LaunchOrder = LaunchRunCost
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
	if c := r.HourlyCost; c < 0 || math.IsNaN(c) || math.IsInf(c, 0) {
		return fmt.Errorf("hourly_cost must be a non-negative number of US dollars per hour (got %g)", c)
	}
	// A zero cap would quietly turn off the client-side checks while EC2
	// refuses every bid (an unparsable one fails to decode).
	if p := r.MaxPrice.V; r.MaxPrice.Set && !(p > 0 && p <= 1e4) {
		return fmt.Errorf("max_price must be a positive number of US dollars an hour, at most 10000, such as \"0.40\" (got %g)", p)
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
		if err := r.validateTypes(); err != nil {
			return err
		}
		if r.VolumeGB < 8 || r.VolumeGB > MaxVolumeGB {
			return fmt.Errorf("volume_gb must be between 8 and %d (got %d)", MaxVolumeGB, r.VolumeGB)
		}
	}
	return nil
}

// MaxVolumeGB is the largest root volume a runner may ask for (16 TiB): a
// spotexec limit well past any build's needs, so a typo can't buy a
// volume that bills by the terabyte.
const MaxVolumeGB = 16384

// validateTypes checks how an aws-spot runner chooses instance types.
func (r *Runner) validateTypes() error {
	for _, t := range append(append([]string(nil), r.InstanceTypes...), r.ExcludeInstanceTypes...) {
		if !typePatternRe.MatchString(t) {
			return fmt.Errorf("invalid instance type %q", t)
		}
	}
	for _, t := range r.InstanceTypes {
		if IsTypePattern(t) {
			if r.VCPUs == 0 {
				return fmt.Errorf("instance_types: %q is a pattern, which needs vcpus (patterns narrow the types vcpus and min_memory_gib select)", t)
			}
			continue
		}
		if archForInstanceType(t) != r.Arch {
			return fmt.Errorf("instance_types mixes architectures (%s is not %s): use one runner per architecture", t, r.Arch)
		}
	}
	if r.VCPUs < 0 || r.VCPUs > 448 {
		return fmt.Errorf("vcpus must be between 1 and 448 (got %d)", r.VCPUs)
	}
	if r.VCPUs == 0 {
		switch {
		case r.MinMemoryGiB != 0:
			return errors.New("min_memory_gib needs vcpus (the exact vCPU count of each VM)")
		case r.MinGeneration != 0:
			return errors.New("min_generation needs vcpus (the exact vCPU count of each VM)")
		case len(r.ExcludeInstanceTypes) > 0:
			return errors.New("exclude_instance_types needs vcpus; with a fixed list, just leave the type out of instance_types")
		}
	}
	if r.MinMemoryGiB < 0 || math.IsNaN(r.MinMemoryGiB) || math.IsInf(r.MinMemoryGiB, 0) {
		return fmt.Errorf("min_memory_gib must be a non-negative number (got %g)", r.MinMemoryGiB)
	}
	if r.MinGeneration < 0 {
		return fmt.Errorf("min_generation must not be negative (got %d)", r.MinGeneration)
	}
	switch r.LaunchOrder {
	case LaunchRunCost, LaunchCheapest:
	case LaunchListed:
		if len(r.InstanceTypes) == 0 {
			return errors.New(`launch_order = "listed" needs instance_types (the order to try)`)
		}
	default:
		return fmt.Errorf("launch_order must be %q, %q or %q (got %q)", LaunchRunCost, LaunchCheapest, LaunchListed, r.LaunchOrder)
	}
	if w := r.WaitCost.V; r.WaitCost.Set && (w < 0 || w > 1e4 || math.IsNaN(w) || math.IsInf(w, 0)) {
		return fmt.Errorf("wait_cost must be between 0 and 10000 US dollars per hour of waiting (got %g)", w)
	}
	return nil
}

// normalizeRegions validates regions and folds a single concrete region
// into region (so it behaves exactly like a single-region runner). For
// several regions, region becomes the home region: the first listed, or
// for "all" the environment default, used for account-wide lookups.
func (r *Runner) normalizeRegions() error {
	if len(r.ExcludeRegions) > 0 && !(len(r.Regions) == 1 && r.Regions[0] == AllRegions) {
		return errors.New(`exclude_regions needs regions = ["all"]; with a list, just leave the region out of regions`)
	}
	for _, rg := range r.ExcludeRegions {
		if !regionRe.MatchString(rg) {
			return fmt.Errorf("exclude_regions: invalid region %q", rg)
		}
	}
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
		if err := checkVersion(md, p.Version); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var und []toml.Key
		for _, k := range md.Undecoded() {
			if len(k) > 0 && strings.EqualFold(k[0], "runners") {
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
	// Defaults fill unset durations; an explicit zero is left for validate
	// to reject.
	if p.Timeout.Duration == 0 && !defined(p.md, "timeout") {
		p.Timeout.Duration = 5 * time.Minute
	}
	if p.Grace.Duration == 0 && !defined(p.md, "grace_period") {
		p.Grace.Duration = 5 * time.Second
	}
	if p.QueueTimeout.Duration == 0 && !defined(p.md, "queue_timeout") {
		p.QueueTimeout.Duration = 10 * time.Minute
	}
	if p.Sync.Mode == "" {
		p.Sync.Mode = "auto"
	}
	if p.Sync.Writeback == "" {
		p.Sync.Writeback = "always"
	}
	// An explicit 0 turns a limit off, so only a missing key gets the default.
	if !defined(p.md, "sync", "writeback_max_files") {
		p.Sync.WritebackMaxFiles = DefaultWritebackMaxFiles
	}
	if !defined(p.md, "sync", "writeback_max_bytes") {
		p.Sync.WritebackMaxBytes = DefaultWritebackMaxBytes
	}
	if p.Sync.WritebackOver == "" {
		p.Sync.WritebackOver = "warn"
	}
	if p.Deps.Hydrate == "" {
		p.Deps.Hydrate = "auto"
	}
	if p.Deps.Timeout.Duration == 0 && !defined(p.md, "deps", "timeout") {
		p.Deps.Timeout.Duration = 20 * time.Minute
	}
	if p.Container.Timeout.Duration == 0 && !defined(p.md, "container", "timeout") {
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
	if !oneOf(p.Sync.WritebackOver, "warn", "hold") {
		return fmt.Errorf("sync.writeback_over must be warn or hold (got %q)", p.Sync.WritebackOver)
	}
	if p.Sync.WritebackMaxFiles < 0 {
		return fmt.Errorf("sync.writeback_max_files must be 0 (no limit) or more (got %d)", p.Sync.WritebackMaxFiles)
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
	if !oneOf(p.Container.Network, "", "host", "private") {
		return fmt.Errorf(`container.network must be "host" or "private" (got %q)`, p.Container.Network)
	}
	if p.Container.Docker && !p.Container.Enabled() {
		return errors.New("container.docker requires container.image or container.dockerfile")
	}
	if p.Deps.Install != "" && len(p.Deps.CacheDirs) == 0 {
		return errors.New("deps.install requires deps.cache_dirs")
	}
	if p.Deps.Install != "" && len(p.Deps.Lockfiles) == 0 {
		return errors.New("deps.install requires deps.lockfiles (the cache key)")
	}
	if _, _, err := depskey.Expand(p.Deps.Lockfiles, nil); err != nil {
		return err // a malformed or escaping pattern; literal paths are checked when read
	}
	if defined(p.md, "setup") && strings.TrimSpace(p.Setup.Run) == "" {
		return errors.New("setup.run must be set when [setup] is present")
	}
	for _, d := range []struct {
		key []string
		v   time.Duration
	}{
		{[]string{"timeout"}, p.Timeout.Duration},
		{[]string{"grace_period"}, p.Grace.Duration},
		{[]string{"queue_timeout"}, p.QueueTimeout.Duration},
		{[]string{"deps", "timeout"}, p.Deps.Timeout.Duration},
		{[]string{"setup", "timeout"}, p.Setup.Timeout.Duration},
		{[]string{"container", "timeout"}, p.Container.Timeout.Duration},
	} {
		if err := p.positive(d.v, d.key...); err != nil {
			return err
		}
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
		if t.Shards != 0 || defined(p.md, "tasks", name, "shards") {
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
		if err := p.positive(t.Timeout.Duration, "tasks", name, "timeout"); err != nil {
			return err
		}
	}
	return nil
}

// positive rejects a negative duration, or a zero one the file sets (unset
// means the default): a zero timeout would end every command at once.
func (p *Project) positive(d time.Duration, key ...string) error {
	if d < 0 || d == 0 && defined(p.md, key...) {
		return fmt.Errorf("%s must be positive (got %s)", strings.Join(key, "."), d)
	}
	return nil
}

// globalOnlyKeys are runner settings a project (.spotexec.toml) may not set.
// ami is among them because an image without cloud-init never arms the
// watchdog, so its VMs never power off; on_demand_fallback because it
// trades Spot prices for on-demand ones.
var globalOnlyKeys = []string{
	"type", "host", "hosts", "ssh_options", "identity_file", "work_root",
	"profile", "region", "subnet_id", "security_group_ids", "ssh_cidr",
	"user", "cache_bucket", "regions", "exclude_regions", "ami",
	"on_demand_fallback",
}

var (
	runnerNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	regionRe      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	userRe        = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	typePatternRe = regexp.MustCompile(`^[a-z0-9*][a-z0-9.*-]{0,40}$`)
)

// MergeProjectRunners decodes runners declared in .spotexec.toml and adds
// them to the global set (a project runner with the same name as a global
// one overrides it). `extends = "name"` starts from a copy of a global
// runner, so a project can keep your account/region settings and change only
// sizes, slots or bootstrap; `extends_type = "aws-spot"` starts from your
// runner of that type, for a repository whose collaborators name their
// runners differently. A project runner whose base this user doesn't have is
// recorded in Unavailable instead of failing the whole config.
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
			if defined(p.md, "runners", name, key) {
				return fmt.Errorf("%s: runner %q: %q can only be set in the global config (%s)", p.Path, name, key, GlobalPath())
			}
		}
		var probe Runner
		if err := p.md.PrimitiveDecode(prim, &probe); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		if probe.ExtendsType != "" && !oneOf(probe.ExtendsType, TypeSSH, TypeAWSSpot, TypeSim) {
			return fmt.Errorf("%s: runner %q: extends_type must be %s, %s or %s (got %q)", p.Path, name, TypeSSH, TypeAWSSpot, TypeSim, probe.ExtendsType)
		}
		// The checks that don't depend on the base (slots, durations) hold
		// whether or not this user has one; the rest run once it's merged.
		if err := probe.validate(); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		g, err := c.projectBase(global, name, probe)
		if err != nil {
			if c.Unavailable == nil {
				c.Unavailable = map[string]error{}
			}
			// A global runner of the same name is not what the project meant:
			// Runner and RunnerNames hide it, so selecting the name can't
			// quietly run on the wrong machine. It stays in Global.Runners for
			// what spans the account (status -A, down -A, quotas).
			c.Unavailable[name] = err
			continue
		}
		base := *g
		base.Hosts = append([]string(nil), g.Hosts...)
		base.InstanceTypes = append([]string(nil), g.InstanceTypes...)
		base.SSHOptions = append([]string(nil), g.SSHOptions...)
		base.SecurityGroupIDs = append([]string(nil), g.SecurityGroupIDs...)
		base.Regions = append([]string(nil), g.Regions...)
		base.ExcludeRegions = append([]string(nil), g.ExcludeRegions...)
		base.ExcludeInstanceTypes = append([]string(nil), g.ExcludeInstanceTypes...)
		// The project's way of choosing types replaces the inherited one: new
		// instance_types alone drop inherited requirements, and requirements
		// alone drop inherited instance_types (which would otherwise narrow
		// them).
		setsTypes := defined(p.md, "runners", name, "instance_types")
		setsReqs := false
		for _, k := range []string{"vcpus", "min_memory_gib", "min_generation", "exclude_instance_types"} {
			setsReqs = setsReqs || defined(p.md, "runners", name, k)
		}
		if setsTypes && !setsReqs {
			base.VCPUs, base.MinMemoryGiB, base.MinGeneration, base.ExcludeInstanceTypes = 0, 0, 0, nil
		}
		if setsReqs && !setsTypes {
			base.InstanceTypes = nil
			// "listed" needs the list just dropped; run_cost and cheapest
			// carry over.
			if !defined(p.md, "runners", name, "launch_order") && base.LaunchOrder == LaunchListed {
				base.LaunchOrder = ""
			}
		}
		// Re-derive the arch from the new instance types, unless they are all
		// patterns, which narrow the base's types without naming an arch.
		if setsTypes && !defined(p.md, "runners", name, "arch") && !allPatterns(probe.InstanceTypes) {
			base.Arch = ""
		}
		if err := p.md.PrimitiveDecode(prim, &base); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		base.Name, base.Extends, base.ExtendsType = name, "", ""
		if err := base.applyDefaults(); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		if err := base.withinBase(g); err != nil {
			return fmt.Errorf("%s: runner %q: %w", p.Path, name, err)
		}
		c.Global.Runners[name] = &base
	}
	if und := p.md.Undecoded(); len(und) > 0 {
		var keys []string
		for _, k := range und {
			if len(k) > 0 && strings.EqualFold(k[0], "runners") {
				keys = append(keys, k.String())
			}
		}
		if len(keys) > 0 {
			return fmt.Errorf("%s: unknown keys: %s", p.Path, strings.Join(keys, ", "))
		}
	}
	for _, n := range p.Pool {
		if !c.known(n) {
			return fmt.Errorf("%s: pool references unknown runner %q", p.Path, n)
		}
	}
	for _, task := range p.TaskNames() {
		for _, n := range p.Tasks[task].Pool {
			if !c.known(n) {
				return fmt.Errorf("%s: task %q: pool references unknown runner %q", p.Path, task, n)
			}
		}
	}
	return nil
}

// known reports whether a pool may name n: a configured runner, or a project
// runner this user's runners can't back (which fails only where it's used).
func (c *Config) known(n string) bool {
	_, ok := c.Global.Runners[n]
	return ok || c.Unavailable[n] != nil
}

// allPatterns reports whether types is a non-empty list of wildcard
// patterns only.
func allPatterns(types []string) bool {
	for _, t := range types {
		if !IsTypePattern(t) {
			return false
		}
	}
	return len(types) > 0
}

// withinBase rejects a project runner that would spend more than the
// user's runner it builds on allows: a repository can lower the fleet
// ceiling, the idle and lifetime limits and the Spot price cap, never
// raise or remove them.
func (r *Runner) withinBase(g *Runner) error {
	if !g.Ephemeral() {
		return nil
	}
	yours := fmt.Sprintf("your runner %q", g.Name)
	if r.MaxInstances > g.MaxInstances {
		return fmt.Errorf("max_instances = %d is more than %s allows (%d); a project can lower it, not raise it", r.MaxInstances, yours, g.MaxInstances)
	}
	for _, d := range []struct {
		key       string
		got, base time.Duration
	}{
		{"idle_timeout", r.IdleTimeout.Duration, g.IdleTimeout.Duration},
		{"max_lifetime", r.MaxLifetime.Duration, g.MaxLifetime.Duration},
	} {
		if d.got > d.base {
			return fmt.Errorf("%s = %s is longer than %s allows (%s); a project can shorten it, not lengthen it", d.key, d.got, yours, d.base)
		}
	}
	if !g.MaxPrice.Set {
		return nil
	}
	if !r.MaxPrice.Set {
		return fmt.Errorf("max_price can't be removed: %s caps the Spot price at %s an hour", yours, g.MaxPrice)
	}
	if r.MaxPrice.V > g.MaxPrice.V {
		return fmt.Errorf("max_price = %q is more than %s allows (%s an hour); a project can lower it, not raise it", r.MaxPrice.String(), yours, g.MaxPrice)
	}
	return nil
}

// projectBase picks the global runner a project runner builds on: the one
// extends names (or its own name, with neither key set), else, with
// extends_type, the user's only runner of that type, or the first of that
// type in default_pool when there are several. The error says what is
// missing, for a collaborator whose runners don't match the repository's.
func (c *Config) projectBase(global map[string]*Runner, name string, probe Runner) (*Runner, error) {
	want := probe.Extends
	if want == "" && probe.ExtendsType == "" {
		want = name
	}
	if g, ok := global[want]; ok && (probe.ExtendsType == "" || g.Type == probe.ExtendsType) {
		return g, nil
	}
	named := global[want]
	var matches []string
	if probe.ExtendsType != "" {
		for n, g := range global {
			if g.Type == probe.ExtendsType {
				matches = append(matches, n)
			}
		}
		sort.Strings(matches)
		if len(matches) == 1 {
			return global[matches[0]], nil
		}
		for _, n := range c.Global.DefaultPool {
			if g, ok := global[n]; ok && g.Type == probe.ExtendsType {
				return g, nil
			}
		}
	}
	var yours []string
	for n, g := range global {
		yours = append(yours, n+" ("+g.Type+")")
	}
	sort.Strings(yours)
	have := "you have no runners"
	if len(yours) > 0 {
		have = "yours: " + strings.Join(yours, ", ")
	}
	what := fmt.Sprintf("runner %q from %s", name, c.Project.Path)
	switch {
	case len(matches) > 1:
		return nil, fmt.Errorf("%s can use any of your %s runners, and you have several (%s): list the one to use in default_pool in %s",
			what, probe.ExtendsType, strings.Join(matches, ", "), c.Global.Path)
	case named != nil:
		return nil, fmt.Errorf("%s needs a %s runner: your runner %q is %s, and you have no %s runner (%s): add one to %s",
			what, probe.ExtendsType, want, named.Type, probe.ExtendsType, have, c.Global.Path)
	case probe.ExtendsType != "" && probe.Extends != "":
		return nil, fmt.Errorf("%s needs your runner %q or any %s runner, and you have neither (%s): add one to %s",
			what, probe.Extends, probe.ExtendsType, have, c.Global.Path)
	case probe.ExtendsType != "":
		return nil, fmt.Errorf("%s needs one of your %s runners, and you have none (%s): add one to %s",
			what, probe.ExtendsType, have, c.Global.Path)
	case probe.Extends != "":
		return nil, fmt.Errorf("%s extends %q, which isn't one of your runners (%s): add a runner named %q to %s",
			what, probe.Extends, have, probe.Extends, c.Global.Path)
	}
	return nil, fmt.Errorf("%s overrides your runner %q, which you don't have (%s); a project runner builds on one of yours with extends or extends_type",
		what, name, have)
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
		r, err := c.Runner(n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

// RunnerNames lists configured runner names, without the project runners
// this user's runners can't back.
func (c *Config) RunnerNames() []string {
	var n []string
	for k := range c.Global.Runners {
		if c.Unavailable[k] == nil {
			n = append(n, k)
		}
	}
	sort.Strings(n)
	return n
}

// UnavailableNames lists the project runners this user's runners can't
// back (see Config.Unavailable), sorted.
func (c *Config) UnavailableNames() []string {
	var n []string
	for k := range c.Unavailable {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Runner returns a runner by name.
func (c *Config) Runner(name string) (*Runner, error) {
	if err := c.Unavailable[name]; err != nil {
		return nil, err
	}
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
