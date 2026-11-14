// Package manifest enumerates the local working set to sync (Git-aware), and
// computes content IDs plus a workspace fingerprint.
package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/ignore"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// BuiltinExcludes are never synced from untracked/non-Git sources. Tracked
// files are always synced unless the user excludes them explicitly.
//
// .git and .spotexec match files too (a linked worktree's .git is a file
// holding a local path). target is anchored: only a top-level Rust/Maven
// build dir is assumed to be output; a source directory named "target"
// elsewhere is not.
var BuiltinExcludes = []string{
	".git", "node_modules/", ".venv/", "venv/", "__pycache__/", ".next/", ".turbo/",
	"/target/", ".spotexec", ".DS_Store", ".pytest_cache/", ".mypy_cache/", ".gradle/",
	".spotexec-tmp-*",
}

// NeverSync is never synced from untracked/non-Git sources, not even where
// Options.Include lifts the built-in excludes: spotexec's own state in a
// non-Git project's .spotexec/ (write-back lock, stashed conflicts). It is
// matched case-insensitively, as macOS and Windows filesystems resolve it.
var NeverSync = []string{"/.spotexec/conflicts/", "/.spotexec/writeback.lock"}

var textAttr = regexp.MustCompile(`(^|\s)-?(text|crlf)(=|\s|$)`)

// HeavyDirs are skipped by the remote post-run walk (they are build outputs
// or dependency trees, never source).
var HeavyDirs = []string{
	".git", "node_modules", ".venv", "venv", "__pycache__", ".next", ".turbo", "target",
	".cache", ".pytest_cache", ".mypy_cache", ".gradle", ".spotexec",
}

// Options controls enumeration.
type Options struct {
	Root    string   // worktree root (absolute)
	Exclude []string // [sync].exclude patterns
	Source  string   // "", "git" or "directory"
	Protect []string // cache dirs never synced (deps.cache_dirs)
	// Include lists repo-relative files and directories synced even where
	// a built-in exclude covers them, Git or not: the container's
	// Dockerfile, its .dockerignore and its build context (.spotexec/ by
	// default). Below a directory listed here only the built-in excludes
	// that match inside it apply. [sync].exclude still applies, and
	// NeverSync always does.
	Include []string
}

// Manifest is the local working set.
type Manifest struct {
	Root        string
	Git         bool
	Head        string
	Entries     []protocol.Entry
	ByPath      map[string]protocol.Entry
	Fingerprint string
	Bytes       int64
	Excluded    map[string]int // pattern/reason -> count, for sync-plan
	Matcher     *ignore.Matcher
	spot        *spotExcluder // spotexec's own excludes, with Include applied
}

// Build enumerates and hashes the working set.
func Build(ctx context.Context, o Options) (*Manifest, error) {
	m := &Manifest{Root: o.Root, ByPath: map[string]protocol.Entry{}, Excluded: map[string]int{}}
	user := ignore.New(o.Exclude...)
	if err := user.AddFile(filepath.Join(o.Root, ".spotexecignore")); err != nil {
		return nil, err
	}
	for _, p := range o.Protect {
		// cache_dirs are paths from the repository root: anchor them, or
		// "build" would also drop a tracked cmd/build/main.go.
		user.Add("/" + strings.Trim(strings.TrimPrefix(p, "./"), "/") + "/")
	}
	m.Matcher = user
	builtin := newSpotExcluder(o.Include)
	m.spot = builtin

	useGit := o.Source != "directory" && isGitWorktree(ctx, o.Root)
	if o.Source == "git" && !useGit {
		return nil, fmt.Errorf("%s is not a git worktree but sync.source = \"git\"", o.Root)
	}
	m.Git = useGit

	type cand struct {
		path    string
		indexID string // non-empty when the index ID can be trusted
	}
	var cands []cand
	if useGit {
		head, _ := git(ctx, o.Root, "rev-parse", "HEAD")
		m.Head = strings.TrimSpace(string(head))
		staged, err := lsStage(ctx, o.Root)
		if err != nil {
			return nil, err
		}
		dirty, untracked, err := statusDirty(ctx, o.Root)
		if err != nil {
			return nil, err
		}
		trustIndex := !hasWorktreeFilters(o.Root, staged)
		// With autocrlf (true or input) git reports a CRLF worktree file as
		// clean while its index ID describes the LF-normalised blob, so the
		// ID would not match the bytes we sync: hash the worktree instead.
		switch v, _ := git(ctx, o.Root, "config", "--get", "core.autocrlf"); strings.ToLower(strings.TrimSpace(string(v))) {
		case "true", "input":
			trustIndex = false
		}
		// assume-unchanged / skip-worktree entries are invisible to git
		// status even when edited: never trust their index IDs.
		if hidden, err := hiddenFromStatus(ctx, o.Root); err == nil {
			for p := range hidden {
				dirty[p] = true
			}
		}
		for p, st := range staged {
			if st.mode == "160000" { // submodule
				m.Excluded["submodule"]++
				continue
			}
			if user.Match(p, false) {
				m.Excluded["exclude"]++
				continue
			}
			c := cand{path: p}
			if !dirty[p] && trustIndex {
				c.indexID = st.id
			}
			cands = append(cands, c)
		}
		for _, p := range untracked {
			if builtin.Match(p, false) {
				m.Excluded["builtin"]++
				continue
			}
			if user.Match(p, false) {
				m.Excluded["exclude"]++
				continue
			}
			cands = append(cands, cand{path: p})
		}
	} else {
		gitignore := ignore.New()
		_ = gitignore.AddFile(filepath.Join(o.Root, ".gitignore"))
		dirMatcher := orMatch{builtin, gitignore}
		err := filepath.WalkDir(o.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == o.Root {
				return nil
			}
			rel := filepath.ToSlash(must(filepath.Rel(o.Root, p)))
			if d.IsDir() {
				if dirMatcher.Match(rel, true) || user.Match(rel, true) {
					m.Excluded["ignored-dir"]++
					return filepath.SkipDir
				}
				return nil
			}
			if dirMatcher.Match(rel, false) || user.Match(rel, false) {
				m.Excluded["ignored"]++
				return nil
			}
			cands = append(cands, cand{path: rel})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// The container's build inputs: the Dockerfile and .dockerignore, and
	// every file in the build context, are needed to build the image. Sync
	// them even when Git ignores them (git status doesn't list them) or
	// they sit in a built-in excluded directory such as .spotexec/.
	listed := make(map[string]bool, len(cands))
	for _, c := range cands {
		listed[c.path] = true
	}
	add := func(p string) {
		if !listed[p] && !user.Match(p, false) && !builtin.Match(p, false) {
			cands = append(cands, cand{path: p})
			listed[p] = true
		}
	}
	for _, r := range builtin.include {
		full, err := fsid.SafeJoin(o.Root, r)
		if err != nil {
			continue
		}
		fi, err := os.Lstat(full)
		switch {
		case err != nil:
		case fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0:
			add(r)
		case fi.IsDir():
			err := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel := path.Join(r, filepath.ToSlash(must(filepath.Rel(full, p))))
				if d.IsDir() {
					if p != full && (builtin.Match(rel, true) || user.Match(rel, true) || isNestedRepo(p)) {
						return filepath.SkipDir
					}
					return nil
				}
				add(rel)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}

	// Stat everything and hash what the index can't vouch for, in parallel.
	entries := make([]protocol.Entry, len(cands))
	keep := make([]bool, len(cands))
	errs := make([]error, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU()*2)
	for i, c := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c cand) {
			defer wg.Done()
			defer func() { <-sem }()
			// A parent directory replaced by a symlink (unstaged) must not
			// make us read, and upload, files from outside the worktree.
			full, err := fsid.SafeJoin(o.Root, c.path)
			if err != nil {
				return // reached through a symlink or an invalid name: skip
			}
			fi, err := os.Lstat(full)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
					return // deleted in the worktree (or its directory became a file)
				}
				errs[i] = err
				return
			}
			mode, ok := fsid.Mode(fi.Mode())
			if !ok {
				return // sockets, fifos, directories (e.g. nested repos)
			}
			e := protocol.Entry{Path: c.path, Mode: mode, Size: fi.Size()}
			if c.indexID != "" && mode != fsid.ModeSymlink {
				e.ID = c.indexID
			} else {
				id, m2, size, err := fsid.Path(full)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return // vanished while scanning
					}
					errs[i] = err
					return
				}
				e.ID, e.Size, e.Mode = id, size, m2
			}
			entries[i], keep[i] = e, true
		}(i, c)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	for i := range entries {
		if keep[i] {
			m.Entries = append(m.Entries, entries[i])
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	h := sha256.New()
	for _, e := range m.Entries {
		m.ByPath[e.Path] = e
		m.Bytes += e.Size
		fmt.Fprintf(h, "%s\x00%o\x00%s\n", e.Path, e.Mode, e.ID)
	}
	m.Fingerprint = hex.EncodeToString(h.Sum(nil))[:32]
	return m, nil
}

// Update replaces entries (used when the helper reports corrected baselines
// or after write-back).
func (m *Manifest) Update(e protocol.Entry) { m.ByPath[e.Path] = e }

type stageEntry struct{ mode, id string }

// hiddenFromStatus lists tracked paths flagged assume-unchanged (lowercase
// tag in `git ls-files -v`) or skip-worktree (tag S).
func hiddenFromStatus(ctx context.Context, root string) (map[string]bool, error) {
	out, err := git(ctx, root, "ls-files", "-v", "-z")
	if err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) < 3 || rec[1] != ' ' {
			continue
		}
		if t := rec[0]; (t >= 'a' && t <= 'z') || t == 'S' {
			res[string(rec[2:])] = true
		}
	}
	return res, nil
}

func lsStage(ctx context.Context, root string) (map[string]stageEntry, error) {
	out, err := git(ctx, root, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	res := map[string]stageEntry{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "<mode> <object> <stage>\t<path>"
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) < 3 {
			continue
		}
		if f[2] != "0" { // merge conflict: treat as dirty, hash from disk
			res[string(path)] = stageEntry{mode: f[0]}
			continue
		}
		res[string(path)] = stageEntry{mode: f[0], id: f[1]}
	}
	return res, nil
}

// statusDirty returns tracked paths whose worktree differs from the index, and
// untracked non-ignored files.
func statusDirty(ctx context.Context, root string) (map[string]bool, []string, error) {
	out, err := gitEnv(ctx, root, []string{"GIT_OPTIONAL_LOCKS=0"}, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=all")
	if err != nil {
		return nil, nil, err
	}
	dirty := map[string]bool{}
	var untracked []string
	recs := bytes.Split(out, []byte{0})
	for i := 0; i < len(recs); i++ {
		r := string(recs[i])
		if r == "" {
			continue
		}
		switch r[0] {
		case '1': // 1 XY sub mH mI mW hH hI path
			f := strings.SplitN(r, " ", 9)
			if len(f) == 9 {
				dirty[f[8]] = dirty[f[8]] || f[1][1] != '.'
			}
		case '2': // 2 XY sub mH mI mW hH hI X<score> path\0origPath
			f := strings.SplitN(r, " ", 10)
			if len(f) == 10 {
				dirty[f[9]] = dirty[f[9]] || f[1][1] != '.'
			}
			i++ // skip origPath
		case 'u':
			f := strings.SplitN(r, " ", 11)
			if len(f) == 11 {
				dirty[f[10]] = true
			}
		case '?':
			untracked = append(untracked, r[2:])
		}
	}
	return dirty, untracked, nil
}

// hasWorktreeFilters reports whether any .gitattributes declares content
// transformations (filters, eol, encodings), in which case index blob IDs do
// not describe worktree bytes and everything must be hashed from disk.
func hasWorktreeFilters(root string, staged map[string]stageEntry) bool {
	check := func(p string) bool {
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		s := string(b)
		// text / crlf attributes convert line endings between the index
		// and the worktree, so index IDs don't describe worktree bytes.
		return strings.Contains(s, "filter=") || strings.Contains(s, "eol=") ||
			strings.Contains(s, "working-tree-encoding") || strings.Contains(s, "ident") ||
			textAttr.MatchString(s)
	}
	for p := range staged {
		if p == ".gitattributes" || strings.HasSuffix(p, "/.gitattributes") {
			if check(filepath.Join(root, filepath.FromSlash(p))) {
				return true
			}
		}
	}
	if gd, err := exec.Command("git", "-C", root, "rev-parse", "--git-common-dir").Output(); err == nil {
		d := strings.TrimSpace(string(gd))
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		if check(filepath.Join(d, "info", "attributes")) {
			return true
		}
	}
	return false
}

func isGitWorktree(ctx context.Context, root string) bool {
	out, err := git(ctx, root, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitEnv(ctx, dir, nil, args...)
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

// Root resolves the worktree root containing dir: the Git toplevel when inside
// a repository, else the directory holding .spotexec.toml, else dir itself.
func Root(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		r := strings.TrimSpace(string(out))
		if rr, err := filepath.EvalSymlinks(r); err == nil {
			r = rr
		}
		return r, true
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".spotexec.toml")); err == nil {
			return d, false
		}
		if d == filepath.Dir(d) {
			return dir, false
		}
	}
}

// GitDir returns the per-worktree git dir (handles linked worktrees whose .git
// is a file).
func GitDir(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CheckIgnored filters paths through git check-ignore (exact Git semantics),
// plus the user matcher. Returns the set of ignored paths.
func (m *Manifest) CheckIgnored(ctx context.Context, paths []string) (map[string]bool, error) {
	ign := map[string]bool{}
	var rest []string
	for _, p := range paths {
		if m.Matcher.Match(p, false) {
			ign[p] = true
		} else {
			rest = append(rest, p)
		}
	}
	if len(rest) == 0 {
		return ign, nil
	}
	builtin := m.spot
	if builtin == nil {
		builtin = newSpotExcluder(nil)
	}
	if !m.Git {
		gitignore := ignore.New()
		_ = gitignore.AddFile(filepath.Join(m.Root, ".gitignore"))
		for _, p := range rest {
			if builtin.Match(p, false) || gitignore.Match(p, false) {
				ign[p] = true
			}
		}
		return ign, nil
	}
	var in bytes.Buffer
	for _, p := range rest {
		if builtin.Match(p, false) {
			ign[p] = true
			continue
		}
		// "./" keeps a name like ":(literal)x" from being read as pathspec
		// magic; git echoes the path as given.
		in.WriteString("./" + p)
		in.WriteByte(0)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", m.Root, "check-ignore", "--no-index", "-z", "--stdin")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if !(errors.As(err, &ee) && ee.ExitCode() == 1) { // 1 = nothing ignored
			return nil, fmt.Errorf("git check-ignore: %w", err)
		}
	}
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			ign[strings.TrimPrefix(string(p), "./")] = true
		}
	}
	return ign, nil
}

// spotExcluder applies spotexec's own excludes to untracked and non-Git
// paths: NeverSync everywhere, and BuiltinExcludes except where they would
// exclude a root Options.Include lists (or a directory above one): below a
// root only the patterns matching deeper apply, so .spotexec/Dockerfile
// syncs while .spotexec/node_modules does not.
type spotExcluder struct {
	builtin, never *ignore.Matcher
	include        []string // cleaned repo-relative roots
}

func newSpotExcluder(include []string) *spotExcluder {
	x := &spotExcluder{builtin: ignore.New(BuiltinExcludes...), never: ignore.New(NeverSync...)}
	for _, p := range include {
		p = path.Clean(strings.TrimPrefix(filepath.ToSlash(p), "./"))
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
			continue // the root itself is never built-in excluded
		}
		x.include = append(x.include, p)
	}
	return x
}

// Match reports whether the slash-separated relative path is excluded.
func (x *spotExcluder) Match(p string, isDir bool) bool {
	if x.never.Match(strings.ToLower(p), isDir) {
		return true
	}
	best := ""
	for _, r := range x.include {
		switch {
		case p == r:
			return false
		case isDir && strings.HasPrefix(r, p+"/"):
			return false // on the way to an included root
		case strings.HasPrefix(p, r+"/") && len(r) > len(best):
			best = r
		}
	}
	if best != "" {
		return x.builtin.MatchBelow(p, isDir, strings.Count(best, "/")+1)
	}
	return x.builtin.Match(p, isDir)
}

// isNestedRepo reports whether dir is another repository's worktree (a
// submodule or nested clone), whose files are not ours to sync.
func isNestedRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// orMatch excludes what any of its matchers excludes.
type orMatch []interface{ Match(string, bool) bool }

func (o orMatch) Match(p string, isDir bool) bool {
	for _, m := range o {
		if m.Match(p, isDir) {
			return true
		}
	}
	return false
}
