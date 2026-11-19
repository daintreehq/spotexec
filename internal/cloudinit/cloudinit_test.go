package cloudinit

import (
	"fmt"
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

// The watchdog ends a VM with a scheduled early drain once it has been idle
// for drain_after, and any other VM only at idle_timeout. Run against a
// copy of the rendered script with its directory and power-off swapped
// out (and flock, logger and GNU stat shimmed where the host lacks them).
func TestWatchdogScheduledDrain(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	s, err := Render(Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test", IdleTimeout: 10 * time.Minute,
		MaxLifetime: 12 * time.Hour, SelfDestruct: `touch "$D/destroyed"`})
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(s, "<<'WATCHDOG'\n")
	end := strings.Index(s, "\nWATCHDOG\n")
	if start < 0 || end < start {
		t.Fatal("watchdog script not found")
	}
	dir := t.TempDir()
	d := filepath.Join(dir, "state")
	bin := filepath.Join(dir, "bin")
	for _, p := range []string{d, bin} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.Replace(s[start+len("<<'WATCHDOG'\n"):end], "D=/var/tmp/spotexec", "D="+strconv.Quote(d), 1)
	wd := filepath.Join(dir, "watchdog")
	shims := map[string]string{
		"logger": "#!/bin/sh\nshift 2; echo \"$*\" >> \"" + d + "/log\"\n",
		// $FLOCK_HELD: a session holds the lifecycle lock; $FLOCK_TOUCH: one
		// ends (touching activity) while the watchdog waits for it;
		// $FLOCK_CANCEL: one cancels the drain (#255) meanwhile.
		"flock": "#!/bin/sh\n[ -n \"$FLOCK_TOUCH\" ] && touch \"" + d + "/activity\"\n[ -n \"$FLOCK_CANCEL\" ] && rm -f \"" + d + "/drain_after\"\n[ -z \"$FLOCK_HELD\" ]\n",
	}
	if exec.Command("stat", "-c", "%Y", dir).Run() != nil {
		shims["stat"] = "#!/bin/sh\nexec /usr/bin/stat -f %m \"$3\"\n" // BSD stat
	}
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(wd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	created := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	var env []string
	run := func(drainAfter string, idle time.Duration) (destroyed bool, log string) {
		t.Helper()
		for _, f := range []string{"destroyed", "draining", "log", "drain_after"} {
			os.Remove(filepath.Join(d, f))
		}
		os.WriteFile(filepath.Join(d, "created_at"), []byte(created+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "lifecycle.lock"), nil, 0o644)
		act := filepath.Join(d, "activity")
		os.WriteFile(act, nil, 0o644)
		at := time.Now().Add(-idle)
		os.Chtimes(act, at, at)
		if drainAfter != "" {
			os.WriteFile(filepath.Join(d, "drain_after"), []byte(drainAfter), 0o644)
		}
		cmd := exec.Command("bash", wd)
		cmd.Env = append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH")), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("watchdog: %v: %s", err, out)
		}
		_, err := os.Stat(filepath.Join(d, "destroyed"))
		b, _ := os.ReadFile(filepath.Join(d, "log"))
		return err == nil, string(b)
	}
	if gone, log := run("", 3*time.Minute); gone {
		t.Fatalf("a retained VM ended before idle_timeout: %s", log)
	}
	if gone, log := run("", 11*time.Minute); !gone || !strings.Contains(log, "limit 10m0s") {
		t.Fatalf("idle past idle_timeout: %v %q", gone, log)
	}
	if gone, log := run("120\n", 3*time.Minute); !gone || !strings.Contains(log, "limit 120s, surplus: early drain") {
		t.Fatalf("a scheduled drain idle past drain_after: %v %q", gone, log)
	}
	if _, err := os.Stat(filepath.Join(d, "draining")); err != nil {
		t.Fatal("ended without the drain marker (acquires must refuse first)")
	}
	if gone, _ := run("120\n", time.Minute); gone {
		t.Fatal("a scheduled drain ended inside drain_after")
	}
	for _, bad := range []string{"12x\n", "-5\n", "\n", "99999999999999\n"} {
		if gone, _ := run(bad, 3*time.Minute); gone {
			t.Fatalf("drain_after %q shortened the wait", bad)
		}
	}
	if gone, _ := run("1800\n", 3*time.Minute); gone {
		t.Fatal("a drain_after past idle_timeout ended the VM early")
	}
	if gone, log := run("1800\n", 11*time.Minute); !gone || !strings.Contains(log, "limit 10m0s") {
		t.Fatalf("a drain_after past idle_timeout kept the VM past it: %v %q", gone, log)
	}
	// Never under a session, nor right after one ended.
	env = []string{"FLOCK_HELD=1"}
	if gone, _ := run("120\n", 3*time.Minute); gone {
		t.Fatal("ended under a session")
	}
	if _, err := os.Stat(filepath.Join(d, "draining")); err == nil {
		t.Fatal("marked draining under a session")
	}
	env = []string{"FLOCK_TOUCH=1"}
	if gone, _ := run("120\n", 3*time.Minute); gone {
		t.Fatal("ended just after a session touched activity")
	}
	// A drain cancelled after the watchdog first read it: idle_timeout again.
	env = []string{"FLOCK_CANCEL=1"}
	if gone, log := run("120\n", 3*time.Minute); gone {
		t.Fatalf("ended on a drain cancelled before it took the lock: %q", log)
	}
}

// The work volume section (#264) is rendered only when asked for, stays
// valid bash within the user-data budget, and refuses a work root it can't
// put in the script and /etc/fstab as is.
func TestRenderWorkVolume(t *testing.T) {
	base := Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test", BootstrapScript: strings.Repeat("apt-get install -y build-essential\n", 40)}
	s, err := Render(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "sx_workvol") || strings.Contains(s, "mkfs.xfs") {
		t.Fatal("work volume set up without one")
	}
	o := base
	o.WorkVolumeGB, o.WorkRoot = 64, "~/.cache/spotexec"
	s, err = Render(o)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ud.sh")
	os.WriteFile(p, []byte(s), 0o644)
	if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v: %s", err, out)
	}
	for _, want := range []string{"$2 == 68719476736", "w='~/.cache/spotexec'", "mkfs.xfs -q -K -m reflink=1"} {
		if !strings.Contains(s, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
	// Run (not just defined) after the watchdog is armed, under the
	// lifecycle lock taken before it, and before the key that lets anyone
	// write to the work root is authorized.
	lock := strings.Index(s, "exec 7<\"$D/lifecycle.lock\"; L7=0; flock -w 30 -s 7 && L7=1\n")
	armed := strings.Index(s, "systemctl enable --now spotexec-watchdog.timer")
	call := strings.Index(s, "\n{ [ \"$L7\" = 1 ] && sx_workvol; } >>")
	unlock := strings.Index(s, "exec 7<&-")
	key := strings.Index(s, ".ssh/authorized_keys")
	if lock < 0 || armed < lock || call < armed || unlock < call || key < unlock {
		t.Fatalf("work volume set up out of order: armed %d lock %d call %d unlock %d key %d", armed, lock, call, unlock, key)
	}
	for _, bad := range []string{"", "~/a b", "/srv/$(id)", "/srv/x'y", "/srv/../etc", "../x", "/srv/~x", "~user/x"} {
		o.WorkRoot = bad
		if _, err := Render(o); err == nil {
			t.Errorf("work root %q accepted", bad)
		}
	}
}

func TestWorkRootOK(t *testing.T) {
	for p, want := range map[string]bool{"~": true, "~/.cache/spotexec": true, "/mnt/work": true, "work": true, "/a/b-c_d.e+f": true,
		"": false, "~user": false, "/a/~b": false, "/a b": false, "/a/../b": false, "/srv/cache..v2": true, "..": false, "/a\n": false, `/a"b`: false} {
		if got := WorkRootOK(p); got != want {
			t.Errorf("WorkRootOK(%q) = %v", p, got)
		}
	}
}

// sx_workvol, run against stubbed disk tools: it formats and mounts the one
// blank disk of the requested size, and touches nothing when that disk
// can't be told apart or the work root is already in use.
func TestWorkVolumeSetup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	const gib64 = "68719476736"
	type disk struct {
		name, size, kind, model string
		parts, fs               bool // fs: blkid -p finds a signature (exit 0)
		probe                   int  // another blkid -p exit status; unset: 2, nothing found
	}
	type env struct {
		root   string
		disks  []disk
		prep   func(home string)
		fstab  bool   // /etc/fstab can't be appended to
		mountE bool   // mount fails
		after  int    // the disks show up from this scan on
		occupy bool   // something writes to the work root while mkfs runs
		hang   string // a disk tool that never returns ("blkid -p", "mkfs.xfs", "blkid -s"): its timeout fires
	}
	run := func(t *testing.T, e env) (log, calls, fstab string, err error) {
		t.Helper()
		dir := t.TempDir()
		home := filepath.Join(dir, "home")
		root := e.root
		if root == "" {
			root = "~/.cache/spotexec"
		}
		root = strings.ReplaceAll(root, "$TMP", dir)
		os.MkdirAll(home, 0o755)
		if e.prep != nil {
			e.prep(home)
		}
		s, rerr := Render(Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test", WorkVolumeGB: 64, WorkRoot: root})
		if rerr != nil {
			t.Fatal(rerr)
		}
		start := strings.Index(s, "sx_workvol() {")
		end := strings.Index(s[start:], "\n}\n")
		if start < 0 || end < 0 {
			t.Fatal("no sx_workvol in user-data")
		}
		fstabPath := filepath.Join(dir, "fstab")
		if e.fstab {
			os.Mkdir(fstabPath, 0o755) // appending to a directory fails
		}
		fn := strings.ReplaceAll(s[start:start+end+3], "/etc/fstab", fstabPath)
		var list, parts, probes strings.Builder
		for _, d := range e.disks {
			if d.kind == "" {
				d.kind = "disk"
			}
			if d.model == "" {
				d.model = "Amazon Elastic Block Store"
			}
			fmt.Fprintf(&list, "echo '%s %s %s %s'; ", d.name, d.size, d.kind, d.model)
			if d.parts {
				fmt.Fprintf(&parts, "%s) printf '%%s\\n%%s1\\n' $3 $3 ;; ", d.name)
			}
			switch {
			case d.fs:
				d.probe = 0
			case d.probe == 0:
				d.probe = 2
			}
			fmt.Fprintf(&probes, "%s) return %d ;; ", d.name, d.probe)
		}
		occupy := ":;"
		if e.occupy {
			// Its log line can't be written either: the guard must hold.
			occupy = `mkdir -p "$w" && touch "$w/early"; echo() { case "$*" in *"in use"*) return 1 ;; esac; builtin echo "$@"; };`
		}
		mountRC := 0
		if e.mountE {
			mountRC = 32
		}
		hang := "-"
		if e.hang != "" {
			hang = `"` + e.hang + `"*`
		}
		stubs := `
C=` + filepath.Join(dir, "calls") + `
timeout() { [ "$1" = -k ] && shift 2; shift; case "$*" in ` + hang + `) return 124 ;; esac; SX_T=1 "$@"; }
getent() { echo "ubuntu:x:1000:1000::` + home + `:/bin/bash"; }
id() { echo ubuntu; }
sleep() { SECONDS=$((SECONDS + 1)); }
lsblk() { case "$1" in -dbnpo) echo >> "$C.scans"; [ "$(wc -l < "$C.scans")" -ge ` + strconv.Itoa(e.after) + ` ] || return 0; ` + list.String() + `;; -npo) case "$3" in ` + parts.String() + `*) echo "$3" ;; esac ;; esac; }
blkid() { [ -n "${SX_T:-}" ] || echo "unbounded blkid $*" >> "$C"; if [ "$1" = -p ]; then case "$2" in ` + probes.String() + `esac; fi; echo uuid-1; }
mkfs.xfs() { [ -n "${SX_T:-}" ] || echo "unbounded mkfs" >> "$C"; echo "mkfs $*" >> "$C"; ` + occupy + ` }
runuser() { while [ "$1" != -- ]; do shift; done; shift; "$@"; }
mount() { echo "mount $*" >> "$C"; return ` + strconv.Itoa(mountRC) + `; }
umount() { echo "umount $*" >> "$C"; }
chown() { echo "chown $*" >> "$C"; }
`
		out, err := exec.Command("bash", "-c", "set -u\n"+stubs+fn+"sx_workvol").CombinedOutput()
		c, _ := os.ReadFile(filepath.Join(dir, "calls"))
		f, _ := os.ReadFile(fstabPath)
		clean := func(x string) string { return strings.ReplaceAll(strings.ReplaceAll(x, home, "$HOME"), dir, "$TMP") }
		return clean(string(out)), clean(string(c)), clean(string(f)), err
	}
	root := disk{name: "/dev/nvme0n1", size: gib64, parts: true}
	t.Run("the blank disk of its size", func(t *testing.T) {
		log, calls, fstab, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: "75000000000"}, {name: "/dev/nvme2n1", size: gib64}}})
		if err != nil {
			t.Fatalf("%v: %s", err, log)
		}
		want := "mkfs -q -K -m reflink=1 -L spotexecwork /dev/nvme2n1\nmount -t xfs -o noatime /dev/nvme2n1 $HOME/.cache/spotexec\nchown ubuntu:ubuntu $HOME/.cache/spotexec\n"
		if calls != want || fstab != "UUID=uuid-1 $HOME/.cache/spotexec xfs noatime,nofail 0 0\n" {
			t.Fatalf("calls:\n%s\nfstab: %q\nlog: %s", calls, fstab, log)
		}
	})
	t.Run("past same-sized instance storage and non-disks", func(t *testing.T) {
		_, calls, _, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: gib64, model: "Amazon EC2 NVMe Instance Storage"},
			{name: "/dev/loop0", size: gib64, kind: "loop"}, {name: "/dev/nvme2n1", size: gib64}}})
		if err != nil || !strings.HasPrefix(calls, "mkfs -q -K -m reflink=1 -L spotexecwork /dev/nvme2n1\n") {
			t.Fatalf("%v\n%s", err, calls)
		}
	})
	for name, c := range map[string]struct{ root, mnt string }{
		"an absolute work root": {"$TMP/srv/work", "$TMP/srv/work"},
		"the home directory":    {"~", "$HOME"},
		"a relative work root":  {"work", "$HOME/work"},
	} {
		t.Run(name, func(t *testing.T) {
			e := env{root: c.root, disks: []disk{root, {name: "/dev/xvdf", size: gib64, model: " "}}}
			if c.root == "~" {
				e.prep = func(h string) { os.RemoveAll(h) } // an empty home: nothing to cover
			}
			_, calls, fstab, err := run(t, e)
			if err != nil || !strings.Contains(calls, "mount -t xfs -o noatime /dev/xvdf "+c.mnt+"\n") || !strings.HasPrefix(fstab, "UUID=uuid-1 "+c.mnt+" ") {
				t.Fatalf("%v\n%s\n%s", err, calls, fstab)
			}
		})
	}
	t.Run("a disk that shows up late", func(t *testing.T) {
		_, calls, _, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: gib64}}, after: 5})
		if err != nil || !strings.HasPrefix(calls, "mkfs -q -K -m reflink=1 -L spotexecwork /dev/nvme1n1\n") {
			t.Fatalf("%v\n%s", err, calls)
		}
	})
	t.Run("work root filled during setup", func(t *testing.T) {
		log, calls, fstab, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: gib64}}, occupy: true})
		if err == nil || strings.Contains(calls, "mount") || fstab != "" {
			t.Fatalf("%v\n%s\n%s", err, log, calls)
		}
	})
	t.Run("fstab can't be written", func(t *testing.T) {
		_, calls, _, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: gib64}}, fstab: true})
		if err == nil || !strings.HasSuffix(calls, "umount $HOME/.cache/spotexec\n") {
			t.Fatalf("not rolled back: %v\n%s", err, calls)
		}
	})
	// A disk that stops answering mid-probe or mid-format: its tool is
	// timed out and the work root stays on the root disk, so cloud-init
	// still reaches the SSH key.
	for name, c := range map[string]struct{ hang, log string }{
		"a probe that hangs":       {"blkid -p", "blkid -p /dev/nvme1n1 timed out"},
		"a format that hangs":      {"mkfs.xfs", "can't format /dev/nvme1n1"},
		"a uuid lookup that hangs": {"blkid -s", "can't format /dev/nvme1n1"},
	} {
		t.Run(name, func(t *testing.T) {
			log, calls, fstab, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: gib64}}, hang: c.hang})
			if err == nil || !strings.Contains(log, c.log) || strings.Contains(calls, "mount") || fstab != "" {
				t.Fatalf("err %v, log %q, calls %q, fstab %q", err, log, calls, fstab)
			}
			if c.hang == "blkid -p" && strings.Contains(calls, "mkfs") {
				t.Fatalf("formatted after a probe timed out: %q", calls)
			}
		})
	}
	t.Run("mount fails", func(t *testing.T) {
		_, calls, fstab, err := run(t, env{disks: []disk{root, {name: "/dev/nvme1n1", size: gib64}}, mountE: true})
		if err == nil || strings.Contains(calls, "chown") || fstab != "" {
			t.Fatalf("%v\n%s\n%q", err, calls, fstab)
		}
	})
	for name, c := range map[string]struct {
		disks []disk
		prep  func(home string)
		log   string
	}{
		"no such disk":            {[]disk{root, {name: "/dev/nvme1n1", size: "75000000000"}}, nil, "found 0 blank disks of 64 GiB"},
		"two such disks":          {[]disk{root, {name: "/dev/nvme1n1", size: gib64}, {name: "/dev/nvme2n1", size: gib64}}, nil, "found 2 blank disks"},
		"only a used disk":        {[]disk{root, {name: "/dev/nvme1n1", size: gib64, fs: true}}, nil, "found 0 blank disks"},
		"a disk blkid can't read": {[]disk{root, {name: "/dev/nvme1n1", size: gib64, probe: 4}, {name: "/dev/nvme2n1", size: gib64, probe: 8}}, nil, "found 0 blank disks"},
		"work root has content":   {[]disk{root, {name: "/dev/nvme1n1", size: gib64}}, func(h string) { os.MkdirAll(filepath.Join(h, ".cache/spotexec/bin"), 0o755) }, "is in use"},
		"work root is a link": {[]disk{root, {name: "/dev/nvme1n1", size: gib64}}, func(h string) {
			os.MkdirAll(filepath.Join(h, ".cache"), 0o755)
			os.Mkdir(filepath.Join(h, "empty"), 0o755)
			os.Symlink(filepath.Join(h, "empty"), filepath.Join(h, ".cache/spotexec"))
		}, "is in use"},
	} {
		t.Run(name, func(t *testing.T) {
			log, calls, fstab, err := run(t, env{disks: c.disks, prep: c.prep})
			if err == nil || !strings.Contains(log, c.log) || calls != "" || fstab != "" {
				t.Fatalf("err %v, log %q, calls %q, fstab %q", err, log, calls, fstab)
			}
		})
	}
}

// A VM whose shutdown timers aren't scheduled would bill until someone
// noticed: it must never report ready, and ends itself instead (#321).
// Runs the timer setup and the final ready write of the rendered script,
// with a stand-in bootstrap between them and systemd stubbed.
func TestRenderRequiresShutdownTimers(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	s, err := Render(Options{User: "ubuntu", PublicKey: "ssh-ed25519 AAAA test", SelfDestruct: "echo DESTROYED"})
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(s, "systemctl daemon-reload\n")
	j := strings.Index(s, "# ---- 2. SSH")
	k := strings.Index(s, `echo "spotexec: first-boot setup finished`)
	if i < 0 || j < i || k < j {
		t.Fatal("user-data has no timer setup before its ready write")
	}
	arm, finish := s[i:j], s[k:]
	for _, tc := range []struct {
		name         string
		boot, later  map[string]string // timer SubStates, from boot and once the bootstrap ran ("dead": won't start)
		status       int               // the bootstrap's exit status
		ready        string            // "" when the VM must end itself
		bootstrapRan bool
	}{
		{name: "both timers scheduled", ready: "0", bootstrapRan: true},
		{name: "watchdog mid-run", boot: map[string]string{"watchdog": "running"}, ready: "0", bootstrapRan: true},
		{name: "failed bootstrap keeps its status", status: 3, ready: "3", bootstrapRan: true},
		{name: "watchdog timer won't start", boot: map[string]string{"watchdog": "dead"}},
		{name: "max-lifetime timer won't start", boot: map[string]string{"maxlife": "dead"}},
		{name: "no systemd timers at all", boot: map[string]string{"watchdog": "dead", "maxlife": "dead"}},
		{name: "watchdog timer failed", boot: map[string]string{"watchdog": "failed"}},
		{name: "watchdog timer stopped by the bootstrap", later: map[string]string{"watchdog": "dead"}, bootstrapRan: true},
		{name: "max lifetime spent during the bootstrap", later: map[string]string{"maxlife": "elapsed"}, bootstrapRan: true},
		{name: "failed bootstrap without timers keeps its status", later: map[string]string{"maxlife": "dead"}, status: 3, bootstrapRan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			d, bin, log := filepath.Join(dir, "state"), filepath.Join(dir, "bin"), filepath.Join(dir, "bootstrap.log")
			os.MkdirAll(d, 0o755)
			os.MkdirAll(bin, 0o755)
			set := func(m map[string]string) string {
				var b strings.Builder
				for u, st := range m {
					fmt.Fprintf(&b, "echo %s > %s\n", st, strconv.Quote(filepath.Join(dir, u)))
				}
				return b.String()
			}
			// Each timer's SubState is in a file named for it (default
			// "waiting"); a "dead" one also fails to start.
			stub := "#!/bin/sh\nfor u in watchdog maxlife; do case \"$*\" in *spotexec-$u*) s=$(cat \"" + dir + "/$u\" 2>/dev/null || echo waiting) ;; esac; done\n" +
				"case \"$1\" in show) echo \"SubState=${s:-dead}\" ;; *) [ \"${s:-}\" != dead ] ;; esac\n"
			for _, b := range []string{"systemctl", "systemd-run"} {
				os.WriteFile(filepath.Join(bin, b), []byte(stub), 0o755)
			}
			os.WriteFile(filepath.Join(bin, "logger"), []byte("#!/bin/sh\n"), 0o755)
			bootstrap := fmt.Sprintf("status=%d\nB=%s\ntouch %s\n", tc.status, strconv.Quote(log), strconv.Quote(filepath.Join(dir, "bootstrap-ran"))) + set(tc.later)
			script := set(tc.boot) + "D=" + strconv.Quote(d) + "\n" + arm + bootstrap + finish
			script = strings.NewReplacer("/var/tmp/spotexec", d, "/var/log/spotexec-bootstrap.log", log, "sleep 120; poweroff -f", ":").Replace(script)
			cmd := exec.Command("bash", "-c", "set -u\n"+script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			b, rerr := os.ReadFile(filepath.Join(d, "ready"))
			ready := strings.TrimSpace(string(b))
			destroyed := strings.Contains(string(out), "DESTROYED")
			if _, serr := os.Stat(filepath.Join(dir, "bootstrap-ran")); (serr == nil) != tc.bootstrapRan {
				t.Errorf("bootstrap ran: %v, want %v", serr == nil, tc.bootstrapRan)
			}
			if tc.ready != "" {
				if err != nil || destroyed || ready != tc.ready {
					t.Fatalf("scheduled timers: err %v, ready %q (%v), want %q: %s", err, ready, rerr, tc.ready, out)
				}
				return
			}
			if err == nil || !destroyed {
				t.Fatalf("boot went on without its shutdown timers: %v: %s", err, out)
			}
			want := "1"
			if tc.status != 0 {
				want = strconv.Itoa(tc.status)
			}
			if ready != want {
				t.Fatalf("ready %q (%v), want the failed boot recorded as %q: %s", ready, rerr, want, out)
			}
			if lb, _ := os.ReadFile(log); !strings.Contains(string(lb), "shutdown timers aren't running") {
				t.Errorf("bootstrap log doesn't say why: %q", lb)
			}
		})
	}
}
