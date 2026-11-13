// Package dockerignore applies .dockerignore rules (Docker's own matcher) to
// build-context paths, so spotexec hashes and stages exactly the files
// `docker build` would see.
package dockerignore

import (
	"bytes"
	"path"
	"strings"
	"sync"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// Source picks the rules file the way BuildKit does: a Dockerfile-specific
// <Dockerfile>.dockerignore wins over <context>/.dockerignore. Paths are
// slash-separated and relative to the repository root; ctx "" is the root.
func Source(dockerfile, ctx string) []string {
	root := ".dockerignore"
	if ctx != "" {
		root = path.Join(ctx, ".dockerignore")
	}
	return []string{dockerfile + ".dockerignore", root}
}

// Matcher reports whether a context-relative path is excluded. It matches
// directory by directory, carrying each parent's results down, which is how
// BuildKit walks a context (and differs from a flat match when a later
// pattern repeats an already-excluded parent).
type Matcher struct {
	pm   *patternmatcher.PatternMatcher
	mu   sync.Mutex
	dirs map[string]patternmatcher.MatchInfo
}

// New parses .dockerignore content (nil or empty: nothing excluded).
func New(content []byte) (*Matcher, error) {
	if len(content) == 0 {
		return &Matcher{}, nil
	}
	pats, err := ignorefile.ReadAll(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	pm, err := patternmatcher.New(pats)
	if err != nil {
		return nil, err
	}
	return &Matcher{pm: pm, dirs: map[string]patternmatcher.MatchInfo{}}, nil
}

// Excluded reports whether rel (relative to the build context) is left out
// of the context. The Dockerfile's own ignore files are always kept, as
// Docker sends them regardless.
func (m *Matcher) Excluded(rel string) bool {
	if m == nil || m.pm == nil || rel == ".dockerignore" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	parent := patternmatcher.MatchInfo{}
	if dir := path.Dir(rel); dir != "." {
		parent = m.dirInfo(dir)
	}
	ok, _, err := m.pm.MatchesUsingParentResults(rel, parent)
	return err == nil && ok
}

// dirInfo returns the memoised match results for a directory, computed from
// its parent's as in a top-down walk.
func (m *Matcher) dirInfo(dir string) patternmatcher.MatchInfo {
	if mi, ok := m.dirs[dir]; ok {
		return mi
	}
	parent := patternmatcher.MatchInfo{}
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		parent = m.dirInfo(dir[:i])
	}
	_, mi, _ := m.pm.MatchesUsingParentResults(dir, parent)
	m.dirs[dir] = mi
	return mi
}
