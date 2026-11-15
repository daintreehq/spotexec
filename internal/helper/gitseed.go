package helper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// A seeded .git ([sync] git_seed) is the HEAD commit as a shallow root, its
// trees, the HEAD blobs of files changed locally, HEAD and the branch ref,
// a minimal config, and an index built from HEAD with git itself. The
// post-run walk skips .git and write-back refuses any path in it, so
// nothing a command does there travels back.

const seedConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"

// maxSeedObject bounds one received object (the CLI caps blobs well below).
const maxSeedObject = 64 << 20

// gitBin finds the git the helper seeds with.
var gitBin = func() (string, error) { return exec.LookPath("git") }

func validateSeed(r protocol.GitSeed) error {
	if r.Key == "" {
		return errors.New("git_seed without a key")
	}
	if r.Head != "" && !protocol.IsObjectID(r.Head) {
		return fmt.Errorf("bad HEAD %q", r.Head)
	}
	if r.Ref != "" {
		if err := protocol.ValidBranchRef(r.Ref); err != nil {
			return err
		}
	}
	if r.Head == "" && len(r.Objects) > 0 {
		return errors.New("objects without a HEAD commit")
	}
	for _, id := range r.Objects {
		if !protocol.IsObjectID(id) {
			return fmt.Errorf("bad object ID %q", id)
		}
	}
	for _, l := range [][]string{r.SkipWorktree, r.IntentToAdd} {
		for _, p := range l {
			if _, err := fsid.CleanRel(p); err != nil {
				return err
			}
		}
	}
	return nil
}

type seedRepo struct {
	ws, dir string // workspace and its .git
	git     string
	req     protocol.GitSeed
	want    map[string]bool
}

func newSeedRepo(ws, git string, req protocol.GitSeed) *seedRepo {
	sr := &seedRepo{ws: ws, dir: filepath.Join(ws, ".git"), git: git, req: req, want: map[string]bool{}}
	for _, id := range req.Objects {
		sr.want[id] = true
	}
	return sr
}

func (sr *seedRepo) headContent() string {
	if sr.req.Ref != "" {
		return "ref: " + sr.req.Ref + "\n"
	}
	if sr.req.Head != "" {
		return sr.req.Head + "\n"
	}
	return "ref: refs/heads/main\n" // detached and unborn can't both hold
}

func (sr *seedRepo) objectPath(id string) string {
	return filepath.Join(sr.dir, "objects", id[:2], id[2:])
}

// current reports whether .git is still exactly the seed: nothing added,
// removed or rewritten since, and every object still there. index and
// digest describe the index as seeded. An index git rewrote with the same
// entries (refreshing its stat cache, as git status does) still counts:
// then refreshed is its new signature.
func (sr *seedRepo) current(ctx context.Context, index *statSig, digest string) (ok bool, refreshed *statSig) {
	if fi, err := os.Lstat(sr.dir); err != nil || !fi.IsDir() {
		return false, nil
	}
	ents, err := os.ReadDir(sr.dir)
	if err != nil {
		return false, nil
	}
	allowed := map[string]bool{"HEAD": true, "config": true, "refs": true, "objects": true}
	if sr.req.Head != "" {
		allowed["shallow"] = true
	}
	if index != nil {
		allowed["index"] = true
	}
	for _, e := range ents {
		if !allowed[e.Name()] {
			return false, nil
		}
	}
	same := func(name, want string) bool {
		b, err := os.ReadFile(filepath.Join(sr.dir, name))
		return err == nil && string(b) == want
	}
	if !same("HEAD", sr.headContent()) || !same("config", seedConfig) {
		return false, nil
	}
	if sr.req.Head != "" && !same("shallow", sr.req.Head+"\n") {
		return false, nil
	}
	if fi, err := os.Lstat(filepath.Join(sr.dir, "refs")); err != nil || !fi.IsDir() {
		return false, nil
	}
	var refs []string
	_ = filepath.WalkDir(filepath.Join(sr.dir, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			refs = append(refs, p)
		}
		return nil
	})
	if sr.req.Ref != "" && sr.req.Head != "" {
		if len(refs) != 1 || refs[0] != filepath.Join(sr.dir, filepath.FromSlash(sr.req.Ref)) || !same(filepath.FromSlash(sr.req.Ref), sr.req.Head+"\n") {
			return false, nil
		}
	} else if len(refs) != 0 {
		return false, nil
	}
	if !sr.storeClean() {
		return false, nil
	}
	fi, err := os.Lstat(filepath.Join(sr.dir, "index"))
	if index == nil {
		return err != nil, nil
	}
	if err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	sg, ok := sigOf(fi)
	if !ok {
		return false, nil
	}
	if sg == *index {
		return true, nil
	}
	if d, err := sr.indexDigest(ctx); err != nil || d != digest {
		return false, nil
	}
	return true, &sg
}

// indexDigest hashes the index's entries (mode, object, flags, path), not
// its stat cache. ls-files doesn't show the intent-to-add bit, so for those
// paths it adds which still differ from the worktree: an intent-to-add
// entry always does, a really added (empty) file doesn't.
func (sr *seedRepo) indexDigest(ctx context.Context) (string, error) {
	h := sha1.New()
	out, err := sr.run(ctx, nil, "ls-files", "-s", "-v", "-z")
	if err != nil {
		return "", err
	}
	h.Write(out)
	if len(sr.req.IntentToAdd) > 0 {
		out, err := sr.run(ctx, nil, append([]string{"diff-files", "--name-only", "-z", "--"}, sr.req.IntentToAdd...)...)
		if err != nil {
			return "", err
		}
		h.Write([]byte{0, 1})
		h.Write(out)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// prepare leaves only the objects the seed wants in .git, and the index
// (its stat cache saves rehashing every file). Anything that isn't a real
// directory or file where one belongs (a symlink a command planted) goes,
// so nothing below is ever read or written through it.
func (sr *seedRepo) prepare() error {
	if fi, err := os.Lstat(sr.dir); err == nil && !fi.IsDir() {
		if err := os.Remove(sr.dir); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(sr.dir, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(sr.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if n := e.Name(); !(n == "objects" && e.IsDir()) && !(n == "index" && e.Type().IsRegular()) {
			if err := os.RemoveAll(filepath.Join(sr.dir, n)); err != nil {
				return err
			}
		}
	}
	objs := filepath.Join(sr.dir, "objects")
	if err := os.MkdirAll(objs, 0o755); err != nil {
		return err
	}
	// Objects nobody asked for go, including packs and alternates a
	// command may have added.
	ents, err = os.ReadDir(objs)
	if err != nil {
		return err
	}
	for _, e := range ents {
		d := filepath.Join(objs, e.Name())
		if !e.IsDir() || len(e.Name()) != 2 || !isHex(e.Name()) {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
			continue
		}
		files, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		for _, f := range files {
			if !f.Type().IsRegular() || !sr.want[e.Name()+f.Name()] {
				if err := os.RemoveAll(filepath.Join(d, f.Name())); err != nil {
					return err
				}
			}
		}
		_ = os.Remove(d) // only when empty
	}
	return nil
}

// storeClean reports whether the object store holds only the seed's
// objects (and the empty blob git writes for intent-to-add entries), as
// plain files in plain directories.
func (sr *seedRepo) storeClean() bool {
	objs := filepath.Join(sr.dir, "objects")
	if fi, err := os.Lstat(objs); err != nil || !fi.IsDir() {
		return false
	}
	ents, err := os.ReadDir(objs)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() || len(e.Name()) != 2 || !isHex(e.Name()) {
			return false
		}
		files, err := os.ReadDir(filepath.Join(objs, e.Name()))
		if err != nil {
			return false
		}
		for _, f := range files {
			id := e.Name() + f.Name()
			if !f.Type().IsRegular() || !(sr.want[id] || id == emptyBlob && len(sr.req.IntentToAdd) > 0) {
				return false
			}
		}
	}
	return len(sr.missing()) == 0
}

const emptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// missing lists the wanted objects not in the store.
func (sr *seedRepo) missing() []string {
	var out []string
	for _, id := range sr.req.Objects {
		if fi, err := os.Lstat(sr.objectPath(id)); err != nil || !fi.Mode().IsRegular() {
			out = append(out, id)
		}
	}
	return out
}

// receive stores objects from a gzip'd tar of "<type>/<id>" entries, each
// checked against its ID and the set asked for.
func (sr *seedRepo) receive(r io.Reader, need map[string]bool) (int, int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	n, total := 0, int64(0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, total, err
		}
		typ, id, _ := strings.Cut(h.Name, "/")
		if !need[id] || h.Typeflag != tar.TypeReg || h.Size > maxSeedObject || (typ != "commit" && typ != "tree" && typ != "blob") {
			return n, total, fmt.Errorf("unexpected object %q", h.Name)
		}
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return n, total, err
		}
		if err := sr.writeObject(typ, id, body); err != nil {
			return n, total, err
		}
		delete(need, id)
		n++
		total += h.Size
	}
	if len(need) > 0 {
		return n, total, fmt.Errorf("%d object(s) never arrived", len(need))
	}
	return n, total, nil
}

// writeObject stores one loose object, refusing content that doesn't hash
// to id.
func (sr *seedRepo) writeObject(typ, id string, body []byte) error {
	hdr := typ + " " + strconv.Itoa(len(body)) + "\x00"
	h := sha1.New()
	h.Write([]byte(hdr))
	h.Write(body)
	if got := hex.EncodeToString(h.Sum(nil)); got != id {
		return fmt.Errorf("object %s arrived with content %s", id, got)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte(hdr))
	zw.Write(body)
	if err := zw.Close(); err != nil {
		return err
	}
	final := sr.objectPath(id)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(final), fmt.Sprintf("tmp_obj_%d_%d", os.Getpid(), time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// build writes HEAD, the ref and config, and the index (with git), and
// returns the index's signature and digest (none when there is no index:
// an unborn branch with nothing staged).
func (sr *seedRepo) build(ctx context.Context) (*statSig, string, error) {
	write := func(rel, content string) error {
		p := filepath.Join(sr.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(content), 0o644)
	}
	if err := os.MkdirAll(filepath.Join(sr.dir, "refs", "heads"), 0o755); err != nil {
		return nil, "", err
	}
	if err := write("config", seedConfig); err != nil {
		return nil, "", err
	}
	if err := write("HEAD", sr.headContent()); err != nil {
		return nil, "", err
	}
	if sr.req.Head != "" {
		if err := write("shallow", sr.req.Head+"\n"); err != nil {
			return nil, "", err
		}
		if sr.req.Ref != "" {
			if err := write(sr.req.Ref, sr.req.Head+"\n"); err != nil {
				return nil, "", err
			}
		}
		// Single-tree merge keeps the stat data of entries that still
		// match, so only files the sync rewrote are hashed again.
		if _, err := sr.run(ctx, nil, "read-tree", "--reset", sr.req.Head); err != nil {
			_ = os.Remove(filepath.Join(sr.dir, "index"))
			if _, err := sr.run(ctx, nil, "read-tree", sr.req.Head); err != nil {
				return nil, "", err
			}
		}
		// --reset keeps the flags of entries it keeps: clear skip-worktree
		// bits no longer wanted, and any assume-unchanged (lowercase tag) a
		// command set.
		skip := map[string]bool{}
		for _, p := range sr.req.SkipWorktree {
			skip[p] = true
		}
		out, err := sr.run(ctx, nil, "ls-files", "-v", "-z")
		if err != nil {
			return nil, "", err
		}
		var unskip, unassume []byte
		for _, rec := range bytes.Split(out, []byte{0}) {
			if len(rec) < 3 || rec[1] != ' ' {
				continue
			}
			p := rec[2:]
			if (rec[0] == 'S' || rec[0] == 's') && !skip[string(p)] {
				unskip = append(append(unskip, p...), 0)
			}
			if rec[0] >= 'a' && rec[0] <= 'z' {
				unassume = append(append(unassume, p...), 0)
			}
		}
		for flag, paths := range map[string][]byte{"--no-skip-worktree": unskip, "--no-assume-unchanged": unassume} {
			if len(paths) > 0 {
				if _, err := sr.run(ctx, paths, "update-index", flag, "-z", "--stdin"); err != nil {
					return nil, "", err
				}
			}
		}
		if len(sr.req.SkipWorktree) > 0 {
			in := []byte(strings.Join(sr.req.SkipWorktree, "\x00") + "\x00")
			if _, err := sr.run(ctx, in, "update-index", "--skip-worktree", "-z", "--stdin"); err != nil {
				return nil, "", err
			}
		}
	} else {
		_ = os.Remove(filepath.Join(sr.dir, "index"))
	}
	// Staged new files: added (intent-to-add) rather than untracked.
	var ita []string
	for _, p := range sr.req.IntentToAdd {
		if full, err := fsid.SafeJoin(sr.ws, p); err == nil {
			if _, err := os.Lstat(full); err == nil {
				ita = append(ita, p)
			}
		}
	}
	for len(ita) > 0 {
		n := min(len(ita), 500)
		if _, err := sr.run(ctx, nil, append([]string{"add", "-N", "-f", "--"}, ita[:n]...)...); err != nil {
			return nil, "", err
		}
		ita = ita[n:]
	}
	if sr.req.Head != "" {
		// Exit 1 only means some file differs from HEAD.
		if _, err := sr.run(ctx, nil, "update-index", "-q", "--refresh"); err != nil && exitStatus(err) != 1 {
			return nil, "", err
		}
	}
	fi, err := os.Lstat(filepath.Join(sr.dir, "index"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	sg, _ := sigOf(fi)
	d, err := sr.indexDigest(ctx)
	if err != nil {
		return nil, "", err
	}
	return &sg, d, nil
}

// run runs git on the seeded repository, isolated from the runner's own
// git configuration (hooks, fsmonitor, filters) and environment.
func (sr *seedRepo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, sr.git, args...)
	cmd.Dir = sr.ws
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// HOME and XDG_CONFIG_HOME too: git before 2.32 ignores GIT_CONFIG_GLOBAL.
	cmd.Env = append(cmd.Env, "HOME="+os.DevNull, "XDG_CONFIG_HOME="+os.DevNull, "GIT_DIR="+sr.dir, "GIT_WORK_TREE="+sr.ws, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &seedGitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

type seedGitError struct {
	args   []string
	err    error
	stderr string
}

func (e *seedGitError) Error() string {
	msg := fmt.Sprintf("git %s: %v", e.args[0], e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *seedGitError) Unwrap() error { return e.err }

func exitStatus(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// gitSeed seeds the workspace's .git. After GitNeed it always answers
// GitSeeded: a failure leaves the slot without a .git and is reported as a
// warning, never as an error, so the run goes on.
func (s *Session) gitSeed(req protocol.GitSeed) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before git_seed")
	}
	if err := validateSeed(req); err != nil {
		s.dropGitSeed() // never leave an older seed behind for this run
		return perr(protocol.CodeBadRequest, "%v", err)
	}
	start := time.Now()
	var res protocol.GitSeeded
	done := func(warning string) error {
		res.Warning, res.MS = warning, time.Since(start).Milliseconds()
		return s.c.Send(protocol.TypeGitSeeded, res)
	}
	fail := func(err error) error {
		s.dropGitSeed()
		return done(fmt.Sprintf("couldn't seed .git, so the workspace has none: %v", err))
	}
	git, err := gitBin()
	if err != nil {
		s.dropGitSeed()
		if err := s.c.Send(protocol.TypeGitNeed, protocol.GitNeed{}); err != nil {
			return err
		}
		return done("git isn't installed on the runner, so the workspace has no .git")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sr := newSeedRepo(s.workspace, git, req)
	if s.state.GitOwned && s.state.GitSeed == req.Key {
		if ok, refreshed := sr.current(ctx, s.state.GitIndex, s.state.GitIndexDigest); ok {
			if refreshed != nil {
				s.state.GitIndex = refreshed
				_ = saveSlotState(s.slotDir, s.state)
			}
			if err := s.c.Send(protocol.TypeGitNeed, protocol.GitNeed{}); err != nil {
				return err
			}
			return done("")
		}
	}
	// A .git spotexec didn't make is replaced outright, and only becomes
	// spotexec's (for a later drop to remove) once it's gone.
	var prepErr error
	if !s.state.GitOwned {
		prepErr = os.RemoveAll(sr.dir)
	}
	if prepErr == nil {
		s.state.GitOwned, s.state.GitSeed, s.state.GitIndex, s.state.GitIndexDigest = true, "", nil, ""
		if err := saveSlotState(s.slotDir, s.state); err != nil {
			return err
		}
		prepErr = sr.prepare()
	}
	var need []string
	if prepErr == nil {
		need = sr.missing()
	}
	if err := s.c.Send(protocol.TypeGitNeed, protocol.GitNeed{IDs: need}); err != nil {
		return err
	}
	if len(need) > 0 {
		set := make(map[string]bool, len(need))
		for _, id := range need {
			set[id] = true
		}
		// A broken connection ends the session; an error the CLI sent at
		// the end of the stream is only the seed's.
		var lost error
		br := &protocol.BlobReader{Next: func() (protocol.Frame, error) {
			f, err := s.next()
			if err != nil {
				lost = err
			}
			return f, err
		}}
		n, b, err := sr.receive(br, set)
		if derr := br.Drain(); err == nil {
			err = derr
		}
		if lost != nil {
			s.dropGitSeed()
			return lost
		}
		res.Objects, res.Bytes = n, b
		if err != nil {
			return fail(err)
		}
	}
	if prepErr != nil {
		return fail(prepErr)
	}
	sig, digest, err := sr.build(ctx)
	if err != nil {
		return fail(err)
	}
	s.state.GitSeed, s.state.GitIndex, s.state.GitIndexDigest = req.Key, sig, digest
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return fail(err)
	}
	res.Reseeded = true
	return done("")
}

// dropGitSeed removes a .git an earlier seed left in the workspace (and
// nothing else: a .git spotexec didn't make is left alone).
func (s *Session) dropGitSeed() {
	if s.slotLock == nil || !s.state.GitOwned {
		return
	}
	if err := os.RemoveAll(filepath.Join(s.workspace, ".git")); err != nil {
		return // still ours: the next run tries again
	}
	s.state.GitOwned, s.state.GitSeed, s.state.GitIndex, s.state.GitIndexDigest = false, "", nil, ""
	_ = saveSlotState(s.slotDir, s.state)
}

// withSafeDirectory lets git in a root container use the seeded .git, which
// the runner user owns (git refuses a repository owned by someone else as
// "dubious ownership"): safe.directory for the workspace, as command-scope
// config, unless the environment already passes config that way.
func withSafeDirectory(env map[string]string, ws string) map[string]string {
	if _, ok := env["GIT_CONFIG_COUNT"]; ok {
		return env
	}
	out := make(map[string]string, len(env)+3)
	for k, v := range env {
		out[k] = v
	}
	out["GIT_CONFIG_COUNT"] = "1"
	out["GIT_CONFIG_KEY_0"] = "safe.directory"
	out["GIT_CONFIG_VALUE_0"] = ws
	return out
}
