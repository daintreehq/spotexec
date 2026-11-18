package client

import (
	"context"
	"errors"
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
	"github.com/daintreehq/spotexec/internal/provider"
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
	broken := imageBuildError(fmt.Errorf("container image: %w", &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeImageBuildFailed, Message: "x"}}))
	if got := lostRunner(spot, broken); got != "" {
		t.Fatalf("a broken Dockerfile counted as a lost runner (%q)", got)
	}
	static := &Lease{Runner: &config.Runner{Type: config.TypeSSH}}
	if got := lostRunner(static, shutdown); got != "" {
		t.Fatalf("a static runner is never replayed (%q)", got)
	}
	if got := shutdownRunner(static); got != "" {
		t.Fatalf("a static runner is never replayed (%q)", got)
	}
}

func TestConflictExit(t *testing.T) {
	conflicted := []string{"src/a.js"}
	for _, c := range []struct {
		name string
		res  Result
		on   bool
		want int
	}{
		{"opt-in off keeps the command's 0", Result{Conflicts: conflicted}, false, 0},
		{"opt-in, no conflict", Result{}, true, 0},
		{"opt-in, a successful command whose changes didn't all land", Result{Conflicts: conflicted}, true, ExitWritebackConflict},
		{"a failed command keeps its own code", Result{ExitCode: 2, Conflicts: conflicted}, true, 2},
		{"interrupted stays 130", Result{ExitCode: ExitInterrupted, Canceled: true, Conflicts: conflicted}, true, ExitInterrupted},
		{"timed out stays 124", Result{ExitCode: ExitTimeout, TimedOut: true, Conflicts: conflicted}, true, ExitTimeout},
		{"a canceled 0 is never promoted", Result{Canceled: true, Conflicts: conflicted}, true, 0},
		{"a timed-out 0 is never promoted", Result{TimedOut: true, Conflicts: conflicted}, true, 0},
		{"a setup failure's 0 is never promoted", Result{SetupFailed: true, Conflicts: conflicted}, true, 0},
		{"a setup failure keeps its code", Result{ExitCode: 1, SetupFailed: true, Conflicts: conflicted}, true, 1},
	} {
		if got := conflictExit(c.res, c.on); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
}

func TestOOMExit(t *testing.T) {
	for _, c := range []struct {
		name string
		res  Result
		want int
	}{
		{"no OOM keeps the command's code", Result{ExitCode: 1}, 1},
		{"a failed command whose process was OOM killed", Result{ExitCode: 1, OOMKilled: true}, ExitOOM},
		{"the command killed itself", Result{ExitCode: ExitOOM, OOMKilled: true}, ExitOOM},
		{"a failed [setup] run", Result{ExitCode: 1, OOMKilled: true, SetupFailed: true}, ExitOOM},
		{"a command that still succeeded keeps 0", Result{OOMKilled: true}, 0},
		{"timed out stays 124", Result{ExitCode: ExitTimeout, TimedOut: true, OOMKilled: true}, ExitTimeout},
		{"interrupted stays 130", Result{ExitCode: ExitInterrupted, Canceled: true, OOMKilled: true}, ExitInterrupted},
	} {
		if got := oomExit(c.res); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
}

// A run's peak memory is kept for the next run of its workload to
// reserve; one OOM killed in the pool is kept at the pool's cap, so the
// next run starts only once nothing runs beside it (#294).
func TestRecordRunMemory(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := &config.Runner{Name: "aws", Type: config.TypeAWSSpot}
	recordRunMemory(r, "tc", protocol.Result{PeakMemoryMB: 7000})
	if got := provider.RunMemory(r, "tc"); got != 7000 {
		t.Fatalf("expected %d MiB, want 7000", got)
	}
	recordRunMemory(r, "tc", protocol.Result{ExitCode: 1, OOMKilled: true, MemoryLimitMB: 29388, PeakMemoryMB: 6000})
	if got := provider.RunMemory(r, "tc"); got != 29388 {
		t.Fatalf("after an OOM kill in the pool: %d MiB, want the pool's 29388", got)
	}
	recordRunMemory(r, "other", protocol.Result{ExitCode: 137, OOMKilled: true})
	if got := provider.RunMemory(r, "other"); got != 0 {
		t.Fatalf("an OOM kill outside the pool, unmeasured: %d MiB", got)
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

// A Dockerfile in a project subdirectory: the build context there follows
// the usual rules, so what Git ignores (a secret, dist/) stays local, while
// the Dockerfile and the context's .dockerignore sync even when ignored, and
// the image tag reads the synced rules.
func TestSubdirDockerfileSyncsOnlyBuildInputs(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for f, body := range map[string]string{
		"services/api/Dockerfile":    "FROM scratch\nCOPY main.go /\n",
		"services/api/.dockerignore": "*.md\n",
		"services/api/main.go":       "package main\n",
		"services/api/README.md":     "# api\n",
		"services/api/secrets.json":  "{}\n",
		"services/api/dist/app.js":   "1\n",
		".gitignore":                 "Dockerfile\n.dockerignore\nsecrets.json\ndist/\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &config.Project{Container: config.Container{Dockerfile: "services/api/Dockerfile"}}
	read := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) }
	tag := func() string {
		t.Helper()
		m, err := manifest.Build(context.Background(), ManifestOptions(root, p))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range m.Entries {
			got = append(got, e.Path)
		}
		// README.md is project source: it syncs though .dockerignore
		// keeps it out of the build.
		if want := ".gitignore services/api/.dockerignore services/api/Dockerfile services/api/README.md services/api/main.go"; strings.Join(got, " ") != want {
			t.Fatalf("synced %q, want %q", strings.Join(got, " "), want)
		}
		tag, err := imageTag(m, "services/api/Dockerfile", "", "linux/amd64", read)
		if err != nil {
			t.Fatalf("imageTag: %v", err)
		}
		return tag
	}
	write := func(f, body string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first := tag()
	write("services/api/README.md", "# api, edited\n")
	if got := tag(); got != first {
		t.Fatal("editing a dockerignored file changed the image tag: the synced .dockerignore wasn't applied")
	}
	write("services/api/main.go", "package main // edited\n")
	if tag() == first {
		t.Fatal("editing a build input left the image tag unchanged")
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
	if err := checkLocal(context.Background(), nil, p, ws, Options{Sync: "none", PreferLast: true}); err != nil {
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
	if err := checkLocal(context.Background(), m, p, ws, Options{Sync: "auto"}); err != nil {
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

// A spare-capacity probe never teaches the duration model.
func TestSpareRequestHasNoTask(t *testing.T) {
	spare := spareRequest(AcquireRequest{Task: "t"}, "i-1", nil)
	if spare.Task != "" || !spare.Exclude["i-1"] {
		t.Fatalf("spare request %+v", spare)
	}
}

// #250: an error the helper reported during execution names that failure,
// code once; only a runner that went away is a lost runner.
func TestExecFailureNamesHelperErrors(t *testing.T) {
	image := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeImageFailed, Message: "can't read the users of image x: docker create: signal: killed"}}
	queue := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeQueueTimeout, Message: "no slot"}}
	shutdown := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeShutdown, Message: "the runner is shutting down"}}
	conn := &transport.ConnError{Target: "helper", Msg: "runner did not confirm cancellation"}
	garbled := errors.New("invalid character 'x' looking for beginning of value")
	for _, c := range []struct {
		name string
		err  error
		dead bool
		want string
	}{
		{"helper error", image, false, "aws#3: IMAGE_FAILED: can't read the users of image x: docker create: signal: killed"},
		{"helper error, session since gone", image, true, "aws#3: IMAGE_FAILED: can't read"},
		{"wrapped helper error", fmt.Errorf("exec: %w", image), false, "aws#3: exec: IMAGE_FAILED: can't read"},
		{"queue timeout", queue, false, "aws#3: QUEUE_TIMEOUT: no slot"},
		{"shutdown", shutdown, false, "lost runner aws during execution: RUNNER_SHUTTING_DOWN"},
		{"connection", conn, false, "lost runner aws during execution: " + conn.Error()},
		{"unreadable reply", garbled, false, "aws#3: unreadable reply from the runner during execution: invalid character"},
		{"unreadable reply, session gone", garbled, true, "lost runner aws during execution: invalid character"},
	} {
		err := execFailure("aws#3", "aws", c.err, c.dead)
		if got := err.Error(); !strings.HasPrefix(got, c.want) || strings.Count(got, protocol.CodeImageFailed) > 1 {
			t.Errorf("%s: got %q, want it to start %q", c.name, got, c.want)
		}
		if !errors.Is(err, c.err) {
			t.Errorf("%s: the cause is no longer wrapped", c.name)
		}
	}
}
