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
	// Only the fixture's own Git settings apply.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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
	// run returns the scans, and each part's manifest events and error.
	run := func(args ...string) (scans int, manifests map[string]int, errs map[string]string) {
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
		manifests, errs = map[string]int{}, map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) != nil {
				t.Fatalf("not NDJSON: %q", line)
			}
			task, _ := ev["task"].(string)
			switch ev["event"] {
			case "manifest":
				manifests[task]++
			case "error":
				if task != "all" {
					errs[task], _ = ev["message"].(string)
				}
			}
		}
		return
	}
	synced := "not in the synced working set"
	s, m, e := run("all")
	if s != 1 || len(m) != 2 || m["x"] != 1 || m["y"] != 1 {
		t.Errorf("x, y and an unsynced z: %d scans, manifests %v; want 1 scan, one manifest each for x and y", s, m)
	}
	if len(e) != 3 || !strings.Contains(e["x"], synced) || !strings.Contains(e["y"], synced) || e["z"] == "" || strings.Contains(e["z"], synced) {
		t.Errorf("part errors: %v", e)
	}
	if s, m, e := run("--sync", "none", "all"); s != 0 || len(m) != 0 || len(e) != 3 {
		t.Errorf("--sync none: %d scans, manifests %v, errors %v; want none, none, 3", s, m, e)
	}
}
