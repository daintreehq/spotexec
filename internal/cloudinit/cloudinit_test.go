package cloudinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRenderIsValidBash(t *testing.T) {
	s, err := Render(Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test", IdleTimeout: 10 * time.Minute,
		MaxLifetime: 2 * time.Hour, BootstrapScript: "apt-get update", UserBootstrap: "echo hi", BootstrapTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ud.sh")
	os.WriteFile(p, []byte(s), 0o644)
	if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v: %s", err, out)
	}
	for _, want := range []string{"IDLE=600", "MAXLIFE=7200", `echo "7200" > "$D/max_lifetime"`, "timeout -k 30 60", "ClientAliveInterval 30", "flock -n -x 9",
		"'AuthenticationMethods publickey'", "'PasswordAuthentication no'", "R=no RT=no", "'GSSAPIAuthentication no'", `"AllowUsers $U"`, "sshd_config.d/00-spotexec.conf"} {
		if !strings.Contains(s, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	if _, err := Render(Options{User: "ubuntu; rm -rf /", PublicKey: "k"}); err == nil {
		t.Fatal("expected invalid user error")
	}
	if _, err := Render(Options{User: "ubuntu", PublicKey: "a'b"}); err == nil {
		t.Fatal("expected invalid key error")
	}
}

// A root runner user keeps key logins: root login can't be refused outright.
func TestRenderRootUserKeepsKeyLogin(t *testing.T) {
	s, err := Render(Options{User: "root", PublicKey: "ssh-ed25519 AAAA test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, `[ "$U" = root ] && R=prohibit-password`) || !strings.Contains(s, `"PermitRootLogin $R"`) {
		t.Fatal("root runner user would be refused by PermitRootLogin no")
	}
}

// localhost must resolve to ::1 as well as 127.0.0.1 (as on macOS and CI
// runners), added once, and only when the VM has an IPv6 loopback.
func TestRenderMapsLocalhostToIPv6Loopback(t *testing.T) {
	s, err := Render(Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test"})
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(s, "# ---- 3. localhost on IPv6 too ----")
	end := strings.Index(s, "# ---- 4. User bootstrap ----")
	if start < 0 || end < start {
		t.Fatal("user-data has no localhost section before the user bootstrap")
	}
	block := s[start:end]
	const lo6 = "00000000000000000000000000000001 01 80 10 80       lo\n"
	const ubuntu = "127.0.0.1 localhost\n\n# The following lines are desirable for IPv6 capable hosts\n::1 ip6-localhost ip6-loopback\nfe00::0 ip6-localnet\nff02::1 ip6-allnodes\n"
	for _, tc := range []struct {
		name, hosts, inet6, want string
	}{
		{"ubuntu", ubuntu, lo6, strings.Replace(ubuntu, "::1 ip6-localhost", "::1 localhost ip6-localhost", 1)},
		{"already mapped", "127.0.0.1 localhost\n::1\tlocalhost ip6-localhost ip6-loopback\n", lo6, "127.0.0.1 localhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"},
		{"mapped later on the line", "127.0.0.1 localhost\n::1 ip6-localhost localhost\n", lo6, "127.0.0.1 localhost\n::1 ip6-localhost localhost\n"},
		{"only in a comment", "127.0.0.1 localhost\n::1 ip6-localhost # localhost\n", lo6, "127.0.0.1 localhost\n::1 localhost ip6-localhost # localhost\n"},
		{"no ::1 line", "127.0.0.1 localhost\n", lo6, "127.0.0.1 localhost\n::1 localhost\n"},
		{"no trailing newline", "127.0.0.1 localhost", lo6, "127.0.0.1 localhost\n::1 localhost\n"},
		{"no IPv6 loopback", ubuntu, "", ubuntu},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			hosts, inet6 := filepath.Join(dir, "hosts"), filepath.Join(dir, "if_inet6")
			if err := os.WriteFile(hosts, []byte(tc.hosts), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inet6, []byte(tc.inet6), 0o644); err != nil {
				t.Fatal(err)
			}
			script := strings.NewReplacer("/etc/hosts", hosts, "/proc/net/if_inet6", inet6).Replace(block)
			for run := 1; run <= 2; run++ { // the second run must change nothing
				if out, err := exec.Command("bash", "-c", "set -u\n"+script).CombinedOutput(); err != nil {
					t.Fatalf("run %d: %v: %s", run, err, out)
				}
				got, _ := os.ReadFile(hosts)
				if string(got) != tc.want {
					t.Fatalf("run %d: /etc/hosts =\n%q\nwant\n%q", run, got, tc.want)
				}
			}
			if _, err := os.Stat(hosts + ".sx"); err == nil {
				t.Fatal("temporary hosts file left behind")
			}
		})
	}
}

// /var/tmp is world-writable: whatever another account left at the state
// directory (a symlink, a file, a directory it owns) is removed before root
// writes state there, and a directory root can't make its own stops the
// boot. Run as an ordinary user, "root" is this test's uid.
func TestRenderClaimsStateDirForRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the guard uses GNU stat")
	}
	s, err := Render(Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test", SelfDestruct: "echo DESTROYED"})
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(s, "D=/var/tmp/spotexec\n")
	end := strings.Index(s, `chmod 0755 "$D"   # opened`)
	if start < 0 || end < start {
		t.Fatal("user-data has no state directory guard before its first chmod")
	}
	block := s[start:end]
	uid := strconv.Itoa(os.Getuid())
	run := func(t *testing.T, d, owner string) (string, error) {
		script := strings.NewReplacer("/var/tmp/spotexec", d, "0:d", owner+":d", "sleep 120; poweroff -f", ":").Replace(block)
		out, err := exec.Command("bash", "-c", "set -u\n"+script).CombinedOutput()
		return string(out), err
	}
	realDir := func(t *testing.T, d string) {
		t.Helper()
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
			t.Fatalf("%s is not a real directory: %v %v", d, fi, err)
		}
	}
	t.Run("missing", func(t *testing.T) {
		d := filepath.Join(t.TempDir(), "spotexec")
		if out, err := run(t, d, uid); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		realDir(t, d)
	})
	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		victim := filepath.Join(base, "victim")
		os.Mkdir(victim, 0o755)
		os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o644)
		d := filepath.Join(base, "spotexec")
		if err := os.Symlink(victim, d); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, d, uid); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		realDir(t, d)
		if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
			t.Fatalf("the symlink's target was touched: %v", err)
		}
	})
	t.Run("file", func(t *testing.T) {
		d := filepath.Join(t.TempDir(), "spotexec")
		os.WriteFile(d, nil, 0o644)
		if out, err := run(t, d, uid); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		realDir(t, d)
	})
	t.Run("world-writable directory", func(t *testing.T) {
		// Root's, but anyone could have planted links in it for root to
		// write through.
		base := t.TempDir()
		target := filepath.Join(base, "passwd")
		os.WriteFile(target, []byte("keep"), 0o644)
		d := filepath.Join(base, "spotexec")
		os.Mkdir(d, 0o700)
		os.Chmod(d, 0o1777)
		os.Symlink(target, filepath.Join(d, "created_at"))
		if out, err := run(t, d, uid); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		realDir(t, d)
		if _, err := os.Lstat(filepath.Join(d, "created_at")); err == nil {
			t.Fatal("the planted link survived")
		}
		if fi, _ := os.Stat(d); fi.Mode().Perm()&0o002 != 0 {
			t.Fatalf("still world-writable: %s", fi.Mode())
		}
		if b, _ := os.ReadFile(target); string(b) != "keep" {
			t.Fatalf("the link's target changed: %q", b)
		}
	})
	t.Run("unreadable mode fails closed", func(t *testing.T) {
		// A stat that answers nothing proves nothing: the boot stops.
		d := filepath.Join(t.TempDir(), "spotexec")
		os.Mkdir(d, 0o755)
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "stat"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
		script := strings.NewReplacer("/var/tmp/spotexec", d, "0:d", uid+":d", "sleep 120; poweroff -f", ":").Replace(block)
		cmd := exec.Command("bash", "-c", "set -u\n"+script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "DESTROYED") {
			t.Fatalf("boot went on without knowing who owns the directory: %v: %s", err, out)
		}
	})
	t.Run("root's directory is kept", func(t *testing.T) {
		d := filepath.Join(t.TempDir(), "spotexec")
		os.Mkdir(d, 0o755)
		os.WriteFile(filepath.Join(d, "keep"), nil, 0o644)
		if out, err := run(t, d, uid); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if _, err := os.Stat(filepath.Join(d, "keep")); err != nil {
			t.Fatalf("root's own directory was emptied: %v", err)
		}
	})
	t.Run("someone else's directory", func(t *testing.T) {
		// "root" is a uid this test isn't: every directory it makes is
		// someone else's, so the planted one goes and the boot stops.
		d := filepath.Join(t.TempDir(), "spotexec")
		os.Mkdir(d, 0o755)
		os.WriteFile(filepath.Join(d, "draining"), nil, 0o644)
		out, err := run(t, d, strconv.Itoa(os.Getuid()+1))
		if err == nil || !strings.Contains(out, "DESTROYED") {
			t.Fatalf("boot went on with a directory root doesn't own: %v: %s", err, out)
		}
		if _, err := os.Stat(filepath.Join(d, "draining")); err == nil {
			t.Fatal("the planted state survived")
		}
	})
}
