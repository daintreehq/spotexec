package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// fakeFlush stands in for the machine: bytes waiting to be written,
// whether Docker shares the work root's filesystem, and a sync that
// finishes when the test says. It counts the syncs started.
type fakeFlush struct {
	syncs   atomic.Int32
	started chan string   // the directory of each sync started
	finish  chan error    // ends a sync in progress
	stop    chan struct{} // ends every sync, at the test's end
	startEr error         // the sync can't start
	shares  atomic.Bool   // Docker is on the work root's filesystem
	pending atomic.Int64  // bytes waiting
	asked   atomic.Int32  // dockerSharesFS calls
}

func stubFlush(t *testing.T) *fakeFlush {
	t.Helper()
	f := &fakeFlush{started: make(chan string, 8), finish: make(chan error, 8), stop: make(chan struct{})}
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
		go func() {
			select {
			case err := <-f.finish:
				done <- err
			case <-f.stop:
				done <- nil
			}
		}()
		return done, nil
	}
	flushTick = 10 * time.Millisecond
	t.Cleanup(func() {
		close(f.stop)
		pendingBytes, startSync, dockerSharesFS, flushTick = oldP, oldS, oldD, oldT
	})
	return f
}

// waitSync waits for the next sync to start, and returns its directory.
func (f *fakeFlush) waitSync(t *testing.T) string {
	t.Helper()
	select {
	case dir := <-f.started:
		return dir
	case <-time.After(5 * time.Second):
		t.Fatal("no sync started")
		return ""
	}
}

// containerDeps is a request, for a containerised project, that hydrates
// the slot from a cache this runner already has (so no docker runs).
func containerDeps(t *testing.T, s *Session) protocol.Deps {
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

// depsAsync runs the request, and returns a wait for its end, bounded.
func depsAsync(t *testing.T, s *Session, d protocol.Deps) func() error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- s.deps(d) }()
	ended := false
	t.Cleanup(func() {
		if !ended {
			<-errc // the session must be idle before its cleanup
		}
	})
	return func() error {
		t.Helper()
		select {
		case err := <-errc:
			ended = true
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the request didn't end")
			return nil
		}
	}
}

// sent is what the helper has written so far, safe to read while it writes.
func sent(out *syncBuf) []byte {
	out.mu.Lock()
	defer out.mu.Unlock()
	return bytes.Clone(out.b.Bytes())
}

// sentEvents is what the helper has sent so far: event names to messages,
// and whether a reply of type typ is among them.
func sentEvents(out *syncBuf, typ string) (events map[string]string, replied bool) {
	events = map[string]string{}
	in := protocol.NewConn(bytes.NewReader(sent(out)), nil)
	for {
		f, err := in.ReadFrame()
		if err != nil {
			return events, replied
		}
		env, err := protocol.Decode(f)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			events[ev.Name] = ev.Message
		case typ:
			replied = true
		}
	}
}

func protoCode(err error) string {
	var pe *protoError
	if errors.As(err, &pe) {
		return pe.code
	}
	return ""
}

// #253: a containerised run's dependencies are written to disk before the
// helper reports them ready, so the container's start (Docker syncs the
// filesystem when it unmounts the overlay) doesn't wait for them inside the
// run's setup. The wait is announced while it lasts, and timed.
func TestDepsFlushBeforeAContainerRun(t *testing.T) {
	f := stubFlush(t)
	s, out := depsSession(t)
	end := depsAsync(t, s, containerDeps(t, s))
	if dir := f.waitSync(t); dir != s.layout.Root {
		t.Fatalf("synced %s, want the work root %s", dir, s.layout.Root)
	}
	eventually(t, "the wait announced", func() bool {
		ev, _ := sentEvents(out, protocol.TypeDepsResult)
		return ev["deps_flush"] == "3.0 GB"
	})
	time.Sleep(50 * time.Millisecond)
	if _, replied := sentEvents(out, protocol.TypeDepsResult); replied {
		t.Fatal("dependencies reported ready before they were written")
	}
	// The slot's copy is complete: a run that loses its client now still
	// leaves a slot the next run reuses.
	if st := loadSlotState(s.slotDir); st.DepsKey == "" {
		t.Fatalf("slot state not saved before the flush: %+v", st)
	}
	f.finish <- nil
	if err := end(); err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	replies(t, out, protocol.TypeDepsResult, &res)
	if !res.Hydrated || res.FlushMS < 50 || res.MS < res.FlushMS {
		t.Fatalf("result %+v", res)
	}
}

// Nothing waits when nothing would hold up a container: an uncontainerised
// project, a slot that already has its dependencies, less than 256 MB
// written, or a work root on another filesystem than Docker's.
func TestDepsFlushOnlyWhenAContainerWouldWait(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*fakeFlush, *protocol.Deps)
		flush bool
	}{
		{"no container", func(_ *fakeFlush, d *protocol.Deps) { d.Container = nil }, false},
		{"nothing written", func(f *fakeFlush, _ *protocol.Deps) { f.pending.Store(0) }, false},
		{"just under 256 MB", func(f *fakeFlush, _ *protocol.Deps) { f.pending.Store(256<<20 - 1) }, false},
		{"256 MB", func(f *fakeFlush, _ *protocol.Deps) { f.pending.Store(256 << 20) }, true},
		{"another filesystem", func(f *fakeFlush, _ *protocol.Deps) { f.shares.Store(false) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := stubFlush(t)
			s, out := depsSession(t)
			d := containerDeps(t, s)
			c.set(f, &d)
			f.finish <- nil
			if err := s.deps(d); err != nil {
				t.Fatal(err)
			}
			var res protocol.DepsResult
			events := replies(t, out, protocol.TypeDepsResult, &res)
			flushed := f.syncs.Load() == 1 && slices.Contains(events, "deps_flush")
			if !res.Hydrated || flushed != c.flush || (!c.flush && (f.syncs.Load() != 0 || res.FlushMS != 0)) {
				t.Fatalf("result %+v, syncs %d, events %v", res, f.syncs.Load(), events)
			}
		})
	}
	t.Run("slot already hydrated", func(t *testing.T) {
		f := stubFlush(t)
		s, _ := depsSession(t)
		d := containerDeps(t, s)
		f.finish <- nil
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
		if err := s.deps(containerDeps(t, s)); err != nil {
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
		f.finish <- errors.New("exit status 1")
		if err := s.deps(containerDeps(t, s)); err != nil {
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
		flushBudget = 100 * time.Millisecond
		t.Cleanup(func() { flushBudget = old })
		s, out := depsSession(t)
		start := time.Now()
		if err := depsAsync(t, s, containerDeps(t, s))(); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("the budget didn't end the wait: %s", took)
		}
		var res protocol.DepsResult
		replies(t, out, protocol.TypeDepsResult, &res)
		if !res.Hydrated || res.FlushMS < 100 {
			t.Fatalf("result %+v", res)
		}
	})
}

// A client that goes away, or a runner shutting down, ends the wait (the
// sync carries on by itself) with an error the client reads as such; the
// slot keeps its complete copy.
func TestDepsFlushEndsWithTheSession(t *testing.T) {
	for _, c := range []struct {
		name, code string
		end        func(*Session)
	}{
		{"client gone", protocol.CodeDepsFailed, func(s *Session) { s.lastSeen.Store(s.now() - int64(time.Hour)) }},
		{"shutting down", protocol.CodeShutdown, func(s *Session) { s.stopping.Store(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := stubFlush(t)
			s, out := depsSession(t)
			// Before the session's own cleanup (which parks while stopping).
			t.Cleanup(func() { s.stopping.Store(false) })
			end := depsAsync(t, s, containerDeps(t, s))
			f.waitSync(t)
			c.end(s)
			if err := end(); protoCode(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
			if _, replied := sentEvents(out, protocol.TypeDepsResult); replied {
				t.Fatal("reported the dependencies ready")
			}
			if st := loadSlotState(s.slotDir); st.DepsKey == "" {
				t.Fatalf("slot state %+v", st)
			}
		})
	}
}

// startSync runs `sync -f` on the directory's filesystem.
func TestStartSyncSyncsTheFilesystem(t *testing.T) {
	bin := t.TempDir()
	rec := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(filepath.Join(bin, "sync"), []byte("#!/bin/sh\necho \"$@\" > "+shq(rec)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	done, err := startSync("/work root")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync didn't finish")
	}
	if b, err := os.ReadFile(rec); err != nil || string(b) != "-f /work root\n" {
		t.Fatalf("sync ran with %q, %v", b, err)
	}
}

func TestMeminfoPending(t *testing.T) {
	meminfo := "MemTotal:       64000000 kB\nDirty:           7394584 kB\nWriteback:         16384 kB\nWritebackTmp:          8 kB\nAnonPages:        123 kB\n"
	if got, want := meminfoPending(meminfo), int64(7394584+16384)<<10; got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
	if got := meminfoPending("MemTotal: 1 kB\n"); got != 0 {
		t.Fatalf("got %d", got)
	}
}

// Containers' overlays are where Docker keeps them: under containerd's
// root with its snapshotter (not reported, so assumed shared), else under
// Docker Root Dir.
func TestSharesContainerFS(t *testing.T) {
	dir := t.TempDir()
	classic := func(root string) string {
		return "Server:\n Storage Driver: overlay2\n Docker Root Dir: " + root + "\n Debug Mode: false\n"
	}
	snapshotter := "Server:\n Storage Driver: overlayfs\n  driver-type: io.containerd.snapshotter.v1\n Docker Root Dir: /dev\n"
	for _, c := range []struct {
		name, info string
		want       bool
	}{
		{"same filesystem", classic(t.TempDir()), true},
		{"another filesystem", classic("/dev"), false},
		{"containerd snapshotter", snapshotter, true},
		{"root dir missing", classic(filepath.Join(dir, "missing")), true},
		{"no root dir", "Cannot connect to the Docker daemon", true},
	} {
		if got := sharesContainerFS(c.info, dir); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if got := dockerRootDir(classic("/var/lib/docker")); got != "/var/lib/docker" {
		t.Fatalf("root dir %q", got)
	}
}

func TestSameDevice(t *testing.T) {
	dir := t.TempDir()
	if same, ok := sameDevice(dir, filepath.Join(dir, ".")); !same || !ok {
		t.Fatalf("same dir: %v %v", same, ok)
	}
	if same, ok := sameDevice(dir, "/dev"); same || !ok {
		t.Fatalf("/dev: %v %v", same, ok)
	}
	for _, p := range [][2]string{{dir, filepath.Join(dir, "missing")}, {filepath.Join(dir, "missing"), dir}} {
		if _, ok := sameDevice(p[0], p[1]); ok {
			t.Fatalf("compared %v", p)
		}
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{300 << 20: "300 MB", 8700 << 20: "8.5 GB", 1 << 30: "1.0 GB"} {
		if got := sizeText(n); got != want {
			t.Fatalf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}

// The client reads flush_ms; nothing is sent when nothing was waited for.
func TestDepsResultFlushField(t *testing.T) {
	b, _ := json.Marshal(protocol.DepsResult{FlushMS: 1200})
	if !strings.Contains(string(b), `"flush_ms":1200`) {
		t.Fatalf("%s", b)
	}
	if b, _ := json.Marshal(protocol.DepsResult{}); strings.Contains(string(b), "flush_ms") {
		t.Fatalf("%s", b)
	}
}
