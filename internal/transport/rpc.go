package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spotexec/spotexec/internal/helper"
	"github.com/spotexec/spotexec/internal/protocol"
)

// Session is a live RPC channel to spotexec-helper.
type Session struct {
	Conn    *protocol.Conn
	Hello   protocol.HelloOK
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *lockedBuf
	frames  chan protocol.Frame
	errMu   sync.Mutex
	err     error
	done    chan struct{}
	closing chan struct{} // closed by Close/Abort: stop delivering frames
	stopped sync.Once
	once    sync.Once
	seen    atomic.Int64
	// OnEvent receives helper events that arrive while waiting for replies.
	OnEvent func(protocol.Event)
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() < 64<<10 {
		l.b.Write(p)
	}
	return len(p), nil
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(l.b.String())
}

// Open starts `spotexec-helper rpc` over SSH and performs the handshake.
func (c *Client) Open(ctx context.Context) (*Session, error) {
	if err := c.EnsureMaster(ctx); err != nil {
		return nil, err
	}
	path, err := c.EnsureHelper(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := c.open(ctx, path)
	if err != nil && !IsConnError(err) && ctx.Err() == nil {
		// Stale marker (helper removed remotely, or work_root wiped): reinstall.
		c.ForgetHelper()
		if path, err2 := c.EnsureHelper(ctx, true); err2 == nil {
			s, err = c.open(ctx, path)
		} else {
			err = err2 // why the reinstall failed is the useful error
		}
	}
	return s, err
}

func (c *Client) open(ctx context.Context, path string) (*Session, error) {
	cmd := exec.Command("ssh", c.Args("exec "+path+" rpc")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Session{cmd: cmd, stdin: stdin, stderr: &lockedBuf{}, frames: make(chan protocol.Frame, 256), done: make(chan struct{}), closing: make(chan struct{})}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.Conn = protocol.NewConn(protocol.ProgressReader{R: stdout, OnRead: func() { s.seen.Store(time.Now().UnixNano()) }}, stdin)
	s.seen.Store(time.Now().UnixNano())
	go s.readLoop()
	go s.heartbeat()
	if err := s.Conn.Send(protocol.TypeHello, protocol.Hello{Proto: protocol.Version, Version: helper.Version, WorkRoot: c.WorkRoot}); err != nil {
		s.Close()
		return nil, s.wrap(err)
	}
	// Shorter than the callers' own probe timeouts (20-30s), so a wedged
	// master is detected and dropped here rather than by their deadline.
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	env, err := s.Expect(hctx, protocol.TypeHelloOK)
	if err != nil {
		if ctx.Err() != nil {
			s.Abort()
			return nil, ctx.Err()
		}
		var re *RemoteError
		if errors.As(err, &re) {
			s.Close() // the helper answered: report what it said
			return nil, err
		}
		s.Close()
		switch code := s.exitCode(); {
		case code == 0:
			// The helper started and ended cleanly but no handshake (stray
			// output from a shell profile, or an incompatible helper): not
			// a connection problem, so Open may reinstall it.
			return nil, s.wrap(err)
		case code == 255:
			return nil, &ConnError{Target: c.T.String(), Msg: s.stderr.String(), Err: err}
		case code > 0:
			// ssh connected but the helper didn't start (missing after a
			// wiped work_root, wrong architecture on a reused address...):
			// not a connection error, so Open reinstalls it.
			return nil, &helperStartError{code: code, stderr: strings.TrimSpace(s.stderr.String())}
		default:
			// No answer and no exit: a wedged link. Drop the master so the
			// next attempt dials afresh.
			c.CloseMaster()
			return nil, &ConnError{Target: c.T.String(), Msg: "no handshake from the helper", Err: err}
		}
	}
	if err := env.Unmarshal(&s.Hello); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) exitCode() int {
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		return -1
	}
	if s.cmd.ProcessState != nil {
		return s.cmd.ProcessState.ExitCode()
	}
	return -1
}

func (s *Session) readLoop() {
	for {
		f, err := s.Conn.ReadFrame()
		s.seen.Store(time.Now().UnixNano())
		if err != nil {
			s.setErr(err)
			close(s.frames)
			_ = s.cmd.Wait()
			close(s.done)
			return
		}
		if f.Kind == protocol.KindMsg {
			if env, err := protocol.Decode(f); err == nil && env.Type == protocol.TypeHeartbeat {
				continue
			}
		}
		// Once the session is closing nobody reads frames: discard them, but
		// keep reading to EOF so the ssh process is reaped and done closes.
		select {
		case s.frames <- f:
		case <-s.closing:
		}
	}
}

// helperStartError: ssh worked but the helper binary could not run.
type helperStartError struct {
	code   int
	stderr string
}

func (e *helperStartError) Error() string {
	return fmt.Sprintf("helper did not start (exit %d): %s", e.code, e.stderr)
}

func (s *Session) heartbeat() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if err := s.Conn.Send(protocol.TypeHeartbeat, nil); err != nil {
				return
			}
		}
	}
}

func (s *Session) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *Session) wrap(err error) error {
	if err == nil {
		return nil
	}
	if msg := s.stderr.String(); msg != "" {
		return fmt.Errorf("%w (remote: %s)", err, msg)
	}
	return err
}

// Next returns the next frame from the helper.
func (s *Session) Next(ctx context.Context) (protocol.Frame, error) {
	select {
	case f, ok := <-s.frames:
		if !ok {
			s.errMu.Lock()
			err := s.err
			s.errMu.Unlock()
			if errors.Is(err, io.EOF) || err == nil {
				err = io.ErrUnexpectedEOF
			}
			return protocol.Frame{}, &ConnError{Target: "helper", Msg: "connection to runner lost", Err: s.wrap(err)}
		}
		return f, nil
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

// RemoteError is a structured error reported by the helper.
type RemoteError struct{ protocol.ErrorMsg }

func (e *RemoteError) Error() string { return e.Code + ": " + e.Message }

// Expect waits for a message of type want, surfacing helper errors and
// passing events to OnEvent. Stream frames are discarded.
func (s *Session) Expect(ctx context.Context, want ...string) (protocol.Envelope, error) {
	for {
		f, err := s.Next(ctx)
		if err != nil {
			return protocol.Envelope{}, err
		}
		if f.Kind != protocol.KindMsg {
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return env, err
		}
		switch env.Type {
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			return env, &RemoteError{em}
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			if s.OnEvent != nil {
				s.OnEvent(ev)
			}
			continue
		}
		for _, w := range want {
			if env.Type == w {
				return env, nil
			}
		}
		return env, fmt.Errorf("protocol: unexpected %q (wanted %v)", env.Type, want)
	}
}

// Blob returns a reader over the next tar blob stream.
func (s *Session) Blob(ctx context.Context) *protocol.BlobReader {
	return &protocol.BlobReader{Next: func() (protocol.Frame, error) { return s.Next(ctx) }, Other: func(f protocol.Frame) error {
		// Messages interleaved with a blob: events (a spot notice during
		// write-back) are delivered; an error ends the stream.
		if f.Kind != protocol.KindMsg {
			return nil
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return err
		}
		switch env.Type {
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			if s.OnEvent != nil {
				s.OnEvent(ev)
			}
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			return &RemoteError{em}
		}
		return nil
	}}
}

// Dead reports whether the SSH channel has closed.
func (s *Session) Dead() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// LastSeen is when the helper last sent anything (heartbeats included).
// While our own frame buffer is backed up (we're not reading fast enough,
// e.g. output piped into a paused pager), the helper is evidently alive.
func (s *Session) LastSeen() time.Time {
	if len(s.frames) >= cap(s.frames)/2 {
		return time.Now()
	}
	return time.Unix(0, s.seen.Load())
}

// Close ends the session (releasing the remote slot). The deadline is armed
// before any protocol write so a wedged channel cannot hang the caller.
func (s *Session) Close() {
	s.stopped.Do(func() { close(s.closing) })
	s.once.Do(func() {
		go func() {
			_ = s.Conn.Send(protocol.TypeBye, nil)
			_ = s.stdin.Close()
		}()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			s.kill()
		}
	})
}

// Abort tears the SSH channel down immediately; the helper sees EOF,
// terminates any command and releases the slot.
func (s *Session) Abort() {
	s.stopped.Do(func() { close(s.closing) })
	s.kill()
}

func (s *Session) kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// Stderr returns captured remote stderr (helper diagnostics).
func (s *Session) Stderr() string { return s.stderr.String() }
