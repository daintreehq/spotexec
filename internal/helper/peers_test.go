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
	need []string
	res  protocol.Synced
	err  error
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
				r.need = need.Paths
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
	need, res := playSync(t, b, second)
	if want := []string{"only/c.txt"}; !reflect.DeepEqual(need, want) {
		t.Fatalf("second slot asked for %v, want %v", need, want)
	}
	if res.Written != 1 {
		t.Fatalf("written = %d, want 1", res.Written)
	}
	checkWorkspace(t, b.workspace, second)

	// Copies, not links: a run changing one slot's file leaves the other's.
	writeFile(t, a.workspace, "a.txt", "changed by a run\n")
	checkWorkspace(t, b.workspace, workingSet{"a.txt": "alpha\n"})

	// What was copied is recorded as synced: the next sync needs nothing.
	need, _ = playSync(t, b, second)
	if len(need) != 0 {
		t.Fatalf("resync asked for %v", need)
	}
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

	var atNeed string
	r := client{onNeed: func([]string) {
		bb, _ := os.ReadFile(filepath.Join(b.workspace, "a.txt"))
		atNeed = string(bb)
	}}.sync(b, workingSet{"a.txt": "alpha\n"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := []string{"a.txt"}; !reflect.DeepEqual(r.need, want) {
		t.Fatalf("asked for %v, want %v", r.need, want)
	}
	if atNeed != "older\n" {
		t.Fatalf("a.txt was %q when the client was asked, want the slot's own %q", atNeed, "older\n")
	}
	checkWorkspace(t, b.workspace, workingSet{"a.txt": "alpha\n"})
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
	close(hold)

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

	scanned, resume := make(chan struct{}), make(chan struct{})
	peerScanned = func(s *Session) {
		if s == b {
			close(scanned)
			<-resume
		}
	}
	t.Cleanup(func() { peerScanned = nil })

	reply := make(chan syncReply, 1)
	go func() { reply <- client{}.sync(b, ws) }()
	<-scanned
	if need, _ := playSync(t, a, ws); len(need) != len(ws) {
		t.Fatalf("first slot asked for %d files, want %d", len(need), len(ws))
	}
	close(resume)
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
	first := make(chan syncReply, 1)
	go func() { first <- client{hold: hold, onNeed: func([]string) { close(asked) }}.sync(a, ws) }()
	<-asked
	need, _ := playSync(t, b, ws)
	if len(need) != len(ws) {
		t.Fatalf("asked for %d files, want all %d", len(need), len(ws))
	}
	checkWorkspace(t, b.workspace, ws)
	close(hold)
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
	first := make(chan syncReply, 1)
	go func() { first <- client{hold: hold, onNeed: func([]string) { close(asked) }}.sync(a, bigSet(nil)) }()
	<-asked
	if need, _ := playSync(t, b, workingSet{"a.txt": "alpha\n"}); len(need) != 1 {
		t.Fatalf("asked for %v", need)
	}
	close(hold)
	if r := <-first; r.err != nil {
		t.Fatal(r.err)
	}
}
