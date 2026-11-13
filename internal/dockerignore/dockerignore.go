// Package dockerignore applies .dockerignore rules (Docker's own matcher) to
// build-context paths, so spotexec hashes and stages exactly the files
// `docker build` would see.
package dockerignore

import (
	"bytes"
	"path"

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

// Matcher reports whether a context-relative path is excluded.
type Matcher struct {
	pm *patternmatcher.PatternMatcher
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
	return &Matcher{pm: pm}, nil
}

// Excluded reports whether rel (relative to the build context) is left out
// of the context. The Dockerfile's own ignore files are always kept, as
// Docker sends them regardless.
func (m *Matcher) Excluded(rel string) bool {
	if m == nil || m.pm == nil || rel == ".dockerignore" {
		return false
	}
	ok, err := m.pm.MatchesOrParentMatches(rel)
	return err == nil && ok
}
