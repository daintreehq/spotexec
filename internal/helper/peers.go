package helper

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// A file another slot of the same repo already holds is copied from there
// rather than sent by the client again (#263): on a fresh VM, a working set
// crosses the network once, not once per slot. Copied, never linked, so a
// run in one slot can't change another slot's files. A copy is hashed as it
// is staged and put in place only when it is the content the manifest
// names: a run in the other slot may be changing the file meanwhile, and
// whatever path it was read through, bytes with the wanted ID are the bytes
// the client would have sent.
//
// Slots syncing at once (eight cold slots filling on a fresh VM) would all
// find nothing to copy and each ask the client for everything. So a sync
// with a large remainder takes the repo's upload lock and holds it while the
// client's files arrive and until they are recorded in the slot's manifest;
// another waits for it, then copies what that upload brought.

// SyncLock is held by the slot receiving a large upload for its repo.
// Never unlinked, like every lock file.
func (l Layout) SyncLock(repoID string) string { return filepath.Join(l.Repo(repoID), "sync.lock") }

// A remainder at least this large waits its turn to upload: a smaller one
// costs less sent twice than queued behind another slot's upload.
const (
	shareMinFiles = 256
	shareMinBytes = 1 << 20
)

// syncShareWait bounds the wait for another slot's upload. Past it the
// sync asks the client for what is still missing: a stalled upload must not
// hold every other slot of the repo.
var syncShareWait = 5 * time.Minute

// peerFile is a file another slot of this repo synced, as that slot's
// manifest recorded it.
type peerFile struct {
	ws, rel string
	e       storedEntry
}

// peerView identifies the peer manifests a scan read, to tell whether any
// was published since (writeJSON replaces the file).
type peerView map[string]os.FileInfo

// peerManifests lists the manifest of every other slot of this repo.
func (s *Session) peerManifests() []string {
	slots := filepath.Join(s.layout.Repo(s.repoID), "slots")
	dirs, _ := os.ReadDir(slots)
	var out []string
	for _, d := range dirs {
		dir := filepath.Join(slots, d.Name())
		if d.IsDir() && dir != s.slotDir {
			out = append(out, filepath.Join(dir, "manifest.json"))
		}
	}
	return out
}

// peersMoved reports whether a peer manifest was published since v.
func (s *Session) peersMoved(v peerView) bool {
	for _, p := range s.peerManifests() {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if old, ok := v[p]; !ok || !os.SameFile(old, fi) || !old.ModTime().Equal(fi.ModTime()) || old.Size() != fi.Size() {
			return true
		}
	}
	return false
}

// peerIndex maps the content IDs in ids to the files other slots of this
// repo hold, and returns the manifests it read.
func (s *Session) peerIndex(ids map[string]bool) (map[string][]peerFile, peerView) {
	idx := map[string][]peerFile{}
	v := peerView{}
	for _, mp := range s.peerManifests() {
		fi, err := os.Stat(mp)
		if err != nil {
			continue
		}
		v[mp] = fi // even unreadable: only a newer one is news
		var m map[string]storedEntry
		if readJSON(mp, &m) != nil {
			continue
		}
		ws := filepath.Join(filepath.Dir(mp), "workspace")
		for rel, e := range m {
			if ids[e.ID] {
				idx[e.ID] = append(idx[e.ID], peerFile{ws: ws, rel: rel, e: e})
			}
		}
	}
	return idx, v
}

// peerTries bounds the candidates read for one file.
const peerTries = 3

// fillFromPeers copies what need lists from other slots of this repo where
// one holds the same content, and returns what is still needed. Every file
// put in place is recorded in s.stored, as extract records what the client
// sends.
func (s *Session) fillFromPeers(need []string, want map[string]protocol.Entry) (rest []string, copied int, v peerView) {
	if len(need) == 0 {
		return nil, 0, nil
	}
	ids := map[string]bool{}
	for _, p := range need {
		ids[want[p].ID] = true
	}
	idx, v := s.peerIndex(ids)
	good := map[string]string{} // content ID -> a source that just verified
	for _, p := range need {
		e := want[p]
		if validMode(e.Mode) && s.fromPeers(p, e, idx[e.ID], good) {
			copied++
			continue
		}
		rest = append(rest, p)
	}
	return rest, copied, v
}

func validMode(m uint32) bool {
	return m == fsid.ModeFile || m == fsid.ModeExec || m == fsid.ModeSymlink
}

// peerLooks bounds the candidates examined for one file: many files can
// share one content ID (every empty file does), and each is a candidate.
const peerLooks = 16

// fromPeers puts e in place at p from the first candidate that verifies:
// the source that last verified for this content, then those still
// matching what their slot recorded, then any of the right type and size
// (a file touched since can still hold the bytes).
func (s *Session) fromPeers(p string, e protocol.Entry, srcs []peerFile, good map[string]string) bool {
	if src, ok := good[e.ID]; ok && s.placeFromPeer(p, e, src) == nil {
		return true
	}
	link := e.Mode == fsid.ModeSymlink
	var fresh, other []string
	for i, src := range srcs {
		if i == peerLooks {
			break
		}
		if (src.e.Mode == fsid.ModeSymlink) != link || src.e.Size != e.Size {
			continue
		}
		if _, err := fsid.CleanRel(src.rel); err != nil {
			continue
		}
		full := filepath.Join(src.ws, filepath.FromSlash(src.rel))
		fi, err := os.Lstat(full)
		if err != nil || (fi.Mode()&os.ModeSymlink != 0) != link || fi.Size() != e.Size {
			continue
		}
		if sg, ok := sigOf(fi); ok && sg == src.e.sig() {
			fresh = append(fresh, full)
		} else {
			other = append(other, full)
		}
	}
	for i, full := range append(fresh, other...) {
		if i == peerTries {
			break
		}
		if s.placeFromPeer(p, e, full) == nil {
			good[e.ID] = full
			return true
		}
	}
	return false
}

// placeFromPeer stages a copy of src, checks it is e's content, and only
// then replaces p with it and records it.
func (s *Session) placeFromPeer(p string, e protocol.Entry, src string) error {
	if e.Mode == fsid.ModeSymlink {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if fsid.Bytes([]byte(target)) != e.ID {
			return fmt.Errorf("%s: content changed", src)
		}
		full, err := fsid.PrepareParent(s.workspace, p)
		if err != nil {
			return err
		}
		if _, _, err := writeEntry(full, &tar.Header{Name: p, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}, nil); err != nil {
			return err
		}
		return s.record(p, full, e.ID)
	}
	// Never a symlink in its place, and never stuck on a FIFO put there.
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() != e.Size {
		return fmt.Errorf("%s: not the recorded file", src)
	}
	// Staged beside the workspace (same filesystem, out of a run's tree),
	// and nothing in the workspace changes until the copy has verified.
	tmp := filepath.Join(s.slotDir, fmt.Sprintf(".peer-tmp-%d-%d", os.Getpid(), time.Now().UnixNano()))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fsid.Perm(e.Mode))
	if err != nil {
		return err
	}
	var full string
	// A file that grows meanwhile reads past its size and fails the hash.
	id, err := fsid.Reader(io.TeeReader(io.LimitReader(f, e.Size+1), out), e.Size)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && id != e.ID {
		err = fmt.Errorf("%s: content changed", src)
	}
	if err == nil {
		err = os.Chmod(tmp, fsid.Perm(e.Mode)) // umask-proof
	}
	if err == nil {
		full, err = fsid.PrepareParent(s.workspace, p)
	}
	if err == nil {
		// Replace a directory occupying the target path (type change).
		if fi, lerr := os.Lstat(full); lerr == nil && fi.IsDir() {
			err = os.RemoveAll(full)
		}
	}
	if err == nil {
		err = os.Rename(tmp, full)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return s.record(p, full, id)
}

// record stores what is now at full as p's synced entry.
func (s *Session) record(p, full, id string) error {
	fi, err := os.Lstat(full)
	if err != nil {
		return err
	}
	sg, ok := sigOf(fi)
	if !ok {
		return fmt.Errorf("%s: unsupported file type", p)
	}
	s.stored[p] = entryFrom(id, sg)
	return nil
}

// shareWorthy: a remainder worth waiting for another slot's upload.
func shareWorthy(need []string, want map[string]protocol.Entry) bool {
	if len(need) >= shareMinFiles {
		return true
	}
	var b int64
	for _, p := range need {
		b += want[p].Size
	}
	return b >= shareMinBytes
}

// uploadTurn fills need from other slots and, while what remains is worth
// sharing, waits for the repo's upload lock, copying again whatever an
// upload published meanwhile. It returns the lock to hold until what the
// client sends is recorded (nil when the remainder is small, or the wait
// ran out), what is still needed, and how many files were copied.
func (s *Session) uploadTurn(need []string, want map[string]protocol.Entry) (*flock, []string, int, error) {
	rest, copied, v := s.fillFromPeers(need, want)
	if peerScanned != nil {
		peerScanned(s)
	}
	deadline := time.Now().Add(syncShareWait)
	waited := false
	for shareWorthy(rest, want) {
		l, ok, err := tryLock(s.layout.SyncLock(s.repoID))
		if err != nil {
			return nil, nil, copied, err
		}
		if ok {
			// An upload may have been published between the scan and
			// the lock: copy that first, outside the lock.
			if !s.peersMoved(v) {
				return l, rest, copied, nil
			}
			l.Unlock()
		} else {
			if !waited {
				waited = true
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "sync_wait", Message: "waiting for another slot on this runner to receive the same files"})
			}
			if e, _ := s.readErr.Load().(error); e != nil || s.clientGone() {
				return nil, nil, copied, perr(protocol.CodeSyncFailed, "client went away while waiting for another slot's upload")
			}
			if !time.Now().After(deadline) {
				time.Sleep(250 * time.Millisecond)
				if !s.peersMoved(v) {
					continue
				}
			}
		}
		var n int
		rest, n, v = s.fillFromPeers(rest, want)
		copied += n
		if time.Now().After(deadline) {
			break
		}
	}
	return nil, rest, copied, nil
}

// peerScanned is a test hook run after a sync's first scan of its peers.
var peerScanned func(*Session)
