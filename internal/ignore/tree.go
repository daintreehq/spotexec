package ignore

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/daintreehq/spotexec/internal/fsid"
)

// Tree applies the ignore files of one name (.gitignore) found in the
// directories under a root, as Git does: each file's patterns apply to the
// paths below its own directory and are relative to it, the deepest file
// with a matching pattern decides (its last matching pattern wins, so a
// deeper file's negation re-includes what a shallower one excludes), and
// nothing below an excluded directory is re-included.
//
// Files are read when a path first needs them and kept for the Tree's
// life, so a fresh Tree sees the worktree as it is now. A Tree is not safe
// for concurrent use.
type Tree struct {
	root, name string
	dirs       map[string]*Matcher // "" is root; nil: no file there
}

// NewTree returns a Tree of the files named name under root.
func NewTree(root, name string) *Tree {
	return &Tree{root: root, name: name, dirs: map[string]*Matcher{}}
}

// Match reports whether the slash-separated relative path is excluded. It
// fails, naming the file, when an ignore file that applies to the path
// exists but can't be read completely.
//
// A trailing slash asks about rel as a directory.
func (t *Tree) Match(rel string, isDir bool) (bool, error) {
	if strings.HasSuffix(rel, "/") {
		rel, isDir = strings.TrimRight(rel, "/"), true
	}
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		if ex, err := t.matchOne(parts[:i], i < len(parts) || isDir); ex || err != nil {
			return ex, err
		}
	}
	return false, nil
}

// matchOne decides parts itself, deepest applicable file first.
func (t *Tree) matchOne(parts []string, isDir bool) (bool, error) {
	for j := len(parts) - 1; j >= 0; j-- {
		m, err := t.load(parts[:j])
		if err != nil {
			return false, err
		}
		if m == nil {
			continue
		}
		if ex, ok := m.decide(parts[j:], isDir); ok {
			return ex, nil
		}
	}
	return false, nil
}

// load returns the matcher of dir's ignore file, nil when there is none.
// A directory reached through a symlink (or not a directory here) has none:
// what lies beyond a symlink is not the project's.
func (t *Tree) load(dir []string) (*Matcher, error) {
	key := strings.Join(dir, "/")
	if m, ok := t.dirs[key]; ok {
		return m, nil
	}
	var m *Matcher
	full, err := fsid.SafeJoin(t.root, path.Join(key, t.name))
	var pe *fs.PathError
	switch {
	case errors.As(err, &pe):
		// A directory on the way that can't be looked at.
		return nil, fmt.Errorf("cannot read ignore file %s: %w", filepath.Join(t.root, filepath.FromSlash(path.Join(key, t.name))), err)
	case err != nil:
		// Through a symlink, past a file, or a name no worktree holds.
	default:
		lines, err := readLines(full)
		if err != nil {
			return nil, err
		}
		if m = New(lines...); m.Empty() {
			m = nil
		}
	}
	t.dirs[key] = m
	return m, nil
}
