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
// conflicts) only outside git (in a git worktree it lives under the git
// dir). The rest of .spotexec/, such as a container's Dockerfile, is an
// ordinary directory.
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
	return len(segs) == 1 || strings.EqualFold(segs[1], "conflicts") || strings.EqualFold(segs[1], "writeback.lock")
}

// pullMatches reports whether p (workspace-relative) was asked for by one of
// the pull patterns: the pattern matches p or one of its parent directories
// (the helper recurses into matched directories). It mirrors the helper's
// pattern normalisation, so a runner can only return what was requested.
func pullMatches(patterns []string, p string) bool {
	for _, pat := range patterns {
		pat = strings.TrimSuffix(strings.TrimPrefix(pat, "./"), "/")
		prefix := p
		for {
			if ok, _ := path.Match(pat, prefix); ok {
				return true
			}
			i := strings.LastIndex(prefix, "/")
			if i < 0 {
				break
			}
			prefix = prefix[:i]
		}
	}
	return false
}
