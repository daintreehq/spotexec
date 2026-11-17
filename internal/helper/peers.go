package helper

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// A file another slot of the same repo already holds is copied from there
// rather than sent by the client again (#263): on a fresh VM, a working set
// crosses the network once, not once per slot. Copied, never linked, so a
// run in one slot can't change another slot's files. The copy is hashed as
// it is written and kept only when it is the content the manifest names: a
// run in the other slot may be changing the file meanwhile.

// peerFile is a file another slot of this repo synced, as that slot's
// manifest recorded it.
type peerFile struct {
	ws, rel string
	e       storedEntry
}

// peerIndex maps content IDs to the files other slots of this repo hold.
func (s *Session) peerIndex() map[string][]peerFile {
	idx := map[string][]peerFile{}
	slots := filepath.Join(s.layout.Repo(s.repoID), "slots")
	dirs, _ := os.ReadDir(slots)
	for _, d := range dirs {
		dir := filepath.Join(slots, d.Name())
		if !d.IsDir() || dir == s.slotDir {
			continue
		}
		var m map[string]storedEntry
		if readJSON(filepath.Join(dir, "manifest.json"), &m) != nil {
			continue
		}
		ws := filepath.Join(dir, "workspace")
		for rel, e := range m {
			idx[e.ID] = append(idx[e.ID], peerFile{ws: ws, rel: rel, e: e})
		}
	}
	return idx
}

// fillFromPeers copies what need lists from other slots of this repo where
// one holds the same content, and returns what is still needed. Every file
// written here is recorded in s.stored, as extract records what the client
// sends.
func (s *Session) fillFromPeers(need []string, want map[string]protocol.Entry) (rest []string, restBytes int64, copied int) {
	if len(need) == 0 {
		return need, 0, 0
	}
	idx := s.peerIndex()
	for _, p := range need {
		e := want[p]
		if e.Mode != fsid.ModeSymlink && s.copyFromPeer(p, e, idx[e.ID]) {
			copied++
			continue
		}
		if e.Mode == fsid.ModeSymlink && s.linkFromPeer(p, e, idx[e.ID]) {
			copied++
			continue
		}
		rest = append(rest, p)
		restBytes += e.Size
	}
	return rest, restBytes, copied
}

// peerSource opens the first of srcs still holding what its slot recorded
// (by stat signature) at e's size, and returns it with its full path.
func peerSource(srcs []peerFile, e protocol.Entry, symlink bool) (full string, ok bool) {
	for _, src := range srcs {
		if src.e.Size != e.Size || (src.e.Mode == fsid.ModeSymlink) != symlink {
			continue
		}
		full, err := fsid.SafeJoin(src.ws, src.rel)
		if err != nil {
			continue
		}
		fi, err := os.Lstat(full)
		if err != nil {
			continue
		}
		if sg, ok := sigOf(fi); ok && sg == src.e.sig() {
			return full, true
		}
	}
	return "", false
}

// copyFromPeer writes regular file p from a peer copy of its content.
func (s *Session) copyFromPeer(p string, e protocol.Entry, srcs []peerFile) bool {
	src, ok := peerSource(srcs, e, false)
	if !ok {
		return false
	}
	f, err := os.Open(src)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() != e.Size {
		return false
	}
	perm := int64(0o644)
	if e.Mode == fsid.ModeExec {
		perm = 0o755
	}
	return s.placeFromPeer(p, e, &tar.Header{Name: p, Typeflag: tar.TypeReg, Size: e.Size, Mode: perm}, io.LimitReader(f, e.Size+1)) // a file that grows fails the hash
}

// linkFromPeer recreates symlink p from a peer's identical link.
func (s *Session) linkFromPeer(p string, e protocol.Entry, srcs []peerFile) bool {
	src, ok := peerSource(srcs, e, true)
	if !ok {
		return false
	}
	target, err := os.Readlink(src)
	if err != nil || fsid.Bytes([]byte(target)) != e.ID {
		return false
	}
	return s.placeFromPeer(p, e, &tar.Header{Name: p, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}, nil)
}

// placeFromPeer writes one entry as extract would, and reports whether what
// landed is the content and mode the manifest names. Whatever landed is
// recorded; a mismatch stays needed, and the client's copy replaces it.
func (s *Session) placeFromPeer(p string, e protocol.Entry, h *tar.Header, r io.Reader) bool {
	full, err := fsid.PrepareParent(s.workspace, p)
	if err != nil {
		return false
	}
	id, mode, err := writeEntry(full, h, r)
	if err != nil {
		return false
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return false
	}
	sg, _ := sigOf(fi)
	s.stored[p] = entryFrom(id, sg)
	return id == e.ID && mode == e.Mode
}
