package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/transport"

	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
)

func mf(entries ...protocol.Entry) *manifest.Manifest {
	m := &manifest.Manifest{ByPath: map[string]protocol.Entry{}}
	for _, e := range entries {
		m.Entries = append(m.Entries, e)
		m.ByPath[e.Path] = e
	}
	return m
}

func TestImageTagScopesAndPlatform(t *testing.T) {
	base := []protocol.Entry{
		{Path: ".spotexec/Dockerfile", ID: "df1", Mode: 0o100644},
		{Path: ".spotexec/setup.sh", ID: "s1", Mode: 0o100755},
		{Path: "src/app.ts", ID: "a1", Mode: 0o100644},
	}
	t1, err := imageTag(mf(base...), ".spotexec/Dockerfile", "", "linux/amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A source change outside the build context must not change the tag.
	changed := append([]protocol.Entry{}, base...)
	changed[2].ID = "a2"
	if t2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", "", "linux/amd64", nil); t2 != t1 {
		t.Fatal("tag changed for a file outside the context")
	}
	// A file inside the context must.
	changed = append([]protocol.Entry{}, base...)
	changed[1].ID = "s2"
	if t2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", "", "linux/amd64", nil); t2 == t1 {
		t.Fatal("tag unchanged for a context file change")
	}
	// Architectures never share a tag.
	if t3, _ := imageTag(mf(base...), ".spotexec/Dockerfile", "", "linux/arm64", nil); t3 == t1 {
		t.Fatal("amd64 and arm64 share a tag")
	}
	// Explicit repo-root context includes everything.
	r1, _ := imageTag(mf(base...), ".spotexec/Dockerfile", ".", "linux/amd64", nil)
	changed = append([]protocol.Entry{}, base...)
	changed[2].ID = "a2"
	if r2, _ := imageTag(mf(changed...), ".spotexec/Dockerfile", ".", "linux/amd64", nil); r2 == r1 {
		t.Fatal("repo-root context should include src/")
	}
}

func TestImageTagHonoursDockerignore(t *testing.T) {
	base := []protocol.Entry{
		{Path: "Dockerfile", ID: "df1", Mode: 0o100644},
		{Path: ".dockerignore", ID: "ig1", Mode: 0o100644},
		{Path: "package.json", ID: "p1", Mode: 0o100644},
		{Path: "src/app.ts", ID: "a1", Mode: 0o100644},
	}
	rules := map[string][]byte{".dockerignore": []byte("*\n!package.json\n!Dockerfile\n")}
	read := func(rel string) ([]byte, error) { return rules[rel], nil }
	base[1].ID = fsid.Bytes(rules[".dockerignore"])
	t1, err := imageTag(mf(base...), "Dockerfile", ".", "linux/amd64", read)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]protocol.Entry{}, base...)
	changed[3].ID = "a2"
	if t2, _ := imageTag(mf(changed...), "Dockerfile", ".", "linux/amd64", read); t2 != t1 {
		t.Fatal("an ignored file changed the tag")
	}
	changed = append([]protocol.Entry{}, base...)
	changed[2].ID = "p2"
	if t2, _ := imageTag(mf(changed...), "Dockerfile", ".", "linux/amd64", read); t2 == t1 {
		t.Fatal("a kept file did not change the tag")
	}
	// A Dockerfile-specific ignore file wins over the context's.
	rules["Dockerfile.dockerignore"] = []byte("src\n")
	base = append(base, protocol.Entry{Path: "Dockerfile.dockerignore", ID: fsid.Bytes(rules["Dockerfile.dockerignore"]), Mode: 0o100644})
	t3, _ := imageTag(mf(base...), "Dockerfile", ".", "linux/amd64", read)
	changed = append([]protocol.Entry{}, base...)
	changed[3].ID = "a2"
	if t4, _ := imageTag(mf(changed...), "Dockerfile", ".", "linux/amd64", read); t4 != t3 {
		t.Fatal("Dockerfile.dockerignore not preferred")
	}
}

func TestImageTagRejectsUnsyncedOrSymlinkedRules(t *testing.T) {
	ents := []protocol.Entry{
		{Path: "Dockerfile", ID: "df1", Mode: 0o100644},
		{Path: ".dockerignore", ID: fsid.Bytes([]byte("src\n")), Mode: 0o100644},
	}
	edited := func(string) ([]byte, error) { return []byte("src\npayload\n"), nil }
	if _, err := imageTag(mf(ents...), "Dockerfile", ".", "linux/amd64", edited); err == nil {
		t.Fatal("rules edited after sync were accepted")
	}
	ents[1].Mode = fsid.ModeSymlink
	same := func(string) ([]byte, error) { return []byte("src\n"), nil }
	if _, err := imageTag(mf(ents...), "Dockerfile", ".", "linux/amd64", same); err == nil {
		t.Fatal("symlinked rules were accepted")
	}
}

func TestUnknownWritebackPolicyNeverWritesBack(t *testing.T) {
	if wantWriteback("nevre", 0) || wantWriteback("", 0) || wantWriteback("never", 0) {
		t.Error("an unknown or never policy wrote back")
	}
	if !wantWriteback("always", 1) || !wantWriteback("on-success", 0) || wantWriteback("on-success", 1) {
		t.Error("known policies misbehave")
	}
	for _, o := range []Options{{Sync: "auto", Writeback: "nevre"}, {Sync: "bogus", Writeback: "always"}, {}} {
		if validOptions(o) == nil {
			t.Errorf("%q/%q accepted", o.Sync, o.Writeback)
		}
	}
	if err := validOptions(Options{Sync: "in", Writeback: "on-success"}); err != nil {
		t.Error(err)
	}
}

func TestKeepLastRun(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	ws := &Workspace{WorktreeKey: "k"}
	lr := LastRun{Runner: "a", Slot: 2}
	var res Result
	keepLastRun(ws, Options{Sync: "auto", DeferLastRun: true}, lr, &res)
	if res.LastRun == nil || res.LastRun.Runner != "a" {
		t.Fatal("deferred record not returned")
	}
	if _, err := LoadLastRun(ws); err == nil {
		t.Fatal("deferred record was saved")
	}
	res = Result{}
	keepLastRun(ws, Options{Sync: "none"}, lr, &res)
	if res.LastRun != nil {
		t.Fatal("unsynced run recorded")
	}
	keepLastRun(ws, Options{Sync: "in"}, lr, &res)
	if got, err := LoadLastRun(ws); err != nil || got.Slot != 2 {
		t.Fatalf("record not saved: %v %v", got, err)
	}
}

func TestClassifyExec(t *testing.T) {
	cases := []struct {
		name string
		r    protocol.Result
		want execOutcome
	}{
		{"plain success", protocol.Result{ExitCode: 0}, execCompleted},
		{"plain failure", protocol.Result{ExitCode: 1}, execCompleted},
		{"never started", protocol.Result{ExitCode: 127, StartError: "no space left on device"}, execNotStarted},
		{"canceled, handled SIGTERM and exited 0", protocol.Result{ExitCode: 0, Canceled: true}, execCanceled},
		{"canceled, killed", protocol.Result{ExitCode: 143, Canceled: true}, execCanceled},
		{"a shutdown is never written back, even if also canceled", protocol.Result{ExitCode: 143, Canceled: true, Shutdown: true}, execShutdown},
		{"killed by the VM shutting down", protocol.Result{ExitCode: 143, Interrupted: true, Shutdown: true}, execShutdown},
		{"finished normally during the warning", protocol.Result{ExitCode: 0, Interrupted: true}, execCompleted},
		{"exited 143 itself during the warning", protocol.Result{ExitCode: 143, Interrupted: true}, execCompleted},
	}
	for _, c := range cases {
		if got := classifyExec(c.r); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestShutdownIsALostRunner(t *testing.T) {
	spot := &Lease{Runner: &config.Runner{Type: config.TypeAWSSpot}, InstanceID: "i-1"}
	shutdown := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeShutdown, Message: "the runner is shutting down"}}
	if got := lostRunner(spot, shutdown); got != "i-1" {
		t.Fatalf("a shutdown error on a Spot runner: lost %q, want i-1", got)
	}
	other := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "x"}}
	if got := lostRunner(spot, other); got != "" {
		t.Fatalf("an ordinary helper error counted as a lost runner (%q)", got)
	}
	static := &Lease{Runner: &config.Runner{Type: config.TypeSSH}}
	if got := lostRunner(static, shutdown); got != "" {
		t.Fatalf("a static runner is never replayed (%q)", got)
	}
	if got := shutdownRunner(static); got != "" {
		t.Fatalf("a static runner is never replayed (%q)", got)
	}
}

func TestInterruptedRunIsNeverOnSuccess(t *testing.T) {
	if wantWriteback("on-success", ExitInterrupted) {
		t.Fatal("an interrupted run counted as success for on-success write-back")
	}
	if !wantWriteback("always", ExitInterrupted) {
		t.Fatal("always must still write back what an interrupted run changed")
	}
}

// A container project outside Git: the scaffolded Dockerfile under
// .spotexec/ is synced and the image tag computes, with no `git add`.
func TestImageTagNonGitProject(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".spotexec"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".spotexec/Dockerfile", "app.py"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) }
	p := &config.Project{Container: config.Container{Dockerfile: ".spotexec/Dockerfile"}}
	m, err := manifest.Build(context.Background(), ManifestOptions(root, p))
	if err != nil {
		t.Fatal(err)
	}
	if m.Git {
		t.Fatal("temp dir is inside a git worktree")
	}
	if _, err := imageTag(m, ".spotexec/Dockerfile", "", "linux/amd64", read); err != nil {
		t.Fatalf("imageTag: %v", err)
	}
	// With context = "." the Dockerfile's support files are build inputs too.
	if err := os.WriteFile(filepath.Join(root, ".spotexec", "setup.sh"), []byte("true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.Container.Context = "."
	m, err = manifest.Build(context.Background(), ManifestOptions(root, p))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ByPath[".spotexec/setup.sh"]; !ok {
		t.Fatal("context = \".\": .spotexec/setup.sh not synced")
	}
	p.Container.Context = ""
	// Excluded on purpose: the error names the exclude, not git.
	p.Sync.Exclude = []string{".spotexec/"}
	m, err = manifest.Build(context.Background(), ManifestOptions(root, p))
	if err != nil {
		t.Fatal(err)
	}
	_, err = imageTag(m, ".spotexec/Dockerfile", "", "linux/amd64", read)
	if err == nil || strings.Contains(err.Error(), "git add") || !strings.Contains(err.Error(), "exclude") {
		t.Fatalf("want an exclude error without a git hint, got %v", err)
	}
}

// A Dockerfile a run can't build from fails the run before it acquires a
// runner (an acquire would have booted a VM only to exit 125), and
// Preflight, which spotexec supported uses, finds the same.
func TestRunChecksDockerfileBeforeAcquiring(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".spotexec.toml"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	p := &config.Project{Container: config.Container{Dockerfile: ".spotexec/Missing"}}
	// A runner Acquire can't use: reaching it would fail differently.
	cfg := &config.Config{Global: &config.Global{Runners: map[string]*config.Runner{"box": {Name: "box", Type: "ssh", Host: "nobody@127.0.0.1:1"}}}, Project: p}
	t.Setenv("SPOTEXEC_POOL", "")
	_, err = Run(context.Background(), cfg, ws, Options{Sync: "auto", Writeback: "always", Rep: quietRep(t)})
	if err == nil || !strings.Contains(err.Error(), "not in the synced working set") {
		t.Fatalf("Run: want the Dockerfile refused before acquiring, got %v", err)
	}
	for _, sync := range []string{"auto", "none"} {
		if err := Preflight(context.Background(), p, ws, Options{Sync: sync}); err == nil {
			t.Fatalf("Preflight(%s): a missing Dockerfile passed", sync)
		}
	}
	// exec reuses the image this worktree last built: no Dockerfile needed.
	if err := checkLocal(nil, p, ws, Options{Sync: "none", PreferLast: true}); err != nil {
		t.Fatalf("checkLocal(exec): %v", err)
	}
	// An image needs no local files.
	if err := Preflight(context.Background(), &config.Project{Container: config.Container{Image: "alpine"}}, ws, Options{Sync: "auto"}); err != nil {
		t.Fatalf("Preflight(image): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".spotexec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".spotexec", "Missing"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), p, ws, Options{Sync: "auto"}); err != nil {
		t.Fatalf("Preflight with the Dockerfile present: %v", err)
	}
	// .dockerignore edited after the scan: the upload sends the new bytes,
	// so only the post-sync check (imageTag) may hold the scan to them.
	ignore := filepath.Join(root, ".spotexec", "Missing.dockerignore")
	if err := os.WriteFile(ignore, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Build(context.Background(), ManifestOptions(root, p))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignore, []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkLocal(m, p, ws, Options{Sync: "auto"}); err != nil {
		t.Fatalf("checkLocal refused a .dockerignore edited since the scan: %v", err)
	}
	if _, err := imageTag(m, ".spotexec/Missing", "", "linux/amd64", readFrom(root)); err == nil || !strings.Contains(err.Error(), "changed after it was synced") {
		t.Fatalf("imageTag: want the synced copy enforced, got %v", err)
	}
}

// sx in the home directory refuses to sync it, even with a leftover
// ~/.spotexec.toml.
func TestRunRefusesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".spotexec.toml"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Marked {
		t.Fatal("home resolved as a marked project")
	}
	_, err = Run(context.Background(), nil, ws, Options{Sync: "auto", Writeback: "always"})
	if err == nil || !strings.Contains(err.Error(), "never syncs") {
		t.Fatalf("want a refusal, got %v", err)
	}
	// A home directory that is a Git worktree (dotfiles), from a child.
	if out, err := exec.Command("git", "-C", home, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	child := filepath.Join(home, "notes")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if ws, err = Resolve(child); err != nil {
		t.Fatal(err)
	}
	if !ws.Git || ws.Marked {
		t.Fatalf("git home: git=%v marked=%v, want git and unmarked", ws.Git, ws.Marked)
	}
	if _, err = Run(context.Background(), nil, ws, Options{Sync: "auto", Writeback: "always"}); err == nil || !strings.Contains(err.Error(), "never syncs") {
		t.Fatalf("git home: want a refusal, got %v", err)
	}
}

// Lockfile patterns match only what is synced to the slot: never an
// excluded file, a dependency tree, or a symlink (whose target the runner
// may not have, or which could point outside the worktree).
func TestDepsLockfilesMatchSyncedFilesOnly(t *testing.T) {
	root := t.TempDir()
	for p, c := range map[string]string{
		"package-lock.json":                      "lock",
		"packages/a/package.json":                "a",
		"packages/secret/package.json":           "excluded",
		"node_modules/x/package.json":            "dep",
		"packages/b/node_modules/y/package.json": "dep",
		"outside/package.json":                   "outside",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "packages", "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside/package.json", filepath.Join(root, "packages", "a", "linked.json")); err != nil {
		t.Fatal(err)
	}
	p := &config.Project{}
	p.Sync.Source = "directory"
	p.Sync.Exclude = []string{"packages/secret/"}
	m, err := manifest.Build(context.Background(), ManifestOptions(root, p))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ByPath["packages/a/linked.json"]; !ok {
		t.Fatal("fixture: the symlink should be synced (as a link)")
	}
	got, unmatched, err := DepsLockfiles(m, []string{"package-lock.json", "**/package.json", "packages/a/*.json", "vendor/*.lock"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"outside/package.json", "package-lock.json", "packages/a/package.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(unmatched) != 1 || unmatched[0] != "vendor/*.lock" {
		t.Fatalf("unmatched %v", unmatched)
	}
}

func TestSetupFor(t *testing.T) {
	p := &config.Project{}
	if run, _ := SetupFor(p, Options{Timeout: time.Minute}); run != "" {
		t.Errorf("no [setup]: %q", run)
	}
	p.Setup.Run = "make gen"
	if run, d := SetupFor(p, Options{Timeout: time.Minute}); run != "make gen" || d != time.Minute {
		t.Errorf("default timeout: %q %s", run, d)
	}
	p.Setup.Timeout.Duration = 2 * time.Minute
	if run, d := SetupFor(p, Options{Timeout: time.Minute}); run != "make gen" || d != 2*time.Minute {
		t.Errorf("setup.timeout: %q %s", run, d)
	}
	if run, _ := SetupFor(p, Options{NoSetup: true}); run != "" {
		t.Errorf("--no-setup: %q", run)
	}
}

// A failed setup exits 1 whatever its own code (so 125 and 130 never pass
// for spotexec's own failure or an interrupt), or 124 when it timed out.
func TestSetupFailureExitCode(t *testing.T) {
	p := &config.Project{}
	p.Setup.Run = "make gen"
	for _, code := range []int{1, 7, 125, 130} {
		if got, msg := setupFailure(p, Options{}, protocol.Result{ExitCode: code, SetupFailed: true}); got != 1 || !strings.Contains(msg, fmt.Sprintf("exit %d", code)) {
			t.Errorf("setup exit %d: run exits %d (%s)", code, got, msg)
		}
	}
	got, msg := setupFailure(p, Options{Timeout: 3 * time.Minute}, protocol.Result{ExitCode: 124, TimedOut: true, SetupFailed: true})
	if got != ExitTimeout || !strings.Contains(msg, "exceeded 3m0s") {
		t.Errorf("setup timeout: run exits %d (%s)", got, msg)
	}
}
