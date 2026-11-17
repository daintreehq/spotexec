package helper

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

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

// playSync plays the client's side of a forward sync of w against s: it
// answers Need with the files asked for and returns what was asked.
func playSync(t *testing.T, s *Session, w workingSet) ([]string, protocol.Synced) {
	t.Helper()
	m := w.manifest()
	byPath := map[string]protocol.Entry{}
	content := map[string]string{}
	for k, v := range w {
		p := strings.TrimSuffix(strings.TrimSuffix(k, "@"), "*")
		content[p] = v
	}
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}
	pr, pw := io.Pipe()
	s.c = protocol.NewConn(strings.NewReader(""), pw)
	s.frames = make(chan protocol.Frame, 4096)
	type reply struct {
		need []string
		res  protocol.Synced
		err  error
	}
	done := make(chan reply, 1)
	go func() {
		in := protocol.NewConn(pr, io.Discard)
		var r reply
		for {
			fr, err := in.ReadFrame()
			if err != nil {
				r.err = err
				done <- r
				return
			}
			env, err := protocol.Decode(fr)
			if err != nil {
				r.err = err
				done <- r
				return
			}
			switch env.Type {
			case protocol.TypeNeed:
				var need protocol.Need
				_ = env.Unmarshal(&need)
				r.need = need.Paths
				if len(need.Paths) == 0 {
					continue
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
				done <- r
				return
			case protocol.TypeError:
				r.err = errors.New(string(env.Body))
				done <- r
				return
			}
		}
	}()
	if err := s.syncManifest(m); err != nil {
		t.Fatal(err)
	}
	r := <-done
	pw.Close()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.need, r.res
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
