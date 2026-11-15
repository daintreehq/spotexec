// Package gitseed plans the minimal .git a slot gets with [sync] git_seed:
// the HEAD commit (as a shallow root), its trees, and the HEAD version of
// files changed locally, so git rev-parse, git log -1, git status and
// git diff answer on the runner as they do in the local worktree. It reads
// nothing of the repository beyond that commit: no other history, refs,
// config, hooks or remotes.
package gitseed

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// MaxBlob caps the HEAD version shipped for a locally changed file: git diff
// on the runner fails for a bigger one rather than doubling a large upload.
const MaxBlob = 16 << 20

// ErrNoGit: the project isn't synced from a Git worktree.
var ErrNoGit = errors.New("git_seed needs a Git worktree")

// Plan is what to seed, and which objects that may send.
type Plan struct {
	Seed  protocol.GitSeed
	root  string
	types map[string]string // object ID -> type, the only objects ever sent
	blobs []string          // HEAD blobs of changed files, before the size cap
}

// NewPlan reads HEAD and its trees from the worktree m was built from.
func NewPlan(ctx context.Context, m *manifest.Manifest) (*Plan, error) {
	if !m.Git {
		return nil, ErrNoGit
	}
	p := &Plan{root: m.Root, types: map[string]string{}}
	head := m.Head
	if head != "" && !protocol.IsObjectID(head) {
		return nil, fmt.Errorf("HEAD %q is not a SHA-1 object ID (SHA-256 repositories aren't supported)", head)
	}
	p.Seed.Head = head
	ref, err := git(ctx, m.Root, nil, "symbolic-ref", "-q", "HEAD")
	switch {
	case err == nil:
		r := strings.TrimSpace(string(ref))
		if !strings.HasPrefix(r, "refs/heads/") || protocol.ValidBranchRef(r) != nil {
			return nil, fmt.Errorf("HEAD points at %q, not a branch", r)
		}
		p.Seed.Ref = r
	case exitCode(err) == 1: // detached
	default:
		return nil, err
	}

	var staged []string
	if head == "" {
		// Unborn branch: no commit to seed, but files already staged show up
		// as added rather than untracked.
		out, err := git(ctx, m.Root, nil, "ls-files", "-z")
		if err != nil {
			return nil, err
		}
		staged = splitZ(out)
	} else {
		tree, err := git(ctx, m.Root, nil, "rev-parse", head+"^{tree}")
		if err != nil {
			return nil, err
		}
		p.types[head] = "commit"
		p.types[strings.TrimSpace(string(tree))] = "tree"
		out, err := git(ctx, m.Root, nil, "ls-tree", "-r", "-t", "-z", "--full-tree", head)
		if err != nil {
			return nil, err
		}
		for _, rec := range bytes.Split(out, []byte{0}) {
			if len(rec) == 0 {
				continue
			}
			// "<mode> <type> <id>\t<path>"
			meta, path, ok := bytes.Cut(rec, []byte{'\t'})
			f := strings.Fields(string(meta))
			if !ok || len(f) != 3 {
				return nil, fmt.Errorf("unexpected git ls-tree output %q", rec)
			}
			p.entry(m, f[0], f[1], f[2], string(path))
		}
		if err := p.addBlobs(ctx); err != nil {
			return nil, err
		}
		out, err = git(ctx, m.Root, nil, "diff-index", "--cached", "--name-only", "--diff-filter=A", "-z", head)
		if err != nil {
			return nil, err
		}
		staged = splitZ(out)
	}
	for _, s := range staged {
		if _, ok := m.ByPath[s]; ok {
			p.Seed.IntentToAdd = append(p.Seed.IntentToAdd, s)
		}
	}
	// A name the runner can't hold (the manifest skipped it too) is left
	// out rather than failing the whole seed.
	p.Seed.SkipWorktree = slices.DeleteFunc(p.Seed.SkipWorktree, func(s string) bool { _, err := fsid.CleanRel(s); return err != nil })
	p.Seed.Objects = make([]string, 0, len(p.types))
	for id := range p.types {
		p.Seed.Objects = append(p.Seed.Objects, id)
	}
	sort.Strings(p.Seed.Objects)
	sort.Strings(p.Seed.SkipWorktree)
	sort.Strings(p.Seed.IntentToAdd)
	h := sha256.New()
	fmt.Fprintf(h, "seed/1\x00%s\x00%s\x00", p.Seed.Head, p.Seed.Ref)
	for _, l := range [][]string{p.Seed.Objects, p.Seed.SkipWorktree, p.Seed.IntentToAdd} {
		for _, s := range l {
			fmt.Fprintf(h, "%s\x00", s)
		}
		h.Write([]byte{1})
	}
	p.Seed.Key = hex.EncodeToString(h.Sum(nil))[:32]
	return p, nil
}

// entry classifies one path of the HEAD tree.
func (p *Plan) entry(m *manifest.Manifest, mode, typ, id, path string) {
	switch typ {
	case "tree":
		p.types[id] = "tree"
		return
	case "commit": // a submodule: never synced; deleted only if it is here
		if m.Hidden[path] || present(m.Root, path, true) {
			p.Seed.SkipWorktree = append(p.Seed.SkipWorktree, path)
		}
		return
	case "blob":
	default:
		return
	}
	if e, ok := m.ByPath[path]; ok {
		switch {
		case m.Hidden[path]:
			// git status doesn't look at it here, so it doesn't there either.
			p.Seed.SkipWorktree = append(p.Seed.SkipWorktree, path)
		case e.ID != id:
			p.blobs = append(p.blobs, id) // changed: its HEAD version, for git diff
		case mode == "120000":
			// Git compares a symlink with its blob, not by hash: without it
			// an unchanged link shows as modified.
			p.blobs = append(p.blobs, id)
		}
		return
	}
	// In HEAD but not synced: hidden or excluded (it's still here), or
	// deleted, which the runner should see as deleted too.
	if m.Hidden[path] || present(m.Root, path, false) {
		p.Seed.SkipWorktree = append(p.Seed.SkipWorktree, path)
		return
	}
	if m.Matcher == nil || !m.Matcher.Match(path, false) {
		p.blobs = append(p.blobs, id)
	}
}

// addBlobs adds the changed files' HEAD blobs up to MaxBlob, sizing only
// those (usually a handful) rather than every blob in HEAD.
func (p *Plan) addBlobs(ctx context.Context) error {
	if len(p.blobs) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", p.root, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	cmd.Env = gitEnv()
	cmd.Stdin = strings.NewReader(strings.Join(p.blobs, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git cat-file --batch-check: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		if n, err := strconv.ParseInt(f[2], 10, 64); err == nil && n <= MaxBlob {
			p.types[f[0]] = "blob"
		}
	}
	return nil
}

// Has reports whether id is one of the plan's objects (the only ones the
// runner may ask for).
func (p *Plan) Has(id string) bool { _, ok := p.types[id]; return ok }

// WriteObjects streams the named objects, each verified to be one of the
// plan's, as a gzip'd tar of "<type>/<id>" entries holding the raw content.
// It returns the uncompressed bytes written.
func (p *Plan) WriteObjects(ctx context.Context, ids []string, w io.Writer) (int64, error) {
	for _, id := range ids {
		if !p.Has(id) {
			return 0, fmt.Errorf("object %s is not part of the seed", id)
		}
	}
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	tw := tar.NewWriter(zw)
	var total int64
	if len(ids) > 0 {
		cmd := exec.CommandContext(ctx, "git", "-C", p.root, "cat-file", "--batch")
		cmd.Env = gitEnv()
		cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		rerr := func() error {
			br := bufio.NewReaderSize(out, 64<<10)
			for _, id := range ids {
				line, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				f := strings.Fields(line)
				if len(f) != 3 || f[0] != id || f[1] != p.types[id] {
					return fmt.Errorf("git cat-file: unexpected %q for %s", strings.TrimSpace(line), id)
				}
				n, err := strconv.ParseInt(f[2], 10, 64)
				if err != nil {
					return err
				}
				if err := tw.WriteHeader(&tar.Header{Name: f[1] + "/" + id, Typeflag: tar.TypeReg, Size: n, Mode: 0o444}); err != nil {
					return err
				}
				if _, err := io.CopyN(tw, br, n); err != nil {
					return err
				}
				if _, err := br.Discard(1); err != nil { // trailing LF
					return err
				}
				total += n
			}
			return nil
		}()
		if rerr != nil {
			_ = cmd.Process.Kill()
		}
		werr := cmd.Wait()
		if rerr != nil {
			return total, rerr
		}
		if werr != nil {
			return total, fmt.Errorf("git cat-file: %v: %s", werr, strings.TrimSpace(stderr.String()))
		}
	}
	if err := tw.Close(); err != nil {
		return total, err
	}
	return total, zw.Close()
}

// present reports whether a HEAD path that wasn't synced is still here as
// what git would count as it (a file or link, or a submodule's directory),
// so it was excluded rather than deleted. Nothing is followed out of the
// worktree.
func present(root, rel string, gitlink bool) bool {
	full, err := fsid.SafeJoin(root, rel)
	if err != nil {
		return false
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return false
	}
	if gitlink {
		return fi.IsDir()
	}
	return fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0
}

// gitEnv keeps replace refs and lazy fetches out of every read: the objects
// sent must be exactly the ones named, from this repository.
func gitEnv() []string {
	return append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
}

func splitZ(b []byte) []string {
	var out []string
	for _, s := range bytes.Split(b, []byte{0}) {
		if len(s) > 0 {
			out = append(out, string(s))
		}
	}
	return out
}

func git(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(gitEnv(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitError) Unwrap() error { return e.err }

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
