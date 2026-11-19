package helperbin

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeTools writes a `go` that "builds" a helper by writing its GOOS/GOARCH
// to the -o path, and, when gzipScript is set, a `gzip` running it, into a
// directory that goes first on PATH for `make helpers`. The scripts find that
// directory in $TOOLS and the real gzip in $REAL_GZIP. Each fake go call
// appends its target to $TOOLS/go.ran; each fake gzip call that isn't a
// stream test leaves $TOOLS/gzip.ran, so a test can tell the stub ran.
func fakeTools(t *testing.T, gzipScript string) string {
	t.Helper()
	bin := t.TempDir()
	goScript := `#!/bin/sh
out=
while [ $# -gt 0 ]; do
  [ "$1" = -o ] && { out=$2; shift; }
  shift
done
[ -n "$out" ] || { echo "fake go: no -o" >&2; exit 2; }
printf '%s/%s\n' "$GOOS" "$GOARCH" >> "$TOOLS/go.ran"
printf 'fake-helper-%s-%s\n' "$GOOS" "$GOARCH" > "$out"
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if gzipScript != "" {
		// Testing a stream is left to the real gzip, so a compressor that
		// lies about success is caught by the check.
		script := `#!/bin/sh
for a; do [ "$a" = -t ] && exec "$REAL_GZIP" "$@"; done
echo >> "$TOOLS/gzip.ran"
` + gzipScript
		if err := os.WriteFile(filepath.Join(bin, "gzip"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// makeHelpers runs the Makefile's helpers target into embed with the fake
// tools in bin first on PATH, plus env.
func makeHelpers(t *testing.T, bin, embed, targets string, env ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	realGzip, err := exec.LookPath("gzip")
	if err != nil {
		t.Skip("gzip not installed")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Read here, the Makefile is an input of this test, so a change to it
	// invalidates a cached pass.
	if mk, err := os.ReadFile(filepath.Join(root, "Makefile")); err != nil || !bytes.Contains(mk, []byte("\nhelpers:")) {
		t.Fatalf("no helpers target in %s/Makefile: %v", root, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "-s", "-C", root, "helpers",
		"HELPER_TARGETS="+targets, "EMBED_DIR="+embed, "VERSION=test")
	// Nothing from an enclosing make.
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TOOLS="+bin, "REAL_GZIP="+realGzip,
		"MAKEFLAGS=", "MFLAGS=", "GNUMAKEFLAGS=", "MAKELEVEL=", "MAKEFILES=")
	cmd.Env = append(cmd.Env, env...)
	// A timeout kills the whole tree, so a stalled tool can't hold the
	// output pipe open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("make helpers timed out:\n%s", out)
	}
	return string(out), err
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// Each helper is a gzip stream of its build, even when the environment
// carries options for gzip that would make it decompress or pass its input
// through.
func TestMakeHelpersCompresses(t *testing.T) {
	embed := t.TempDir()
	out, err := makeHelpers(t, fakeTools(t, ""), embed, "linux/amd64 darwin/arm64", "GZIP=-d -f")
	if err != nil {
		t.Fatalf("make helpers: %v\n%s", err, out)
	}
	for _, p := range []string{"linux/amd64", "darwin/arm64"} {
		if !strings.Contains(out, "helper "+p+"\n") {
			t.Errorf("no success line for %s in %q", p, out)
		}
		goos, goarch, _ := strings.Cut(p, "/")
		if got, want := gunzip(t, filepath.Join(embed, Name(goos, goarch))), "fake-helper-"+goos+"-"+goarch+"\n"; got != want {
			t.Errorf("%s decompresses to %q, want %q", p, got, want)
		}
	}
	if got := listDir(t, embed); len(got) != 2 {
		t.Errorf("embed dir holds %q, want only the two helpers", got)
	}
}

// A compressor that fails, even after writing part or all of its output, or
// that claims success for something that isn't gzip, fails the build, prints
// no success line, leaves the helper already there untouched and leaves
// nothing behind for go:embed to pick up.
func TestMakeHelpersFailsWhenCompressionFails(t *testing.T) {
	for name, script := range map[string]string{
		"fails":           "exit 1\n",
		"partial output":  "printf '\\037\\213\\010'\nexit 1\n",
		"killed":          "printf '\\037\\213\\010'\nkill -9 $$\n",
		"invalid success": "printf 'not gzip'\n",
		// A whole, valid stream doesn't make up for the failure status.
		"valid but failing": "\"$REAL_GZIP\" \"$@\"\nexit 1\n",
		// A stream another format's decompressor accepts isn't gzip.
		"wrong format": "for a; do f=$a; done\nbzip2 -c \"$f\" 2>/dev/null || cat \"$f\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			embed := t.TempDir()
			old := []byte("previous helper")
			final := filepath.Join(embed, Name("linux", "amd64"))
			if err := os.WriteFile(final, old, 0o644); err != nil {
				t.Fatal(err)
			}
			bin := fakeTools(t, script)
			out, err := makeHelpers(t, bin, embed, "linux/amd64")
			if _, serr := os.Stat(filepath.Join(bin, "gzip.ran")); serr != nil {
				t.Fatalf("the fake gzip never ran:\n%s", out)
			}
			if err == nil {
				t.Fatalf("make helpers succeeded with a failing compressor:\n%s", out)
			}
			if strings.Contains(out, "helper linux/amd64\n") {
				t.Errorf("success line printed for a failed helper: %q", out)
			}
			if b, err := os.ReadFile(final); err != nil || !bytes.Equal(b, old) {
				t.Errorf("existing helper changed: %q, %v", b, err)
			}
			if got := listDir(t, embed); len(got) != 1 {
				t.Errorf("embed dir holds %q, want only the previous helper", got)
			}
		})
	}
}

// A failure on a later target stops the build there: no later helper is
// built, and the earlier one stays published.
func TestMakeHelpersStopsAtFirstFailure(t *testing.T) {
	embed := t.TempDir()
	// Fails for the second helper only: its input names arm64.
	script := `for a; do f=$a; done
grep -q arm64 "$f" && exit 1
exec "$REAL_GZIP" "$@"
`
	bin := fakeTools(t, script)
	out, err := makeHelpers(t, bin, embed, "linux/amd64 linux/arm64 darwin/amd64")
	if err == nil {
		t.Fatalf("make helpers succeeded:\n%s", out)
	}
	if !strings.Contains(out, "helper linux/amd64\n") || strings.Contains(out, "helper linux/arm64\n") || strings.Contains(out, "darwin/amd64") {
		t.Errorf("output %q, want only linux/amd64 to succeed", out)
	}
	if b, _ := os.ReadFile(filepath.Join(bin, "go.ran")); string(b) != "linux/amd64\nlinux/arm64\n" {
		t.Errorf("built %q, want linux/amd64 and linux/arm64 only", b)
	}
	if got := listDir(t, embed); len(got) != 1 || got[0] != Name("linux", "amd64") {
		t.Errorf("embed dir holds %q, want only the linux/amd64 helper", got)
	}
}

// A directory where a helper belongs fails the build rather than taking the
// helper inside it.
func TestMakeHelpersRefusesDirectoryDestination(t *testing.T) {
	embed := t.TempDir()
	final := filepath.Join(embed, Name("linux", "amd64"))
	if err := os.Mkdir(final, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := makeHelpers(t, fakeTools(t, ""), embed, "linux/amd64")
	if err == nil || !strings.Contains(out, "is a directory") || strings.Contains(out, "helper linux/amd64\n") {
		t.Fatalf("make helpers: %v\n%s", err, out)
	}
	if got := listDir(t, final); len(got) != 0 {
		t.Errorf("%s holds %q", final, got)
	}
	if got := listDir(t, embed); len(got) != 1 {
		t.Errorf("embed dir holds %q, want only the directory", got)
	}
}
