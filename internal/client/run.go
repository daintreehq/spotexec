package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/daintreehq/spotexec/internal/dockerignore"
	"github.com/daintreehq/spotexec/internal/fsid"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// Exit codes for spotexec's own failures (command exit codes pass through).
const (
	ExitInfra       = 125
	ExitTimeout     = 124
	ExitInterrupted = 130 // Ctrl-C, SIGTERM or SIGHUP
	// ExitWritebackConflict: with Options.FailOnConflict, the command
	// succeeded but write-back kept a local edit over one of its changes.
	ExitWritebackConflict = 122
)

// Large-sync guardrails.
const (
	WarnFiles  = 25000
	AbortFiles = 100000
	WarnBytes  = 1 << 30
	AbortBytes = 5 << 30
)

// Options for one run.
type Options struct {
	Command      string // shell command line
	Display      string // for status/holder display
	Timeout      time.Duration
	Grace        time.Duration
	QueueTimeout time.Duration
	Sync         string // auto|in|none
	Writeback    string // always|on-success|never
	// WritebackLimits: what a write-back may bring back before it warns
	// (or holds the new files in the slot). Zero: no limits.
	WritebackLimits WritebackLimits
	Pool            []string
	Exclusive       bool
	Pull            []string
	NoDeps          bool
	NoSetup         bool // skip [setup]
	ForceLargeSync  bool
	PreferLast      bool     // exec: reuse last runner+slot without syncing
	SameSlot        bool     // --same-slot: sync and run in this worktree's last slot
	LastWithin      []string // with PreferLast/SameSlot: the last runner must be one of these
	DeferLastRun    bool     // return the last-run record in Result instead of saving it
	Stdout          io.Writer
	Stderr          io.Writer
	Rep             ui.Reporter
	Interrupt       <-chan struct{} // each receive = one Ctrl-C
	Label           string
	RetryPreempt    bool
	NoBoot          bool
	SyncOnly        bool // stop after sync + deps (spotexec sync)
	WaitPush        bool // push a freshly built image before returning (container build)
	Priority        int  // queue priority when every slot is busy (higher first)
	// FailOnConflict turns a successful run whose write-back conflicted
	// into ExitWritebackConflict, so its exit status says the changes
	// didn't all land.
	FailOnConflict bool
	// Env is set for the command over the project's env (a shard's
	// SPOTEXEC_SHARD and SPOTEXEC_SHARDS).
	Env map[string]string
}

// Result of a run.
type Result struct {
	// Reached: some attempt got a runner (so the command may have run there;
	// a local fallback must not run it again).
	Reached   bool   `json:"-"`
	ExitCode  int    `json:"exit_code"`
	Runner    string `json:"runner"`
	Slot      int    `json:"slot"`
	Duration  time.Duration
	TimedOut  bool     `json:"timed_out"`
	Canceled  bool     `json:"canceled"` // interrupted (Ctrl-C/SIGTERM); ExitCode is 130
	Mutated   []string `json:"mutated_files"`
	Deleted   []string `json:"deleted_files"`
	Conflicts []string `json:"conflicts"`
	// BackedUp: files write-back deleted or replaced that git had no
	// committed copy of, saved under BackupDir first.
	BackedUp  []string `json:"backed_up_files,omitempty"`
	BackupDir string   `json:"backup_dir,omitempty"`
	// Held: deletions write-back kept back (the project's own config, or
	// most of the synced files).
	Held   []string `json:"held_deletions,omitempty"`
	Pulled int      `json:"pulled"`
	// HeldFiles, HeldBytes: new files left in the slot, over the
	// write-back limit.
	HeldFiles int    `json:"held_files"`
	HeldBytes int64  `json:"held_bytes"`
	Synced    int    `json:"synced_files"`
	SyncBytes int64  `json:"synced_bytes"`
	Image     string `json:"image,omitempty"` // the project container image used
	// DepsInstalled is how long the dependency phase took when it ran the
	// install (a cache miss); zero otherwise.
	DepsInstalled time.Duration `json:"-"`
	// LastRun is where this worktree's files now are, when the run
	// synced (saved already unless Options.DeferLastRun).
	LastRun *LastRun `json:"-"`
	// OOMKilled: the kernel killed a process in the command's container
	// for running out of memory: the shared memory pool's cap, the
	// container's own limit, or the whole VM's memory.
	OOMKilled bool `json:"oom_killed,omitempty"`
	// SetupFailed: [setup] failed, timed out or couldn't start, so the
	// command never ran (ExitCode is 1, or 124 for a timeout).
	SetupFailed bool `json:"setup_failed,omitempty"`
	// noReplay, when the runner was lost, is why the run must not be
	// replayed elsewhere (see replayable).
	noReplay noReplay
	// lostAt, when the runner was lost, is how far the attempt had got.
	lostAt stage
}

// stage is how far an attempt had got: the phase it was in, whether the
// command had been handed to the helper (dispatched), after which it may
// have started and its outcome is unknown, and whether the helper had
// confirmed starting the phase's process ([setup] run or the command).
type stage struct {
	phase      string
	dispatched bool
	started    bool
}

// where describes s for a warning about losing the runner during it.
func (s stage) where() string {
	if s.dispatched {
		return "while " + s.phase
	}
	return "while " + s.phase + ", before the command started"
}

// progress tracks an attempt's stage as it goes. It is read when the
// runner is lost; helper events may update it from another goroutine.
type progress struct {
	mu sync.Mutex
	at stage
}

func (p *progress) set(phase string) {
	p.mu.Lock()
	p.at.phase = phase
	p.mu.Unlock()
}

// dispatch records that the command is being handed to the helper, from
// just before the request is sent: a send that fails part way may still
// have reached it. setup: the helper runs [setup] run first.
func (p *progress) dispatch(setup bool) {
	phase := phaseStartCommand
	if setup {
		phase = phaseStartSetup
	}
	p.mu.Lock()
	p.at = stage{phase: phase, dispatched: true}
	p.mu.Unlock()
}

// started records the helper confirming it started the process the
// attempt was waiting on.
func (p *progress) started() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.at.phase {
	case phaseStartSetup:
		p.at.phase, p.at.started = phaseSetup, true
	case phaseStartCommand:
		p.at.phase, p.at.started = phaseCommand, true
	}
}

// setupDone records that [setup] run finished and the command is next.
func (p *progress) setupDone() {
	p.mu.Lock()
	p.at.phase, p.at.started = phaseStartCommand, false
	p.mu.Unlock()
}

func (p *progress) get() stage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.at
}

// noReplay is a warning code and the reason behind it.
type noReplay struct{ code, why string }

// InfraError marks failures of spotexec itself (exit 125).
type InfraError struct{ Err error }

func (e *InfraError) Error() string { return e.Err.Error() }
func (e *InfraError) Unwrap() error { return e.Err }

func infra(err error) error {
	if err == nil {
		return nil
	}
	var ie *InfraError
	if errors.As(err, &ie) {
		return err
	}
	return &InfraError{err}
}

// attempt identifies one try of a logical run. SPOTEXEC_RUN_ID stays the
// same across retries so commands with external side effects can dedupe.
type attempt struct {
	runID   string
	n       int
	exclude map[string]bool // fleet instances lost in earlier attempts
	fresh   *bool           // an earlier attempt already showed CONTAINER_FRESH
}

// Run executes opts.Command remotely for workspace ws. If the runner is lost
// before the command's result arrives (a reclaimed Spot instance, a dead
// VM), the run is replayed once on another slot: nothing from the lost
// attempt was applied locally (whyNoReplay says when it isn't). Once a
// result has arrived it is never replayed.
func Run(ctx context.Context, cfg *config.Config, ws *Workspace, o Options) (Result, error) {
	if err := validOptions(o); err != nil {
		return Result{}, infra(err)
	}
	if !ws.Marked && o.Sync != "none" && !o.PreferLast {
		if config.NeverProject(ws.Root) {
			return Result{}, infra(fmt.Errorf("spotexec never syncs %s: it is your home directory, / or a credential directory (~/.aws, ~/.ssh); cd into a project, or pass --sync none", ws.Root))
		}
		return Result{}, infra(fmt.Errorf("%s is not in a Git repository and has no .spotexec.toml, so spotexec won't sync it; run `spotexec init` there to mark it as a project, or pass --sync none", ws.Root))
	}
	defer KeepAwake()()
	att := attempt{runID: newRunID(), n: 1, exclude: map[string]bool{}, fresh: new(bool)}
	reached := false
	for {
		res, err, lost := runOnce(ctx, cfg, ws, o, att)
		reached = reached || res.Runner != ""
		res.Reached = reached
		retry, code, why := replayable(lost, res, o, ctx.Err() != nil, att.n)
		if why != "" {
			o.Rep.Warn(code, "%s", why)
		}
		if !retry {
			return res, err
		}
		o.Rep.Warn("RUNNER_LOST", "lost %s %s (%v); replaying the run on another slot", lost, res.lostAt.where(), err)
		o.Rep.Event("retry", map[string]any{"run_id": att.runID, "attempt": att.n + 1, "lost": lost})
		if lost != "static" {
			att.exclude[lost] = true
			markLost(cfg, res.Runner, lost)
		}
		att.n++
	}
}

// replayable decides whether an attempt that lost its runner (lost, from
// lostRunner) is replayed on another slot. why is set when whyNoReplay
// withheld the replay (the runner was out of memory, or still running with
// no notice it was going away after the command was dispatched): that run
// is never replayed, since a run that used up its VM's memory would most
// likely do the same on the next one, and take that VM's other jobs with
// it.
func replayable(lost string, res Result, o Options, canceled bool, n int) (retry bool, code, why string) {
	if lost == "" {
		return false, "", ""
	}
	eligible := o.RetryPreempt && !canceled && o.Sync != "none" && !o.PreferLast && !o.SameSlot && n < 2
	if res.noReplay.why == "" {
		return eligible, "", ""
	}
	why = "lost " + lost + " " + res.noReplay.why
	if eligible {
		why += "; not replaying the run on another slot"
	}
	return false, res.noReplay.code, why
}

// whyNoReplay decides, once a fleet lease's runner is lost at stage at,
// whether the run must not be replayed elsewhere: its VM reported memory
// pressure (a run that used up its VM's memory would most likely do the
// same on the next one, and take that VM's other jobs with it), or the
// command had been dispatched and the VM is still running and gave no Spot
// notice, so whatever stopped it answering came from inside, quite
// possibly from the command, whose outcome is unknown. A VM that announced
// it was going away (a Spot notice, or its helper reporting the shutdown,
// as at max_lifetime) is replayed whatever its memory looked like: it was
// going down whatever the run did. A VM that is gone is replayed too, and
// so is one lost before the command was dispatched (the command can't
// have run, so it has no outcome to protect), unless it was out of memory.
// exclusive is whether the run already had the whole VM.
//
// A runner lost after this machine was asleep (or sx was stopped) for
// longer than the runner may wait for a silent client (#142) was most
// likely lost to that, not to the VM or the run: replayed if the command
// hadn't been dispatched, and otherwise not (its outcome is unknown), with
// a warning that says so instead of blaming the run or the VM.
func whyNoReplay(lease *Lease, noticed bool, at stage, exclusive bool) noReplay {
	if noticed {
		return noReplay{}
	}
	if d := sessionSuspended(lease.Sess); d > 0 {
		if !at.dispatched {
			return noReplay{}
		}
		return noReplay{"CLIENT_SUSPENDED", fmt.Sprintf("%s, after this machine was asleep or sx was stopped for %s: a runner may end a run after %s without word from sx, and the attempt's outcome is unknown. Rerun it, and keep this machine awake while it runs", at.where(), ui.Dur(d), transport.HelperPatience.Round(time.Second))}
	}
	if p := lease.Sess.MemoryPressure(PressureWindow); p != "" {
		return noReplay{"MEMORY_PRESSURE", at.where() + ": it was out of memory (" + p + "); it would likely run out on another VM too. Give the run more memory (--exclusive, fewer max_slots, a larger instance type) or make it use less"}
	}
	if !at.dispatched || !instanceRunning(lease) {
		return noReplay{}
	}
	what := "the command"
	if at.phase == phaseSetup || at.phase == phaseStartSetup {
		what = "[setup] run"
	}
	why := at.where() + ", although EC2 still reports it running, without a Spot notice: "
	if !at.started {
		// Handed over, but not confirmed started: nothing says it ran,
		// let alone overloaded the VM, but it may have.
		why += what + " may have started, so the attempt's outcome is unknown"
		if m := lastMemory(lease.Sess); m != "" {
			why += " (" + m + ")"
		}
		return noReplay{"RUNNER_UNRESPONSIVE", why + ". Rerun it"}
	}
	why += what + " may be what overloaded it (memory, most often)"
	if m := lastMemory(lease.Sess); m != "" {
		why += "; " + m
	}
	why += ". The attempt's outcome is unknown. Rerun it"
	if !exclusive {
		why += ", with --exclusive if it may need the whole VM"
	}
	return noReplay{"RUNNER_UNRESPONSIVE", why}
}

// Phases of an attempt (stage.phase), as warnings name them.
const (
	phaseSlot         = "setting up the slot"
	phaseSync         = "syncing files"
	phaseGitSeed      = "seeding .git"
	phaseImage        = "preparing the container image"
	phaseDepsCheck    = "checking the dependency cache"
	phaseDeps         = "installing dependencies"
	phaseStartSetup   = "starting [setup] run"
	phaseSetup        = "running [setup] run"
	phaseStartCommand = "starting the command"
	phaseCommand      = "running the command"
)

// lastMemory describes the memory reading of s's latest heartbeat, "" when
// none carried one.
func lastMemory(s *transport.Session) string {
	m, ok := s.LastMemory()
	if !ok {
		return ""
	}
	return fmt.Sprintf("its last heartbeat (%s ago) reported %s of %s memory available", ui.Dur(time.Since(m.At).Round(time.Second)), ui.Bytes(m.AvailMB<<20), ui.Bytes(m.TotalMB<<20))
}

// sessionSuspended is how long this machine was suspended while the
// session was open, past the runner's patience (transport.Session.Suspended;
// tests replace it).
var sessionSuspended = (*transport.Session).Suspended

// noteReclaimed tells the lease's fleet that AWS is reclaiming its VM, so
// new launches (a replacement first) prefer other zones and types for a
// while.
func noteReclaimed(lease *Lease) {
	if rc, ok := lease.Provider.(provider.Reclaimer); ok && lease.Instance.ID != "" {
		rc.Reclaimed(lease.Instance)
	}
}

// replayVerdict is whyNoReplay for an attempt that lost its runner with
// err: a shutdown the helper reported counts as the VM's notice, since EC2
// still reports a VM running while its OS halts.
func replayVerdict(lease *Lease, noticed bool, err error, at stage, exclusive bool) noReplay {
	return whyNoReplay(lease, noticed || vmShutdown(err), at, exclusive)
}

// shutdownErr is what runOnce returns when the VM's shutdown killed the
// command.
func shutdownErr(label string, code int) error {
	return infra(&shutdownError{label, code})
}

// shutdownError is an attempt whose command the VM's shutdown killed.
type shutdownError struct {
	label string
	code  int
}

func (e *shutdownError) Error() string {
	return fmt.Sprintf("%s shut down while the command ran (it was stopped with exit %d); nothing was written back", e.label, e.code)
}

// vmShutdown reports whether err says the VM began shutting down under the
// attempt: the helper's RUNNER_SHUTTING_DOWN error, or a result the
// shutdown killed (shutdownError).
func vmShutdown(err error) bool {
	var se *shutdownError
	return errors.As(err, &se) || helperShutdown(err)
}

// helperShutdown reports whether err is the helper saying the VM began
// shutting down under the request (protocol.CodeShutdown).
func helperShutdown(err error) bool {
	var re *transport.RemoteError
	return errors.As(err, &re) && re.Code == protocol.CodeShutdown
}

// lostRunner reports what to exclude when the connection to a lease died.
func lostRunner(l *Lease, err error) string {
	if l == nil || err == nil || !l.Runner.Ephemeral() {
		return ""
	}
	// An error the helper reported itself means it was alive to report it
	// (the command may well have run): never treat that as a lost runner,
	// unless it says the VM is shutting down (a container killed as dockerd
	// stops with the VM).
	if helperShutdown(err) {
		return shutdownRunner(l)
	}
	var re *transport.RemoteError
	if errors.As(err, &re) {
		return ""
	}
	// Writes to a dead SSH channel fail with plain pipe errors; the session
	// being gone is what matters.
	if !transport.IsConnError(err) && !(l.Sess != nil && l.Sess.Dead()) {
		return ""
	}
	if l.InstanceID != "" {
		return l.InstanceID
	}
	return "static"
}

func runOnce(ctx context.Context, cfg *config.Config, ws *Workspace, o Options, att attempt) (res Result, err error, lost string) {
	start := time.Now()
	p := cfg.Project
	runID := att.runID

	// Returning to the last slot needs only that runner: a default pool this
	// machine can't use, such as a project fleet built on a runner it lacks,
	// mustn't block it. A pool given with --runner/--pool (LastWithin) still
	// has to be valid.
	pool, err := cfg.Pool(o.Pool)
	if err != nil && (!o.PreferLast && !o.SameSlot || len(o.LastWithin) > 0) {
		return res, infra(err), ""
	}

	// 1. Local manifest (what the remote workspace must contain).
	var m *manifest.Manifest
	if o.Sync != "none" {
		o.Rep.Status("scanning %s", ws.Root)
		t := time.Now()
		m, err = manifest.Build(ctx, ManifestOptions(ws.Root, p))
		if err != nil {
			return res, infra(fmt.Errorf("enumerating files: %w", err)), ""
		}
		o.Rep.Event("manifest", map[string]any{"files": len(m.Entries), "bytes": m.Bytes, "duration_ms": time.Since(t).Milliseconds(), "git": m.Git})
		if len(m.Entries) > WarnFiles && (len(m.Entries) <= AbortFiles || o.ForceLargeSync) {
			o.Rep.Warn("LARGE_SYNC", "syncing %d files; consider [sync].exclude", len(m.Entries))
		}
	}
	if err := checkLocal(ctx, m, p, ws, o); err != nil {
		return res, infra(err), ""
	}

	// 2. Runner + slot.
	host, _ := os.Hostname()
	base := protocol.Acquire{
		RepoID: ws.RepoID, WorktreeKey: ws.WorktreeKey, Exclusive: o.Exclusive,
		Client: protocol.ClientInfo{Host: host, PID: os.Getpid(), Worktree: ws.Root, Command: o.Display, RunID: runID},
	}
	if m != nil {
		base.Fingerprint = m.Fingerprint
	}
	// Without a sync the slot's files are not ours: don't claim ownership.
	base.KeepOwner = o.Sync == "none"
	req := AcquireRequest{Pool: pool, Base: base, QueueTimeout: o.QueueTimeout, Rep: o.Rep, NoBoot: o.NoBoot, Exclude: att.exclude, Priority: o.Priority}
	if o.Sync != "none" {
		// A launch for this run prefers regions that hold its caches.
		req.Project = func(r *config.Runner) *provider.Project { return PlacementProject(p, ws, m, o.NoDeps, r) }
	}
	// Recently used here: go back to that VM while it has room (warm files
	// and build caches). After a while, let work consolidate onto the
	// oldest VMs again so younger ones can idle out.
	if fi, err := os.Stat(lastRunPath(ws)); err == nil && time.Since(fi.ModTime()) < 10*time.Minute {
		if lr, err := LoadLastRun(ws); err == nil {
			req.PreferInstance = lr.InstanceID
		}
	}
	var lease *Lease
	if o.PreferLast || o.SameSlot {
		// exec runs without syncing, so it must land in exactly the slot that
		// holds this worktree's files; --same-slot syncs, but wants the same
		// machine state (whatever the last commands installed there).
		lr, err := LoadLastRun(ws)
		if err != nil {
			return res, infra(errors.New("no previous run from this worktree to return to; run `sx <command>` first")), ""
		}
		if len(o.LastWithin) > 0 && !slices.Contains(o.LastWithin, lr.Runner) {
			return res, infra(fmt.Errorf("this worktree last ran on %s, which --runner/--pool excludes (%s), so --same-slot can't return there; drop one of them", lr.Runner, strings.Join(o.LastWithin, ", "))), ""
		}
		r, err := cfg.Runner(lr.Runner)
		if err != nil {
			return res, infra(err), ""
		}
		host, _ := os.Hostname()
		fp := ""
		if o.SameSlot {
			fp = base.Fingerprint
		}
		lease, err = AcquireSlot(ctx, cfg, ws, r, lr, o.Rep, host, o.Display, o.Exclusive, o.QueueTimeout, o.SameSlot && o.Sync != "none", fp)
		if err != nil {
			return res, infra(fmt.Errorf("can't return to this worktree's last slot (%s slot %d%s): %w", lr.Runner, lr.Slot, instanceSuffix(lr.InstanceID), err)), ""
		}
	} else {
		lease, err = Acquire(ctx, req)
		if err != nil {
			return res, infra(err), ""
		}
	}
	defer lease.Close()
	if lease.InstanceID != "" {
		lease.Sess.SetOnSilent(waitIfRunning(lease, o.Rep))
	}
	var noticed atomic.Bool // a reclamation notice arrived
	// Before any replacement launches: steer it away from this VM's pool.
	reclaimed := sync.OnceFunc(func() { noteReclaimed(lease) })
	prog := &progress{at: stage{phase: phaseSlot}}
	defer func() {
		if lost != "" {
			res.lostAt = prog.get()
			if lease.InstanceID != "" {
				res.noReplay = replayVerdict(lease, noticed.Load(), err, res.lostAt, o.Exclusive)
			}
		}
	}()
	// Before the command starts, Ctrl-C aborts the session outright (a
	// blocked upload cannot otherwise be interrupted). Once it runs,
	// interrupts are forwarded as cancels and the session stays up so
	// mutations made before the cancel are still written back.
	execStarted := make(chan struct{})
	var stopWatch sync.Once
	defer stopWatch.Do(func() { close(execStarted) })
	go func() {
		select {
		case <-ctx.Done():
			lease.Sess.Abort()
		case <-execStarted:
		}
	}()
	r := lease.Runner
	res.Runner, res.Slot = r.Name, lease.Acq.Token
	sess := lease.Sess
	sess.OnEvent = func(ev protocol.Event) {
		o.Rep.Event(ev.Name, map[string]any{"message": ev.Message})
		if ev.Name == "deps_install" {
			prog.set(phaseDeps)
		}
		if ev.Name == "spot_interruption" {
			noticed.Store(true)
			reclaimed()
		}
	}
	remoteWS := lease.Acq.Workspace
	// The last-run record (used by exec and pull) is written once this
	// worktree's files are in the slot, and never for unsynced runs.
	lastRun := LastRun{Runner: r.Name, InstanceID: lease.InstanceID, Slot: lease.Acq.Slot, Target: lease.Target, WorkRoot: r.WorkRoot, RemoteWS: remoteWS}
	recordLastRun := func() { keepLastRun(ws, o, lastRun, &res) }
	label := fmt.Sprintf("%s#%d", r.Name, lease.Acq.Token)

	// 3. Forward sync.
	if m != nil {
		if lease.Acq.FingerprintMatch {
			o.Rep.Event("sync_in", map[string]any{"mode": "fingerprint_hit", "files": 0, "bytes": 0, "duration_ms": 0})
		} else {
			t := time.Now()
			prog.set(phaseSync)
			o.Rep.Status("%s · syncing %d files", label, len(m.Entries))
			stray, err := strayFiles(ctx, ws, m, lease.Acq.Pending)
			if err != nil {
				return res, infra(fmt.Errorf("checking files earlier runs left in %s: %w", label, err)), ""
			}
			syn, err := syncIn(ctx, sess, m, p.Deps.CacheDirs, stray, o)
			if err != nil {
				return res, infra(fmt.Errorf("sync to %s: %w", r.Name, err)), lostRunner(lease, err)
			}
			// The sync removed whatever an earlier write-back held here.
			clearHeld(ws, lastRun)
			res.Synced, res.SyncBytes = syn.Written, syn.Bytes
			o.Rep.Event("sync_in", map[string]any{"mode": "delta", "files": syn.Written, "deleted": syn.Deleted, "bytes": syn.Bytes, "duration_ms": time.Since(t).Milliseconds()})
			o.Rep.Status("%s · synced %d files (%s) in %s", label, syn.Written, ui.Bytes(syn.Bytes), ui.Dur(time.Since(t)))
		}
		// Before the image and dependencies, so install scripts see it too.
		prog.set(phaseGitSeed)
		if err := seedGit(ctx, sess, m, p.Sync.GitSeed, o, label); err != nil {
			return res, infra(fmt.Errorf("seeding .git on %s: %w", r.Name, err)), lostRunner(lease, err)
		}
	}

	// 4. Project container image.
	var crun *protocol.ContainerRun
	imageID := ""
	if p.Container.Enabled() {
		prog.set(phaseImage)
		ref, id, err := prepareImage(ctx, sess, p, o, label, ws, lease, m)
		if err != nil {
			return res, infra(err), lostRunner(lease, err)
		}
		crun = &protocol.ContainerRun{Image: ref, Options: p.Container.Options, Root: p.Container.User == "root", Network: p.Container.Network, Docker: p.Container.Docker}
		imageID = id
		lastRun.Image = ref
		res.Image = ref
	}
	recordLastRun()

	// 5. Dependencies.
	if p.Deps.Install != "" && !o.NoDeps && o.Sync != "none" {
		prog.set(phaseDepsCheck)
		dr, err := runDeps(ctx, sess, p, r, o, label, ws, lease, crun, imageID, m)
		if err != nil {
			return res, infra(err), lostRunner(lease, err)
		}
		if dr.Built {
			res.DepsInstalled = time.Duration(dr.MS) * time.Millisecond
		}
	}

	if o.SyncOnly {
		res.Duration = time.Since(start)
		o.Rep.Event("result", map[string]any{"exit_code": 0, "duration_ms": res.Duration.Milliseconds(), "runner": r.Name, "slot": res.Slot, "sync_only": true, "conflict_count": 0})
		return res, nil, ""
	}

	// 5. Execute.
	env := map[string]string{
		"SPOTEXEC":           "1",
		"SPOTEXEC_SLOT":      fmt.Sprint(lease.Acq.Token),
		"SPOTEXEC_PORT_BASE": fmt.Sprint(20000 + lease.Acq.Token*1000),
		"SPOTEXEC_RUNNER":    r.Name,
		"SPOTEXEC_RUN_ID":    runID,
		"SPOTEXEC_ATTEMPT":   fmt.Sprint(att.n),
		"SPOTEXEC_WORKSPACE": remoteWS,
		// CPUs this job may use: its fair share of the machine, or all of
		// it for --exclusive. Use it for test-runner worker counts.
		"SPOTEXEC_CPUS":        fmt.Sprint(cpuShare(sess.Hello.CPUs, lease.Acq.MaxSlots, o.Exclusive)),
		"SPOTEXEC_SLOTS":       fmt.Sprint(lease.Acq.MaxSlots),
		"COMPOSE_PROJECT_NAME": fmt.Sprintf("sx_%s_s%02d", strings.ToLower(strings.ReplaceAll(ws.RepoName, ".", "_")), lease.Acq.Token),
		"CI":                   "true",
	}
	if f, ok := o.Stdout.(*os.File); ok && ui.IsTerminal(f) {
		env["FORCE_COLOR"] = "1"
		env["CLICOLOR_FORCE"] = "1"
	}
	for _, k := range p.EnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range o.Env {
		env[k] = v
	}
	skip := manifest.SnapshotSkip(p.Deps.CacheDirs)
	ex := protocol.Exec{
		Shell: o.Command, Cwd: ws.Cwd, Env: env, Setup: r.RemotePathSetup,
		TimeoutMS: o.Timeout.Milliseconds(), GraceMS: o.Grace.Milliseconds(),
		// Always snapshotted, so the helper knows which files even an
		// unsynced run created (removed by the next run that syncs here).
		Snapshot: true, Skip: skip, SpotWatch: r.Type == config.TypeAWSSpot,
		Container: crun,
	}
	if run, timeout := SetupFor(p, o); run != "" {
		ex.SetupRun, ex.SetupTimeoutMS = run, timeout.Milliseconds()
		o.Rep.Status("%s · running setup ([setup] run)", label)
		o.Rep.Event("setup", map[string]any{"runner": r.Name, "slot": lease.Acq.Token, "command": run})
	} else {
		o.Rep.Status("%s · running %s", label, o.Display)
	}
	o.Rep.Event("exec", map[string]any{"runner": r.Name, "slot": lease.Acq.Token, "command": o.Display, "cwd": ws.Cwd})
	stopWatch.Do(func() { close(execStarted) })
	if ctx.Err() != nil {
		return res, infra(ctx.Err()), ""
	}
	// The VM announced its reclamation while we were syncing: don't start
	// the command on it; replay the run elsewhere.
	if noticed.Load() && lease.InstanceID != "" {
		if f, ok := lease.Provider.(provider.Fleet); ok {
			NewPool(r, f).Observe(lease.InstanceID, OutDraining, time.Time{})
		}
		return res, infra(fmt.Errorf("%s announced a spot interruption before the command started", lease.InstanceID)), lease.InstanceID
	}
	if crun != nil && !*att.fresh {
		*att.fresh = true // a replay doesn't repeat it
		warnContainerFresh(ws, o, p.Container.Dockerfile)
	}
	prog.dispatch(ex.SetupRun != "")
	if err := sess.Conn.Send(protocol.TypeExec, ex); err != nil {
		return res, infra(err), lostRunner(lease, err)
	}
	// On a reclamation notice, warm replacement capacity elsewhere right away
	// (without holding a slot) so a replay, if needed, doesn't wait for a boot.
	var stopWarm context.CancelFunc
	warmDone := make(chan struct{})
	defer func() {
		if stopWarm != nil {
			stopWarm()
			<-warmDone // withdraw the warming waiter before any retry registers its own
		}
	}()
	onSpot := func() {
		noticed.Store(true)
		reclaimed()
		if lease.InstanceID == "" || !(o.RetryPreempt && att.n < 2 && o.Sync != "none" && !o.PreferLast && !o.SameSlot) {
			return
		}
		f, ok := lease.Provider.(provider.Fleet)
		if !ok {
			return
		}
		// Count the doomed instance as draining right away so the pool may
		// launch its replacement even at max_instances.
		NewPool(r, f).Observe(lease.InstanceID, OutDraining, time.Time{})
		spare := spareRequest(req, lease.InstanceID, att.exclude)
		var sctx context.Context
		sctx, stopWarm = context.WithTimeout(context.WithoutCancel(ctx), r.BootTimeout.Duration+2*time.Minute)
		go func() { defer close(warmDone); EnsureSpare(sctx, spare) }()
	}
	outW := &ui.Rewriter{W: &ui.ClearingWriter{W: o.Stdout, R: o.Rep}, Old: []byte(remoteWS), New: []byte(ws.Root)}
	errW := &ui.Rewriter{W: &ui.ClearingWriter{W: o.Stderr, R: o.Rep}, Old: []byte(remoteWS), New: []byte(ws.Root)}
	afterSetup := func() {
		prog.setupDone()
		o.Rep.Status("%s · running %s", label, o.Display)
	}
	result, err := pumpExec(sess, outW, errW, o, onSpot, afterSetup, prog.started)
	outW.Flush()
	errW.Flush()
	if err != nil {
		return res, infra(fmt.Errorf("lost runner %s during execution: %w", r.Name, err)), lostRunner(lease, err)
	}
	res.ExitCode, res.TimedOut = result.ExitCode, result.TimedOut
	// An interrupt (Ctrl-C, SIGTERM) at any point up to the final result
	// makes the run interrupted: 130, never a success.
	checkCanceled := func() {
		if ctx.Err() != nil && !res.Canceled {
			res.Canceled, res.ExitCode = true, ExitInterrupted
		}
	}
	switch classifyExec(result) {
	case execNotStarted:
		what := "the command"
		if result.SetupFailed {
			what = "[setup] run"
		}
		return res, infra(fmt.Errorf("%s could not be started on %s: %s", what, r.Name, result.StartError)), ""
	case execCanceled:
		res.Canceled, res.ExitCode = true, ExitInterrupted
	case execShutdown:
		// Killed by the VM shutting down (a Spot reclaim): like a lost
		// runner, nothing from this attempt is written back, and on a Spot
		// runner the run is replayed (unless the user interrupted it: then
		// the CLI exits 130).
		return res, shutdownErr(label, result.ExitCode), shutdownRunner(lease)
	}
	checkCanceled()
	// Post-exec work uses its own bounded context: an interrupt that already
	// canceled ctx must not prevent writing back what the command changed.
	post, cancelPost := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelPost()
	// Created files that are ignored here are build outputs the slot keeps;
	// the rest stay pending until written back or removed by the next sync.
	var ignoredCreated map[string]bool
	if m != nil && len(result.Created) > 0 {
		ignoredCreated = settleCreated(post, sess, ws, m, result.Created)
	}
	if m != nil && len(result.Unverified) > 0 {
		o.Rep.Warn("UNVERIFIED", "%d file(s) could not be read on the runner after the run and were not written back: %s", len(result.Unverified), strings.Join(firstN(result.Unverified, 5), ", "))
	}
	if result.SetupFailed && !res.Canceled {
		var msg string
		res.ExitCode, msg = setupFailure(p, o, result)
		res.SetupFailed = true
		o.Rep.Warn("SETUP_FAILED", "%s", msg)
	} else if result.TimedOut {
		o.Rep.Warn("TIMEOUT", "command exceeded %s and was terminated (raise with --timeout)", o.Timeout)
	}
	if result.OOMKilled {
		res.OOMKilled = true
		o.Rep.Warn("OOM_KILLED", "%s", oomMessage(result.MemoryLimitMB))
	}

	// 6. Write-back of mutations.
	if m != nil && o.Sync == "auto" && wantWriteback(o.Writeback, res.ExitCode) &&
		(len(result.Changed)+len(result.Deleted)+len(result.Created)+len(result.GitDirs) > 0) {
		t := time.Now()
		// on-success: an interrupt while the changes are being fetched means
		// the run no longer counts as a success; apply nothing.
		var proceed func() bool
		if o.Writeback == "on-success" {
			proceed = func() bool { return ctx.Err() == nil }
		}
		wb, err := writeback(post, sessionFetch(post, sess), ws, m, result, runID, ignoredCreated, proceed, o.WritebackLimits, o.Rep)
		// Conflicts stashed and files backed up before any failure are
		// reported either way.
		reportConflicts(o.Rep, wb.Stashed, runID)
		reportSafety(o.Rep, wb, runID)
		reportOutsideLinks(o.Rep, wb.OutsideLinks, runID)
		res.BackedUp, res.BackupDir, res.Held = wb.BackedUp, wb.BackupDir, wb.Held
		res.Conflicts = wb.Conflicts
		// So are new files held in the slot: they are there whatever
		// happened next.
		if len(wb.HeldNew) > 0 {
			res.HeldFiles, res.HeldBytes = len(wb.HeldNew), wb.HeldNewBytes
			if err := saveHeld(ws, lastRun, runID, wb.HeldNew); err != nil {
				o.Rep.Warn("WRITEBACK_HELD", "could not record the held files (%v); `spotexec pull <path>` can still fetch them into a directory outside this worktree with --dest", err)
			}
		}
		if err != nil {
			// The command already finished: never replay it. Report what is
			// known so the caller can decide.
			return res, infra(fmt.Errorf("command finished (exit %d) but its file changes could not be written back: %w", res.ExitCode, err)), ""
		}
		res.Mutated, res.Deleted = wb.Applied, wb.Deleted
		o.Rep.Event("sync_out", map[string]any{"mutated_files": nz(wb.Applied), "deleted_files": nz(wb.Deleted), "conflicts": nz(wb.Conflicts), "backed_up_files": nz(wb.BackedUp), "held_deletions": nz(wb.Held), "ignored_created": wb.Ignored, "held_files": len(wb.HeldNew), "held_bytes": wb.HeldNewBytes, "duration_ms": time.Since(t).Milliseconds()})
		if len(wb.Refused) > 0 {
			o.Rep.Warn("WRITEBACK_REFUSED", "ignored %d path(s) the runner reported that may not be written back (a .git directory, inside .git or spotexec's own state in .spotexec, or never uploaded): %s", len(wb.Refused), strings.Join(firstN(wb.Refused, 5), ", "))
		}
		if result.Truncated {
			o.Rep.Warn("WRITEBACK_TRUNCATED", "more than %d new files were created; only the first batch was considered for write-back", 50000)
		}
	} else if m != nil && o.Sync == "auto" {
		o.Rep.Event("sync_out", map[string]any{"mutated_files": []string{}, "conflicts": []string{}, "duration_ms": 0})
	}

	// 7. Explicit artifact pulls.
	pulls := append(append([]string{}, p.Sync.PullArtifacts...), o.Pull...)
	if len(pulls) > 0 {
		n, err := PullArtifacts(post, sess, ws, ws.Root, pulls, nil, false, o.Rep, runID)
		if err != nil {
			o.Rep.Warn("PULL_FAILED", "pulling %v: %v", pulls, err)
		}
		res.Pulled = n
		o.Rep.Event("pull", map[string]any{"patterns": pulls, "files": n})
	}
	checkCanceled()
	res.ExitCode = conflictExit(res, o.FailOnConflict)
	res.Duration = time.Since(start)
	ev := map[string]any{"exit_code": res.ExitCode, "duration_ms": res.Duration.Milliseconds(), "timed_out": res.TimedOut, "canceled": res.Canceled, "runner": r.Name, "slot": res.Slot, "conflict_count": len(res.Conflicts)}
	if res.OOMKilled {
		ev["oom_killed"] = true
	}
	if result.SetupMS > 0 || result.SetupFailed {
		ev["setup_ms"] = result.SetupMS
	}
	if res.SetupFailed {
		ev["setup_failed"] = true
		ev["setup_exit_code"] = result.ExitCode
	}
	o.Rep.Event("result", ev)
	return res, nil, ""
}

// keepLastRun records where this worktree's files now are (never for
// unsynced runs): saved for exec, pull and --same-slot, or only returned
// with DeferLastRun, for a caller choosing among several runs.
func keepLastRun(ws *Workspace, o Options, lr LastRun, res *Result) {
	if o.Sync == "none" || o.PreferLast {
		return
	}
	res.LastRun = &lr
	if !o.DeferLastRun {
		saveLastRun(ws, lr)
	}
}

// reportOutsideLinks warns about symlinks write-back created or retargeted
// to point outside the worktree: local tools and agents reading one read
// whatever it points at, such as a file under ~/.ssh.
func reportOutsideLinks(rep ui.Reporter, links []Link, runID string) {
	for _, l := range links {
		where := "outside the worktree"
		if strings.HasPrefix(l.Target, "/") {
			where = "an absolute path"
		}
		rep.Warn("WRITEBACK_SYMLINK", "the remote command made %s a symlink to %s, %s; anything reading it here reads that instead (delete it if you didn't expect it)", printable(l.Path), printable(l.Target), where)
		ev := map[string]any{"path": l.Path, "target": l.Target}
		if runID != "" {
			ev["run_id"] = runID
		}
		rep.Event("outside_symlink", ev)
	}
}

// printable quotes s when it holds anything a terminal could act on (a
// runner chooses link names and targets).
func printable(s string) string {
	if !utf8.ValidString(s) || strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return strconv.Quote(s)
	}
	return s
}

// reportConflicts says, for each write-back conflict or name collision,
// exactly where to look: the stash is under the git dir, out of sight of
// `git status` and editors.
func reportConflicts(rep ui.Reporter, stashed []Conflict, runID string) {
	for _, c := range stashed {
		diff := conflictDiff(c)
		ev := map[string]any{"path": c.Path, "local": c.Local, "remote": c.Remote, "deleted": c.Deleted, "diff": diff, "run_id": runID}
		if c.Reason != "" {
			ev["reason"] = c.Reason
		}
		switch {
		case c.Reason == ReasonSpotexecConfig && c.Deleted:
			rep.Warn("WRITEBACK_CONFIG", "the remote command deleted %s, spotexec's own config; kept it here, since a runner may not change spotexec's configuration (delete it yourself if that was intended)", printable(c.Path))
		case c.Reason == ReasonSpotexecConfig:
			rep.Warn("WRITEBACK_CONFIG", "the remote command wrote %s, spotexec's own config; not applied, since a runner may not change spotexec's configuration (the next run would load it). Review its version, and copy it over yourself if you want it: %s", printable(c.Path), diff)
		case c.With != "":
			// Two remote names, one local file: say which landed, and spell
			// out names that only differ in Unicode normalisation.
			path, with := c.Path, c.With
			if !strings.EqualFold(path, with) {
				path, with = fmt.Sprintf("%s (%+q)", path, path), fmt.Sprintf("%s (%+q)", with, with)
			}
			rep.Warn("SYNC_COLLISION", "the remote run's %s is the same file as %s here, because this filesystem ignores case and Unicode normalisation in names; kept %s, and saved the remote %s instead: %s", path, with, with, path, diff)
			ev["with"] = c.With
		case c.Reason == ReasonSymlinkParent && c.Deleted:
			rep.Warn("SYNC_CONFLICT", "%s was deleted by the remote command but not here: a parent directory of it is a symlink locally", c.Path)
		case c.Reason == ReasonSymlinkParent:
			rep.Warn("SYNC_CONFLICT", "%s was not written back: a parent directory of it is a symlink locally. The remote version: %s", c.Path, diff)
		case c.Deleted:
			rep.Warn("SYNC_CONFLICT", "%s changed locally during the run, and the remote command deleted it; kept your version (delete it yourself to accept the deletion). Your changes: %s", c.Path, diff)
		default:
			rep.Warn("SYNC_CONFLICT", "%s changed locally during the run; kept your version. The remote version: %s", c.Path, diff)
		}
		rep.Event("conflict", ev)
	}
}

// reportSafety says where write-back saved the uncommitted files it deleted
// or replaced, and which deletions it held back and why.
func reportSafety(rep ui.Reporter, wb WritebackResult, runID string) {
	if len(wb.BackedUp) > 0 {
		rep.Warn("WRITEBACK_BACKUP", "the run deleted or replaced %d file(s) git has no committed copy of; your versions are saved in %s: %s", len(wb.BackedUp), wb.BackupDir, strings.Join(firstN(wb.BackedUp, 5), ", "))
		rep.Event("backup", map[string]any{"dir": wb.BackupDir, "files": wb.BackedUp, "run_id": runID})
	}
	if len(wb.Held) > 0 {
		rep.Warn("WRITEBACK_HELD", "%s; kept %d file(s) here instead of deleting them (delete them yourself if that was intended): %s", wb.HeldWhy, len(wb.Held), strings.Join(firstN(wb.Held, 5), ", "))
		rep.Event("held_deletions", map[string]any{"reason": wb.HeldWhy, "files": wb.Held, "run_id": runID})
	}
}

// conflictDiff is a ready-to-run command comparing your kept file with what
// the remote run produced (/dev/null for a side that doesn't exist).
func conflictDiff(c Conflict) string {
	local, remote := c.Local, c.Remote
	if c.Deleted {
		remote = os.DevNull // Remote is only a marker
	}
	if c.Reason == ReasonSymlinkParent {
		local = os.DevNull // never read through the symlink, out of the worktree
	} else if fi, err := os.Lstat(local); err != nil || fi.IsDir() {
		local = os.DevNull // deleted (or replaced by a directory) locally
	}
	return "git diff --no-index -- " + transport.ShellQuote(local) + " " + transport.ShellQuote(remote)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(append([]string{}, s[:n]...), "…")
	}
	return s
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// validOptions rejects sync modes and write-back policies Run doesn't know,
// rather than guessing what they meant.
func validOptions(o Options) error {
	switch o.Sync {
	case "auto", "in", "none":
	default:
		return fmt.Errorf("sync mode must be auto, in or none (got %q)", o.Sync)
	}
	switch o.Writeback {
	case "always", "on-success", "never":
	default:
		return fmt.Errorf("write-back policy must be always, on-success or never (got %q)", o.Writeback)
	}
	return nil
}

// execOutcome is how a result from the helper is treated.
type execOutcome int

const (
	execCompleted  execOutcome = iota // the command's own exit status
	execCanceled                      // interrupted by the user: exit 130, never a success
	execShutdown                      // killed by the VM shutting down: a lost runner
	execNotStarted                    // the helper could not start it: exit 125
)

// classifyExec decides what a helper result means. A command that finished
// on its own during a Spot interruption's two-minute warning is a
// legitimate result; one the shutdown killed is not.
func classifyExec(r protocol.Result) execOutcome {
	switch {
	case r.StartError != "":
		return execNotStarted
	case r.Shutdown:
		// Even if the user interrupted it too: nothing a killed process left
		// behind is written back (the CLI still exits 130 then).
		return execShutdown
	case r.Canceled:
		return execCanceled
	}
	return execCompleted
}

// spareRequest is what warming capacity for a replay of req asks for, when
// the VM it runs on (doomed) announced its reclamation: room anywhere but
// that VM and those lost before, for the same kind of run. An --exclusive
// run needs a whole VM, so its spare must too: one free slot on a busy VM
// would satisfy a shared spare and launch nothing (#162).
func spareRequest(req AcquireRequest, doomed string, lost map[string]bool) AcquireRequest {
	spare := req
	spare.Exclude = map[string]bool{doomed: true}
	for k := range lost {
		spare.Exclude[k] = true
	}
	spare.Base.Fingerprint = ""
	return spare
}

// shutdownRunner is what to exclude from a replay when the VM shut down
// under a run ("" on a static runner: it is not replayed there).
func shutdownRunner(l *Lease) string {
	if !l.Runner.Ephemeral() {
		return ""
	}
	if l.InstanceID != "" {
		return l.InstanceID
	}
	return "static"
}

// conflictExit is the run's exit code once write-back is done: with
// failOnConflict, a command that succeeded but whose changes didn't all come
// back (a SYNC_CONFLICT kept the local file) exits ExitWritebackConflict. A
// command that failed, timed out or was interrupted keeps its own code.
func conflictExit(res Result, failOnConflict bool) int {
	if failOnConflict && len(res.Conflicts) > 0 && res.ExitCode == 0 && !res.Canceled && !res.TimedOut && !res.SetupFailed {
		return ExitWritebackConflict
	}
	return res.ExitCode
}

func wantWriteback(policy string, code int) bool {
	switch policy {
	case "always":
		return true
	case "on-success":
		return code == 0
	}
	return false // never, or anything unknown
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// strayFiles picks, from the files earlier runs created in the slot and never
// wrote back, the ones the next sync must remove: everything this worktree
// doesn't ignore (ignored build outputs stay, like dependency caches).
func strayFiles(ctx context.Context, ws *Workspace, m *manifest.Manifest, pending []string) ([]string, error) {
	var stray, check, gitDirs, probes []string
	for _, p := range pending {
		if _, ok := m.ByPath[p]; ok {
			continue // synced over by this run
		}
		// A .git directory a run made goes unless its directory is ignored
		// here (a build output's own clone stays, like the output).
		if probe, ok := gitDirProbe(ws, m, p); ok {
			gitDirs, probes = append(gitDirs, p), append(probes, probe)
			continue
		}
		// Never a file of ours: an invalid or reserved path, or one under a
		// synced file or a local symlink or file (git can't judge those).
		if _, err := fsid.SafeJoin(ws.Root, p); err != nil || reservedPath(p, ws.Git) || underSyncedFile(m, p) {
			stray = append(stray, p)
			continue
		}
		check = append(check, p)
	}
	if len(check)+len(probes) == 0 {
		return stray, nil
	}
	ign, err := m.CheckIgnored(ctx, append(append([]string{}, check...), probes...))
	if err != nil {
		return nil, err
	}
	for _, p := range check {
		if !ign[p] {
			stray = append(stray, p)
		}
	}
	for i, p := range gitDirs {
		if !ign[probes[i]] {
			stray = append(stray, p)
		}
	}
	return stray, nil
}

// gitDirProbe returns, for p a .git directory below the root that a run
// created, the path whose being ignored here says whether p is: p's
// directory, with a trailing slash so it is asked about as a directory (the
// built-in excludes match .git itself, whatever surrounds it). false when p
// is no such directory, or its directory can't be judged here (a local
// symlink or file, or beyond one): it is never kept.
func gitDirProbe(ws *Workspace, m *manifest.Manifest, p string) (string, bool) {
	dir, name := path.Split(p)
	if name != ".git" || dir == "" {
		return "", false
	}
	d := strings.TrimSuffix(dir, "/")
	full, err := fsid.SafeJoin(ws.Root, d)
	if err != nil || reservedPath(d, ws.Git) || underSyncedFile(m, p) {
		return "", false
	}
	if fi, err := os.Lstat(full); err == nil && !fi.IsDir() || err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	return dir, true
}

// underSyncedFile reports whether a parent of p is a synced file or symlink.
func underSyncedFile(m *manifest.Manifest, p string) bool {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if _, ok := m.ByPath[d]; ok {
			return true
		}
	}
	return false
}

// settleCreated tells the helper which files the run created are ignored
// here, and returns that set (nil if it couldn't be determined: write-back
// then checks again and reports the error). A reserved path (spotexec's own
// state in a non-Git .spotexec/) is ignored but never settled: write-back
// refuses it, and it stays pending so the next sync removes it.
func settleCreated(ctx context.Context, s *transport.Session, ws *Workspace, m *manifest.Manifest, created []protocol.Entry) map[string]bool {
	paths := make([]string, 0, len(created))
	for _, c := range created {
		if _, err := fsid.CleanRel(c.Path); err == nil {
			paths = append(paths, c.Path)
		}
	}
	ign, err := m.CheckIgnored(ctx, paths)
	if err != nil {
		return nil
	}
	var keep []string
	for _, p := range paths {
		if ign[p] && !reservedPath(p, ws.Git) {
			keep = append(keep, p)
		}
	}
	if len(keep) > 0 {
		_ = s.Conn.Send(protocol.TypeSettle, protocol.Settle{Ignored: keep})
	}
	return ign
}

// syncIn sends the manifest and streams whatever the helper reports missing.
// remove lists files earlier runs left in the slot that must go.
func syncIn(ctx context.Context, s *transport.Session, m *manifest.Manifest, protect, remove []string, o Options) (protocol.Synced, error) {
	var syn protocol.Synced
	if err := s.Conn.Send(protocol.TypeManifest, protocol.Manifest{Fingerprint: m.Fingerprint, Entries: m.Entries, Protect: protect, Remove: remove}); err != nil {
		return syn, err
	}
	env, err := s.Expect(ctx, protocol.TypeNeed)
	if err != nil {
		return syn, err
	}
	var need protocol.Need
	if err := env.Unmarshal(&need); err != nil {
		return syn, err
	}
	var uploaded map[string]protocol.Entry // what we actually sent, hashed here
	// The runner may only ask for files in the manifest we sent it: never
	// anything else on this machine (a compromised runner would otherwise
	// read ~/.aws/credentials through "../..").
	asked := map[string]bool{}
	for _, p := range need.Paths {
		if _, ok := m.ByPath[p]; !ok || asked[p] {
			return syn, fmt.Errorf("runner asked for %q, which is not in the working set", p)
		}
		asked[p] = true
	}
	if len(need.Paths) > 0 {
		if need.Bytes > AbortBytes && !o.ForceLargeSync {
			return syn, fmt.Errorf("refusing to transfer %s (limit %s); add excludes or pass --force-large-sync", ui.Bytes(need.Bytes), ui.Bytes(AbortBytes))
		}
		if need.Bytes > WarnBytes {
			o.Rep.Warn("LARGE_SYNC", "transferring %s", ui.Bytes(need.Bytes))
		}
		bw := &protocol.BlobWriter{C: s.Conn}
		uploaded = map[string]protocol.Entry{}
		zw, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
		tw := tar.NewWriter(zw)
		werr := func() error {
			for _, p := range need.Paths {
				full, err := fsid.SafeJoin(m.Root, p) // no symlinked parent directories
				if err != nil {
					return err
				}
				sent, err := addLocalTar(tw, full, p)
				if err != nil {
					return err
				}
				uploaded[p] = sent
			}
			if err := tw.Close(); err != nil {
				return err
			}
			return zw.Close()
		}()
		if err := bw.CloseWithError(werr); err != nil {
			return syn, err
		}
		if werr != nil {
			_, _ = s.Expect(ctx, protocol.TypeSynced)
			return syn, werr
		}
	}
	env, err = s.Expect(ctx, protocol.TypeSynced)
	if err != nil {
		return syn, err
	}
	if err := env.Unmarshal(&syn); err != nil {
		return syn, err
	}
	// Files that changed while we were reading them: the bytes that actually
	// landed remotely are the write-back baseline. Taken from what we hashed
	// while sending, never from the runner's report (it could otherwise
	// plant entries for files we never uploaded).
	for p, e := range uploaded {
		if cur := m.ByPath[p]; cur.ID != e.ID || cur.Mode != e.Mode {
			m.Update(e)
		}
	}
	return syn, nil
}

// DepsLockfiles expands the deps.lockfiles patterns against the synced
// regular files in m (see depskey.Expand). It returns the concrete list to
// hash and the patterns that matched nothing.
func DepsLockfiles(m *manifest.Manifest, entries []string) ([]string, []string, error) {
	var files []string
	if m != nil {
		files = make([]string, 0, len(m.ByPath))
		for p, e := range m.ByPath {
			if e.Mode != fsid.ModeSymlink {
				files = append(files, p)
			}
		}
	}
	return depskey.Expand(entries, files)
}

// addLocalTar adds one file and returns the entry for exactly what was sent.
func addLocalTar(tw *tar.Writer, full, name string) (protocol.Entry, error) {
	fi, err := os.Lstat(full)
	if err != nil {
		return protocol.Entry{}, fmt.Errorf("%s vanished during sync; re-run", name)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(full)
		if err != nil {
			return protocol.Entry{}, err
		}
		e := protocol.Entry{Path: name, ID: fsid.Bytes([]byte(t)), Mode: fsid.ModeSymlink, Size: int64(len(t))}
		return e, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: t, Mode: 0o777})
	}
	if !fi.Mode().IsRegular() {
		return protocol.Entry{}, fmt.Errorf("%s is no longer a regular file; re-run", name)
	}
	// Streamed, never read whole, and hashed as it goes: the entry is for
	// exactly the bytes in the tar.
	f, err := os.Open(full)
	if err != nil {
		return protocol.Entry{}, err
	}
	defer f.Close()
	if fi, err = f.Stat(); err != nil {
		return protocol.Entry{}, err
	}
	if !fi.Mode().IsRegular() {
		return protocol.Entry{}, fmt.Errorf("%s is no longer a regular file; re-run", name)
	}
	mode, emode := int64(0o644), fsid.ModeFile
	if fi.Mode()&0o111 != 0 {
		mode, emode = 0o755, fsid.ModeExec
	}
	size := fi.Size()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: size, Mode: mode, ModTime: fi.ModTime()}); err != nil {
		return protocol.Entry{}, err
	}
	// A file that grows meanwhile is cut at the header's size (the hash
	// covers what was sent); one that shrinks can't fill the entry.
	id, err := fsid.Reader(io.TeeReader(io.LimitReader(f, size), tw), size)
	if err != nil {
		return protocol.Entry{}, fmt.Errorf("sending %s: %w; re-run", name, err)
	}
	return protocol.Entry{Path: name, ID: id, Mode: emode, Size: size}, nil
}

func runDeps(ctx context.Context, s *transport.Session, p *config.Project, r *config.Runner, o Options, label string, ws *Workspace, lease *Lease, crun *protocol.ContainerRun, imageID string, m *manifest.Manifest) (protocol.DepsResult, error) {
	if len(p.Deps.Lockfiles) == 0 {
		return protocol.DepsResult{}, errors.New("deps.install is set but deps.lockfiles is empty")
	}
	// The runner hashes exactly this list: it never expands patterns itself,
	// so both sides key the cache on the same files.
	lockfiles, unmatched, err := DepsLockfiles(m, p.Deps.Lockfiles)
	if err != nil {
		return protocol.DepsResult{}, err
	}
	for _, pat := range unmatched {
		o.Rep.Warn("DEPS_LOCKFILES", "deps.lockfiles pattern %q matches no synced file, so it adds nothing to the dependency cache key", pat)
	}
	if len(lockfiles) == 0 {
		return protocol.DepsResult{}, errNoLockfiles
	}
	o.Rep.Status("%s · checking dependency cache", label)
	t := time.Now()
	env := map[string]string{}
	for _, k := range p.EnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range p.Env {
		env[k] = v
	}
	req := protocol.Deps{
		Lockfiles: lockfiles, Install: p.Deps.Install, CacheDirs: p.Deps.CacheDirs, Hydrate: p.Deps.Hydrate,
		Setup: r.RemotePathSetup, Env: env, TimeoutMS: p.Deps.Timeout.Milliseconds(),
		Toolchain: toolchainFor(r, crun),
		Container: crun,
		ImageID:   imageID,
	}
	image := imageID
	if image == "" && crun != nil {
		image = crun.Image
	}
	// Shared cache: sign URLs for the key the runner will compute. Any
	// failure just means installing without the cache.
	// The cache lives in the instance's own region (multi-region fleets).
	var resign func(context.Context) (string, string, error)
	if dc, ok := provider.ForInstance(ctx, lease.Provider, lease.InstanceID).(provider.DepsCache); ok {
		key, err := depskey.Key(ws.Root, depskey.Spec{Lockfiles: lockfiles, Install: p.Deps.Install, CacheDirs: p.Deps.CacheDirs, Image: image, Toolchain: req.Toolchain, OS: s.Hello.OS, Arch: s.Hello.Arch})
		if err == nil {
			sign := func(ctx context.Context) (string, string, error) {
				cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				return dc.CacheURLs(cctx, ws.RepoID, key)
			}
			get, put, err := sign(ctx)
			if err != nil {
				o.Rep.Warn("DEPS_CACHE", "shared dependency cache unavailable: %v", err)
			} else if get != "" {
				req.CacheKey, req.CacheGet, req.CachePut = key, get, put
				resign = sign
			}
		}
	}
	onEvent := func(ev protocol.Event) {
		if s.OnEvent != nil {
			s.OnEvent(ev) // records interruption notices for the pre-exec check
		} else {
			o.Rep.Event(ev.Name, map[string]any{"message": ev.Message})
		}
	}
	log := newDepsLog(depsLogDir(), label)
	dr, shared, upload, err := fetchDeps(ctx, sessionRPC{s}, onEvent, o, label, log, req, resign, p.Deps.Timeout.Duration+shareSlack)
	log.Close()
	// The install's status (and plain mode's heartbeat of it) ends here:
	// the command's own phase starts next.
	o.Rep.Clear()
	ms := time.Since(t).Milliseconds()
	if err != nil {
		tail, truncated := log.Tail(depsTailLines)
		var re *transport.RemoteError
		if errors.As(err, &re) {
			// The helper reported the failure: no retry follows.
			ev := map[string]any{"code": re.Code, "message": re.Message, "duration_ms": ms}
			if log.Bytes() > 0 {
				ev["output_tail"], ev["output_truncated"] = tail, truncated
			}
			if p := log.Path(); p != "" {
				ev["log"] = p
			}
			o.Rep.Event("deps_failed", ev)
		}
		if log.Bytes() == 0 || ctx.Err() != nil {
			return dr, err // nothing to show, or this run was interrupted
		}
		// A runner lost mid-install keeps what it printed, too.
		return dr, &DepsError{Err: err, Tail: tail, Truncated: truncated, Log: log.Path()}
	}
	ev := map[string]any{"key": dr.Key, "built": dr.Built, "restored": dr.Restored, "uploading": dr.Uploaded,
		"hydrated": dr.Hydrated, "method": dr.Method, "duration_ms": ms}
	if p := log.Path(); p != "" {
		ev["log"] = p
	}
	if shared.waited > 0 {
		ev["waited_ms"] = shared.waited.Milliseconds()
	}
	if upload != nil {
		ev["uploaded"], ev["upload_ms"] = upload.OK, upload.MS
	}
	o.Rep.Event("deps", ev)
	return dr, nil
}

// fetchDeps sends the deps request. With a shared cache, which may lack
// this key, of the runs on this machine needing it now (the parts of a
// task, concurrent commands) one installs it and uploads it, and the rest
// wait for it and restore it (#139): each run first probes (restore only),
// and on a miss queues for the key's lock. resign signs fresh cache URLs
// (nil: no shared cache): presigned URLs expire, and a run may have waited
// a long time for another's install.
func fetchDeps(ctx context.Context, c rpc, onEvent func(protocol.Event), o Options, label string, log *depsLog, req protocol.Deps, resign func(context.Context) (string, string, error), limit time.Duration) (dr protocol.DepsResult, shared shareOutcome, upload *protocol.ShareDone, err error) {
	signed := time.Now()
	ask := func(req protocol.Deps) (protocol.DepsResult, error) {
		if err := c.send(protocol.TypeDeps, req); err != nil {
			return protocol.DepsResult{}, err
		}
		return consumeDeps(ctx, c.next, onEvent, o.Rep, label, log)
	}
	if req.CacheGet == "" || req.CachePut == "" {
		dr, err = ask(req)
		return dr, shared, nil, err
	}
	holder := ""
	probe := req
	probe.Probe = true
	sh := sharer{
		lock:  shareLockFor("deps", stripQuery(req.CacheGet)),
		who:   shareWho(o, label),
		limit: limit,
		probe: func(context.Context) (bool, error) {
			r, err := ask(probe)
			if err != nil || r.Missing {
				return false, err
			}
			dr = r
			return true, nil
		},
		build: func(ctx context.Context) error {
			if resign != nil && time.Since(signed) > resignAfter {
				if get, put, err := resign(ctx); err == nil && get != "" {
					req.CacheGet, req.CachePut = get, put
				}
			}
			dr, err = ask(req)
			return err
		},
		publish: func(ctx context.Context) error {
			if !dr.Built || !dr.Uploaded {
				return nil
			}
			o.Rep.Status("%s · uploading the dependency cache, for other runs to restore", label)
			d, err := awaitShared(ctx, c, protocol.ShareDeps, shareTransfer)
			if err != nil {
				return err
			}
			if !d.OK {
				o.Rep.Warn("DEPS_CACHE", "uploading the dependency cache failed (%s): runs waiting for it install the dependencies themselves", d.Message)
			}
			upload = &d
			return nil
		},
		waiting: func(h string) {
			holder = h
			o.Rep.Status("%s · waiting for %s to install the dependencies", label, h)
			o.Rep.Event("deps_wait", map[string]any{"key": req.CacheKey, "holder": h,
				"message": "waiting for " + h + " to install the same dependencies, to restore them from the shared cache"})
		},
		sess: c,
	}
	shared, err = sh.run(ctx)
	if shared.timedOut && err == nil {
		o.Rep.Warn("DEPS_WAIT", "waited %s for %s to install the dependencies; installed them here instead", ui.Dur(shared.waited), holder)
	}
	return dr, shared, upload, err
}

// shareWho names this run to the runs waiting on it: its part of the task
// (when it has a name) and where it runs.
func shareWho(o Options, label string) string {
	if o.Label != "" {
		return o.Label + " on " + label
	}
	return label
}

// SetupFor is the [setup] command a run starts with ("" when none applies)
// and its time limit: setup.timeout, or the command's own timeout.
func SetupFor(p *config.Project, o Options) (string, time.Duration) {
	if o.NoSetup || strings.TrimSpace(p.Setup.Run) == "" {
		return "", 0
	}
	if p.Setup.Timeout.Duration > 0 {
		return p.Setup.Run, p.Setup.Timeout.Duration
	}
	return p.Setup.Run, o.Timeout
}

// setupFailure is the exit code and SETUP_FAILED message for a run whose
// [setup] failed. The setup's own exit code is not passed on: a setup that
// exits 125 or 130 must not look like a spotexec failure or an interrupt.
func setupFailure(p *config.Project, o Options, r protocol.Result) (int, string) {
	if r.TimedOut {
		_, t := SetupFor(p, o)
		return ExitTimeout, fmt.Sprintf("[setup] run exceeded %s and was terminated (raise setup.timeout); the command did not run", t)
	}
	return 1, fmt.Sprintf("[setup] run failed (exit %d); the command did not run. Its output is above; skip the setup with --no-setup or setup = false on a task", r.ExitCode)
}

// pumpExec streams output until the result arrives, forwarding interrupts.
func pumpExec(s *transport.Session, stdout, stderr io.Writer, o Options, onSpot, afterSetup, onStarted func()) (protocol.Result, error) {
	var res protocol.Result
	type fr struct {
		f   protocol.Frame
		err error
	}
	frames := make(chan fr, 1)
	go func() {
		for {
			f, err := s.Next(context.Background())
			frames <- fr{f, err}
			if err != nil {
				return
			}
			if f.Kind == protocol.KindMsg {
				if env, e := protocol.Decode(f); e == nil && (env.Type == protocol.TypeResult || env.Type == protocol.TypeError) {
					return
				}
			}
		}
	}()
	// A silent helper is the session watchdog's to give up on (after any
	// grace for a VM that is still running): Next then fails.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var canceledAt time.Time
	cancelGrace := o.Grace + 30*time.Second
	for {
		select {
		case <-tick.C:
			if !canceledAt.IsZero() && time.Since(canceledAt) > cancelGrace {
				s.Abort()
				return res, &transport.ConnError{Target: "helper", Msg: "runner did not confirm cancellation"}
			}
			continue
		case <-o.Interrupt:
			o.Rep.Event("cancel", map[string]any{"reason": "interrupt"})
			if canceledAt.IsZero() {
				canceledAt = time.Now()
			}
			go func() { _ = s.Conn.Send(protocol.TypeCancel, protocol.Cancel{Reason: "interrupt"}) }()
			continue
		case x := <-frames:
			if x.err != nil {
				return res, x.err
			}
			switch x.f.Kind {
			case protocol.KindStdout:
				stdout.Write(x.f.Data)
				continue
			case protocol.KindStderr:
				stderr.Write(x.f.Data)
				continue
			}
			env, err := protocol.Decode(x.f)
			if err != nil {
				return res, err
			}
			switch env.Type {
			case protocol.TypeStarted:
				if onStarted != nil {
					onStarted()
				}
			case protocol.TypeEvent:
				var ev protocol.Event
				_ = env.Unmarshal(&ev)
				o.Rep.Event(ev.Name, map[string]any{"message": ev.Message})
				if ev.Name == "setup_done" && afterSetup != nil {
					afterSetup()
				}
				if ev.Name == "spot_interruption" {
					o.Rep.Warn("SPOT_INTERRUPTION", "%s; warming a replacement", ev.Message)
					if onSpot != nil {
						onSpot()
						onSpot = nil
					}
				}
			case protocol.TypeError:
				var em protocol.ErrorMsg
				_ = env.Unmarshal(&em)
				return res, &transport.RemoteError{ErrorMsg: em}
			case protocol.TypeResult:
				err := env.Unmarshal(&res)
				return res, err
			}
		}
	}
}

// PressureWindow: memory pressure the runner reported this recently before
// it was lost counts as the reason it was lost (heartbeats come every 20s,
// and silence lasts SilenceLimit plus any SilentGrace before a loss).
var PressureWindow = 5 * time.Minute

// SilentGrace is how much longer a session waits for a silent helper whose
// VM the cloud still reports as running: a VM short of memory can stall
// for minutes until the kernel's OOM killer acts, and then recovers, and
// giving up on it throws away every slot's run there.
var SilentGrace = 3 * time.Minute

// waitIfRunning returns the session's silence hook for a fleet lease: if
// the VM is still running, wait SilentGrace longer before giving up on it.
func waitIfRunning(lease *Lease, rep ui.Reporter) func(time.Duration) time.Duration {
	return func(silent time.Duration) time.Duration {
		if !instanceRunning(lease) {
			return 0
		}
		why := ""
		if p := lease.Sess.MemoryPressure(PressureWindow); p != "" {
			why = "; it last reported memory pressure (" + p + ")"
		}
		rep.Warn("RUNNER_SILENT", "%s hasn't answered for %s but is still running%s; waiting up to %s more for it", lease.InstanceID, silent.Round(time.Second), why, SilentGrace)
		return SilentGrace
	}
}

// instanceRunning asks the cloud (the instance's own region, for a
// multi-region fleet) whether a lease's VM is running, within 10s; false
// when it can't tell.
func instanceRunning(lease *Lease) bool {
	if lease.InstanceID == "" || lease.Provider == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, ok := provider.ForInstance(ctx, lease.Provider, lease.InstanceID).(provider.Fleet)
	if !ok {
		if f, ok = lease.Provider.(provider.Fleet); !ok {
			return false
		}
	}
	list, err := f.List(ctx)
	if err != nil {
		return false
	}
	for _, in := range list {
		if in.ID == lease.InstanceID {
			return in.State == provider.StateRunning
		}
	}
	return false
}

// oomMessage explains an OOM_KILLED result; limitMB is the memory pool's
// cap, 0 when the container wasn't in the pool: a job alone on its VM
// (--exclusive, a single slot), a host without the pool, or a memory
// option in container.options. Then the kill came from a limit
// container.options set (on the container, or on a --cgroup-parent), or
// from the whole VM running out.
func oomMessage(limitMB int64) string {
	if limitMB <= 0 {
		return "the command ran out of memory: the kernel killed a process in its container, which either reached a memory limit set in container.options (its own, or its --cgroup-parent's) or used up the memory of the whole VM. Raise that limit, use a larger instance type, or make it use less"
	}
	return fmt.Sprintf("the command ran out of memory: the jobs sharing the runner reached the memory set aside for them (%.1f GiB), and the kernel killed the largest process, in this command's container. Use --exclusive to have the whole VM, fewer max_slots, or make it use less", float64(limitMB)/1024)
}

// cpuShare is the number of CPUs one job should use on a host.
func cpuShare(cpus, slots int, exclusive bool) int {
	if cpus < 1 {
		return 1
	}
	if exclusive || slots < 1 {
		return cpus
	}
	return max(1, cpus/slots)
}

// markLost records a fleet instance that dropped mid-run as draining, so the
// pool stops counting it toward max_instances and may launch a replacement.
func markLost(cfg *config.Config, runner, id string) {
	r, ok := cfg.Global.Runners[runner]
	if !ok {
		return
	}
	p, err := provider.New(r)
	if err != nil {
		return
	}
	if f, ok := p.(provider.Fleet); ok {
		NewPool(r, f).Observe(id, OutDraining, time.Time{})
	}
}

// checkLocal is what a run refuses from the local files alone, so Run
// checks it before acquiring a runner: a working set over AbortFiles, a
// container built from a Dockerfile whose build inputs aren't synced (exec,
// PreferLast, reuses the image this worktree last built instead), and a
// dependency install whose deps.lockfiles match no synced file, leave the
// repository, or name a file the runner can't have (runDeps).
// m is nil when the run doesn't sync.
func checkLocal(ctx context.Context, m *manifest.Manifest, p *config.Project, ws *Workspace, o Options) error {
	if m != nil && len(m.Entries) > AbortFiles && !o.ForceLargeSync {
		return fmt.Errorf("refusing to sync %d files (limit %d); add excludes to .spotexec.toml or pass --force-large-sync", len(m.Entries), AbortFiles)
	}
	if c := p.Container; c.Dockerfile != "" && c.Image == "" && !o.PreferLast {
		if _, _, _, err := containerInputs(m, c.Dockerfile, c.Context, readFrom(ws.Root), false); err != nil {
			return err
		}
	}
	if p.Deps.Install != "" && !o.NoDeps && m != nil {
		return checkDeps(ctx, m, p.Deps, ws.Root)
	}
	return nil
}

// CheckDeps checks deps.lockfiles against the working set a synced run from
// ws would send, as that run does before acquiring a runner (for doctor).
func CheckDeps(ctx context.Context, p *config.Project, ws *Workspace) error {
	m, err := manifest.Build(ctx, ManifestOptions(ws.Root, p))
	if err != nil {
		return fmt.Errorf("enumerating files: %w", err)
	}
	return checkDeps(ctx, m, p.Deps, ws.Root)
}

// checkDeps refuses deps.lockfiles every install of a run syncing m would
// fail on: none matching a synced file, an entry leaving the repository, or
// a literal entry missing here (missingLockfile).
func checkDeps(ctx context.Context, m *manifest.Manifest, d config.Deps, root string) error {
	lockfiles, _, err := DepsLockfiles(m, d.Lockfiles)
	if err != nil {
		return err
	}
	if len(lockfiles) == 0 {
		return errNoLockfiles
	}
	// The runner keys the cache on the files in its workspace
	// (depskey.Key), so a literal entry that escapes the repository
	// fails every install.
	for _, lf := range lockfiles {
		if _, err := fsid.CleanRel(lf); err != nil {
			return fmt.Errorf("deps.lockfiles: %w", err)
		}
	}
	return missingLockfile(ctx, m, d, root)
}

// missingLockfile refuses a literal deps.lockfiles entry that is missing
// here: the runner gets the local tree, so every install would fail to read
// it. One the slot may still have is left to the runner: under a symlinked
// directory (it resolves there), or in a cache_dirs directory or a path this
// worktree ignores (kept in the slot from earlier runs, never synced).
func missingLockfile(ctx context.Context, m *manifest.Manifest, d config.Deps, root string) error {
	var missing []string
	for _, lf := range d.Lockfiles {
		if depskey.IsPattern(lf) {
			continue
		}
		rel, err := fsid.CleanRel(lf)
		if err != nil {
			return fmt.Errorf("deps.lockfiles: %w", err)
		}
		_, err = os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err == nil || !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			continue // there, or unknown: the runner says
		}
		if underSymlink(root, rel) || inCacheDir(rel, d.CacheDirs) {
			continue
		}
		if underSyncedFile(m, rel) {
			// The next sync clears anything below a synced file.
			return errMissingLockfile(rel)
		}
		missing = append(missing, rel)
	}
	if len(missing) == 0 {
		return nil
	}
	ign, err := m.CheckIgnored(ctx, missing)
	if err != nil {
		return nil // unknown: the runner says
	}
	for _, rel := range missing {
		if !ign[rel] {
			return errMissingLockfile(rel)
		}
	}
	return nil
}

// underSymlink reports whether a directory on rel's path is a symlink.
func underSymlink(root, rel string) bool {
	dir := root
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		dir = filepath.Join(dir, s)
		fi, err := os.Lstat(dir)
		if err != nil {
			return false
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// inCacheDir reports whether rel is inside a directory matching one of the
// cache_dirs patterns (single-level globs, as the runner expands them).
func inCacheDir(rel string, patterns []string) bool {
	segs := strings.Split(rel, "/")
	for _, p := range patterns {
		ps := strings.Split(strings.Trim(p, "/"), "/")
		if len(segs) <= len(ps) {
			continue
		}
		in := true
		for i, pat := range ps {
			if ok, _ := path.Match(pat, segs[i]); !ok {
				in = false
				break
			}
		}
		if in {
			return true
		}
	}
	return false
}

var errNoLockfiles = errors.New("deps.lockfiles matches no synced file: the dependency cache needs at least one to key on")

func errMissingLockfile(rel string) error {
	return fmt.Errorf("deps.lockfiles: %s doesn't exist, so every dependency install would fail; create it or fix the path", rel)
}

// Preflight checks, without a runner, what a run from ws with options o
// (its Sync, PreferLast, NoDeps and ForceLargeSync) would refuse from the
// local files alone: it builds the working set that run would sync, as Run
// does, and checks it as Run does (checkLocal). Runner availability,
// credentials, the transfer size, and the image build and dependency
// install themselves are not checked.
func Preflight(ctx context.Context, p *config.Project, ws *Workspace, o Options) error {
	var m *manifest.Manifest
	if o.Sync != "none" {
		var err error
		if m, err = manifest.Build(ctx, ManifestOptions(ws.Root, p)); err != nil {
			return fmt.Errorf("enumerating files: %w", err)
		}
	}
	return checkLocal(ctx, m, p, ws, o)
}

// readFrom loads a file of the working set rooted at root.
func readFrom(root string) func(rel string) ([]byte, error) {
	return func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	}
}

// imageTag hashes the runner platform, the Dockerfile, a companion
// <Dockerfile>.dockerignore, and every synced file in the build context
// that .dockerignore keeps (with any corrections from the upload), so a
// built image is reused until one of them changes, and never across CPU
// architectures. read loads a synced file's content (the ignore rules).
func imageTag(m *manifest.Manifest, dockerfile, buildCtx, platform string, read func(rel string) ([]byte, error)) (string, error) {
	df, buildCtx, ign, err := containerInputs(m, dockerfile, buildCtx, read, true)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "platform\x00%s\x00dockerfile\x00%s\x00%s\x00ctx\x00%s\x00", platform, dockerfile, df.ID, buildCtx)
	if ign, ok := m.ByPath[dockerfile+".dockerignore"]; ok {
		fmt.Fprintf(h, "dockerignore\x00%s\x00", ign.ID)
	}
	paths := make([]string, 0, len(m.ByPath))
	for p := range m.ByPath {
		if buildCtx == "" || p == buildCtx || strings.HasPrefix(p, buildCtx+"/") {
			inner := strings.TrimPrefix(strings.TrimPrefix(p, buildCtx), "/")
			if ign.Excluded(inner) {
				continue
			}
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		e := m.ByPath[p]
		fmt.Fprintf(h, "%s\x00%o\x00%s\n", e.Path, e.Mode, e.ID)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// containerInputs checks a Dockerfile build's inputs in the working set m,
// as the runner will build from its synced copy: the Dockerfile is a synced
// regular file, the context stays inside the repository, and the
// .dockerignore that applies is a synced regular file that read loads and
// that parses. synced says m is what the upload left on the runner: then
// the rules read must be exactly that copy. Before the sync, m is only the
// scan, and an edit since is what the upload will send. It returns the
// Dockerfile's entry, the normalised context ("" for the root) and the
// ignore rules.
func containerInputs(m *manifest.Manifest, dockerfile, buildCtx string, read func(rel string) ([]byte, error), synced bool) (protocol.Entry, string, *dockerignore.Matcher, error) {
	if m == nil {
		return protocol.Entry{}, "", nil, errors.New("container.dockerfile needs a synced workspace (not --sync none)")
	}
	df, ok := m.ByPath[dockerfile]
	if !ok {
		if read != nil {
			if _, err := read(dockerfile); err == nil {
				// The Dockerfile syncs whether or not Git tracks it, so only
				// an exclude can keep it out.
				if m.Matcher != nil && m.Matcher.Match(dockerfile, false) {
					return df, "", nil, fmt.Errorf("container.dockerfile %q exists but is not synced: [sync].exclude, .spotexecignore or deps.cache_dirs excludes it; remove that pattern", dockerfile)
				}
				return df, "", nil, fmt.Errorf("container.dockerfile %q exists but is not synced: spotexec never syncs that path; move the Dockerfile and point container.dockerfile at it", dockerfile)
			}
		}
		return df, "", nil, fmt.Errorf("container.dockerfile %q is not in the synced working set", dockerfile)
	}
	if df.Mode == fsid.ModeSymlink {
		return df, "", nil, fmt.Errorf("container.dockerfile %q must be a regular file, not a symlink", dockerfile)
	}
	// Normalised exactly as the helper does (normContext).
	if buildCtx == "" {
		buildCtx = path.Dir(dockerfile)
	}
	buildCtx = strings.TrimSuffix(strings.TrimPrefix(buildCtx, "./"), "/")
	if buildCtx == "." {
		buildCtx = ""
	}
	if buildCtx != "" {
		c, err := fsid.CleanRel(buildCtx)
		if err != nil {
			return df, "", nil, fmt.Errorf("container.context %q must stay inside the repository", buildCtx)
		}
		buildCtx = c
	}
	// The same rules file the helper stages with (see stageContext).
	var rules []byte
	for _, cand := range dockerignore.Source(dockerfile, buildCtx) {
		e, ok := m.ByPath[cand]
		if !ok {
			continue
		}
		if e.Mode == fsid.ModeSymlink {
			// It could resolve differently on the runner than here.
			return df, "", nil, fmt.Errorf("%s is a symlink; make it a regular file", cand)
		}
		if read == nil {
			return df, "", nil, fmt.Errorf("cannot read %s", cand)
		}
		b, err := read(cand)
		if err != nil {
			return df, "", nil, fmt.Errorf("reading %s: %w", cand, err)
		}
		// The rules must be exactly what was synced (the helper stages with
		// that copy), or the tag would not describe the built image.
		if synced && fsid.Bytes(b) != e.ID {
			return df, "", nil, fmt.Errorf("%s changed after it was synced; run again", cand)
		}
		rules = b
		break
	}
	ign, err := dockerignore.New(rules)
	if err != nil {
		return df, "", nil, fmt.Errorf("parsing .dockerignore: %w", err)
	}
	return df, buildCtx, ign, nil
}

// prepareImage makes the project's container image available on the runner
// (pull, or build once per content hash, shared through the provider's
// registry when it has one) and returns the local reference to run.
func prepareImage(ctx context.Context, s *transport.Session, p *config.Project, o Options, label string, ws *Workspace, lease *Lease, m *manifest.Manifest) (string, string, error) {
	c := p.Container
	req := protocol.Image{TimeoutMS: c.Timeout.Milliseconds(), WaitPush: o.WaitPush}
	if c.Image != "" {
		req.Ref = c.Image
	} else if o.PreferLast {
		// exec (no sync): reuse the image this worktree last built here.
		lr, err := LoadLastRun(ws)
		if err != nil || lr.Image == "" {
			return "", "", errors.New("no previously built container image for this worktree; use `spotexec run` first")
		}
		req.Ref = lr.Image
	} else {
		tag, err := imageTag(m, c.Dockerfile, c.Context, s.Hello.OS+"/"+s.Hello.Arch, readFrom(ws.Root))
		if err != nil {
			return "", "", err
		}
		req.Dockerfile, req.Context, req.Tag = c.Dockerfile, c.Context, tag
		if reg, ok := provider.ForInstance(ctx, lease.Provider, lease.InstanceID).(provider.ImageRegistry); ok {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			auth, err := reg.RegistryFor(cctx, ws.RepoID)
			cancel()
			if err != nil {
				o.Rep.Warn("IMAGE_REGISTRY", "image registry unavailable (building on this runner only): %v", err)
			} else {
				req.Registry = auth
			}
		}
	}
	o.Rep.Status("%s · preparing container image", label)
	t := time.Now()
	errW := &ui.ClearingWriter{W: o.Stderr, R: o.Rep}
	log := newDepsLog(imageLogDir(), label)
	r, shared, push, err := fetchImage(ctx, sessionRPC{s}, o, label, errW, log, newImageMemo(ws.RepoID), req, c.Timeout.Duration+shareSlack)
	log.Close()
	if err != nil {
		var be *ImageBuildError
		if errors.As(err, &be) {
			ev := map[string]any{"code": protocol.CodeImageFailed, "message": be.Message, "tag": be.Tag, "cached": be.Cached,
				"built_by": be.Who, "failed_at": be.At.UTC().Format(time.RFC3339), "retry_after": be.Until.UTC().Format(time.RFC3339)}
			if be.Tail != "" {
				ev["output_tail"], ev["output_truncated"] = be.Tail, be.Truncated
			}
			if be.Log != "" {
				ev["log"] = be.Log
			}
			o.Rep.Event("image_failed", ev)
		}
		return "", "", err
	}
	ev := map[string]any{"image": r.Image, "image_id": r.ImageID, "pulled": r.Pulled, "built": r.Built, "pushing": r.Pushing, "pushed": r.Pushed,
		"installed_docker": r.InstalledDocker, "duration_ms": time.Since(t).Milliseconds()}
	if shared.waited > 0 {
		ev["waited_ms"] = shared.waited.Milliseconds()
	}
	if push != nil {
		ev["pushed"], ev["push_ms"] = push.OK, push.MS
	}
	o.Rep.Event("image", ev)
	return r.Image, r.ImageID, nil
}

// fetchImage sends the image request. With a registry, which may lack this
// image, of the runs on this machine needing it now one builds and pushes
// it, and the rest wait for it and pull it (#139), as fetchDeps does. A
// build that fails is remembered in memo, and a run needing an image whose
// build failed fails at once instead of building it again (#194), unless
// the image can be had without building or o.WaitPush forces the build.
// log keeps the build's output.
func fetchImage(ctx context.Context, c rpc, o Options, label string, errW io.Writer, log *depsLog, memo imageMemo, req protocol.Image, limit time.Duration) (r protocol.ImageResult, shared shareOutcome, push *protocol.ShareDone, err error) {
	building := false // the helper said it is building: its output goes to log too
	pushing := false  // the helper is pushing the image: it has it
	ask := func(req protocol.Image) (protocol.ImageResult, error) {
		if err := c.send(protocol.TypeImage, req); err != nil {
			return protocol.ImageResult{}, err
		}
		for {
			f, err := c.next(ctx)
			if err != nil {
				return protocol.ImageResult{}, err
			}
			switch f.Kind {
			case protocol.KindStdout, protocol.KindStderr:
				errW.Write(f.Data) // pull/build output goes to stderr
				if building && log != nil {
					log.Write(f.Data)
				}
				continue
			}
			env, err := protocol.Decode(f)
			if err != nil {
				return protocol.ImageResult{}, err
			}
			switch env.Type {
			case protocol.TypeStarted:
			case protocol.TypeEvent:
				var ev protocol.Event
				_ = env.Unmarshal(&ev)
				building = building || ev.Name == "image_build"
				pushing = pushing || ev.Name == "image_push"
				o.Rep.Status("%s · %s %s", label, strings.ReplaceAll(ev.Name, "_", " "), ev.Message)
				c.event(ev)
			case protocol.TypeError:
				var em protocol.ErrorMsg
				_ = env.Unmarshal(&em)
				return protocol.ImageResult{}, fmt.Errorf("container image: %w", &transport.RemoteError{ErrorMsg: em})
			case protocol.TypeImageResult:
				var r protocol.ImageResult
				_ = env.Unmarshal(&r)
				return r, nil
			default:
				return protocol.ImageResult{}, fmt.Errorf("protocol: unexpected %q while preparing the image", env.Type)
			}
		}
	}
	// recall is the failure of an earlier build of this image, if one is
	// remembered and not to be built again regardless.
	recall := func() error {
		if o.WaitPush {
			return nil
		}
		if f := memo.recall(req.Tag); f != nil {
			return f.cachedError(memo.clock())
		}
		return nil
	}
	// built settles a build before the runs waiting on it are let go: an
	// image had forgets an earlier failure (even when pushing it then
	// failed), and a failure of the build itself is remembered, with its
	// output.
	built := func(err error) error {
		if pushing || err == nil && r.Image != "" {
			memo.forget(req.Tag)
		}
		var re *transport.RemoteError
		if err == nil || !memo.on(req.Tag) || ctx.Err() != nil || !errors.As(err, &re) || !re.BuildFailed {
			return err
		}
		f := imageFailure{Tag: req.Tag, Message: re.Message, Who: shareWho(o, label), At: memo.clock()}
		if log != nil {
			log.Close()
			f.Tail, f.Truncated = log.Tail(depsTailLines)
			f.Log = log.Path()
		}
		memo.remember(f)
		f.Repo = memo.repo
		return &ImageBuildError{Err: err, Until: f.At.Add(imageFailAge), imageFailure: f}
	}
	defer func() {
		if err == nil && r.Image != "" {
			memo.forget(req.Tag)
		}
	}()
	probe := req
	probe.Probe = true
	if req.Registry == nil || req.Tag == "" {
		if failed := recall(); failed != nil {
			// The runner may have the image already: only building it is
			// ruled out.
			if r, err = ask(probe); err == nil && r.Missing {
				return protocol.ImageResult{InstalledDocker: r.InstalledDocker}, shared, nil, failed
			}
			return r, shared, nil, err
		}
		r, err = ask(req)
		return r, shared, nil, built(err)
	}
	holder := ""
	installed := false // a probe installed Docker
	sh := sharer{
		lock:  shareLockFor("image", req.Registry.Repo+":"+req.Tag),
		who:   shareWho(o, label),
		limit: limit,
		probe: func(context.Context) (bool, error) {
			got, err := ask(probe)
			installed = got.InstalledDocker
			if err == nil && got.Missing {
				err = recall()
			}
			if err != nil || got.Missing {
				return false, err
			}
			r = got
			return true, nil
		},
		build: func(context.Context) error {
			// A run that waited on another's build that failed (or gave up
			// waiting) doesn't build it again.
			if failed := recall(); failed != nil {
				return failed
			}
			r, err = ask(req)
			return built(err)
		},
		publish: func(ctx context.Context) error {
			if !r.Built || !r.Pushing {
				return nil
			}
			o.Rep.Status("%s · pushing the image, for other runs to pull", label)
			d, err := awaitShared(ctx, c, protocol.ShareImage, shareTransfer)
			if err != nil {
				return err
			}
			if !d.OK {
				o.Rep.Warn("IMAGE_REGISTRY", "pushing the image failed (%s): runs waiting for it build it themselves", d.Message)
			}
			push = &d
			return nil
		},
		waiting: func(h string) {
			holder = h
			o.Rep.Status("%s · waiting for %s to build the container image", label, h)
			o.Rep.Event("image_wait", map[string]any{"tag": req.Tag, "holder": h,
				"message": "waiting for " + h + " to build the same image, to pull it from the registry"})
		},
		sess: c,
	}
	shared, err = sh.run(ctx)
	r.InstalledDocker = r.InstalledDocker || installed
	if shared.timedOut && err == nil {
		o.Rep.Warn("IMAGE_WAIT", "waited %s for %s to build the container image; built it here instead", ui.Dur(shared.waited), holder)
	}
	return r, shared, push, err
}

// toolchainFor fingerprints what the dependency install depends on besides
// the lockfile. Inside a container that is the image (already in the key by
// its immutable ID), so caches are shared across fleets with different host
// setups; on the host it is the runner's AMI, bootstrap and PATH setup.
func toolchainFor(r *config.Runner, crun *protocol.ContainerRun) string {
	if crun != nil {
		return "container"
	}
	return depskey.Toolchain(r.Type, r.AMI, r.Arch, r.Image, r.BootstrapScript, r.UserBootstrap, r.RemotePathSetup)
}

func instanceSuffix(id string) string {
	if id == "" {
		return ""
	}
	return ", instance " + id
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
