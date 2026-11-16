package helper

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// replies decodes what the helper sent: event names and the last reply of
// type want.
func replies(t *testing.T, out *syncBuf, want string, into any) []string {
	t.Helper()
	var events []string
	in := protocol.NewConn(bytes.NewReader(out.b.Bytes()), io.Discard)
	found := false
	for {
		f, err := in.ReadFrame()
		if err != nil {
			break
		}
		if f.Kind != protocol.KindMsg {
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		switch env.Type {
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			events = append(events, ev.Name)
		case want:
			if err := env.Unmarshal(into); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s reply; events %v", want, events)
	}
	return events
}

func depsSession(t *testing.T) (*Session, *syncBuf) {
	t.Helper()
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.workspace, "deps.lock"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	s.c = protocol.NewConn(strings.NewReader(""), out)
	return s, out
}

// #139: a probe on a cold cache reports the miss and leaves the install to
// whichever run the client picks: it runs nothing and invalidates nothing,
// not even the slot's existing (other key's) dependencies.
func TestDepsProbeMissInstallsNothing(t *testing.T) {
	s, out := depsSession(t)
	if err := os.MkdirAll(filepath.Join(s.workspace, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.workspace, "vendor", "old"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.state.Fingerprint, s.state.DepsKey = "fp", "old-key"
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		t.Fatal(err)
	}
	d := protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "mkdir -p vendor && touch vendor/installed", CacheDirs: []string{"vendor"}, Probe: true}
	key, err := depsKey(s.workspace, d)
	if err != nil {
		t.Fatal(err)
	}
	// A shared cache without this key (the archive isn't there).
	d.CacheKey, d.CacheGet = key, "file://"+filepath.Join(t.TempDir(), "missing.tar")
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	events := replies(t, out, protocol.TypeDepsResult, &res)
	if !res.Missing || res.Built || res.Hydrated || res.Restored {
		t.Fatalf("result %+v", res)
	}
	for _, e := range events {
		if e == "deps_install" {
			t.Fatalf("probe installed: events %v", events)
		}
	}
	if _, err := os.Stat(filepath.Join(s.workspace, "vendor", "installed")); !os.IsNotExist(err) {
		t.Fatalf("probe ran the install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.workspace, "vendor", "old")); err != nil {
		t.Fatalf("probe cleared the slot's dependencies: %v", err)
	}
	if st := loadSlotState(s.slotDir); st.Fingerprint != "fp" || st.DepsKey != "old-key" || s.state.Fingerprint != "fp" {
		t.Fatalf("probe changed the slot's state: saved %+v, in memory %+v", st, s.state)
	}
}

// A probe restores from the shared cache: a fresh VM gets the archive
// another installed, and never runs the install.
func TestDepsProbeRestoresFromTheSharedCache(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "vendor", "installed"), []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".ready"), []byte("vendor"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "k.tar")
	if out, err := exec.Command("tar", "-C", src, "-cf", archive, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	s, out := depsSession(t)
	d := protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "exit 7", CacheDirs: []string{"vendor"}, Probe: true}
	key, err := depsKey(s.workspace, d)
	if err != nil {
		t.Fatal(err)
	}
	d.CacheKey, d.CacheGet = key, "file://"+archive
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	replies(t, out, protocol.TypeDepsResult, &res)
	if res.Missing || res.Built || !res.Restored || !res.Hydrated {
		t.Fatalf("result %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(s.workspace, "vendor", "installed")); err != nil || string(b) != "shared" {
		t.Fatalf("restored %q, %v", b, err)
	}
}

// A probe still uses what the runner has: here the cache another slot
// built, hydrated into this one.
func TestDepsProbeHydratesALocalCache(t *testing.T) {
	s, out := depsSession(t)
	d := protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "mkdir -p vendor && echo one > vendor/installed", CacheDirs: []string{"vendor"}}
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	replies(t, out, protocol.TypeDepsResult, &res)
	if !res.Built {
		t.Fatalf("first run: %+v", res)
	}
	// The slot loses its copy (as another slot would not have it).
	if err := os.RemoveAll(filepath.Join(s.workspace, "vendor")); err != nil {
		t.Fatal(err)
	}
	s.state.DepsKey = ""
	out.b.Reset()
	d.Probe = true
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	res = protocol.DepsResult{}
	replies(t, out, protocol.TypeDepsResult, &res)
	if res.Missing || res.Built || !res.Hydrated {
		t.Fatalf("probe: %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(s.workspace, "vendor", "installed")); err != nil || string(b) != "one\n" {
		t.Fatalf("hydrated %q, %v", b, err)
	}
}

func shareSession(t *testing.T) (*Session, *syncBuf) {
	t.Helper()
	out := &syncBuf{}
	s := testSession(t, t.TempDir())
	s.c = protocol.NewConn(strings.NewReader(""), out)
	return s, out
}

// pending is a transfer the test finishes.
func pending() (*transfer, func(error)) {
	t := &transfer{done: make(chan struct{})}
	return t, func(err error) { t.err = err; close(t.done) }
}

// ShareWait reports how the session's background upload or push ended
// (with where its log is), so the client knows whether the runs waiting
// for it can fetch it.
func TestShareWaitReportsTheTransfer(t *testing.T) {
	for _, tc := range []struct {
		kind string
		err  error
		log  string
	}{
		{protocol.ShareDeps, nil, ""},
		{protocol.ShareDeps, errors.New("exit status 1"), "cache-upload.log"},
		{protocol.ShareImage, nil, ""},
		{protocol.ShareImage, errors.New("exit status 1"), "image-push.log"},
	} {
		t.Run(fmt.Sprintf("%s %v", tc.kind, tc.err), func(t *testing.T) {
			s, out := shareSession(t)
			tr, finish := pending()
			s.track(tc.kind, tr)
			go func() { time.Sleep(50 * time.Millisecond); finish(tc.err) }()
			if err := s.shareWait(protocol.ShareWait{Kind: tc.kind, TimeoutMS: 60000}); err != nil {
				t.Fatal(err)
			}
			var d protocol.ShareDone
			replies(t, out, protocol.TypeShareDone, &d)
			if d.OK != (tc.err == nil) {
				t.Fatalf("done %+v", d)
			}
			if tc.err != nil && !strings.Contains(d.Message, tc.log) {
				t.Fatalf("message %q doesn't say where the log is (%s)", d.Message, tc.log)
			}
		})
	}
}

// ShareWait follows the latest transfer of its kind.
func TestShareWaitFollowsTheLatestTransfer(t *testing.T) {
	s, out := shareSession(t)
	older, _ := pending() // never finishes
	s.track(protocol.ShareDeps, older)
	latest, finish := pending()
	s.track(protocol.ShareDeps, latest)
	finish(nil)
	if err := s.shareWait(protocol.ShareWait{Kind: protocol.ShareDeps, TimeoutMS: 60000}); err != nil {
		t.Fatal(err)
	}
	var d protocol.ShareDone
	replies(t, out, protocol.TypeShareDone, &d)
	if !d.OK {
		t.Fatalf("done %+v", d)
	}
}

// watch reports the detached process's exit.
func TestWatchReportsTheExit(t *testing.T) {
	for script, ok := range map[string]bool{"exit 0": true, "exit 3": false} {
		cmd := exec.Command("sh", "-c", script)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		tr := watch(cmd)
		select {
		case <-tr.done:
		case <-time.After(10 * time.Second):
			t.Fatal("not reaped")
		}
		if (tr.err == nil) != ok {
			t.Fatalf("%s: err %v", script, tr.err)
		}
	}
}

// Nothing to wait for (no upload started, or only a push): not OK, at once.
func TestShareWaitWithoutATransfer(t *testing.T) {
	s, out := shareSession(t)
	tr, finish := pending()
	finish(nil)
	s.track(protocol.ShareImage, tr)
	if err := s.shareWait(protocol.ShareWait{Kind: protocol.ShareDeps}); err != nil {
		t.Fatal(err)
	}
	var d protocol.ShareDone
	replies(t, out, protocol.TypeShareDone, &d)
	if d.OK || d.Message == "" {
		t.Fatalf("done %+v", d)
	}
}

// A transfer still running at the limit is reported as such: the waiting
// runs then build for themselves.
func TestShareWaitGivesUpAtItsLimit(t *testing.T) {
	s, out := shareSession(t)
	tr, _ := pending()
	s.track(protocol.ShareImage, tr)
	start := time.Now()
	if err := s.shareWait(protocol.ShareWait{Kind: protocol.ShareImage, TimeoutMS: 100}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > 5*time.Second {
		t.Fatalf("gave up after %s, limit 100ms", took)
	}
	var d protocol.ShareDone
	replies(t, out, protocol.TypeShareDone, &d)
	if d.OK || !strings.Contains(d.Message, "still running") {
		t.Fatalf("done %+v", d)
	}
}

// A client that went away ends the wait (and the session) instead of
// holding the helper for the whole transfer.
func TestShareWaitEndsWhenTheClientIsGone(t *testing.T) {
	old := shareTick
	shareTick = 10 * time.Millisecond
	t.Cleanup(func() { shareTick = old })
	s, _ := shareSession(t)
	s.lastSeen.Store(s.now() - int64(time.Hour))
	tr, _ := pending()
	s.track(protocol.ShareDeps, tr)
	err := s.shareWait(protocol.ShareWait{Kind: protocol.ShareDeps, TimeoutMS: 60000})
	if err == nil || !strings.Contains(err.Error(), "client went away") {
		t.Fatalf("err = %v", err)
	}
}

// registryDocker puts a docker on PATH that tracks images by full
// reference, each with its ID. The returned directory seeds them: files
// local-<tag> (spotexec-local/r:<tag>, ID sha256:feed), remote-<tag> (the
// runner's registry, ID sha256:home) and source-<tag> (another region's
// registry src.example, ID sha256:src); nologin-<server> fails that login.
// Builds and pushes ("<ref> <id>") are logged.
func registryDocker(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
d=` + shq(dir) + `
ref() { echo "$d/ref-$(echo "$1" | tr '/:' '__')"; }
id() {
  if [ -e "$(ref "$1")" ]; then cat "$(ref "$1")"; return 0; fi
  case "$1" in spotexec-local/r:*) [ -e "$d/local-${1##*:}" ] && { echo sha256:feed; return 0; } ;; esac
  return 1
}
[ "$1" = --config ] && shift 2
case "$1" in
info) exit 0 ;;
login) cat >/dev/null; for a; do srv=$a; done
  [ -e "$d/nologin-$srv" ] && { echo "denied" >&2; exit 1; }
  exit 0 ;;
image) [ "$2" = inspect ] || exit 1
  for a; do r=$a; done
  id "$r" || exit 1; exit 0 ;;
pull) for a; do r=$a; done; tag=${r##*:}; have=remote; img=sha256:home
  case "$r" in src.example/*) have=source; img=sha256:src ;; esac
  [ -e "$d/$have-$tag" ] || { echo "manifest unknown" >&2; exit 1; }
  echo "$img" > "$(ref "$r")"; exit 0 ;;
tag) i=$(id "$2") || { echo "no such image $2" >&2; exit 1; }
  echo "$i" > "$(ref "$3")"; exit 0 ;;
rmi) rm -f "$(ref "$2")"; exit 0 ;;
build) echo "$@" >> "$d/builds"; exit 0 ;;
push) for a; do r=$a; done
  i=$(id "$r") || { echo "no such image $r" >&2; exit 1; }
  echo "$r $i" >> "$d/pushes"; exit 0 ;;
*) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// #139: an image probe uses an image already on the runner or in the
// registry, and otherwise reports it missing without building it.
func TestImageProbe(t *testing.T) {
	const tag = "0123456789abcdef"
	for _, tc := range []struct {
		name          string
		have          string // "", "local-" or "remote-"
		missing, pull bool
	}{
		{"missing everywhere", "", true, false},
		{"in the registry", "remote-", false, true},
		{"on the runner", "local-", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := registryDocker(t)
			if tc.have != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.have+tag), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := testSession(t, t.TempDir())
			if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
				t.Fatal(err)
			}
			out := &syncBuf{}
			s.c = protocol.NewConn(strings.NewReader(""), out)
			err := s.image(protocol.Image{Dockerfile: "Dockerfile", Tag: tag, Probe: true,
				Registry: &protocol.RegistryAuth{Server: "reg.example", Repo: "reg.example/spotexec/r", User: "AWS", Password: "pw"}})
			if err != nil {
				t.Fatal(err)
			}
			var res protocol.ImageResult
			replies(t, out, protocol.TypeImageResult, &res)
			if res.Missing != tc.missing || res.Pulled != tc.pull || res.Built || res.Pushing {
				t.Fatalf("result %+v", res)
			}
			if _, err := os.Stat(filepath.Join(dir, "builds")); !os.IsNotExist(err) {
				t.Fatalf("probe built the image: %v", err)
			}
		})
	}
}

// #194: only the image build itself failing is marked as a failure to
// remember: not docker missing under the shell, the login profile giving
// up, a build killed under its shell, the daemon going away during the
// build, or a setup failure. The mark reaches the client.
func TestImageBuildFailureIsMarked(t *testing.T) {
	const tag = "0123456789abcdef"
	for _, tc := range []struct {
		name       string
		exit       int
		daemonGone bool // the build takes the daemon down with it
		synced     bool
		marked     bool
	}{
		{"the build fails", 1, false, true, true},
		{"the profile gives up", 125, false, true, false},
		{"docker isn't there", 127, false, true, false},
		{"the build is killed", 137, false, true, false},
		{"the daemon goes away", 1, true, true, false},
		{"the Dockerfile wasn't synced", 1, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gone := ""
			if tc.daemonGone {
				gone = "touch \"$d/gone\"; "
			}
			script := fmt.Sprintf("#!/bin/sh\nd=%s\ncase \"$1\" in\ninfo) [ -e \"$d/gone\" ] && exit 1; exit 0 ;;\nimage) exit 1 ;;\nbuild) touch \"$d/built\"; %secho 'RUN false: exit 1' >&2; exit %d ;;\nesac\nexit 0\n", shq(dir), gone, tc.exit)
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// The build runs under a login profile, which may set its own
			// PATH (macOS's /etc/paths.d puts any installed docker first):
			// the profile in HOME puts the stub back in front.
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("PATH="+shq(dir)+":$PATH\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", dir+":/usr/bin:/bin")
			s := testSession(t, t.TempDir())
			if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.workspace, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			s.stored = map[string]storedEntry{}
			if tc.synced {
				s.stored["Dockerfile"] = storedEntry{}
			}
			s.c = protocol.NewConn(strings.NewReader(""), &syncBuf{})
			err := s.image(protocol.Image{Dockerfile: "Dockerfile", Tag: tag})
			var pe *protoError
			if !errors.As(err, &pe) || pe.code != protocol.CodeImageFailed {
				t.Fatalf("got %v", err)
			}
			if _, serr := os.Stat(filepath.Join(dir, "built")); (serr == nil) != tc.synced {
				t.Fatalf("stub build ran: %v, want %v", serr == nil, tc.synced)
			}
			if pe.buildFailed != tc.marked || pe.wire().BuildFailed != tc.marked {
				t.Fatalf("%v: marked %v (sent %v), want %v", err, pe.buildFailed, pe.wire().BuildFailed, tc.marked)
			}
		})
	}
}

// Only docker itself running to a failing exit is a failed build.
func TestStreamExitBuilt(t *testing.T) {
	for _, tc := range []struct {
		e    streamExit
		want bool
	}{
		{streamExit{code: 1, ran: true}, true},
		{streamExit{code: 2, ran: true}, true},
		{streamExit{code: 1, ran: false}, false}, // canceled
		{streamExit{code: 125, ran: true}, false},
		{streamExit{code: 126, ran: true}, false},
		{streamExit{code: 127, ran: true}, false},
		{streamExit{code: 124, ran: false}, false}, // timed out
		{streamExit{code: 143, ran: false}, false}, // killed
	} {
		if got := tc.e.built(); got != tc.want {
			t.Errorf("%+v: built %v, want %v", tc.e, got, tc.want)
		}
	}
}

// #192: an image the runner's registry lacks but another region's holds is
// copied from there and pushed to the runner's registry, never built.
func TestImageCopiesFromAnotherRegion(t *testing.T) {
	const tag = "0123456789abcdef"
	for _, tc := range []struct {
		name               string
		inSource, waitPush bool
		probe, noLogin     bool
		noRegistry         bool // the runner's own registry couldn't be had (#218)
	}{
		{"probe copies and pushes in the background", true, false, true, false, false},
		{"container build waits for the push", true, true, false, false, false},
		{"a source without it is a miss", false, false, true, false, false},
		{"container build can't push without a login", true, true, false, true, false},
		{"without a registry of its own, copies and pushes nothing", true, false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := registryDocker(t)
			seed := func(name string) {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.inSource {
				seed("source-" + tag)
			}
			if tc.noLogin {
				seed("nologin-reg.example")
			}
			s := testSession(t, t.TempDir())
			if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
				t.Fatal(err)
			}
			out := &syncBuf{}
			s.c = protocol.NewConn(strings.NewReader(""), out)
			req := protocol.Image{Dockerfile: "Dockerfile", Tag: tag, Probe: tc.probe, WaitPush: tc.waitPush,
				Registry: &protocol.RegistryAuth{Server: "reg.example", Repo: "reg.example/spotexec/r", User: "AWS", Password: "pw"},
				Source:   &protocol.RegistryAuth{Server: "src.example", Repo: "src.example/spotexec/r", User: "AWS", Password: "pw2"}}
			if tc.noRegistry {
				req.Registry = nil
			}
			err := s.image(req)
			if tc.noLogin {
				if err == nil || !strings.Contains(err.Error(), "could not log in to push") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var res protocol.ImageResult
			replies(t, out, protocol.TypeImageResult, &res)
			if _, err := os.Stat(filepath.Join(dir, "builds")); !os.IsNotExist(err) {
				t.Fatalf("built the image: %v", err)
			}
			if !tc.inSource {
				if !res.Missing || res.Copied || res.Pulled {
					t.Fatalf("result %+v", res)
				}
				return
			}
			// The source's image, under the local name, with its ID.
			if res.Missing || !res.Copied || !res.Pulled || res.Built || res.ImageID != "sha256:src" || res.Image != "spotexec-local/r:"+tag {
				t.Fatalf("result %+v", res)
			}
			if tc.noRegistry {
				if res.Pushed || res.Pushing {
					t.Fatalf("pushed %v, pushing %v", res.Pushed, res.Pushing)
				}
				if _, err := os.Stat(filepath.Join(dir, "pushes")); !os.IsNotExist(err) {
					t.Fatalf("pushed: %v", err)
				}
				return
			}
			if tc.waitPush != res.Pushed || tc.waitPush == res.Pushing {
				t.Fatalf("pushed %v, pushing %v", res.Pushed, res.Pushing)
			}
			if res.Pushing {
				tr := s.transfers[protocol.ShareImage]
				select {
				case <-tr.done:
				case <-time.After(time.Minute):
					t.Fatal("the push never ended")
				}
				if tr.err != nil {
					b, _ := os.ReadFile(filepath.Join(s.layout.Root, "image-push.log"))
					t.Fatalf("push: %v: %s", tr.err, b)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "pushes")); strings.TrimSpace(string(b)) != "reg.example/spotexec/r:"+tag+" sha256:src" {
				t.Fatalf("pushed %q", b)
			}
		})
	}
}

// A slot whose venvs an older helper relocated (text files only) has its
// symlinks into the cache's origin workspace relocated on its next run.
func TestDepsRelocatesVenvLinksOfOlderSlots(t *testing.T) {
	s, out := depsSession(t)
	d := protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "mkdir -p .venv && touch .venv/pyvenv.cfg", CacheDirs: []string{".venv"}}
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	replies(t, out, protocol.TypeDepsResult, &res)
	if !res.Built || s.state.VenvRelocation != venvRelocation {
		t.Fatalf("first run: %+v, state %+v", res, s.state)
	}
	// As hydrated from a cache built in /old/ws by a helper that left links.
	if err := os.WriteFile(filepath.Join(s.layout.Deps(s.repoID), res.Key, originFile), []byte("/old/ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.workspace, ".venv", "tool")
	if err := os.Symlink("/old/ws/bin/tool", link); err != nil {
		t.Fatal(err)
	}
	s.state.VenvRelocation = 0
	out.b.Reset()
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	replies(t, out, protocol.TypeDepsResult, &res)
	ws, _ := filepath.EvalSymlinks(s.workspace)
	if got, _ := os.Readlink(link); got != ws+"/bin/tool" {
		t.Fatalf("link -> %s", got)
	}
	if st := loadSlotState(s.slotDir); st.VenvRelocation != venvRelocation || st.DepsKey != res.Key {
		t.Fatalf("saved state %+v", st)
	}
}

// A slot an older helper relocated from a cache that doesn't record its
// origin can't say where its venv's links point any more: it is hydrated
// afresh from the cache rather than marked relocated as it is.
func TestDepsRehydratesOlderSlotsOfUnknownOrigin(t *testing.T) {
	s, out := depsSession(t)
	d := protocol.Deps{Lockfiles: []string{"deps.lock"}, Install: "mkdir -p .venv && touch .venv/pyvenv.cfg", CacheDirs: []string{".venv"}}
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	var res protocol.DepsResult
	replies(t, out, protocol.TypeDepsResult, &res)
	if err := os.Remove(filepath.Join(s.layout.Deps(s.repoID), res.Key, originFile)); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(s.workspace, ".venv", "tool")
	if err := os.Symlink("/old/ws/bin/tool", stale); err != nil {
		t.Fatal(err)
	}
	s.state.VenvsRelocated, s.state.VenvRelocation = true, 0
	out.b.Reset()
	if err := s.deps(d); err != nil {
		t.Fatal(err)
	}
	res = protocol.DepsResult{}
	replies(t, out, protocol.TypeDepsResult, &res)
	if !res.Hydrated {
		t.Fatalf("not hydrated afresh: %+v", res)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("the old copy's link survived: %v", err)
	}
	if st := loadSlotState(s.slotDir); st.VenvRelocation != venvRelocation || st.DepsKey != res.Key {
		t.Fatalf("saved state %+v", st)
	}
}
