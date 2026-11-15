package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
