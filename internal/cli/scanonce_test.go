package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitProject is project in a Git repository holding a few committed files.
func gitProject(t *testing.T, toml string) string {
	t.Helper()
	dir := project(t, toml)
	for i, f := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

// A sharded task scans the worktree once, however many shards sync it
// (#275). A lockfile missing here stops every shard after the scan, before
// any runner is contacted.
func TestShardedTaskScansTheWorktreeOnce(t *testing.T) {
	dir := gitProject(t, `[deps]
install = "true"
cache_dirs = ["node_modules"]
lockfiles = ["package-lock.json"]
[tasks.unit]
run = "true"
shards = 4
[tasks.solo]
run = "true"
`)
	t.Setenv("HOME", t.TempDir())
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	scans := func() int {
		b, _ := os.ReadFile(trace)
		os.Remove(trace)
		n := 0
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "ls-files") && strings.Contains(l, " -s ") {
				n++
			}
		}
		return n
	}
	code, stderr := runMain(t, "sx", "-C", dir, "--json", "solo")
	if code == 0 || !strings.Contains(stderr, "package-lock.json") {
		t.Fatalf("solo: exit %d, stderr %q", code, stderr)
	}
	one := scans()
	if one == 0 {
		t.Fatalf("a single run's scan left no trace")
	}
	code, stderr = runMain(t, "sx", "-C", dir, "--json", "unit")
	if code == 0 {
		t.Fatalf("unit: exit %d, stderr %q", code, stderr)
	}
	if n := scans(); n != one {
		t.Errorf("4 shards listed the index %d times; one run lists it %d", n, one)
	}
	// Every shard still stops on the missing lockfile, with its own error.
	failed := 0
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			t.Fatalf("not NDJSON: %q", line)
		}
		if ev["event"] == "error" && ev["shard"] != nil && strings.Contains(ev["message"].(string), "package-lock.json") {
			failed++
		}
	}
	if failed != 4 {
		t.Errorf("%d shards reported the missing lockfile, want 4: %q", failed, stderr)
	}
}

// Parts that sync share the scan; one that doesn't sync never scans, and
// with --sync none nothing does. (A Dockerfile missing here stops every
// part before any runner is contacted.)
func TestParallelTaskScansOnlyForItsSyncedParts(t *testing.T) {
	dir := gitProject(t, `[container]
dockerfile = ".spotexec/Missing"
[tasks.x]
run = "true"
[tasks.y]
run = "true"
sync = "in"
[tasks.z]
run = "true"
sync = "none"
[tasks.all]
parallel = ["x", "y", "z"]
`)
	t.Setenv("HOME", t.TempDir())
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	run := func(args ...string) (scans, manifests, errs int) {
		t.Helper()
		code, stderr := runMain(t, "sx", append([]string{"-C", dir, "--json"}, args...)...)
		if code == 0 {
			t.Fatalf("%v: exit 0, stderr %q", args, stderr)
		}
		b, _ := os.ReadFile(trace)
		os.Remove(trace)
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "ls-files") && strings.Contains(l, " -s ") {
				scans++
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) != nil {
				t.Fatalf("not NDJSON: %q", line)
			}
			switch ev["event"] {
			case "manifest":
				manifests++
			case "error":
				if ev["task"] != nil && ev["task"] != "all" {
					errs++
				}
			}
		}
		return
	}
	if s, m, e := run("all"); s != 1 || m != 2 || e != 3 {
		t.Errorf("x, y and an unsynced z: %d scans, %d manifests, %d part errors; want 1, 2, 3", s, m, e)
	}
	if s, m, e := run("--sync", "none", "all"); s != 0 || m != 0 || e != 3 {
		t.Errorf("--sync none: %d scans, %d manifests, %d part errors; want 0, 0, 3", s, m, e)
	}
}
