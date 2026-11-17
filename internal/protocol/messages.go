package protocol

import (
	"fmt"
	"time"
)

// Message type names.
const (
	TypeHello       = "hello"
	TypeHelloOK     = "hello_ok"
	TypeAcquire     = "acquire"
	TypeWaiting     = "waiting"
	TypeAcquired    = "acquired"
	TypeBusy        = "busy"
	TypeManifest    = "manifest"
	TypeNeed        = "need"
	TypeSynced      = "synced"
	TypeDeps        = "deps"
	TypeDepsResult  = "deps_result"
	TypeExec        = "exec"
	TypeStarted     = "started"
	TypeResult      = "result"
	TypeCancel      = "cancel"
	TypeFetch       = "fetch"
	TypeFetched     = "fetched"
	TypeSettle      = "settle"
	TypePull        = "pull"
	TypeHeartbeat   = "hb"
	TypeEvent       = "event"
	TypeError       = "error"
	TypeBye         = "bye"
	TypeImage       = "image"
	TypeImageResult = "image_result"
	TypeGitSeed     = "git_seed"
	TypeGitNeed     = "git_need"
	TypeGitSeeded   = "git_seeded"
	TypeGitDrop     = "git_drop" // no body, no reply: remove a .git an earlier seed left in the slot
	TypeShareWait   = "share_wait"
	TypeShareDone   = "share_done"
	// TypeWithdraw (no body, no reply of its own): take the job back if it
	// still waits for a run permit, because the client found a free slot
	// on another VM. Honored only while the job waits (the exec's Result
	// then has Withdrawn set); ignored once its [setup] run or command has
	// a permit.
	TypeWithdraw = "withdraw"
)

// MaxSlots caps slots per machine, explicit (max_slots) or automatic
// (vCPUs / 4): each slot gets a 1000-port range at SPOTEXEC_PORT_BASE =
// 20000 + slot*1000, which must stay a valid port.
const MaxSlots = 44

// AdmitLimit is how many jobs a machine with slots slots admits at once:
// twice its slots (at most MaxSlots), since a job holds a slot's command
// capacity only while its [setup] run and command run. The rest of the
// time (syncing, the image, the dependency install, the write-back) it
// holds only its own token and workspace, so jobs preparing don't keep
// others out.
func AdmitLimit(slots int) int {
	return max(min(2*slots, MaxSlots), slots)
}

// Error codes carried by ErrorMsg.Code.
const (
	CodeDraining     = "RUNNER_DRAINING"
	CodeBooting      = "RUNNER_BOOTING"
	CodeBootFailed   = "BOOTSTRAP_FAILED"
	CodeQueueTimeout = "QUEUE_TIMEOUT"
	CodeBadRequest   = "BAD_REQUEST"
	CodeInternal     = "INTERNAL"
	CodeSyncFailed   = "SYNC_FAILED"
	CodeDepsFailed   = "DEPS_FAILED"
	CodeImageFailed  = "IMAGE_FAILED"
	// CodeImageBuildFailed: the project's own Dockerfile failed to build
	// (a RUN step failing, a syntax error, a COPY source missing), the
	// project's failure rather than spotexec's. Detail is the end of the
	// build's output.
	CodeImageBuildFailed = "IMAGE_BUILD_FAILED"
	CodeShutdown         = "RUNNER_SHUTTING_DOWN" // the VM began shutting down under the request
)

// BootTailMark ends a BOOTSTRAP_FAILED message's own words: the last lines
// of the bootstrap log follow it.
const BootTailMark = "last lines of /var/log/spotexec-bootstrap.log:"

// ErrorMsg is sent by the helper for any failed request.
type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Detail adds context for some codes: with RUNNER_BOOTING, the latest
	// line of the VM's bootstrap log.
	Detail string `json:"detail,omitempty"`
	// BuildFailed marks an IMAGE_FAILED from the image build itself exiting
	// non-zero (not a pull, login, push or setup failure, nor a timeout or
	// cancellation): building the same tag again would fail the same way,
	// so the client remembers it (#194).
	BuildFailed bool `json:"build_failed,omitempty"`
}

// Hello opens a session.
type Hello struct {
	Proto    int    `json:"proto"`
	Version  string `json:"version"`
	WorkRoot string `json:"work_root"`
}

// HelloOK describes the remote host.
type HelloOK struct {
	Proto    int    `json:"proto"`
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	WorkRoot string `json:"work_root"` // absolute, expanded
	Draining bool   `json:"draining"`
}

// ClientInfo identifies who holds a slot; shown by `status`.
type ClientInfo struct {
	Host     string `json:"host"`
	PID      int    `json:"pid"`
	Worktree string `json:"worktree"`
	Command  string `json:"command"`
	RunID    string `json:"run_id"`
	// Attempt: the client's reservation for this acquire (its pool
	// ledger), echoed in holders so the client can match them.
	Attempt string `json:"attempt,omitempty"`
}

// Acquire asks for a concurrency token and a workspace slot.
type Acquire struct {
	RepoID      string     `json:"repo_id"`
	WorktreeKey string     `json:"worktree_key"`
	Fingerprint string     `json:"fingerprint"`
	MaxSlots    int        `json:"max_slots"` // 0 = auto (cpus/4)
	Exclusive   bool       `json:"exclusive"`
	Wait        bool       `json:"wait"`
	QueueMS     int64      `json:"queue_ms"`
	Client      ClientInfo `json:"client"`
	PreferSlot  int        `json:"prefer_slot,omitempty"`
	OnlySlot    bool       `json:"only_slot,omitempty"`  // wait for PreferSlot rather than take another
	KeepOwner   bool       `json:"keep_owner,omitempty"` // don't claim the slot (exec/pull)
	SpotWatch   bool       `json:"spot_watch,omitempty"` // watch for spot interruption / rebalance notices
	// ReadOnly: nothing will run, only files are copied (pull), so a VM near
	// its max_lifetime may still serve it.
	ReadOnly bool `json:"read_only,omitempty"`
	// Overflow: admit this job past MaxSlots jobs (up to AdmitLimit) while
	// fewer than MaxSlots of them run commands: it prepares beside them and
	// waits for a free slot to run its command. Without it a runner admits
	// MaxSlots jobs, whatever they are doing. Clients ask for it once no
	// runner has a free slot, before queueing or launching a VM.
	Overflow bool `json:"overflow,omitempty"`
}

// LifetimeDrainAhead is how long before an ephemeral VM's max_lifetime it
// stops taking new runs, so the hard poweroff never cuts short a run that
// started after that point: 15 minutes, or a quarter of a shorter lifetime.
func LifetimeDrainAhead(maxLifetime time.Duration) time.Duration {
	return min(15*time.Minute, maxLifetime/4)
}

// Holder describes a held token, for waiting/busy/status displays.
type Holder struct {
	Token     int        `json:"token"`
	RepoID    string     `json:"repo_id"`
	Slot      int        `json:"slot"`
	Client    ClientInfo `json:"client"`
	StartedAt int64      `json:"started_at"` // unix ms
	HelperPID int        `json:"helper_pid"`
	ChildPGID int        `json:"child_pgid,omitempty"`
	// Phased: a helper that takes command capacity separately (a run
	// permit, Run) only while the job's [setup] run and command run. A
	// holder without it (an older helper, an exclusive run, or a token
	// whose record isn't written yet) takes command capacity for its
	// whole lease.
	Phased bool   `json:"phased,omitempty"`
	Phase  string `json:"phase,omitempty"` // a phased job's PhaseRunWait or PhaseRun; "" otherwise
	Run    int    `json:"run,omitempty"`   // the run permit a PhaseRun job holds, 1-based
}

// Phases of a phased holder (Holder.Phase): waiting for a run permit, and
// running its [setup] run and command with one.
const (
	PhaseRunWait = "run_wait"
	PhaseRun     = "run"
)

// Running reports whether h takes command capacity: it runs (or waits to
// run) its command, or it holds its slot's capacity for its whole lease.
func (h Holder) Running() bool { return !h.Phased || h.Phase != "" }

// Overflowable reports whether a busy runner with slots slots and these
// holders may admit an Overflow request: some of its jobs only prepare or
// write back, and fewer than slots run (or wait to run) commands. A run
// holding several tokens (exclusive) counts once.
func Overflowable(holders []Holder, slots int) bool {
	for _, h := range holders {
		if !h.Running() {
			return RunningJobs(holders) < slots
		}
	}
	return false
}

// RunningJobs counts the jobs among holders taking command capacity; a
// run holding several tokens (exclusive) counts once.
func RunningJobs(holders []Holder) int {
	runs := map[int]bool{}
	for i, h := range holders {
		if h.Running() {
			id := h.HelperPID
			if id == 0 {
				id = -1 - i
			}
			runs[id] = true
		}
	}
	return len(runs)
}

// Waiting is sent periodically while blocked on a full runner.
type Waiting struct {
	Holders []Holder `json:"holders"`
}

// Acquired reports the slot granted.
type Acquired struct {
	Token            int    `json:"token"` // host-wide concurrency index, 1-based
	Slot             int    `json:"slot"`  // repo workspace slot, 1-based
	Workspace        string `json:"workspace"`
	FingerprintMatch bool   `json:"fingerprint_match"`
	PrevWorktreeKey  string `json:"prev_worktree_key"` // who last synced this slot
	WaitedMS         int64  `json:"waited_ms"`
	MaxSlots         int    `json:"max_slots"`
	// Admit is how many jobs the runner admits at once (AdmitLimit of
	// MaxSlots); 0 from a helper that admits only MaxSlots.
	Admit int `json:"admit,omitempty"`
	// Pending lists files earlier runs created in this slot that were never
	// written back (nor found to be ignored): the next sync removes the ones
	// the client doesn't ignore (Manifest.Remove).
	Pending []string `json:"pending,omitempty"`
}

// Busy is returned when Wait=false and nothing is free.
type Busy struct {
	Holders []Holder `json:"holders"`
}

// Entry is one file in a sync manifest. ID is the Git blob SHA-1 of the
// content (or of the link target for symlinks), which both sides can compute.
type Entry struct {
	Path string `json:"p"`
	ID   string `json:"id"`
	Mode uint32 `json:"m"` // 0o100644, 0o100755, 0o120000
	Size int64  `json:"s"`
}

// Manifest is the full set of files the workspace must contain.
type Manifest struct {
	Fingerprint string   `json:"fingerprint"`
	Entries     []Entry  `json:"entries"`
	Protect     []string `json:"protect"` // dirs never pruned (cache_dirs)
	// Remove: Acquired.Pending files to delete (not ignored by the client).
	// The rest of the pending list is kept and forgotten.
	Remove []string `json:"remove,omitempty"`
}

// Need lists the paths the helper is missing. The CLI follows with a tar blob.
type Need struct {
	Paths []string `json:"paths"`
	Bytes int64    `json:"bytes"`
}

// Synced reports forward sync results. Corrected carries entries whose
// uploaded bytes differed from the manifest (file changed while syncing);
// the CLI uses these as the true baseline.
type Synced struct {
	Written   int     `json:"written"`
	Deleted   int     `json:"deleted"`
	Bytes     int64   `json:"bytes"`
	Corrected []Entry `json:"corrected,omitempty"`
}

// ContainerRun says how to run a command inside the project's container.
type ContainerRun struct {
	Image   string   `json:"image"`             // local image reference to run
	Options []string `json:"options,omitempty"` // extra `docker run` arguments
	Root    bool     `json:"root,omitempty"`    // run as root instead of the runner user
	// Network is "private" for a network namespace of the run's own
	// (Docker's default bridge); anything else is the runner's (host).
	Network string `json:"network,omitempty"`
	// Docker gives the command a Docker socket (scoped to the run) so it
	// can start containers of its own.
	Docker bool `json:"docker,omitempty"`
	// CPUPinning pins a shared slot's container to a block of CPUs of its
	// own; otherwise it may use any of the runner's CPUs.
	CPUPinning bool `json:"cpu_pinning,omitempty"`
}

// RegistryAuth is a short-lived login for the image registry (ECR).
type RegistryAuth struct {
	Server   string `json:"server"`
	Repo     string `json:"repo"` // e.g. 123.dkr.ecr.us-east-2.amazonaws.com/spotexec/repo-abc
	User     string `json:"user"`
	Password string `json:"password"`
}

// Image prepares the project's container image on the runner: pull Ref, or
// build Dockerfile (from the synced workspace) tagged Tag, reusing a copy
// from Registry when another runner already built it.
type Image struct {
	Ref        string        `json:"ref,omitempty"`
	Dockerfile string        `json:"dockerfile,omitempty"`
	Context    string        `json:"context,omitempty"`
	Tag        string        `json:"tag,omitempty"`
	Registry   *RegistryAuth `json:"registry,omitempty"`
	TimeoutMS  int64         `json:"timeout_ms"`
	// WaitPush pushes a fresh build to Registry before replying (instead of
	// in the background), so a push failure is reported.
	WaitPush bool `json:"wait_push,omitempty"`
	// Probe uses an image already on the runner or in Registry, but never
	// builds one: a miss is reported as ImageResult.Missing.
	Probe bool `json:"probe,omitempty"`
	// Source is another region's registry holding Tag: when Registry lacks
	// it, the image is pulled from Source (keeping its ID) and pushed to
	// Registry, instead of being built again (#192).
	Source *RegistryAuth `json:"source,omitempty"`
}

// ImageResult reports the local image reference to run.
type ImageResult struct {
	Image           string `json:"image"`
	ImageID         string `json:"image_id,omitempty"` // immutable identity (keys dependency caches)
	Pulled          bool   `json:"pulled"`
	Built           bool   `json:"built"`
	Pushing         bool   `json:"pushing"`
	Pushed          bool   `json:"pushed,omitempty"`
	Missing         bool   `json:"missing,omitempty"` // Probe: neither on the runner nor in the registry
	Copied          bool   `json:"copied,omitempty"`  // pulled from Image.Source (another region)
	InstalledDocker bool   `json:"installed_docker"`
	MS              int64  `json:"ms"`
}

// Deps asks for lockfile-keyed dependency hydration.
type Deps struct {
	Lockfiles []string          `json:"lockfiles"`
	Install   string            `json:"install"`
	CacheDirs []string          `json:"cache_dirs"`
	Hydrate   string            `json:"hydrate"` // auto|reflink|copy|hardlink
	Setup     string            `json:"setup"`
	Env       map[string]string `json:"env"`
	TimeoutMS int64             `json:"timeout_ms"`
	// Shared cache (optional): URLs (https presigned, or file:// in the
	// simulator) for this dependency key, computed by the client. The helper
	// only uses them if its own key matches CacheKey.
	Container *ContainerRun `json:"container,omitempty"`
	ImageID   string        `json:"image_id,omitempty"` // container image identity for the cache key
	CacheKey  string        `json:"cache_key,omitempty"`
	Toolchain string        `json:"toolchain,omitempty"` // part of the key (see depskey.Toolchain)
	CacheGet  string        `json:"cache_get,omitempty"`
	CachePut  string        `json:"cache_put,omitempty"`
	// Probe hydrates from the runner's own cache or restores from the
	// shared one, but never installs: a miss is reported as
	// DepsResult.Missing.
	Probe bool `json:"probe,omitempty"`
}

// DepsResult reports hydration.
type DepsResult struct {
	Key      string `json:"key"`
	Built    bool   `json:"built"`
	Restored bool   `json:"restored"`  // fetched from the shared cache instead of installing
	Uploaded bool   `json:"uploading"` // a background upload to the shared cache started
	Hydrated bool   `json:"hydrated"`
	Missing  bool   `json:"missing,omitempty"` // Probe: nothing to restore, an install is needed
	Method   string `json:"method"`
	MS       int64  `json:"ms"`
	// FlushMS is the part of MS spent waiting for what the dependencies
	// wrote to reach the disk, before a containerised run (#253).
	FlushMS int64 `json:"flush_ms,omitempty"`
}

// ShareWait asks the helper to wait for the background transfer this
// session started for Kind (ShareDeps: the dependency cache upload;
// ShareImage: the image push), so the runs waiting for it can fetch it.
type ShareWait struct {
	Kind      string `json:"kind"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// ShareWait kinds.
const (
	ShareDeps  = "deps"
	ShareImage = "image"
)

// ShareDone answers ShareWait: whether the transfer finished and succeeded.
type ShareDone struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"` // why not
	MS      int64  `json:"ms"`
}

// Exec runs a command in the slot workspace.
type Exec struct {
	Shell     string            `json:"shell"` // command line run via bash -lc (or sh -c)
	Cwd       string            `json:"cwd"`   // relative to workspace
	Env       map[string]string `json:"env"`
	Setup     string            `json:"setup"` // remote_path_setup prefix
	TimeoutMS int64             `json:"timeout_ms"`
	GraceMS   int64             `json:"grace_ms"`
	Snapshot  bool              `json:"snapshot"` // compute mutations (and track created files)
	Skip      []string          `json:"skip"`     // dirs the post-run walk skips: a bare name at any depth, "/name" or a path/glob from the root
	SpotWatch bool              `json:"spot_watch"`
	Container *ContainerRun     `json:"container,omitempty"`
	// SetupRun ([setup] run) runs as its own process, from the workspace
	// root, before Shell and inside the same snapshot. Its output is held
	// back and sent (as stderr) only if it fails; then Shell never runs.
	SetupRun       string `json:"setup_run,omitempty"`
	SetupTimeoutMS int64  `json:"setup_timeout_ms,omitempty"`
}

// Started confirms the child was spawned.
type Started struct {
	PID int `json:"pid"`
}

// Result is the final exec outcome plus the mutation batch.
type Result struct {
	ExitCode    int    `json:"exit_code"`
	Signal      string `json:"signal,omitempty"`
	TimedOut    bool   `json:"timed_out"`
	Canceled    bool   `json:"canceled"`
	Interrupted bool   `json:"spot_interrupted"`
	// Withdrawn: the client withdrew the job (TypeWithdraw) while it
	// waited for a run permit. Nothing ran, and the workspace is as the
	// client's sync left it.
	Withdrawn bool `json:"withdrawn,omitempty"`
	// Shutdown: the command was killed by the VM shutting down (a Spot
	// reclaim): a lost runner, not the command's result.
	Shutdown   bool     `json:"shutdown,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Changed    []Entry  `json:"changed,omitempty"` // manifest files with new content/mode
	Deleted    []string `json:"deleted,omitempty"` // manifest files removed
	Created    []Entry  `json:"created,omitempty"` // non-manifest files created/changed
	// GitDirs are the .git directories below the root the run created:
	// never written back, and removed by the next sync unless ignored.
	GitDirs    []string `json:"git_dirs,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
	Unverified []string `json:"unverified,omitempty"` // paths that could not be read after the run
	SnapshotMS int64    `json:"snapshot_ms"`
	StartError string   `json:"start_error,omitempty"`
	// OOMKilled: the kernel's OOM killer killed a process in the command's
	// container, for a memory limit on it or on the memory pool it ran in,
	// or for the whole machine running out (Docker's oom event).
	OOMKilled bool `json:"oom_killed,omitempty"`
	// MemoryLimitMB is the cap of the memory pool the container ran in
	// (MiB), or 0 when it ran outside it.
	MemoryLimitMB int64 `json:"memory_limit_mb,omitempty"`
	// SetupFailed: Exec.SetupRun did not succeed (it failed, timed out, was
	// canceled or couldn't start), so Shell never ran. ExitCode, Signal,
	// TimedOut, Canceled, StartError and OOMKilled then describe the setup.
	SetupFailed bool  `json:"setup_failed,omitempty"`
	SetupMS     int64 `json:"setup_ms,omitempty"`
}

// Heartbeat is the body of a helper heartbeat (empty from older helpers):
// the runner's memory, so a client that loses the runner can tell whether
// it was out of memory at the time. Zero fields are unknown.
type Heartbeat struct {
	MemTotalMB int64   `json:"mem_total_mb,omitempty"`
	MemAvailMB int64   `json:"mem_avail_mb,omitempty"`
	PSIFull10  float64 `json:"psi_full10,omitempty"` // /proc/pressure/memory "full" avg10, percent
}

// Pressure says whether a heartbeat shows a runner running out of memory:
// under 5% of it available, or every non-idle task stalled on memory for
// over 10% of the last 10s. "" when it doesn't.
func (h Heartbeat) Pressure() string {
	switch {
	case h.MemTotalMB > 0 && h.MemAvailMB*20 < h.MemTotalMB:
		return fmt.Sprintf("%d MiB of %d MiB memory available", h.MemAvailMB, h.MemTotalMB)
	case h.PSIFull10 >= 10:
		return fmt.Sprintf("every task stalled on memory %.0f%% of the time", h.PSIFull10)
	}
	return ""
}

// Fetch asks for the content of mutated files. Accept lists created paths the
// CLI decided are in scope, so the helper adds them to the slot manifest.
type Fetch struct {
	Paths  []string `json:"paths"`
	Accept []string `json:"accept"`
}

// Settle tells the helper which files the run created are ignored by the
// client (build outputs): they stay in the slot and are no longer pending.
// There is no reply.
type Settle struct {
	Ignored []string `json:"ignored"`
}

// Pull asks for gitignored artifacts by glob (relative to the workspace).
type Pull struct {
	Patterns []string `json:"patterns"`
}

// GitSeed asks the helper to give the slot workspace a minimal .git
// ([sync] git_seed) after the forward sync: the HEAD commit as a shallow
// root, its trees, the HEAD blobs of files changed locally, HEAD and the
// branch ref, and an index built from HEAD. The helper answers GitNeed
// (the CLI follows with a tar blob of those objects when any are missing),
// then GitSeeded. Nothing else of the local repository is sent.
type GitSeed struct {
	// Key identifies the requested state: when it matches what the slot
	// holds and nothing in .git was touched since, nothing is redone.
	Key  string `json:"key"`
	Head string `json:"head,omitempty"` // HEAD commit; empty on an unborn branch
	Ref  string `json:"ref,omitempty"`  // refs/heads/<branch>; empty when detached
	// Objects lists every object ID the seed holds (anything else in the
	// slot's object store is removed).
	Objects []string `json:"objects,omitempty"`
	// SkipWorktree: paths in HEAD that weren't synced (excluded files,
	// submodules) or that git status hides locally, so they don't show up
	// as deleted or changed on the runner.
	SkipWorktree []string `json:"skip_worktree,omitempty"`
	// IntentToAdd: synced files staged locally that HEAD doesn't have.
	IntentToAdd []string `json:"intent_to_add,omitempty"`
}

// GitNeed lists the objects the slot is missing. When non-empty, the CLI
// follows with a gzip'd tar blob holding one "<type>/<id>" entry each, with
// the object's raw content.
type GitNeed struct {
	IDs []string `json:"ids"`
}

// GitSeeded reports the seed. Warning is set when the slot was left
// without a .git (git missing on the runner, or seeding failed); the run
// goes on either way.
type GitSeeded struct {
	Reseeded bool   `json:"reseeded"`
	Objects  int    `json:"objects"`
	Bytes    int64  `json:"bytes"`
	Warning  string `json:"warning,omitempty"`
	MS       int64  `json:"ms"`
}

// Cancel asks the helper to terminate the running command.
type Cancel struct {
	Reason string `json:"reason"`
}

// Event is an out-of-band notice from the helper (e.g. spot interruption).
type Event struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}
