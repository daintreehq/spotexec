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
