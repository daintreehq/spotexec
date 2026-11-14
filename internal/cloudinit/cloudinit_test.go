package cloudinit

import (
	"os"
	"os/exec"
	"path/filepath"
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
