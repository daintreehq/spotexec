package helper

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// workingSet is a client's working tree: path -> content. A path ending in
// "@" is a symlink whose target is the content; "*" an executable.
type workingSet map[string]string

func (w workingSet) manifest() protocol.Manifest {
	var m protocol.Manifest
	for k, v := range w {
		p, mode := k, fsid.ModeFile
		switch {
		case strings.HasSuffix(k, "@"):
			p, mode = strings.TrimSuffix(k, "@"), fsid.ModeSymlink
		case strings.HasSuffix(k, "*"):
			p, mode = strings.TrimSuffix(k, "*"), fsid.ModeExec
		}
		m.Entries = append(m.Entries, protocol.Entry{Path: p, ID: fsid.Bytes([]byte(v)), Mode: mode, Size: int64(len(v))})
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m
}

// client plays the CLI's side of a forward sync. hold, when set, is waited
// on before the archive goes; onNeed sees what was asked first; events gets
// every event's name.
type client struct {
	hold   chan struct{}
	onNeed func([]string)
	events chan string
}

type syncReply struct {
	need      []string
	needBytes int64
	res       protocol.Synced
	err       error
}

// playSync syncs w to s and returns what the helper asked for.
func playSync(t *testing.T, s *Session, w workingSet) ([]string, protocol.Synced) {
	t.Helper()
	r := client{}.sync(s, w)
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.need, r.res
}

func (c client) sync(s *Session, w workingSet) syncReply {
	m := w.manifest()
	byPath := map[string]protocol.Entry{}
	content := map[string]string{}
	for k, v := range w {
		content[strings.TrimSuffix(strings.TrimSuffix(k, "@"), "*")] = v
	}
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}
	pr, pw := io.Pipe()
	s.c = protocol.NewConn(strings.NewReader(""), pw)
	s.frames = make(chan protocol.Frame, 4096)
	done := make(chan syncReply, 1)
	go func() {
		in := protocol.NewConn(pr, io.Discard)
		var r syncReply
		defer func() { done <- r }()
		for {
			fr, err := in.ReadFrame()
			if err != nil {
				r.err = err
				return
			}
			env, err := protocol.Decode(fr)
			if err != nil {
				r.err = err
				return
			}
			switch env.Type {
			case protocol.TypeEvent:
				var ev protocol.Event
				_ = env.Unmarshal(&ev)
				if c.events != nil {
					c.events <- ev.Name
				}
			case protocol.TypeNeed:
				var need protocol.Need
				_ = env.Unmarshal(&need)
				r.need, r.needBytes = need.Paths, need.Bytes
				if c.onNeed != nil {
					c.onNeed(need.Paths)
				}
				if len(need.Paths) == 0 {
					continue
				}
				if c.hold != nil {
					<-c.hold
				}
				bpr, bpw := io.Pipe()
				go func() {
					bw := &protocol.BlobWriter{C: protocol.NewConn(strings.NewReader(""), bpw)}
					zw := gzip.NewWriter(bw)
					tw := tar.NewWriter(zw)
					var err error
					for _, p := range need.Paths {
						e := byPath[p]
						switch e.Mode {
						case fsid.ModeSymlink:
							err = errors.Join(err, tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeSymlink, Linkname: content[p], Mode: 0o777}))
						default:
							mode := int64(0o644)
							if e.Mode == fsid.ModeExec {
								mode = 0o755
							}
							err = errors.Join(err, tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Size: e.Size, Mode: mode}))
							_, werr := tw.Write([]byte(content[p]))
							err = errors.Join(err, werr)
						}
					}
					err = errors.Join(err, tw.Close(), zw.Close())
					bw.CloseWithError(err)
					bpw.Close()
				}()
				bc := protocol.NewConn(bpr, io.Discard)
				for {
					bf, err := bc.ReadFrame()
					if err != nil {
						break
					}
					s.frames <- bf
				}
			case protocol.TypeSynced:
				r.err = env.Unmarshal(&r.res)
				return
			case protocol.TypeError:
				r.err = errors.New(string(env.Body))
				return
			}
		}
	}()
	err := s.syncManifest(m)
	pw.Close()
	r := <-done
	if err != nil {
		r.err = err
	}
	return r
}

// slotSession acquires a slot of repo r on root for a new session.
func slotSession(t *testing.T, root string, a protocol.Acquire) *Session {
	t.Helper()
	s := testSession(t, root)
	if a.RepoID == "" {
		a.RepoID = "r"
	}
	if a.MaxSlots == 0 {
		a.MaxSlots = 4
	}
	if err := s.acquire(a); err != nil {
		t.Fatal(err)
	}
	return s
}

func checkWorkspace(t *testing.T, ws string, w workingSet) {
	t.Helper()
	for k, v := range w {
		p := strings.TrimSuffix(strings.TrimSuffix(k, "@"), "*")
		full := filepath.Join(ws, p)
		fi, err := os.Lstat(full)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		switch {
		case strings.HasSuffix(k, "@"):
			if got, err := os.Readlink(full); err != nil || got != v {
				t.Errorf("%s -> %q (%v), want -> %q", p, got, err, v)
			}
			continue
		case strings.HasSuffix(k, "*"):
			if fi.Mode().Perm()&0o111 == 0 {
				t.Errorf("%s is not executable (%v)", p, fi.Mode())
			}
		default:
			if fi.Mode().Perm()&0o111 != 0 {
				t.Errorf("%s is executable (%v)", p, fi.Mode())
			}
		}
		if b, err := os.ReadFile(full); err != nil || string(b) != v {
			t.Errorf("%s = %q (%v), want %q", p, b, err, v)
		}
	}
}

// A slot syncing a working set another slot of the repo already holds asks
// the client only for what differs, and copies the rest from that slot (#263).
func TestSyncCopiesWhatAnotherSlotHolds(t *testing.T) {
	root := t.TempDir()
	first := workingSet{"a.txt": "alpha\n", "src/b.go": "package b\n", "bin/run*": "#!/bin/sh\n", "link@": "src/b.go", "same/a.txt": "alpha\n"}
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	need, _ := playSync(t, a, first)
	if len(need) != len(first) {
		t.Fatalf("first slot asked for %v, want all %d files", need, len(first))
	}

	second := workingSet{"a.txt": "alpha\n", "src/b.go": "package b\n", "bin/run*": "#!/bin/sh\n", "link@": "src/b.go", "only/c.txt": "mine\n", "renamed.txt": "alpha\n"}
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	if b.slotDir == a.slotDir {
		t.Fatal("both sessions hold one slot")
	}
	r := client{}.sync(b, second)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := []string{"only/c.txt"}; !reflect.DeepEqual(r.need, want) {
		t.Fatalf("second slot asked for %v, want %v", r.need, want)
	}
	if r.needBytes != int64(len("mine\n")) {
		t.Fatalf("Need.Bytes = %d, want only what is still sent (%d)", r.needBytes, len("mine\n"))
	}
	if r.res.Written != 1 || r.res.Reused != len(second)-1 {
		t.Fatalf("written %d, reused %d; want 1 and %d", r.res.Written, r.res.Reused, len(second)-1)
	}
	checkWorkspace(t, b.workspace, second)

	// What was copied is recorded as synced in the slot's manifest.
	var rec map[string]storedEntry
	if err := readJSON(b.manifestPath(), &rec); err != nil {
		t.Fatal(err)
	}
	for _, e := range second.manifest().Entries {
		if rec[e.Path].ID != e.ID {
			t.Errorf("%s recorded as %q, want %q", e.Path, rec[e.Path].ID, e.ID)
		}
	}

	// Copies, not links: a run changing one slot's file leaves the other's.
	writeFile(t, a.workspace, "src/b.go", "changed by a run\n")
	checkWorkspace(t, b.workspace, workingSet{"src/b.go": "package b\n"})
}

// A peer file that no longer holds what its slot recorded (a run there
// changed it) is never copied: the client sends it.
func TestSyncSkipsAChangedPeerFile(t *testing.T) {
	root := t.TempDir()
	ws := workingSet{"a.txt": "alpha\n", "b.txt": "beta\n"}
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	playSync(t, a, ws)
	writeFile(t, a.workspace, "a.txt", "omega\n") // same size, other content

	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	need, _ := playSync(t, b, ws)
	if want := []string{"a.txt"}; !reflect.DeepEqual(need, want) {
		t.Fatalf("asked for %v, want %v", need, want)
	}
	checkWorkspace(t, b.workspace, ws)
}

// A peer file whose recorded signature still matches but whose content
// changed (the signature can't see every write) is copied, found wrong by
// its hash, and sent by the client.
func TestSyncHashChecksAPeerCopy(t *testing.T) {
	root := t.TempDir()
	ws := workingSet{"a.txt": "alpha\n"}
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	playSync(t, a, ws)
	// The slot's record still says alpha, at the changed file's signature.
	writeFile(t, a.workspace, "a.txt", "omega\n")
	fi, err := os.Lstat(filepath.Join(a.workspace, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sg, _ := sigOf(fi)
	if err := writeJSON(a.manifestPath(), map[string]storedEntry{"a.txt": entryFrom(fsid.Bytes([]byte("alpha\n")), sg)}); err != nil {
		t.Fatal(err)
	}

	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	need, _ := playSync(t, b, ws)
	if want := []string{"a.txt"}; !reflect.DeepEqual(need, want) {
		t.Fatalf("asked for %v, want %v", need, want)
	}
	checkWorkspace(t, b.workspace, ws)
}

// Another repo's slots are never a source.
func TestSyncNeverCopiesFromAnotherRepo(t *testing.T) {
	root := t.TempDir()
	ws := workingSet{"a.txt": "alpha\n"}
	playSync(t, slotSession(t, root, protocol.Acquire{RepoID: "other"}), ws)
	b := slotSession(t, root, protocol.Acquire{})
	need, _ := playSync(t, b, ws)
	if want := []string{"a.txt"}; !reflect.DeepEqual(need, want) {
		t.Fatalf("asked for %v, want %v", need, want)
	}
}

// A peer copy that isn't the wanted content never replaces the slot's file:
// at the moment the client is asked, the old file is still there.
func TestSyncRejectedPeerCopyLeavesTheFile(t *testing.T) {
	root := t.TempDir()
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	playSync(t, b, workingSet{"a.txt": "older\n"})
	writeFile(t, b.workspace, "out", "a run's output\n") // never synced

	// a records alpha at a.txt, but holds other bytes at that signature.
	writeFile(t, a.workspace, "a.txt", "omega\n")
	fi, err := os.Lstat(filepath.Join(a.workspace, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sg, _ := sigOf(fi)
	if err := writeJSON(a.manifestPath(), map[string]storedEntry{"a.txt": entryFrom(fsid.Bytes([]byte("alpha\n")), sg)}); err != nil {
		t.Fatal(err)
	}

	var atNeed, outAtNeed string
	ws := workingSet{"a.txt": "alpha\n", "out/a.txt": "alpha\n"}
	r := client{onNeed: func([]string) {
		bb, _ := os.ReadFile(filepath.Join(b.workspace, "a.txt"))
		atNeed = string(bb)
		bb, _ = os.ReadFile(filepath.Join(b.workspace, "out"))
		outAtNeed = string(bb)
	}}.sync(b, ws)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := []string{"a.txt", "out/a.txt"}; !reflect.DeepEqual(r.need, want) {
		t.Fatalf("asked for %v, want %v", r.need, want)
	}
	if atNeed != "older\n" {
		t.Fatalf("a.txt was %q when the client was asked, want the slot's own %q", atNeed, "older\n")
	}
	if outAtNeed != "a run's output\n" {
		t.Fatalf("out was %q when the client was asked: a rejected copy replaced it", outAtNeed)
	}
	checkWorkspace(t, b.workspace, ws)
}

// bigSet is a working set large enough that slots share its upload.
func bigSet(extra workingSet) workingSet {
	w := workingSet{}
	for i := range shareMinFiles + 10 {
		w[fmt.Sprintf("src/f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
	}
	for k, v := range extra {
		w[k] = v
	}
	return w
}

func waitEvent(t *testing.T, ch chan string, name string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == name {
				return
			}
		case <-timeout:
			t.Fatalf("no %s event", name)
		}
	}
}

// Slots of one repo filling at once on a fresh VM receive the working set
// from the client once between them: one uploads while the others wait,
// then copy it and ask only for what is theirs alone (#263).
func TestSyncColdSlotsShareOneUpload(t *testing.T) {
	root := t.TempDir()
	const n = 4
	var ss []*Session
	for i := range n {
		ss = append(ss, slotSession(t, root, protocol.Acquire{WorktreeKey: fmt.Sprintf("wt%d", i)}))
	}
	sets := make([]workingSet, n)
	for i := range n {
		sets[i] = bigSet(workingSet{fmt.Sprintf("own%d.txt", i): fmt.Sprintf("only in %d\n", i)})
	}

	// The first slot takes the upload and holds its archive until every
	// other slot is waiting for it.
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release) // never leave the uploader blocked on a failure
	asked := make(chan struct{})
	replies := make([]chan syncReply, n)
	for i := range replies {
		replies[i] = make(chan syncReply, 1)
	}
	go func() {
		replies[0] <- client{hold: hold, onNeed: func([]string) { close(asked) }}.sync(ss[0], sets[0])
	}()
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the first slot never asked for its files")
	}
	for i := 1; i < n; i++ {
		ev := make(chan string, 16)
		go func() { replies[i] <- client{events: ev}.sync(ss[i], sets[i]) }()
		waitEvent(t, ev, "sync_wait")
	}
	release()

	total := 0
	for i := range n {
		r := <-replies[i]
		if r.err != nil {
			t.Fatalf("slot %d: %v", i, r.err)
		}
		total += len(r.need)
		if i == 0 {
			if len(r.need) != len(sets[0]) {
				t.Fatalf("the uploading slot asked for %d files, want %d", len(r.need), len(sets[0]))
			}
			continue
		}
		if want := []string{fmt.Sprintf("own%d.txt", i)}; !reflect.DeepEqual(r.need, want) {
			t.Fatalf("slot %d asked for %v, want %v", i, r.need, want)
		}
		if r.res.Reused != len(sets[i])-1 {
			t.Fatalf("slot %d reused %d files, want %d", i, r.res.Reused, len(sets[i])-1)
		}
		checkWorkspace(t, ss[i].workspace, sets[i])
	}
	if want := len(sets[0]) + n - 1; total != want {
		t.Fatalf("the client sent %d files in all, want %d", total, want)
	}
}

// A slot that found nothing to copy, and then got the upload lock only after
// another slot had published its upload, copies that upload rather than
// asking the client again.
func TestSyncCopiesAnUploadPublishedBeforeItsTurn(t *testing.T) {
	root := t.TempDir()
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	ws := bigSet(nil)

	scanned, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	// Registered after the sessions, so it runs before they are released:
	// never leave the paused sync running past the test.
	t.Cleanup(func() { release(); <-finished })
	peerScanned = func(s *Session) {
		if s == b {
			close(scanned)
			<-resume
		}
	}
	t.Cleanup(func() { peerScanned = nil })

	reply := make(chan syncReply, 1)
	go func() { defer close(finished); reply <- client{}.sync(b, ws) }()
	select {
	case <-scanned:
	case r := <-reply:
		t.Fatalf("the slot finished before its peer scan: %+v", r)
	}
	if need, _ := playSync(t, a, ws); len(need) != len(ws) {
		t.Fatalf("first slot asked for %d files, want %d", len(need), len(ws))
	}
	release()
	r := <-reply
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.need) != 0 {
		t.Fatalf("asked for %d files the other slot had published", len(r.need))
	}
	checkWorkspace(t, b.workspace, ws)
}

// An upload that takes too long doesn't hold the other slots: past the wait
// they ask the client themselves.
func TestSyncStopsWaitingForAStalledUpload(t *testing.T) {
	old := syncShareWait
	syncShareWait = 300 * time.Millisecond
	t.Cleanup(func() { syncShareWait = old })
	root := t.TempDir()
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	ws := bigSet(nil)

	hold, asked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	first := make(chan syncReply, 1)
	go func() { first <- client{hold: hold, onNeed: func([]string) { close(asked) }}.sync(a, ws) }()
	<-asked
	ev := make(chan string, 16)
	start := time.Now()
	r := client{events: ev}.sync(b, ws)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if took := time.Since(start); took < syncShareWait || took > 10*time.Second {
		t.Fatalf("gave up after %s, want just past the %s wait", took, syncShareWait)
	}
	waitEvent(t, ev, "sync_wait")
	if len(r.need) != len(ws) {
		t.Fatalf("asked for %d files, want all %d", len(r.need), len(ws))
	}
	checkWorkspace(t, b.workspace, ws)
	release()
	if r := <-first; r.err != nil {
		t.Fatal(r.err)
	}
}

// A small remainder doesn't wait for another slot's upload.
func TestSyncSmallRemainderDoesNotWait(t *testing.T) {
	old := syncShareWait
	syncShareWait = time.Hour
	t.Cleanup(func() { syncShareWait = old })
	root := t.TempDir()
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})

	hold, asked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	first := make(chan syncReply, 1)
	go func() { first <- client{hold: hold, onNeed: func([]string) { close(asked) }}.sync(a, bigSet(nil)) }()
	<-asked
	if need, _ := playSync(t, b, workingSet{"a.txt": "alpha\n"}); len(need) != 1 {
		t.Fatalf("asked for %v", need)
	}
	release()
	if r := <-first; r.err != nil {
		t.Fatal(r.err)
	}
}

// A remainder shares an upload from 256 files or 1 MiB.
func TestShareWorthyThresholds(t *testing.T) {
	files := func(n int, size int64) ([]string, map[string]protocol.Entry) {
		var need []string
		want := map[string]protocol.Entry{}
		for i := range n {
			p := fmt.Sprintf("f%d", i)
			need = append(need, p)
			want[p] = protocol.Entry{Path: p, Size: size}
		}
		return need, want
	}
	for _, c := range []struct {
		n    int
		size int64
		want bool
	}{
		{255, 1, false},
		{256, 0, true},
		{1, 1<<20 - 1, false},
		{1, 1 << 20, true},
		{2, 1 << 19, true},
		{0, 0, false},
	} {
		need, want := files(c.n, c.size)
		if got := shareWorthy(need, want); got != c.want {
			t.Errorf("%d files of %d bytes: shareWorthy = %v, want %v", c.n, c.size, got, c.want)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

// Files copied from another slot stay recorded when the client is gone
// before it can be asked for the rest, so a later sync still prunes them.
func TestSyncRecordsCopiesWhenTheClientIsGone(t *testing.T) {
	root := t.TempDir()
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	playSync(t, a, workingSet{"a.txt": "alpha\n"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	b.c = protocol.NewConn(strings.NewReader(""), failWriter{})
	if err := b.syncManifest(workingSet{"a.txt": "alpha\n", "b.txt": "beta\n"}.manifest()); err == nil {
		t.Fatal("sync succeeded with no client")
	}
	var rec map[string]storedEntry
	if err := readJSON(b.manifestPath(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["a.txt"].ID != fsid.Bytes([]byte("alpha\n")) {
		t.Fatalf("the copied a.txt isn't recorded: %v", rec)
	}
}

// An entry whose mode isn't one a worktree can hold is never reused: the
// client is asked for it.
func TestSyncNeverReusesAnUnknownMode(t *testing.T) {
	root := t.TempDir()
	playSync(t, slotSession(t, root, protocol.Acquire{WorktreeKey: "one"}), workingSet{"a.txt": "alpha\n"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	b.loadStored()
	m := workingSet{"a.txt": "alpha\n"}.manifest()
	m.Entries[0].Mode = 0o100600
	rest, copied, _ := b.fillFromPeers([]string{"a.txt"}, map[string]protocol.Entry{"a.txt": m.Entries[0]})
	if copied != 0 || len(rest) != 1 {
		t.Fatalf("copied %d, still needed %v; want it left to the client", copied, rest)
	}
}

// Many needed files with one content (empty files) are all copied, from
// a source found once.
func TestSyncCopiesManyFilesOfOneContent(t *testing.T) {
	root := t.TempDir()
	ws := workingSet{}
	for i := range 300 {
		ws[fmt.Sprintf("pkg%03d/__init__.py", i)] = ""
	}
	playSync(t, slotSession(t, root, protocol.Acquire{WorktreeKey: "one"}), ws)
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	r := client{}.sync(b, ws)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.need) != 0 || r.res.Reused != len(ws) {
		t.Fatalf("asked for %d, reused %d; want 0 and %d", len(r.need), r.res.Reused, len(ws))
	}
	checkWorkspace(t, b.workspace, ws)
}

// A directory where the manifest wants a file is left to the client's copy.
func TestSyncLeavesATypeChangeToTheClient(t *testing.T) {
	root := t.TempDir()
	playSync(t, slotSession(t, root, protocol.Acquire{WorktreeKey: "one"}), workingSet{"a": "alpha\n"})
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	writeFile(t, b.workspace, "a/inner.txt", "a run's\n")
	var dirAtNeed bool
	r := client{onNeed: func([]string) {
		fi, err := os.Lstat(filepath.Join(b.workspace, "a"))
		dirAtNeed = err == nil && fi.IsDir()
	}}.sync(b, workingSet{"a": "alpha\n"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !dirAtNeed || len(r.need) != 1 {
		t.Fatalf("directory there when asked: %v, asked for %v", dirAtNeed, r.need)
	}
	checkWorkspace(t, b.workspace, workingSet{"a": "alpha\n"})
}

// Candidates that fail their hash are dropped once: the next file with
// that content goes straight to one that verifies.
func TestSyncDropsAFailingCandidate(t *testing.T) {
	root := t.TempDir()
	alpha := fsid.Bytes([]byte("alpha\n"))
	// Slot one records alpha at three files that hold other bytes, each at
	// its current signature, so they rank first.
	a := slotSession(t, root, protocol.Acquire{WorktreeKey: "one"})
	rec := map[string]storedEntry{}
	for _, p := range []string{"x1", "x2", "x3"} {
		writeFile(t, a.workspace, p, "omega\n")
		fi, err := os.Lstat(filepath.Join(a.workspace, p))
		if err != nil {
			t.Fatal(err)
		}
		sg, _ := sigOf(fi)
		rec[p] = entryFrom(alpha, sg)
	}
	if err := writeJSON(a.manifestPath(), rec); err != nil {
		t.Fatal(err)
	}
	// Slot two holds alpha, but at a signature its record no longer
	// matches, so it ranks after them.
	b := slotSession(t, root, protocol.Acquire{WorktreeKey: "two"})
	writeFile(t, b.workspace, "good", "alpha\n")
	if err := writeJSON(b.manifestPath(), map[string]storedEntry{"good": {ID: alpha, Mode: fsid.ModeFile, Size: 6}}); err != nil {
		t.Fatal(err)
	}

	c := slotSession(t, root, protocol.Acquire{WorktreeKey: "three"})
	ws := workingSet{"d1": "alpha\n", "d2": "alpha\n", "d3": "alpha\n", "d4": "alpha\n"}
	need, _ := playSync(t, c, ws)
	if want := []string{"d1"}; !reflect.DeepEqual(need, want) {
		t.Fatalf("asked for %v, want only the first file %v", need, want)
	}
	checkWorkspace(t, c.workspace, ws)
}
