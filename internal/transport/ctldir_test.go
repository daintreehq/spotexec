package transport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/daintreehq/spotexec/internal/provider"
)

// useCtlDirs points controlDir at a fresh home and tmp root for one test.
func useCtlDirs(t *testing.T, home, tmp string) {
	t.Helper()
	t.Setenv("HOME", home)
	ctlMu.Lock()
	oldTmp := tmpRoot
	ctlDir, tmpRoot = "", tmp
	ctlMu.Unlock()
	t.Cleanup(func() {
		ctlMu.Lock()
		ctlDir, tmpRoot = "", oldTmp
		ctlMu.Unlock()
	})
}

// shortDir makes a directory with a path short enough for a home whose
// control directory fits a socket path.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "sxh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// longHome is a home too long for its control sockets: the shared tmp root
// is used instead.
func longHome(t *testing.T) string {
	return filepath.Join(t.TempDir(), strings.Repeat("h", 80))
}

func mustPrivate(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s is not a private directory: %v %v", dir, fi, err)
	}
}

// When the preferred directory in shared tmp isn't ours and no private
// alternative can be made, there is no directory to hand to OpenSSH: the
// rejected path must never come back, and no client may use it.
func TestControlDirFailsWhenPreferredAndFallbackFail(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tmp := t.TempDir()
	useCtlDirs(t, longHome(t), tmp)
	preferred := filepath.Join(tmp, fmt.Sprintf("sx-cm-%d", os.Getuid()))
	if err := os.WriteFile(preferred, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(tmp, 0o700) })
	d, err := controlDir()
	if err == nil {
		t.Fatalf("controlDir handed out %s with neither directory usable", d)
	}
	if d != "" {
		t.Fatalf("controlDir returned %q alongside its error", d)
	}

	// A client built now fails every call with that error, and never gets
	// as far as ssh (there is none on PATH to run).
	t.Setenv("PATH", t.TempDir())
	c := New(provider.Target{Host: "box", User: "dev"}, "~/.cache/spotexec")
	if c.ctlErr == nil {
		t.Fatalf("client built with control path %s", c.ctlPath)
	}
	ctx := context.Background()
	if _, err := c.Run(ctx, "true", nil); !errors.Is(err, c.ctlErr) || IsConnError(err) {
		t.Fatalf("Run: %v, want the setup error", err)
	}
	if _, err := c.Open(ctx); !errors.Is(err, c.ctlErr) || IsConnError(err) {
		t.Fatalf("Open: %v, want the setup error", err)
	}
	if err := c.EnsureMaster(ctx); !errors.Is(err, c.ctlErr) {
		t.Fatalf("EnsureMaster: %v, want the setup error", err)
	}
	if _, err := c.EnsureHelper(ctx, true); !errors.Is(err, c.ctlErr) {
		t.Fatalf("EnsureHelper: %v, want the setup error", err)
	}
	if c.MasterAlive(ctx) {
		t.Fatal("MasterAlive without a control directory")
	}
	if _, err := c.HelperCmdInstalled(ctx, "status"); !errors.Is(err, c.ctlErr) {
		t.Fatalf("HelperCmdInstalled: %v, want the setup error", err)
	}
	for _, a := range c.Args("true") {
		if strings.HasPrefix(a, "ControlPath=") && a != "ControlPath=none" {
			t.Fatalf("argv hands ssh an unverified control path: %v", c.Args("true"))
		}
	}
	c.CloseMaster()
}

// A preferred directory in shared tmp that isn't ours (here a symlink an
// attacker could point anywhere) is replaced by a fresh private one.
func TestControlDirReplacesForeignTmpDir(t *testing.T) {
	tmp := t.TempDir()
	useCtlDirs(t, longHome(t), tmp)
	preferred := filepath.Join(tmp, fmt.Sprintf("sx-cm-%d", os.Getuid()))
	if err := os.Symlink(t.TempDir(), preferred); err != nil {
		t.Fatal(err)
	}
	d, err := controlDir()
	if err != nil {
		t.Fatal(err)
	}
	if d == preferred || filepath.Dir(d) != tmp {
		t.Fatalf("controlDir = %s, want a new directory in %s", d, tmp)
	}
	mustPrivate(t, d)
}

// A directory of ours that others can write to may already hold a socket
// of theirs: it is replaced, not made private in place. A fallback the
// umask left without the owner's bits is made usable before use.
func TestControlDirRefusesWritableTmpDir(t *testing.T) {
	tmp := t.TempDir()
	useCtlDirs(t, longHome(t), tmp)
	preferred := filepath.Join(tmp, fmt.Sprintf("sx-cm-%d", os.Getuid()))
	if err := os.Mkdir(preferred, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(preferred, 0o777); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o700)
	d, err := controlDir()
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	if d == preferred {
		t.Fatalf("controlDir kept %s, which others could write to", d)
	}
	mustPrivate(t, d)
}

// Only the owner may write to a control directory: group or other write
// access is refused, read access is just removed.
func TestPrivateDirModes(t *testing.T) {
	for mode, ok := range map[os.FileMode]bool{0o700: true, 0o755: true, 0o770: false, 0o707: false, 0o777: false} {
		d := filepath.Join(t.TempDir(), "cm")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		if err := privateDir(d); (err == nil) != ok {
			t.Errorf("mode %o: privateDir = %v, want ok=%v", mode, err, ok)
		} else if ok {
			mustPrivate(t, d)
		}
	}
}

// A home whose ancestor others could rename lets them swap the control
// directory for theirs after it was checked: the short one in tmp is used
// instead. A sticky ancestor (as /tmp is) is fine.
func TestControlDirRefusesReplaceableAncestor(t *testing.T) {
	top := shortDir(t)
	home := filepath.Join(top, "h")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	useCtlDirs(t, home, tmp)
	if err := os.Chmod(top, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(top, 0o700) })
	wantIn(t, tmp)
	useCtlDirs(t, home, tmp)
	if err := os.Chmod(top, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	wantIn(t, filepath.Join(home, ".cache", "spotexec"))
}

// A symlink on the way may lead through a directory that is on neither
// the path as written nor the path it resolves to: that one is checked too.
func TestControlDirChecksDirsBetweenSymlinks(t *testing.T) {
	top := shortDir(t)
	real := filepath.Join(top, "r")
	open := filepath.Join(top, "w") // others may rename what's in here
	for _, d := range []string{real, open} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real, filepath.Join(open, "l2")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(top, "l1")
	if err := os.Symlink(filepath.Join(open, "l2"), home); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(open, 0o700) })
	tmp := t.TempDir()
	useCtlDirs(t, home, tmp)
	wantIn(t, tmp)
}

// OpenSSH expands %-tokens and ${VAR} in a ControlPath: a home holding
// either isn't used, since ssh would read another path than the one checked.
func TestControlDirSkipsExpandingHome(t *testing.T) {
	for _, name := range []string{"a%h", "b${X}"} {
		home := filepath.Join(shortDir(t), name)
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		tmp := t.TempDir()
		useCtlDirs(t, home, tmp)
		wantIn(t, tmp)
	}
}

// wantIn checks controlDir gives a private directory inside parent.
func wantIn(t *testing.T, parent string) {
	t.Helper()
	d, err := controlDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(d) != parent && d != filepath.Join(parent, "cm") {
		t.Fatalf("controlDir = %s, want one in %s", d, parent)
	}
	mustPrivate(t, d)
}

// A client re-checks its directory before each use: one cleaned away is
// made anew, and one whose name is now something else's is refused.
func TestClientRechecksControlDir(t *testing.T) {
	tmp := t.TempDir()
	useCtlDirs(t, longHome(t), tmp)
	t.Setenv("PATH", t.TempDir()) // no ssh: nothing gets that far
	c := New(provider.Target{Host: "box"}, "")
	if c.ctlErr != nil {
		t.Fatal(c.ctlErr)
	}
	dir := filepath.Dir(c.ctlPath)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "true", nil); IsSetupError(err) {
		t.Fatalf("Run after cleanup: %v", err)
	}
	mustPrivate(t, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "true", nil); !IsSetupError(err) {
		t.Fatalf("Run with the name taken: %v, want a setup error", err)
	}
	if c.MasterAlive(context.Background()) {
		t.Fatal("MasterAlive with the name taken")
	}
}

// A directory of ours holding another user's entry (left from when it was
// writable) is replaced: their socket could stand in for a master. Only root
// can make another user's file to test with.
func TestControlDirRefusesForeignEntry(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to own a file as another user")
	}
	tmp := t.TempDir()
	useCtlDirs(t, longHome(t), tmp)
	preferred := filepath.Join(tmp, fmt.Sprintf("sx-cm-%d", os.Getuid()))
	if err := os.Mkdir(preferred, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(preferred, "0123456789abcdef")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(f, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	d, err := controlDir()
	if err != nil {
		t.Fatal(err)
	}
	if d == preferred {
		t.Fatalf("controlDir kept %s, which holds another user's entry", d)
	}
}

// The home directory's control directory must be private too. A failure
// to make it, or a symlink in its place, falls back to the short one in
// tmp, never to the home path unchecked.
func TestControlDirHomeMustBePrivate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		home := shortDir(t)
		useCtlDirs(t, home, t.TempDir())
		wantIn(t, filepath.Join(home, ".cache", "spotexec"))
	})
	t.Run("cannot create", func(t *testing.T) {
		home := shortDir(t)
		tmp := t.TempDir()
		useCtlDirs(t, home, tmp)
		if err := os.WriteFile(filepath.Join(home, ".cache"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		wantIn(t, tmp)
	})
	t.Run("symlink", func(t *testing.T) {
		home := shortDir(t)
		tmp := t.TempDir()
		useCtlDirs(t, home, tmp)
		if err := os.MkdirAll(filepath.Join(home, ".cache", "spotexec"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(home, ".cache", "spotexec", "cm")); err != nil {
			t.Fatal(err)
		}
		wantIn(t, tmp)
	})
	t.Run("both fail", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		home := shortDir(t)
		tmp := t.TempDir()
		useCtlDirs(t, home, tmp)
		if err := os.WriteFile(filepath.Join(home, ".cache"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tmp, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(tmp, 0o700) })
		if d, err := controlDir(); !IsSetupError(err) || d != "" {
			t.Fatalf("controlDir = %q, %v; want a setup error", d, err)
		}
	})
}
