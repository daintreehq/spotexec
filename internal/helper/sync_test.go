package helper

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Names no client can use (a backslash, a ".Git" directory) never go back,
// but a run that creates them still leaves them pending, and the next sync
// removes them like any other file the run left behind.
func TestSyncRemovesFilesNoClientCanUse(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.workspace, "keep.txt", "synced\n")
	s.loadStored()
	pre := s.takeSnapshot(nil)

	// What the run creates.
	odd := []string{`docs/.Git/config`, `docs/a\b.txt`, `x\y/z.txt`}
	for _, p := range odd {
		writeFile(t, s.workspace, p, "made remotely\n")
	}
	writeFile(t, s.workspace, "new.txt", "made remotely\n")

	_, _, created, _, _, unreported := s.diff(pre, s.takeSnapshot(nil))
	if len(created) != 1 || created[0].Path != "new.txt" {
		t.Fatalf("created = %v, want only new.txt", created)
	}
	sort.Strings(unreported)
	if !reflect.DeepEqual(unreported, odd) {
		t.Fatalf("unreported = %v, want %v", unreported, odd)
	}
	s.state.Pending = s.pendingAfter(created, nil, unreported)
	want := append([]string{"new.txt"}, odd...)
	sort.Strings(want)
	if !reflect.DeepEqual(s.state.Pending, want) {
		t.Fatalf("pending = %v, want %v", s.state.Pending, want)
	}

	// The next sync: the client lists every pending path it doesn't ignore.
	if err := s.syncManifest(protocol.Manifest{Remove: want}); err != nil {
		t.Fatal(err)
	}
	for _, p := range want {
		if _, err := os.Lstat(filepath.Join(s.workspace, p)); !os.IsNotExist(err) {
			t.Errorf("%s still in the slot (err %v)", p, err)
		}
	}
	for _, d := range []string{"docs", `x\y`} {
		if _, err := os.Lstat(filepath.Join(s.workspace, d)); !os.IsNotExist(err) {
			t.Errorf("empty directory %s left behind (err %v)", d, err)
		}
	}
	if len(s.state.Pending) != 0 {
		t.Fatalf("still pending: %v", s.state.Pending)
	}
}

// A pending path the run itself removed is dropped, whatever its name.
func TestPendingAfterDropsGoneOddNames(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	s.loadStored()
	writeFile(t, s.workspace, `still\here`, "x")
	s.state.Pending = []string{`gone\now`, `still\here`, "docs/.GIT/gone"}
	if got := s.pendingAfter(nil, nil, nil); !reflect.DeepEqual(got, []string{`still\here`}) {
		t.Fatalf("pending = %v, want [still\\here]", got)
	}
}

// A seeded .git is never reached through a pending path.
func TestSyncNeverRemovesInsideGit(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.workspace, ".git/HEAD", "ref: refs/heads/main\n")
	s.state.Pending = []string{".git/HEAD"}
	if err := s.syncManifest(protocol.Manifest{Remove: []string{".git/HEAD"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.workspace, ".git/HEAD")); err != nil {
		t.Fatalf(".git/HEAD removed: %v", err)
	}
	if len(s.state.Pending) != 0 {
		t.Fatalf("a path that can't be a run's file stays pending: %v", s.state.Pending)
	}
}
