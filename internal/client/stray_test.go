package client

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
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

	// .git directories a run made below the root: kept only where their
	// directory is ignored here; write-back refuses the rest by name.
	gitDirs := []string{
		"sub/.git",        // goes
		"deep/x/.git",     // goes
		"dist/clone/.git", // a build output's clone (dist/ is ignored): stays
		"linked/.git",     // under a local symlink: goes
		"src/a.go/.git",   // under a synced file: goes
		"sub/.git/x/.git", // inside another .git: goes
		"dist/clone/.Git", // not a .git directory: goes
	}
	got, err = strayFiles(ctx, ws, m, gitDirs)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"deep/x/.git", "dist/clone/.Git", "linked/.git", "src/a.go/.git", "sub/.git", "sub/.git/x/.git"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stray = %v, want %v", got, want)
	}
	fetch := func(f protocol.Fetch) (io.Reader, error) {
		t.Fatalf("fetched %v for .git directories", f.Paths)
		return nil, nil
	}
	wb, err := writeback(ctx, fetch, ws, m, protocol.Result{GitDirs: gitDirs}, "r", nil, nil, WritebackLimits{}, &recordingReporter{})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(wb.Refused)
	if !reflect.DeepEqual(wb.Refused, want) {
		t.Fatalf("refused %v, want %v", wb.Refused, want)
	}
}
