package helper

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Python virtualenvs record the absolute path they were created at: console
// scripts' shebangs, the activate scripts, and the .pth files (and
// direct_url.json) of editable installs, which point at the project's
// source. A .venv cached from one slot and hydrated into another would run
// the first slot's interpreter against the first slot's source, and break
// once that slot is pruned. relocateVenvs rewrites those paths for the slot
// it was hydrated into. Compiled bytecode needs nothing: CPython replaces
// co_filename with the real source path on import.

// originFile, in a dependency cache, records the workspace it was built in
// (as spelled, then resolved through symlinks).
const originFile = ".origin"

// maxVenvText caps the files relocateVenvs reads (scripts are small).
const maxVenvText = 1 << 20

var activateVenv = regexp.MustCompile(`(?m)^\s*(?:export\s+)?VIRTUAL_ENV=['"]?([^'"\n]+?)['"]?\s*$`)

// originOf returns the spellings of workspace ws to record in originFile.
func originOf(ws string) string {
	if r, err := filepath.EvalSymlinks(ws); err == nil && r != ws {
		return ws + "\n" + r
	}
	return ws
}

// cacheOrigins reads the workspace spellings a dependency cache recorded
// (none for caches written by older helpers).
func cacheOrigins(cacheRoot string) []string {
	b, err := readSmallFile(filepath.Join(cacheRoot, originFile))
	if err != nil {
		return nil
	}
	var origins []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			origins = append(origins, l)
		}
	}
	return origins
}

// relocateVenvs fixes the virtualenvs at dir (a hydrated cache dir, rel
// from the workspace) and in its immediate subdirectories (.tox/py312),
// built in the workspace spelled origins, for workspace ws. Each venv's
// activate script (when it has one) also says where it was built, which
// covers caches written by older helpers.
func relocateVenvs(dir, rel string, origins []string, ws string) error {
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	venvs := []string{dir}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				venvs = append(venvs, filepath.Join(dir, e.Name()))
			}
		}
	}
	for _, v := range venvs {
		if fi, err := os.Lstat(filepath.Join(v, "pyvenv.cfg")); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		var olds []string
		for _, o := range append(origins, venvOrigin(v, dir, rel)) {
			if filepath.IsAbs(o) && o != ws {
				olds = append(olds, o)
			}
		}
		if len(olds) == 0 {
			continue
		}
		if err := relocateVenv(v, olds, ws); err != nil {
			return err
		}
	}
	return nil
}

// venvOrigin infers the workspace a venv was built in from its activate
// script.
func venvOrigin(v, dir, rel string) string {
	b, err := readSmallFile(filepath.Join(v, "bin", "activate"))
	if err != nil {
		return ""
	}
	m := activateVenv.FindSubmatch(b)
	if m == nil {
		return ""
	}
	sub, err := filepath.Rel(dir, v)
	if err != nil {
		return ""
	}
	ws, ok := strings.CutSuffix(string(m[1]), "/"+filepath.ToSlash(filepath.Join(rel, sub)))
	if !ok {
		return ""
	}
	return ws
}

// relocateVenv rewrites the old workspace paths to ws in the venv's text
// files that record them.
func relocateVenv(v string, olds []string, ws string) error {
	var files []string
	if ents, err := os.ReadDir(filepath.Join(v, "bin")); err == nil {
		for _, e := range ents {
			files = append(files, filepath.Join(v, "bin", e.Name()))
		}
	}
	files = append(files, filepath.Join(v, "pyvenv.cfg"))
	// The top level of site-packages: editable installs' .pth files and the
	// modules they load (setuptools' __editable__*_finder.py, the editables
	// package's _editable_impl_*.py, ...), and each dist-info's
	// direct_url.json.
	sites, _ := filepath.Glob(filepath.Join(v, "lib", "python*", "site-packages"))
	for _, sp := range sites {
		ents, err := os.ReadDir(sp)
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			switch {
			case strings.HasSuffix(n, ".pth") || strings.HasSuffix(n, ".py"):
				files = append(files, filepath.Join(sp, n))
			case strings.HasSuffix(n, ".dist-info") && e.IsDir():
				files = append(files, filepath.Join(sp, n, "direct_url.json"))
			}
		}
	}
	// The activate scripts go last: for caches without originFile they are
	// how a retried relocation knows where the venv came from.
	sort.SliceStable(files, func(i, j int) bool {
		return !strings.HasPrefix(filepath.Base(files[i]), "activate") && strings.HasPrefix(filepath.Base(files[j]), "activate")
	})
	canonV, err := filepath.EvalSymlinks(v)
	if err != nil {
		return err
	}
	for _, f := range files {
		// Never write through a symlinked directory (into another slot).
		if d, err := filepath.EvalSymlinks(filepath.Dir(f)); err != nil || !strings.HasPrefix(d+"/", canonV+"/") {
			continue
		}
		if err := rewritePaths(f, olds, ws); err != nil {
			return err
		}
	}
	return nil
}

func readSmallFile(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxVenvText {
		return nil, os.ErrInvalid
	}
	return os.ReadFile(p)
}

// rewritePaths replaces the paths olds with repl in the text file p,
// replacing the file rather than writing into it: a hardlink-hydrated copy
// shares its inode with the cache. A symlink to a file holding an old path
// (uv's link-mode = "symlink") is replaced by a rewritten regular file; its
// target is left alone.
func rewritePaths(p string, olds []string, repl string) error {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxVenvText {
		return nil // missing, not a regular file, or too large
	}
	b, err := os.ReadFile(p)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return nil // unreadable or binary
	}
	out, n := replacePaths(b, olds, repl)
	if n == 0 {
		return nil
	}
	tmp := p + ".spotexec-relocate"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	// Bytecode compiled from the old text may still pass CPython's
	// mtime+size check (same-length paths, same second) and would keep
	// the old paths in its constants: drop it.
	if stem, ok := strings.CutSuffix(filepath.Base(p), ".py"); ok {
		stale, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "__pycache__", stem+".*.pyc"))
		for _, c := range stale {
			if err := os.Remove(c); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// replacePaths replaces each occurrence of any of olds that is a whole path
// prefix (followed by a separator, quote, whitespace or the end) with repl,
// scanning left to right and preferring the longest at a position, so
// /private/tmp/ws is replaced whole rather than its /tmp/ws suffix.
func replacePaths(b []byte, olds []string, repl string) ([]byte, int) {
	olds = append([]string(nil), olds...)
	sort.Slice(olds, func(i, j int) bool { return len(olds[i]) > len(olds[j]) })
	var out []byte
	n, last := 0, 0
	for i := 0; i < len(b); {
		at, end := -1, -1
		for _, o := range olds {
			j := bytes.Index(b[i:], []byte(o))
			for j >= 0 {
				e := i + j + len(o)
				if e == len(b) || bytes.IndexByte([]byte("/\\'\" \t\r\n:;,"), b[e]) >= 0 {
					break
				}
				k := bytes.Index(b[i+j+1:], []byte(o))
				if k < 0 {
					j = -1
				} else {
					j += 1 + k
				}
			}
			if j >= 0 && (at < 0 || i+j < at) {
				at, end = i+j, i+j+len(o)
			}
		}
		if at < 0 {
			break
		}
		out = append(out, b[last:at]...)
		out = append(out, repl...)
		last, i, n = end, end, n+1
	}
	if n == 0 {
		return b, 0
	}
	return append(out, b[last:]...), n
}
