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
	g, err = loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"all\"]\nexclude_regions = [\"ap-south-2\", \"me-central-1\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.Runners["aws"]; !r.AnyRegion() || strings.Join(r.ExcludeRegions, ",") != "ap-south-2,me-central-1" {
		t.Fatalf("all but some: %+v", r)
	}
	for name, body := range map[string]string{
		"exclude, list":      "regions = [\"us-east-1\", \"us-east-2\"]\nexclude_regions = [\"us-east-2\"]\n",
		"exclude, region":    "region = \"us-east-1\"\nexclude_regions = [\"us-east-2\"]\n",
		"exclude invalid":    "regions = [\"all\"]\nexclude_regions = [\"mars\"]\n",
		"exclude all":        "regions = [\"all\"]\nexclude_regions = [\"all\"]\n",
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
	os.WriteFile(pp, []byte("version = 1\n[runners.aws2]\nextends = \"aws\"\nexclude_regions = []\n"), 0o644)
	if p, err = LoadProject(pp); err != nil {
		t.Fatal(err)
	}
	if err := (&Config{Global: g, Project: p}).MergeProjectRunners(); err == nil || !strings.Contains(err.Error(), `"exclude_regions" can only be set in the global config`) {
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

func TestExtendKeepsExcludedRegions(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregions = [\"all\"]\nexclude_regions = [\"ap-south-2\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	os.WriteFile(pp, []byte("version = 1\n[runners.big]\nextends = \"aws\"\nmax_slots = 1\n"), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Global: g, Project: p}
	if err := c.MergeProjectRunners(); err != nil {
		t.Fatal(err)
	}
	r := c.Global.Runners["big"]
	if !r.AnyRegion() || strings.Join(r.ExcludeRegions, ",") != "ap-south-2" {
		t.Fatalf("extended: %+v", r)
	}
	r.ExcludeRegions[0] = "changed"
	if g.Runners["aws"].ExcludeRegions[0] != "ap-south-2" {
		t.Fatal("extending must copy exclude_regions")
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
	if !r.ByRequirements() || len(r.InstanceTypes) != 0 || r.Arch != "amd64" || r.MinGeneration != DefaultMinGeneration || r.LaunchOrder != LaunchRunCost || r.WaitCostPerHour() != DefaultWaitCost {
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
		"negative wait_cost":        "wait_cost = -1\n",
		"nan wait_cost":             "wait_cost = nan\n",
		"infinite wait_cost":        "wait_cost = inf\n",
		"-infinite wait_cost":       "wait_cost = -inf\n",
		"huge wait_cost":            "wait_cost = 10001\n",
		"wordy wait_cost":           "wait_cost = \"a lot\"\n",
		"boolean wait_cost":         "wait_cost = true\n",
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

// wait_cost takes a number or a dollar string, and an explicit 0 is kept
// (launch costs then count only the Spot bill).
func TestWaitCost(t *testing.T) {
	if DefaultWaitCost != 1 {
		t.Fatalf("documented default is $1/h, not %g", DefaultWaitCost)
	}
	for body, want := range map[string]float64{
		"":                        1,
		"wait_cost = 10000\n":     10000,
		"wait_cost = 0\n":         0,
		"wait_cost = 3\n":         3,
		"wait_cost = 0.25\n":      0.25,
		"wait_cost = \"$12.5\"\n": 12.5,
		"wait_cost = \"2\"\n":     2,
	} {
		g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregion = \"us-east-1\"\n"+body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		r := g.Runners["aws"]
		if got := r.WaitCostPerHour(); got != want {
			t.Errorf("%q: wait cost %g, want %g", body, got, want)
		}
		// A fixed list defaults to run_cost too (it ranks by price in one
		// region anyway).
		if r.LaunchOrder != LaunchRunCost {
			t.Errorf("%q: launch order %q", body, r.LaunchOrder)
		}
		// It encodes back to what was set ("" when unset).
		b, _ := r.WaitCost.MarshalText()
		var back Dollars
		if r.WaitCost.Set {
			if err := back.UnmarshalText(b); err != nil || back != r.WaitCost {
				t.Errorf("%q: round trip %q -> %+v (%v)", body, b, back, err)
			}
		} else if len(b) != 0 {
			t.Errorf("unset encodes as %q", b)
		}
	}
	// A project runner inherits it, and may override it.
	g, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nregion = \"us-east-1\"\nwait_cost = 5\nlaunch_order = \"cheapest\"\n")
	if err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(t.TempDir(), ".spotexec.toml")
	os.WriteFile(pp, []byte("version = 1\n[runners.same]\nextends = \"aws\"\nvcpus = 8\n[runners.batch]\nextends = \"aws\"\nwait_cost = 0\n"), 0o644)
	p, err := LoadProject(pp)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Global: g, Project: p}
	if err := c.MergeProjectRunners(); err != nil {
		t.Fatal(err)
	}
	// Switching to vcpus keeps an inherited cheapest (only "listed" needs the
	// list it drops).
	if r := c.Global.Runners["same"]; r.WaitCostPerHour() != 5 || r.LaunchOrder != LaunchCheapest {
		t.Fatalf("same: %+v", r)
	}
	if r := c.Global.Runners["batch"]; r.WaitCostPerHour() != 0 || !r.WaitCost.Set {
		t.Fatalf("batch: %+v", r)
	}
	if g.Runners["aws"].WaitCostPerHour() != 5 {
		t.Fatal("base runner must not be modified")
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
	if r := c.Global.Runners["big"]; !r.ByRequirements() || len(r.InstanceTypes) != 0 || r.MinMemoryGiB != 32 || r.LaunchOrder != LaunchRunCost {
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
	for in, want := range map[string]int64{"0": 0, "0.0": 0, "0KB": 0, "512": 512, "512B": 512, "4KB": 4096, "4k": 4096, "100MB": 100 << 20, "100 MiB": 100 << 20, "2GB": 2 << 30, "0.5MB": 512 << 10, "1.5": 1, "1": 1, "1B": 1, "0.0009765625KB": 1, "-0": 0, "0e5": 0} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1", "1TB", "NaN", "1e300GB"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) accepted", in)
		}
	}
	// Below one byte would truncate to 0, which turns a limit off: only an
	// explicit zero may do that.
	// (1e-400 underflows to 0 when parsed, but isn't zero.)
	for _, in := range []string{"0.4", "+0.4", "0.4B", "0.0001KB", "1e-10GB", "0.999", "1e-400", "1e-400GB", "-1e-400", "0x1p-2000"} {
		if _, err := ParseBytes(in); err == nil || !strings.Contains(err.Error(), "below one byte") {
			t.Errorf("ParseBytes(%q) = %v, want a below-one-byte error", in, err)
		}
	}
	// In the config, that is an error on load, quoted or not.
	for v, want := range map[string]int64{`"0.4"`: -1, `0.4`: -1, `0.0000001`: -1, `0.9999999`: -1, `0`: 0, `0.0`: 0, `2048`: 2048, `"2KB"`: 2048, `1024.0`: 1024} {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		if err := os.WriteFile(pp, []byte("version = 1\n[sync]\nwriteback_max_bytes = "+v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := LoadProject(pp)
		switch {
		case want < 0:
			if err == nil || !strings.Contains(err.Error(), "below one byte") {
				t.Errorf("writeback_max_bytes = %s loaded: %v", v, err)
			}
		case err != nil || int64(p.Sync.WritebackMaxBytes) != want:
			t.Errorf("writeback_max_bytes = %s: %v, %v; want %d", v, p, err, want)
		}
	}
}

func TestConfigVersion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	const upgrade = "; upgrade spotexec to use this config"
	for _, tc := range []struct {
		name, body string
		want       string // the error after the path; empty: accepted
	}{
		{"current", "version = 1\n", ""},
		{"absent", "", ""},
		{"newer", "version = 2\n", "version = 2 isn't supported (supported: 1)" + upgrade},
		// A newer format's keys are unknown here: the version says why.
		{"newer with new keys", "version = 3\nnew_key = true\n", "version = 3 isn't supported (supported: 1)" + upgrade},
		// The decoder reads Version as version, so the check must too.
		{"mixed case", "Version = 2\n", "version = 2 isn't supported (supported: 1)" + upgrade},
		{"zero", "version = 0\n", "version = 0 isn't supported (supported: 1)"},
		{"negative", "version = -1\n", "version = -1 isn't supported (supported: 1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gp := filepath.Join(dir, "global.toml")
			if err := os.WriteFile(gp, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SPOTEXEC_CONFIG", gp)
			_, gerr := LoadGlobal()
			pp := filepath.Join(dir, ".spotexec.toml")
			if err := os.WriteFile(pp, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, perr := LoadProject(pp)
			cerr := CheckVersion(pp)
			for which, c := range map[string]struct {
				path string
				err  error
			}{"global": {gp, gerr}, "project": {pp, perr}, "CheckVersion": {pp, cerr}} {
				if tc.want == "" {
					if c.err != nil {
						t.Errorf("%s: %v", which, c.err)
					}
					continue
				}
				if want := c.path + ": " + tc.want; c.err == nil || c.err.Error() != want {
					t.Errorf("%s: got %v, want %s", which, c.err, want)
				}
			}
		})
	}
}

// A repository may tune a fleet, never loosen what the user's own runner
// allows: its image, its price, or how long and how many VMs bill (#152).
func TestProjectRunnerCannotLoosenBase(t *testing.T) {
	g, err := loadGlobalText(t, `[runners.sim]
type = "sim"
max_instances = 2
[runners.aws]
type = "aws-spot"
region = "us-east-1"
max_price = "0.40"
max_instances = 4
idle_timeout = "10m"
max_lifetime = "6h"
[runners.open]
type = "aws-spot"
region = "us-east-1"
`)
	if err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		"[runners.big]\nextends = \"aws\"\nami = \"ami-0123456789abcdef0\"\n": `"ami" can only be set in the global config`,
		"[runners.big]\nextends = \"aws\"\non_demand_fallback = true\n":       `"on_demand_fallback" can only be set in the global config`,
		"[runners.big]\nextends = \"aws\"\nAMI = \"ami-0123456789abcdef0\"\n": `"ami" can only be set in the global config`,
		"[runners.big]\nextends = \"aws\"\nOn_Demand_Fallback = true\n":       `"on_demand_fallback" can only be set in the global config`,
		"[runners.big]\nextends = \"aws\"\nPROFILE = \"x\"\n":                 `"profile" can only be set in the global config`,
		"[runners.big]\nextends = \"aws\"\nmax_price = \"\"\n":                `max_price can't be removed`,
		"[runners.big]\nextends = \"aws\"\nmax_price = \"0.41\"\n":            `max_price = "0.41" is more than your runner "aws" allows (0.40 an hour)`,
		"[runners.big]\nextends = \"aws\"\nmax_price = \"lots\"\n":            `max_price must be a positive number`,
		"[runners.big]\nextends = \"aws\"\nmax_instances = 5\n":               `max_instances = 5 is more than your runner "aws" allows (4)`,
		"[runners.big]\nextends = \"aws\"\nidle_timeout = \"11m\"\n":          `idle_timeout = 11m0s is longer than your runner "aws" allows (10m0s)`,
		"[runners.big]\nextends = \"aws\"\nmax_lifetime = \"7h\"\n":           `max_lifetime = 7h0m0s is longer than your runner "aws" allows (6h0m0s)`,
		"[runners.big]\nextends = \"open\"\nmax_lifetime = \"13h\"\n":         `max_lifetime = 13h0m0s is longer than your runner "open" allows (12h0m0s)`,
		"[runners.aws]\nmax_price = \"\"\n":                                   `max_price can't be removed`,
		"[runners.aws]\nmax_price = \"0.5\"\n":                                `is more than your runner "aws" allows`,
		"[runners.aws]\nidle_timeout = \"1h\"\n":                              `idle_timeout = 1h0m0s is longer`,
		"[runners.aws]\nmax_lifetime = \"24h\"\n":                             `max_lifetime = 24h0m0s is longer`,
		"[runners.sim]\nidle_timeout = \"11m\"\n":                             `idle_timeout = 11m0s is longer than your runner "sim" allows (10m0s)`,
		"[runners.big]\nextends_type = \"sim\"\nmax_lifetime = \"3h\"\n":      `max_lifetime = 3h0m0s is longer than your runner "sim" allows (2h0m0s)`,
		"[runners.big]\nextends = \"aws\"\nvolume_gb = 16385\n":               `volume_gb must be between 8 and 16384`,
		"[runners.big]\nextends = \"aws\"\nmax_price = \"NaN\"\n":             `max_price must be a positive number`,
		"[runners.big]\nextends = \"aws\"\nmax_price = \"0\"\n":               `max_price must be a positive number`,
		"[runners.big]\nextends = \"aws\"\nmax_price = \"-1\"\n":              `max_price must be a positive number`,
		"[runners.big]\nextends = \"open\"\nmax_price = \"Inf\"\n":            `max_price must be a positive number`,
		"[runners.big]\nextends = \"open\"\nmax_price = \"cheap\"\n":          `max_price must be a positive number`,
		"[runners.big]\nextends_type = \"sim\"\nmax_price = \"x\"\n":          `max_price must be a positive number`,
		"[runners.sim]\nmax_instances = 3\n":                                  `max_instances = 3 is more than your runner "sim" allows (2)`,
		"[runners.aws]\nmax_instances = 40\n":                                 `max_instances`,
	} {
		if _, err := mergeProject(t, g, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
	// Tightening, or matching the base, is fine.
	c, err := mergeProject(t, g, `[runners.big]
extends = "aws"
max_price = "0.25"
max_instances = 2
idle_timeout = "5m"
max_lifetime = "2h"
volume_gb = 200
[runners.same]
extends = "aws"
max_price = "0.4"
max_instances = 4
idle_timeout = "10m"
max_lifetime = "6h"
volume_gb = 16384
[runners.capped]
extends = "open"
max_price = "1.00"
`)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Global.Runners["big"]; r.MaxPrice != "0.25" || r.MaxInstances != 2 || r.IdleTimeout.Duration != 5*time.Minute || r.MaxLifetime.Duration != 2*time.Hour || r.VolumeGB != 200 {
		t.Fatalf("big: %+v", r)
	}
	if r := c.Global.Runners["capped"]; r.MaxPrice != "1.00" {
		t.Fatalf("capped: %+v", r)
	}
	if r := c.Global.Runners["same"]; r.MaxPrice != "0.4" || r.MaxInstances != 4 {
		t.Fatalf("same: %+v", r)
	}
	if r := c.Global.Runners["same"]; r.VolumeGB != 16384 || r.IdleTimeout.Duration != 10*time.Minute || r.MaxLifetime.Duration != 6*time.Hour {
		t.Fatalf("same: %+v", r)
	}
	if b := g.Runners["aws"]; b.MaxPrice != "0.40" || b.MaxInstances != 4 || b.MaxLifetime.Duration != 6*time.Hour {
		t.Fatalf("base runner must not be modified: %+v", b)
	}
	if b := g.Runners["open"]; b.MaxLifetime.Duration != 12*time.Hour {
		t.Fatalf("the open fixture relies on the 12h default lifetime: %s", b.MaxLifetime.Duration)
	}
	// A same-name override may tighten too, and an SSH base has no fleet
	// limits to keep.
	if _, err := mergeProject(t, g, "[runners.aws]\nmax_price = \"0.3\"\nidle_timeout = \"5m\"\nmax_lifetime = \"1h\"\nmax_instances = 1\n"); err != nil {
		t.Errorf("same-name tightening: %v", err)
	}
	gs, err := loadGlobalText(t, "[runners.box]\ntype = \"ssh\"\nhost = \"box\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mergeProject(t, gs, "[runners.box]\nmax_instances = 9\nidle_timeout = \"1h\"\n"); err != nil {
		t.Errorf("ssh override: %v", err)
	}
	if _, err := loadGlobalText(t, "[runners.aws]\ntype = \"aws-spot\"\nmax_price = \"$0.40\"\n"); err == nil || !strings.Contains(err.Error(), "max_price must be a positive number") {
		t.Errorf("global max_price: %v", err)
	}
}

// Wildcard instance_types narrow the base's types; they say nothing about
// the architecture, so the base's arch stays (#152).
func TestProjectTypePatternsKeepBaseArch(t *testing.T) {
	g, err := loadGlobalText(t, "[runners.arm]\ntype = \"aws-spot\"\nregion = \"us-east-1\"\narch = \"arm64\"\nvcpus = 8\n")
	if err != nil {
		t.Fatal(err)
	}
	c, err := mergeProject(t, g, `[runners.mem]
extends = "arm"
vcpus = 8
instance_types = ["m*", "r*"]
[runners.concrete]
extends = "arm"
instance_types = ["c7a.2xlarge"]
[runners.graviton]
extends = "arm"
vcpus = 8
instance_types = ["m*g*"]
[runners.mixed]
extends = "arm"
vcpus = 8
instance_types = ["m*", "c7a.2xlarge"]
[runners.mixed2]
extends = "arm"
vcpus = 8
instance_types = ["c7a.2xlarge", "m*"]
[runners.empty]
extends = "arm"
instance_types = []
[runners.untouched]
extends = "arm"
max_slots = 2
[runners.explicit]
extends = "arm"
vcpus = 8
instance_types = ["c*"]
arch = "amd64"
`)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"mem": "arm64", "graviton": "arm64", "mixed": "amd64", "mixed2": "amd64", "empty": "amd64", "concrete": "amd64", "explicit": "amd64", "untouched": "arm64"} {
		if got := c.Global.Runners[name].Arch; got != want {
			t.Errorf("%s: arch %q, want %q", name, got, want)
		}
	}
	// instance_types alone drop the inherited vcpus, so patterns need their own.
	if _, err := mergeProject(t, g, "[runners.x]\nextends = \"arm\"\ninstance_types = [\"m*\"]\n"); err == nil || !strings.Contains(err.Error(), "is a pattern, which needs vcpus") {
		t.Errorf("patterns without vcpus: %v", err)
	}
}

// Every duration in .spotexec.toml must be positive, and a task's pool must
// name runners, like the project's (#152).
func TestProjectDurationsAndTaskPools(t *testing.T) {
	load := func(body string) (*Project, error) {
		pp := filepath.Join(t.TempDir(), ".spotexec.toml")
		if err := os.WriteFile(pp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadProject(pp)
	}
	for body, want := range map[string]string{
		"timeout = \"0s\"\n":                                    "timeout must be positive (got 0s)",
		"timeout = \"-1m\"\n":                                   "timeout must be positive (got -1m0s)",
		"Timeout = \"0s\"\n":                                    "timeout must be positive (got 0s)",
		"grace_period = \"0s\"\n":                               "grace_period must be positive",
		"grace_period = \"-1s\"\n":                              "grace_period must be positive",
		"queue_timeout = \"0s\"\n":                              "queue_timeout must be positive",
		"queue_timeout = \"-5m\"\n":                             "queue_timeout must be positive",
		"[deps]\ntimeout = \"0s\"\n":                            "deps.timeout must be positive",
		"[deps]\ntimeout = \"-1m\"\n":                           "deps.timeout must be positive",
		"[container]\nimage = \"node:22\"\ntimeout = \"0s\"\n":  "container.timeout must be positive",
		"[container]\nimage = \"node:22\"\ntimeout = \"-1m\"\n": "container.timeout must be positive",
		"[tasks.a]\nrun = \"x\"\ntimeout = \"0s\"\n":            "tasks.a.timeout must be positive",
		"[tasks.a]\nrun = \"x\"\ntimeout = \"-1m\"\n":           "tasks.a.timeout must be positive",
		"[tasks.a]\nrun = \"x\"\nTIMEOUT = \"0s\"\n":            "tasks.a.timeout must be positive",
	} {
		if _, err := load(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
	// Unset durations get their defaults (a task's inherits the command's).
	for _, body := range []string{"", "[deps]\nhydrate = \"copy\"\n[container]\nimage = \"node:22\"\n[tasks.a]\nrun = \"x\"\n"} {
		p, err := load(body)
		if err != nil {
			t.Fatal(err)
		}
		if p.Timeout.Duration != 5*time.Minute || p.Grace.Duration != 5*time.Second || p.QueueTimeout.Duration != 10*time.Minute || p.Deps.Timeout.Duration != 20*time.Minute || p.Container.Timeout.Duration != 20*time.Minute {
			t.Errorf("%q: defaults %+v", body, p)
		}
		if a := p.Tasks["a"]; a != nil && a.Timeout.Duration != 0 {
			t.Errorf("unset task timeout: %s", a.Timeout.Duration)
		}
	}
	p, err := load("timeout = \"30m\"\ngrace_period = \"2s\"\nqueue_timeout = \"1h\"\n[deps]\ntimeout = \"3m\"\n[container]\nimage = \"node:22\"\ntimeout = \"4m\"\n[tasks.a]\nrun = \"x\"\ntimeout = \"7m\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.Timeout.Duration != 30*time.Minute || p.Grace.Duration != 2*time.Second || p.QueueTimeout.Duration != time.Hour || p.Deps.Timeout.Duration != 3*time.Minute || p.Container.Timeout.Duration != 4*time.Minute || p.Tasks["a"].Timeout.Duration != 7*time.Minute {
		t.Errorf("positive durations: %+v", p)
	}

	g, err := loadGlobalText(t, "[runners.homelab]\ntype = \"ssh\"\nhost = \"box\"\n")
	if err != nil {
		t.Fatal(err)
	}
	// Tasks are checked in name order, each pool in its own order.
	if _, err := mergeProject(t, g, "[tasks.b]\nrun = \"x\"\npool = [\"zz\"]\n[tasks.a]\nrun = \"x\"\npool = [\"homelab\", \"nope\", \"also\"]\n"); err == nil || !strings.Contains(err.Error(), `task "a": pool references unknown runner "nope"`) {
		t.Errorf("task pool: %v", err)
	}
	if _, err := mergeProject(t, g, "[tasks.a]\nrun = \"x\"\npool = [\"homelab\"]\n"); err != nil {
		t.Errorf("valid task pool: %v", err)
	}
	// A task pool naming a fleet this user can't back loads, like a project pool.
	if _, err := mergeProject(t, g, "[runners.big]\nextends = \"aws\"\n[tasks.a]\nrun = \"x\"\npool = [\"big\"]\n"); err != nil {
		t.Errorf("unavailable fleet in a task pool: %v", err)
	}
}
