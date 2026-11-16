package helper

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// MaxCreated caps the number of created files reported back in one result.
const MaxCreated = 50000

// snapshot maps relative paths to stat signatures for everything in scope:
// a walk that skips heavy/cache directories, plus every manifest file (so a
// tracked file inside e.g. target/ is still observed). Paths that could not
// be inspected (permissions) are recorded as unknown, never as absent.
type snapshot struct {
	sig     map[string]statSig
	unknown map[string]bool
	// gitDirs are the .git directories below the root: never entered, but
	// a run that creates one must not leave it in the slot.
	gitDirs map[string]bool
}

func (s *Session) takeSnapshot(skip []string) snapshot {
	snap := snapshot{sig: map[string]statSig{}, unknown: map[string]bool{}, gitDirs: map[string]bool{}}
	// A bare name ("node_modules") is skipped at any depth. An entry with a
	// leading "/" or "./" ("/target", "./build"), a "/" inside it, or a glob
	// is a path from the workspace root and is skipped only there, as the
	// manifest anchors /target/ and cache_dirs.
	skipNames := map[string]bool{}
	var skipPaths []string
	for _, k := range skip {
		anchored := strings.HasPrefix(k, "/") || strings.HasPrefix(k, "./")
		k = strings.Trim(strings.TrimPrefix(k, "./"), "/")
		if k == "" {
			continue
		}
		if anchored || strings.Contains(k, "/") || strings.ContainsAny(k, "*?[") {
			skipPaths = append(skipPaths, k)
		} else {
			skipNames[k] = true
		}
	}
	_ = filepath.WalkDir(s.workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == s.workspace {
			return nil
		}
		rel := filepath.ToSlash(p[len(s.workspace)+1:])
		if d.IsDir() {
			// .git is never a source of changes (a seeded one included),
			// whatever the client asked to skip; one below the root is
			// noted as a whole.
			if d.Name() == ".git" {
				if rel != ".git" {
					snap.gitDirs[rel] = true
				}
				return filepath.SkipDir
			}
			if skipNames[d.Name()] || strings.HasPrefix(d.Name(), ".spotexec-tmp") {
				return filepath.SkipDir
			}
			for _, sp := range skipPaths {
				if ok, _ := filepath.Match(sp, rel); ok || sp == rel {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".spotexec-tmp") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if sg, ok := sigOf(fi); ok {
			snap.sig[rel] = sg
		}
		return nil
	})
	for rel := range s.stored {
		if _, ok := snap.sig[rel]; ok {
			continue
		}
		// A parent replaced by a symlink (or a file) means the path is gone
		// from the workspace: never follow it out of the workspace.
		full, err := fsid.SafeJoin(s.workspace, rel)
		if err != nil {
			// Through a symlink, or a parent became a file: gone. Anything
			// else (a directory made unreadable) is unknown, not deleted.
			if !errors.Is(err, fsid.ErrSymlinkParent) && !errors.Is(err, syscall.ENOTDIR) {
				snap.unknown[rel] = true
			}
			continue
		}
		fi, err := os.Lstat(full)
		switch {
		case err == nil:
			if sg, ok := sigOf(fi); ok {
				snap.sig[rel] = sg
			}
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		default:
			snap.unknown[rel] = true
		}
	}
	return snap
}

// diff computes the mutation batch between pre and post snapshots. Manifest
// files are compared by content ID against the verified baseline, so a tool
// that rewrites identical bytes produces no mutation.
//
// unreported lists created paths the result leaves out but the slot still
// tracks for cleanup: those beyond MaxCreated, and those named so that no
// client could use them (.git in another case, such as .Git).
func (s *Session) diff(pre, post snapshot) (changed []protocol.Entry, deleted []string, created []protocol.Entry, truncated bool, unverified, unreported []string) {
	for rel, st := range s.stored {
		if post.unknown[rel] {
			unverified = append(unverified, rel)
			continue
		}
		sg, ok := post.sig[rel]
		if !ok {
			deleted = append(deleted, rel)
			delete(s.stored, rel)
			continue
		}
		if st.sig() == sg {
			continue
		}
		full, err := fsid.SafeJoin(s.workspace, rel)
		if err != nil {
			if errors.Is(err, fsid.ErrSymlinkParent) || errors.Is(err, syscall.ENOTDIR) {
				deleted = append(deleted, rel)
				delete(s.stored, rel)
			} else {
				unverified = append(unverified, rel)
			}
			continue
		}
		id, mode, size, err := fsid.Path(full)
		if err != nil {
			unverified = append(unverified, rel)
			continue
		}
		ne := entryFrom(id, sg)
		s.stored[rel] = ne
		if id != st.ID || mode != st.Mode {
			changed = append(changed, protocol.Entry{Path: rel, ID: id, Mode: mode, Size: size})
		}
	}
	for rel, sg := range post.sig {
		if _, ok := s.stored[rel]; ok {
			continue
		}
		if p, ok := pre.sig[rel]; ok && p == sg {
			continue // untouched non-manifest file (e.g. old build output)
		}
		full, err := fsid.SafeJoin(s.workspace, rel) // also rejects names a client can't use
		if err != nil {
			// Such a name never goes back, but it stays in the slot, where
			// every later command would see it, until a sync removes it.
			if _, cerr := fsid.CleanRel(rel); cerr != nil {
				unreported = append(unreported, rel)
			}
			continue
		}
		if len(created) >= MaxCreated {
			truncated = true
			unreported = append(unreported, rel)
			continue
		}
		id, mode, size, err := fsid.Path(full)
		if err != nil {
			unverified = append(unverified, rel)
			continue
		}
		created = append(created, protocol.Entry{Path: rel, ID: id, Mode: mode, Size: size})
	}
	return
}

// newGitDirs lists the .git directories below the root that appeared
// during the run. They never go back (nothing in one is a client's file),
// so the client is told, and the slot keeps them pending: a later git
// command in their directory would otherwise act on a leftover repository.
func newGitDirs(pre, post snapshot) []string {
	var out []string
	for rel := range post.gitDirs {
		if !pre.gitDirs[rel] {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// nestedGitDir reports whether p, a slot-relative path, names a .git
// directory below the root (never the root's own, the seeded repository).
func nestedGitDir(p string) bool {
	return path.Base(p) == ".git" && strings.Contains(p, "/")
}
