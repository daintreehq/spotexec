package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/manifest"
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
	if pj, err := ProjectKeys(p, ws, m, false, r); err == nil || pj.ImageTag != "" || !pj.Image {
		t.Fatalf("missing Dockerfile: %+v, %v", pj, err)
	}
}
