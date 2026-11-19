package client

import (
	"os/exec"
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
	if !strings.HasPrefix(upper, "app-") {
		t.Fatalf("RepoID %s: want the lowercase app- prefix", upper)
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
	if err := checkSameSlot(acq, lr, ws, "r", false); err == nil {
		t.Fatal("exec/pull accepted a slot at another path")
	}
	if err := checkSameSlot(acq, lr, ws, "r", true); err != nil {
		t.Fatalf("a syncing run refused: %v", err)
	}
	lr.RemoteWS = acq.Workspace
	if err := checkSameSlot(acq, lr, ws, "r", false); err != nil {
		t.Fatalf("the same slot refused: %v", err)
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
