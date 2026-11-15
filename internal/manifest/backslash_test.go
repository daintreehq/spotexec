package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// A backslash is a legal filename byte on macOS and Linux: such files sync
// like any other, tracked, untracked or outside Git (#92).
func TestBackslashNamesSync(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip(`\ separates paths here`)
	}
	for _, source := range []string{"git", "directory"} {
		root := t.TempDir()
		writeTree(t, root, "src/a.txt", `src/loc\al.txt`, `src/new\file.txt`, `odd\dir/x.txt`)
		if source == "git" {
			gitIn(t, root, "init", "-q")
			gitIn(t, root, "add", "src/a.txt", `src/loc\al.txt`)
		}
		opts := Options{Root: root}
		if source == "directory" {
			opts.Source = "directory"
		}
		m, err := Build(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := paths(m), `odd\dir/x.txt src/a.txt src/loc\al.txt src/new\file.txt`; got != want {
			t.Errorf("source %s: synced %q, want %q", source, got, want)
		}
	}
}
