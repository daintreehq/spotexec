package helper

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spotexec/spotexec/internal/fsid"
	"github.com/spotexec/spotexec/internal/protocol"
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
}

func (s *Session) takeSnapshot(skip []string) snapshot {
	snap := snapshot{sig: map[string]statSig{}, unknown: map[string]bool{}}
	skipNames := map[string]bool{}
	var skipPaths []string
	for _, k := range skip {
		k = strings.Trim(k, "/")
		if k == "" {
			continue
		}
		if strings.Contains(k, "/") || strings.ContainsAny(k, "*?[") {
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
		fi, err := os.Lstat(filepath.Join(s.workspace, filepath.FromSlash(rel)))
		switch {
		case err == nil:
			if sg, ok := sigOf(fi); ok {
				snap.sig[rel] = sg
			}
		case !errors.Is(err, fs.ErrNotExist):
			snap.unknown[rel] = true
		}
	}
	return snap
}

// diff computes the mutation batch between pre and post snapshots. Manifest
// files are compared by content ID against the verified baseline, so a tool
// that rewrites identical bytes produces no mutation.
func (s *Session) diff(pre, post snapshot) (changed []protocol.Entry, deleted []string, created []protocol.Entry, truncated bool, unverified []string) {
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
		id, mode, size, err := fsid.Path(filepath.Join(s.workspace, filepath.FromSlash(rel)))
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
		if len(created) >= MaxCreated {
			truncated = true
			break
		}
		id, mode, size, err := fsid.Path(filepath.Join(s.workspace, filepath.FromSlash(rel)))
		if err != nil {
			unverified = append(unverified, rel)
			continue
		}
		created = append(created, protocol.Entry{Path: rel, ID: id, Mode: mode, Size: size})
	}
	return
}
