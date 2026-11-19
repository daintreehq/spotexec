package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/manifest"
)

// A checkout whose name ends in a space resolves to itself, never to the
// sibling Git would name if its output were trimmed: sync lists its files
// and write-back lands in it.
func TestResolveKeepsTrailingSpaceCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot end in a space")
	}
	parent := t.TempDir()
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		parent = r
	}
	plain, spaced := filepath.Join(parent, "checkout"), filepath.Join(parent, "checkout ")
	for dir, marker := range map[string]string{plain: "plain.txt", spaced: "spaced.txt"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "init", "-q")
		writeFile(t, dir, marker, marker+"\n")
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-qm", "base")
	}

	ws, err := Resolve(filepath.Join(spaced))
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != spaced || !ws.Git {
		t.Fatalf("Resolve root = %q (git %v), want %q", ws.Root, ws.Git, spaced)
	}
	for name, d := range map[string]string{"GitDir": ws.GitDir, "CommonGitDir": ws.CommonGitDir} {
		if !strings.HasPrefix(d, spaced+string(filepath.Separator)) {
			t.Errorf("%s = %q, want it inside %q", name, d, spaced)
		}
	}

	m, err := manifest.Build(context.Background(), manifest.Options{Root: ws.Root})
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	for _, e := range m.Entries {
		synced = append(synced, e.Path)
	}
	if got := strings.Join(synced, " "); got != "spaced.txt" {
		t.Errorf("synced %q, want spaced.txt only", got)
	}

	f := wbFixture{ws: ws, m: m}
	f.apply(t, nil, map[string]string{"out.txt": "result\n"})
	if got := readOr(filepath.Join(spaced, "out.txt")); got != "result\n" {
		t.Errorf("write-back into the requested checkout: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(plain, "out.txt")); err == nil {
		t.Error("write-back landed in the sibling checkout")
	}
}
