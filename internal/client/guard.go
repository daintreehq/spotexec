package client

import (
	"path"
	"strings"
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
// Case is folded, as macOS and Windows filesystems fold it.
func spotexecConfigPath(p string) bool {
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if strings.EqualFold(seg, ".spotexec") {
			return true
		}
	}
	base := segs[len(segs)-1]
	return strings.EqualFold(base, ".spotexec.toml") || strings.EqualFold(base, ".spotexecignore")
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
