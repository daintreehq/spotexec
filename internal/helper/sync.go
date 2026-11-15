package helper

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// storedEntry is the helper's record of a synced file: its content ID plus
// the stat signature observed when the ID was last verified.
type storedEntry struct {
	ID    string `json:"id"`
	Mode  uint32 `json:"m"`
	Size  int64  `json:"s"`
	Mtime int64  `json:"t"`
	Ino   uint64 `json:"i"`
	Ctime int64  `json:"c"`
}

type statSig struct {
	Mode  uint32
	Size  int64
	Mtime int64
	Ino   uint64
	Ctime int64
}

func sigOf(fi fs.FileInfo) (statSig, bool) {
	m, ok := fsid.Mode(fi.Mode())
	if !ok {
		return statSig{}, false
	}
	ino, ctime := inoCtime(fi)
	return statSig{Mode: m, Size: fi.Size(), Mtime: fi.ModTime().UnixNano(), Ino: ino, Ctime: ctime}, true
}

func (e storedEntry) sig() statSig {
	return statSig{Mode: e.Mode, Size: e.Size, Mtime: e.Mtime, Ino: e.Ino, Ctime: e.Ctime}
}

func entryFrom(id string, sg statSig) storedEntry {
	return storedEntry{ID: id, Mode: sg.Mode, Size: sg.Size, Mtime: sg.Mtime, Ino: sg.Ino, Ctime: sg.Ctime}
}

func (s *Session) manifestPath() string { return filepath.Join(s.slotDir, "manifest.json") }

func (s *Session) loadStored() map[string]storedEntry {
	if s.stored != nil {
		return s.stored
	}
	m := map[string]storedEntry{}
	if b, err := os.ReadFile(s.manifestPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	s.stored = m
	return m
}

func (s *Session) saveStored() error {
	return writeJSON(s.manifestPath(), s.stored)
}

// verify returns the current stored entry for path if the file on disk still
// matches (by stat signature, or by re-hashing when the signature moved).
func (s *Session) verify(rel string) (storedEntry, bool) {
	full, err := fsid.SafeJoin(s.workspace, rel)
	if err != nil {
		return storedEntry{}, false // symlinked ancestor: re-send and repair
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return storedEntry{}, false
	}
	sg, ok := sigOf(fi)
	if !ok {
		return storedEntry{}, false
	}
	if st, ok := s.stored[rel]; ok && st.sig() == sg {
		return st, true
	}
	id, _, _, err := fsid.Path(full)
	if err != nil {
		return storedEntry{}, false
	}
	return entryFrom(id, sg), true
}

func (s *Session) syncManifest(m protocol.Manifest) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before sync")
	}
	s.loadStored()
	// Mark the slot dirty first: if anything below fails half-way, the next
	// run must not trust the fingerprint.
	s.state.Fingerprint = ""
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return err
	}

	want := map[string]protocol.Entry{}
	for _, e := range m.Entries {
		if _, err := fsid.CleanRel(e.Path); err != nil {
			return perr(protocol.CodeBadRequest, "%v", err)
		}
		want[e.Path] = e
	}

	// Prune files we previously synced that are no longer in the manifest.
	deleted := 0
	var dirs []string
	for p := range s.stored {
		if _, ok := want[p]; ok {
			continue
		}
		// Never follow a symlinked ancestor out of the workspace.
		full, err := fsid.SafeJoin(s.workspace, p)
		if err == nil {
			if fi, err := os.Lstat(full); err == nil && !fi.IsDir() {
				if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
					_ = s.saveStored()
					return perr(protocol.CodeSyncFailed, "removing %s: %v", p, err)
				}
				deleted++
			}
		}
		delete(s.stored, p)
		dirs = append(dirs, path.Dir(p))
	}
	// Files earlier runs created and never wrote back: the client listed the
	// ones it doesn't ignore, which go now. Ignored outputs and anything in
	// a protected cache dir stay. Either way none is pending any more.
	pending := map[string]bool{}
	for _, p := range s.state.Pending {
		pending[p] = true
	}
	var keep []string
	for _, p := range m.Remove {
		if _, ok := want[p]; ok || !pending[p] || underAny(p, m.Protect) {
			continue // only what a run created here, and never a synced file
		}
		if _, err := fsid.SlotRel(p); err != nil {
			continue // never a file a run made here (inside .git, or not a path)
		}
		// A name no client could use (a backslash, .Git) goes too.
		full, err := fsid.SlotJoin(s.workspace, p)
		if err != nil {
			if !gonePath(err) {
				keep = append(keep, p) // can't tell: try again next sync
			}
			continue
		}
		fi, err := os.Lstat(full)
		switch {
		case err == nil && fi.IsDir():
		case err == nil:
			if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				keep = append(keep, p)
				continue
			}
			deleted++
			dirs = append(dirs, path.Dir(p))
		case !gonePath(err):
			keep = append(keep, p)
		}
	}
	// Whatever couldn't be removed stays pending (and keeps the fingerprint
	// shortcut off) so the next sync tries again.
	s.state.Pending = keep
	removeEmptyDirs(s.workspace, dirs, m.Protect)

	var need []string
	var needBytes int64
	for _, e := range m.Entries {
		cur, ok := s.verify(e.Path)
		if ok && cur.ID == e.ID {
			if cur.Mode != e.Mode && e.Mode != fsid.ModeSymlink && cur.Mode != fsid.ModeSymlink {
				full, _ := fsid.SafeJoin(s.workspace, e.Path) // verify() already checked
				if err := os.Chmod(full, fsid.Perm(e.Mode)); err == nil {
					if fi, err := os.Lstat(full); err == nil {
						if sg, ok := sigOf(fi); ok {
							cur = entryFrom(cur.ID, sg)
						}
					}
				}
			}
			if cur.Mode == e.Mode {
				s.stored[e.Path] = cur
				continue
			}
		}
		need = append(need, e.Path)
		needBytes += e.Size
	}
	sort.Strings(need)
	if err := s.c.Send(protocol.TypeNeed, protocol.Need{Paths: need, Bytes: needBytes}); err != nil {
		return err
	}
	res := protocol.Synced{Deleted: deleted}
	if len(need) > 0 {
		needSet := make(map[string]bool, len(need))
		for _, p := range need {
			needSet[p] = true
		}
		br := &protocol.BlobReader{Next: s.next}
		written, bytes, corrected, err := s.extract(br, needSet, want)
		if err != nil {
			_ = br.Drain()
			_ = s.saveStored()
			return perr(protocol.CodeSyncFailed, "%v", err)
		}
		if err := br.Drain(); err != nil {
			return perr(protocol.CodeSyncFailed, "%v", err)
		}
		res.Written, res.Bytes, res.Corrected = written, bytes, corrected
		if written != len(need) {
			_ = s.saveStored()
			return perr(protocol.CodeSyncFailed, "expected %d files, received %d", len(need), written)
		}
	}
	if err := s.saveStored(); err != nil {
		return err
	}
	if len(res.Corrected) == 0 {
		s.state.Fingerprint = m.Fingerprint
	}
	s.state.LastUsed = time.Now().Unix()
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return err
	}
	return s.c.Send(protocol.TypeSynced, res)
}

// extract applies a gzip'd tar of needed files into the workspace.
func (s *Session) extract(r io.Reader, needSet map[string]bool, want map[string]protocol.Entry) (int, int64, []protocol.Entry, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	written := 0
	var bytes int64
	var corrected []protocol.Entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return written, bytes, corrected, err
		}
		if !needSet[h.Name] {
			return written, bytes, corrected, fmt.Errorf("unexpected path in archive: %q", h.Name)
		}
		full, err := fsid.PrepareParent(s.workspace, h.Name)
		if err != nil {
			return written, bytes, corrected, err
		}
		id, mode, err := writeEntry(full, h, tr)
		if err != nil {
			return written, bytes, corrected, fmt.Errorf("%s: %w", h.Name, err)
		}
		fi, err := os.Lstat(full)
		if err != nil {
			return written, bytes, corrected, err
		}
		sg, _ := sigOf(fi)
		s.stored[h.Name] = entryFrom(id, sg)
		if w := want[h.Name]; w.ID != id || w.Mode != mode {
			corrected = append(corrected, protocol.Entry{Path: h.Name, ID: id, Mode: mode, Size: sg.Size})
		}
		written++
		bytes += h.Size
	}
	return written, bytes, corrected, nil
}

// writeEntry atomically materializes one tar entry at full and returns its ID.
func writeEntry(full string, h *tar.Header, r io.Reader) (string, uint32, error) {
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	// Replace a directory occupying the target path (type change).
	if fi, err := os.Lstat(full); err == nil && fi.IsDir() {
		if err := os.RemoveAll(full); err != nil {
			return "", 0, err
		}
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".spotexec-tmp-%d-%d", os.Getpid(), time.Now().UnixNano()))
	switch h.Typeflag {
	case tar.TypeSymlink:
		if err := os.Symlink(h.Linkname, tmp); err != nil {
			return "", 0, err
		}
		if err := os.Rename(tmp, full); err != nil {
			os.Remove(tmp)
			return "", 0, err
		}
		return fsid.Bytes([]byte(h.Linkname)), fsid.ModeSymlink, nil
	case tar.TypeReg:
		mode := fsid.ModeFile
		if h.Mode&0o111 != 0 {
			mode = fsid.ModeExec
		}
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fsid.Perm(mode))
		if err != nil {
			return "", 0, err
		}
		pr, pw := io.Pipe()
		idc := make(chan string, 1)
		errc := make(chan error, 1)
		go func() {
			id, err := fsid.Reader(pr, h.Size)
			pr.CloseWithError(err)
			idc <- id
			errc <- err
		}()
		_, cerr := io.Copy(io.MultiWriter(f, pw), r)
		pw.CloseWithError(cerr)
		id, herr := <-idc, <-errc
		if err := f.Close(); cerr == nil {
			cerr = err
		}
		if cerr == nil {
			cerr = herr
		}
		if cerr == nil {
			cerr = os.Chmod(tmp, fsid.Perm(mode)) // umask-proof
		}
		if cerr == nil {
			cerr = os.Rename(tmp, full)
		}
		if cerr != nil {
			os.Remove(tmp)
			return "", 0, cerr
		}
		return id, mode, nil
	}
	return "", 0, fmt.Errorf("unsupported tar entry type %q", h.Typeflag)
}

// removeEmptyDirs removes now-empty parent directories (never the root and
// never protected cache dirs), deepest first.
func removeEmptyDirs(root string, dirs []string, protect []string) {
	seen := map[string]bool{}
	var all []string
	for _, d := range dirs {
		for d != "." && d != "/" && d != "" && !seen[d] {
			seen[d] = true
			all = append(all, d)
			d = path.Dir(d)
		}
	}
	sort.Slice(all, func(i, j int) bool { return strings.Count(all[i], "/") > strings.Count(all[j], "/") })
	for _, d := range all {
		if underAny(d, protect) {
			continue
		}
		// Never act through a symlinked ancestor, and only ever rmdir.
		full, err := fsid.SlotJoin(root, d)
		if err != nil {
			continue
		}
		if fi, err := os.Lstat(full); err == nil && fi.IsDir() {
			_ = os.Remove(full) // fails unless empty
		}
	}
}

// underAny reports whether p is one of dirs or inside one.
func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		d = strings.Trim(d, "/")
		if d != "" && (p == d || strings.HasPrefix(p, d+"/")) {
			return true
		}
	}
	return false
}

// pendingAfter is the slot's pending list after a run: earlier entries that
// are still there and still not synced, plus what this run created (or
// could not read).
func (s *Session) pendingAfter(created []protocol.Entry, unverified, more []string) []string {
	set := map[string]bool{}
	for _, p := range s.state.Pending {
		if _, ok := s.stored[p]; ok {
			continue
		}
		if _, err := fsid.SlotRel(p); err != nil {
			continue // never a file a run made here
		}
		full, err := fsid.SlotJoin(s.workspace, p)
		if err != nil {
			if !gonePath(err) {
				set[p] = true // can't tell (permissions): keep it
			}
			continue
		}
		if fi, err := os.Lstat(full); err == nil && fi.IsDir() || err != nil && gonePath(err) {
			continue // removed (or replaced by a directory) by the run itself
		}
		set[p] = true
	}
	for _, c := range created {
		set[c.Path] = true
	}
	for _, p := range more {
		set[p] = true
	}
	for _, p := range unverified {
		if _, ok := s.stored[p]; !ok {
			set[p] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// gonePath: an error meaning the path no longer exists as such (absent, a
// parent replaced by a file, or reached only through a symlink, where the
// walk records the file under its real path).
func gonePath(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, fsid.ErrSymlinkParent)
}

// sweepPending recovers after a helper died during a run: every file in
// the workspace that isn't synced becomes pending (the client keeps the
// ones it ignores).
func (s *Session) sweepPending() {
	// Anything the dead run left running could still be creating files.
	reapWorkspace(s.workspace, 2*time.Second)
	s.stored = nil
	s.loadStored()
	snap := s.takeSnapshot(s.state.SweepSkip)
	var more []string
	for rel := range snap.sig {
		if _, ok := s.stored[rel]; !ok {
			more = append(more, rel)
		}
	}
	s.state.Pending = s.pendingAfter(nil, nil, more)
	s.state.Sweep, s.state.SweepSkip = false, nil
	_ = saveSlotState(s.slotDir, s.state)
}

// dropPending forgets paths that are settled: written back (now synced
// files) or ignored by the client (kept as build outputs).
func (s *Session) dropPending(paths []string) {
	if len(s.state.Pending) == 0 || len(paths) == 0 {
		return
	}
	drop := make(map[string]bool, len(paths))
	for _, p := range paths {
		drop[p] = true
	}
	var keep []string
	for _, p := range s.state.Pending {
		if !drop[p] {
			keep = append(keep, p)
		}
	}
	if len(keep) == len(s.state.Pending) {
		return
	}
	s.state.Pending = keep
	_ = saveSlotState(s.slotDir, s.state)
}

func (s *Session) settle(st protocol.Settle) {
	if s.slotLock == nil {
		return
	}
	s.dropPending(st.Ignored)
}

// sendTar streams the named workspace files as a gzip'd tar blob.
func (s *Session) sendTar(paths []string) error {
	bw := &protocol.BlobWriter{C: s.c}
	zw := gzip.NewWriter(bw)
	tw := tar.NewWriter(zw)
	err := func() error {
		for _, p := range paths {
			full, err := fsid.SafeJoin(s.workspace, p)
			if err != nil {
				return err
			}
			if err := addTar(tw, full, p); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return zw.Close()
	}()
	return bw.CloseWithError(err)
}

func addTar(tw *tar.Writer, full, name string) error {
	fi, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // vanished; the receiver treats it as absent
		}
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(full)
		if err != nil {
			return err
		}
		return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: t, Mode: 0o777, ModTime: fi.ModTime()})
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(b)), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = tw.Write(b)
	return err
}

func (s *Session) fetch(f protocol.Fetch) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before fetch")
	}
	s.loadStored()
	for _, p := range f.Accept {
		if _, err := fsid.CleanRel(p); err != nil {
			return perr(protocol.CodeBadRequest, "%v", err)
		}
		if cur, ok := s.verify(p); ok {
			s.stored[p] = cur
		}
	}
	if len(f.Accept) > 0 {
		s.state.Fingerprint = ""
		_ = saveSlotState(s.slotDir, s.state)
		if err := s.saveStored(); err != nil {
			return err
		}
		// Only once they are recorded as synced files.
		s.dropPending(f.Accept)
	}
	if err := s.c.Send(protocol.TypeFetched, nil); err != nil {
		return err
	}
	return s.sendTar(f.Paths)
}

// pull streams files matching glob patterns (directories recurse). Patterns
// are workspace-relative and may not escape it.
func (s *Session) pull(p protocol.Pull) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before pull")
	}
	var files []string
	for _, pat := range p.Patterns {
		pat = strings.TrimSuffix(strings.TrimPrefix(pat, "./"), "/")
		if err := fsid.CheckPattern(pat); err != nil {
			return perr(protocol.CodeBadRequest, "bad pull pattern %q", pat)
		}
		matches, err := filepath.Glob(filepath.Join(s.workspace, filepath.FromSlash(pat)))
		if err != nil {
			return perr(protocol.CodeBadRequest, "bad pull pattern %q: %v", pat, err)
		}
		for _, m := range matches {
			if rel, err := filepath.Rel(s.workspace, m); err != nil {
				continue
			} else if _, err := fsid.SafeJoin(s.workspace, filepath.ToSlash(rel)); err != nil {
				continue // reached through a symlink: don't walk wherever it points
			}
			_ = filepath.WalkDir(m, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if strings.EqualFold(d.Name(), ".git") {
						return filepath.SkipDir // never pulled (and not a valid path)
					}
					return nil
				}
				rel, err := filepath.Rel(s.workspace, fp)
				if err == nil {
					if _, err := fsid.SafeJoin(s.workspace, filepath.ToSlash(rel)); err == nil {
						files = append(files, filepath.ToSlash(rel))
					}
				}
				return nil
			})
		}
	}
	sort.Strings(files)
	if err := s.c.Send(protocol.TypeFetched, protocol.Need{Paths: files}); err != nil {
		return err
	}
	return s.sendTar(files)
}
