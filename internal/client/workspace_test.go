package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestNormalizeRemote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// Equivalent https, ssh and scp forms share one identity.
		{"https://github.com/team/app.git", "github.com/team/app"},
		{"https://github.com/team/app", "github.com/team/app"},
		{"https://github.com/team/app/", "github.com/team/app"},
		{"ssh://git@github.com/team/app.git", "github.com/team/app"},
		{"git@github.com:team/app.git", "github.com/team/app"},
		{"github.com:team/app", "github.com/team/app"},
		{"https://user:tok@github.com/team/app.git", "github.com/team/app"},
		// The host is case-insensitive; the repository path is not.
		{"https://GitHub.COM/team/app.git", "github.com/team/app"},
		{"git@GITHUB.com:team/App.git", "github.com/team/App"},
		{"https://github.com/Team/App.git", "github.com/Team/App"},
		// An "@" in the path is part of the path, not userinfo.
		{"https://host.example/org/a@b.git", "host.example/org/a@b"},
		{"git@host.example:org/a@b.git", "host.example/org/a@b"},
		{"ssh://git@host.example/org/a@b", "host.example/org/a@b"},
		// Userinfo holding an "@" ends at the authority's last "@".
		{"https://a@b:tok@host.example/org/r.git", "host.example/org/r"},
		{"git@host.example:org/a@b:part.git", "host.example/org/a@b:part"},
		// Bracketed IPv6 hosts, in both forms.
		{"git@[::1]:Org/App.git", "[::1]/Org/App"},
		{"ssh://git@[::1]/Org/App.git", "[::1]/Org/App"},
		{"[::1]:Org/App.git", "[::1]/Org/App"},
		// An explicit port stays in the authority, never in the path.
		{"ssh://git@Host:22/org/repo.git", "host:22/org/repo"},
		{"https://host/22/org/repo.git", "host/22/org/repo"},
		// Home-relative and absolute scp paths stay as distinct as before.
		{"host:~user/repo", "host/~user/repo"},
		{"ssh://host/~user/repo", "host/~user/repo"},
		{"host:/srv/repo", "host//srv/repo"},
		// HTTP drops a fragment; scp keeps "#" as part of the path.
		{"https://host/org/app.git#preview", "host/org/app"},
		{"HTTP://host/org/app#preview", "host/org/app"},
		{"git@host:org/app#preview", "host/org/app#preview"},
		// Local paths keep their case and any "@".
		{"file:///Srv/A@B.git", "/Srv/A@B"},
		{"/Srv/A@B.git", "/Srv/A@B"},
		{"../Repo", "../Repo"},
	} {
		if got := normalizeRemote(c.in); got != c.want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Two repositories whose remote paths differ only in case get distinct
// RepoIDs (#313).
func TestResolveRepoIDKeepsPathCase(t *testing.T) {
	names := map[string]string{}
	id := func(remote string) string {
		t.Helper()
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		w, err := Resolve(dir)
		if err != nil {
			t.Fatal(err)
		}
		names[w.RepoID] = w.RepoName
		return w.RepoID
	}
	lower, upper := id("git@example.invalid:team/app.git"), id("git@example.invalid:team/App.git")
	if lower == upper {
		t.Fatalf("case-distinct remote paths share RepoID %s", lower)
	}
	// A lowercase remote keeps the RepoID earlier versions gave it, so its
	// caches and slots survive the upgrade.
	if lower != "app-2c09ba21" {
		t.Fatalf("lowercase remote RepoID = %s, want app-2c09ba21", lower)
	}
	if !strings.HasPrefix(upper, "app-") || names[upper] != "app" {
		t.Fatalf("RepoID %s, RepoName %q: want the lowercase app name", upper, names[upper])
	}
	if same := id("https://Example.invalid/team/app"); same != lower {
		t.Fatalf("equivalent https remote got RepoID %s, want %s", same, lower)
	}
}

// A last run recorded under another RepoID (an earlier version's
// derivation) never lets exec or pull into the new namespace's slot, even
// once an interrupted sync has claimed it for this worktree.
func TestCheckSameSlotRefusesMovedWorkspace(t *testing.T) {
	ws := &Workspace{RepoID: "app-11111111", WorktreeKey: "wt"}
	lr := &LastRun{Slot: 1, RemoteWS: "/w/repos/app-00000000/slots/slot-01/workspace"}
	acq := protocol.Acquired{Slot: 1, PrevWorktreeKey: "wt", Workspace: "/w/repos/app-11111111/slots/slot-01/workspace"}
	if err := checkSameSlot(acq, lr, ws, "r", false); err == nil || !strings.Contains(err.Error(), "app-00000000") {
		t.Fatalf("exec/pull into another RepoID's slot: err = %v", err)
	}
	if err := checkSameSlot(acq, lr, ws, "r", true); err != nil {
		t.Fatalf("a syncing run refused: %v", err)
	}
	// The same RepoID under a root that resolves differently is the same slot.
	lr.RemoteWS = "/old/root/repos/app-11111111/slots/slot-01/workspace"
	if err := checkSameSlot(acq, lr, ws, "r", false); err != nil {
		t.Fatalf("the same slot refused after a root move: %v", err)
	}
	// A record without a workspace path falls back to the owner check.
	lr.RemoteWS = ""
	if err := checkSameSlot(acq, lr, ws, "r", false); err != nil {
		t.Fatalf("a record without RemoteWS refused: %v", err)
	}
	acq.PrevWorktreeKey = "other"
	if err := checkSameSlot(acq, lr, ws, "r", false); err == nil {
		t.Fatal("exec/pull accepted another worktree's slot")
	}
	acq.Slot = 2
	if err := checkSameSlot(acq, lr, ws, "r", true); err == nil {
		t.Fatal("accepted another slot number")
	}
}

func TestRemoteRepoID(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/w/repos/app-1234abcd/slots/slot-01/workspace", "app-1234abcd"},
		{"/home/u/.spotexec/repos/My.App-1/slots/slot-12/workspace", "My.App-1"},
		{"", ""},
		{"/w/other/app/slots/slot-01/workspace", ""},
		{"/w/repos/app/workspace", ""},
	} {
		if got := remoteRepoID(c.in); got != c.want {
			t.Errorf("remoteRepoID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A subdirectory whose name merely starts with ".." is inside the worktree,
// so Resolve keeps it as the working directory (#324).
func TestResolveKeepsDoubleDotPrefixedCwd(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, rel := range []string{"..build/subdir", "...", "..a/..b", "..build"} {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		w, err := Resolve(dir)
		if err != nil {
			t.Fatal(err)
		}
		if w.Cwd != rel {
			t.Errorf("Resolve(%s).Cwd = %q, want %q", rel, w.Cwd, rel)
		}
	}
}

// Only a whole ".." component leaves the root; names that merely start
// with ".." stay inside it.
func TestCwdIn(t *testing.T) {
	root := filepath.FromSlash("/w/repo")
	for _, c := range []struct{ abs, want string }{
		{"/w/repo", "."},
		{"/w/repo/a/b", "a/b"},
		{"/w/repo/..build", "..build"},
		{"/w/repo/..build/subdir", "..build/subdir"},
		{"/w/repo/...", "..."},
		{"/w", "."},
		{"/w/other", "."},
		{"/w/repo2", "."},
		{"/elsewhere/..x", "."},
		// filepath.Rel fails on a relative path against an absolute root.
		{"..build", "."},
	} {
		if got := cwdIn(root, filepath.FromSlash(c.abs)); got != c.want {
			t.Errorf("cwdIn(%s) = %q, want %q", c.abs, got, c.want)
		}
	}
}
