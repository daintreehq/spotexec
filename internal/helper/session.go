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

	"github.com/spotexec/spotexec/internal/protocol"
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

	lifecycle   *flock
	tokens      []*flock
	tokenIdx    int
	slotLock    *flock
	slotDir     string
	workspace   string
	repoID      string
	state       SlotState
	acq         protocol.Acquire
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
func Serve(in io.Reader, out io.Writer) error {
	s := &Session{c: protocol.NewConn(in, out), frames: make(chan protocol.Frame, 64)}
	s.lastSeen.Store(time.Now().UnixNano())
	go s.readLoop()
	defer s.release()
	go s.lastResort()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// Only owned work counts as activity: probes and idle
				// sessions must not keep an idle VM alive.
				if len(s.tokens) > 0 {
					touchActivity(s.layout.Root)
				}
				_ = s.c.Send(protocol.TypeHeartbeat, nil)
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
			var pe *protoError
			if errors.As(err, &pe) {
				_ = s.c.Send(protocol.TypeError, protocol.ErrorMsg{Code: pe.code, Message: pe.msg})
				continue
			}
			_ = s.c.Send(protocol.TypeError, protocol.ErrorMsg{Code: protocol.CodeInternal, Message: err.Error()})
			return err
		}
	}
}

var errBye = errors.New("bye")

type protoError struct{ code, msg string }

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
		if time.Since(time.Unix(0, s.lastSeen.Load())) > ClientTimeout+30*time.Second {
			if pg := int(s.child.Load()); pg > 0 {
				killGroup(pg, 2*time.Second)
			}
			if s.workspace != "" {
				reapWorkspace(s.workspace, 2*time.Second)
			}
			s.removeContainers(false)
			os.Exit(4)
		}
	}
}

func (s *Session) clientGone() bool {
	return time.Since(time.Unix(0, s.lastSeen.Load())) > ClientTimeout
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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	s.layout = Layout{Root: root}
	return s.c.Send(protocol.TypeHelloOK, protocol.HelloOK{
		Proto: protocol.Version, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), WorkRoot: root, Draining: draining(root),
	})
}

// release drops every lock (also on abnormal exit).
func (s *Session) release() {
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
