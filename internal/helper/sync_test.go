package helper

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// A name no client can use (a ".Git" directory) never goes back, but a run
// that creates it still leaves it pending, and the next sync removes it like
// any other file the run left behind. A backslash name is an ordinary created
// file (#92): it is reported, and goes the same way if it doesn't come back.
func TestSyncRemovesFilesNoClientCanUse(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.workspace, "keep.txt", "synced\n")
	writeFile(t, s.workspace, `old\name`, "there before the run\n")
	s.loadStored()
	pre := s.takeSnapshot(nil)

	// What the run creates.
	odd := []string{`docs/.Git/config`}
	reported := []string{`docs/a\b.txt`, "new.txt", `x\y/z.txt`}
	for _, p := range append(append([]string{}, odd...), reported...) {
		writeFile(t, s.workspace, p, "made remotely\n")
	}

	_, _, created, _, _, unreported := s.diff(pre, s.takeSnapshot(nil))
	var got []string
	for _, c := range created {
		got = append(got, c.Path)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, reported) {
		t.Fatalf("created = %v, want %v", got, reported)
	}
	sort.Strings(unreported)
	if !reflect.DeepEqual(unreported, odd) {
		t.Fatalf("unreported = %v, want %v", unreported, odd)
	}
	s.state.Pending = s.pendingAfter(created, nil, unreported)
	want := append(append([]string{}, reported...), odd...)
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
	if _, err := os.Lstat(filepath.Join(s.workspace, `old\name`)); err != nil {
		t.Errorf("a file the run didn't touch was removed: %v", err)
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

// A pending path the run itself removed is dropped, whatever its name, and
// so is one that can never be a run's file.
func TestPendingAfterDropsGoneOddNames(t *testing.T) {
	s := testSession(t, t.TempDir())
	if err := s.acquire(protocol.Acquire{RepoID: "r", MaxSlots: 1}); err != nil {
		t.Fatal(err)
	}
	s.loadStored()
	writeFile(t, s.workspace, `still\here`, "x")
	writeFile(t, s.workspace, ".git/HEAD", "x")
	// .git/HEAD and ../x exist too, but are never a run's file: dropped.
	s.state.Pending = []string{`gone\now`, `still\here`, "docs/.GIT/gone", ".git/HEAD", "../x"}
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
