package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/daintreehq/spotexec/internal/manifest"
)

func TestStrayFilesKeepsIgnoredOutputs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitc("init", "-q")
	write := func(rel, s string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "dist/\n")
	write("src/a.go", "package a\n")
	write("gen", "now a file\n")
	if err := os.Symlink("src", filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := manifest.Build(ctx, manifest.Options{Root: dir, Protect: []string{".cache"}})
	if err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{Root: dir, Git: true}
	pending := []string{
		"src/a.go",         // now synced: the sync overwrites it
		"extra_test.go",    // created remotely, never written back: goes
		"src/gen/x.go",     // likewise
		"dist/app.js",      // gitignored build output: stays
		".cache/blob",      // a declared cache dir: stays
		".git/hooks/pre-x", // can never be ours: goes
		"../escape",        // not a valid path: goes (the helper checks it too)
		"gen/out.go",       // a directory remotely, a synced file here: goes
		"linked/x.go",      // under a local symlink (git refuses to judge it): goes
	}
	got, err := strayFiles(ctx, ws, m, pending)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"../escape", ".git/hooks/pre-x", "extra_test.go", "gen/out.go", "linked/x.go", "src/gen/x.go"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stray = %v, want %v", got, want)
	}
	if got, _ := strayFiles(ctx, ws, m, nil); len(got) != 0 {
		t.Fatalf("nothing pending, but stray = %v", got)
	}
}
