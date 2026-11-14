package config

import (
	"os"
	"path/filepath"
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
