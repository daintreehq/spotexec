package depskey

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/daintreehq/spotexec/internal/fsid"
)

// IsPattern reports whether a lockfiles entry is a glob rather than a path.
func IsPattern(entry string) bool { return strings.ContainsAny(entry, "*?[") }

// Expand resolves the lockfiles entries to the concrete list that is hashed
// and sent to the runner. It runs once, on the CLI, against files: the
// repo-relative regular files that are synced to the slot. Matching only
// those means a pattern can never pick up an ignored, excluded or symlinked
// file the runner doesn't have, and never reaches outside the root.
//
// Without a pattern, entries are returned as given (order and duplicates
// included) so existing cache keys stay valid. With one, the result is the
// sorted, deduplicated union of the literal entries and every match. Literal
// entries are never checked against files: a missing one still fails in Key.
// unmatched lists the patterns that matched nothing.
func Expand(entries, files []string) (out, unmatched []string, err error) {
	var pats [][]string
	for _, e := range entries {
		if !IsPattern(e) {
			continue
		}
		segs, err := compile(e)
		if err != nil {
			return nil, nil, err
		}
		pats = append(pats, segs)
	}
	if len(pats) == 0 {
		return entries, nil, nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if IsPattern(e) {
			continue
		}
		if _, err := fsid.CleanRel(e); err != nil {
			return nil, nil, fmt.Errorf("deps.lockfiles: %w", err)
		}
		seen[e] = true
	}
	hit := make([]bool, len(pats))
	for _, f := range files {
		segs := strings.Split(f, "/")
		for i, p := range pats {
			if match(p, segs) {
				hit[i] = true
				seen[f] = true
			}
		}
	}
	i := 0
	for _, e := range entries {
		if IsPattern(e) {
			if !hit[i] {
				unmatched = append(unmatched, e)
			}
			i++
		}
	}
	out = make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, unmatched, nil
}

// compile validates a pattern as a clean repo-relative path and splits it
// into segments. Each segment is a path.Match pattern; a segment that is
// exactly "**" matches any number of directories, including none.
func compile(p string) ([]string, error) {
	if _, err := fsid.CleanRel(strings.NewReplacer("*", "x", "?", "x", "[", "x", "]", "x").Replace(p)); err != nil {
		return nil, fmt.Errorf("deps.lockfiles: invalid pattern %q", p)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if _, err := path.Match(s, ""); err != nil {
			return nil, fmt.Errorf("deps.lockfiles: invalid pattern %q: %w", p, err)
		}
	}
	return segs, nil
}

// match reports whether name (path segments) matches pat, in
// O(len(pat)*len(name)) however many "**" segments pat has.
func match(pat, name []string) bool {
	// m[j]: pat[i:] matches name[j:], filled from the last segment back.
	m := make([]bool, len(name)+1)
	m[len(name)] = true
	for i := len(pat) - 1; i >= 0; i-- {
		next := m
		m = make([]bool, len(name)+1)
		if pat[i] == "**" {
			for j := len(name); j >= 0; j-- {
				m[j] = next[j] || (j < len(name) && m[j+1])
			}
			continue
		}
		for j := 0; j < len(name); j++ {
			if next[j+1] {
				ok, _ := path.Match(pat[i], name[j])
				m[j] = ok
			}
		}
	}
	return m[0]
}
