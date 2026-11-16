package helper

import (
	"os"
	"path/filepath"
	"testing"
)

// /var/tmp is world-writable: a directory, symlink or file another account
// put at the lifecycle path must not make the host look like spotexec's VM.
func TestTrustedDir(t *testing.T) {
	uid := uint32(os.Getuid())
	base := t.TempDir()
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // past the umask
			t.Fatal(err)
		}
		return p
	}
	real := mk("real", 0o755)
	group := mk("group", 0o775) // cloud-init opens it to the runner's group
	open := mk("open", 0o1777)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		path  string
		owner uint32
		want  bool
	}{
		{"owned directory", real, uid, true},
		{"group-writable", group, uid, true},
		{"someone else's directory", real, uid + 1, false},
		{"world-writable", open, uid, false},
		{"symlink to an owned directory", link, uid, false},
		{"regular file", file, uid, false},
		{"missing", filepath.Join(base, "none"), uid, false},
	} {
		if got := trustedDir(tc.path, tc.owner); got != tc.want {
			t.Errorf("%s: trustedDir = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Whatever an account other than root put at the lifecycle path (in CI,
// usually nothing) never turns the host into a spotexec VM.
func TestOnSpotexecVMNeedsRoot(t *testing.T) {
	if trustedDir(LifecycleDir, 0) {
		t.Skip("running on one of spotexec's VMs")
	}
	if onSpotexecVM() || simulated() {
		t.Fatalf("%s isn't a directory root owns, yet counts as spotexec's VM", LifecycleDir)
	}
}
