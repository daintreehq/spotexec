package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectRunnerExtends(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "global.toml")
	os.WriteFile(gp, []byte(`
version = 1
[runners.aws]
type = "aws-spot"
region = "us-east-2"
profile = "dev"
instance_types = ["c7a.large"]
max_slots = 2
`), 0o644)
	t.Setenv("SPOTEXEC_CONFIG", gp)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(dir, ".spotexec.toml")
	os.WriteFile(pp, []byte(`
version = 1
pool = ["big"]
[runners.big]
extends = "aws"
instance_types = ["c7g.2xlarge"]
max_slots = 1
max_instances = 3
`), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Global: g, Project: p}
	if err := c.MergeProjectRunners(); err != nil {
		t.Fatal(err)
	}
	r := c.Global.Runners["big"]
	if r == nil || r.Region != "us-east-2" || r.Profile != "dev" || r.MaxSlots != 1 || r.MaxInstances != 3 {
		t.Fatalf("bad merge: %+v", r)
	}
	if r.Arch != "arm64" {
		t.Fatalf("arch should be re-derived for Graviton types, got %q", r.Arch)
	}
	if c.Global.Runners["aws"].MaxSlots != 2 {
		t.Fatal("base runner must not be modified")
	}
	if r.IdleTimeout.Duration.Minutes() != 10 {
		t.Fatalf("default idle timeout should be 10m, got %s", r.IdleTimeout.Duration)
	}
}

func TestEmbeddedArgs(t *testing.T) {
	for run, want := range map[string]bool{
		"pytest {args}":             false,
		`pytest "{args}"`:           false,
		"(cd pkg && pytest {args})": false,
		"pnpm vitest run --minWorkers=1 --maxWorkers=$SPOTEXEC_CPUS {args}": false,
		`sh -c "pytest {args} -v"`:  true,
		`echo "testing {args} now"`: true,
		"pytest --k={args}":         true,
		`sh -c 'pytest {args}'`:     true,
	} {
		if got := embeddedArgs(run); got != want {
			t.Errorf("embeddedArgs(%q) = %v, want %v", run, got, want)
		}
	}
}

func TestTasksMayShareSubcommandNames(t *testing.T) {
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	os.WriteFile(pp, []byte(`
[tasks.sync]
run = "pnpm sync"
[tasks.status]
run = "git status"
[tasks.run]
parallel = ["sync", "status"]
`), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tasks) != 3 {
		t.Fatalf("tasks: %v", p.TaskNames())
	}
}

func TestNeverProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if !NeverProject(home) || !NeverProject(home+"/") || !NeverProject("/") {
		t.Fatal("home and / must never be projects")
	}
	sub := filepath.Join(home, "repo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if NeverProject(sub) {
		t.Fatal("a directory under home is fine")
	}
	if !NeverProject(filepath.Join(home, ".ssh")) || !NeverProject(filepath.Join(sub, ".AWS", "x")) {
		t.Fatal("credential directories must never be projects")
	}
	for _, d := range []string{home, sub} {
		if err := os.WriteFile(filepath.Join(d, ".spotexec.toml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := FindProject(filepath.Join(home, "other"), home); got != "" {
		t.Fatalf("FindProject found %s in home", got)
	}
	if got := FindProject(sub, sub); got != filepath.Join(sub, ".spotexec.toml") {
		t.Fatalf("FindProject(sub) = %q", got)
	}
}

// Credential directories reached through a symlink, and home spelled in
// another case on a case-insensitive disk, are still never projects.
func TestNeverProjectByIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	keys := filepath.Join(home, "privatekeys")
	if err := os.MkdirAll(filepath.Join(keys, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keys, filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{keys, filepath.Join(keys, "sub"), filepath.Join(home, ".ssh", "sub")} {
		if !NeverProject(d) {
			t.Errorf("NeverProject(%s) = false: it is ~/.ssh", d)
		}
	}
	if NeverProject(filepath.Join(home, "repo")) {
		t.Error("an ordinary directory refused")
	}
	upper := strings.ToUpper(home)
	if fi, err := os.Stat(upper); err != nil || !fi.IsDir() {
		t.Skip("case-sensitive filesystem")
	}
	if !NeverProject(upper) {
		t.Errorf("NeverProject(%s) = false on a case-insensitive disk", upper)
	}
}

func loadGlobalText(t *testing.T, body string) (*Global, error) {
	t.Helper()
	gp := filepath.Join(t.TempDir(), "global.toml")
	if err := os.WriteFile(gp, []byte("version = 1\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", gp)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	return LoadGlobal()
}

func TestRegions(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"all\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	r := g.Runners["aws"]
	if !r.MultiRegion() || !r.AnyRegion() || r.Region != "us-east-1" {
		t.Fatalf("all: %+v", r)
	}
	g, err = loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"eu-north-1\", \"us-east-2\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.Runners["aws"]; !r.MultiRegion() || r.AnyRegion() || r.Region != "eu-north-1" {
		t.Fatalf("list: %+v", r)
	}
	// One region is just a single-region runner.
	g, err = loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"eu-north-1\"]\nsubnet_id = \"subnet-1\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.Runners["aws"]; r.MultiRegion() || r.Region != "eu-north-1" {
		t.Fatalf("single: %+v", r)
	}
	for _, ok := range []string{"ami = \"auto\"", "ami = \"base\"", "cache_bucket = \"auto\"", "cache_bucket = \"off\""} {
		if _, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"all\"]\n"+ok+"\n"); err != nil {
			t.Errorf("%s with regions: %v", ok, err)
		}
	}
	for name, body := range map[string]string{
		"region and regions": "region = \"us-east-1\"\nregions = [\"us-east-1\", \"us-east-2\"]\n",
		"all plus a region":  "regions = [\"all\", \"us-east-1\"]\n",
		"invalid region":     "regions = [\"us-east-1\", \"mars\"]\n",
		"duplicate":          "regions = [\"us-east-1\", \"us-east-1\"]\n",
		"subnet":             "regions = [\"all\"]\nsubnet_id = \"subnet-1\"\n",
		"security groups":    "regions = [\"all\"]\nsecurity_group_ids = [\"sg-1\"]\n",
		"ami id":             "regions = [\"us-east-1\", \"us-east-2\"]\nami = \"ami-123\"\n",
		"named cache bucket": "regions = [\"all\"]\ncache_bucket = \"my-bucket\"\n",
		"china region":       "region = \"cn-north-1\"\n",
		"china in a list":    "regions = [\"us-east-1\", \"cn-northwest-1\"]\n",
		"govcloud mixed":     "regions = [\"us-east-1\", \"us-gov-west-1\"]\n",
	} {
		if _, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\n"+body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProjectCannotSetRegions(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"eu-north-1\", \"us-east-2\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	os.WriteFile(pp, []byte("version = 1\n[runners.aws2]\nextends = \"aws\"\nregions = [\"all\"]\n"), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Config{Global: g, Project: p}).MergeProjectRunners(); err == nil || !strings.Contains(err.Error(), `"regions" can only be set in the global config`) {
		t.Fatalf("a repository must not widen where code runs: %v", err)
	}
	// Extending a multi-region runner keeps its regions.
	os.WriteFile(pp, []byte("version = 1\n[runners.big]\nextends = \"aws\"\nmax_slots = 1\n"), 0o644)
	p, _ = LoadProject(pp)
	c := &Config{Global: g, Project: p}
	if err := c.MergeProjectRunners(); err != nil {
		t.Fatal(err)
	}
	r := c.Global.Runners["big"]
	if !r.MultiRegion() || strings.Join(r.Regions, ",") != "eu-north-1,us-east-2" || r.Region != "eu-north-1" || r.MaxSlots != 1 {
		t.Fatalf("extended: %+v", r)
	}
	r.Regions[0] = "changed"
	if g.Runners["aws"].Regions[0] != "eu-north-1" {
		t.Fatal("extending must copy the region list")
	}
}

func TestTaskShards(t *testing.T) {
	load := func(tasks string) (*Project, error) {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		if err := os.WriteFile(pp, []byte(tasks), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadProject(pp)
	}
	p, err := load(`
[tasks.unit]
run = "npx vitest run --shard={shard}/{shards} {args}"
shards = 8
[tasks.env-only]
run = "./test.sh"
shards = 2
[tasks.all]
parallel = ["unit", "lint"]
[tasks.lint]
run = "eslint ."
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Tasks["unit"].Describe(); got != "8 shards: npx vitest run --shard={shard}/{shards} {args}" {
		t.Errorf("Describe: %q", got)
	}
	if got := p.Tasks["all"].Describe(); got != "parallel: unit, lint" {
		t.Errorf("Describe: %q", got)
	}
	if got := p.Tasks["lint"].Describe(); got != "eslint ." {
		t.Errorf("Describe: %q", got)
	}
	for tasks, want := range map[string]string{
		"[tasks.a]\nrun = \"x\"\nshards = 0\n":                                                   "shards must be between 1 and 64 (got 0)",
		"[tasks.a]\nrun = \"x\"\nshards = -2\n":                                                  "shards must be between 1 and 64",
		"[tasks.a]\nrun = \"x\"\nshards = 65\n":                                                  "shards must be between 1 and 64 (got 65)",
		"[tasks.a]\nparallel = [\"b\"]\nshards = 2\n[tasks.b]\nrun = \"x\"\n":                    "shards applies to a run task, not a parallel one",
		"[tasks.a]\nrun = \"vitest --shard={shard}/8\"\n":                                        "{shard} and {shards} need shards = N",
		"[tasks.a]\nrun = \"echo {shards}\"\n":                                                   "{shard} and {shards} need shards = N",
		"[tasks.a]\nshards = 2\n":                                                                "must set exactly one of run or parallel",
		"[tasks.a]\nparallel = [\"b\"]\n[tasks.b]\nparallel = [\"c\"]\n[tasks.c]\nrun = \"x\"\n": "nested parallel task",
	} {
		if _, err := load(tasks); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", tasks, err, want)
		}
	}
	if p, err := load("[tasks.a]\nrun = \"x {shard}\"\nshards = 1\n"); err != nil || p.Tasks["a"].Describe() != "1 shard: x {shard}" {
		t.Errorf("one shard: %v", err)
	}
}

func TestTaskGroup(t *testing.T) {
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	toml := "[tasks.a]\nrun = \"x\"\nshards = 2\ngroup = true\n[tasks.b]\nrun = \"y\"\ngroup = false\n[tasks.c]\nparallel = [\"a\", \"b\"]\n"
	if err := os.WriteFile(pp, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	// Unset stays unset, so --group and the agent default can decide.
	if g := p.Tasks["a"].Group; g == nil || !*g {
		t.Errorf("a: %v", g)
	}
	if g := p.Tasks["b"].Group; g == nil || *g {
		t.Errorf("b: %v", g)
	}
	if g := p.Tasks["c"].Group; g != nil {
		t.Errorf("c: %v", *g)
	}
	if err := os.WriteFile(pp, []byte("[tasks.a]\nrun = \"x\"\ngroup = \"yes\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProject(pp); err == nil {
		t.Error("group = \"yes\" loaded")
	}
}

func TestDepsLockfilePatternsValidated(t *testing.T) {
	for lockfiles, ok := range map[string]bool{
		`["package-lock.json"]`:                                   true,
		`["package-lock.json", "packages/*/package.json"]`:        true,
		`["electron/native/*/src/**", "scripts/patch[A-Z]*.cjs"]`: true,
		`["../*.lock"]`: false,
		`["docs/[a"]`:   false,
		`[".git/*"]`:    false,
	} {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		os.WriteFile(pp, []byte("[deps]\ninstall = \"npm ci\"\ncache_dirs = [\"node_modules\"]\nlockfiles = "+lockfiles+"\n"), 0o644)
		_, err := LoadProject(pp)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", lockfiles, err, ok)
		}
	}
}

func TestContainerNetworkValidated(t *testing.T) {
	for network, ok := range map[string]bool{
		"":        true,
		"host":    true,
		"private": true,
		"bridge":  false,
		"Private": false,
	} {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		body := "[container]\nimage = \"node:22\"\n"
		if network != "" {
			body += "network = \"" + network + "\"\n"
		}
		os.WriteFile(pp, []byte(body), 0o644)
		p, err := LoadProject(pp)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", network, err, ok)
		}
		if err == nil && p.Container.Network != network {
			t.Errorf("%q: loaded %q", network, p.Container.Network)
		}
	}
}

func TestInstanceRequirements(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregion = \"us-east-1\"\nvcpus = 8\nmin_memory_gib = 32\n")
	if err != nil {
		t.Fatal(err)
	}
	r := g.Runners["aws"]
	if !r.ByRequirements() || len(r.InstanceTypes) != 0 || r.Arch != "amd64" || r.MinGeneration != DefaultMinGeneration || r.LaunchOrder != LaunchCheapest {
		t.Fatalf("vcpus runner: %+v", r)
	}
	g, err = loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nvcpus = 8\narch = \"arm64\"\ninstance_types = [\"m*g*\", \"c7g.2xlarge\"]\nexclude_instance_types = [\"*d.*\"]\nmin_generation = 7\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.Runners["aws"]; r.Arch != "arm64" || r.MinGeneration != 7 {
		t.Fatalf("patterns: %+v", r)
	}
	// A fixed list keeps its default order, derived arch and default types.
	g, err = loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.Runners["aws"]; r.ByRequirements() || len(r.InstanceTypes) != 3 || r.MinGeneration != 0 {
		t.Fatalf("default: %+v", r)
	}
	for name, body := range map[string]string{
		"pattern without vcpus":     "instance_types = [\"c7a.*\"]\n",
		"memory without vcpus":      "min_memory_gib = 32\n",
		"generation without vcpus":  "min_generation = 7\n",
		"exclusions without vcpus":  "exclude_instance_types = [\"m5*\"]\n",
		"negative vcpus":            "vcpus = -1\n",
		"negative memory":           "vcpus = 8\nmin_memory_gib = -1\n",
		"nan memory":                "vcpus = 8\nmin_memory_gib = nan\n",
		"infinite memory":           "vcpus = 8\nmin_memory_gib = inf\n",
		"mixed arch":                "vcpus = 8\narch = \"amd64\"\ninstance_types = [\"c7g.2xlarge\"]\n",
		"bad pattern":               "vcpus = 8\ninstance_types = [\"c7a.[2]xlarge\"]\n",
		"bad launch order":          "launch_order = \"random\"\n",
		"listed without types":      "vcpus = 8\nlaunch_order = \"listed\"\n",
		"listed list with no types": "instance_types = []\nvcpus = 4\nlaunch_order = \"listed\"\n",
	} {
		if _, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\n"+body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatchType(t *testing.T) {
	for _, c := range []struct {
		p, t string
		ok   bool
	}{
		{"m7a.2xlarge", "m7a.2xlarge", true}, {"m7a.2xlarge", "m7a.4xlarge", false},
		{"m*", "m7a.2xlarge", true}, {"m*", "c7a.2xlarge", false}, {"*7*", "r7iz.2xlarge", true},
		{"c7a.*", "c7a.large", true}, {"*.2xlarge", "c7a.4xlarge", false}, {"a*a", "a", false}, {"m*g.*", "m7g.large", true},
		{"*n.*", "m7a.2xlarge", false}, {"*n.*", "c6in.large", true}, {"*7*a*", "m7a.large", true}, {"*a*7*", "m7a.large", false},
		{"m**a.*", "m7a.large", true}, {"m*x*z", "m7a.2xlarge", false},
	} {
		if got := MatchType(c.p, c.t); got != c.ok {
			t.Errorf("MatchType(%q, %q) = %v", c.p, c.t, got)
		}
	}
}

// A project switches how types are chosen: requirements replace an
// inherited list, and a list replaces inherited requirements.
func TestProjectRequirementsReplaceTypes(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregion = \"us-east-1\"\ninstance_types = [\"c7a.2xlarge\", \"m7a.2xlarge\"]\nlaunch_order = \"listed\"\n[runners.req]\ntype = \"aws-spot\"\nregion = \"us-east-1\"\nvcpus = 8\nmin_memory_gib = 16\nexclude_instance_types = [\"m5*\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	os.WriteFile(pp, []byte(`version = 1
[runners.big]
extends = "aws"
vcpus = 8
min_memory_gib = 32
[runners.listed]
extends = "req"
instance_types = ["c7a.2xlarge"]
[runners.narrow]
extends = "req"
min_memory_gib = 32
`), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Global: g, Project: p}
	if err := c.MergeProjectRunners(); err != nil {
		t.Fatal(err)
	}
	if r := c.Global.Runners["big"]; !r.ByRequirements() || len(r.InstanceTypes) != 0 || r.MinMemoryGiB != 32 || r.LaunchOrder != LaunchCheapest {
		t.Fatalf("big: %+v", r)
	}
	if r := c.Global.Runners["listed"]; r.ByRequirements() || r.MinGeneration != 0 || len(r.ExcludeInstanceTypes) != 0 || r.InstanceTypes[0] != "c7a.2xlarge" {
		t.Fatalf("listed: %+v", r)
	}
	if r := c.Global.Runners["narrow"]; r.VCPUs != 8 || r.MinMemoryGiB != 32 || len(r.ExcludeInstanceTypes) != 1 {
		t.Fatalf("narrow: %+v", r)
	}
	// The merged runners own their lists.
	c.Global.Runners["listed"].InstanceTypes[0] = "x"
	c.Global.Runners["narrow"].ExcludeInstanceTypes[0] = "x"
	if g.Runners["aws"].InstanceTypes[0] != "c7a.2xlarge" || g.Runners["req"].ExcludeInstanceTypes[0] != "m5*" || len(g.Runners["req"].InstanceTypes) != 0 {
		t.Fatal("base runner must not be modified")
	}
}

// A committed project fleet whose base isn't one of this user's runners must
// not break commands that don't use it (#66).
func TestUnresolvedProjectRunnerDoesNotBreakLoading(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.homelab]\ntype = \"ssh\"\nhost = \"box\"\n")
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	os.WriteFile(pp, []byte("version = 1\n[runners.aws-big]\nextends = \"aws\"\ninstance_types = [\"m7a.2xlarge\"]\nmax_slots = 1\n"), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Global: g, Project: p}
	if err := c.MergeProjectRunners(); err != nil {
		t.Fatalf("an unusable project runner must not fail loading: %v", err)
	}
	rs, err := c.Pool(nil)
	if err != nil || len(rs) != 1 || rs[0].Name != "homelab" {
		t.Fatalf("the default pool should be the user's own runners: %v %v", rs, err)
	}
	if _, err := c.Pool([]string{"aws-big"}); err == nil || !strings.Contains(err.Error(), "aws-big") || !strings.Contains(err.Error(), `"aws"`) {
		t.Fatalf("targeting the fleet should say what's missing: %v", err)
	}
}

func mergeProject(t *testing.T, g *Global, body string) (*Config, error) {
	t.Helper()
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	if err := os.WriteFile(pp, []byte("version = 1\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Global: g, Project: p}
	return c, c.MergeProjectRunners()
}

func TestProjectRunnerExtendsType(t *testing.T) {
	const fleet = "[runners.aws-big]\nextends = \"aws\"\nextends_type = \"aws-spot\"\ninstance_types = [\"m7a.2xlarge\"]\nmax_slots = 1\n"
	// A collaborator whose AWS runner has another name still gets the fleet.
	g, err := loadGlobalText(t, "[runners.spot]\ntype = \"aws-spot\"\nregion = \"eu-west-1\"\n[runners.homelab]\ntype = \"ssh\"\nhost = \"box\"\n")
	if err != nil {
		t.Fatal(err)
	}
	c, err := mergeProject(t, g, fleet)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Runner("aws-big")
	if err != nil || r.Region != "eu-west-1" || r.MaxSlots != 1 || r.InstanceTypes[0] != "m7a.2xlarge" || r.Type != TypeAWSSpot {
		t.Fatalf("extends_type should pick the only aws-spot runner: %+v %v", r, err)
	}

	// The named runner wins when it exists and has the right type.
	g, _ = loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregion = \"us-east-2\"\n[runners.spot]\ntype = \"aws-spot\"\nregion = \"eu-west-1\"\n")
	c, err = mergeProject(t, g, fleet)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Runner("aws-big"); r == nil || r.Region != "us-east-2" {
		t.Fatalf("extends should be preferred: %+v", r)
	}

	// A named runner of another type doesn't count.
	g, _ = loadGlobalText(t, "[runners.aws]\ntype = \"ssh\"\nhost = \"box\"\n[runners.spot]\ntype = \"aws-spot\"\nregion = \"eu-west-1\"\n")
	c, err = mergeProject(t, g, fleet)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Runner("aws-big"); r == nil || r.Type != TypeAWSSpot || r.Region != "eu-west-1" {
		t.Fatalf("the base must have the requested type: %+v", r)
	}

	// Several candidates: default_pool decides, or the reason says how to choose.
	const two = "[runners.a]\ntype = \"aws-spot\"\nregion = \"us-east-2\"\n[runners.b]\ntype = \"aws-spot\"\nregion = \"eu-west-1\"\n"
	g, _ = loadGlobalText(t, two)
	c, err = mergeProject(t, g, "[runners.aws-big]\nextends_type = \"aws-spot\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Runner("aws-big"); err == nil || !strings.Contains(err.Error(), "several (a, b)") || !strings.Contains(err.Error(), "default_pool") {
		t.Fatalf("ambiguity should name the candidates: %v", err)
	}
	g, _ = loadGlobalText(t, "default_pool = [\"b\", \"a\"]\n"+two)
	c, err = mergeProject(t, g, "[runners.aws-big]\nextends_type = \"aws-spot\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Runner("aws-big"); r == nil || r.Region != "eu-west-1" {
		t.Fatalf("default_pool order should break the tie: %+v", r)
	}

	// No match at all: a clear reason, only when selected.
	g, _ = loadGlobalText(t, "[runners.homelab]\ntype = \"ssh\"\nhost = \"box\"\n")
	c, err = mergeProject(t, g, fleet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Runner("aws-big"); err == nil || !strings.Contains(err.Error(), `your runner "aws" or any aws-spot runner`) || !strings.Contains(err.Error(), "homelab (ssh)") {
		t.Fatalf("reason: %v", err)
	}
	if names := c.RunnerNames(); len(names) != 1 || names[0] != "homelab" {
		t.Fatalf("an unavailable runner isn't a runner: %v", names)
	}
	if u := c.UnavailableNames(); len(u) != 1 || u[0] != "aws-big" {
		t.Fatalf("unavailable: %v", u)
	}

	// A named runner of the wrong type, and no other: say so.
	g2, _ := loadGlobalText(t, "[runners.aws]\ntype = \"ssh\"\nhost = \"box\"\n")
	c, err = mergeProject(t, g2, fleet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Runner("aws-big"); err == nil || !strings.Contains(err.Error(), `your runner "aws" is ssh, and you have no aws-spot runner`) {
		t.Fatalf("wrong-type reason: %v", err)
	}

	// An unsupported type is the repository's mistake: a config error.
	if _, err := mergeProject(t, g, "[runners.x]\nextends_type = \"aws\"\n"); err == nil || !strings.Contains(err.Error(), "extends_type must be") {
		t.Fatalf("bad extends_type: %v", err)
	}
}

func TestUnavailableProjectRunnerStillValidated(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.homelab]\ntype = \"ssh\"\nhost = \"box\"\n")
	if err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		"[runners.big]\nextends = \"aws\"\nmax_slotz = 1\n":                    "unknown keys",
		"[runners.big]\nextends = \"aws\"\nprofile = \"x\"\n":                  "can only be set in the global config",
		"[runners.big]\nextends = \"aws\"\nmax_slots = \"x\"\n":                "runner \"big\"",
		"pool = [\"nope\"]\n[runners.big]\nextends = \"aws\"\n":                "pool references unknown runner \"nope\"",
		"[runners.big]\nextends = \"aws\"\nmax_slots = -1\n":                   "max_slots must be between",
		"[runners.big]\nextends_type = \"aws-spot\"\nidle_timeout = \"-1m\"\n": "idle_timeout must not be negative",
	} {
		if _, err := mergeProject(t, g, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}

	// A project pool naming the fleet loads, and fails only where it's used.
	c, err := mergeProject(t, g, "pool = [\"big\"]\n[runners.big]\nextends = \"aws\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pool(nil); err == nil || !strings.Contains(err.Error(), `extends "aws"`) {
		t.Fatalf("project pool: %v", err)
	}
	if rs, err := c.Pool([]string{"homelab"}); err != nil || len(rs) != 1 {
		t.Fatalf("another runner can still be chosen: %v", err)
	}
}

func TestUnavailableProjectRunnerMasksSameName(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.spot]\ntype = \"ssh\"\nhost = \"small\"\n[runners.homelab]\ntype = \"ssh\"\nhost = \"box\"\n")
	if err != nil {
		t.Fatal(err)
	}
	c, err := mergeProject(t, g, "[runners.spot]\nextends = \"aws\"\nmax_slots = 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Runner("spot"); err == nil || !strings.Contains(err.Error(), `extends "aws"`) {
		t.Fatalf("the user's own spot is not the project's spot: %v", err)
	}
	if names := c.RunnerNames(); len(names) != 1 || names[0] != "homelab" {
		t.Fatalf("the masked name must not be enumerated: %v", names)
	}
	if rs, err := c.Pool(nil); err != nil || len(rs) != 1 || rs[0].Name != "homelab" {
		t.Fatalf("the implicit pool must skip the masked name: %v %v", rs, err)
	}
	if c.Global.Runners["spot"] == nil {
		t.Fatal("the user's own runner must stay known for account-wide commands")
	}
	// A default_pool naming the masked runner fails rather than running on it.
	g, _ = loadGlobalText(t, "default_pool = [\"spot\"]\n[runners.spot]\ntype = \"ssh\"\nhost = \"small\"\n")
	c, err = mergeProject(t, g, "[runners.spot]\nextends = \"aws\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pool(nil); err == nil || !strings.Contains(err.Error(), `extends "aws"`) {
		t.Fatalf("default_pool naming the masked runner: %v", err)
	}
	// Without extends, the same name is the base and the override applies.
	g, _ = loadGlobalText(t, "[runners.spot]\ntype = \"ssh\"\nhost = \"small\"\n")
	c, err = mergeProject(t, g, "[runners.spot]\nmax_slots = 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := c.Runner("spot"); err != nil || r.MaxSlots != 3 {
		t.Fatalf("override: %+v %v", r, err)
	}
	// Overriding a runner the user doesn't have is unavailable, not fatal.
	c, err = mergeProject(t, g, "[runners.studio]\nmax_slots = 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Runner("studio"); err == nil || !strings.Contains(err.Error(), "extends or extends_type") {
		t.Fatalf("override of a missing runner: %v", err)
	}
}

func TestGlobalRejectsExtendsType(t *testing.T) {
	if _, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nextends_type = \"sim\"\n"); err == nil || !strings.Contains(err.Error(), "extends_type is for runners in a project") {
		t.Fatalf("got %v", err)
	}
}

func TestSetup(t *testing.T) {
	load := func(body string) (*Project, error) {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		if err := os.WriteFile(pp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadProject(pp)
	}
	p, err := load(`
[setup]
run = "npm run prepare --workspaces --if-present"
timeout = "3m"
[tasks.unit]
run = "vitest"
[tasks.status]
run = "git status"
setup = false
[tasks.typecheck]
run = "tsc"
setup = true
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Setup.Run != "npm run prepare --workspaces --if-present" || p.Setup.Timeout.Duration != 3*time.Minute {
		t.Errorf("setup %+v", p.Setup)
	}
	for name, skips := range map[string]bool{"unit": false, "status": true, "typecheck": false} {
		if p.Tasks[name].SkipsSetup() != skips {
			t.Errorf("%s: SkipsSetup = %v", name, !skips)
		}
	}
	if p, err := load("[setup]\nrun = \"x\"\n"); err != nil || p.Setup.Run != "x" || p.Setup.Timeout.Duration != 0 {
		t.Errorf("setup without a timeout: %+v, %v", p.Setup, err)
	}
	if p, err := load("[tasks.a]\nrun = \"x\"\n"); err != nil || p.Setup.Run != "" {
		t.Errorf("no setup: %+v, %v", p.Setup, err)
	}
	for body, want := range map[string]string{
		"[setup]\n":               "setup.run must be set",
		"[setup]\nrun = \"  \"\n": "setup.run must be set",
		"[setup]\nrun = \"x\"\ntimeout = \"0s\"\n":  "setup.timeout must be positive",
		"[setup]\nrun = \"x\"\ntimeout = \"-1m\"\n": "setup.timeout must be positive",
		"[setup]\nrun = \"x\"\nonce = true\n":       "unknown keys",
		"[tasks.a]\nrun = \"x\"\nsetup = \"no\"\n":  "setup",
	} {
		if _, err := load(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", body, err, want)
		}
	}
}

func TestContainerDockerNeedsContainer(t *testing.T) {
	for body, ok := range map[string]bool{
		"[container]\nimage = \"node:22\"\ndocker = true\n": true,
		"[container]\nimage = \"node:22\"\n":                true,
		"[container]\ndocker = true\n":                      false,
	} {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		os.WriteFile(pp, []byte(body), 0o644)
		p, err := LoadProject(pp)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", body, err, ok)
		}
		if err == nil && p.Container.Docker != strings.Contains(body, "docker = true") {
			t.Errorf("%q: Docker = %v", body, p.Container.Docker)
		}
	}
}

func TestWritebackLimits(t *testing.T) {
	load := func(body string) (*Project, error) {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		if err := os.WriteFile(pp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadProject(pp)
	}
	p, err := LoadProject("")
	if err != nil || p.Sync.WritebackMaxFiles != 1000 || p.Sync.WritebackMaxBytes != 100<<20 || p.Sync.WritebackOver != "warn" {
		t.Fatalf("defaults: %+v, %v", p.Sync, err)
	}
	for body, want := range map[string]Sync{
		"[sync]\nwriteback_max_files = 0\nwriteback_max_bytes = 0\n":           {WritebackMaxFiles: 0, WritebackMaxBytes: 0, WritebackOver: "warn"},
		"[sync]\nwriteback_max_bytes = \"250MB\"\nwriteback_over = \"hold\"\n": {WritebackMaxFiles: DefaultWritebackMaxFiles, WritebackMaxBytes: 250 << 20, WritebackOver: "hold"},
		"[sync]\nwriteback_max_files = 50\nwriteback_max_bytes = 4096\n":       {WritebackMaxFiles: 50, WritebackMaxBytes: 4096, WritebackOver: "warn"},
		"[sync]\nwriteback_max_bytes = \"1.5 GiB\"\n":                          {WritebackMaxFiles: DefaultWritebackMaxFiles, WritebackMaxBytes: 3 << 29, WritebackOver: "warn"},
	} {
		p, err := load(body)
		if err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		got := Sync{WritebackMaxFiles: p.Sync.WritebackMaxFiles, WritebackMaxBytes: p.Sync.WritebackMaxBytes, WritebackOver: p.Sync.WritebackOver}
		if got.WritebackMaxFiles != want.WritebackMaxFiles || got.WritebackMaxBytes != want.WritebackMaxBytes || got.WritebackOver != want.WritebackOver {
			t.Errorf("%q: got %+v, want %+v", body, got, want)
		}
	}
	for _, body := range []string{
		"[sync]\nwriteback_over = \"drop\"\n",
		"[sync]\nwriteback_max_files = -1\n",
		"[sync]\nwriteback_max_bytes = \"lots\"\n",
		"[sync]\nwriteback_max_bytes = -5\n",
	} {
		if _, err := load(body); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "512": 512, "512B": 512, "4KB": 4096, "4k": 4096, "100MB": 100 << 20, "100 MiB": 100 << 20, "2GB": 2 << 30, "0.5MB": 512 << 10} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1", "1TB", "NaN", "1e300GB"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) accepted", in)
		}
	}
}
