package client

import (
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/provider"
)

// PlacementProject is what a launch of runner r for this run looks for in
// each region (see provider.Project): the keys the run's dependency archive
// and image will have there, computed as the run computes them. Nil when
// placement doesn't weigh it (r doesn't launch across regions) or nothing
// is synced; a key that can't be computed is left for VM history to judge.
func PlacementProject(p *config.Project, ws *Workspace, m *manifest.Manifest, noDeps bool, r *config.Runner) *provider.Project {
	if p == nil || ws == nil || m == nil || r.Type != config.TypeAWSSpot || !r.MultiRegion() {
		return nil
	}
	// AWS VMs are Linux; the runner's arch is the Go name the helper reports.
	goos, arch := "linux", r.Arch
	pj := &provider.Project{RepoID: ws.RepoID}
	c := p.Container
	if c.Enabled() && c.Image == "" {
		pj.Image = true
		if tag, err := imageTag(m, c.Dockerfile, c.Context, goos+"/"+arch, readFrom(ws.Root)); err == nil {
			pj.ImageTag = tag
		}
	}
	if p.Deps.Install == "" || noDeps || len(p.Deps.Lockfiles) == 0 {
		return pj
	}
	pj.Deps = true
	lockfiles, _, err := DepsLockfiles(m, p.Deps.Lockfiles)
	if err != nil || len(lockfiles) == 0 {
		return pj
	}
	spec := depskey.Spec{Lockfiles: lockfiles, Install: p.Deps.Install, CacheDirs: p.Deps.CacheDirs, OS: goos, Arch: arch}
	switch {
	case !c.Enabled():
		spec.Toolchain = toolchainFor(r, nil)
		if key, err := depskey.Key(ws.Root, spec); err == nil {
			pj.DepsKey = key
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
	return pj
}
