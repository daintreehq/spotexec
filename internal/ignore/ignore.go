// Package ignore implements a gitignore-style path matcher used for
// .spotexecignore, [sync].exclude, and non-Git directory walks.
//
// Supported syntax: blank lines and # comments, ! negation (last match wins),
// trailing / (directories only), leading or embedded / (anchored to the root),
// *, ?, [...] within a segment, and ** across segments.
package ignore

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
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
	// git: a trailing ** matches only what is inside a directory, never the
	// directory itself, as Git reads .gitignore files (Tree). Otherwise
	// "dir/**" also excludes dir, as .spotexecignore and [sync].exclude
	// always have.
	git bool
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

// AddFile appends the patterns of a file if it exists. A file that exists
// but can't be read whole (a directory in its place, no permission, a read
// error) is an error naming it.
func (m *Matcher) AddFile(p string) error {
	lines, err := readLines(p, false)
	if err != nil {
		return err
	}
	m.Add(lines...)
	return nil
}

// readLines returns the lines of the file at p: none when there is no such
// file (or a file stands where a directory on its path would be), and an
// error naming p when one exists but can't be read completely. With
// noFollow a symlink at p counts as no file, as Git skips a symlinked
// .gitignore.
func readLines(p string, noFollow bool) ([]string, error) {
	// Nonblocking, so a FIFO in its place fails the regular-file check
	// instead of hanging the scan.
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(p, flags, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || noFollow && errors.Is(err, syscall.ELOOP) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot read ignore file %s: %w", p, unwrapPath(err))
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("cannot read ignore file %s: %w", p, unwrapPath(err))
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("cannot read ignore file %s: not a regular file (%s)", p, fi.Mode().Type())
	}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot read ignore file %s: %w", p, unwrapPath(err))
	}
	return lines, nil
}

// unwrapPath drops the path a *fs.PathError repeats.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
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
	ex, _ := m.decide(parts, isDir)
	return ex
}

// decide applies the rules to the path itself (not its parents): matched
// reports whether any rule matched, excluded what the last one says.
func (m *Matcher) decide(parts []string, isDir bool) (excluded, matched bool) {
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if matchSegs(r.segs, parts, m.git) {
			excluded, matched = !r.negate, true
		}
	}
	return excluded, matched
}

func matchSegs(pat, parts []string, git bool) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		// Read as Git does, a trailing ** matches everything inside,
		// never the directory itself: "dir/**" leaves dir to be entered,
		// so a deeper rule can re-include a file in it.
		if git && len(pat) == 1 {
			return len(parts) > 0
		}
		for i := 0; i <= len(parts); i++ {
			if matchSegs(pat[1:], parts[i:], git) {
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
	return matchSegs(pat[1:], parts[1:], git)
}
