package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/ui"
)

// fakeRTKPipe puts an rtk on PATH whose filters are: good (a line count),
// silent (prints nothing) and anything else (fails like an unknown filter).
func fakeRTKPipe(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$1 $2" = "pipe -f" ] || exit 9
case "$3" in
good) printf "compact: %s lines" "$(wc -l | tr -d ' ')" ;;
silent) cat >/dev/null ;;
*) cat >/dev/null; echo "unknown filter" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "rtk"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRTKOutput(t *testing.T) {
	fakeRTKPipe(t)
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	errFile, _ := os.CreateTemp(t.TempDir(), "stderr")
	rep, err := ui.New(ui.ModeJSON, errFile, "")
	if err != nil {
		t.Fatal(err)
	}
	warnings := func() string {
		b, _ := os.ReadFile(errFile.Name())
		errFile.Truncate(0)
		errFile.Seek(0, 0)
		return string(b)
	}
	run := func(filter string, failed bool, writes ...string) string {
		var out bytes.Buffer
		ro, err := newRTKOutput(filter, &out, rep)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range writes {
			ro.Write([]byte(w))
		}
		ro.finish(failed)
		return out.String()
	}
	kept := func() []os.DirEntry {
		ents, _ := os.ReadDir(filepath.Join(state, "rtk"))
		return ents
	}
	if got := run("good", false, "a\nb\n", "c\n"); got != "compact: 3 lines\n" {
		t.Fatalf("passed: %q", got)
	}
	if len(kept()) != 0 {
		t.Fatalf("raw output of a passing run kept: %v", kept())
	}
	got := run("good", true, "a\nb\n", "c\n")
	i := strings.Index(got, "[full output: ")
	if !strings.HasPrefix(got, "compact: 3 lines\n") || i < 0 {
		t.Fatalf("failed: %q", got)
	}
	path := strings.TrimSuffix(strings.TrimSpace(got[i+len("[full output: "):]), "]")
	if b, err := os.ReadFile(path); err != nil || string(b) != "a\nb\nc\n" {
		t.Fatalf("raw output %q: %q %v", path, b, err)
	}
	// Nothing printed: nothing to filter, no warning.
	if got := run("nope", true); got != "" || warnings() != "" {
		t.Fatalf("empty output: %q", got)
	}
	// RTK failing, or saying nothing, never costs the output.
	for _, f := range []string{"nope", "silent"} {
		if got := run(f, true, "a\nb\n", "c\n"); got != "a\nb\nc\n" {
			t.Fatalf("%s: %q", f, got)
		}
		if w := warnings(); !strings.Contains(w, `"RTK_FILTER"`) || !strings.Contains(w, "rtk pipe -f "+f+" failed") {
			t.Fatalf("%s: no warning: %q", f, w)
		}
	}
	// Too much to hold: what was held, then the rest, unfiltered, in order.
	old := rtkMaxSpool
	rtkMaxSpool = 4
	defer func() { rtkMaxSpool = old }()
	if got := run("good", true, "a\nb\n", "c\nd\n", "e\n"); got != "a\nb\nc\nd\ne\n" {
		t.Fatalf("overflow: %q", got)
	}
	if w := warnings(); !strings.Contains(w, "can't hold the output") {
		t.Fatalf("overflow: no warning: %q", w)
	}
}

func TestRTKOutputNeedsRTK(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := newRTKOutput("good", &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want an error without rtk on PATH")
	}
}

func TestPruneRTK(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * rtkActive)
	for i := 0; i < rtkKeep+5; i++ {
		p := filepath.Join(dir, strings.Repeat("x", i+1))
		os.WriteFile(p, nil, 0o600)
		// All old; the active file below is the newest, so the six oldest go.
		os.Chtimes(p, old.Add(time.Duration(i)*time.Second), old.Add(time.Duration(i)*time.Second))
	}
	active := filepath.Join(dir, "active")
	os.WriteFile(active, nil, 0o600) // a run still writing: recent, never pruned
	pruneRTK(dir)
	ents, _ := os.ReadDir(dir)
	if len(ents) != rtkKeep {
		t.Fatalf("kept %d, want %d", len(ents), rtkKeep)
	}
	for _, e := range ents {
		if len(e.Name()) <= 6 && e.Name() != "active" {
			t.Fatalf("kept an oldest file: %s", e.Name())
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatal("pruned an active file")
	}
}
