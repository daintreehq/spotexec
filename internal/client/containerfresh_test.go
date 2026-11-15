package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type warnRec struct {
	mu    sync.Mutex
	warns []string
}

func (r *warnRec) Event(string, map[string]any) {}
func (r *warnRec) Status(string, ...any)        {}
func (r *warnRec) Clear()                       {}
func (r *warnRec) Done(string, bool)            {}
func (r *warnRec) PartDone(string, bool)        {}
func (r *warnRec) IsJSON() bool                 { return false }
func (r *warnRec) Warn(code, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warns = append(r.warns, code+": "+fmt.Sprintf(format, args...))
}

func (r *warnRec) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.warns
	r.warns = nil
	return w
}

func TestContainerFreshWarnsOncePerWorktree(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	rep := &warnRec{}
	ws := &Workspace{WorktreeKey: "a"}

	// A plain run picks any slot: nothing to warn about, and it doesn't
	// use up the warning.
	warnContainerFresh(ws, Options{Command: "pnpm test", Rep: rep}, ".spotexec/Dockerfile")
	if w := rep.take(); len(w) != 0 {
		t.Fatalf("plain run warned: %v", w)
	}
	warnContainerFresh(ws, Options{Command: "cat /etc/hosts", PreferLast: true, Rep: rep}, ".spotexec/Dockerfile")
	w := rep.take()
	if len(w) != 1 || !strings.HasPrefix(w[0], "CONTAINER_FRESH: ") || !strings.Contains(w[0], ".spotexec/Dockerfile") {
		t.Fatalf("first exec: %v", w)
	}
	for _, o := range []Options{{Command: "cat /etc/hosts", PreferLast: true}, {Command: "pnpm test", SameSlot: true}} {
		o.Rep = rep
		warnContainerFresh(ws, o, ".spotexec/Dockerfile")
		if w := rep.take(); len(w) != 0 {
			t.Fatalf("repeated warning for %+v: %v", o, w)
		}
	}
	// Another worktree hasn't seen it.
	warnContainerFresh(&Workspace{WorktreeKey: "b"}, Options{Command: "ls", SameSlot: true, Rep: rep}, "")
	if w := rep.take(); len(w) != 1 || !strings.Contains(w[0], "the image") {
		t.Fatalf("second worktree: %v", w)
	}
}

func TestContainerFreshWarnsOnEveryPackageInstall(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	rep := &warnRec{}
	ws := &Workspace{WorktreeKey: "a"}
	for _, o := range []Options{
		{Command: "sudo apt-get install -y libvips-dev", SameSlot: true},
		{Command: "apk add jq", PreferLast: true},
		{Command: "dnf -y install git && make"}, // any run: packages never outlive it
	} {
		o.Rep = rep
		warnContainerFresh(ws, o, ".spotexec/Dockerfile")
		if w := rep.take(); len(w) != 1 || !strings.Contains(w[0], "installs system packages") {
			t.Fatalf("%q: %v", o.Command, w)
		}
	}
	// Having been told, the next read-only exec isn't told again.
	warnContainerFresh(ws, Options{Command: "jq --version", PreferLast: true, Rep: rep}, "")
	if w := rep.take(); len(w) != 0 {
		t.Fatalf("warned after an install already did: %v", w)
	}
}

func TestContainerFreshConcurrentClaimsHaveOneWinner(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	ws := &Workspace{WorktreeKey: "a"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimContainerFresh(ws) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d winners", wins)
	}
}

func TestInstallsSystemPackages(t *testing.T) {
	yes := []string{
		"apt-get install -y curl",
		"sudo apt install curl",
		"apt-get -qq -y install curl",
		"apt-get update && apt-get install -y libvips-dev",
		"apk add --no-cache jq",
		"apk --no-cache add jq",
		"dnf install -y git",
		"yum -y install git",
		"microdnf install git",
		"zypper in git",
		"pacman -S --noconfirm git",
		"pacman -Syu git",
		"dpkg -i ./pkg.deb",
		"rpm -ivh pkg.rpm",
		"rpm -U pkg.rpm",
		"bash -c 'apt-get install -y jq'",
		"(apt-get install -y jq)",
		"true;apk add jq",
		"aptitude install curl",
		"apt-get reinstall curl",
		"apt-get -o Dpkg::Options::=--force-confold -y install curl",
		"apt-get -t bookworm-backports install curl",
		"DEBIAN_FRONTEND=noninteractive sudo -E apt-get -y --no-install-recommends install curl",
		"tdnf install git",
		"dnf -c /etc/dnf/alt.conf install git",
		"yum groupinstall 'Development Tools'",
		"dnf group install 'Development Tools'",
		"dpkg --install ./pkg.deb",
		"rpm --upgrade pkg.rpm",
	}
	no := []string{
		"cat /etc/hosts",
		"pnpm install",
		"npm install",
		"pip install -r requirements.txt",
		"go install ./cmd/sx",
		"apt-get update",
		"apt list --installed",
		"apk info",
		"pacman -Ss git",
		"pacman -Q",
		"dpkg -l",
		"rpm -qi bash",
		"my-apt-get install x",
		"grep install apt.log",
		"dnf group list",
		"zypper -t search install",
		"apt-get -o Debug::pkgProblemResolver=yes update",
	}
	for _, c := range yes {
		if !installsSystemPackages(c) {
			t.Errorf("missed %q", c)
		}
	}
	for _, c := range no {
		if installsSystemPackages(c) {
			t.Errorf("false positive %q", c)
		}
	}
}

func TestContainerFreshWarnsWhenStateCantBeWritten(t *testing.T) {
	f := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_STATE_DIR", f) // a file: the marker's directory can't be made
	rep := &warnRec{}
	ws := &Workspace{WorktreeKey: "a"}
	for range 2 {
		warnContainerFresh(ws, Options{Command: "ls", SameSlot: true, Rep: rep}, "")
		if w := rep.take(); len(w) != 1 {
			t.Fatalf("got %v, want one warning", w)
		}
		warnContainerFresh(ws, Options{Command: "apk add jq", SameSlot: true, Rep: rep}, "")
		if w := rep.take(); len(w) != 1 || !strings.Contains(w[0], "installs system packages") {
			t.Fatalf("install: got %v, want one install warning", w)
		}
	}
}
