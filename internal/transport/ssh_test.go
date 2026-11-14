package transport

import (
	"context"
	"slices"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

func sshRunnerTarget(t *testing.T, opts ...string) provider.Target {
	t.Helper()
	p, err := provider.New(&config.Runner{Name: "r", Type: "ssh", Hosts: []string{"homelab"}, User: "me", SSHOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	info, err := p.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.Target
}

// Two runners on one host that differ only in their ssh_options port are
// different endpoints: they must not share a control master or the helper
// markers keyed on it.
func TestConnIdentityIncludesSSHOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := New(sshRunnerTarget(t, "Port=2222"), "~/.cache/spotexec")
	b := New(sshRunnerTarget(t, "Port=2223"), "~/.cache/spotexec")
	if a.ctlPath == b.ctlPath {
		t.Fatalf("runners differing only in port share control path %s", a.ctlPath)
	}
	if markerPath(a, "h") == markerPath(b, "h") || markerPathAny(a) == markerPathAny(b) {
		t.Fatal("runners differing only in port share helper markers")
	}
	// Same settings, same master.
	if c := New(sshRunnerTarget(t, "Port=2222"), "~/.cache/spotexec"); c.ctlPath != a.ctlPath {
		t.Fatalf("identical runners got different control paths %s / %s", a.ctlPath, c.ctlPath)
	}
	// Order matters to ssh (first value wins), so it matters to identity.
	x := New(provider.Target{Host: "h", Options: []string{"Port=1", "Port=2"}}, "")
	y := New(provider.Target{Host: "h", Options: []string{"Port=2", "Port=1"}}, "")
	if x.ctlPath == y.ctlPath {
		t.Fatal("option order ignored in connection identity")
	}
	// No ambiguity from joining: ["A=1|B=2"] is not ["A=1", "B=2"].
	p := New(provider.Target{Host: "h", Options: []string{"A=1|B=2"}}, "")
	q := New(provider.Target{Host: "h", Options: []string{"A=1", "B=2"}}, "")
	if p.ctlPath == q.ctlPath {
		t.Fatal("joined options collide")
	}
}

func optIndex(a []string, opt string) int {
	for i := 0; i+1 < len(a); i++ {
		if a[i] == "-o" && a[i+1] == opt {
			return i
		}
	}
	return -1
}

// ssh takes the first value it sees for an option: the runner's
// ssh_options must come before spotexec's tunable defaults, and after the
// control settings spotexec depends on.
func TestSSHOptionsPrecedeDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	overrides := map[string]string{
		"ConnectTimeout=10":      "ConnectTimeout=60",
		"ServerAliveInterval=10": "ServerAliveInterval=30",
		"ServerAliveCountMax=3":  "ServerAliveCountMax=9",
		"LogLevel=ERROR":         "LogLevel=DEBUG",
	}
	var opts []string
	for _, o := range overrides {
		opts = append(opts, o)
	}
	c := New(provider.Target{Host: "h", User: "u", IdentityFile: "/k", KnownHostsFile: "/kh", Options: opts}, "")
	a := c.baseArgs("auto")
	user := len(a)
	for def, o := range overrides {
		i, j := optIndex(a, o), optIndex(a, def)
		if i < 0 || j < 0 {
			t.Fatalf("%s or %s missing: %v", o, def, a)
		}
		if j < i {
			t.Fatalf("default %s at %d precedes the runner's %s at %d: %v", def, j, o, i, a)
		}
		user = min(user, i)
	}
	for _, fixed := range []string{"BatchMode=yes", "ControlMaster=auto", "ControlPath=" + c.ctlPath, "ControlPersist=" + ControlPersist} {
		if i := optIndex(a, fixed); i < 0 || i > user {
			t.Fatalf("control option %s at %d must precede ssh_options at %d: %v", fixed, i, user, a)
		}
	}
	// The target's own settings (per-instance known_hosts, key, user) are
	// not defaults: they stay ahead of ssh_options.
	if i := optIndex(a, "UserKnownHostsFile=/kh"); i < 0 || i > user {
		t.Fatalf("known_hosts after ssh_options: %v", a)
	}
	if i := slices.Index(a, "-i"); i < 0 || i > user {
		t.Fatalf("identity after ssh_options: %v", a)
	}
}
