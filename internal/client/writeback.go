package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"unicode/utf8"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
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
	// OutsideLinks are the symlinks write-back created or retargeted that
	// point outside the worktree: an absolute target, or one that climbs
	// above the root. They are applied, and reported.
	OutsideLinks []Link
	// HeldNew: new files left in the slot because they were over the
	// write-back limit (hold mode). Neither fetched nor accepted.
	HeldNew      []protocol.Entry
	HeldNewBytes int64
}

// Link is a symlink in the worktree and its target, as written.
type Link struct {
	Path   string `json:"path"`   // worktree-relative
	Target string `json:"target"` // the link's contents
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
//
// limits are checked before anything is fetched: over them, rep gets a
// LARGE_WRITEBACK warning, or (hold mode, when the new files alone are over)
// a WRITEBACK_HELD one, and the new files stay in the slot.
func writeback(ctx context.Context, fetchFiles fetchFunc, ws *Workspace, m *manifest.Manifest, r protocol.Result, runID string, ignored map[string]bool, proceed func() bool, limits WritebackLimits, rep ui.Reporter) (WritebackResult, error) {
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
	// .git directories the run made below the root never come back. One
	// whose directory is ignored here stays in the slot, like any build
	// output; the rest are refused, and the next sync removes them.
	if len(r.GitDirs) > 0 {
		var dirs, probes []string
		for _, p := range r.GitDirs {
			if probe, ok := gitDirProbe(ws, m, p); ok {
				dirs, probes = append(dirs, p), append(probes, probe)
			} else {
				refuse(p)
			}
		}
		ign, err := m.CheckIgnored(ctx, probes)
		for i, p := range dirs {
			if err != nil || !ign[probes[i]] {
				refuse(p)
			}
		}
	}
	var fresh []protocol.Entry // created and not ignored: new files coming back
	for _, c := range created {
		if ignored[c.Path] {
			wb.Ignored++
			continue
		}
		fresh = append(fresh, c)
	}
	// The new side of a case-only rename (foo -> Foo) is never held: on a
	// case-insensitive filesystem it is the same file as the deleted one,
	// renamed in place below.
	gone := map[string]bool{}
	for _, d := range deleted {
		gone[strings.ToLower(d)] = true
	}
	var renamed, holdable []protocol.Entry
	for _, c := range fresh {
		if gone[strings.ToLower(c.Path)] {
			renamed = append(renamed, c)
		} else {
			holdable = append(holdable, c)
		}
	}
	if n, b := entryTotals(holdable); limits.Hold && limits.Over(n, b) {
		wb.HeldNew, wb.HeldNewBytes, fresh = holdable, b, renamed
		if rep != nil {
			largest := largestPaths(wb.HeldNew, 3)
			rep.Warn("WRITEBACK_HELD", "left %s (%s) in the slot: over the write-back limit of %s. Largest: %s. Bring them back with `spotexec pull --held` before the next run syncs into this slot, which removes them; add outputs to .gitignore", files(n), ui.Bytes(b), limits, describeLargest(largest))
			rep.Event("writeback_held", map[string]any{"files": n, "bytes": b, "largest": largest, "run_id": runID})
		}
	}
	if n, b := entryTotals(append(append([]protocol.Entry(nil), changed...), fresh...)); limits.Over(n, b) && rep != nil {
		largest := largestPaths(append(append([]protocol.Entry(nil), changed...), fresh...), 3)
		hint := ""
		if nf, bf := entryTotals(fresh); limits.Over(nf, bf) {
			hint = `; if they are outputs, add them to .gitignore, or set writeback_over = "hold" in [sync] (or --writeback-over hold) to leave them in the slot`
		}
		rep.Warn("LARGE_WRITEBACK", "bringing back %s (%s: %d new, %d changed), over the write-back limit of %s. Largest: %s%s", files(n), ui.Bytes(b), len(fresh), len(changed), limits, describeLargest(largest), hint)
		rep.Event("large_writeback", map[string]any{"files": n, "bytes": b, "new_files": len(fresh), "changed_files": len(changed), "largest": largest})
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
	for _, c := range fresh {
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
	blob, err := fetchFiles(protocol.Fetch{Paths: fetch, Accept: accept})
	if err != nil {
		return wb, err
	}
	if err := readTar(blob, func(h *tar.Header, rd io.Reader) error {
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

	err = applyWriteback(ctx, ws, m, remote, files, unjudged, runID, &wb)
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
	wb.ConflictDir = filepath.Join(keepBase(ws), "conflicts", runID)
	synced := len(m.ByPath)

	// save backs up the local version of p, about to be deleted or replaced,
	// unless git has it committed.
	committed := &committedFiles{ctx: ctx, m: m}
	save := func(p string, local version) error {
		if !local.exists || committed.has(p, local) {
			return nil
		}
		dir := filepath.Join(keepBase(ws), "backup", runID)
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
	// Symlinks written into the worktree, judged once every write is done
	// (a link may lead out through another link applied after it).
	var links []Link
	appliedLink := func(p string, rf remoteFile) {
		if rf.mode == fsid.ModeSymlink {
			links = append(links, Link{Path: p, Target: rf.link})
		}
	}
	defer func() { wb.OutsideLinks = outsideLinks(ws.Root, links) }()

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
			appliedLink(q, rf)
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
			// A new name already holding these bytes may be another
			// remote path's file: still two names that can't both exist.
			if !base.exists && local.exists {
				if with := collidesWith(ws.Root, p, remote, m, wb.Applied); with != "" {
					return wb.conflict(ws.Root, p, want, rf, with)
				}
			}
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
				appliedLink(p, rf)
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
				with = collidesWith(ws.Root, p, remote, m, wb.Applied)
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
	// A deletion loses nothing when git has the file committed or the run
	// wrote the same content back under another path (a move).
	cameBack := map[string]bool{}
	for p, rf := range files {
		if !unjudged[p] { // may yet turn out ignored, and stay remote
			cameBack[rf.id] = true
		}
	}
	recoverable := func(p string, local version) bool {
		return cameBack[local.id] || committed.has(p, local)
	}
	if dels, err = holdDeletions(ws.Root, m, dels, synced, recoverable, wb); err != nil {
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
// deleted most of the synced files, all of them that recoverable can't give
// back (so a restructuring move, or deleting committed files, applies).
// Only deletions that would apply count (the local file still matches what
// was uploaded). It returns the deletions to go ahead with.
func holdDeletions(root string, m *manifest.Manifest, dels []string, synced int, recoverable func(string, version) bool, wb *WritebackResult) ([]string, error) {
	var lost, protected []string
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
		if protectedPath(p) {
			protected = append(protected, p)
		}
		if !recoverable(p, local) {
			lost = append(lost, p)
		}
	}
	hold := protected
	switch {
	case massDeletion(len(lost), synced):
		hold = append(append([]string{}, lost...), protected...)
		wb.HeldWhy = fmt.Sprintf("the run deleted %d of the %d synced files with no committed or moved copy", len(lost), synced)
	case len(protected) > 0:
		wb.HeldWhy = "the run deleted the project's own " + strings.Join(protected, " and ")
	default:
		return dels, nil
	}
	held := map[string]bool{}
	for _, p := range hold {
		if !held[p] { // a protected file may be lost too
			held[p] = true
			wb.Held = append(wb.Held, p)
		}
	}
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
// is no other path the run knows.
func collidesWith(root, p string, remote map[string]version, m *manifest.Manifest, applied []string) string {
	q, ok := onDiskName(root, p)
	if ok && q != p {
		if v, ok := remote[q]; ok && v.exists {
			return q
		}
		if _, ok := m.ByPath[q]; ok {
			return q
		}
	}
	// A path this run wrote may have landed under another spelling of a
	// parent directory (d/A written into an existing D/ is D/A, and src/a
	// may be SRC/a's file): match those by identity, and then by directory
	// entry, since a hard link is the same file under a name of its own.
	if !ok {
		return ""
	}
	full, err := fsid.SafeJoin(root, p)
	if err != nil {
		return ""
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return ""
	}
	for _, r := range applied {
		if r == p {
			continue
		}
		fr, err := fsid.SafeJoin(root, r)
		if err != nil {
			continue
		}
		if ri, err := os.Lstat(fr); err != nil || !os.SameFile(ri, fi) {
			continue
		}
		if rq, ok := onDiskName(root, r); ok && rq == q {
			return r
		}
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
		var same []string
		for _, e := range ents {
			if e.Name() == part {
				name = part
				break
			}
			if ei, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(ei, fi) {
				same = append(same, e.Name())
			}
		}
		if name == "" && len(same) > 1 {
			// Hard links: only the entry differing from part in case is
			// the one this name folds to; otherwise it can't be told.
			var folded []string
			for _, n := range same {
				if strings.EqualFold(n, part) {
					folded = append(folded, n)
				}
			}
			same = folded
		}
		if name == "" && len(same) == 1 {
			name = same[0]
		}
		if name == "" {
			return "", false
		}
		out = append(out, name)
		dir = filepath.Join(dir, name)
	}
	return strings.Join(out, "/"), true
}

// outsideLinks returns the links that point outside root: an absolute
// target, a relative one that climbs above root from the link's directory,
// or one that leads outside it on disk through another symlink (even when
// its final target doesn't exist yet).
func outsideLinks(root string, links []Link) []Link {
	var out []Link
	realRoot := resolveExisting(root)
	rootInfo, rootErr := os.Stat(realRoot)
	for _, l := range links {
		if linkEscapes(l.Path, l.Target) {
			out = append(out, l)
			continue
		}
		full, err := fsid.SafeJoin(root, l.Path)
		if err != nil {
			continue
		}
		if t, err := os.Readlink(full); err != nil || t != l.Target {
			continue // no longer the link that was written
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(full))
		if err != nil {
			continue
		}
		hops := 0
		r, ok := followLink(dir, l.Target, &hops)
		if ok && !within(r, realRoot, rootInfo, rootErr == nil) {
			out = append(out, l)
		}
	}
	return out
}

// followLink is where target, read from a link in the resolved directory
// dir, leads: every existing symlink on the way is followed (so ".." after
// one means its target's parent, as for the kernel), and a missing
// remainder is kept as written. false: a loop, or a link that can't be read.
func followLink(dir, target string, hops *int) (string, bool) {
	cur := dir
	if strings.HasPrefix(target, "/") {
		cur = "/"
	}
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if *hops++; *hops > 40 {
			return "", false
		}
		t, err := os.Readlink(next)
		if err != nil {
			return "", false
		}
		r, ok := followLink(cur, t, hops)
		if !ok {
			return "", false
		}
		cur = r
	}
	return cur, true
}

// linkEscapes reports whether a link at the worktree-relative path p with
// the given target points outside the worktree, judged by its text alone.
func linkEscapes(p, target string) bool {
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return true
	}
	t := path.Clean(path.Join(path.Dir(p), target))
	return t == ".." || strings.HasPrefix(t, "../")
}

// within reports whether p is dir or inside it: by its spelling, or by an
// existing ancestor being dir itself (another spelling of it, as on a
// case-insensitive filesystem).
func within(p, dir string, dirInfo fs.FileInfo, haveInfo bool) bool {
	if p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator)) {
		return true
	}
	if !haveInfo {
		return false
	}
	for d := p; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && os.SameFile(fi, dirInfo) {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// fetchFunc asks the slot for files and returns them as a gzip'd tar.
type fetchFunc func(protocol.Fetch) (io.Reader, error)

// sessionFetch fetches over the helper session.
func sessionFetch(ctx context.Context, s *transport.Session) fetchFunc {
	return func(f protocol.Fetch) (io.Reader, error) {
		if err := s.Conn.Send(protocol.TypeFetch, f); err != nil {
			return nil, err
		}
		if _, err := s.Expect(ctx, protocol.TypeFetched); err != nil {
			return nil, err
		}
		return s.Blob(ctx), nil
	}
}

func conflictBase(ws *Workspace) string {
	if ws.GitDir != "" {
		return filepath.Join(ws.GitDir, "spotexec")
	}
	return filepath.Join(ws.Root, ".spotexec")
}

// keepBase is where backups and conflict copies go: the repository's common
// git dir, which outlives a linked worktree (git worktree remove deletes the
// worktree's own git dir).
func keepBase(ws *Workspace) string {
	if ws.CommonGitDir != "" {
		return filepath.Join(ws.CommonGitDir, "spotexec")
	}
	return conflictBase(ws)
}

// stashConflict saves the remote version of p under dir and returns where it
// went (a ".deleted" marker when the remote command deleted it).
func stashConflict(dir, p string, want version, rf remoteFile) (string, error) {
	if !want.exists {
		mark := []byte("deleted by the remote command\n")
		full, err := fsid.SafeJoin(dir, p+".deleted")
		if err == nil {
			if err = os.MkdirAll(filepath.Dir(full), 0o700); err == nil {
				err = writeNew(full, mark) // never over an earlier marker whose name this one folds to
			}
		}
		if err != nil && stashCollision(err) {
			// Something in the stash is in the way: a fresh flattened name.
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			f, err := os.CreateTemp(dir, flatName(p)+".deleted-*")
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
	// An earlier stashed path this one folds to (src/Dup.txt, src/dup.txt
	// on a case-insensitive filesystem) must not be overwritten.
	var err error
	if full, jerr := fsid.SafeJoin(dir, p); jerr == nil {
		if _, lerr := os.Lstat(full); lerr == nil {
			err = fmt.Errorf("%s is already stashed: %w", p, syscall.EEXIST)
		}
	}
	if err == nil {
		err = writeLocal(dir, p, rf, nil)
	}
	if err != nil && stashCollision(err) {
		// A collision inside the stash itself (a path that was a directory
		// and is now a file, or a name an earlier stashed path folds to):
		// keep it at the top of the stash under a flattened name nothing
		// else uses.
		base := flatName(p) + ".remote"
		name := base
		for i := 2; ; i++ {
			// Free, or unusable for another reason that writing reports.
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				break
			}
			name = fmt.Sprintf("%s.%d", base, i)
		}
		return filepath.Join(dir, name), writeLocal(dir, name, rf, nil)
	}
	return filepath.Join(dir, filepath.FromSlash(p)), err
}

// flatName is p as one file name for the top of the stash, short enough for
// any filesystem: a deep path keeps its last part and a hash of the rest.
func flatName(p string) string {
	flat := strings.ReplaceAll(p, "/", "__")
	if len(flat) <= 160 {
		return flat
	}
	tail := path.Base(p)
	for len(tail) > 100 {
		_, n := utf8.DecodeLastRuneInString(tail)
		tail = tail[:len(tail)-n]
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:8]) + "__" + tail
}

// writeNew creates p holding b, failing with EEXIST if anything is there.
func writeNew(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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
//
// held are files an over-limit write-back left in this slot. Landing at
// their own path in the worktree, they may come back although Git doesn't
// ignore them, as write-back would have brought them: only while Git still
// doesn't track them, nothing is at that path locally, and the slot still
// has the version that was held.
//
// heldOnly (pull --held) writes nothing but held files, whatever else the
// patterns reach.
//
// A symlink it writes that points outside the worktree is reported to rep
// as write-back reports one; runID, when known, is the run that made it.
func PullArtifacts(ctx context.Context, s *transport.Session, ws *Workspace, root string, patterns []string, held HeldFiles, heldOnly bool, rep ui.Reporter, runID string) (int, error) {
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
	return applyPull(ctx, ws, root, patterns, held, heldOnly, listed.Paths, s.Blob(ctx), rep, runID)
}

// applyPull writes the files of blob (a gzip'd tar) that the runner listed
// and that patterns asked for, under the rules of PullArtifacts.
func applyPull(ctx context.Context, ws *Workspace, root string, patterns []string, held HeldFiles, heldOnly bool, listed []string, blob io.Reader, rep ui.Reporter, runID string) (int, error) {
	// Only paths the runner listed, that match what was asked for, and that
	// are not reserved, may be written; the rest of the stream is ignored.
	allowed := map[string]bool{}
	var candidates []string
	requested := newPullMatcher(patterns)
	for _, p := range listed {
		if _, isHeld := held[p]; heldOnly && !isHeld {
			continue
		}
		if _, err := fsid.CleanRel(p); err != nil || reservedPath(p, ws.Git) || !requested(p) {
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
	heldOK := map[string]bool{} // protected only as not ignored, and held here
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
			protected[p] = prot[l] != 0 || reservedPath(l, ws.Git)
			_, isHeld := held[p]
			heldOK[p] = isHeld && rel == "." && prot[l] == protNotIgnored
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
				heldOK[p] = false
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
	// Symlinks written, judged once every file is in place (a link may
	// lead out through another pulled after it).
	var links []Link
	appliedLink := func(p string, rf remoteFile) {
		if rf.mode == fsid.ModeSymlink {
			links = append(links, Link{Path: p, Target: rf.link})
		}
	}
	defer func() { reportOutsideLinks(rep, pulledOutsideLinks(ws.Root, root, links), runID) }()
	var skipped, kept, stale []string
	defer func() {
		if len(skipped) > 0 {
			fmt.Fprintf(os.Stderr, "spotexec: pull skipped %d source file(s) (tracked or not gitignored): %s\n", len(skipped), strings.Join(firstN(skipped, 5), ", "))
		}
		if len(kept) > 0 {
			fmt.Fprintf(os.Stderr, "spotexec: pull kept %d local file(s) where a held file would land: %s\n", len(kept), strings.Join(firstN(kept, 5), ", "))
		}
		if len(stale) > 0 {
			fmt.Fprintf(os.Stderr, "spotexec: pull skipped %d held file(s) that changed in the slot after the run: %s\n", len(stale), strings.Join(firstN(stale, 5), ", "))
		}
	}()
	n := 0
	err = readTar(blob, func(h *tar.Header, rd io.Reader) error {
		if !allowed[h.Name] {
			return nil
		}
		if protected[h.Name] && !heldOK[h.Name] {
			skipped = append(skipped, h.Name)
			return nil
		}
		rf := remoteFile{}
		switch h.Typeflag {
		case tar.TypeSymlink:
			rf.link, rf.mode, rf.id = h.Linkname, fsid.ModeSymlink, fsid.Bytes([]byte(h.Linkname))
		case tar.TypeReg:
			b, err := io.ReadAll(rd)
			if err != nil {
				return err
			}
			rf.data, rf.mode, rf.id = b, fsid.ModeFile, fsid.Bytes(b)
			if h.Mode&0o111 != 0 {
				rf.mode = fsid.ModeExec
			}
		default:
			return nil
		}
		// A held file, wherever it lands, comes back only as it was held and
		// only into an empty place, ignored there or not.
		if e, isHeld := held[h.Name]; isHeld {
			if rf.id != e.ID || rf.mode != e.Mode {
				stale = append(stale, h.Name)
				return nil
			}
			want := version{true, rf.id, rf.mode}
			local, err := localVersion(root, h.Name)
			if err != nil {
				return err
			}
			if local.eq(want) {
				return nil // already here
			}
			if local.exists {
				kept = append(kept, h.Name)
				return nil
			}
			err = writeLocal(root, h.Name, rf, &version{})
			if errors.Is(err, errChangedUnderUs) || errors.Is(err, fsid.ErrSymlinkParent) || stashCollision(err) {
				kept = append(kept, h.Name)
				return nil
			}
			if err == nil {
				n++
				appliedLink(h.Name, rf)
			}
			return err
		}
		if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(h.Name))); err == nil && fi.IsDir() {
			return nil
		}
		n++
		if err := writeLocal(root, h.Name, rf, nil); err != nil {
			return err
		}
		appliedLink(h.Name, rf)
		return nil
	})
	return n, err
}

// pulledOutsideLinks returns the pulled links (named relative to root) that
// point outside the worktree, judged where they landed: against the
// worktree when root is inside it, named relative to the worktree; against
// root when it is elsewhere (--dest), named by full path.
func pulledOutsideLinks(wsRoot, root string, links []Link) []Link {
	if len(links) == 0 {
		return nil
	}
	if rel, err := filepath.Rel(wsRoot, root); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		in := make([]Link, len(links))
		for i, l := range links {
			in[i] = Link{Path: path.Join(filepath.ToSlash(rel), l.Path), Target: l.Target}
		}
		return outsideLinks(wsRoot, in)
	}
	out := outsideLinks(root, links)
	for i := range out {
		out[i].Path = filepath.Join(root, filepath.FromSlash(out[i].Path))
	}
	return out
}

// Why a pulled path is protected (0: it isn't).
const (
	protTracked    = 1 + iota // Git tracks it
	protNotIgnored            // untracked, and Git would not ignore it
	protUnknown               // Git couldn't say
)

// protectedPaths returns the paths Git tracks or would not ignore, and why.
// Outside a git worktree nothing is tracked, so pulled outputs (already
// limited to the requested patterns) may be written. If git can't answer for
// a git worktree (for example safe.directory), it fails closed.
func protectedPaths(ctx context.Context, root string, paths []string) (map[string]int, error) {
	out := map[string]int{}
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
			out[p] = protUnknown
		}
		return out, nil
	}
	for _, p := range bytes.Split(ib, []byte{0}) {
		if len(p) > 0 {
			ign[string(p)] = true
		}
	}
	isTracked := map[string]bool{}
	b, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		for _, p := range paths { // cannot tell: protect everything
			out[p] = protUnknown
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
			isTracked[fold(string(p))] = true
		}
	}
	for _, p := range paths {
		switch {
		case isTracked[fold(p)]:
			out[p] = protTracked
		case !ign[p]:
			out[p] = protNotIgnored
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
