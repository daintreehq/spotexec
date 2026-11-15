package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// Sharing one build (#139). The parts of a sharded or parallel run, and
// runs started at the same time on this machine, often need the same thing
// that the region doesn't have yet: a dependency cache entry, or a project
// image in its registry. Without coordination every one of them builds it
// and uploads it. Instead, a run that finds it missing takes a local lock
// for it: the first builds it, waits for its upload (or push) to finish and
// leaves a verdict; the others wait on the lock and then fetch it like any
// later run. Everything is driven from this machine, so a flock is enough:
// it works between goroutines (each take opens its own file) and
// processes, and a process that dies releases it.

// sharePoll is how often a waiting run checks the lock.
var sharePoll = 250 * time.Millisecond

// shareDone is the verdict a leader leaves when it finished: the artifact
// is published, or the build failed in a way that would fail again (the
// install itself failed). Either way the runs waiting go on at once, each
// fetching it or building it for itself. A leader that was lost or
// cancelled leaves none: the next waiting run builds instead.
const shareDone = "done"

// shareLock is the local lock on building one artifact.
type shareLock struct{ path string }

// shareLockFor is the lock for one artifact: kind ("deps", "image") and an
// identity naming where it is shared (cache object, registry repository and
// tag), so different regions never wait on each other.
func shareLockFor(kind, id string) *shareLock {
	h := sha256.Sum256([]byte(kind + "\x00" + id))
	return &shareLock{path: filepath.Join(config.StateDir(), "share", kind+"-"+hex.EncodeToString(h[:])[:24])}
}

// shareTurn is a run's turn at the lock.
type shareTurn struct {
	lead     bool // this run holds the lock and builds
	waited   time.Duration
	timedOut bool // waited the limit out; builds without the lock
	f        *os.File
	path     string
}

type shareHolder struct {
	Who string `json:"who"`
	PID int    `json:"pid"`
}

var tmpSeq atomic.Int64

// take waits for the lock. A run that finds a verdict other than before
// (read before it found the artifact missing) doesn't lead: someone
// finished building it meanwhile, so it goes on without the lock. onWait is
// called once while the lock is held by another, with who holds it (as
// soon as the holder has said, or after a second).
func (l *shareLock) take(ctx context.Context, before, who string, limit time.Duration, onWait func(holder string)) (*shareTurn, error) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	waiting, announced := false, onWait == nil
	for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		waiting = true
		if !announced {
			// The holder names itself just after locking.
			if h, ok := l.holder(); ok || time.Since(start) >= time.Second {
				announced = true
				onWait(h)
			}
		}
		if limit > 0 && time.Since(start) >= limit {
			f.Close()
			return &shareTurn{timedOut: true, waited: time.Since(start)}, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(sharePoll):
		}
	}
	t := &shareTurn{f: f, path: l.path}
	if waiting {
		t.waited = time.Since(start)
		if !announced {
			// Freed before its holder could be read: still say so.
			h, _ := l.holder()
			onWait(h)
		}
	}
	if l.verdict() != before {
		t.release("")
		return t, nil
	}
	t.lead = true
	b, _ := json.Marshal(shareHolder{Who: who, PID: os.Getpid()})
	_ = writeAtomic(l.path+".holder", b)
	return t, nil
}

// release lets the next run in, leaving verdict (shareDone, or "" for
// none: the next waiting run builds).
func (t *shareTurn) release(verdict string) {
	if t.f == nil {
		return
	}
	if verdict != "" && t.lead {
		// A fresh token: equal clocks or timestamps can't make a new
		// verdict look like the old one.
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		_ = writeAtomic(t.path+".done", []byte(hex.EncodeToString(b)+" "+time.Now().UTC().Format(time.RFC3339)+"\n"))
	}
	if t.lead {
		_ = os.Remove(t.path + ".holder")
	}
	_ = syscall.Flock(int(t.f.Fd()), syscall.LOCK_UN)
	t.f.Close()
	t.f = nil
}

// verdict identifies the last leader's verdict ("" for none yet).
func (l *shareLock) verdict() string {
	b, _ := os.ReadFile(l.path + ".done")
	return string(b)
}

// holder describes who holds the lock, for a waiting run's events, and
// whether the holder said.
func (l *shareLock) holder() (string, bool) {
	var h shareHolder
	b, err := os.ReadFile(l.path + ".holder")
	if err != nil || json.Unmarshal(b, &h) != nil || h.Who == "" {
		return "another run on this machine", false
	}
	if h.PID == os.Getpid() {
		return "another part of this run (" + h.Who + ")", true
	}
	return fmt.Sprintf("another run on this machine (%s, sx pid %d)", h.Who, h.PID), true
}

func writeAtomic(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), tmpSeq.Add(1))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// sharer fetches or builds one artifact so that, of the runs on this
// machine needing it at once, one builds it and the rest fetch it.
type sharer struct {
	lock  *shareLock
	who   string        // this run, as the others see it: "aws#3"
	limit time.Duration // the longest a run waits before building for itself
	// probe fetches the artifact if it can be had without building
	// (true), and never builds.
	probe func(context.Context) (bool, error)
	// build fetches it, or builds it when it's missing.
	build func(context.Context) error
	// publish, called on the leader after a successful build, makes the
	// result fetchable: waits for its upload or push. A failed transfer is
	// not an error (the others build for themselves); losing the runner
	// is, and leaves the build to the next waiting run.
	publish func(context.Context) error
	waiting func(holder string) // called once if this run has to wait
	// sess is the helper session the artifact comes through (nil: none
	// to watch). A run waiting for another's build stops as soon as its own
	// session is lost, rather than when the wait runs out (#144).
	sess rpc
}

type shareOutcome struct {
	led      bool          // built while holding the lock
	waited   time.Duration // for another run's build
	timedOut bool          // gave up waiting
}

func (s sharer) run(ctx context.Context) (shareOutcome, error) {
	var out shareOutcome
	// Read before probing: a leader finishing while the probe is out
	// changes it, so this run fetches instead of building again.
	before := s.lock.verdict()
	if ok, err := s.probe(ctx); err != nil || ok {
		return out, err
	}
	turn, err := s.take(ctx, before)
	if err != nil {
		return out, err
	}
	out.waited, out.timedOut, out.led = turn.waited, turn.timedOut, turn.lead
	if !turn.lead {
		return out, s.build(ctx)
	}
	verdict := ""
	defer func() { turn.release(verdict) }()
	err = s.build(ctx)
	var re *transport.RemoteError
	switch {
	case err == nil:
		if s.publish != nil {
			if err := s.publish(ctx); err != nil {
				return out, err
			}
		}
		verdict = shareDone
	case errors.As(err, &re) && ctx.Err() == nil:
		// The runner reported the failure: the others needn't wait in
		// line to fail the same way.
		verdict = shareDone
	}
	return out, err
}

// take waits for the lock, giving up with the session's error if the
// session is lost meanwhile: the lock is local, so nothing else in the wait
// notices that the runner stopped answering.
func (s sharer) take(ctx context.Context, before string) (*shareTurn, error) {
	if s.sess == nil {
		return s.lock.take(ctx, before, s.who, s.limit, s.waiting)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lost := false
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-s.sess.lost():
			lost = true
			cancel()
		case <-wctx.Done():
		}
	}()
	turn, err := s.lock.take(wctx, before, s.who, s.limit, s.waiting)
	cancel()
	<-stopped
	if err != nil && lost && ctx.Err() == nil {
		return nil, s.sess.lostErr()
	}
	return turn, err
}

// rpc is what preparing an artifact needs of a helper session (a
// *transport.Session; scripted in tests).
type rpc interface {
	send(typ string, v any) error
	next(ctx context.Context) (protocol.Frame, error)
	event(protocol.Event)  // a helper event that arrived meanwhile
	lost() <-chan struct{} // closed once the session has ended (nil: never)
	lostErr() error        // why, once lost() is closed
}

type sessionRPC struct{ s *transport.Session }

func (r sessionRPC) send(typ string, v any) error { return r.s.Conn.Send(typ, v) }
func (r sessionRPC) next(ctx context.Context) (protocol.Frame, error) {
	return r.s.Next(ctx)
}
func (r sessionRPC) lost() <-chan struct{} { return r.s.Lost() }
func (r sessionRPC) lostErr() error        { return r.s.LostErr() }
func (r sessionRPC) event(ev protocol.Event) {
	if r.s.OnEvent != nil {
		r.s.OnEvent(ev)
	}
}

// awaitShared waits for the upload or push (kind) that the helper started
// in this session, so the runs waiting for it can fetch it. A transfer
// that failed is reported in ShareDone; an error is the session's.
func awaitShared(ctx context.Context, c rpc, kind string, limit time.Duration) (protocol.ShareDone, error) {
	if err := c.send(protocol.TypeShareWait, protocol.ShareWait{Kind: kind, TimeoutMS: limit.Milliseconds()}); err != nil {
		return protocol.ShareDone{}, err
	}
	for {
		f, err := c.next(ctx)
		if err != nil {
			return protocol.ShareDone{}, err
		}
		if f.Kind != protocol.KindMsg {
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return protocol.ShareDone{}, err
		}
		switch env.Type {
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			c.event(ev)
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			// Not the transfer's failure (that is ShareDone) but the
			// session's: the helper gave up on this client.
			return protocol.ShareDone{}, &transport.RemoteError{ErrorMsg: em}
		case protocol.TypeShareDone:
			var d protocol.ShareDone
			err := env.Unmarshal(&d)
			return d, err
		default:
			return protocol.ShareDone{}, fmt.Errorf("protocol: unexpected %q while waiting for an upload", env.Type)
		}
	}
}

// shareSlack is how much longer than the install's (or build's) own time
// limit a run waits for another to finish it: that run may first spend a
// transfer's time trying the shared cache, and then uploads its result.
const shareSlack = 25 * time.Minute

// shareTransfer bounds the wait for an upload or push to finish (the
// helper's own transfers give up after 10 and 20 minutes).
const shareTransfer = 22 * time.Minute

// resignAfter: cache URLs older than this are signed afresh before the
// request that may install and upload (they are good for at least half an
// hour; a run may have waited longer for another's install).
var resignAfter = 10 * time.Minute

// stripQuery drops a presigned URL's query (its signature, which changes
// with every signing), leaving the object it names.
func stripQuery(u string) string {
	u, _, _ = strings.Cut(u, "?")
	return u
}
