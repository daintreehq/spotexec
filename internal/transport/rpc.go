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

	"github.com/daintreehq/spotexec/internal/helper"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// Session is a live RPC channel to spotexec-helper.
type Session struct {
	Conn    *protocol.Conn
	Hello   protocol.HelloOK
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.Closer
	stderr  *lockedBuf
	frames  chan protocol.Frame
	errMu   sync.Mutex
	err     error
	done    chan struct{}
	closing chan struct{} // closed by Close/Abort: stop delivering frames
	stopped sync.Once
	once    sync.Once
	seen    atomic.Int64
	silent  atomic.Bool  // the watchdog gave up on the helper
	limit   atomic.Int64 // SilenceLimit when the session opened, plus any current grace (ns)
	// onSilent, if set, is asked whether to keep waiting when the helper
	// goes silent past the limit, once per silence (see SetOnSilent).
	onSilent atomic.Pointer[func(time.Duration) time.Duration]
	pressure atomic.Pointer[pressureSeen] // the latest heartbeat showing memory pressure
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

// Handshake timeouts. Both are shorter than the callers' own probe timeouts
// (30s), so a wedged link is detected and replaced here rather than by
// their deadline. A reused master normally answers in milliseconds; one that
// doesn't is most likely dead (the laptop slept or changed networks while it
// persisted), so it gets less time before Open dials afresh.
var (
	handshakeTimeout       = 10 * time.Second
	reusedHandshakeTimeout = 5 * time.Second
)

// Open starts `spotexec-helper rpc` over SSH and performs the handshake.
func (c *Client) Open(ctx context.Context) (*Session, error) {
	pid, dialed, err := c.ensureMaster(ctx)
	if err != nil {
		return nil, err
	}
	s, err := c.openHelper(ctx, dialed)
	if err == nil || !IsConnError(err) || ctx.Err() != nil {
		return s, err
	}
	// The master answered `-O check` but couldn't carry a session. The check
	// only asks the local master process, which outlives its connection:
	// after a sleep or a network change it is still "alive" but nothing
	// gets through. Retire it so the next attempt dials afresh, and if it
	// was an old one, dial now and try once more.
	c.dropMaster(pid)
	if dialed {
		return nil, err
	}
	if pid, _, err = c.ensureMaster(ctx); err != nil {
		return nil, err
	}
	s, err = c.openHelper(ctx, true)
	if err != nil && IsConnError(err) && ctx.Err() == nil {
		c.dropMaster(pid)
	}
	return s, err
}

// openHelper installs the helper if needed and opens a session on it.
// fresh says the master was just dialed (so it is known to be live).
func (c *Client) openHelper(ctx context.Context, fresh bool) (*Session, error) {
	probe, hs := probeTimeout, handshakeTimeout
	if !fresh {
		probe, hs = reusedHandshakeTimeout, reusedHandshakeTimeout
	}
	path, err := c.ensureHelper(ctx, false, probe)
	if err != nil {
		return nil, err
	}
	s, err := c.open(ctx, path, hs)
	if err != nil && !IsConnError(err) && ctx.Err() == nil {
		// Stale marker (helper removed remotely, or work_root wiped): reinstall.
		c.ForgetHelper()
		if path, err2 := c.ensureHelper(ctx, true, probe); err2 == nil {
			s, err = c.open(ctx, path, hs)
		} else {
			err = err2 // why the reinstall failed is the useful error
		}
	}
	return s, err
}

func (c *Client) open(ctx context.Context, path string, handshake time.Duration) (*Session, error) {
	cmd := exec.Command("ssh", c.Args("exec "+path+" rpc")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Session{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &lockedBuf{}, frames: make(chan protocol.Frame, 256), done: make(chan struct{}), closing: make(chan struct{})}
	cmd.Stderr = s.stderr
	// A master holding a stuck channel keeps the stderr pipe open after the
	// client is gone: don't let that hold up Wait (and so Dead) for long.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.Conn = protocol.NewConn(protocol.ProgressReader{R: stdout, OnRead: func() { s.seen.Store(time.Now().UnixNano()) }}, stdin)
	s.seen.Store(time.Now().UnixNano())
	go s.readLoop()
	go s.heartbeat()
	s.limit.Store(int64(SilenceLimit))
	go s.watchdog()
	if err := s.Conn.Send(protocol.TypeHello, protocol.Hello{Proto: protocol.Version, Version: helper.Version, WorkRoot: c.WorkRoot}); err != nil {
		s.Close()
		return nil, s.wrap(err)
	}
	hctx, cancel := context.WithTimeout(ctx, handshake)
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
		if errors.Is(err, context.DeadlineExceeded) {
			// No handshake in time while our caller still waits: a wedged
			// link. Don't Close (Bye would let a slow helper exit 0 and look
			// broken); Open replaces the master.
			s.Abort()
			return nil, &ConnError{Target: c.T.String(), Msg: "no handshake from the helper", Err: err}
		}
		s.Close()
		switch code := s.exitCode(); {
		case code == 0:
			// The helper started and ended cleanly but no handshake (stray
			// output from a shell profile, or an incompatible helper): not
			// a connection problem, so Open may reinstall it.
			rerr := err
			if IsConnError(rerr) { // the channel closed: use the read error underneath
				s.errMu.Lock()
				rerr = s.err
				s.errMu.Unlock()
				if rerr == nil || errors.Is(rerr, io.EOF) {
					rerr = io.ErrUnexpectedEOF
				}
			}
			return nil, fmt.Errorf("helper ended without a handshake: %w", s.wrap(rerr))
		case code == 255:
			return nil, &ConnError{Target: c.T.String(), Msg: s.stderr.String(), Err: err}
		case code > 0:
			// ssh connected but the helper didn't start (missing after a
			// wiped work_root, wrong architecture on a reused address...):
			// not a connection error, so Open reinstalls it.
			return nil, &helperStartError{code: code, stderr: strings.TrimSpace(s.stderr.String())}
		default:
			// No answer and no exit: a wedged link (Open replaces the
			// master).
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
				s.notePressure(env)
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

// SilenceLimit is how long a session may hear nothing from the helper. The
// helper sends a heartbeat every 20s whatever it is doing (queueing for a
// slot, syncing, installing, running), so silence well beyond that means it
// or the link is wedged, even if SSH keepalives still pass.
var SilenceLimit = 75 * time.Second

// errSilent is the session error once the watchdog has given up on it.
var errSilent = errors.New("runner stopped responding")

// watchdog ends a session whose helper has gone silent, in any phase: the
// pending read (or write) then fails as a lost connection instead of
// waiting forever.
func (s *Session) watchdog() {
	base := s.Limit()
	t := time.NewTicker(min(5*time.Second, base/4))
	defer t.Stop()
	asked := false // onSilent was asked about the current silence
	for {
		select {
		case <-s.done:
			return
		case <-s.closing:
			return
		case <-t.C:
			silent := time.Since(s.LastSeen())
			if silent <= s.Limit() {
				// Silence within the plain limit, once onSilent was asked,
				// means the helper spoke since (asked only past base): drop
				// any grace, so a later death is noticed as quickly as the
				// first, and ask again next time.
				if asked && silent <= base {
					s.limit.Store(int64(base))
					asked = false
				}
				continue
			}
			if f := s.onSilent.Load(); f != nil && !asked {
				asked = true
				if grace := (*f)(silent); grace > 0 {
					s.limit.Add(int64(grace))
					continue
				}
				if time.Since(s.LastSeen()) <= s.Limit() {
					continue // it spoke up while we asked
				}
			}
			s.setErr(fmt.Errorf("%w (nothing for %s)", errSilent, s.Limit()))
			s.silent.Store(true)
			s.Abort()
			return
		}
	}
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
			msg := "connection to runner lost"
			if errors.Is(err, errSilent) {
				msg = err.Error()
			}
			return protocol.Frame{}, &ConnError{Target: "helper", Msg: msg, Err: s.wrap(err)}
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

// Dead reports whether the SSH channel has closed (or the helper went
// silent and the session was ended).
func (s *Session) Dead() bool {
	if s.silent.Load() {
		return true
	}
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
// Limit is how long the session may currently hear nothing from the
// helper before it is given up on.
func (s *Session) Limit() time.Duration { return time.Duration(s.limit.Load()) }

// SetOnSilent sets f to be asked, once per silence, when the helper has
// been silent past SilenceLimit: it returns how much longer to wait (0 to
// give up now). A helper that speaks again goes back to SilenceLimit, and
// f is asked afresh if it falls silent later. f runs on the watchdog, so
// it should return within seconds.
func (s *Session) SetOnSilent(f func(silent time.Duration) time.Duration) { s.onSilent.Store(&f) }

type pressureSeen struct {
	at   time.Time
	what string
}

// MemoryPressure describes the memory pressure the helper last reported in
// a heartbeat (protocol.Heartbeat.Pressure) if that was within the last
// within, else "". A nil session has seen none.
func (s *Session) MemoryPressure(within time.Duration) string {
	if s == nil {
		return ""
	}
	if p := s.pressure.Load(); p != nil && time.Since(p.at) <= within {
		return p.what
	}
	return ""
}

func (s *Session) notePressure(env protocol.Envelope) {
	var hb protocol.Heartbeat
	if len(env.Body) == 0 || env.Unmarshal(&hb) != nil {
		return
	}
	if what := hb.Pressure(); what != "" {
		s.pressure.Store(&pressureSeen{at: time.Now(), what: what})
	}
}

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

// kill ends the ssh client and closes our ends of its pipes. A session on a
// ControlMaster handed its descriptors to the master, which keeps them open
// while the channel is stuck: closing ours is what ends a pending read or
// write here.
func (s *Session) kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.stdin.Close()
	_ = s.stdout.Close()
}

// Stderr returns captured remote stderr (helper diagnostics).
func (s *Session) Stderr() string { return s.stderr.String() }
