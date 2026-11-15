package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/transport/sshtest"
	"github.com/daintreehq/spotexec/internal/ui"
)

func TestMain(m *testing.M) {
	sshtest.Main()
	os.Exit(m.Run())
}

func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func liveRunner(t *testing.T, behavior string) (*config.Runner, provider.Target) {
	t.Helper()
	f := sshtest.Install(t)
	if behavior != "" {
		f.Set(behavior, "")
	}
	shorten(t, &transport.SilenceLimit, time.Second)
	return &config.Runner{Name: "box", Type: config.TypeSSH, Hosts: []string{"box"}, MaxSlots: 2}, provider.Target{Host: "box", User: "dev"}
}

func quietRep(t *testing.T) ui.Reporter {
	rep, err := ui.New(ui.ModeQuiet, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// A helper that stops responding while we queue for a slot (here, waiting
// for a --same-slot workspace) ends the attempt as a lost connection
// within the silence limit, rather than hanging the run.
func TestAcquireFailsOnSilentHelper(t *testing.T) {
	r, tgt := liveRunner(t, "rpc-silent")
	req := AcquireRequest{Base: protocol.Acquire{RepoID: "repo"}, QueueTimeout: time.Hour, PreferSlot: 1, OnlySlot: true, Rep: quietRep(t)}
	start := time.Now()
	_, _, err := tryAcquire(context.Background(), req, r, nil, tgt, true)
	if !transport.IsConnError(err) || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("want a stopped-responding connection error, got %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
}

// The same during the sync phase.
func TestSyncFailsOnSilentHelper(t *testing.T) {
	r, tgt := liveRunner(t, "rpc-silent")
	_, s, err := Connect(context.Background(), r, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	m := &manifest.Manifest{Entries: []protocol.Entry{{Path: "a.txt", Size: 1}}}
	m.ByPath = map[string]protocol.Entry{"a.txt": m.Entries[0]}
	done := make(chan error, 1)
	go func() {
		_, err := syncIn(context.Background(), s, m, nil, nil, Options{Rep: quietRep(t)})
		done <- err
	}()
	select {
	case err := <-done:
		if !transport.IsConnError(err) {
			t.Fatalf("want a connection error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync still waiting on a silent helper")
	}
}

// A helper that is alive but never grants the slot is held to the queue
// deadline here too, not only on the runner; heartbeats keep it from being
// mistaken for a dead one.
func TestAcquireQueueDeadlineIsEnforcedLocally(t *testing.T) {
	r, tgt := liveRunner(t, "rpc-heartbeat")
	shorten(t, &queueGrace, 500*time.Millisecond)
	req := AcquireRequest{Base: protocol.Acquire{RepoID: "repo"}, QueueTimeout: 1500 * time.Millisecond, Rep: quietRep(t)}
	_, _, err := tryAcquire(context.Background(), req, r, nil, tgt, true)
	var re *transport.RemoteError
	if !errors.As(err, &re) || re.Code != protocol.CodeQueueTimeout {
		t.Fatalf("want a queue timeout, got %v", err)
	}
}

// The same during the dependency step (#144), with and without the shared
// cache: a helper that falls silent while the client checks the cache or
// installs is given up on within the silence limit.
func TestDepsFailsOnSilentHelper(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			r, tgt := liveRunner(t, "rpc-silent")
			_, s, err := Connect(context.Background(), r, tgt)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Abort()
			req := protocol.Deps{Lockfiles: []string{"package-lock.json"}, Install: "npm ci", CacheDirs: []string{"node_modules"}}
			if shared {
				req = sharedDeps()
			}
			done := make(chan error, 1)
			go func() {
				_, _, _, err := depsFetch(fastShare(t), t, sessionRPC{s}, &recRep{}, req)
				done <- err
			}()
			select {
			case err := <-done:
				if !transport.IsConnError(err) || !strings.Contains(err.Error(), "stopped responding") {
					t.Fatalf("want a stopped-responding connection error, got %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("dependency step still waiting on a silent helper")
			}
		})
	}
}

// A run waiting for another run to install the same dependencies waits on
// a local lock, not its session: when its own helper falls silent meanwhile
// it stops with that lost connection as soon as the session is given up
// on (#144), not when the wait for the other run's install runs out.
func TestDepsWaitEndsWhenItsHelperGoesSilent(t *testing.T) {
	r, tgt := liveRunner(t, "rpc-deps-missing")
	ctx := fastShare(t)
	_, s, err := Connect(context.Background(), r, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	req := sharedDeps()
	lock := shareLockFor("deps", stripQuery(req.CacheGet))
	held, err := lock.take(ctx, lock.verdict(), "other#1", time.Minute, nil)
	if err != nil || !held.lead {
		t.Fatalf("holding the lock: %v", err)
	}
	defer held.release("")
	rep := &recRep{}
	start := time.Now()
	_, _, _, err = depsFetch(ctx, t, sessionRPC{s}, rep, req)
	if !transport.IsConnError(err) || !strings.Contains(err.Error(), "stopped responding") {
		t.Fatalf("want a stopped-responding connection error, got %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
	if rep.event("deps_wait") == nil {
		t.Fatal("never waited for the other run: the test didn't reach the lock")
	}
}
