package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
)

// What a launch looks for is what the run will use: the dependency key the
// run computes once the VM reports its platform, and the image tag.
func TestPlacementProjectKeys(t *testing.T) {
	root := t.TempDir()
	for f, body := range map[string]string{"package-lock.json": "{}\n", "Dockerfile": "FROM scratch\n", "app.js": "1\n"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := &Workspace{Root: root, RepoID: "repo1"}
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot, Regions: []string{"all"}, Region: "us-east-1", Arch: "arm64", User: "ubuntu", BootstrapScript: "true"}
	p := &config.Project{Deps: config.Deps{Install: "npm ci", Lockfiles: []string{"package-lock.json"}, CacheDirs: []string{"node_modules"}}}
	build := func() *manifest.Manifest {
		m, err := manifest.Build(context.Background(), ManifestOptions(root, p))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := build()
	spec := depskey.Spec{Lockfiles: []string{"package-lock.json"}, Install: "npm ci", CacheDirs: []string{"node_modules"}, OS: "linux", Arch: "arm64"}

	// On the host: the key is known now.
	pj := PlacementProject(p, ws, m, false, r)
	hostSpec := spec
	hostSpec.Toolchain = toolchainFor(r, nil)
	want, err := depskey.Key(root, hostSpec)
	if err != nil {
		t.Fatal(err)
	}
	if pj == nil || pj.RepoID != "repo1" || !pj.Deps || pj.DepsKey != want || pj.Image || pj.DepsKeyFor != nil {
		t.Fatalf("host project %+v, want key %s", pj, want)
	}
	if pj := PlacementProject(p, ws, m, true, r); pj.Deps || pj.DepsKey != "" {
		t.Fatalf("--no-deps: %+v", pj)
	}

	// In an image built from the Dockerfile: the tag now, the key per image.
	p.Container = config.Container{Dockerfile: "Dockerfile"}
	m = build()
	pj = PlacementProject(p, ws, m, false, r)
	tag, err := imageTag(m, "Dockerfile", "", "linux/arm64", readFrom(root))
	if err != nil {
		t.Fatal(err)
	}
	if !pj.Image || pj.ImageTag != tag || pj.DepsKey != "" || pj.DepsKeyFor == nil {
		t.Fatalf("container project %+v, want tag %s", pj, tag)
	}
	ctrSpec := spec
	ctrSpec.Toolchain, ctrSpec.Image = "container", "sha256:abc"
	want, _ = depskey.Key(root, ctrSpec)
	if got, err := pj.DepsKeyFor("sha256:abc"); err != nil || got != want {
		t.Fatalf("key for image: %s %v, want %s", got, err, want)
	}
	ctrSpec.Image = "sha256:def"
	want2, _ := depskey.Key(root, ctrSpec)
	if got, _ := pj.DepsKeyFor("sha256:def"); got != want2 || want2 == want {
		t.Fatalf("key for another image: %s, want %s", got, want2)
	}
	// Another arch: another tag, as the run computes it on that VM.
	r.Arch = "amd64"
	tag64, _ := imageTag(m, "Dockerfile", "", "linux/amd64", readFrom(root))
	if pj := PlacementProject(p, ws, m, false, r); pj.ImageTag != tag64 || tag64 == tag {
		t.Fatalf("amd64 tag %s, want %s", pj.ImageTag, tag64)
	}
	r.Arch = "arm64"
	// Without the manifest the keys are unknown, but still needed.
	if pj := PlacementProject(p, ws, nil, false, r); !pj.Image || pj.ImageTag != "" || !pj.Deps || pj.DepsKeyFor != nil {
		t.Fatalf("no manifest: %+v", pj)
	}

	// A pulled image: nothing to build, and the key isn't known in advance.
	p.Container = config.Container{Image: "node:22"}
	if pj := PlacementProject(p, ws, build(), false, r); pj.Image || !pj.Deps || pj.DepsKey != "" || pj.DepsKeyFor != nil {
		t.Fatalf("pulled image: %+v", pj)
	}

	// Only launches across regions weigh it.
	r.Regions = nil
	if pj := PlacementProject(p, ws, m, false, r); pj != nil {
		t.Fatalf("one region: %+v", pj)
	}
}

// ProjectKeys computes the same keys for a runner in one region, and says
// why a key can't be computed.
func TestProjectKeysOneRegion(t *testing.T) {
	root := t.TempDir()
	for f, body := range map[string]string{"package-lock.json": "{}\n", "Dockerfile": "FROM scratch\n"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := &Workspace{Root: root, RepoID: "repo1"}
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot, Region: "us-east-1", Arch: "amd64", User: "ubuntu", BootstrapScript: "true"}
	p := &config.Project{Deps: config.Deps{Install: "npm ci", Lockfiles: []string{"package-lock.json"}}, Container: config.Container{Dockerfile: "Dockerfile"}}
	m, err := manifest.Build(context.Background(), ManifestOptions(root, p))
	if err != nil {
		t.Fatal(err)
	}
	pj, err := ProjectKeys(p, ws, m, false, r)
	tag, _ := imageTag(m, "Dockerfile", "", "linux/amd64", readFrom(root))
	if err != nil || pj == nil || !pj.Image || pj.ImageTag != tag || tag == "" || !pj.Deps || pj.DepsKeyFor == nil {
		t.Fatalf("one region: %+v, %v", pj, err)
	}
	// A Dockerfile that isn't in the checkout: no tag, and the reason.
	p.Container.Dockerfile = "missing/Dockerfile"
	if pj, err := ProjectKeys(p, ws, m, false, r); err == nil || pj.ImageTag != "" || !pj.Image ||
		!strings.Contains(err.Error(), "image tag") || !strings.Contains(err.Error(), "missing/Dockerfile") {
		t.Fatalf("missing Dockerfile: %+v, %v", pj, err)
	}
}

// A run's image build and dependency install are kept for the leased
// runner's next launch, under the run's repository (#195).
func TestRecordColdWorkKeepsBuildTimes(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot, Regions: []string{"all"}, Region: "us-east-1"}
	lease, ws := &Lease{Runner: r}, &Workspace{RepoID: "repo1"}
	// A pull, a restore or a hydration built nothing; a missing lease or a
	// zero time measured nothing.
	recordImageBuild(lease, ws, protocol.ImageResult{Pulled: true, MS: 30000})
	recordDepsInstall(lease, ws, protocol.DepsResult{Restored: true, Hydrated: true, MS: 40000})
	recordDepsInstall(nil, ws, protocol.DepsResult{Built: true, MS: 1000})
	recordDepsInstall(lease, ws, protocol.DepsResult{Built: true})
	path := filepath.Join(provider.RunnerStateDir("aws"), "placement.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nothing measured, yet state: %v", err)
	}
	recordImageBuild(lease, ws, protocol.ImageResult{Built: true, Pushing: true, MS: 720000})
	recordDepsInstall(lease, ws, protocol.DepsResult{Built: true, Uploaded: true, MS: 900000})
	var st struct {
		ColdWork map[string]struct {
			Image, Deps []struct{ MS int64 }
		} `json:"cold_work"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if w := st.ColdWork["repo1"]; len(w.Image) != 1 || w.Image[0].MS != 720000 || len(w.Deps) != 1 || w.Deps[0].MS != 900000 {
		t.Fatalf("state %s", b)
	}
}

// A workload is the same in every worktree of a project, and differs by
// what would change how long it runs: the command, its environment (a
// shard), the directory and having the VM to itself.
func TestSpeedWorkload(t *testing.T) {
	o := Options{Command: "npm run typecheck", Env: map[string]string{"SHARD": "1", "TOTAL": "4"}}
	base := SpeedWorkload("app-1234", ".", o)
	if len(base) != 16 || SpeedWorkload("app-1234", ".", Options{Command: o.Command, Env: map[string]string{"TOTAL": "4", "SHARD": "1"}}) != base {
		t.Fatalf("not stable: %q", base)
	}
	seen := map[string]string{base: "base"}
	for name, w := range map[string]string{
		"repo":      SpeedWorkload("other-1234", ".", o),
		"cwd":       SpeedWorkload("app-1234", "web", o),
		"command":   SpeedWorkload("app-1234", ".", Options{Command: "npm test", Env: o.Env}),
		"env":       SpeedWorkload("app-1234", ".", Options{Command: o.Command, Env: map[string]string{"SHARD": "2", "TOTAL": "4"}}),
		"exclusive": SpeedWorkload("app-1234", ".", Options{Command: o.Command, Env: o.Env, Exclusive: true}),
	} {
		if prev, ok := seen[w]; ok {
			t.Errorf("%s: same workload as %s", name, prev)
		}
		seen[w] = name
	}
}

// Only a clean exit 0 on a fleet VM of a known type is kept, as the
// command's own time without [setup].
func TestRecordRunSpeed(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "fleet", Type: config.TypeAWSSpot, VCPUs: 8}
	lease := &Lease{Runner: r, InstanceID: "i-1", Instance: provider.Instance{Type: "m7a.2xlarge"}}
	ws := &Workspace{RepoID: "app-1234", Cwd: "."}
	o := Options{Command: "npm run typecheck"}
	ok := protocol.Result{DurationMS: 75000, SetupMS: 5000}
	for name, res := range map[string]protocol.Result{
		"failed":       {ExitCode: 1, DurationMS: 1000},
		"timed out":    {TimedOut: true, DurationMS: 1000},
		"canceled":     {Canceled: true, DurationMS: 1000},
		"shutdown":     {Shutdown: true, DurationMS: 1000},
		"setup failed": {SetupFailed: true, DurationMS: 1000},
		"oom":          {OOMKilled: true, DurationMS: 1000},
		"not started":  {StartError: "no shell", DurationMS: 1000},
		"no time":      {DurationMS: 5000, SetupMS: 5000},
	} {
		recordRunSpeed(lease, ws, o, res, false)
		if s := speedState(t, r); s != nil {
			t.Fatalf("%s: recorded %v", name, s)
		}
	}
	recordRunSpeed(lease, ws, o, ok, true) // interrupted
	recordRunSpeed(&Lease{Runner: r, Instance: provider.Instance{Type: "m7a.2xlarge"}}, ws, o, ok, false)
	recordRunSpeed(&Lease{Runner: r, InstanceID: "i-1"}, ws, o, ok, false)
	if s := speedState(t, r); s != nil {
		t.Fatalf("recorded %v", s)
	}
	recordRunSpeed(lease, ws, o, ok, false)
	s := speedState(t, r)
	w := SpeedWorkload(ws.RepoID, ws.Cwd, o)
	if len(s) != 1 || len(s[w]["m7a.2xlarge"]) != 1 || s[w]["m7a.2xlarge"][0].MS != 70000 {
		t.Fatalf("speed %v", s)
	}
}

type speedRun struct {
	MS int64 `json:"ms"`
}

func speedState(t *testing.T, r *config.Runner) map[string]map[string][]speedRun {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(provider.RunnerStateDir(r.Name), "placement.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Speed map[string]map[string][]speedRun `json:"speed"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st.Speed
}
