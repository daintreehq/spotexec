package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
)

// The tests drive the real transport against sshtest's fake ssh.
func TestMain(m *testing.M) {
	sshtest.Main()
	os.Exit(m.Run())
}

type fakeEnv struct {
	*sshtest.Fake
	c *Client
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	f := sshtest.Install(t)
	ctlMu.Lock()
	ctlDir = ""
	ctlMu.Unlock()
	t.Cleanup(func() {
		ctlMu.Lock()
		ctlDir = ""
		ctlMu.Unlock()
	})
	short(t, &reusedHandshakeTimeout, 500*time.Millisecond)
	short(t, &handshakeTimeout, 2*time.Second)
	short(t, &probeTimeout, 3*time.Second)
	return &fakeEnv{Fake: f, c: New(provider.Target{Host: "box", User: "dev"}, "~/.cache/spotexec")}
}

func short(t *testing.T, v *time.Duration, d time.Duration) {
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func (e *fakeEnv) open(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := e.c.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v\nssh log:\n%s", err, e.Log())
	}
	s.Close()
}

// A reused master whose connection died is replaced: the run reconnects
// once and proceeds.
func TestOpenReplacesDeadMaster(t *testing.T) {
	e := newFakeEnv(t)
	e.open(t) // dials master 101 and installs the helper
	e.Set("wedged-101", "")
	start := time.Now()
	e.open(t)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("recovery took %s", took)
	}
	if got, want := e.Log(), "dial 101\nstop 101\ndial 102\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// Same, when the helper isn't known to be installed (a new CLI version):
// the install's first remote command finds the dead master out.
func TestOpenReplacesDeadMasterBeforeInstall(t *testing.T) {
	e := newFakeEnv(t)
	e.open(t)
	os.RemoveAll(filepath.Join(os.Getenv("HOME"), ".cache", "spotexec", "helpers"))
	e.Set("wedged-101", "")
	e.open(t)
	if got, want := e.Log(), "dial 101\nstop 101\ndial 102\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// A master just dialed that can't carry a session is a real failure: no
// second dial, but it is retired so the next attempt starts afresh.
func TestOpenFreshMasterFailureIsNotRetried(t *testing.T) {
	e := newFakeEnv(t)
	e.Set("wedged-101", "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := e.c.Open(ctx)
	if !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
	if got, want := e.Log(), "dial 101\nstop 101\n"; got != want {
		t.Fatalf("ssh log:\n%s\nwant:\n%s", got, want)
	}
}

// A run that found the old master dead must not retire the replacement
// another run has already dialed.
func TestDropMasterKeepsReplacement(t *testing.T) {
	e := newFakeEnv(t)
	e.Set("master", "102")
	e.c.dropMaster(101)
	if !e.c.MasterAlive(context.Background()) || e.Log() != "" {
		t.Fatalf("replacement master retired: %q", e.Log())
	}
	e.c.dropMaster(102)
	if e.c.MasterAlive(context.Background()) || e.Log() != "stop 102\n" {
		t.Fatalf("dead master kept: %q", e.Log())
	}
}

// A probe command that fails at once is not a dead connection: its own
// error is kept.
func TestProbeFailureKeepsItsError(t *testing.T) {
	e := newFakeEnv(t)
	e.Set("uname-fails", "")
	_, err := e.c.EnsureHelper(context.Background(), false)
	if err == nil || IsConnError(err) || !strings.Contains(err.Error(), "uname: boom") {
		t.Fatalf("want the remote error, got %v", err)
	}
}

func (e *fakeEnv) session(t *testing.T) *Session {
	t.Helper()
	s, err := e.c.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Abort)
	return s
}

// A helper that goes silent ends the session within the silence limit, in
// any phase: waiting for a reply (acquire)...
func TestSilentHelperWhileWaiting(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	_ = s.Conn.Send(protocol.TypeAcquire, protocol.Acquire{RepoID: "r", Wait: true})
	start := time.Now()
	_, err := s.Expect(context.Background(), protocol.TypeAcquired)
	if !IsConnError(err) || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("want a stopped-responding connection error, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %s", took)
	}
	if !s.Dead() {
		t.Fatal("session not dead after the watchdog")
	}
	if !s.Silent() {
		t.Fatal("a session the watchdog gave up on isn't marked silent")
	}
}

// A session that ends because its ssh client was killed here (as an
// interrupt that reaches it does) didn't lose a silent helper (#289), even
// when the watchdog was judging a silence as the connection ended: the
// first error the session recorded decides.
func TestEndedSessionIsNotSilent(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	asked, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	let := func() { released.Do(func() { close(release) }) }
	t.Cleanup(let) // a failed wait mustn't leave the watchdog blocked
	s.SetOnSilent(func(time.Duration) time.Duration {
		once.Do(func() { close(asked) })
		<-release
		return 0 // no grace: the watchdog gives up once it may go on
	})
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog never judged the silence")
	}
	s.Abort() // the connection ends while the watchdog is judging
	deadline := time.Now().Add(5 * time.Second)
	for !s.Dead() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.Dead() {
		t.Fatal("killed session not dead")
	}
	let()
	deadline = time.Now().Add(5 * time.Second)
	for !s.silent.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.silent.Load() {
		t.Fatal("the watchdog didn't go on to give up on the helper")
	}
	if s.Silent() {
		t.Fatalf("a session whose connection ended first is marked silent (%v)", s.LostErr())
	}
	if (*Session)(nil).Silent() {
		t.Fatal("a nil session is silent")
	}
}

// ...or sending to it (an upload the helper never reads).
func TestSilentHelperWhileSending(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	s := e.session(t)
	chunk := make([]byte, 64<<10)
	errc := make(chan error, 1)
	go func() {
		for {
			if err := s.Conn.WriteFrame(protocol.KindBlob, chunk); err != nil {
				errc <- err
				return
			}
		}
	}()
	select {
	case <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("write to a silent helper still blocked")
	}
	if _, err := s.Next(context.Background()); !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
}

// A helper that is slow but alive (heartbeats) is never cut off.
func TestHeartbeatingHelperIsNotSilent(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, 500*time.Millisecond)
	e.Set("rpc-heartbeat", "")
	s := e.session(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Expect(ctx, protocol.TypeAcquired); err != context.DeadlineExceeded {
		t.Fatalf("want our own deadline, got %v", err)
	}
	if s.Dead() {
		t.Fatal("a heartbeating helper was cut off")
	}
}

// An install that stalls after the probe (the upload) fails as a lost
// connection rather than hanging the run.
func TestStalledUploadFails(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &uploadTimeout, time.Second)
	e.Set("upload-hangs", "")
	start := time.Now()
	_, err := e.c.EnsureHelper(context.Background(), false)
	if !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("took %s", took)
	}
}

// Through a ControlMaster the channel's descriptors outlive the ssh client:
// ending a silent session must still unblock reads and writes and finish
// its cleanup.
func TestSilentHelperBehindHeldPipes(t *testing.T) {
	e := newFakeEnv(t)
	short(t, &SilenceLimit, time.Second)
	e.Set("rpc-silent", "")
	e.Set("hold-fds", "")
	t.Cleanup(e.Release)
	s := e.session(t)
	errc := make(chan error, 1)
	go func() {
		chunk := make([]byte, 64<<10)
		for {
			if err := s.Conn.WriteFrame(protocol.KindBlob, chunk); err != nil {
				errc <- err
				return
			}
		}
	}()
	start := time.Now()
	if _, err := s.Expect(context.Background(), protocol.TypeAcquired); !IsConnError(err) {
		t.Fatalf("want a connection error, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("read returned after %s", took)
	}
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("write still blocked")
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session cleanup still waiting on held pipes")
	}
}
