package helper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/gitseed"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedFixture is a local worktree with a commit and local changes of every
// kind, and a slot holding its synced files.
type seedFixture struct {
	local string
	s     *Session
	m     *manifest.Manifest
}

func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, local, "init", "-q", "-b", "perf/provenance")
	gitIn(t, local, "config", "user.email", "t@t")
	gitIn(t, local, "config", "user.name", "t")
	writeFile(t, local, "src/lib/a.txt", "a\n")
	writeFile(t, local, "src/b.txt", "b\n")
	writeFile(t, local, "gone.txt", "old\n")
	writeFile(t, local, "secret.env", "TOKEN=1\n")
	writeFile(t, local, ".gitignore", "dist/\n")
	os.Symlink("lib/a.txt", filepath.Join(local, "src/link"))
	gitIn(t, local, "add", "-A")
	gitIn(t, local, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendor/sub")
	gitIn(t, local, "commit", "-qm", "first")
	writeFile(t, local, "src/b.txt", "b2\n")
	writeFile(t, local, "src/lib/a.txt", "a\n")
	gitIn(t, local, "commit", "-qam", "second: the HEAD under test")
	os.MkdirAll(filepath.Join(local, "vendor/sub"), 0o755)
	writeFile(t, local, "src/b.txt", "b3\n")
	os.Remove(filepath.Join(local, "gone.txt"))
	writeFile(t, local, "staged.txt", "s\n")
	gitIn(t, local, "add", "staged.txt")
	writeFile(t, local, "untracked.txt", "u\n")

	f := &seedFixture{local: local}
	f.m = f.manifest(t)
	f.s = testSession(t, t.TempDir())
	if err := f.s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	// What the forward sync would have written.
	for _, e := range f.m.Entries {
		if target, err := os.Readlink(filepath.Join(local, e.Path)); err == nil {
			os.MkdirAll(filepath.Dir(filepath.Join(f.s.workspace, e.Path)), 0o755)
			if err := os.Symlink(target, filepath.Join(f.s.workspace, e.Path)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(local, e.Path))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, f.s.workspace, e.Path, string(b))
	}
	return f
}

func (f *seedFixture) manifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Build(context.Background(), manifest.Options{Root: f.local, Exclude: []string{"*.env"}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *seedFixture) plan(t *testing.T) *gitseed.Plan {
	t.Helper()
	p, err := gitseed.NewPlan(context.Background(), f.m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// seed plays the CLI's side of git_seed against the session: it answers
// GitNeed with the objects asked for and returns GitSeeded.
func (f *seedFixture) seed(t *testing.T, p *gitseed.Plan) (protocol.GitSeeded, []string) {
	t.Helper()
	s := f.s
	pr, pw := io.Pipe()
	s.c = protocol.NewConn(strings.NewReader(""), pw)
	s.frames = make(chan protocol.Frame, 4096)
	type reply struct {
		res  protocol.GitSeeded
		need []string
		err  error
	}
	done := make(chan reply, 1)
	go func() {
		in := protocol.NewConn(pr, io.Discard)
		var r reply
		for {
			fr, err := in.ReadFrame()
			if err != nil {
				r.err = err
				done <- r
				return
			}
			env, err := protocol.Decode(fr)
			if err != nil {
				r.err = err
				done <- r
				return
			}
			switch env.Type {
			case protocol.TypeGitNeed:
				var need protocol.GitNeed
				_ = env.Unmarshal(&need)
				r.need = need.IDs
				if len(need.IDs) == 0 {
					continue
				}
				bpr, bpw := io.Pipe()
				go func() {
					bw := &protocol.BlobWriter{C: protocol.NewConn(strings.NewReader(""), bpw)}
					_, err := p.WriteObjects(context.Background(), need.IDs, bw)
					bw.CloseWithError(err)
					bpw.Close()
				}()
				bc := protocol.NewConn(bpr, io.Discard)
				for {
					bf, err := bc.ReadFrame()
					if err != nil {
						break
					}
					s.frames <- bf
				}
			case protocol.TypeGitSeeded:
				r.err = env.Unmarshal(&r.res)
				done <- r
				return
			case protocol.TypeError:
				r.err = errors.New(string(env.Body))
				done <- r
				return
			}
		}
	}()
	if err := s.gitSeed(p.Seed); err != nil {
		t.Fatal(err)
	}
	r := <-done
	pw.Close()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.res, r.need
}

// statusSet is git status relative to HEAD as kind+path, without the
// staged/unstaged distinction (the runner's index is built from HEAD).
func statusSet(t *testing.T, dir string) []string {
	t.Helper()
	b, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=all", "--no-renames").Output()
	if err != nil {
		t.Fatalf("git status in %s: %v", dir, err)
	}
	out := string(b)
	var set []string
	for _, l := range strings.Split(out, "\n") {
		if len(l) < 4 {
			continue
		}
		set = append(set, strings.TrimSpace(l[:2])+" "+l[3:])
	}
	slices.Sort(set)
	return set
}

func TestGitSeedAnswersLikeTheLocalWorktree(t *testing.T) {
	f := newSeedFixture(t)
	p := f.plan(t)
	res, need := f.seed(t, p)
	if res.Warning != "" || !res.Reseeded || res.Objects != len(p.Seed.Objects) || len(need) != len(p.Seed.Objects) {
		t.Fatalf("result %+v, asked for %d of %d", res, len(need), len(p.Seed.Objects))
	}
	ws, local := f.s.workspace, f.local
	for _, q := range [][]string{
		{"rev-parse", "HEAD"},
		{"rev-parse", "--abbrev-ref", "HEAD"},
		{"log", "-1", "--format=%H %an <%ae> %at%n%B"},
		{"describe", "--always"},
	} {
		if got, want := gitIn(t, ws, q...), gitIn(t, local, q...); got != want {
			t.Errorf("git %v: runner %q, local %q", q, got, want)
		}
	}
	if real, _ := filepath.EvalSymlinks(ws); gitIn(t, ws, "rev-parse", "--show-toplevel") != real {
		t.Errorf("toplevel %q, want %q", gitIn(t, ws, "rev-parse", "--show-toplevel"), real)
	}
	want := []string{"?? untracked.txt", "A staged.txt", "D gone.txt", "M src/b.txt"}
	if got := statusSet(t, local); !slices.Equal(got, want) {
		t.Fatalf("fixture: local status %v", got)
	}
	if got := statusSet(t, ws); !slices.Equal(got, want) {
		t.Errorf("runner status %v, want %v", got, want)
	}
	// The HEAD versions of changed files are there, so git diff works.
	if d := gitIn(t, ws, "diff", "HEAD", "--", "src/b.txt", "gone.txt"); !strings.Contains(d, "-b2") || !strings.Contains(d, "+b3") || !strings.Contains(d, "-old") {
		t.Errorf("git diff:\n%s", d)
	}
	// Nothing beyond HEAD: no parent, no other refs, no config of ours.
	if _, err := exec.Command("git", "-C", ws, "rev-parse", "--verify", "-q", "HEAD~1").Output(); err == nil {
		t.Error("HEAD~1 resolves on the runner")
	}
	if refs := gitIn(t, ws, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/perf/provenance" {
		t.Errorf("refs %q", refs)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "objects", p.Seed.Head[:2])); err != nil {
		t.Error("HEAD commit not stored loose")
	}
	// The secret's content was never sent.
	if _, err := exec.Command("git", "-C", ws, "cat-file", "-e", gitIn(t, local, "rev-parse", "HEAD:secret.env")).Output(); err == nil {
		t.Error("an excluded file's content reached the runner")
	}

	// The post-run walk never reports anything in .git.
	for rel := range f.s.takeSnapshot(nil).sig {
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			t.Fatalf("snapshot lists %s", rel)
		}
	}
}

func TestGitSeedIsCheapWhenNothingChanged(t *testing.T) {
	f := newSeedFixture(t)
	p := f.plan(t)
	f.seed(t, p)
	if res, need := f.seed(t, p); res.Reseeded || len(need) != 0 {
		t.Fatalf("second seed redid work: %+v need %v", res, need)
	}
	// git status refreshing the index's stat cache changes nothing that
	// matters: no reseed.
	ws := f.s.workspace
	before := *f.s.state.GitIndex
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(ws, "src/lib/a.txt"), future, future)
	gitIn(t, ws, "status", "--porcelain")
	if after, _ := os.Lstat(filepath.Join(ws, ".git", "index")); after != nil {
		if sg, _ := sigOf(after); sg == before {
			t.Fatal("fixture: git status didn't rewrite the index")
		}
	}
	if res, _ := f.seed(t, p); res.Reseeded {
		t.Fatal("an index refresh by git status caused a reseed")
	}
	// A command that staged something: the index is rebuilt, nothing is
	// resent.
	writeFile(t, ws, "src/lib/a.txt", "changed remotely\n")
	gitIn(t, ws, "add", "src/lib/a.txt")
	writeFile(t, ws, "src/lib/a.txt", "a\n")
	if res, need := f.seed(t, p); !res.Reseeded || len(need) != 0 {
		t.Fatalf("after git add: %+v need %v", res, need)
	}
	if got := statusSet(t, ws); !slices.Contains(got, "M src/b.txt") || slices.Contains(got, "M src/lib/a.txt") {
		t.Fatalf("status after reseed %v", got)
	}
}

// Whatever a command does to .git, the next run's seed puts back exactly
// the local HEAD and nothing else.
func TestGitSeedUndoesWhatACommandDidToGit(t *testing.T) {
	f := newSeedFixture(t)
	p := f.plan(t)
	f.seed(t, p)
	ws := f.s.workspace
	head := p.Seed.Head
	made := gitIn(t, ws, "-c", "user.name=r", "-c", "user.email=r@r", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "made on the runner")
	gitIn(t, ws, "update-ref", "HEAD", made)
	gitIn(t, ws, "branch", "other")
	gitIn(t, ws, "tag", "v9")
	gitIn(t, ws, "config", "core.hooksPath", "/tmp/hooks")
	writeFile(t, ws, ".git/hooks/pre-commit", "#!/bin/sh\n")
	res, _ := f.seed(t, p)
	if !res.Reseeded {
		t.Fatal("tampered .git not reseeded")
	}
	if got := gitIn(t, ws, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}
	if refs := gitIn(t, ws, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/perf/provenance" {
		t.Fatalf("refs %q", refs)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); string(b) != seedConfig {
		t.Fatalf("config %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "hooks")); err == nil {
		t.Fatal("hooks survived")
	}
	// Only the seed's objects are left: the runner's commit is gone.
	var objs []string
	filepath.WalkDir(filepath.Join(ws, ".git", "objects"), func(path string, d os.DirEntry, _ error) error {
		// The empty blob is git's own, written for intent-to-add entries.
		if d != nil && !d.IsDir() && !strings.HasSuffix(filepath.ToSlash(path), "e6/9de29bb2d1d6434b8b29ae775ad8c2e48c5391") {
			objs = append(objs, path)
		}
		return nil
	})
	if len(objs) != len(p.Seed.Objects) {
		t.Fatalf("objects %v, want %d", objs, len(p.Seed.Objects))
	}
}

// A new local HEAD moves the seed; only the new objects travel.
func TestGitSeedFollowsHead(t *testing.T) {
	f := newSeedFixture(t)
	f.seed(t, f.plan(t))
	writeFile(t, f.local, "src/new.txt", "n\n")
	gitIn(t, f.local, "add", "src/new.txt")
	gitIn(t, f.local, "commit", "-qm", "third")
	writeFile(t, f.s.workspace, "src/new.txt", "n\n")
	f.m = f.manifest(t)
	p := f.plan(t)
	res, need := f.seed(t, p)
	if !res.Reseeded || len(need) == 0 || len(need) >= len(p.Seed.Objects) {
		t.Fatalf("result %+v, asked for %d of %d", res, len(need), len(p.Seed.Objects))
	}
	if got, want := gitIn(t, f.s.workspace, "log", "-1", "--format=%s"), "third"; got != want {
		t.Fatalf("log -1 %q", got)
	}
	if got, want := statusSet(t, f.s.workspace), statusSet(t, f.local); !slices.Equal(got, want) {
		t.Fatalf("status %v, local %v", got, want)
	}
}

func TestGitSeedWithoutGitOnTheRunner(t *testing.T) {
	f := newSeedFixture(t)
	p := f.plan(t)
	f.seed(t, p)
	defer func(old func() (string, error)) { gitBin = old }(gitBin)
	gitBin = func() (string, error) { return "", exec.ErrNotFound }
	res, need := f.seed(t, p)
	if res.Warning == "" || len(need) != 0 {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Lstat(filepath.Join(f.s.workspace, ".git")); err == nil {
		t.Fatal("a stale .git was left behind")
	}
}

func TestGitDropRemovesOnlyASeededGit(t *testing.T) {
	f := newSeedFixture(t)
	ws := f.s.workspace
	// A .git spotexec didn't make is left alone by a drop...
	gitIn(t, ws, "init", "-q")
	f.s.dispatch(protocol.Envelope{Type: protocol.TypeGitDrop})
	if _, err := os.Stat(filepath.Join(ws, ".git", "HEAD")); err != nil {
		t.Fatal("drop removed a .git spotexec didn't seed")
	}
	// ...and replaced by a seed.
	f.seed(t, f.plan(t))
	if got := gitIn(t, ws, "rev-parse", "--abbrev-ref", "HEAD"); got != "perf/provenance" {
		t.Fatalf("branch %q", got)
	}
	f.s.dispatch(protocol.Envelope{Type: protocol.TypeGitDrop})
	if _, err := os.Lstat(filepath.Join(ws, ".git")); err == nil {
		t.Fatal("seeded .git survived the drop")
	}
	if st := loadSlotState(f.s.slotDir); st.GitOwned || st.GitSeed != "" {
		t.Fatalf("state %+v", st)
	}
}

func TestGitSeedUnbornBranch(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	local, _ := filepath.EvalSymlinks(t.TempDir())
	gitIn(t, local, "init", "-q", "-b", "main")
	writeFile(t, local, "a.txt", "a\n")
	writeFile(t, local, "b.txt", "b\n")
	gitIn(t, local, "add", "a.txt")
	f := &seedFixture{local: local}
	m, err := manifest.Build(context.Background(), manifest.Options{Root: local})
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	f.s = testSession(t, t.TempDir())
	if err := f.s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.s.workspace, "a.txt", "a\n")
	writeFile(t, f.s.workspace, "b.txt", "b\n")
	if res, _ := f.seed(t, f.plan(t)); res.Warning != "" {
		t.Fatal(res.Warning)
	}
	if got := gitIn(t, f.s.workspace, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("HEAD %q", got)
	}
	if got := statusSet(t, f.s.workspace); !slices.Equal(got, []string{"?? b.txt", "A a.txt"}) {
		t.Fatalf("status %v", got)
	}
}

func TestValidateSeed(t *testing.T) {
	id := strings.Repeat("a", 40)
	for _, bad := range []protocol.GitSeed{
		{},
		{Key: "k", Head: "HEAD"},
		{Key: "k", Head: id, Ref: "refs/heads/../../x"},
		{Key: "k", Head: id, Ref: "refs/tags/v1"},
		{Key: "k", Objects: []string{id}},
		{Key: "k", Head: id, Objects: []string{"../x"}},
		{Key: "k", Head: id, SkipWorktree: []string{"../etc/passwd"}},
		{Key: "k", Head: id, IntentToAdd: []string{".git/config"}},
	} {
		if validateSeed(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := validateSeed(protocol.GitSeed{Key: "k", Head: id, Ref: "refs/heads/feature/x-1", Objects: []string{id}}); err != nil {
		t.Error(err)
	}
}

func TestReceiveRejectsForgedObjects(t *testing.T) {
	body := []byte("content")
	sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(body))), body...))
	id := hex.EncodeToString(sum[:])
	other := strings.Repeat("b", 40)
	archive := func(entries ...[2]string) io.Reader {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		for _, e := range entries {
			tw.WriteHeader(&tar.Header{Name: e[0], Typeflag: tar.TypeReg, Size: int64(len(e[1])), Mode: 0o444})
			tw.Write([]byte(e[1]))
		}
		tw.Close()
		zw.Close()
		return &buf
	}
	for name, c := range map[string]struct {
		r    io.Reader
		need []string
	}{
		"wrong content":  {archive([2]string{"blob/" + other, "content"}), []string{other}},
		"not asked for":  {archive([2]string{"blob/" + id, "content"}), []string{other}},
		"bad type":       {archive([2]string{"tag/" + id, "content"}), []string{id}},
		"path traversal": {archive([2]string{"blob/../../" + id, "content"}), []string{id}},
		"never arrived":  {archive(), []string{id}},
	} {
		ws := t.TempDir()
		sr := newSeedRepo(ws, "git", protocol.GitSeed{})
		need := map[string]bool{}
		for _, n := range c.need {
			need[n] = true
		}
		if _, _, err := sr.receive(c.r, need); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	sr := newSeedRepo(t.TempDir(), "git", protocol.GitSeed{})
	if n, _, err := sr.receive(archive([2]string{"blob/" + id, "content"}), map[string]bool{id: true}); err != nil || n != 1 {
		t.Fatalf("a good object: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(sr.objectPath(id)); err != nil {
		t.Fatal(err)
	}
}

// Each kind of change a command can make to .git, alone, is undone by
// the next seed.
func TestGitSeedNoticesEachTampering(t *testing.T) {
	for name, tamper := range map[string]func(t *testing.T, ws string){
		"loose object": func(t *testing.T, ws string) {
			c := exec.Command("git", "hash-object", "-w", "--stdin")
			c.Dir, c.Stdin = ws, strings.NewReader("planted")
			if err := c.Run(); err != nil {
				t.Fatal(err)
			}
		},
		"alternates": func(t *testing.T, ws string) {
			writeFile(t, ws, ".git/objects/info/alternates", "/elsewhere\n")
		},
		"assume-unchanged": func(t *testing.T, ws string) {
			gitIn(t, ws, "update-index", "--assume-unchanged", "src/lib/a.txt")
		},
		"staged empty file": func(t *testing.T, ws string) {
			gitIn(t, ws, "add", "staged-empty.txt")
		},
		"branch": func(t *testing.T, ws string) { gitIn(t, ws, "branch", "other") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newSeedFixture(t)
			writeFile(t, f.local, "staged-empty.txt", "")
			gitIn(t, f.local, "add", "staged-empty.txt")
			writeFile(t, f.s.workspace, "staged-empty.txt", "")
			f.m = f.manifest(t)
			p := f.plan(t)
			f.seed(t, p)
			want := statusSet(t, f.s.workspace)
			flags := gitIn(t, f.s.workspace, "ls-files", "-v")
			tamper(t, f.s.workspace)
			if res, _ := f.seed(t, p); !res.Reseeded {
				t.Fatal("not noticed")
			}
			if got := statusSet(t, f.s.workspace); !slices.Equal(got, want) {
				t.Fatalf("status %v, want %v", got, want)
			}
			if got := gitIn(t, f.s.workspace, "ls-files", "-v"); got != flags {
				t.Fatalf("flags\n%s\nwant\n%s", got, flags)
			}
			if got := gitIn(t, f.s.workspace, "diff", "--cached", "--name-only", "HEAD"); got != "" {
				t.Fatalf("staged on the runner: %q", got)
			}
		})
	}
}

// A symlink planted for .git/objects is replaced, never written or pruned
// through.
func TestGitSeedNeverFollowsPlantedSymlinks(t *testing.T) {
	f := newSeedFixture(t)
	p := f.plan(t)
	f.seed(t, p)
	outside := t.TempDir()
	writeFile(t, outside, "ab/cdef", "keep me")
	objs := filepath.Join(f.s.workspace, ".git", "objects")
	os.RemoveAll(objs)
	if err := os.Symlink(outside, objs); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.seed(t, p); !res.Reseeded || res.Warning != "" {
		t.Fatalf("result %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "ab/cdef")); err != nil || string(b) != "keep me" {
		t.Fatal("pruned through the symlink")
	}
	if fi, err := os.Lstat(objs); err != nil || !fi.IsDir() {
		t.Fatal("objects is still a symlink")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatal("wrote through the symlink")
	}
}

// A refused request drops an earlier seed rather than leave it standing.
func TestGitSeedRefusedDropsTheOldSeed(t *testing.T) {
	f := newSeedFixture(t)
	f.seed(t, f.plan(t))
	if err := f.s.gitSeed(protocol.GitSeed{Key: "k", Head: "nope"}); err == nil {
		t.Fatal("accepted a bad request")
	}
	if _, err := os.Lstat(filepath.Join(f.s.workspace, ".git")); err == nil {
		t.Fatal("the earlier seed survived")
	}
}

func TestSafeDirectoryForRootContainers(t *testing.T) {
	env := withSafeDirectory(map[string]string{"A": "1"}, "/w")
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "safe.directory" || env["GIT_CONFIG_VALUE_0"] != "/w" || env["A"] != "1" {
		t.Fatalf("env %v", env)
	}
	own := map[string]string{"GIT_CONFIG_COUNT": "2"}
	if got := withSafeDirectory(own, "/w"); len(got) != 1 || got["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("overrode the project's own git config env: %v", got)
	}
}
