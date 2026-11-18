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
	lost    chan struct{} // closed when the session ends: silence or a closed channel
	lostMu  sync.Once
	born    time.Time    // seen and woke count from here, on the monotonic clock (which stops while this machine sleeps)
	seen    atomic.Int64 // when the helper last sent anything, as time since born (ns)
	woke    atomic.Int64 // when this process last came back from a suspension, as time since born (ns)
	backed  atomic.Bool  // readLoop is blocked handing a frame to a full buffer
	// away is the longest suspension of this process (the machine asleep,
	// or the process stopped) the session hasn't recovered from: the helper
	// hasn't been heard well after it ended (ns).
	away atomic.Int64
	// clk is the last reading of the clocks the watchdog compares.
	clk struct {
		sync.Mutex
		boot, mono int64 // sinceBoot, and time since born
		bootOK     bool  // boot was read
	}
	hooking  atomic.Bool   // the watchdog is in the silence hook
	period   time.Duration // the watchdog's tick
	patience time.Duration // HelperPatience when the session opened
	silent   atomic.Bool   // the watchdog gave up on the helper
	limit    atomic.Int64  // SilenceLimit when the session opened, plus any current grace (ns)
	// onSilent, if set, is asked whether to keep waiting when the helper
	// goes silent past the limit, once per silence (see SetOnSilent).
	onSilent atomic.Pointer[func(time.Duration) time.Duration]
	pressure atomic.Pointer[pressureSeen]  // the latest heartbeat showing memory pressure
	memory   atomic.Pointer[MemoryReading] // the latest heartbeat's memory reading
	sent     atomic.Int64                  // bytes written to the helper so far
	writing  atomic.Bool                   // a write to the helper is under way
	up       uploadWatch                   // whether what we send is getting through
	// retired is closed once the master a stalled upload ran on has been
	// dealt with (see UploadStalled).
	retired chan struct{}
	// client and master are the connection the session runs on: the
	// master's pid (0 if unknown), retired when an upload stalls on it.
	client *Client
	master atomic.Int64
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
	s, err := c.openHelper(ctx, dialed, pid)
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
	s, err = c.openHelper(ctx, true, pid)
	if err != nil && IsConnError(err) && ctx.Err() == nil {
		c.dropMaster(pid)
	}
	return s, err
}

// openHelper installs the helper if needed and opens a session on it.
// fresh says the master was just dialed (so it is known to be live); pid
// is that master's.
func (c *Client) openHelper(ctx context.Context, fresh bool, pid int) (*Session, error) {
	probe, hs := probeTimeout, handshakeTimeout
	if !fresh {
		probe, hs = reusedHandshakeTimeout, reusedHandshakeTimeout
	}
	path, err := c.ensureHelper(ctx, false, probe)
	if err != nil {
		return nil, err
	}
	s, err := c.open(ctx, path, hs, pid)
	if err != nil && !IsConnError(err) && ctx.Err() == nil {
		// Stale marker (helper removed remotely, or work_root wiped): reinstall.
		c.ForgetHelper()
		if path, err2 := c.ensureHelper(ctx, true, probe); err2 == nil {
			s, err = c.open(ctx, path, hs, pid)
		} else {
			err = err2 // why the reinstall failed is the useful error
		}
	}
	return s, err
}

func (c *Client) open(ctx context.Context, path string, handshake time.Duration, master int) (*Session, error) {
	cmd := exec.Command("ssh", c.Args("exec "+path+" rpc")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Session{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &lockedBuf{}, frames: make(chan protocol.Frame, 256), done: make(chan struct{}), closing: make(chan struct{}), lost: make(chan struct{}), born: time.Now(), client: c, retired: make(chan struct{})}
	s.master.Store(int64(master))
	cmd.Stderr = s.stderr
	// A master holding a stuck channel keeps the stderr pipe open after the
	// client is gone: don't let that hold up Wait (and so Dead) for long.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.patience = HelperPatience
	s.period = min(5*time.Second, SilenceLimit/4)
	s.clk.boot, s.clk.bootOK = sinceBoot()
	s.Conn = protocol.NewConn(protocol.ProgressReader{R: stdout, OnRead: s.heard}, countWriter{stdin, s})
	s.heard()
	s.up.window, s.up.floor = UploadStallWindow, UploadStallFloor
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
	// The master looked up before the session started may have been
	// replaced since: then which one carries it is unknown, and a stall
	// retires none.
	if cur, ok := c.masterPID(ctx); !ok || cur != master {
		s.master.Store(0)
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

// heard notes that the helper just sent something. Silence is measured on
// the monotonic clock: a wall clock that jumps (NTP, a laptop waking) must
// not make a helper look silent, or alive.
func (s *Session) heard() { s.seen.Store(int64(time.Since(s.born))) }

func (s *Session) readLoop() {
	for {
		f, err := s.Conn.ReadFrame()
		if err != nil {
			// Waking can bring the end of the connection before the
			// watchdog's next tick: note the suspension first. The end is
			// not word from the helper: it mustn't clear one (recovered).
			s.observe(0)
			s.setErr(err)
			s.lostMu.Do(func() { close(s.lost) })
			close(s.frames)
			_ = s.cmd.Wait()
			close(s.done)
			return
		}
		s.heard()
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
			continue
		case <-s.closing:
			continue
		default:
		}
		// The buffer is full: until we take this frame we read nothing, so
		// the helper can't be heard (it blocks writing to us). That is not
		// its silence: count it from when we listen again.
		s.backed.Store(true)
		select {
		case s.frames <- f:
		case <-s.closing:
		}
		s.heard()
		s.backed.Store(false)
		// Nor was what it read from us meanwhile: it may have been
		// waiting on us too.
		s.up.reset()
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

// HelperPatience is how long the helper waits for a silent client before it
// may give up on the session (helper.ClientTimeout): a session lost after
// this process was suspended for longer (the machine asleep, #142) was most
// likely lost to that, however healthy the runner.
var HelperPatience = helper.ClientTimeout

// bootSkew moves the clock that counts sleep forward (tests: a machine that
// slept).
var bootSkew atomic.Int64

// errSilent is the session error once the watchdog has given up on it.
var errSilent = errors.New("runner stopped responding")

// watchdog ends a session whose helper has gone silent, in any phase: the
// pending read (or write) then fails as a lost connection instead of
// waiting forever.
//
// Time this process spends suspended (observe) is not the helper's
// silence: the helper gets the plain limit again to speak after it, and
// after one longer than its patience no grace for a VM still running, since
// the helper has most likely given up on this client meanwhile. (Not ended
// at once: a helper stalled itself meanwhile excuses its client longer.)
func (s *Session) watchdog() {
	base := s.Limit()
	t := time.NewTicker(s.period)
	defer t.Stop()
	asked := false    // onSilent was asked about the current silence
	var askedAt int64 // s.seen when it was
	for {
		select {
		case <-s.done:
			return
		case <-s.closing:
			return
		case <-t.C:
			if s.observe(0) > 0 {
				// Any grace was for a silence this suspension interrupted.
				s.limit.Store(int64(base))
				asked = false
			}
			// The helper sent something since onSilent was asked: that
			// silence is over. Drop its grace, so a later one is noticed
			// as quickly as the first, and ask afresh then.
			seen := s.seen.Load() // before judging the silence: progress after it rearms
			if asked && seen != askedAt {
				s.limit.Store(int64(base))
				asked = false
			}
			s.recovered(seen, base/2)
			// The verdict is the session's error, set while the watch is
			// still armed: a caller that ends the upload either sees it or
			// heads off any later one.
			if s.up.judge(func(why string) bool {
				return s.firstErr(fmt.Errorf("%w: %s", errUploadStalled, why))
			}) {
				s.silent.Store(true)
				// The master carrying it most likely carries nothing else
				// either: no new session may go onto it, a retry's included.
				if s.client != nil {
					s.client.retireMaster(int(s.master.Load()))
				}
				close(s.retired)
				s.lostMu.Do(func() { close(s.lost) })
				s.Abort()
				return
			}
			silent := s.silence()
			if silent <= s.Limit() {
				continue
			}
			if f := s.onSilent.Load(); f != nil && !asked && s.Suspended() == 0 {
				asked, askedAt = true, seen
				hookStart := time.Now()
				s.hooking.Store(true)
				grace := (*f)(silent)
				s.hooking.Store(false)
				// A sleep while it ran counts; its own running time doesn't.
				if s.observe(time.Since(hookStart)) > 0 {
					s.limit.Store(int64(base))
					asked = false
					continue
				}
				if grace > 0 {
					// From now: the hook ran after the limit was passed.
					s.limit.Store(int64(s.silence() + grace))
					continue
				}
				if s.silence() <= s.Limit() {
					continue // it spoke up while we asked
				}
			}
			s.setErr(fmt.Errorf("%w (nothing for %s%s)", errSilent, s.Limit().Round(time.Second), s.suspendedNote()))
			s.silent.Store(true)
			s.lostMu.Do(func() { close(s.lost) })
			s.Abort()
			return
		}
	}
}

// suspendedNote is what a lost session's error adds about a suspension of
// this process that most likely lost it ("" for none).
func (s *Session) suspendedNote() string {
	if d := s.Suspended(); d > 0 {
		return fmt.Sprintf(" after this machine was asleep or sx was stopped for %s", d.Round(time.Second))
	}
	return ""
}

// now is the session's monotonic clock: time since it opened.
func (s *Session) now() int64 { return int64(time.Since(s.born)) }

// minSuspension is the least that counts as a suspension, so that a
// loaded machine's late ticks don't.
const minSuspension = 5 * time.Second

// observe reports how long this process was just suspended (0 if it
// wasn't), from the clocks' advance since their last reading, and records
// it for the session. The machine asleep advances the clock that counts
// sleep (sinceBoot) beyond the monotonic one. The process stopped shows as
// the watchdog's tick being late: the monotonic clock's advance beyond the
// tick period, less busy, the time the watchdog spent in the silence hook
// (and not judged at all while it is in there: it may simply be slow).
func (s *Session) observe(busy time.Duration) time.Duration {
	s.clk.Lock()
	defer s.clk.Unlock()
	boot, bootOK := sinceBoot()
	mono := s.now()
	monoGap := time.Duration(mono - s.clk.mono)
	var gap time.Duration
	if bootOK && s.clk.bootOK {
		gap = time.Duration(boot-s.clk.boot) - monoGap
	}
	if !s.hooking.Load() {
		gap = max(gap, monoGap-s.period-busy)
	}
	s.clk.boot, s.clk.bootOK, s.clk.mono = boot, bootOK, mono
	if gap <= max(s.period, minSuspension) {
		return 0
	}
	s.up.reset() // no upload got through while we were away
	s.woke.Store(mono)
	s.away.Store(max(s.away.Load(), int64(gap)))
	return gap
}

// recovered clears the suspension once the helper has been heard (at seen)
// more than after its end: the session outlived it. Serialised with
// observe, so a suspension recorded meanwhile is kept.
func (s *Session) recovered(seen int64, after time.Duration) {
	s.clk.Lock()
	defer s.clk.Unlock()
	if s.away.Load() > 0 && seen > s.woke.Load()+int64(after) {
		s.away.Store(0)
	}
}

// Suspended is how long this process was suspended (the machine asleep, or
// the process stopped) while the session was open, when that was longer
// than the helper may wait for a silent client (HelperPatience) and the
// helper hasn't been heard well after it since: a session lost then was
// most likely lost to the suspension, not to the runner. Otherwise 0.
func (s *Session) Suspended() time.Duration {
	if s == nil {
		return 0
	}
	if d := time.Duration(s.away.Load()); d > s.patience {
		return d
	}
	return 0
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

func (s *Session) setErr(err error) { s.firstErr(err) }

// firstErr sets the session's error, if it has none yet, and reports
// whether it did.
func (s *Session) firstErr(err error) bool {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
		return true
	}
	return false
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
			return protocol.Frame{}, s.lostErr()
		}
		return f, nil
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

func (s *Session) lostErr() error {
	s.errMu.Lock()
	err := s.err
	s.errMu.Unlock()
	if errors.Is(err, io.EOF) || err == nil {
		err = io.ErrUnexpectedEOF
	}
	msg := "connection to runner lost" + s.suspendedNote()
	if errors.Is(err, errSilent) || errors.Is(err, errUploadStalled) {
		msg = err.Error()
	}
	return &ConnError{Target: "helper", Msg: msg, Err: s.wrap(err)}
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

// Silent reports whether the session ended because the watchdog gave up
// on the helper (silent past its limit, or an upload that stopped getting
// through), rather than because the connection closed or this side ended
// it: a loss that stopping the local ssh client can't have caused. The
// first error the session recorded decides, so a watchdog tick that
// overlaps the connection ending doesn't claim the loss.
func (s *Session) Silent() bool {
	if s == nil {
		return false
	}
	s.errMu.Lock()
	err := s.err
	s.errMu.Unlock()
	return errors.Is(err, errSilent) || errors.Is(err, errUploadStalled)
}

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

// MemoryReading is the VM's memory as a heartbeat last reported it, and
// when that heartbeat arrived.
type MemoryReading struct {
	At               time.Time
	AvailMB, TotalMB int64
}

// LastMemory is the memory reading of the latest heartbeat that carried
// one; ok is false when none has (an older helper, or a nil session).
func (s *Session) LastMemory() (m MemoryReading, ok bool) {
	if s == nil {
		return m, false
	}
	if p := s.memory.Load(); p != nil {
		return *p, true
	}
	return m, false
}

func (s *Session) notePressure(env protocol.Envelope) {
	var hb protocol.Heartbeat
	if len(env.Body) == 0 || env.Unmarshal(&hb) != nil {
		return
	}
	now := time.Now()
	if hb.MemTotalMB > 0 {
		s.memory.Store(&MemoryReading{At: now, AvailMB: hb.MemAvailMB, TotalMB: hb.MemTotalMB})
	}
	if what := hb.Pressure(); what != "" {
		s.pressure.Store(&pressureSeen{at: now, what: what})
	}
	// After the memory: a stall it proves must be judged knowing that.
	if hb.Received > 0 {
		s.up.note(s.now(), hb.Received, time.Duration(hb.HeldMS)*time.Millisecond, s.sent.Load(), s.writing.Load())
	}
}

// LastSeen is when the helper last sent anything (heartbeats included).
// While the frame buffer is full (we're not reading fast enough, e.g.
// output piped into a paused pager), nothing more can be read from the
// helper, so it isn't held to its silence; once frames are taken again it
// is, from then. Frames merely queued, with room for more, excuse nothing:
// readLoop still hears the helper's heartbeats, or their absence.
func (s *Session) LastSeen() time.Time {
	if s.backed.Load() {
		return time.Now()
	}
	return s.born.Add(time.Duration(s.seen.Load()))
}

// silence is how long the helper has sent nothing, as LastSeen judges it,
// not counting time this process was suspended: the helper gets the full
// limit again to speak after a suspension.
func (s *Session) silence() time.Duration {
	if s.backed.Load() {
		return 0
	}
	return time.Duration(s.now() - max(s.seen.Load(), s.woke.Load()))
}

// Lost is closed once the session has ended, because the helper went
// silent past its limit or the channel closed: a caller waiting on
// something other than the session (a local lock) stops then, with
// LostErr.
func (s *Session) Lost() <-chan struct{} { return s.lost }

// LostErr is the error the session's reads end with once it is lost.
func (s *Session) LostErr() error { return s.lostErr() }

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
