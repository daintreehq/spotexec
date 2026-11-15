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
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	Command        string // shell command line
	Display        string // for status/holder display
	Timeout        time.Duration
	Grace          time.Duration
	QueueTimeout   time.Duration
	Sync           string // auto|in|none
	Writeback      string // always|on-success|never
	Pool           []string
	Exclusive      bool
	Pull           []string
	NoDeps         bool
	ForceLargeSync bool
	PreferLast     bool     // exec: reuse last runner+slot without syncing
	SameSlot       bool     // --same-slot: sync and run in this worktree's last slot
	LastWithin     []string // with PreferLast/SameSlot: the last runner must be one of these
	DeferLastRun   bool     // return the last-run record in Result instead of saving it
	Stdout         io.Writer
	Stderr         io.Writer
	Rep            ui.Reporter
	Interrupt      <-chan struct{} // each receive = one Ctrl-C
	Label          string
	RetryPreempt   bool
	NoBoot         bool
	SyncOnly       bool // stop after sync + deps (spotexec sync)
	WaitPush       bool // push a freshly built image before returning (container build)
	Priority       int  // queue priority when every slot is busy (higher first)
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
	Pulled    int      `json:"pulled"`
	Synced    int      `json:"synced_files"`
	SyncBytes int64    `json:"synced_bytes"`
	Image     string   `json:"image,omitempty"` // the project container image used
	// LastRun is where this worktree's files now are, when the run
	// synced (saved already unless Options.DeferLastRun).
	LastRun *LastRun `json:"-"`
	// OOMKilled: the kernel killed a process in the command's container
	// for exceeding its memory limit.
	OOMKilled bool `json:"oom_killed,omitempty"`
	// noReplay, when the runner was lost, is why the run must not be
	// replayed elsewhere (see replayable).
	noReplay noReplay
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
// attempt was applied locally. Once a result has arrived it is never
// replayed.
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
		o.Rep.Warn("RUNNER_LOST", "lost %s before the command finished (%v); replaying the run on another slot", lost, err)
		o.Rep.Event("retry", map[string]any{"run_id": att.runID, "attempt": att.n + 1, "lost": lost})
		if lost != "static" {
			att.exclude[lost] = true
			markLost(cfg, res.Runner, lost)
		}
		att.n++
	}
}

// replayable decides whether an attempt that lost its runner (lost, from
// lostRunner) is replayed on another slot. why is set when the runner was
// out of memory when it was lost: that run is never replayed, since a run
// that used up its VM's memory would most likely do the same on the next
// one, and take that VM's other jobs with it.
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

// whyNoReplay decides, once a fleet lease's runner is lost, whether the
// run must not be replayed elsewhere: its VM reported memory pressure (a
// run that used up its VM's memory would most likely do the same on the
// next one, and take that VM's other jobs with it), or the VM is still
// running and gave no Spot notice, so whatever stopped it answering came
// from inside, quite possibly from this run. A VM that is gone, or noticed
// its reclamation, is replayed as before.
func whyNoReplay(lease *Lease, noticed bool) noReplay {
	if p := lease.Sess.MemoryPressure(PressureWindow); p != "" {
		return noReplay{"MEMORY_PRESSURE", "while it was out of memory (" + p + "); it would likely run out on another VM too. Give the run more memory (--exclusive, fewer max_slots, a larger instance type) or make it use less"}
	}
	if !noticed && instanceRunning(lease) {
		return noReplay{"RUNNER_UNRESPONSIVE", "although EC2 still reports it running, without a Spot notice: the run may be what overloaded it (memory, most often), and the attempt's outcome is unknown. Rerun it, with --exclusive if it may need the whole VM"}
	}
	return noReplay{}
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
	var re *transport.RemoteError
	if errors.As(err, &re) {
		if re.Code == protocol.CodeShutdown {
			return shutdownRunner(l)
		}
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
		if len(m.Entries) > AbortFiles && !o.ForceLargeSync {
			return res, infra(fmt.Errorf("refusing to sync %d files (limit %d); add excludes to .spotexec.toml or pass --force-large-sync", len(m.Entries), AbortFiles)), ""
		}
		if len(m.Entries) > WarnFiles {
			o.Rep.Warn("LARGE_SYNC", "syncing %d files; consider [sync].exclude", len(m.Entries))
		}
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
	defer func() {
		if lost != "" && lease.InstanceID != "" {
			res.noReplay = whyNoReplay(lease, noticed.Load())
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
		if ev.Name == "spot_interruption" {
			noticed.Store(true)
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
			o.Rep.Status("%s · syncing %d files", label, len(m.Entries))
			stray, err := strayFiles(ctx, ws, m, lease.Acq.Pending)
			if err != nil {
				return res, infra(fmt.Errorf("checking files earlier runs left in %s: %w", label, err)), ""
			}
			syn, err := syncIn(ctx, sess, m, p.Deps.CacheDirs, stray, o)
			if err != nil {
				return res, infra(fmt.Errorf("sync to %s: %w", r.Name, err)), lostRunner(lease, err)
			}
			res.Synced, res.SyncBytes = syn.Written, syn.Bytes
			o.Rep.Event("sync_in", map[string]any{"mode": "delta", "files": syn.Written, "deleted": syn.Deleted, "bytes": syn.Bytes, "duration_ms": time.Since(t).Milliseconds()})
			o.Rep.Status("%s · synced %d files (%s) in %s", label, syn.Written, ui.Bytes(syn.Bytes), ui.Dur(time.Since(t)))
		}
		// Before the image and dependencies, so install scripts see it too.
		if err := seedGit(ctx, sess, m, p.Sync.GitSeed, o, label); err != nil {
			return res, infra(fmt.Errorf("seeding .git on %s: %w", r.Name, err)), lostRunner(lease, err)
		}
	}

	// 4. Project container image.
	var crun *protocol.ContainerRun
	imageID := ""
	if p.Container.Enabled() {
		ref, id, err := prepareImage(ctx, sess, p, o, label, ws, lease, m)
		if err != nil {
			return res, infra(err), lostRunner(lease, err)
		}
		crun = &protocol.ContainerRun{Image: ref, Options: p.Container.Options, Root: p.Container.User == "root", Network: p.Container.Network}
		imageID = id
		lastRun.Image = ref
		res.Image = ref
	}
	recordLastRun()

	// 5. Dependencies.
	if p.Deps.Install != "" && !o.NoDeps && o.Sync != "none" {
		if err := runDeps(ctx, sess, p, r, o, label, ws, lease, crun, imageID, m); err != nil {
			return res, infra(err), lostRunner(lease, err)
		}
	}

	if o.SyncOnly {
		res.Duration = time.Since(start)
		o.Rep.Event("result", map[string]any{"exit_code": 0, "duration_ms": res.Duration.Milliseconds(), "runner": r.Name, "slot": res.Slot, "sync_only": true})
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
	skip := append([]string{}, manifest.HeavyDirs...)
	skip = append(skip, p.Deps.CacheDirs...)
	ex := protocol.Exec{
		Shell: o.Command, Cwd: ws.Cwd, Env: env, Setup: r.RemotePathSetup,
		TimeoutMS: o.Timeout.Milliseconds(), GraceMS: o.Grace.Milliseconds(),
		// Always snapshotted, so the helper knows which files even an
		// unsynced run created (removed by the next run that syncs here).
		Snapshot: true, Skip: skip, SpotWatch: r.Type == config.TypeAWSSpot,
		Container: crun,
	}
	o.Rep.Status("%s · running %s", label, o.Display)
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
		spare := req
		spare.Exclude = map[string]bool{lease.InstanceID: true}
		for k := range att.exclude {
			spare.Exclude[k] = true
		}
		spare.Base.Fingerprint = ""
		spare.Base.Exclusive = false
		var sctx context.Context
		sctx, stopWarm = context.WithTimeout(context.WithoutCancel(ctx), r.BootTimeout.Duration+2*time.Minute)
		go func() { defer close(warmDone); EnsureSpare(sctx, spare) }()
	}
	outW := &ui.Rewriter{W: &ui.ClearingWriter{W: o.Stdout, R: o.Rep}, Old: []byte(remoteWS), New: []byte(ws.Root)}
	errW := &ui.Rewriter{W: &ui.ClearingWriter{W: o.Stderr, R: o.Rep}, Old: []byte(remoteWS), New: []byte(ws.Root)}
	result, err := pumpExec(sess, outW, errW, o, onSpot)
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
		return res, infra(fmt.Errorf("the command could not be started on %s: %s", r.Name, result.StartError)), ""
	case execCanceled:
		res.Canceled, res.ExitCode = true, ExitInterrupted
	case execShutdown:
		// Killed by the VM shutting down (a Spot reclaim): like a lost
		// runner, nothing from this attempt is written back, and on a Spot
		// runner the run is replayed (unless the user interrupted it: then
		// the CLI exits 130).
		return res, infra(fmt.Errorf("%s shut down while the command ran (it was stopped with exit %d); nothing was written back", label, result.ExitCode)), shutdownRunner(lease)
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
		ignoredCreated = settleCreated(post, sess, m, result.Created)
	}
	if m != nil && len(result.Unverified) > 0 {
		o.Rep.Warn("UNVERIFIED", "%d file(s) could not be read on the runner after the run and were not written back: %s", len(result.Unverified), strings.Join(firstN(result.Unverified, 5), ", "))
	}
	if result.TimedOut {
		o.Rep.Warn("TIMEOUT", "command exceeded %s and was terminated (raise with --timeout)", o.Timeout)
	}
	if result.OOMKilled {
		res.OOMKilled = true
		o.Rep.Warn("OOM_KILLED", "%s", oomMessage(result.MemoryLimitMB))
	}

	// 6. Write-back of mutations.
	if m != nil && o.Sync == "auto" && wantWriteback(o.Writeback, res.ExitCode) &&
		(len(result.Changed)+len(result.Deleted)+len(result.Created) > 0) {
		t := time.Now()
		// on-success: an interrupt while the changes are being fetched means
		// the run no longer counts as a success; apply nothing.
		var proceed func() bool
		if o.Writeback == "on-success" {
			proceed = func() bool { return ctx.Err() == nil }
		}
		wb, err := writeback(post, sess, ws, m, result, runID, ignoredCreated, proceed)
		// Conflicts stashed before any failure are reported either way.
		reportConflicts(o.Rep, wb.Stashed, runID)
		if err != nil {
			// The command already finished: never replay it. Report what is
			// known so the caller can decide.
			return res, infra(fmt.Errorf("command finished (exit %d) but its file changes could not be written back: %w", res.ExitCode, err)), ""
		}
		res.Mutated, res.Deleted, res.Conflicts = wb.Applied, wb.Deleted, wb.Conflicts
		o.Rep.Event("sync_out", map[string]any{"mutated_files": nz(wb.Applied), "deleted_files": nz(wb.Deleted), "conflicts": nz(wb.Conflicts), "ignored_created": wb.Ignored, "duration_ms": time.Since(t).Milliseconds()})
		if len(wb.Refused) > 0 {
			o.Rep.Warn("WRITEBACK_REFUSED", "ignored %d path(s) the runner reported that may not be written back (inside .git or .spotexec, or never uploaded): %s", len(wb.Refused), strings.Join(firstN(wb.Refused, 5), ", "))
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
		n, err := PullArtifacts(post, sess, ws, ws.Root, pulls)
		if err != nil {
			o.Rep.Warn("PULL_FAILED", "pulling %v: %v", pulls, err)
		}
		res.Pulled = n
		o.Rep.Event("pull", map[string]any{"patterns": pulls, "files": n})
	}
	checkCanceled()
	res.Duration = time.Since(start)
	ev := map[string]any{"exit_code": res.ExitCode, "duration_ms": res.Duration.Milliseconds(), "timed_out": res.TimedOut, "canceled": res.Canceled, "runner": r.Name, "slot": res.Slot}
	if res.OOMKilled {
		ev["oom_killed"] = true
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

// reportConflicts says, for each write-back conflict, exactly where to look:
// the stash is under the git dir, out of sight of `git status` and editors.
func reportConflicts(rep ui.Reporter, stashed []Conflict, runID string) {
	for _, c := range stashed {
		diff := conflictDiff(c)
		if c.Deleted {
			rep.Warn("SYNC_CONFLICT", "%s changed locally during the run, and the remote command deleted it; kept your version (delete it yourself to accept the deletion). Your changes: %s", c.Path, diff)
		} else {
			rep.Warn("SYNC_CONFLICT", "%s changed locally during the run; kept your version. The remote version: %s", c.Path, diff)
		}
		rep.Event("conflict", map[string]any{"path": c.Path, "local": c.Local, "remote": c.Remote, "deleted": c.Deleted, "diff": diff, "run_id": runID})
	}
}

// conflictDiff is a ready-to-run command comparing your kept file with what
// the remote run produced (/dev/null for a side that doesn't exist).
func conflictDiff(c Conflict) string {
	local, remote := c.Local, c.Remote
	if c.Deleted {
		remote = os.DevNull // Remote is only a marker
	}
	if fi, err := os.Lstat(local); err != nil || fi.IsDir() {
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
	var stray, check []string
	for _, p := range pending {
		if _, ok := m.ByPath[p]; ok {
			continue // synced over by this run
		}
		// Never a file of ours: an invalid or reserved path, or one under a
		// synced file or a local symlink or file (git can't judge those).
		if _, err := fsid.SafeJoin(ws.Root, p); err != nil || reservedPath(p, ws.Git) || underSyncedFile(m, p) {
			stray = append(stray, p)
			continue
		}
		check = append(check, p)
	}
	if len(check) == 0 {
		return stray, nil
	}
	ign, err := m.CheckIgnored(ctx, check)
	if err != nil {
		return nil, err
	}
	for _, p := range check {
		if !ign[p] {
			stray = append(stray, p)
		}
	}
	return stray, nil
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
// then checks again and reports the error).
func settleCreated(ctx context.Context, s *transport.Session, m *manifest.Manifest, created []protocol.Entry) map[string]bool {
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
		if ign[p] {
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
	b, err := os.ReadFile(full)
	if err != nil {
		return protocol.Entry{}, err
	}
	mode, emode := int64(0o644), fsid.ModeFile
	if fi.Mode()&0o111 != 0 {
		mode, emode = 0o755, fsid.ModeExec
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(b)), Mode: mode, ModTime: fi.ModTime()}); err != nil {
		return protocol.Entry{}, err
	}
	_, err = tw.Write(b)
	return protocol.Entry{Path: name, ID: fsid.Bytes(b), Mode: emode, Size: int64(len(b))}, err
}

func runDeps(ctx context.Context, s *transport.Session, p *config.Project, r *config.Runner, o Options, label string, ws *Workspace, lease *Lease, crun *protocol.ContainerRun, imageID string, m *manifest.Manifest) error {
	if len(p.Deps.Lockfiles) == 0 {
		return errors.New("deps.install is set but deps.lockfiles is empty")
	}
	// The runner hashes exactly this list: it never expands patterns itself,
	// so both sides key the cache on the same files.
	lockfiles, unmatched, err := DepsLockfiles(m, p.Deps.Lockfiles)
	if err != nil {
		return err
	}
	for _, pat := range unmatched {
		o.Rep.Warn("DEPS_LOCKFILES", "deps.lockfiles pattern %q matches no synced file, so it adds nothing to the dependency cache key", pat)
	}
	if len(lockfiles) == 0 {
		return errors.New("deps.lockfiles matches no synced file: the dependency cache needs at least one to key on")
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
	if dc, ok := provider.ForInstance(ctx, lease.Provider, lease.InstanceID).(provider.DepsCache); ok {
		key, err := depskey.Key(ws.Root, depskey.Spec{Lockfiles: lockfiles, Install: p.Deps.Install, CacheDirs: p.Deps.CacheDirs, Image: image, Toolchain: req.Toolchain, OS: s.Hello.OS, Arch: s.Hello.Arch})
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			get, put, err := dc.CacheURLs(cctx, ws.RepoID, key)
			cancel()
			if err != nil {
				o.Rep.Warn("DEPS_CACHE", "shared dependency cache unavailable: %v", err)
			} else if get != "" {
				req.CacheKey, req.CacheGet, req.CachePut = key, get, put
			}
		}
	}
	if err := s.Conn.Send(protocol.TypeDeps, req); err != nil {
		return err
	}
	errW := &ui.ClearingWriter{W: o.Stderr, R: o.Rep}
	for {
		f, err := s.Next(ctx)
		if err != nil {
			return err
		}
		switch f.Kind {
		case protocol.KindStdout, protocol.KindStderr:
			// Install output goes to stderr so command stdout stays clean.
			errW.Write(f.Data)
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return err
		}
		switch env.Type {
		case protocol.TypeStarted:
			continue
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			if ev.Name == "deps_install" {
				o.Rep.Status("%s · installing dependencies (%s)", label, ev.Message)
			}
			if s.OnEvent != nil {
				s.OnEvent(ev) // records interruption notices for the pre-exec check
			} else {
				o.Rep.Event(ev.Name, map[string]any{"message": ev.Message})
			}
			continue
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			return fmt.Errorf("dependencies: %w", &transport.RemoteError{ErrorMsg: em})
		case protocol.TypeDepsResult:
			var dr protocol.DepsResult
			_ = env.Unmarshal(&dr)
			o.Rep.Event("deps", map[string]any{"key": dr.Key, "built": dr.Built, "restored": dr.Restored, "uploading": dr.Uploaded,
				"hydrated": dr.Hydrated, "method": dr.Method, "duration_ms": time.Since(t).Milliseconds()})
			return nil
		default:
			return fmt.Errorf("protocol: unexpected %q during deps", env.Type)
		}
	}
}

// pumpExec streams output until the result arrives, forwarding interrupts.
func pumpExec(s *transport.Session, stdout, stderr io.Writer, o Options, onSpot func()) (protocol.Result, error) {
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
			case protocol.TypeEvent:
				var ev protocol.Event
				_ = env.Unmarshal(&ev)
				o.Rep.Event(ev.Name, map[string]any{"message": ev.Message})
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
// cap, 0 when the container wasn't in the pool (container.options set a
// limit of its own).
func oomMessage(limitMB int64) string {
	if limitMB <= 0 {
		return "the command ran out of memory: the kernel killed a process in its container for exceeding the container's memory limit (set in container.options)"
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

// imageTag hashes the runner platform, the Dockerfile, a companion
// <Dockerfile>.dockerignore, and every synced file in the build context
// that .dockerignore keeps (with any corrections from the upload), so a
// built image is reused until one of them changes, and never across CPU
// architectures. read loads a synced file's content (the ignore rules).
func imageTag(m *manifest.Manifest, dockerfile, buildCtx, platform string, read func(rel string) ([]byte, error)) (string, error) {
	if m == nil {
		return "", errors.New("container.dockerfile needs a synced workspace (not --sync none)")
	}
	df, ok := m.ByPath[dockerfile]
	if !ok {
		if read != nil {
			if _, err := read(dockerfile); err == nil {
				// The Dockerfile syncs whether or not Git tracks it, so only
				// an exclude can keep it out.
				if m.Matcher != nil && m.Matcher.Match(dockerfile, false) {
					return "", fmt.Errorf("container.dockerfile %q exists but is not synced: [sync].exclude, .spotexecignore or deps.cache_dirs excludes it; remove that pattern", dockerfile)
				}
				return "", fmt.Errorf("container.dockerfile %q exists but is not synced: spotexec never syncs that path; move the Dockerfile and point container.dockerfile at it", dockerfile)
			}
		}
		return "", fmt.Errorf("container.dockerfile %q is not in the synced working set", dockerfile)
	}
	if df.Mode == fsid.ModeSymlink {
		return "", fmt.Errorf("container.dockerfile %q must be a regular file, not a symlink", dockerfile)
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
			return "", fmt.Errorf("container.context %q must stay inside the repository", buildCtx)
		}
		buildCtx = c
	}
	h := sha256.New()
	fmt.Fprintf(h, "platform\x00%s\x00dockerfile\x00%s\x00%s\x00ctx\x00%s\x00", platform, dockerfile, df.ID, buildCtx)
	if ign, ok := m.ByPath[dockerfile+".dockerignore"]; ok {
		fmt.Fprintf(h, "dockerignore\x00%s\x00", ign.ID)
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
			return "", fmt.Errorf("%s is a symlink; make it a regular file", cand)
		}
		if read == nil {
			return "", fmt.Errorf("cannot read %s", cand)
		}
		b, err := read(cand)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", cand, err)
		}
		// The rules must be exactly what was synced (the helper stages with
		// that copy), or the tag would not describe the built image.
		if fsid.Bytes(b) != e.ID {
			return "", fmt.Errorf("%s changed after it was synced; run again", cand)
		}
		rules = b
		break
	}
	ign, err := dockerignore.New(rules)
	if err != nil {
		return "", fmt.Errorf("parsing .dockerignore: %w", err)
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
		tag, err := imageTag(m, c.Dockerfile, c.Context, s.Hello.OS+"/"+s.Hello.Arch, func(rel string) ([]byte, error) {
			return os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(rel)))
		})
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
	if err := s.Conn.Send(protocol.TypeImage, req); err != nil {
		return "", "", err
	}
	errW := &ui.ClearingWriter{W: o.Stderr, R: o.Rep}
	for {
		f, err := s.Next(ctx)
		if err != nil {
			return "", "", err
		}
		switch f.Kind {
		case protocol.KindStdout, protocol.KindStderr:
			errW.Write(f.Data) // pull/build output goes to stderr
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return "", "", err
		}
		switch env.Type {
		case protocol.TypeStarted:
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			o.Rep.Status("%s · %s %s", label, strings.ReplaceAll(ev.Name, "_", " "), ev.Message)
			if s.OnEvent != nil {
				s.OnEvent(ev)
			}
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			return "", "", fmt.Errorf("container image: %w", &transport.RemoteError{ErrorMsg: em})
		case protocol.TypeImageResult:
			var r protocol.ImageResult
			_ = env.Unmarshal(&r)
			o.Rep.Event("image", map[string]any{"image": r.Image, "image_id": r.ImageID, "pulled": r.Pulled, "built": r.Built, "pushing": r.Pushing, "pushed": r.Pushed,
				"installed_docker": r.InstalledDocker, "duration_ms": time.Since(t).Milliseconds()})
			return r.Image, r.ImageID, nil
		default:
			return "", "", fmt.Errorf("protocol: unexpected %q while preparing the image", env.Type)
		}
	}
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
