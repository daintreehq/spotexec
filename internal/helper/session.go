// Package helper implements spotexec-helper, the small remote binary that the
// CLI drives over an SSH channel. It owns slot locking, forward-sync
// application, dependency hydration, process-group execution, and mutation
// detection for write-back.
package helper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	c        *protocol.Conn
	layout   Layout
	frames   chan protocol.Frame
	readErr  atomic.Value // error
	lastSeen atomic.Int64
	// stalledAt is when the helper last found itself unscheduled for
	// seconds (see watchStalls).
	stalledAt atomic.Int64
	stallTick atomic.Int64 // the stall sampler's latest run
	// owned is the work root while this session holds a slot (read by the
	// heartbeat goroutine, so not s.tokens / s.layout directly).
	owned    atomic.Pointer[string]
	relMu    sync.Mutex
	procMu   sync.Mutex  // held while a process or container is being started
	stopping atomic.Bool // Shutdown/lastResort own cleanup and exit

	lifecycle *flock
	tokens    []*flock
	tokenIdx  int
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
	}
	stored      map[string]storedEntry
	exiting     atomic.Bool
	child       atomic.Int64 // running command's process group, if any
	gate        *flock
	containers  sync.Map    // running container name -> dockerRunner
	interrupted atomic.Bool // a spot interruption notice arrived
	watching    bool
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
		s.stopping.Store(true)
		s.procMu.Lock() // wait out a start in progress; none begins after this
		s.procMu.Unlock()
		if pg := int(s.child.Load()); pg > 0 {
			killGroup(pg, 2*time.Second)
		}
		if s.workspace != "" {
			reapWorkspace(s.workspace, 2*time.Second)
		}
		s.removeContainers(false)
		if s.slotDir != "" {
			_ = os.RemoveAll(filepath.Join(s.slotDir, "run"))
		}
		s.release()
	}
	os.Exit(code)
}

func Serve(in io.Reader, out io.Writer) error {
	startProtection()
	s := &Session{frames: make(chan protocol.Frame, 64)}
	// Liveness counts bytes arriving, so a large manifest still streaming
	// over a slow link is not mistaken for a silent client.
	s.c = protocol.NewConn(protocol.ProgressReader{R: in, OnRead: func() { s.lastSeen.Store(time.Now().UnixNano()) }}, out)
	current.Store(s)
	s.lastSeen.Store(time.Now().UnixNano())
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
	go s.lastResort()
	done := make(chan struct{})
	defer close(done)
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
				_ = s.c.Send(protocol.TypeError, protocol.ErrorMsg{Code: pe.code, Message: pe.msg, Detail: pe.detail})
				continue
			}
			_ = s.c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeInternal, Message: err.Error()})
			return err
		}
	}
}

var errBye = errors.New("bye")

type protoError struct{ code, msg, detail string }

func (e *protoError) Error() string { return e.code + ": " + e.msg }

func perr(code, format string, a ...any) error {
	return &protoError{code: code, msg: fmt.Sprintf(format, a...)}
}

func (s *Session) readLoop() {
	for {
		f, err := s.c.ReadFrame()
		if err != nil {
			s.readErr.Store(err)
			close(s.frames)
			return
		}
		s.lastSeen.Store(time.Now().UnixNano())
		if f.Kind == protocol.KindMsg {
			if env, err := protocol.Decode(f); err == nil && env.Type == protocol.TypeHeartbeat {
				continue
			}
		}
		s.frames <- f
	}
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

// lastResort exits the process if the client has been silent well past the
// heartbeat timeout. Normal paths notice earlier; this covers a helper wedged
// in a blocking write to a client that stopped reading (laptop asleep) so the
// kernel releases every flock and the idle watchdog can proceed.
func (s *Session) lastResort() {
	for {
		time.Sleep(5 * time.Second)
		if s.silentFor(ClientTimeout + 30*time.Second) {
			s.stopping.Store(true)
			s.procMu.Lock()
			s.procMu.Unlock()
			if pg := int(s.child.Load()); pg > 0 {
				killGroup(pg, 2*time.Second)
			}
			if s.workspace != "" {
				reapWorkspace(s.workspace, 2*time.Second)
			}
			s.removeContainers(false)
			if s.slotDir != "" {
				_ = os.RemoveAll(filepath.Join(s.slotDir, "run"))
			}
			s.release()
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
	silent := time.Since(time.Unix(0, s.lastSeen.Load()))
	if silent <= limit {
		return false
	}
	return silent > limit+stallCap || !s.recentlyStalled()
}

// recentlyStalled: the helper was unscheduled for seconds within the last
// stallGrace, or is only now running again (its stall sampler is overdue:
// whichever goroutine wakes first after a stall must not act on it).
func (s *Session) recentlyStalled() bool {
	now := time.Now()
	if t := s.stallTick.Load(); t != 0 && now.Sub(time.Unix(0, t)) > 3*time.Second {
		return true
	}
	return now.Sub(time.Unix(0, s.stalledAt.Load())) < stallGrace
}

// watchStalls samples the helper's own scheduling every second, recording
// when it last went unscheduled for several seconds.
func (s *Session) watchStalls(done <-chan struct{}) {
	last := time.Now()
	s.stallTick.Store(last.UnixNano())
	for {
		select {
		case <-done:
			return
		case <-time.After(time.Second):
		}
		now := time.Now()
		if now.Sub(last) > 3*time.Second {
			s.stalledAt.Store(now.UnixNano())
		}
		s.stallTick.Store(now.UnixNano())
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
		return s.deps(d)
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
		return s.image(m)
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
	case protocol.TypeCancel:
		return nil // nothing running
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

func (s *Session) hello(h protocol.Hello) error {
	if h.Proto != protocol.Version {
		return perr(protocol.CodeBadRequest, "protocol mismatch: client %d, helper %d", h.Proto, protocol.Version)
	}
	root, err := ExpandRoot(h.WorkRoot)
	if err != nil {
		return err
	}
	// Private to the runner user: synced source and commands live here.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(root, 0o700)
	// Resolved, so process cleanup (which matches /proc/<pid>/cwd, always a
	// resolved path) and output path rewriting see the same paths.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	s.layout = Layout{Root: root}
	return s.c.Send(protocol.TypeHelloOK, protocol.HelloOK{
		Proto: protocol.Version, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), WorkRoot: root, Draining: draining(root),
	})
}

// release drops every lock (also on abnormal exit).
func (s *Session) release() {
	s.owned.Store(nil)
	// Shutdown (a signal) and Serve's own exit may both get here: the
	// second waits for the first to finish rather than exiting mid-release.
	s.relMu.Lock()
	defer s.relMu.Unlock()
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
	s.gate.Unlock()
	s.slotLock.Unlock()
	s.lifecycle.Unlock()
	if held {
		touchActivity(s.layout.Root) // the idle clock starts when work ends
	}
}

func (s *Session) tokenIdxAt(i int) int {
	if len(s.tokens) == 1 {
		return s.tokenIdx
	}
	return i + 1
}
