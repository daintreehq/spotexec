package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// WritebackResult summarizes a write-back.
type WritebackResult struct {
	Applied     []string
	Deleted     []string
	Conflicts   []string
	Stashed     []Conflict // one per Conflicts entry: where each remote version went
	Ignored     int
	Refused     []string // paths the runner reported that may not be written
	ConflictDir string
	// BackedUp: local files the write-back deleted or replaced that had no
	// committed copy, saved under BackupDir first.
	BackedUp  []string
	BackupDir string
	// Held: deletions kept back because the run would have deleted the
	// project's own config or most of its files (HeldWhy says which).
	Held    []string
	HeldWhy string
}

// Conflict is a path where your local edit was kept and the remote version
// was stashed instead, or, when With is set, a remote path whose name is
// another remote path's file on this filesystem.
type Conflict struct {
	Path    string `json:"path"`    // worktree-relative
	Local   string `json:"local"`   // your file (absolute)
	Remote  string `json:"remote"`  // the stashed remote version (absolute)
	Deleted bool   `json:"deleted"` // the remote command deleted it (Remote is a marker file)
	// Reason is empty for a local edit made during the run, or
	// ReasonSymlinkParent: a local parent directory is a symlink, so the
	// path can't be written here without leaving the worktree.
	Reason string `json:"reason,omitempty"`
	// With is the path whose file Path opens here, because the filesystem
	// ignores case or Unicode normalisation (src/Dup.txt is src/DUP.txt):
	// a name collision, not a local edit. Local is With's file, kept.
	With string `json:"with,omitempty"`
}

// ReasonSymlinkParent marks a conflict caused by a symlinked local parent.
const ReasonSymlinkParent = "symlink_parent"

// conflict stashes the remote version of p and records the conflict; with
// names the path p collides with, if that is why ("" for a local edit).
func (wb *WritebackResult) conflict(root, p string, want version, rf remoteFile, with string) error {
	stashed, err := stashConflict(wb.ConflictDir, p, want, rf)
	if err != nil {
		return err
	}
	local := p
	if with != "" {
		local = with
	}
	wb.Conflicts = append(wb.Conflicts, p)
	c := Conflict{Path: p, Local: filepath.Join(root, filepath.FromSlash(local)), Remote: stashed, Deleted: !want.exists, With: with}
	if _, err := fsid.SafeJoin(root, p); errors.Is(err, fsid.ErrSymlinkParent) {
		c.Reason = ReasonSymlinkParent
	}
	wb.Stashed = append(wb.Stashed, c)
	return nil
}

// remoteFile is a fetched file version.
type remoteFile struct {
	data []byte
	link string
	mode uint32
	id   string
}

// state of a path: absent, or (id, mode).
type version struct {
	exists bool
	id     string
	mode   uint32
}

func (a version) eq(b version) bool {
	if !a.exists || !b.exists {
		return a.exists == b.exists
	}
	return a.id == b.id && a.mode == b.mode
}

// blocked is a local state that matches nothing (a directory where a file
// is expected, or a path through a symlink): the path becomes a conflict.
var blocked = version{exists: true, id: "!blocked"}

func localVersion(root, rel string) (version, error) {
	full, err := fsid.SafeJoin(root, rel) // never read through a symlinked parent
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return version{}, nil // a parent is now a file: the path is gone
		}
		return blocked, nil
	}
	if fi, err := os.Lstat(full); err == nil && fi.IsDir() {
		return blocked, nil
	}
	id, mode, _, err := fsid.Path(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return version{}, nil // gone, or its parent is now a file
		}
		return version{}, err
	}
	return version{exists: true, id: id, mode: mode}, nil
}

// writeback applies remote mutations with a per-path 3-way check:
//
//	base   = what we uploaded (L_pre)
//	local  = what is on disk now (L_now)
//	remote = what the command produced (R_post)
//
// local == remote: nothing to do. local == base: apply remote. Otherwise
// someone edited the file locally during the run: keep it and stash the
// remote version under <git-dir>/spotexec/conflicts/<run-id>/.
//
// ignored is which created files are ignored here, if already known.
// proceed, when set, is asked once everything is fetched and before anything
// is applied: false applies nothing.
func writeback(ctx context.Context, s *transport.Session, ws *Workspace, m *manifest.Manifest, r protocol.Result, runID string, ignored map[string]bool, proceed func() bool) (WritebackResult, error) {
	var wb WritebackResult
	// Created (non-manifest) files only come back if not ignored locally.
	// The runner's report is not trusted: changed or deleted paths must be
	// files we uploaded, and nothing may land in .git or .spotexec.
	refuse := func(p string) {
		wb.Refused = append(wb.Refused, p)
	}
	var changed, created []protocol.Entry
	var deleted []string
	for _, c := range r.Changed {
		if _, ok := m.ByPath[c.Path]; !ok || reservedPath(c.Path, ws.Git) {
			refuse(c.Path)
			continue
		}
		changed = append(changed, c)
	}
	for _, c := range r.Created {
		if _, err := fsid.CleanRel(c.Path); err != nil || reservedPath(c.Path, ws.Git) {
			refuse(c.Path)
			continue
		}
		created = append(created, c)
	}
	for _, d := range r.Deleted {
		if _, ok := m.ByPath[d]; !ok || reservedPath(d, ws.Git) {
			refuse(d)
			continue
		}
		deleted = append(deleted, d)
	}
	var createdPaths []string
	for _, c := range created {
		createdPaths = append(createdPaths, c.Path)
	}
	if ignored == nil {
		var err error
		if ignored, err = m.CheckIgnored(ctx, createdPaths); err != nil {
			return wb, err
		}
	}
	remote := map[string]version{}
	var fetch, accept []string
	for _, c := range changed {
		remote[c.Path] = version{true, c.ID, c.Mode}
		fetch = append(fetch, c.Path)
	}
	// Created paths beyond a local symlink: git couldn't judge whether they
	// are ignored, so they are asked again if a deletion clears the way.
	unjudged := map[string]bool{}
	for _, c := range created {
		if ignored[c.Path] {
			wb.Ignored++
			continue
		}
		if _, err := fsid.SafeJoin(ws.Root, c.Path); errors.Is(err, fsid.ErrSymlinkParent) {
			unjudged[c.Path] = true
		}
		remote[c.Path] = version{true, c.ID, c.Mode}
		fetch = append(fetch, c.Path)
		accept = append(accept, c.Path)
	}
	for _, d := range deleted {
		remote[d] = version{}
	}
	if len(remote) == 0 {
		return wb, nil
	}
	sort.Strings(fetch)

	files := map[string]remoteFile{}
	if err := s.Conn.Send(protocol.TypeFetch, protocol.Fetch{Paths: fetch, Accept: accept}); err != nil {
		return wb, err
	}
	if _, err := s.Expect(ctx, protocol.TypeFetched); err != nil {
		return wb, err
	}
	if err := readTar(s.Blob(ctx), func(h *tar.Header, rd io.Reader) error {
		rf := remoteFile{}
		switch h.Typeflag {
		case tar.TypeSymlink:
			rf.link, rf.mode, rf.id = h.Linkname, fsid.ModeSymlink, fsid.Bytes([]byte(h.Linkname))
		case tar.TypeReg:
			b, err := io.ReadAll(rd)
			if err != nil {
				return err
			}
			rf.data, rf.id, rf.mode = b, fsid.Bytes(b), fsid.ModeFile
			if h.Mode&0o111 != 0 {
				rf.mode = fsid.ModeExec
			}
		default:
			return nil
		}
		if v, ok := remote[h.Name]; !ok || !v.exists {
			return nil // not something we asked for
		}
		files[h.Name] = rf
		return nil
	}); err != nil {
		return wb, err
	}
	if proceed != nil && !proceed() {
		return wb, nil
	}

	err := applyWriteback(ctx, ws, m, remote, files, unjudged, runID, &wb)
	return wb, err
}

// applyWriteback applies the fetched remote versions (remote: what the
// command left at each path; files: the bytes fetched for those that exist)
// to the local worktree, under the write-back lock. unjudged holds created
// paths git couldn't judge for ignores (beyond a local symlink).
func applyWriteback(ctx context.Context, ws *Workspace, m *manifest.Manifest, remote map[string]version, files map[string]remoteFile, unjudged map[string]bool, runID string, wb *WritebackResult) error {
	// Serialize cooperating spotexec write-backs into this worktree.
	lockDir := conflictBase(ws)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(ctx, filepath.Join(lockDir, "writeback.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	wb.ConflictDir = filepath.Join(lockDir, "conflicts", runID)
	synced := len(m.ByPath)

	// save backs up the local version of p, about to be deleted or replaced,
	// unless git has it committed.
	committed := &committedFiles{ctx: ctx, m: m}
	save := func(p string, local version) error {
		if !local.exists || committed.has(p, local) {
			return nil
		}
		dir := filepath.Join(lockDir, "backup", runID)
		if _, err := backupLocal(ws.Root, dir, p, local); err != nil {
			return err
		}
		wb.BackupDir = dir
		wb.BackedUp = append(wb.BackedUp, p)
		return nil
	}

	paths := make([]string, 0, len(remote))
	for p := range remote {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var removedDirs []string
	handled := map[string]bool{}

	// Case-only renames (foo -> Foo) on a case-insensitive filesystem (the
	// macOS default): both names resolve to one file, so applying "create Foo"
	// and "delete foo" independently would delete it. Rename in place instead.
	for _, del := range paths {
		if remote[del].exists {
			continue
		}
		for _, q := range paths {
			if q == del || !remote[q].exists || !strings.EqualFold(q, del) {
				continue
			}
			fd, err1 := fsid.SafeJoin(ws.Root, del)
			fq, err2 := fsid.SafeJoin(ws.Root, q)
			if err1 != nil || err2 != nil {
				continue
			}
			di, e1 := os.Lstat(fd)
			qi, e2 := os.Lstat(fq)
			if e1 != nil || e2 != nil || !os.SameFile(di, qi) {
				continue // case-sensitive filesystem: normal handling is right
			}
			handled[del], handled[q] = true, true
			rf, ok := files[q]
			if !ok {
				continue
			}
			base := version{}
			if e, ok := m.ByPath[del]; ok {
				base = version{true, e.ID, e.Mode}
			}
			local, err := localVersion(ws.Root, del)
			if err != nil {
				return err
			}
			want := version{true, rf.id, rf.mode}
			if !local.eq(base) && !local.eq(want) {
				if err := wb.conflict(ws.Root, q, want, rf, ""); err != nil {
					return err
				}
				break
			}
			if !local.eq(want) {
				err := save(del, local)
				if errors.Is(err, errChangedUnderUs) {
					if err := wb.conflict(ws.Root, q, want, rf, ""); err != nil {
						return err
					}
					break
				}
				if err != nil {
					return fmt.Errorf("backing up %s: %w", del, err)
				}
			}
			if err := os.Rename(fd, fq); err != nil {
				return fmt.Errorf("renaming %s to %s: %w", del, q, err)
			}
			if !local.eq(want) {
				err := writeLocal(ws.Root, q, rf, &local)
				if errors.Is(err, errChangedUnderUs) {
					if err := wb.conflict(ws.Root, q, want, rf, ""); err != nil {
						return err
					}
					break
				}
				if err != nil {
					return fmt.Errorf("%s: %w", q, err)
				}
			}
			wb.Applied = append(wb.Applied, q)
			wb.Deleted = append(wb.Deleted, del)
			m.Update(protocol.Entry{Path: q, ID: rf.id, Mode: rf.mode, Size: int64(len(rf.data))})
			break
		}
	}

	// Deletions first, deepest first, then the emptied directories are
	// pruned, then writes: a directory replaced by a file (or the reverse)
	// is applied in an order that works.
	apply := func(p string) error {
		if handled[p] {
			return nil
		}
		want := remote[p]
		rf, fetched := files[p]
		if want.exists {
			if !fetched {
				return nil // vanished remotely between diff and fetch
			}
			want = version{true, rf.id, rf.mode} // trust bytes actually received
		}
		base := version{}
		if e, ok := m.ByPath[p]; ok {
			base = version{true, e.ID, e.Mode}
		}
		local, err := localVersion(ws.Root, p)
		if err != nil {
			return err
		}
		switch {
		case local.eq(want):
			return nil
		case local.eq(base):
			// Whatever is lost here and isn't committed is saved first.
			if err := save(p, local); errors.Is(err, errChangedUnderUs) {
				return wb.conflict(ws.Root, p, want, rf, "")
			} else if err != nil {
				return fmt.Errorf("backing up %s: %w", p, err)
			}
			if want.exists {
				err := writeLocal(ws.Root, p, rf, &local)
				// Changed under us, or the way is blocked (a file where a
				// directory is needed, kept because of a local edit): keep
				// the local side, stash the remote one.
				if errors.Is(err, errChangedUnderUs) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EEXIST) || errors.Is(err, fsid.ErrSymlinkParent) {
					if err := wb.conflict(ws.Root, p, want, rf, ""); err != nil {
						return err
					}
					return nil
				}
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				wb.Applied = append(wb.Applied, p)
				m.Update(protocol.Entry{Path: p, ID: rf.id, Mode: rf.mode, Size: int64(len(rf.data))})
			} else {
				full, err := fsid.SafeJoin(ws.Root, p)
				if errors.Is(err, fsid.ErrSymlinkParent) {
					return wb.conflict(ws.Root, p, want, rf, "") // a symlink appeared above it
				}
				if err != nil {
					return err
				}
				if now, err := localVersion(ws.Root, p); err != nil || !now.eq(local) {
					return wb.conflict(ws.Root, p, want, rf, "")
				}
				if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				wb.Deleted = append(wb.Deleted, p)
				removedDirs = append(removedDirs, path.Dir(p))
			}
		default:
			with := ""
			if local.exists && local != blocked {
				with = collidesWith(ws.Root, p, remote, m)
			}
			if err := wb.conflict(ws.Root, p, want, rf, with); err != nil {
				return err
			}
		}
		return nil
	}
	var dels, writes []string
	for _, p := range paths {
		if handled[p] {
			continue
		}
		if remote[p].exists {
			writes = append(writes, p)
		} else {
			dels = append(dels, p)
		}
	}
	sort.SliceStable(dels, func(i, j int) bool { return strings.Count(dels[i], "/") > strings.Count(dels[j], "/") })
	if dels, err = holdDeletions(ws.Root, m, dels, synced, wb); err != nil {
		return err
	}
	for _, p := range dels {
		if err := apply(p); err != nil {
			return err
		}
	}
	pruneEmptyDirs(ws.Root, removedDirs)
	removedDirs = nil
	for _, p := range nowIgnored(ctx, m, ws.Root, writes, unjudged) {
		handled[p] = true
		wb.Ignored++
	}
	for _, p := range writes {
		if err := apply(p); err != nil {
			return err
		}
	}
	pruneEmptyDirs(ws.Root, removedDirs)
	return nil
}

// holdDeletions keeps back the deletions a run should not make on its own:
// the project's own root .spotexec.toml or .gitignore, or, when the run
// deleted most of the synced files, all of them. Only deletions that would
// apply count (the local file still matches what was uploaded). It returns
// the deletions to go ahead with.
func holdDeletions(root string, m *manifest.Manifest, dels []string, synced int, wb *WritebackResult) ([]string, error) {
	var live, protected []string
	for _, p := range dels {
		e, ok := m.ByPath[p]
		if !ok {
			continue
		}
		local, err := localVersion(root, p)
		if err != nil {
			return nil, err
		}
		if !local.exists || !local.eq(version{true, e.ID, e.Mode}) {
			continue
		}
		live = append(live, p)
		if protectedPath(p) {
			protected = append(protected, p)
		}
	}
	hold := protected
	switch {
	case massDeletion(len(live), synced):
		hold = live
		wb.HeldWhy = fmt.Sprintf("the run deleted %d of the %d synced files", len(live), synced)
	case len(protected) > 0:
		wb.HeldWhy = "the run deleted the project's own " + strings.Join(protected, " and ")
	default:
		return dels, nil
	}
	held := map[string]bool{}
	for _, p := range hold {
		held[p] = true
	}
	wb.Held = append(wb.Held, hold...)
	sort.Strings(wb.Held)
	var keep []string
	for _, p := range dels {
		if !held[p] {
			keep = append(keep, p)
		}
	}
	return keep, nil
}

// nowIgnored asks git again about the unjudged paths (created beyond a local
// symlink) that a deletion has since cleared the way to, as when a symlinked
// dist/ is replaced by a real one: ignored outputs then stay remote as usual.
// If git still can't answer, they are written back.
func nowIgnored(ctx context.Context, m *manifest.Manifest, root string, paths []string, unjudged map[string]bool) []string {
	var recheck, ignored []string
	for _, p := range paths {
		if _, err := fsid.SafeJoin(root, p); unjudged[p] && err == nil {
			recheck = append(recheck, p)
		}
	}
	if len(recheck) == 0 {
		return nil
	}
	ign, err := m.CheckIgnored(ctx, recheck)
	if err != nil {
		return nil
	}
	for _, p := range recheck {
		if ign[p] {
			ignored = append(ignored, p)
		}
	}
	return ignored
}

// collidesWith is the other path the run knows whose file p opens here,
// when this filesystem ignores case or Unicode normalisation in names
// (src/Dup.txt opens src/DUP.txt, src/n\u00e9.txt opens src/ne\u0301.txt):
// two remote names for one local file, not a local edit. "" when p's file
// has p's own name, or a name the run doesn't know.
func collidesWith(root, p string, remote map[string]version, m *manifest.Manifest) string {
	q, ok := onDiskName(root, p)
	if !ok || q == p {
		return ""
	}
	if v, ok := remote[q]; ok && v.exists {
		return q
	}
	if _, ok := m.ByPath[q]; ok {
		return q
	}
	return ""
}

// onDiskName is the name rel's file has under root, component by component:
// rel itself where that name is listed, otherwise the listed entry that is
// the same file. false when a component is missing or can't be read.
func onDiskName(root, rel string) (string, bool) {
	dir := root
	var out []string
	for _, part := range strings.Split(rel, "/") {
		fi, err := os.Lstat(filepath.Join(dir, part))
		if err != nil {
			return "", false
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return "", false
		}
		name := ""
		for _, e := range ents {
			if e.Name() == part {
				name = part
				break
			}
		}
		for _, e := range ents {
			if name != "" {
				break
			}
			if ei, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(ei, fi) {
				name = e.Name()
			}
		}
		if name == "" {
			return "", false
		}
		out = append(out, name)
		dir = filepath.Join(dir, name)
	}
	return strings.Join(out, "/"), true
}

func conflictBase(ws *Workspace) string {
	if ws.GitDir != "" {
		return filepath.Join(ws.GitDir, "spotexec")
	}
	return filepath.Join(ws.Root, ".spotexec")
}

// stashConflict saves the remote version of p under dir and returns where it
// went (a ".deleted" marker when the remote command deleted it).
func stashConflict(dir, p string, want version, rf remoteFile) (string, error) {
	if !want.exists {
		mark := []byte("deleted by the remote command\n")
		full, err := fsid.SafeJoin(dir, p+".deleted")
		if err == nil {
			if err = os.MkdirAll(filepath.Dir(full), 0o700); err == nil {
				err = os.WriteFile(full, mark, 0o600)
			}
		}
		if err != nil && stashCollision(err) {
			// Something in the stash is in the way: a fresh flattened name.
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			f, err := os.CreateTemp(dir, strings.ReplaceAll(p, "/", "__")+".deleted-*")
			if err != nil {
				return "", err
			}
			_, err = f.Write(mark)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return f.Name(), err
		}
		return full, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	err := writeLocal(dir, p, rf, nil)
	if err != nil && stashCollision(err) {
		// A directory/file collision inside the stash itself (a path that
		// was a directory and is now a file): keep it at the top of the
		// stash under a flattened name nothing else uses.
		base := strings.ReplaceAll(p, "/", "__") + ".remote"
		name := base
		for i := 2; ; i++ {
			if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, fs.ErrNotExist) {
				break
			}
			name = fmt.Sprintf("%s.%d", base, i)
		}
		return filepath.Join(dir, name), writeLocal(dir, name, rf, nil)
	}
	return filepath.Join(dir, filepath.FromSlash(p)), err
}

func stashCollision(err error) bool {
	return errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EEXIST) || errors.Is(err, fsid.ErrSymlinkParent)
}

var errChangedUnderUs = errors.New("file changed during write-back")

// writeLocal atomically replaces root/p with rf. When expect is set, the
// current file is re-checked immediately before the rename (after the new
// content is staged), shrinking the window in which an editor's save could be
// lost to the rename syscall itself.
func writeLocal(root, p string, rf remoteFile, expect *version) error {
	full, err := fsid.SafeJoin(root, p)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".spotexec-tmp-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if rf.mode == fsid.ModeSymlink {
		if err := os.Symlink(rf.link, tmp); err != nil {
			return err
		}
	} else {
		// Over an existing file, staged private (it may be a 0600 secret)
		// and given that file's bits just before the rename; a new file
		// gets 0644/0755 as the umask allows.
		stage := fsid.Perm(rf.mode)
		if _, existing := replacementPerm(full, rf.mode); existing {
			stage = 0o600
		}
		if err := os.WriteFile(tmp, rf.data, stage); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if stagedHook != nil {
		stagedHook()
	}
	if fi, err := os.Lstat(full); err == nil && fi.IsDir() {
		os.Remove(tmp)
		return fmt.Errorf("a directory exists at %s: %w", p, syscall.EISDIR)
	}
	if expect != nil {
		if now, err := localVersion(root, p); err != nil || !now.eq(*expect) {
			os.Remove(tmp)
			return errChangedUnderUs
		}
	}
	if rf.mode != fsid.ModeSymlink {
		// Read now, not before staging: a chmod made meanwhile (tightening
		// a secret) must not be undone.
		if perm, existing := replacementPerm(full, rf.mode); existing {
			if err := os.Chmod(tmp, perm); err != nil {
				os.Remove(tmp)
				return err
			}
		}
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// stagedHook runs after writeLocal stages a file (tests).
var stagedHook func()

// replacementPerm is the permission for a file written over full. An
// existing regular file keeps its own bits (a 0600 .npmrc stays 0600); only
// an executable-bit change made remotely is carried over, for whoever may
// read the file. existing is false when there is no regular file there: a
// new file gets the usual 0644/0755, which the umask narrows.
func replacementPerm(full string, mode uint32) (perm os.FileMode, existing bool) {
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return fsid.Perm(mode), false
	}
	perm = fi.Mode().Perm()
	switch {
	case mode == fsid.ModeExec && perm&0o111 == 0:
		perm |= (perm & 0o444) >> 2 // +x wherever it is readable
	case mode != fsid.ModeExec:
		perm &^= 0o111
	}
	return perm, true
}

func pruneEmptyDirs(root string, dirs []string) {
	seen := map[string]bool{}
	var all []string
	for _, d := range dirs {
		for d != "." && d != "" && d != "/" && !seen[d] {
			seen[d] = true
			all = append(all, d)
			d = path.Dir(d)
		}
	}
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	for _, d := range all {
		full, err := fsid.SafeJoin(root, d)
		if err != nil {
			continue
		}
		if fi, err := os.Lstat(full); err == nil && fi.IsDir() {
			_ = os.Remove(full) // only succeeds when empty
		}
	}
}

func readTar(r io.Reader, fn func(*tar.Header, io.Reader) error) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if _, err := fsid.CleanRel(h.Name); err != nil {
			continue // never a name we asked for: skip it, don't abort
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
	_, _ = io.Copy(io.Discard, r)
	return nil
}

// lockFile takes the worktree's write-back lock, waiting no longer than ctx
// allows (a suspended or stuck sx holding it must not block every later
// run, with its remote lease, forever). The holder records who it is in the
// file, so a wait that gives up can say which process holds the lock.
func lockFile(ctx context.Context, p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	delay := 10 * time.Millisecond
	for {
		if ctx.Err() != nil {
			f.Close()
			return nil, fmt.Errorf("gave up waiting for this worktree's write-back lock (%s), held by %s; if that sx is suspended (Ctrl-Z), resume it with `fg` or end it", p, lockOwner(p))
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("gave up after %s waiting for this worktree's write-back lock (%s), held by %s; if that sx is suspended (Ctrl-Z), resume it with `fg` or end it", time.Since(start).Round(time.Second), p, lockOwner(p))
		case <-time.After(delay):
		}
		delay = min(delay*2, 250*time.Millisecond)
	}
	// The record stays after release (until the next holder overwrites it):
	// clearing it first would leave nothing to report if this process were
	// suspended in between.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(lockHolder()+"\n"), 0)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// lockOwner reads who holds the lock at p.
func lockOwner(p string) string {
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b))
	}
	return "another process"
}

// lockHolder describes this process for a lock it holds.
func lockHolder() string {
	args := append([]string{filepath.Base(os.Args[0])}, os.Args[1:]...)
	cmd := strings.Join(args, " ")
	if len(cmd) > 200 {
		cmd = cmd[:200] + "…"
	}
	return fmt.Sprintf("pid %d (%s), since %s", os.Getpid(), cmd, time.Now().Format("15:04:05"))
}

// PullArtifacts fetches files matching patterns from the slot workspace and
// writes them under root (overwriting: these are gitignored outputs).
//
// Source files are never touched: a pulled path that Git tracks, or that Git
// would not ignore, is skipped (write-back owns those, with its conflict
// checks).
//
// ws is the worktree (for the write-back lock); root is where files go (the
// worktree root, or --dest).
func PullArtifacts(ctx context.Context, s *transport.Session, ws *Workspace, root string, patterns []string) (int, error) {
	if err := s.Conn.Send(protocol.TypePull, protocol.Pull{Patterns: patterns}); err != nil {
		return 0, err
	}
	env, err := s.Expect(ctx, protocol.TypeFetched)
	if err != nil {
		return 0, err
	}
	var listed protocol.Need
	if err := env.Unmarshal(&listed); err != nil {
		return 0, fmt.Errorf("pull: bad listing from runner: %w", err)
	}
	// Only paths the runner listed, that match what was asked for, and that
	// are not reserved, may be written; the rest of the stream is ignored.
	allowed := map[string]bool{}
	var candidates []string
	for _, p := range listed.Paths {
		if _, err := fsid.CleanRel(p); err != nil || reservedPath(p, ws.Git) || !pullMatches(patterns, p) {
			continue
		}
		allowed[p] = true
		candidates = append(candidates, p)
	}
	// Judge each path by where it will land: under --dest inside the
	// worktree, that's dest/<path>; outside the worktree, nothing is source.
	// (root is resolved like ws.Root, so relative and symlinked spellings of
	// the same directory compare equal.)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = resolveExisting(root)
	if g := ws.GitDir; g != "" {
		if rg := resolveExisting(g); root == rg || strings.HasPrefix(root, rg+string(filepath.Separator)) {
			return 0, fmt.Errorf("pull: refusing to write into the git directory")
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(root), "/") {
		if strings.EqualFold(seg, ".git") {
			return 0, fmt.Errorf("pull: refusing to write into a .git directory")
		}
	}
	protected := map[string]bool{}
	rel, relErr := filepath.Rel(ws.Root, root)
	if relErr != nil {
		return 0, fmt.Errorf("pull: can't place %s relative to the worktree: %w", root, relErr)
	}
	if rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && reservedPath(filepath.ToSlash(rel), ws.Git) {
		return 0, fmt.Errorf("pull: refusing to write into %s", rel)
	}
	// The git directories of this worktree (linked worktrees keep hooks in
	// the common one): nothing may land inside them.
	var gitDirs []string
	for _, arg := range []string{"--git-dir", "--git-common-dir"} {
		if out, err := exec.CommandContext(ctx, "git", "-C", ws.Root, "rev-parse", arg).Output(); err == nil {
			d := strings.TrimSpace(string(out))
			if !filepath.IsAbs(d) {
				d = filepath.Join(ws.Root, d)
			}
			gitDirs = append(gitDirs, resolveExisting(d))
		}
	}
	under := func(p, dir string) bool {
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			p, dir = strings.ToLower(p), strings.ToLower(dir)
		}
		return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
	}
	for _, g := range gitDirs {
		if under(root, g) {
			return 0, fmt.Errorf("pull: refusing to write into the git directory")
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, err
	}
	// Each path is judged where it lands: inside this worktree against its
	// git; anywhere else against whatever repository is there (a sibling
	// worktree's tracked files deserve the same protection).
	var inWS, outWS []string
	landing := map[string]string{}
	for _, p := range candidates {
		full := filepath.Join(root, filepath.FromSlash(p))
		if lr, err := filepath.Rel(ws.Root, full); err == nil && lr != ".." && !strings.HasPrefix(lr, "../") {
			landing[p] = filepath.ToSlash(lr)
			inWS = append(inWS, landing[p])
		} else {
			outWS = append(outWS, p)
		}
	}
	if len(inWS) > 0 {
		prot, err := protectedPaths(ctx, ws.Root, inWS)
		if err != nil {
			return 0, err
		}
		for p, l := range landing {
			// Judged where it lands too: a --dest above the worktree
			// must not reach its spotexec state (conflicts, backups).
			protected[p] = prot[l] || reservedPath(l, ws.Git)
		}
	}
	// Outside this worktree, anything inside some git repository (a sibling
	// worktree, a bare repo) is someone's source: left alone.
	inRepo := map[string]bool{}
	for _, p := range outWS {
		dir := resolveExisting(filepath.Dir(filepath.Join(root, filepath.FromSlash(p))))
		for !dirExists(dir) && filepath.Dir(dir) != dir {
			dir = filepath.Dir(dir)
		}
		v, ok := inRepo[dir]
		if !ok {
			err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-dir").Run()
			v = err == nil
			inRepo[dir] = v
		}
		protected[p] = v
	}
	for _, p := range candidates {
		full := resolveExisting(filepath.Join(root, filepath.FromSlash(p)))
		for _, g := range gitDirs {
			if under(full, g) {
				protected[p] = true // an in-tree git dir (a gitfile pointing inside)
			}
		}
	}
	_ = rel
	// Serialise with write-back into the same worktree.
	lockDir := conflictBase(ws)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return 0, err
	}
	unlock, err := lockFile(ctx, filepath.Join(lockDir, "writeback.lock"))
	if err != nil {
		return 0, err
	}
	defer unlock()
	var skipped []string
	defer func() {
		if len(skipped) > 0 {
			fmt.Fprintf(os.Stderr, "spotexec: pull skipped %d source file(s) (tracked or not gitignored): %s\n", len(skipped), strings.Join(firstN(skipped, 5), ", "))
		}
	}()
	n := 0
	err = readTar(s.Blob(ctx), func(h *tar.Header, rd io.Reader) error {
		if !allowed[h.Name] {
			return nil
		}
		if protected[h.Name] {
			skipped = append(skipped, h.Name)
			return nil
		}
		rf := remoteFile{}
		switch h.Typeflag {
		case tar.TypeSymlink:
			rf.link, rf.mode = h.Linkname, fsid.ModeSymlink
		case tar.TypeReg:
			b, err := io.ReadAll(rd)
			if err != nil {
				return err
			}
			rf.data, rf.mode = b, fsid.ModeFile
			if h.Mode&0o111 != 0 {
				rf.mode = fsid.ModeExec
			}
		default:
			return nil
		}
		if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(h.Name))); err == nil && fi.IsDir() {
			return nil
		}
		n++
		return writeLocal(root, h.Name, rf, nil)
	})
	return n, err
}

// protectedPaths returns the paths Git tracks or would not ignore. Outside a
// git worktree nothing is tracked, so pulled outputs (already limited to the
// requested patterns) may be written. If git can't answer for a git
// worktree (for example safe.directory), it fails closed.
func protectedPaths(ctx context.Context, root string, paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out, nil
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		if _, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-dir").Output(); err != nil {
			return out, nil // not a git worktree: nothing tracked to protect
		}
	}
	if _, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-dir").Output(); err != nil {
		return nil, fmt.Errorf("pull: git can't read this worktree (%v); refusing to write", err)
	}
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	ign := map[string]bool{}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "--no-index", "-z", "--stdin")
	cmd.Stdin = bytes.NewReader(in.Bytes())
	ib, ierr := cmd.Output()
	var ee *exec.ExitError
	if ierr != nil && !(errors.As(ierr, &ee) && ee.ExitCode() == 1) { // 1 = none ignored
		for _, p := range paths {
			out[p] = true
		}
		return out, nil
	}
	for _, p := range bytes.Split(ib, []byte{0}) {
		if len(p) > 0 {
			ign[string(p)] = true
		}
	}
	tracked := map[string]bool{}
	b, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		for _, p := range paths { // cannot tell: protect everything
			out[p] = true
		}
		return out, nil
	}
	// macOS folds case: dist/readme.md would overwrite tracked dist/README.md.
	fold := func(p string) string {
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			return strings.ToLower(p)
		}
		return p
	}
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			tracked[fold(string(p))] = true
		}
	}
	for _, p := range paths {
		if tracked[fold(p)] || !ign[p] {
			out[p] = true
		}
	}
	return out, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// resolveExisting resolves symlinks in the deepest existing ancestor of p
// and re-joins the rest (EvalSymlinks fails on paths that don't exist yet).
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
