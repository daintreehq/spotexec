package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
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

// recordImageBuild keeps how long the run's image build took on the leased
// runner, for the runner's next launch to price a region without the image
// by (#195). A pull or a reuse built nothing, and isn't kept.
func recordImageBuild(lease *Lease, ws *Workspace, r protocol.ImageResult) {
	if r.Built {
		recordColdWork(lease, ws, provider.ColdImage, r.MS)
	}
}

// recordDepsInstall is recordImageBuild for the dependency install: a
// restore or a hydration from the runner's cache installed nothing.
func recordDepsInstall(lease *Lease, ws *Workspace, dr protocol.DepsResult) {
	if dr.Built {
		recordColdWork(lease, ws, provider.ColdDeps, dr.MS)
	}
}

// recordColdWork records one measurement. Best effort: a failure to save it
// never fails the run.
func recordColdWork(lease *Lease, ws *Workspace, part provider.ColdWorkPart, ms int64) {
	if lease == nil || ws == nil || ms <= 0 {
		return
	}
	_ = provider.RecordColdWork(lease.Runner, ws.RepoID, part, time.Duration(ms)*time.Millisecond)
}

// speedSample is one attempt's command time on a fleet VM, for
// recordRunSpeed.
type speedSample struct {
	runner   *config.Runner
	workload string
	itype    string
	vm       string
	took     time.Duration
}

// runSpeed is what a run's command time says of the leased VM's instance
// type, for the runner's next launches to weigh its speed by (#279), or
// nil. Only a command that ran to a clean exit 0 counts (one that finished
// after a Spot notice too): a failure, timeout or interrupt may have
// stopped it early. The time is the helper's own of the command's process,
// so queueing, [setup], preparing the container and the network don't
// count. env is what the command ran with.
func runSpeed(lease *Lease, ws *Workspace, o Options, r protocol.Result, env map[string]string, canceled bool) *speedSample {
	if lease == nil || ws == nil || lease.InstanceID == "" || lease.Instance.Type == "" || canceled || !lease.Runner.ByRequirements() {
		return nil
	}
	if r.ExitCode != 0 || r.TimedOut || r.Canceled || r.Shutdown || r.Withdrawn || r.SetupFailed || r.OOMKilled || r.StartError != "" || r.CommandMS <= 0 {
		return nil
	}
	return &speedSample{runner: lease.Runner, workload: SpeedWorkload(ws.RepoID, ws.Cwd, o, env), itype: lease.Instance.Type, vm: lease.InstanceID, took: time.Duration(r.CommandMS) * time.Millisecond}
}

// recordRunSpeed keeps s, if any. Best effort: a failure to save it never
// fails the run.
func recordRunSpeed(s *speedSample) {
	if s != nil {
		_ = provider.RecordRunSpeed(s.runner, s.workload, s.itype, s.vm, s.took)
	}
}

// speedEnvSkip is what of a command's environment SpeedWorkload leaves out:
// what differs from one run or slot to the next.
var speedEnvSkip = map[string]bool{"SPOTEXEC_SLOT": true, "SPOTEXEC_PORT_BASE": true, "SPOTEXEC_RUN_ID": true,
	"SPOTEXEC_ATTEMPT": true, "SPOTEXEC_WORKSPACE": true, "COMPOSE_PROJECT_NAME": true, "FORCE_COLOR": true, "CLICOLOR_FORCE": true}

// SpeedWorkload names what a run runs, for comparing its speed on one
// instance type with another (provider.RecordRunSpeed): the project (the
// same in every worktree of it), the directory it runs from, the command
// line, the environment it runs with (a shard's number, forwarded
// variables, its CPU share) but for what differs from run to run or slot
// to slot, and whether it had the VM to itself. Hashed: the runner's state
// keeps no command lines.
func SpeedWorkload(repoID, cwd string, o Options, env map[string]string) string {
	keep := make(map[string]string, len(env))
	for k, v := range env {
		if !speedEnvSkip[k] {
			keep[k] = v
		}
	}
	b, _ := json.Marshal(struct {
		V         int               `json:"v"`
		Repo      string            `json:"repo"`
		Cwd       string            `json:"cwd"`
		Command   string            `json:"command"`
		Env       map[string]string `json:"env,omitempty"` // encoding/json sorts the keys
		Exclusive bool              `json:"exclusive,omitempty"`
	}{1, repoID, cwd, o.Command, keep, o.Exclusive})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}
