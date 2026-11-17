package helper

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// fakeFlush stands in for the machine: pending bytes waiting to be
// written, whether Docker shares the work root's filesystem, and a sync
// that finishes when the test says. It counts the syncs started.
type fakeFlush struct {
	syncs   atomic.Int32
	started chan string  // the directory of each sync started
	finish  chan error   // ends the sync in progress
	startEr error        // the sync can't start
	shares  atomic.Bool  // Docker is on the work root's filesystem
	pending atomic.Int64 // bytes waiting
	asked   atomic.Int32 // dockerSharesFS calls
}

func stubFlush(t *testing.T) *fakeFlush {
	t.Helper()
	f := &fakeFlush{started: make(chan string, 4), finish: make(chan error, 4)}
	f.shares.Store(true)
	f.pending.Store(3 << 30)
	oldP, oldS, oldD, oldT := pendingBytes, startSync, dockerSharesFS, flushTick
	pendingBytes = func() int64 { return f.pending.Load() }
	dockerSharesFS = func(string) bool { f.asked.Add(1); return f.shares.Load() }
	startSync = func(dir string) (<-chan error, error) {
		if f.startEr != nil {
			return nil, f.startEr
		}
		f.syncs.Add(1)
		f.started <- dir
		done := make(chan error, 1)
		go func() { done <- <-f.finish }()
		return done, nil
	}
	flushTick = 10 * time.Millisecond
	t.Cleanup(func() { pendingBytes, startSync, dockerSharesFS, flushTick = oldP, oldS, oldD, oldT })
	return f
}

// containerDeps is a request, for a containerised project, that hydrates
// the slot from a cache this runner already has (so no docker runs).
func containerDeps(t *testing.T, s *Session, _ *syncBuf) protocol.Deps {
	t.Helper()
	d := protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "exit 7", CacheDirs: []string{"vendor"}, Probe: true,
		Container: &protocol.ContainerRun{Image: "img"}, ImageID: "sha256:feed"}
	key, err := depsKey(s.workspace, d)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(s.layout.Deps(s.repoID), key)
	if err := os.MkdirAll(filepath.Join(cache, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{"vendor/installed": "one", ".ready": "vendor"} {
		if err := os.WriteFile(filepath.Join(cache, name), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// sent is what the helper has written so far, safe to read while it writes.
func sent(out *syncBuf) *syncBuf {
	out.mu.Lock()
	defer out.mu.Unlock()
	c := &syncBuf{}
	c.b.Write(out.b.Bytes())
	return c
}

func hasReply(out *syncBuf, typ string) bool {
	in := protocol.NewConn(bytes.NewReader(sent(out).b.Bytes()), nil)
	for {
		f, err := in.ReadFrame()
		if err != nil {
			return false
		}
		if env, err := protocol.Decode(f); err == nil && env.Type == typ {
			return true
		}
	}
}

// #253: a containerised run's dependencies are written to disk before the
// helper reports them ready, so the container's start (Docker syncs the
// filesystem when it unmounts the overlay) doesn't wait for them inside the
// run's setup. The wait is announced, and timed in the result.
func TestDepsFlushBeforeAContainerRun(t *testing.T) {
	f := stubFlush(t)
	s, out := depsSession(t)
	d := containerDeps(t, s, out)
	errc := make(chan error, 1)
	go func() { errc <- s.deps(d) }()
	var dir string
	select {
	case dir = <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no sync started")
	}
	if dir != s.layout.Root {
		t.Fatalf("synced %s, want the work root %s", dir, s.layout.Root)
	}
	time.Sleep(50 * time.Millisecond)
	if hasReply(out, protocol.TypeDepsResult) {
		t.Fatal("dependencies reported ready before they were written")
	}
	// The slot's copy is complete: a run that loses its client now still
	// leaves a slot the next run reuses.
	if st := loadSlotState(s.slotDir); st.DepsKey == "" {
		t.Fatalf("slot state not saved before the flush: %+v", st)
	}
	f.finish <- nil
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	events := replies(t, out, protocol.TypeDepsResult, &res)
	if !res.Hydrated || res.FlushMS < 40 || res.MS < res.FlushMS {
		t.Fatalf("result %+v", res)
	}
	if !slices.Contains(events, "deps_flush") {
		t.Fatalf("events %v", events)
	}
	if msg := eventMessage(t, out, "deps_flush"); msg != "3.0 GB" {
		t.Fatalf("deps_flush message %q", msg)
	}
}

// Nothing waits when nothing would hold up a container: an uncontainerised
// project, a slot that already has its dependencies, little written, or a
// work root on another filesystem than Docker's.
func TestDepsFlushOnlyWhenAContainerWouldWait(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeFlush, *protocol.Deps)
	}{
		{"no container", func(_ *fakeFlush, d *protocol.Deps) { d.Container = nil }},
		{"little written", func(f *fakeFlush, _ *protocol.Deps) { f.pending.Store(flushMin - 1) }},
		{"another filesystem", func(f *fakeFlush, _ *protocol.Deps) { f.shares.Store(false) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := stubFlush(t)
			s, out := depsSession(t)
			d := containerDeps(t, s, out)
			c.set(f, &d)
			if err := s.deps(d); err != nil {
				t.Fatal(err)
			}
			var res protocol.DepsResult
			events := replies(t, out, protocol.TypeDepsResult, &res)
			if !res.Hydrated || res.FlushMS != 0 || f.syncs.Load() != 0 || slices.Contains(events, "deps_flush") {
				t.Fatalf("result %+v, syncs %d, events %v", res, f.syncs.Load(), events)
			}
		})
	}
	t.Run("slot already hydrated", func(t *testing.T) {
		f := stubFlush(t)
		s, out := depsSession(t)
		d := containerDeps(t, s, out)
		go func() { f.finish <- nil }()
		if err := s.deps(d); err != nil {
			t.Fatal(err)
		}
		if f.syncs.Load() != 1 {
			t.Fatalf("hydrating: %d syncs", f.syncs.Load())
		}
		asked := f.asked.Load()
		if err := s.deps(d); err != nil {
			t.Fatal(err)
		}
		if f.syncs.Load() != 1 || f.asked.Load() != asked {
			t.Fatalf("a warm slot waited: %d syncs", f.syncs.Load())
		}
	})
}

// The wait never fails the run on its own: a sync that can't start, fails,
// or outlasts its budget leaves the rest to the container's start.
func TestDepsFlushFailureIsNotTheRunsFailure(t *testing.T) {
	t.Run("can't start", func(t *testing.T) {
		f := stubFlush(t)
		f.startEr = errors.New("no sync")
		s, out := depsSession(t)
		d := containerDeps(t, s, out)
		if err := s.deps(d); err != nil {
			t.Fatal(err)
		}
		var res protocol.DepsResult
		events := replies(t, out, protocol.TypeDepsResult, &res)
		if !res.Hydrated || slices.Contains(events, "deps_flush") {
			t.Fatalf("result %+v, events %v", res, events)
		}
	})
	t.Run("fails", func(t *testing.T) {
		f := stubFlush(t)
		s, out := depsSession(t)
		d := containerDeps(t, s, out)
		f.finish <- errors.New("exit status 1")
		if err := s.deps(d); err != nil {
			t.Fatal(err)
		}
		var res protocol.DepsResult
		replies(t, out, protocol.TypeDepsResult, &res)
		if !res.Hydrated {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("over budget", func(t *testing.T) {
		stubFlush(t) // never finishes
		old := flushBudget
		flushBudget = 50 * time.Millisecond
		t.Cleanup(func() { flushBudget = old })
		s, out := depsSession(t)
		d := containerDeps(t, s, out)
		if err := s.deps(d); err != nil {
			t.Fatal(err)
		}
		var res protocol.DepsResult
		replies(t, out, protocol.TypeDepsResult, &res)
		if !res.Hydrated || res.FlushMS < 50 {
			t.Fatalf("result %+v", res)
		}
	})
}

// A client that goes away, or a runner shutting down, ends the wait (the
// sync carries on by itself); the slot keeps its complete copy.
func TestDepsFlushEndsWithTheSession(t *testing.T) {
	for _, c := range []struct {
		name, want string
		end        func(*Session)
	}{
		{"client gone", "client went away", func(s *Session) { s.lastSeen.Store(s.now() - int64(time.Hour)) }},
		{"shutting down", "shutting down", func(s *Session) { s.stopping.Store(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := stubFlush(t)
			s, out := depsSession(t)
			d := containerDeps(t, s, out)
			errc := make(chan error, 1)
			go func() { errc <- s.deps(d) }()
			<-f.started
			c.end(s)
			select {
			case err := <-errc:
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the wait didn't end")
			}
			s.stopping.Store(false) // let the test's cleanup release the slot
			if hasReply(out, protocol.TypeDepsResult) {
				t.Fatal("reported the dependencies ready")
			}
			if st := loadSlotState(s.slotDir); st.DepsKey == "" {
				t.Fatalf("slot state %+v", st)
			}
			f.finish <- nil
		})
	}
}

func TestDockerRootDir(t *testing.T) {
	info := "Client:\n Version: 29.1.3\n\nServer:\n Storage Driver: overlayfs\n  driver-type: io.containerd.snapshotter.v1\n Docker Root Dir: /var/lib/docker\n Debug Mode: false\n"
	if got := dockerRootDir(info); got != "/var/lib/docker" {
		t.Fatalf("got %q", got)
	}
	if got := dockerRootDir("Cannot connect to the Docker daemon"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestSameDevice(t *testing.T) {
	dir := t.TempDir()
	if same, ok := sameDevice(dir, filepath.Join(dir, ".")); !same || !ok {
		t.Fatalf("same dir: %v %v", same, ok)
	}
	if _, ok := sameDevice(dir, filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing path was compared")
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{300 << 20: "300 MB", 8700 << 20: "8.5 GB", 1 << 30: "1.0 GB"} {
		if got := sizeText(n); got != want {
			t.Fatalf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}

// The real sync: `sync -f` on the work root's filesystem (Linux runners).
func TestStartSyncRuns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sync -f and /proc/meminfo are Linux's")
	}
	if _, err := exec.LookPath("sync"); err != nil {
		t.Skip("no sync")
	}
	done, err := startSync(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("sync didn't finish")
	}
	if pendingWrites() < 0 {
		t.Fatal("negative pending writes")
	}
}

// eventMessage is the message of the first event named name.
func eventMessage(t *testing.T, out *syncBuf, name string) string {
	t.Helper()
	in := protocol.NewConn(bytes.NewReader(sent(out).b.Bytes()), nil)
	for {
		f, err := in.ReadFrame()
		if err != nil {
			t.Fatalf("no %s event", name)
		}
		env, err := protocol.Decode(f)
		if err != nil || env.Type != protocol.TypeEvent {
			continue
		}
		var ev protocol.Event
		_ = env.Unmarshal(&ev)
		if ev.Name == name {
			return ev.Message
		}
	}
}
