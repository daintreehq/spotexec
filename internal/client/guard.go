package client

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
)

// reservedPath reports whether write-back or pull must never touch p: any
// path through a .git component, and outside git spotexec's own state in
// .spotexec/. The comparison folds case, because macOS (and Windows)
// filesystems do: a remote ".GIT/hooks/x" lands in the real .git/hooks. A
// runner is not trusted to write there: a git hook would run on the laptop.
//
// .spotexec holds spotexec's own state (the write-back lock, stashed
// conflicts, backups of what write-back deleted) only outside git (in a git worktree it lives under the git
// dir). The rest of .spotexec/, such as a container's Dockerfile, syncs like
// any project file, but a runner's edits to it are held (spotexecConfigPath).
func reservedPath(p string, git bool) bool {
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	if git || !strings.EqualFold(segs[0], ".spotexec") {
		return false
	}
	return len(segs) == 1 || strings.EqualFold(segs[1], "conflicts") || strings.EqualFold(segs[1], "backup") || strings.EqualFold(segs[1], "writeback.lock")
}

// spotexecConfigPath reports whether p is spotexec's own configuration, at
// any depth: a .spotexec.toml (a nested one is the project config for runs
// below it), a .spotexecignore, or anything in a .spotexec directory (a
// container's Dockerfile, [setup] scripts). A runner may not write these
// into the worktree: the next run would load what it wrote, and a single
// env_allow line forwards local secrets to whoever controls the runner.
// Case is folded, as macOS and Windows filesystems fold it. A config name
// anywhere in p counts, so a run can't make .spotexec.toml a directory.
func spotexecConfigPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".spotexec") || strings.EqualFold(seg, ".spotexec.toml") || strings.EqualFold(seg, ".spotexecignore") {
			return true
		}
	}
	return false
}

// configLinks lists the config paths here that may be symlinks for
// configGuard: those m synced as links (m may be nil), and the ones the next
// run here loads (the root's, and each .spotexec.toml from the working
// directory up).
func configLinks(ws *Workspace, m *manifest.Manifest) []string {
	links := []string{".spotexec.toml", ".spotexecignore", ".spotexec"}
	for d := filepath.ToSlash(ws.Cwd); d != "." && d != "" && d != "/"; d = path.Dir(d) {
		links = append(links, path.Join(d, ".spotexec.toml"))
	}
	if m != nil {
		for p, e := range m.ByPath {
			if e.Mode == fsid.ModeSymlink && spotexecConfigPath(p) {
				links = append(links, p)
			}
		}
	}
	return links
}

// configGuard returns whether a run may not write p: spotexec's own config
// (spotexecConfigPath), or a path that config reaches here through a
// symlink among links (worktree-relative config paths that may be links):
// every hop of the chain and every symlinked directory on the way, and
// anything below them. Writing config.toml rewrites the config as surely as
// writing .spotexec.toml when .spotexec.toml -> config.toml. Case is folded.
func configGuard(root string, links []string) func(string) bool {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	reached := map[string]bool{}
	add := func(p string) { reached[strings.ToLower(p)] = true }
	inside := func(p string) bool { return p != ".." && !strings.HasPrefix(p, "../") && !path.IsAbs(p) }
	isLink := func(p string) bool {
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		return err == nil && fi.Mode()&fs.ModeSymlink != 0
	}
	for _, cur := range links {
		for hop := 0; hop < 40; hop++ {
			// cur as spelled (its directory may not exist yet), then where
			// it really is: through any symlinked directories, each of which
			// the config also depends on.
			add(cur)
			dir, base := path.Split(cur)
			if dir = strings.TrimSuffix(dir, "/"); dir != "" {
				for d := dir; d != "."; d = path.Dir(d) {
					if isLink(d) {
						add(d)
					}
				}
				rd, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(dir)))
				if err != nil {
					break
				}
				rel, err := filepath.Rel(root, rd)
				if rel = filepath.ToSlash(rel); err != nil || !inside(rel) {
					break
				}
				cur = path.Join(rel, base)
			}
			add(cur)
			if !isLink(cur) {
				break
			}
			t, err := os.Readlink(filepath.Join(root, filepath.FromSlash(cur)))
			if err != nil {
				break
			}
			if filepath.IsAbs(t) {
				if t, err = filepath.Rel(root, t); err != nil {
					break
				}
				cur = path.Clean(filepath.ToSlash(t))
			} else {
				cur = path.Join(path.Dir(cur), filepath.ToSlash(t))
			}
			if !inside(cur) {
				break
			}
		}
	}
	return func(p string) bool {
		if spotexecConfigPath(p) {
			return true
		}
		for q := strings.ToLower(p); q != "." && q != "/" && q != ""; q = path.Dir(q) {
			if reached[q] {
				return true
			}
		}
		return false
	}
}

// pullMatches reports whether p (workspace-relative) was asked for by one of
// the pull patterns: the pattern matches p or one of its parent directories
// (the helper recurses into matched directories). It mirrors the helper's
// pattern normalisation, so a runner can only return what was requested.
func pullMatches(patterns []string, p string) bool {
	return newPullMatcher(patterns)(p)
}

// newPullMatcher is pullMatches for many paths: patterns without glob
// characters (pull --held sends one per file) are looked up, not matched.
func newPullMatcher(patterns []string) func(string) bool {
	literal := map[string]bool{}
	var globs []string
	for _, pat := range patterns {
		pat = strings.TrimSuffix(strings.TrimPrefix(pat, "./"), "/")
		if strings.ContainsAny(pat, `*?[\`) {
			globs = append(globs, pat)
		} else {
			literal[pat] = true
		}
	}
	return func(p string) bool {
		for prefix := p; ; {
			if literal[prefix] {
				return true
			}
			for _, pat := range globs {
				if ok, _ := path.Match(pat, prefix); ok {
					return true
				}
			}
			i := strings.LastIndex(prefix, "/")
			if i < 0 {
				return false
			}
			prefix = prefix[:i]
		}
	}
}
