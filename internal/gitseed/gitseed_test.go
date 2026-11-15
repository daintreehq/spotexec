package gitseed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/manifest"
)

type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &repo{t: t, dir: dir}
	r.git("init", "-q", "-b", "feature/seed")
	r.git("config", "user.email", "t@t")
	r.git("config", "user.name", "t")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = r.dir
	out, err := c.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) plan(exclude ...string) (*Plan, *manifest.Manifest) {
	r.t.Helper()
	m, err := manifest.Build(context.Background(), manifest.Options{Root: r.dir, Exclude: exclude})
	if err != nil {
		r.t.Fatal(err)
	}
	p, err := NewPlan(context.Background(), m)
	if err != nil {
		r.t.Fatal(err)
	}
	return p, m
}

// fixture: a committed tree with a nested directory, a tracked secret the
// project excludes and a submodule, then local edits of every kind.
func fixture(t *testing.T) *repo {
	r := newRepo(t)
	r.write("src/lib/a.txt", "a\n")
	r.write("src/b.txt", "b\n")
	r.write("gone.txt", "old\n")
	r.write("secret.env", "TOKEN=1\n")
	r.git("add", "-A")
	r.git("update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendor/sub")
	r.git("commit", "-qm", "first")
	os.MkdirAll(filepath.Join(r.dir, "vendor/sub"), 0o755) // not initialized
	r.write("src/b.txt", "b2\n")                           // modified
	os.Remove(filepath.Join(r.dir, "gone.txt"))            // deleted
	r.write("staged.txt", "s\n")                           // staged new
	r.git("add", "staged.txt")
	r.write("untracked.txt", "u\n")
	return r
}

func TestPlanSeedsOnlyHeadMetadataAndChangedBlobs(t *testing.T) {
	r := fixture(t)
	p, _ := r.plan("*.env")
	s := p.Seed
	if s.Head != r.git("rev-parse", "HEAD") || s.Ref != "refs/heads/feature/seed" {
		t.Fatalf("head %q ref %q", s.Head, s.Ref)
	}
	want := []string{
		r.git("rev-parse", "HEAD"),
		r.git("rev-parse", "HEAD^{tree}"),
		r.git("rev-parse", "HEAD:src"),
		r.git("rev-parse", "HEAD:src/lib"),
		r.git("rev-parse", "HEAD:vendor"),
		r.git("rev-parse", "HEAD:src/b.txt"), // modified: its HEAD version
		r.git("rev-parse", "HEAD:gone.txt"),  // deleted: its HEAD version
	}
	slices.Sort(want)
	if !slices.Equal(s.Objects, want) {
		t.Fatalf("objects\n got %v\nwant %v", s.Objects, want)
	}
	for _, never := range []string{"HEAD:src/lib/a.txt", "HEAD:secret.env"} {
		if p.Has(r.git("rev-parse", never)) {
			t.Fatalf("%s is in the seed", never)
		}
	}
	if !slices.Equal(s.SkipWorktree, []string{"secret.env", "vendor/sub"}) {
		t.Fatalf("skip-worktree %v", s.SkipWorktree)
	}
	if !slices.Equal(s.IntentToAdd, []string{"staged.txt"}) {
		t.Fatalf("intent-to-add %v", s.IntentToAdd)
	}

	// The key is stable, and follows the local state.
	if p2, _ := r.plan("*.env"); p2.Seed.Key != s.Key {
		t.Fatal("key changed without any change")
	}
	r.write("src/lib/a.txt", "a2\n")
	if p3, _ := r.plan("*.env"); p3.Seed.Key == s.Key || !p3.Has(r.git("rev-parse", "HEAD:src/lib/a.txt")) {
		t.Fatal("editing a file didn't change the seed")
	}
}

// An excluded file that is also deleted locally shows as deleted, but its
// content never leaves the machine.
func TestPlanNeverShipsExcludedContent(t *testing.T) {
	r := fixture(t)
	os.Remove(filepath.Join(r.dir, "secret.env"))
	p, _ := r.plan("*.env")
	if p.Has(r.git("rev-parse", "HEAD:secret.env")) || slices.Contains(p.Seed.SkipWorktree, "secret.env") {
		t.Fatalf("seed %+v", p.Seed)
	}
	var buf bytes.Buffer
	if _, err := p.WriteObjects(context.Background(), []string{p.Seed.Head, r.git("rev-parse", "HEAD:secret.env")}, &buf); err == nil || buf.Len() != 0 {
		t.Fatalf("wrote %d bytes for a request outside the seed (err %v)", buf.Len(), err)
	}
}

func TestPlanDetachedAndUnborn(t *testing.T) {
	r := fixture(t)
	r.git("checkout", "-q", "--detach")
	if p, _ := r.plan(); p.Seed.Ref != "" || p.Seed.Head != r.git("rev-parse", "HEAD") {
		t.Fatalf("detached: %+v", p.Seed)
	}

	u := newRepo(t)
	u.write("a.txt", "a\n")
	u.write("b.txt", "b\n")
	u.git("add", "a.txt")
	p, _ := u.plan()
	if p.Seed.Head != "" || p.Seed.Ref != "refs/heads/feature/seed" || len(p.Seed.Objects) != 0 || !slices.Equal(p.Seed.IntentToAdd, []string{"a.txt"}) {
		t.Fatalf("unborn: %+v", p.Seed)
	}
}

func TestPlanNeedsGit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644)
	m, err := manifest.Build(context.Background(), manifest.Options{Root: dir, Source: "directory"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPlan(context.Background(), m); err != ErrNoGit {
		t.Fatalf("err %v", err)
	}
}

// readObjects decodes WriteObjects' stream, checking every entry hashes to
// its name.
func readObjects(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		typ, id, _ := strings.Cut(h.Name, "/")
		body, _ := io.ReadAll(tr)
		sum := sha1.Sum(append([]byte(fmt.Sprintf("%s %d\x00", typ, len(body))), body...))
		if hex.EncodeToString(sum[:]) != id {
			t.Fatalf("%s carries other content", h.Name)
		}
		got[id] = body
	}
}

func TestWriteObjectsStreamsRawObjects(t *testing.T) {
	r := fixture(t)
	r.write("bin.dat", "no final newline\n\x00binary\nlines")
	r.git("add", "bin.dat")
	r.git("commit", "-qm", "binary")
	r.write("bin.dat", "changed")
	p, _ := r.plan("*.env")
	var buf bytes.Buffer
	if _, err := p.WriteObjects(context.Background(), p.Seed.Objects, &buf); err != nil {
		t.Fatal(err)
	}
	got := readObjects(t, buf.Bytes())
	if len(got) != len(p.Seed.Objects) {
		t.Fatalf("%d objects for %d asked", len(got), len(p.Seed.Objects))
	}
	if string(got[r.git("rev-parse", "HEAD:bin.dat")]) != "no final newline\n\x00binary\nlines" {
		t.Fatal("binary blob mangled")
	}
}

// A local replace ref must not make the CLI send another object's content
// (here an excluded secret's) under a seed object's name.
func TestWriteObjectsIgnoresReplaceRefs(t *testing.T) {
	r := fixture(t)
	headB := r.git("rev-parse", "HEAD:src/b.txt")
	r.git("replace", headB, r.git("rev-parse", "HEAD:secret.env"))
	p, _ := r.plan("*.env")
	if !p.Has(headB) {
		t.Fatal("fixture: src/b.txt's HEAD blob isn't in the seed")
	}
	var buf bytes.Buffer
	if _, err := p.WriteObjects(context.Background(), []string{headB}, &buf); err != nil {
		t.Fatal(err)
	}
	if body := readObjects(t, buf.Bytes())[headB]; string(body) != "b\n" {
		t.Fatalf("sent %q", body)
	}
}

// A tracked file that became a directory is deleted as far as git is
// concerned, so it must not be hidden on the runner.
func TestPlanFileReplacedByDirectoryIsDeleted(t *testing.T) {
	r := fixture(t)
	os.Remove(filepath.Join(r.dir, "src/lib/a.txt"))
	r.write("src/lib/a.txt/inner", "x\n")
	p, _ := r.plan("*.env")
	if slices.Contains(p.Seed.SkipWorktree, "src/lib/a.txt") || !p.Has(r.git("rev-parse", "HEAD:src/lib/a.txt")) {
		t.Fatalf("seed %+v", p.Seed)
	}
}

// A large changed file's HEAD version stays home (git diff on the runner
// fails for it rather than doubling a big upload).
func TestPlanCapsChangedBlobs(t *testing.T) {
	r := newRepo(t)
	r.write("big.bin", strings.Repeat("x", MaxBlob+1))
	r.write("small.txt", "s\n")
	r.git("add", "-A")
	r.git("commit", "-qm", "big")
	r.write("big.bin", "now small\n")
	r.write("small.txt", "s2\n")
	p, _ := r.plan()
	if p.Has(r.git("rev-parse", "HEAD:big.bin")) || !p.Has(r.git("rev-parse", "HEAD:small.txt")) {
		t.Fatalf("objects %v", p.Seed.Objects)
	}
}
