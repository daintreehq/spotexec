package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// The home directory's control directory must exist and be private too: a
// failure to make it, or a symlink in its place, is an error, not a path.
func TestControlDirHomeMustBePrivate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		home := shortDir(t)
		useCtlDirs(t, home, t.TempDir())
		d, err := controlDir()
		if err != nil {
			t.Fatal(err)
		}
		if d != filepath.Join(home, ".cache", "spotexec", "cm") {
			t.Fatalf("controlDir = %s, want the home directory's", d)
		}
		mustPrivate(t, d)
	})
	t.Run("cannot create", func(t *testing.T) {
		home := shortDir(t)
		useCtlDirs(t, home, t.TempDir())
		if err := os.WriteFile(filepath.Join(home, ".cache"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if d, err := controlDir(); err == nil || d != "" {
			t.Fatalf("controlDir = %q, %v; want an error", d, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		home := shortDir(t)
		useCtlDirs(t, home, t.TempDir())
		if err := os.MkdirAll(filepath.Join(home, ".cache", "spotexec"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(home, ".cache", "spotexec", "cm")); err != nil {
			t.Fatal(err)
		}
		if d, err := controlDir(); err == nil || d != "" {
			t.Fatalf("controlDir = %q, %v; want an error", d, err)
		}
	})
}
