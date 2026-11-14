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
