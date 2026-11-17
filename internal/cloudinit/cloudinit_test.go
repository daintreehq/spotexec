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
	// Mounted before the key that lets anyone write to it is authorized.
	if strings.Index(s, "\nsx_workvol()") > strings.Index(s, "authorized_keys") || strings.Index(s, "\nsx_workvol()") < strings.Index(s, "spotexec-watchdog.timer") {
		t.Fatal("work volume set up out of order")
	}
	for _, bad := range []string{"", "~/a b", "/srv/$(id)", "/srv/x'y", "/srv/../etc", "/srv/~x", "~user/x"} {
		o.WorkRoot = bad
		if _, err := Render(o); err == nil {
			t.Errorf("work root %q accepted", bad)
		}
	}
}

func TestWorkRootOK(t *testing.T) {
	for p, want := range map[string]bool{"~": true, "~/.cache/spotexec": true, "/mnt/work": true, "work": true, "/a/b-c_d.e+f": true,
		"": false, "~user": false, "/a/~b": false, "/a b": false, "/a/../b": false, "/a\n": false, `/a"b`: false} {
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
		name, size string
		parts, fs  bool
	}
	run := func(t *testing.T, root string, disks []disk, prep func(home string)) (log, calls, fstab string, err error) {
		t.Helper()
		dir := t.TempDir()
		home := filepath.Join(dir, "home")
		if root == "" {
			root = "~/.cache/spotexec"
		}
		root = strings.ReplaceAll(root, "$TMP", dir)
		os.MkdirAll(home, 0o755)
		if prep != nil {
			prep(home)
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
		fn := strings.ReplaceAll(s[start:start+end+3], "/etc/fstab", filepath.Join(dir, "fstab"))
		var list, parts, fs strings.Builder
		for _, d := range disks {
			fmt.Fprintf(&list, "echo '%s %s disk'; ", d.name, d.size)
			if d.parts {
				fmt.Fprintf(&parts, "%s) printf '%%s\\n%%s1\\n' $3 $3 ;; ", d.name)
			}
			if d.fs {
				fmt.Fprintf(&fs, "%s) return 0 ;; ", d.name)
			}
		}
		stubs := `
C=` + filepath.Join(dir, "calls") + `
getent() { echo "ubuntu:x:1000:1000::` + home + `:/bin/bash"; }
id() { echo ubuntu; }
sleep() { :; }
lsblk() { case "$1" in -dbnpo) ` + list.String() + `;; -npo) case "$3" in ` + parts.String() + `*) echo "$3" ;; esac ;; esac; }
blkid() { if [ "$1" = -p ]; then case "$2" in ` + fs.String() + `*) return 2 ;; esac; fi; echo uuid-1; }
mkfs.xfs() { echo "mkfs $*" >> "$C"; }
runuser() { while [ "$1" != -- ]; do shift; done; shift; "$@"; }
mount() { echo "mount $*" >> "$C"; }
umount() { echo "umount $*" >> "$C"; }
chown() { echo "chown $*" >> "$C"; }
`
		out, err := exec.Command("bash", "-c", "set -u\n"+stubs+fn+"sx_workvol").CombinedOutput()
		c, _ := os.ReadFile(filepath.Join(dir, "calls"))
		f, _ := os.ReadFile(filepath.Join(dir, "fstab"))
		return strings.ReplaceAll(string(out), home, "$HOME"), strings.ReplaceAll(strings.ReplaceAll(string(c), home, "$HOME"), dir, "$TMP"), strings.ReplaceAll(strings.ReplaceAll(string(f), home, "$HOME"), dir, "$TMP"), err
	}
	root := disk{name: "/dev/nvme0n1", size: gib64, parts: true}
	t.Run("the blank disk of its size", func(t *testing.T) {
		log, calls, fstab, err := run(t, "", []disk{root, {name: "/dev/nvme1n1", size: "75000000000"}, {name: "/dev/nvme2n1", size: gib64}}, nil)
		if err != nil {
			t.Fatalf("%v: %s", err, log)
		}
		want := "mkfs -q -K -m reflink=1 -L spotexecwork /dev/nvme2n1\nmount -t xfs -o noatime /dev/nvme2n1 $HOME/.cache/spotexec\nchown ubuntu:ubuntu $HOME/.cache/spotexec\n"
		if calls != want || fstab != "UUID=uuid-1 $HOME/.cache/spotexec xfs noatime,nofail 0 0\n" {
			t.Fatalf("calls:\n%s\nfstab: %q\nlog: %s", calls, fstab, log)
		}
	})
	t.Run("an absolute work root", func(t *testing.T) {
		_, calls, fstab, err := run(t, "$TMP/srv/work", []disk{root, {name: "/dev/xvdf", size: gib64}}, nil)
		if err != nil || !strings.Contains(calls, "mount -t xfs -o noatime /dev/xvdf $TMP/srv/work\n") || !strings.HasPrefix(fstab, "UUID=uuid-1 $TMP/srv/work ") {
			t.Fatalf("%v\n%s\n%s", err, calls, fstab)
		}
	})
	for name, c := range map[string]struct {
		disks []disk
		prep  func(home string)
		log   string
	}{
		"no such disk":          {[]disk{root, {name: "/dev/nvme1n1", size: "75000000000"}}, nil, "found 0 blank disks of 64 GiB"},
		"two such disks":        {[]disk{root, {name: "/dev/nvme1n1", size: gib64}, {name: "/dev/nvme2n1", size: gib64}}, nil, "found 2 blank disks"},
		"only a used disk":      {[]disk{root, {name: "/dev/nvme1n1", size: gib64, fs: true}}, nil, "found 0 blank disks"},
		"work root has content": {[]disk{root, {name: "/dev/nvme1n1", size: gib64}}, func(h string) { os.MkdirAll(filepath.Join(h, ".cache/spotexec/bin"), 0o755) }, "is in use"},
		"work root is a link": {[]disk{root, {name: "/dev/nvme1n1", size: gib64}}, func(h string) {
			os.MkdirAll(filepath.Join(h, ".cache"), 0o755)
			os.Symlink("/tmp", filepath.Join(h, ".cache/spotexec"))
		}, "is in use"},
	} {
		t.Run(name, func(t *testing.T) {
			log, calls, fstab, err := run(t, "", c.disks, c.prep)
			if err == nil || !strings.Contains(log, c.log) || calls != "" || fstab != "" {
				t.Fatalf("err %v, log %q, calls %q, fstab %q", err, log, calls, fstab)
			}
		})
	}
}
