package client

import (
	"fmt"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/provider"
)

// PlacementProject is what a launch of runner r for this run looks for in
// each region (see provider.Project): the keys the run's dependency archive
// and image will have there, computed as the run computes them. Nil when
// placement doesn't weigh it (r doesn't launch across regions); callers
// pass none for a run that syncs nothing. A key that can't be computed
// (m is nil) is left for VM history to judge.
func PlacementProject(p *config.Project, ws *Workspace, m *manifest.Manifest, noDeps bool, r *config.Runner) *provider.Project {
	if p == nil || ws == nil || r.Type != config.TypeAWSSpot || !r.MultiRegion() {
		return nil
	}
	pj, _ := ProjectKeys(p, ws, m, noDeps, r)
	return pj
}

// ProjectKeys is what a run of this checkout on aws-spot runner r keeps in
// a region (see provider.Project): the content tag of the image it builds
// from its Dockerfile and the key of its dependency archive, computed as
// the run computes them, in one region or several. A key that can't be
// computed is left unset, and the first such failure is returned with
// what could be (m nil fails none: the keys are just unknown).
func ProjectKeys(p *config.Project, ws *Workspace, m *manifest.Manifest, noDeps bool, r *config.Runner) (*provider.Project, error) {
	// AWS VMs are Linux; the runner's arch is the Go name the helper reports.
	goos, arch := "linux", r.Arch
	pj := &provider.Project{RepoID: ws.RepoID}
	var first error
	c := p.Container
	if c.Enabled() && c.Image == "" {
		pj.Image = true
		if m == nil {
			// unknown without the manifest
		} else if tag, err := imageTag(m, c.Dockerfile, c.Context, goos+"/"+arch, readFrom(ws.Root)); err == nil {
			pj.ImageTag = tag
		} else {
			first = fmt.Errorf("image tag: %w", err)
		}
	}
	if p.Deps.Install == "" || noDeps || len(p.Deps.Lockfiles) == 0 {
		return pj, first
	}
	pj.Deps = true
	if m == nil {
		return pj, first
	}
	lockfiles, _, err := DepsLockfiles(m, p.Deps.Lockfiles)
	if err != nil {
		if first == nil {
			first = fmt.Errorf("dependency lockfiles: %w", err)
		}
		return pj, first
	}
	if len(lockfiles) == 0 {
		return pj, first
	}
	spec := depskey.Spec{Lockfiles: lockfiles, Install: p.Deps.Install, CacheDirs: p.Deps.CacheDirs, OS: goos, Arch: arch}
	switch {
	case !c.Enabled():
		spec.Toolchain = toolchainFor(r, nil)
		if key, err := depskey.Key(ws.Root, spec); err == nil {
			pj.DepsKey = key
		} else if first == nil {
			first = fmt.Errorf("dependency key: %w", err)
		}
	case pj.Image:
		// Keyed by the image's ID, which differs from region to region
		// (each builds its own).
		spec.Toolchain = "container"
		pj.DepsKeyFor = func(imageID string) (string, error) {
			s := spec
			s.Image = imageID
			return depskey.Key(ws.Root, s)
		}
	}
	return pj, first
}

// PlacementNeedsManifest reports whether PlacementProject needs the
// project's manifest: for a dependency key or an image tag.
func PlacementNeedsManifest(p *config.Project) bool {
	return p.Deps.Install != "" || p.Container.Enabled() && p.Container.Image == ""
}
