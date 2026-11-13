package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/manifest"
	"github.com/spotexec/spotexec/internal/protocol"
	"github.com/spotexec/spotexec/internal/provider"
	"github.com/spotexec/spotexec/internal/transport"
	"github.com/spotexec/spotexec/internal/ui"
)

// Exit codes for spotexec's own failures (command exit codes pass through).
const (
	ExitInfra   = 125
	ExitTimeout = 124
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
	PreferLast     bool // exec: reuse last runner+slot
	Stdout         io.Writer
	Stderr         io.Writer
	Rep            ui.Reporter
	Interrupt      <-chan struct{} // each receive = one Ctrl-C
	Label          string
	RetryPreempt   bool
	NoBoot         bool
	SyncOnly       bool // stop after sync + deps (spotexec sync)
}

// Result of a run.
type Result struct {
	ExitCode  int    `json:"exit_code"`
	Runner    string `json:"runner"`
	Slot      int    `json:"slot"`
	Duration  time.Duration
	TimedOut  bool     `json:"timed_out"`
	Mutated   []string `json:"mutated_files"`
	Deleted   []string `json:"deleted_files"`
	Conflicts []string `json:"conflicts"`
	Pulled    int      `json:"pulled"`
	Synced    int      `json:"synced_files"`
	SyncBytes int64    `json:"synced_bytes"`
}

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
}

// Run executes opts.Command remotely for workspace ws. If the runner is lost
// before the command's result arrives (a reclaimed Spot instance, a dead
// VM), the run is replayed once on another slot: nothing from the lost
// attempt was applied locally. Once a result has arrived it is never
// replayed.
func Run(ctx context.Context, cfg *config.Config, ws *Workspace, o Options) (Result, error) {
	att := attempt{runID: newRunID(), n: 1, exclude: map[string]bool{}}
	for {
		res, err, lost := runOnce(ctx, cfg, ws, o, att)
		retryable := lost != "" && o.RetryPreempt && ctx.Err() == nil && o.Sync != "none" && !o.PreferLast && att.n < 2
		if !retryable {
			return res, err
		}
		o.Rep.Warn("RUNNER_LOST", "lost %s before the command finished (%v); replaying the run on another slot", lost, err)
		o.Rep.Event("retry", map[string]any{"run_id": att.runID, "attempt": att.n + 1, "lost": lost})
		if lost != "static" {
			att.exclude[lost] = true
		}
		att.n++
	}
}

// lostRunner reports what to exclude when the connection to a lease died.
func lostRunner(l *Lease, err error) string {
	if l == nil || err == nil || !l.Runner.Ephemeral() {
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

func runOnce(ctx context.Context, cfg *config.Config, ws *Workspace, o Options, att attempt) (Result, error, string) {
	start := time.Now()
	var res Result
	p := cfg.Project
	runID := att.runID

	pool, err := cfg.Pool(o.Pool)
	if err != nil {
		return res, infra(err), ""
	}

	// 1. Local manifest (what the remote workspace must contain).
	var m *manifest.Manifest
	if o.Sync != "none" {
		o.Rep.Status("scanning %s", ws.Root)
		t := time.Now()
		m, err = manifest.Build(ctx, manifest.Options{Root: ws.Root, Exclude: p.Sync.Exclude, Source: p.Sync.Source, Protect: p.Deps.CacheDirs})
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
	req := AcquireRequest{Pool: pool, Base: base, QueueTimeout: o.QueueTimeout, Rep: o.Rep, NoBoot: o.NoBoot, Exclude: att.exclude}
	var lease *Lease
	if o.PreferLast {
		// exec runs without syncing, so it must land in exactly the slot that
		// holds this worktree's files.
		lr, err := LoadLastRun(ws)
		if err != nil {
			return res, infra(errors.New("no previous run from this worktree; use `spotexec run` first")), ""
		}
		r, err := cfg.Runner(lr.Runner)
		if err != nil {
			return res, infra(err), ""
		}
		host, _ := os.Hostname()
		lease, err = AcquireSlot(ctx, cfg, ws, r, lr, o.Rep, host, o.Display, o.Exclusive, o.QueueTimeout)
		if err != nil {
			return res, infra(err), ""
		}
	} else {
		lease, err = Acquire(ctx, req)
		if err != nil {
			return res, infra(err), ""
		}
	}
	defer lease.Close()
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
	}
	remoteWS := lease.Acq.Workspace
	saveLastRun(ws, LastRun{Runner: r.Name, InstanceID: lease.InstanceID, Slot: lease.Acq.Slot, Target: lease.Target, WorkRoot: r.WorkRoot, RemoteWS: remoteWS})
	label := fmt.Sprintf("%s#%d", r.Name, lease.Acq.Token)

	// 3. Forward sync.
	if m != nil {
		if lease.Acq.FingerprintMatch {
			o.Rep.Event("sync_in", map[string]any{"mode": "fingerprint_hit", "files": 0, "bytes": 0, "duration_ms": 0})
		} else {
			t := time.Now()
			o.Rep.Status("%s · syncing %d files", label, len(m.Entries))
			syn, err := syncIn(ctx, sess, m, p.Deps.CacheDirs, o)
			if err != nil {
				return res, infra(fmt.Errorf("sync to %s: %w", r.Name, err)), lostRunner(lease, err)
			}
			res.Synced, res.SyncBytes = syn.Written, syn.Bytes
			o.Rep.Event("sync_in", map[string]any{"mode": "delta", "files": syn.Written, "deleted": syn.Deleted, "bytes": syn.Bytes, "duration_ms": time.Since(t).Milliseconds()})
			o.Rep.Status("%s · synced %d files (%s) in %s", label, syn.Written, ui.Bytes(syn.Bytes), ui.Dur(time.Since(t)))
		}
	}

	// 4. Dependencies.
	if p.Deps.Install != "" && !o.NoDeps && o.Sync != "none" {
		if err := runDeps(ctx, sess, p, r, o, label); err != nil {
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
		"SPOTEXEC":             "1",
		"SPOTEXEC_SLOT":        fmt.Sprint(lease.Acq.Token),
		"SPOTEXEC_PORT_BASE":   fmt.Sprint(20000 + lease.Acq.Token*1000),
		"SPOTEXEC_RUNNER":      r.Name,
		"SPOTEXEC_RUN_ID":      runID,
		"SPOTEXEC_ATTEMPT":     fmt.Sprint(att.n),
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
	skip := append([]string{}, manifest.HeavyDirs...)
	skip = append(skip, p.Deps.CacheDirs...)
	ex := protocol.Exec{
		Shell: o.Command, Cwd: ws.Cwd, Env: env, Setup: r.RemotePathSetup,
		TimeoutMS: o.Timeout.Milliseconds(), GraceMS: o.Grace.Milliseconds(),
		Snapshot: o.Sync != "none", Skip: skip, SpotWatch: r.Type == config.TypeAWSSpot,
	}
	o.Rep.Status("%s · running %s", label, o.Display)
	o.Rep.Event("exec", map[string]any{"runner": r.Name, "slot": lease.Acq.Token, "command": o.Display, "cwd": ws.Cwd})
	stopWatch.Do(func() { close(execStarted) })
	if ctx.Err() != nil {
		return res, infra(ctx.Err()), ""
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
		if lease.InstanceID == "" || !(o.RetryPreempt && att.n < 2 && o.Sync != "none" && !o.PreferLast) {
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
	if result.StartError != "" {
		o.Rep.Warn("START_FAILED", "could not start command: %s", result.StartError)
	}
	res.ExitCode, res.TimedOut = result.ExitCode, result.TimedOut
	// Post-exec work uses its own bounded context: an interrupt that already
	// canceled ctx must not prevent writing back what the command changed.
	post, cancelPost := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelPost()
	if len(result.Unverified) > 0 {
		o.Rep.Warn("UNVERIFIED", "%d file(s) could not be read on the runner after the run and were not written back: %s", len(result.Unverified), strings.Join(firstN(result.Unverified, 5), ", "))
	}
	if result.TimedOut {
		o.Rep.Warn("TIMEOUT", "command exceeded %s and was terminated (raise with --timeout)", o.Timeout)
	}

	// 6. Write-back of mutations.
	if m != nil && o.Sync == "auto" && wantWriteback(o.Writeback, result.ExitCode) &&
		(len(result.Changed)+len(result.Deleted)+len(result.Created) > 0) {
		t := time.Now()
		wb, err := writeback(post, sess, ws, m, result, runID)
		if err != nil {
			// The command already finished: never replay it. Report what is
			// known so the caller can decide.
			return res, infra(fmt.Errorf("command finished (exit %d) but its file changes could not be written back: %w", result.ExitCode, err)), ""
		}
		res.Mutated, res.Deleted, res.Conflicts = wb.Applied, wb.Deleted, wb.Conflicts
		o.Rep.Event("sync_out", map[string]any{"mutated_files": nz(wb.Applied), "deleted_files": nz(wb.Deleted), "conflicts": nz(wb.Conflicts), "ignored_created": wb.Ignored, "duration_ms": time.Since(t).Milliseconds()})
		for _, c := range wb.Conflicts {
			o.Rep.Warn("SYNC_CONFLICT", "%s changed locally during the run; kept your version, remote version saved to %s", c, filepath.Join(wb.ConflictDir, c))
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
		n, err := PullArtifacts(post, sess, ws.Root, pulls)
		if err != nil {
			o.Rep.Warn("PULL_FAILED", "pulling %v: %v", pulls, err)
		}
		res.Pulled = n
		o.Rep.Event("pull", map[string]any{"patterns": pulls, "files": n})
	}
	res.Duration = time.Since(start)
	o.Rep.Event("result", map[string]any{"exit_code": res.ExitCode, "duration_ms": res.Duration.Milliseconds(), "timed_out": res.TimedOut, "runner": r.Name, "slot": res.Slot})
	return res, nil, ""
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

func wantWriteback(policy string, code int) bool {
	switch policy {
	case "never":
		return false
	case "on-success":
		return code == 0
	}
	return true
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// syncIn sends the manifest and streams whatever the helper reports missing.
func syncIn(ctx context.Context, s *transport.Session, m *manifest.Manifest, protect []string, o Options) (protocol.Synced, error) {
	var syn protocol.Synced
	if err := s.Conn.Send(protocol.TypeManifest, protocol.Manifest{Fingerprint: m.Fingerprint, Entries: m.Entries, Protect: protect}); err != nil {
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
	if len(need.Paths) > 0 {
		if need.Bytes > AbortBytes && !o.ForceLargeSync {
			return syn, fmt.Errorf("refusing to transfer %s (limit %s); add excludes or pass --force-large-sync", ui.Bytes(need.Bytes), ui.Bytes(AbortBytes))
		}
		if need.Bytes > WarnBytes {
			o.Rep.Warn("LARGE_SYNC", "transferring %s", ui.Bytes(need.Bytes))
		}
		bw := &protocol.BlobWriter{C: s.Conn}
		zw, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
		tw := tar.NewWriter(zw)
		werr := func() error {
			for _, p := range need.Paths {
				if err := addLocalTar(tw, filepath.Join(m.Root, filepath.FromSlash(p)), p); err != nil {
					return err
				}
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
	// landed remotely are the write-back baseline.
	for _, c := range syn.Corrected {
		m.Update(c)
	}
	return syn, nil
}

func addLocalTar(tw *tar.Writer, full, name string) error {
	fi, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("%s vanished during sync; re-run", name)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(full)
		if err != nil {
			return err
		}
		return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: t, Mode: 0o777})
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	mode := int64(0o644)
	if fi.Mode()&0o111 != 0 {
		mode = 0o755
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(b)), Mode: mode, ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = tw.Write(b)
	return err
}

func runDeps(ctx context.Context, s *transport.Session, p *config.Project, r *config.Runner, o Options, label string) error {
	lockfiles := p.Deps.Lockfiles
	if len(lockfiles) == 0 {
		return errors.New("deps.install is set but deps.lockfiles is empty")
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
	if err := s.Conn.Send(protocol.TypeDeps, protocol.Deps{
		Lockfiles: lockfiles, Install: p.Deps.Install, CacheDirs: p.Deps.CacheDirs, Hydrate: p.Deps.Hydrate,
		Setup: r.RemotePathSetup, Env: env, TimeoutMS: p.Deps.Timeout.Milliseconds(),
	}); err != nil {
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
			o.Rep.Event(ev.Name, map[string]any{"message": ev.Message})
			continue
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			return fmt.Errorf("dependencies: %s", em.Message)
		case protocol.TypeDepsResult:
			var dr protocol.DepsResult
			_ = env.Unmarshal(&dr)
			o.Rep.Event("deps", map[string]any{"key": dr.Key, "built": dr.Built, "hydrated": dr.Hydrated, "method": dr.Method, "duration_ms": time.Since(t).Milliseconds()})
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
	// The helper heartbeats every 20s; silence well beyond that means it is
	// wedged or the link is a blackhole even if SSH keepalives still pass.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var canceledAt time.Time
	cancelGrace := o.Grace + 30*time.Second
	for {
		select {
		case <-tick.C:
			if time.Since(s.LastSeen()) > 75*time.Second {
				s.Abort()
				return res, &transport.ConnError{Target: "helper", Msg: "runner stopped responding"}
			}
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
