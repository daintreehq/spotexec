package protocol

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
)

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
	CodeShutdown     = "RUNNER_SHUTTING_DOWN" // the VM began shutting down under the request
)

// ErrorMsg is sent by the helper for any failed request.
type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
}

// ImageResult reports the local image reference to run.
type ImageResult struct {
	Image           string `json:"image"`
	ImageID         string `json:"image_id,omitempty"` // immutable identity (keys dependency caches)
	Pulled          bool   `json:"pulled"`
	Built           bool   `json:"built"`
	Pushing         bool   `json:"pushing"`
	Pushed          bool   `json:"pushed,omitempty"`
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
}

// DepsResult reports hydration.
type DepsResult struct {
	Key      string `json:"key"`
	Built    bool   `json:"built"`
	Restored bool   `json:"restored"`  // fetched from the shared cache instead of installing
	Uploaded bool   `json:"uploading"` // a background upload to the shared cache started
	Hydrated bool   `json:"hydrated"`
	Method   string `json:"method"`
	MS       int64  `json:"ms"`
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
	Skip      []string          `json:"skip"`     // dir names/paths skipped by the post-run walk
	SpotWatch bool              `json:"spot_watch"`
	Container *ContainerRun     `json:"container,omitempty"`
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
	// Shutdown: the command was killed by the VM shutting down (a Spot
	// reclaim): a lost runner, not the command's result.
	Shutdown   bool     `json:"shutdown,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Changed    []Entry  `json:"changed,omitempty"` // manifest files with new content/mode
	Deleted    []string `json:"deleted,omitempty"` // manifest files removed
	Created    []Entry  `json:"created,omitempty"` // non-manifest files created/changed
	Truncated  bool     `json:"truncated,omitempty"`
	Unverified []string `json:"unverified,omitempty"` // paths that could not be read after the run
	SnapshotMS int64    `json:"snapshot_ms"`
	StartError string   `json:"start_error,omitempty"`
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

// Cancel asks the helper to terminate the running command.
type Cancel struct {
	Reason string `json:"reason"`
}

// Event is an out-of-band notice from the helper (e.g. spot interruption).
type Event struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}
