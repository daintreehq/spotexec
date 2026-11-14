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
