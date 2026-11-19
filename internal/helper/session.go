// Package helper implements spotexec-helper, the small remote binary that the
// CLI drives over an SSH channel. It owns slot locking, forward-sync
// application, dependency hydration, process-group execution, and mutation
// detection for write-back.
package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Version is set by the build (ldflags) and shown in hello.
var Version = "dev"

// ClientTimeout is how long the helper tolerates silence from the CLI before
// assuming it is gone (laptop asleep, network blackhole) and cancelling.
const ClientTimeout = 45 * time.Second

// Session is one RPC connection.
type Session struct {
	c       *protocol.Conn
	layout  Layout
	frames  chan protocol.Frame
	readErr atomic.Value // error
	// born anchors the session's monotonic clock (see now): lastSeen,
	// stalledAt and stallTick are offsets from it, so a wall clock stepped
	// by NTP on a just-booted or resumed VM can't make the client look
	// silent, or alive.
	born     time.Time
	lastSeen atomic.Int64
	// received counts the bytes read from the client (Heartbeat.Received).
	received atomic.Int64
	// held is how long readLoop has waited on a full frame queue, not
	// counting a wait under way, which began at holding (on the session
	// clock; 0 for none). Heartbeat.HeldMS. Both under heldMu, so a
	// heartbeat counts a wait once.
	heldMu        sync.Mutex
	held, holding int64
	// stalledAt is when the helper last found itself unscheduled for
	// seconds (see watchStalls); 0 (never) until then.
	stalledAt atomic.Int64
	stallTick atomic.Int64 // the stall sampler's latest run; 0 before its first
	// owned is the work root while this session holds a slot (read by the
	// heartbeat goroutine, so not s.tokens / s.layout directly).
	owned atomic.Pointer[string]
	// relMu guards what the session owns (its locks and the slot's paths)
	// against Shutdown and lastResort, which tear it down from another
	// goroutine: acquire publishes them under it (see own).
	relMu    sync.Mutex
	procMu   sync.Mutex  // held while a process or container is being started
	stopping atomic.Bool // Shutdown/lastResort own cleanup and exit
	// prepCancel ends the slow preparation a start does under procMu (see
	// startPrep), so teardown doesn't wait it out; guarded by prepMu.
	prepMu     sync.Mutex
	prepCancel context.CancelFunc

	lifecycle *flock
	tokens    []*flock
	tokenIdx  int
	run       *flock // the run permit, while the command runs (see takeRun)
	runIdx    int
	slotLock  *flock
	slotDir   string
	workspace string
	repoID    string
	state     SlotState
	acq       protocol.Acquire
	maxSlots  int   // the slot count acquire used (max_slots, or automatic)
	cpusNoted bool  // the container_cpus event was sent
	memNoted  bool  // the container_memory event was sent
	memLimit  int64 // the memory limit given to the last container, bytes
	// lastContainer is the session's latest project container, to look up
	// whether it was OOM-killed once it has exited.
	lastContainer struct {
		name    string
		d       dockerRunner
		started time.Time
		cid     string // the file docker writes its ID to, when in the memory pool
	}
	// runMem is the memory the current exec's command reserves in the
	// pool, recorded with its run permit (see runmemory.go).
	runMem     runMemRecord
	stored     map[string]storedEntry
	exiting    atomic.Bool
	child      atomic.Int64 // running command's process group, if any
	gate       *flock
	containers sync.Map // running container name -> dockerRunner
	// nested: project container name -> nestedRun, for runs with
	// container.docker whose own containers aren't removed yet.
	nested      sync.Map
	interrupted atomic.Bool // a spot interruption notice arrived
	watching    bool
	// transfers: the background upload (protocol.ShareDeps) or push
	// (protocol.ShareImage) this session last started, for ShareWait.
	transfers map[string]*transfer
}

// Serve runs the RPC loop on stdin/stdout until the client says bye or goes
// away. All locks are released on return.
// current is the session being served (one per helper process), for
// Shutdown.
var current atomic.Pointer[Session]

// Shutdown ends the process after cleaning up what the session owns (its
// command's process group, containers, slot locks): used on SIGTERM/SIGHUP,
// when a blocked read of stdin can't be interrupted.
func Shutdown(code int) {
	if s := current.Load(); s != nil {
		s.teardown()
	}
	os.Exit(code)
}

// teardown cleans up what the session owns (its command's process group,
// containers, slot locks) before the process exits. The dispatch goroutine
// may be acquiring or releasing meanwhile: once stopping is set it gives up
// nothing and takes nothing more (it parks; see parkIfStopping), so the
// slot read here under relMu stays this session's until release below.
func (s *Session) teardown() {
	s.stopping.Store(true)
	s.cancelPrep()
	s.procMu.Lock() // wait out a start in progress; none begins after this
	s.procMu.Unlock()
	if pg := int(s.child.Load()); pg > 0 {
		killGroup(pg, 2*time.Second)
	}
	s.relMu.Lock()
	workspace, slotDir := s.workspace, s.slotDir
	if s.exiting.Load() {
		workspace, slotDir = "", "" // already released: another run's now
	}
	s.relMu.Unlock()
	if workspace != "" {
		reapWorkspace(workspace, 2*time.Second)
	}
	s.removeContainers(false)
	if slotDir != "" {
		_ = os.RemoveAll(filepath.Join(slotDir, "run"))
	}
	s.releaseLocks(true)
}

// startPrep returns a context, ending after budget or with parent, for
// preparation a
// start does under procMu that can take minutes (reading an image's users),
// and done, to call once it is over. Teardown cancels it rather than wait
// for it: stopping is set before teardown takes prepMu, so a context made
// after teardown looked is cancelled here.
func (s *Session) startPrep(parent context.Context, budget time.Duration) (ctx context.Context, done func()) {
	ctx, cancel := context.WithTimeout(parent, budget)
	s.prepMu.Lock()
	defer s.prepMu.Unlock()
	if s.stopping.Load() {
		cancel()
	}
	s.prepCancel = cancel
	return ctx, func() {
		s.prepMu.Lock()
		s.prepCancel = nil
		s.prepMu.Unlock()
		cancel()
	}
}

// cancelPrep ends the preparation a start is doing (startPrep), if any.
func (s *Session) cancelPrep() {
	s.prepMu.Lock()
	defer s.prepMu.Unlock()
	if s.prepCancel != nil {
		s.prepCancel()
	}
}

// parkIfStopping, called with relMu held, parks the calling goroutine for
// good (releasing relMu) once teardown has begun: teardown releases
// everything and exits, and must not find the slot given up or taken
// underneath it.
func (s *Session) parkIfStopping() {
	if s.stopping.Load() {
		s.relMu.Unlock()
		select {}
	}
}

// own publishes what acquire just took (locks, the slot's paths) under
// relMu, so a concurrent teardown sees all of it or none.
func (s *Session) own(f func()) {
	s.relMu.Lock()
	defer s.relMu.Unlock()
	s.parkIfStopping()
	f()
}

// newSession starts a session's monotonic clock, with the client just
// heard from.
func newSession() *Session {
	s := &Session{frames: make(chan protocol.Frame, 64), born: time.Now()}
	s.heard()
	return s
}

// now is the session's monotonic clock: time since it began, never 0 (a
// timestamp's "never"). Every liveness and stall timestamp is one of its
// readings.
func (s *Session) now() int64 { return max(int64(time.Since(s.born)), 1) }

// heard notes that the client just sent something.
func (s *Session) heard() { s.lastSeen.Store(s.now()) }

func Serve(in io.Reader, out io.Writer) error {
	startProtection()
	s := newSession()
	s.connect(in, out)
	current.Store(s)
	go s.readLoop()
	defer s.release()
	// Registered after release, so it runs first: while Shutdown/lastResort
	// are cleaning up, wait for them (they exit the process) instead of
	// releasing and returning underneath them.
	defer func() {
		if s.stopping.Load() {
			select {}
		}
	}()
	done := make(chan struct{})
	defer close(done)
	go s.lastResort(done)
	go s.watchStalls(done)
	go func() {
		// A heartbeat every 20s carries the runner's memory. Memory is
		// checked every 2s, and pressure is reported at once: a VM running
		// out of memory can stall the helper before its next heartbeat, and
		// the client must know why it lost it (see Heartbeat.Pressure).
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		lastSent, lastRegular, pressured := time.Now(), time.Now(), false
		for {
			select {
			case <-done:
				return
			case <-t.C:
				hb := memStats()
				// By elapsed time, not ticks: a stalled ticker drops them.
				regular := time.Since(lastRegular) >= 20*time.Second
				onset := hb.Pressure() != "" && !pressured // the first sign goes out at once
				pressured = hb.Pressure() != ""
				if !regular && !onset && (!pressured || time.Since(lastSent) < 6*time.Second) {
					continue
				}
				if regular {
					lastRegular = time.Now()
					// Only owned work counts as activity: probes and idle
					// sessions must not keep an idle VM alive.
					if root := s.owned.Load(); root != nil {
						touchActivity(*root)
					}
				}
				lastSent = time.Now()
				s.beat(&hb)
				_ = s.c.Send(protocol.TypeHeartbeat, hb)
			}
		}
	}()
	for {
		f, err := s.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return err
		}
		if err := s.dispatch(env); err != nil {
			if errors.Is(err, errBye) {
				return nil
			}
			if s.stopping.Load() {
				select {} // shutting down: no error reply (it would block a replay)
			}
			var pe *protoError
			if errors.As(err, &pe) {
				_ = s.c.Send(protocol.TypeError, pe.wire())
				continue
			}
			_ = s.c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeInternal, Message: err.Error()})
			return err
		}
	}
}

// connect sets the session up to talk to the client over in and out.
// Liveness counts bytes arriving, so a large manifest still streaming over
// a slow link is not mistaken for a silent client, and every byte is
// counted for Heartbeat.Received.
func (s *Session) connect(in io.Reader, out io.Writer) {
	s.c = protocol.NewConn(protocol.ProgressReader{R: countReader{in, &s.received}, OnRead: s.heard}, out)
}

// countReader adds every byte read through it to n.
type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countReader) Read(b []byte) (int, error) {
	k, err := c.r.Read(b)
	c.n.Add(int64(k))
	return k, err
}

var errBye = errors.New("bye")

type protoError struct {
	code, msg, detail string
	buildFailed       bool // ErrorMsg.BuildFailed
}

func (e *protoError) Error() string { return e.code + ": " + e.msg }

// wire is the error as sent to the client.
func (e *protoError) wire() protocol.ErrorMsg {
	return protocol.ErrorMsg{Code: e.code, Message: e.msg, Detail: e.detail, BuildFailed: e.buildFailed}
}

func perr(code, format string, a ...any) error {
	return &protoError{code: code, msg: fmt.Sprintf(format, a...)}
}

// imageFailed reports err as IMAGE_FAILED, keeping the code of an error the
// helper already gave one: wrapping it again would print its code twice
// (#250).
func imageFailed(err error) error {
	if pe, ok := err.(*protoError); ok {
		return pe
	}
	return perr(protocol.CodeImageFailed, "%v", err)
}

// uncoded is err's message without the code of an error the helper already
// gave one, for wrapping it under another code.
func uncoded(err error) string {
	if pe, ok := err.(*protoError); ok {
		return pe.msg
	}
	return err.Error()
}

// stoppingNow is systemStopping (tests stand in for the VM shutting down).
var stoppingNow = systemStopping

// cutShort reports a dependency install or image build that failed as the
// VM began shutting down under it (dockerd stopped, the install killed) as
// the runner shutting down, not the build failing: a client's runs waiting
// on that build then hand it to one of them, rather than all building at
// once as for a build that would fail again (#162), and the run is
// replayed elsewhere.
func cutShort(err error) error {
	var pe *protoError
	if errors.As(err, &pe) && (pe.code == protocol.CodeDepsFailed || pe.code == protocol.CodeImageFailed || pe.code == protocol.CodeImageBuildFailed) && stoppingNow() {
		return &protoError{code: protocol.CodeShutdown, msg: "the runner is shutting down: " + pe.msg, detail: pe.detail}
	}
	return err
}

func (s *Session) readLoop() {
	for {
		f, err := s.c.ReadFrame()
		if err != nil {
			s.readErr.Store(err)
			close(s.frames)
			return
		}
		s.heard()
		if f.Kind == protocol.KindMsg {
			if env, err := protocol.Decode(f); err == nil && env.Type == protocol.TypeHeartbeat {
				continue
			}
		}
		select {
		case s.frames <- f:
			continue
		default:
		}
		// The queue is full: until dispatch takes a frame nothing more is
		// read from the client, which must not judge its connection by it.
		s.heldMu.Lock()
		s.holding = s.now()
		s.heldMu.Unlock()
		s.frames <- f
		s.heldMu.Lock()
		s.held += s.now() - s.holding
		s.holding = 0
		s.heldMu.Unlock()
	}
}

// beat adds what a heartbeat says about reading the client: what arrived,
// and how long the helper left it unread.
func (s *Session) beat(hb *protocol.Heartbeat) {
	hb.Received = s.received.Load()
	s.heldMu.Lock()
	held := s.held
	if s.holding != 0 {
		held += s.now() - s.holding
	}
	s.heldMu.Unlock()
	hb.HeldMS = held / int64(time.Millisecond)
}

// next returns the next non-heartbeat frame, enforcing client liveness.
func (s *Session) next() (protocol.Frame, error) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				if e, _ := s.readErr.Load().(error); e != nil {
					return protocol.Frame{}, e
				}
				return protocol.Frame{}, io.EOF
			}
			return f, nil
		case <-t.C:
			if s.clientGone() {
				return protocol.Frame{}, errors.New("client heartbeat timeout")
			}
		}
	}
}

// lastResortAfter is the client silence after which lastResort ends the
// session.
const lastResortAfter = ClientTimeout + 30*time.Second

// OrphanLimit bounds how long a helper outlives the last word from its
// client: by then lastResort has seen the silence (it polls every 5s), even
// past the longest excuse a stall of the helper's own earns (stallCap), and
// has begun tearing the session's processes and containers down before it
// exits. A client that stopped talking to a runner this long ago can count
// on the run it handed over having been torn down there (teardown itself
// may still be finishing).
const OrphanLimit = lastResortAfter + stallCap + 5*time.Second

// lastResort exits the process if the client has been silent well past the
// heartbeat timeout. Normal paths notice earlier; this covers a helper wedged
// in a blocking write to a client that stopped reading (laptop asleep) so the
// kernel releases every flock and the idle watchdog can proceed.
func (s *Session) lastResort(done <-chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if s.silentFor(lastResortAfter) {
			s.teardown()
			os.Exit(4)
		}
	}
}

// clientGone: nothing from the client for ClientTimeout. Not while the
// helper itself has just been stalled (a VM thrashing for memory starves
// every process): the client's heartbeats are then still waiting in the
// pipe, and must get the chance to arrive before it counts as gone.
func (s *Session) clientGone() bool {
	return s.silentFor(ClientTimeout)
}

// stallGrace is how long after its own stall the helper waits for input
// that queued up meanwhile before judging the client; stallCap bounds the
// excuse, so stall after stall can't keep a dead client's run going.
const (
	stallGrace = 15 * time.Second
	stallCap   = 4 * time.Minute
)

// silentFor: nothing from the client for longer than limit, and not
// because the helper itself was just stalled.
func (s *Session) silentFor(limit time.Duration) bool {
	return s.silentForAt(s.now(), limit)
}

// silentForAt is silentFor at the session clock's reading now.
func (s *Session) silentForAt(now int64, limit time.Duration) bool {
	silent := time.Duration(now - s.lastSeen.Load())
	if silent <= limit {
		return false
	}
	return silent > limit+stallCap || !s.recentlyStalledAt(now)
}

// recentlyStalled: the helper was unscheduled for seconds within the last
// stallGrace, or is only now running again (its stall sampler is overdue:
// whichever goroutine wakes first after a stall must not act on it).
func (s *Session) recentlyStalled() bool { return s.recentlyStalledAt(s.now()) }

func (s *Session) recentlyStalledAt(now int64) bool {
	if t := s.stallTick.Load(); t != 0 && time.Duration(now-t) > 3*time.Second {
		return true
	}
	t := s.stalledAt.Load()
	return t != 0 && time.Duration(now-t) < stallGrace
}

// watchStalls samples the helper's own scheduling every second, recording
// when it last went unscheduled for several seconds.
func (s *Session) watchStalls(done <-chan struct{}) {
	last := s.now()
	s.stallTick.Store(last)
	for {
		select {
		case <-done:
			return
		case <-time.After(time.Second):
		}
		now := s.now()
		if time.Duration(now-last) > 3*time.Second {
			s.stalledAt.Store(now)
		}
		s.stallTick.Store(now)
		last = now
	}
}

func (s *Session) dispatch(env protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeHello:
		var h protocol.Hello
		if err := env.Unmarshal(&h); err != nil {
			return err
		}
		return s.hello(h)
	case protocol.TypeAcquire:
		var a protocol.Acquire
		if err := env.Unmarshal(&a); err != nil {
			return err
		}
		return s.acquire(a)
	case protocol.TypeManifest:
		var m protocol.Manifest
		if err := env.Unmarshal(&m); err != nil {
			return err
		}
		return s.syncManifest(m)
	case protocol.TypeDeps:
		var d protocol.Deps
		if err := env.Unmarshal(&d); err != nil {
			return err
		}
		return cutShort(s.deps(d))
	case protocol.TypeExec:
		var e protocol.Exec
		if err := env.Unmarshal(&e); err != nil {
			return err
		}
		return s.exec(e)
	case protocol.TypeFetch:
		var f protocol.Fetch
		if err := env.Unmarshal(&f); err != nil {
			return err
		}
		return s.fetch(f)
	case protocol.TypeImage:
		var m protocol.Image
		if err := env.Unmarshal(&m); err != nil {
			return err
		}
		return cutShort(s.image(m))
	case protocol.TypePull:
		var p protocol.Pull
		if err := env.Unmarshal(&p); err != nil {
			return err
		}
		return s.pull(p)
	case protocol.TypeSettle:
		var st protocol.Settle
		if err := env.Unmarshal(&st); err != nil {
			return nil // no reply is expected: never answer with an error
		}
		s.settle(st)
		return nil
	case protocol.TypeGitSeed:
		var g protocol.GitSeed
		if err := env.Unmarshal(&g); err != nil {
			return err
		}
		return s.gitSeed(g)
	case protocol.TypeGitDrop:
		s.dropGitSeed() // no reply is expected
		return nil
	case protocol.TypeShareWait:
		var w protocol.ShareWait
		if err := env.Unmarshal(&w); err != nil {
			return err
		}
		return s.shareWait(w)
	case protocol.TypeCancel, protocol.TypeWithdraw:
		return nil // nothing running, or waiting to
	case protocol.TypeBye:
		return errBye
	}
	return perr(protocol.CodeBadRequest, "unknown message %q", env.Type)
}

// ExpandRoot expands ~ in a work root against $HOME.
func ExpandRoot(p string) (string, error) {
	if p == "" {
		p = "~/.cache/spotexec"
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

// chmodRoot is os.Chmod, swapped out by tests.
var chmodRoot = os.Chmod

// privateRoot makes the work root private to the runner user, or says why it
// can't be. Synced source and commands live under it, in directories other
// users could otherwise read (0755), so a session never starts without it:
// an existing shared directory the runner can write to but not chmod would
// leave every slot open. The owner is checked first, so a helper running as
// root never changes the mode of another user's directory.
func privateRoot(root string) error {
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	var why string
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		why = "it is not a directory"
	case !ok:
		why = "its owner can't be read"
	case int(st.Uid) != os.Geteuid():
		why = fmt.Sprintf("it is owned by uid %d, not the runner user (uid %d)", st.Uid, os.Geteuid())
	case fi.Mode().Perm() != 0o700:
		chErr := chmodRoot(root, 0o700)
		if fi, err = os.Stat(root); err != nil {
			return err
		}
		if fi.Mode().Perm()&0o077 == 0 {
			return nil
		}
		why = fmt.Sprintf("its mode is %#o, open to other users", fi.Mode().Perm())
		if chErr != nil {
			why += fmt.Sprintf(", and making it 0700 failed (%v)", chErr)
		}
	default:
		return nil
	}
	return perr(protocol.CodeBadRequest, "work root %s can't be made private: %s; point work_root at a directory the runner user owns, or chmod 700 it", root, why)
}

func (s *Session) hello(h protocol.Hello) error {
	if h.Proto != protocol.Version {
		return perr(protocol.CodeBadRequest, "protocol mismatch: client %d, helper %d", h.Proto, protocol.Version)
	}
	root, err := ExpandRoot(h.WorkRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	// Resolved, so process cleanup (which matches /proc/<pid>/cwd, always a
	// resolved path) and output path rewriting see the same paths.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if err := privateRoot(root); err != nil {
		return err
	}
	s.layout = Layout{Root: root}
	return s.c.Send(protocol.TypeHelloOK, protocol.HelloOK{
		Proto: protocol.Version, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), WorkRoot: root, Draining: draining(root),
	})
}

// release drops every lock (also on abnormal exit).
func (s *Session) release() { s.releaseLocks(false) }

// releaseLocks is release; only teardown's goes ahead once it has begun.
func (s *Session) releaseLocks(teardown bool) {
	s.owned.Store(nil)
	// Shutdown (a signal) and Serve's own exit may both get here: the
	// second waits for the first to finish rather than exiting mid-release.
	s.relMu.Lock()
	defer s.relMu.Unlock()
	if !teardown {
		s.parkIfStopping()
	}
	if s.exiting.Swap(true) {
		return
	}
	held := len(s.tokens) > 0
	if s.slotDir != "" {
		_ = os.Remove(filepath.Join(s.slotDir, "holder.json"))
	}
	for i, t := range s.tokens {
		if t != nil {
			_ = os.Remove(s.layout.TokenHolder(s.tokenIdxAt(i)))
			t.Unlock()
		}
	}
	s.tokens = nil
	// The idle clock starts when work ends: touched before the lifecycle
	// lock goes, so the watchdog never sees the VM free with the clock
	// still at the last heartbeat.
	if held {
		touchActivity(s.layout.Root)
	}
	if s.run != nil {
		_ = os.Remove(s.layout.RunMem(s.runIdx))
	}
	s.run.Unlock()
	s.run, s.runIdx = nil, 0
	s.gate.Unlock()
	s.slotLock.Unlock()
	s.lifecycle.Unlock()
}

func (s *Session) tokenIdxAt(i int) int {
	if len(s.tokens) == 1 {
		return s.tokenIdx
	}
	return i + 1
}
