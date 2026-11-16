package client

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
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
	return len(segs) == 1 || strings.EqualFold(segs[1], "conflicts") || strings.EqualFold(segs[1], "backup") || strings.EqualFold(segs[1], "spool") || strings.EqualFold(segs[1], "writeback.lock")
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

// configLinks lists the config paths here that may be symlinks, for
// configGuard: the ones the next run here loads (the root's, and each
// .spotexec.toml from the working directory up), those m synced as links
// (m may be nil), and, with find, every config path the worktree has
// (tracked or not ignored in Git, any outside it), for callers without a
// manifest.
func configLinks(ctx context.Context, ws *Workspace, m *manifest.Manifest, find bool) []string {
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
	if !find {
		return links
	}
	if ws.Git {
		cmd := exec.CommandContext(ctx, "git", "-C", ws.Root, "ls-files", "-z", "-c", "-o", "--exclude-standard", "--",
			":(icase,glob)**/.spotexec.toml", ":(icase,glob)**/.spotexecignore", ":(icase,glob)**/.spotexec", ":(icase,glob)**/.spotexec/**")
		if out, err := cmd.Output(); err == nil {
			for _, p := range strings.Split(string(out), "\x00") {
				if p != "" {
					links = append(links, p)
				}
			}
		}
		return links
	}
	heavy := map[string]bool{}
	for _, d := range manifest.HeavyDirs {
		heavy[d] = true
	}
	_ = filepath.WalkDir(ws.Root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(ws.Root, full)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && rel != "." && (heavy[d.Name()] && !strings.EqualFold(d.Name(), ".spotexec") || stateDir(rel)) {
			return filepath.SkipDir
		}
		if spotexecConfigPath(rel) && d.Type()&fs.ModeSymlink != 0 {
			links = append(links, rel)
		}
		return nil
	})
	return links
}

// stateDir reports whether rel is spotexec's own state in a .spotexec
// directory (conflicts, backups), which holds no config.
func stateDir(rel string) bool {
	dir, name := path.Split(rel)
	return strings.EqualFold(path.Base(strings.TrimSuffix(dir, "/")), ".spotexec") && (strings.EqualFold(name, "conflicts") || strings.EqualFold(name, "backup"))
}

// configGuard returns whether a run may not write p: spotexec's own config
// (spotexecConfigPath), or what that config leads to here through symlinks
// among links (worktree-relative config paths that may be links). Each link
// is resolved as the filesystem would, a component at a time: every symlink
// it passes through and where it lands (existing yet or not) is protected,
// with everything below it, and so is each directory on the way there, as a
// name (a run may not make one a link elsewhere). Writing config.toml
// rewrites the config as surely as writing .spotexec.toml when
// .spotexec.toml -> config.toml. Case is folded.
func configGuard(root string, links []string) func(string) bool {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	reached, above := map[string]bool{}, map[string]bool{}
	for _, l := range links {
		resolveLinks(root, l, func(p string) {
			p = strings.ToLower(p)
			reached[p] = true
			for d := path.Dir(p); d != "."; d = path.Dir(d) {
				above[d] = true
			}
		})
	}
	return func(p string) bool {
		if spotexecConfigPath(p) || reached["."] || above[strings.ToLower(p)] {
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

// resolveLinks resolves root/p (root canonical) through symlinks a
// component at a time from /, as the kernel does: an absolute target starts
// again at /, and .. leaves what is resolved so far, wherever that is. visit
// gets, relative to root, every symlink passed through inside root and
// where p ends, if inside root. A component that doesn't exist is taken as
// spelled. It stops after 40 links.
func resolveLinks(root, p string, visit func(string)) {
	inRoot := func(abs string) {
		if rel, err := filepath.Rel(root, filepath.FromSlash(abs)); err == nil {
			if rel = filepath.ToSlash(rel); rel != ".." && !strings.HasPrefix(rel, "../") {
				visit(rel)
			}
		}
	}
	todo := strings.Split(filepath.ToSlash(filepath.Join(root, filepath.FromSlash(p))), "/")
	cur := "/"
	for hops := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, c)
		fi, err := os.Lstat(filepath.FromSlash(next))
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		inRoot(next)
		if hops++; hops > 40 {
			return
		}
		t, err := os.Readlink(filepath.FromSlash(next))
		if err != nil {
			return
		}
		if t = filepath.ToSlash(t); path.IsAbs(t) {
			cur = "/"
		}
		todo = append(strings.Split(t, "/"), todo...)
	}
	inRoot(cur)
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
