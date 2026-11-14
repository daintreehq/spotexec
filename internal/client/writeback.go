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

	"github.com/spotexec/spotexec/internal/fsid"
	"github.com/spotexec/spotexec/internal/manifest"
	"github.com/spotexec/spotexec/internal/protocol"
	"github.com/spotexec/spotexec/internal/transport"
)

// WritebackResult summarizes a write-back.
type WritebackResult struct {
	Applied     []string
	Deleted     []string
	Conflicts   []string
	Ignored     int
	Refused     []string // paths the runner reported that may not be written
	ConflictDir string
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
func writeback(ctx context.Context, s *transport.Session, ws *Workspace, m *manifest.Manifest, r protocol.Result, runID string) (WritebackResult, error) {
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
	ignored, err := m.CheckIgnored(ctx, createdPaths)
	if err != nil {
		return wb, err
	}
	remote := map[string]version{}
	var fetch, accept []string
	for _, c := range changed {
		remote[c.Path] = version{true, c.ID, c.Mode}
		fetch = append(fetch, c.Path)
	}
	for _, c := range created {
		if ignored[c.Path] {
			wb.Ignored++
			continue
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

	// Serialize cooperating spotexec write-backs into this worktree.
	lockDir := conflictBase(ws)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return wb, err
	}
	unlock, err := lockFile(filepath.Join(lockDir, "writeback.lock"))
	if err != nil {
		return wb, err
	}
	defer unlock()
	wb.ConflictDir = filepath.Join(lockDir, "conflicts", runID)

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
				return wb, err
			}
			want := version{true, rf.id, rf.mode}
			if !local.eq(base) && !local.eq(want) {
				if err := stashConflict(wb.ConflictDir, q, want, rf); err != nil {
					return wb, err
				}
				wb.Conflicts = append(wb.Conflicts, q)
				break
			}
			if err := os.Rename(fd, fq); err != nil {
				return wb, fmt.Errorf("renaming %s to %s: %w", del, q, err)
			}
			if !local.eq(want) {
				err := writeLocal(ws.Root, q, rf, &local)
				if errors.Is(err, errChangedUnderUs) {
					if err := stashConflict(wb.ConflictDir, q, want, rf); err != nil {
						return wb, err
					}
					wb.Conflicts = append(wb.Conflicts, q)
					break
				}
				if err != nil {
					return wb, fmt.Errorf("%s: %w", q, err)
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
			if want.exists {
				err := writeLocal(ws.Root, p, rf, &local)
				// Changed under us, or the way is blocked (a file where a
				// directory is needed, kept because of a local edit): keep
				// the local side, stash the remote one.
				if errors.Is(err, errChangedUnderUs) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EEXIST) {
					if err := stashConflict(wb.ConflictDir, p, want, rf); err != nil {
						return err
					}
					wb.Conflicts = append(wb.Conflicts, p)
					return nil
				}
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				wb.Applied = append(wb.Applied, p)
				m.Update(protocol.Entry{Path: p, ID: rf.id, Mode: rf.mode, Size: int64(len(rf.data))})
			} else {
				full, err := fsid.SafeJoin(ws.Root, p)
				if err != nil {
					return err
				}
				if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				wb.Deleted = append(wb.Deleted, p)
				removedDirs = append(removedDirs, path.Dir(p))
			}
		default:
			if err := stashConflict(wb.ConflictDir, p, want, rf); err != nil {
				return err
			}
			wb.Conflicts = append(wb.Conflicts, p)
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
	for _, p := range dels {
		if err := apply(p); err != nil {
			return wb, err
		}
	}
	pruneEmptyDirs(ws.Root, removedDirs)
	removedDirs = nil
	for _, p := range writes {
		if err := apply(p); err != nil {
			return wb, err
		}
	}
	pruneEmptyDirs(ws.Root, removedDirs)
	return wb, nil
}

func conflictBase(ws *Workspace) string {
	if ws.GitDir != "" {
		return filepath.Join(ws.GitDir, "spotexec")
	}
	return filepath.Join(ws.Root, ".spotexec")
}

func stashConflict(dir, p string, want version, rf remoteFile) error {
	if !want.exists {
		full, err := fsid.SafeJoin(dir, p+".deleted")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return err
		}
		return os.WriteFile(full, []byte("deleted by the remote command\n"), 0o600)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	err := writeLocal(dir, p, rf, nil)
	if err != nil && (errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EEXIST)) {
		// A directory/file collision inside the stash itself (a path that
		// was a directory and is now a file): keep it at the top of the
		// stash under a flattened name, where nothing can be in the way.
		return writeLocal(dir, strings.ReplaceAll(p, "/", "__")+".remote", rf, nil)
	}
	return err
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
		if err := os.WriteFile(tmp, rf.data, fsid.Perm(rf.mode)); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Chmod(tmp, fsid.Perm(rf.mode)); err != nil {
			os.Remove(tmp)
			return err
		}
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
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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

func lockFile(p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, err
	}
	if rel != ".." && !strings.HasPrefix(rel, "../") {
		prefix := ""
		if rel != "." {
			prefix = filepath.ToSlash(rel) + "/"
		}
		landing := make([]string, len(candidates))
		for i, p := range candidates {
			landing[i] = prefix + p
		}
		prot, err := protectedPaths(ctx, ws.Root, landing)
		if err != nil {
			return 0, err
		}
		for _, p := range candidates {
			protected[p] = prot[prefix+p]
		}
	}
	// Serialise with write-back into the same worktree.
	lockDir := conflictBase(ws)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return 0, err
	}
	unlock, err := lockFile(filepath.Join(lockDir, "writeback.lock"))
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
