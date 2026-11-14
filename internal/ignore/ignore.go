// Package ignore implements a gitignore-style path matcher used for
// .spotexecignore, [sync].exclude, and non-Git directory walks.
//
// Supported syntax: blank lines and # comments, ! negation (last match wins),
// trailing / (directories only), leading or embedded / (anchored to the root),
// *, ?, [...] within a segment, and ** across segments.
package ignore

import (
	"bufio"
	"os"
	"path"
	"strings"
)

type rule struct {
	negate   bool
	dirOnly  bool
	anchored bool
	segs     []string
}

// Matcher holds ordered rules.
type Matcher struct {
	rules []rule
}

// New builds a matcher from pattern lines.
func New(lines ...string) *Matcher {
	m := &Matcher{}
	m.Add(lines...)
	return m
}

// Add appends pattern lines.
func (m *Matcher) Add(lines ...string) {
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#") {
			continue
		}
		// Unescaped trailing spaces are ignored, as in git.
		for strings.HasSuffix(l, " ") && !strings.HasSuffix(l, "\\ ") {
			l = l[:len(l)-1]
		}
		r := rule{}
		if strings.HasPrefix(l, "!") {
			r.negate = true
			l = l[1:]
		} else if strings.HasPrefix(l, "\\!") || strings.HasPrefix(l, "\\#") {
			l = l[1:]
		}
		if strings.HasSuffix(l, "/") {
			r.dirOnly = true
			l = strings.TrimRight(l, "/")
		}
		if strings.HasPrefix(l, "/") {
			r.anchored = true
			l = strings.TrimLeft(l, "/")
		} else if strings.Contains(l, "/") {
			r.anchored = true
		}
		if l == "" {
			continue
		}
		r.segs = strings.Split(l, "/")
		if !r.anchored {
			r.segs = append([]string{"**"}, r.segs...)
		}
		m.rules = append(m.rules, r)
	}
}

// AddFile appends the patterns of a file if it exists.
func (m *Matcher) AddFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m.Add(sc.Text())
	}
	return sc.Err()
}

// Empty reports whether the matcher has no rules.
func (m *Matcher) Empty() bool { return m == nil || len(m.rules) == 0 }

// Match reports whether the slash-separated relative path is excluded. A path
// is also excluded when any of its parent directories is excluded (git cannot
// re-include a file inside an excluded directory).
func (m *Matcher) Match(rel string, isDir bool) bool {
	if m.Empty() {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if m.matchOne(parts[:i], true) {
			return true
		}
	}
	return m.matchOne(parts, isDir)
}

// MatchBelow is Match for a path whose first skip directories are wanted
// whatever the patterns say: exclusions of those directories are ignored,
// while every pattern is still matched against the whole path (so anchored
// patterns keep their meaning).
func (m *Matcher) MatchBelow(rel string, isDir bool, skip int) bool {
	if m.Empty() {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := skip + 1; i < len(parts); i++ {
		if m.matchOne(parts[:i], true) {
			return true
		}
	}
	return m.matchOne(parts, isDir)
}

func (m *Matcher) matchOne(parts []string, isDir bool) bool {
	ex := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if matchSegs(r.segs, parts) {
			ex = !r.negate
		}
	}
	return ex
}

func matchSegs(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegs(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], parts[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], parts[1:])
}
