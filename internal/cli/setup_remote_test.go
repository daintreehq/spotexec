package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/hooks"
)

// fakeHost runs remote setup's scripts on this machine: a stub ssh runs the
// remote command with /bin/sh, and stub brew, curl and login shell stand in
// for the remote host's.
type fakeHost struct {
	t    *testing.T
	dir  string // stubs and their state
	home string // the remote $HOME
	out  bytes.Buffer
}

const fakeSSH = `#!/bin/sh
# Fake ssh: run the remote command here, logging "<batch> <host> <command>".
batch=prompts
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; break ;;
    -O) exit 0 ;;
    -o) if [ "$2" = BatchMode=yes ]; then batch=batch; fi; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s %s %.30s\n' "$batch" "$1" "$2" >> "$FAKE/ssh.log"
if grep -qx "$1" "$FAKE/unreachable" 2>/dev/null; then echo "ssh: connect to host $1: Connection refused" >&2; exit 255; fi
shift
cd "$HOME" || exit 1
exec /bin/sh -c "$*"
`

// The fake spotexec: version from the file next to it (or baked in).
const fakeSpotexec = `#!/bin/sh
v=$(cat "$(dirname "$0")/.version" 2>/dev/null || echo VERSION)
case "$1" in
  version|--version) echo "spotexec $v" ;;
  setup)
    for a; do
      if [ "$a" = --admin-from-env ] && [ -f "$FAKE/old-spotexec" ]; then echo "Error: unknown flag: $a" >&2; exit 1; fi
    done
    for a; do
      if [ "$a" = --help ]; then
        [ -f "$FAKE/old-hooks" ] || echo "      --hooks      install the Claude Code hook without asking"
        echo "      --no-hooks   don't offer the Claude Code hook"; exit 0
      fi
    done
    echo "remote setup $*"; echo "admin key: ${AWS_ACCESS_KEY_ID:-none}"
    if [ -f "$FAKE/setup-fails" ]; then exit 1; fi ;;
esac
`

const fakeBrew = `#!/bin/sh
d="$FAKE/brew"
echo "$*" >> "$FAKE/brew.log"
case "$1" in
  update) exit 0 ;;
  --prefix) echo "$d" ;;
  list) [ -f "$d/bin/.version" ] && echo "spotexec $(cat "$d/bin/.version")" || exit 1 ;;
  install|upgrade)
    if [ -f "$FAKE/brew-fails" ]; then echo "Error: Your Command Line Tools are too outdated."; exit 1; fi
    mkdir -p "$d/bin"; cp "$FAKE/spotexec-stub" "$d/bin/spotexec"; ln -sf spotexec "$d/bin/sx"
    cat "$FAKE/brew-version" > "$d/bin/.version" ;;
esac
`

// curl serves files from $FAKE/dl by name.
const fakeCurl = `#!/bin/sh
o=
while [ $# -gt 1 ]; do case "$1" in -o) o=$2; shift 2 ;; *) shift ;; esac; done
f="$FAKE/dl/$(basename "$1")"
[ -f "$f" ] || { echo "curl: (22) 404 $1" >&2; exit 22; }
echo "$1" >> "$FAKE/curl.log"
if [ -n "$o" ]; then cp "$f" "$o"; else cat "$f"; fi
`

// The login shell's PATH comes from $FAKE/login-path, where @INHERIT@ is
// the PATH it was started with.
const fakeLoginShell = `#!/bin/sh
PATH=$(sed "s|@INHERIT@|$PATH|" "$FAKE/login-path"); export PATH
[ "$1" = -lc ] && shift
exec /bin/sh -c "$1"
`

func newFakeHost(t *testing.T, home string) *fakeHost {
	t.Helper()
	f := &fakeHost{t: t, dir: t.TempDir(), home: home}
	for _, d := range []string{"ssh", "stub", "dl", "brew/bin"} {
		if err := os.MkdirAll(filepath.Join(f.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("ssh/ssh", fakeSSH, 0o755)
	f.write("stub/brew", fakeBrew, 0o755)
	f.write("stub/curl", fakeCurl, 0o755)
	f.write("stub/loginsh", fakeLoginShell, 0o755)
	f.write("spotexec-stub", fakeSpotexec, 0o755)
	f.write("local-config.toml", "version = 1\n", 0o644)
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(f.dir, "local-config.toml"))
	t.Setenv("SPOTEXEC_CONFIG_DIR", filepath.Join(f.dir, "local-config"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("FAKE", f.dir)
	t.Setenv("HOME", home)
	t.Setenv("SHELL", filepath.Join(f.dir, "stub", "loginsh"))
	// Only the system's tools: nothing installed here leaks in.
	t.Setenv("PATH", filepath.Join(f.dir, "ssh")+":/usr/bin:/bin:/usr/sbin:/sbin")
	// Like the real one: Homebrew before ~/.local/bin.
	old := remotePath
	remotePath = `PATH=` + shq(filepath.Join(f.dir, "brew", "bin")+":"+filepath.Join(f.dir, "stub")) + `:"$HOME/.local/bin:$PATH"; export PATH; `
	t.Cleanup(func() { remotePath = old })
	f.loginPath(filepath.Join(f.dir, "brew", "bin"), filepath.Join(home, ".local", "bin"))
	return f
}

func (f *fakeHost) write(name, content string, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
}

// loginPath sets the PATH a login shell there gets (before /usr/bin:/bin).
func (f *fakeHost) loginPath(dirs ...string) {
	f.write("login-path", strings.Join(append(dirs, "/usr/bin", "/bin"), ":"), 0o644)
}

// release publishes a fake release archive for version v.
func (f *fakeHost) release(v string) []byte {
	f.t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	body := strings.Replace(fakeSpotexec, "VERSION", v, 1)
	_ = tw.WriteHeader(&tar.Header{Name: "spotexec", Mode: 0o755, Size: int64(len(body))})
	_, _ = io.WriteString(tw, body)
	_ = tw.Close()
	_ = gz.Close()
	f.write("dl/spotexec_"+v+"_"+f.goos()+"_"+f.goarch()+".tar.gz", b.String(), 0o644)
	return b.Bytes()
}

func (f *fakeHost) goos() string {
	i, err := f.setup().probe(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return i.os
}

func (f *fakeHost) goarch() string {
	i, _ := f.setup().probe(context.Background())
	return i.arch
}

// brewInstalled puts Homebrew's spotexec v there; brewOffers is what an
// upgrade installs.
func (f *fakeHost) brewInstalled(v string) {
	f.write("brew/bin/spotexec", fakeSpotexec, 0o755)
	f.write("brew/bin/.version", v, 0o644)
	_ = os.Symlink("spotexec", filepath.Join(f.dir, "brew", "bin", "sx"))
}

func (f *fakeHost) brewOffers(v string) { f.write("brew-version", v, 0o644) }

// localInstalled puts spotexec v in ~/.local/bin there.
func (f *fakeHost) localInstalled(v string) {
	d := filepath.Join(f.home, ".local", "bin")
	if err := os.MkdirAll(d, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "spotexec"), []byte(strings.Replace(fakeSpotexec, "VERSION", v, 1)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	_ = os.Symlink("spotexec", filepath.Join(d, "sx"))
}

func (f *fakeHost) setup() *remoteSetup {
	return &remoteSetup{host: "box", install: "auto", ctlDir: f.dir, out: &f.out, profile: "spotexec"}
}

// versionAt runs the spotexec at path and returns its version.
func versionAt(t *testing.T, path string) string {
	t.Helper()
	b, err := exec.Command(path, "version").Output()
	if err != nil {
		return ""
	}
	return versionOf(string(b))
}

// withLatest makes tag the latest release, with checksums.txt listing the
// archives in the fake download directory (or sums, when given).
func withLatest(t *testing.T, tag string) {
	old, oldSums := latestRelease, releaseChecksums
	latestRelease = func(context.Context) (string, error) { return tag, nil }
	releaseChecksums = func(_ context.Context, got string) (map[string]string, error) {
		if got != tag {
			t.Errorf("checksums for %s, not %s", got, tag)
		}
		var b strings.Builder
		files, _ := filepath.Glob(filepath.Join(os.Getenv("FAKE"), "dl", "*"))
		for _, f := range files {
			data, _ := os.ReadFile(f)
			fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(data), filepath.Base(f))
		}
		if p := filepath.Join(os.Getenv("FAKE"), "checksums.txt"); fileExists(p) {
			data, _ := os.ReadFile(p)
			return parseChecksums(string(data)), nil
		}
		return parseChecksums(b.String()), nil
	}
	t.Cleanup(func() { latestRelease, releaseChecksums = old, oldSums })
}

func (f *fakeHost) install(r *remoteSetup) (installed, error) {
	f.t.Helper()
	i, err := r.probe(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return r.installSpotexec(context.Background(), i)
}

func TestRemoteInstallBrewUpgradeFailsFallsBack(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	withLatest(t, "v0.3.0")
	f.brewInstalled("0.2.0")
	f.write("brew-fails", "", 0o644)
	f.release("0.3.0")
	r := f.setup()
	in, err := f.install(r)
	if err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if in.bin != localBin || in.want != "0.3.0" || !strings.Contains(f.out.String(), "Command Line Tools are too outdated") || !strings.Contains(f.out.String(), "using the release binary instead") {
		t.Fatalf("%+v\n%s", in, f.out.String())
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.3.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
	// Homebrew's old copy comes first in a login shell: not ready.
	err = r.checkLoginShell(context.Background(), in.dir, in.want)
	var nr *notReadyError
	if !errors.As(err, &nr) || !strings.Contains(err.Error(), "spotexec 0.2.0") || !strings.Contains(err.Error(), "brew upgrade spotexec") {
		t.Fatalf("checkLoginShell = %v", err)
	}
	f.loginPath(filepath.Join(f.home, ".local", "bin"), filepath.Join(f.dir, "brew", "bin"))
	if err := r.checkLoginShell(context.Background(), in.dir, in.want); err != nil {
		t.Fatalf("checkLoginShell = %v", err)
	}
}

func TestRemoteInstallBrewStaleVersion(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	withLatest(t, "v0.3.0")
	f.brewOffers("0.2.0") // the tap is behind
	f.localInstalled("0.1.0")
	f.release("0.3.0")
	// --install brew: an error, and ~/.local/bin is left alone.
	r := f.setup()
	r.install = "brew"
	if _, err := f.install(r); err == nil || !strings.Contains(err.Error(), "not the latest release (0.3.0)") {
		t.Fatalf("err = %v", err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.1.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
	// auto: falls back to the release binary.
	in, err := f.install(f.setup())
	if err != nil || in.want != "0.3.0" {
		t.Fatalf("%+v %v", in, err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.3.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
}

func TestRemoteInstallBrewReplacesOlderLocalCopy(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	withLatest(t, "v0.3.0")
	f.brewOffers("0.3.0")
	f.localInstalled("0.2.0")
	r := f.setup()
	in, err := f.install(r)
	if err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if in.want != "0.3.0" || in.bin != shq(filepath.Join(f.dir, "brew", "bin", "spotexec")) || in.dir != filepath.Join(f.dir, "brew", "bin") {
		t.Fatalf("%+v", in)
	}
	for _, n := range []string{"spotexec", "sx"} {
		if _, err := os.Lstat(filepath.Join(f.home, ".local/bin", n)); !os.IsNotExist(err) {
			t.Fatalf("~/.local/bin/%s still there (%v)", n, err)
		}
	}
	if err := r.checkLoginShell(context.Background(), in.dir, in.want); err != nil {
		t.Fatal(err)
	}
	// A newer copy there (a development build) stays.
	f.localInstalled("0.4.0")
	if _, err := f.install(f.setup()); err != nil {
		t.Fatal(err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.4.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
}

// An older modified build that an earlier setup copied into ~/.local/bin
// (#98) goes too, so a login shell finds Homebrew's.
func TestRemoteInstallBrewReplacesOlderModifiedLocalCopy(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	withLatest(t, "v0.4.0")
	f.brewOffers("0.4.0")
	f.localInstalled("v0.2.0-10-g6fb2e4a-dirty")
	// As on that machine: ~/.local/bin first, so the old copy shadows.
	f.loginPath(filepath.Join(f.home, ".local", "bin"), filepath.Join(f.dir, "brew", "bin"))
	r := f.setup()
	in, err := f.install(r)
	if err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	for _, n := range []string{"spotexec", "sx"} {
		if _, err := os.Lstat(filepath.Join(f.home, ".local/bin", n)); !os.IsNotExist(err) {
			t.Fatalf("~/.local/bin/%s still there (%v)\n%s", n, err, f.out.String())
		}
	}
	if err := r.checkLoginShell(context.Background(), in.dir, in.want); err != nil {
		t.Fatal(err)
	}
}

// An sx in ~/.local/bin that isn't the link setup made stays.
func TestRemoteInstallBrewKeepsOtherSx(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	withLatest(t, "v0.3.0")
	f.brewOffers("0.3.0")
	f.localInstalled("0.2.0")
	sx := filepath.Join(f.home, ".local/bin/sx")
	_ = os.Remove(sx)
	if err := os.Symlink("/usr/bin/true", sx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.install(f.setup()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".local/bin/spotexec")); !os.IsNotExist(err) {
		t.Fatalf("~/.local/bin/spotexec still there (%v)", err)
	}
	if target, err := os.Readlink(sx); err != nil || target != "/usr/bin/true" {
		t.Fatalf("~/.local/bin/sx = %q, %v", target, err)
	}
}

func TestLocalCopyNoNewer(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"0.3.0", true}, {"0.2.0", true}, {"v0.3.0-2-gabc123", false}, {"0.4.0", false},
		{"v0.2.0-10-g6fb2e4a-dirty", true}, {"v0.2.9-dirty", true},
		{"v0.3.0-dirty", false}, {"v0.3.0-2-gabc123-dirty", false},
		{"dev", false}, {"", false}, {"0.3.0-rc1", false}, {"0.2.0-rc1-dirty", false}, {"v0.2.0-dirty-dirty", false},
	} {
		if got := localCopyNoNewer(c.v, "0.3.0"); got != c.want {
			t.Errorf("localCopyNoNewer(%q, 0.3.0) = %v", c.v, got)
		}
	}
}

func TestCheckLoginShell(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	f.localInstalled("0.3.0")
	r := f.setup()
	ctx := context.Background()
	if err := r.checkLoginShell(ctx, "$HOME/.local/bin", "0.3.0"); err != nil {
		t.Fatal(err)
	}
	f.loginPath() // ~/.local/bin isn't on PATH
	err := r.checkLoginShell(ctx, "$HOME/.local/bin", "0.3.0")
	if err == nil || !strings.Contains(err.Error(), "doesn't find spotexec") || !strings.Contains(err.Error(), `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Fatalf("err = %v", err)
	}
	f.loginPath(filepath.Join(f.home, ".local", "bin"))
	_ = os.Remove(filepath.Join(f.home, ".local/bin/sx"))
	if err := r.checkLoginShell(ctx, "$HOME/.local/bin", "0.3.0"); err == nil || !strings.Contains(err.Error(), "not sx") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"0.4.0", "0.3.9", 1, true}, {"0.3.0", "0.3.0", 0, true}, {"0.10.0", "0.9.0", 1, true}, {"0.2.0", "0.3.0", -1, true},
		{"v0.3.0-5-gabc123", "0.3.0", 1, true}, {"0.3.0-5-gabc123", "0.3.1", -1, true}, {"v0.3.0", "0.3.0", 0, true},
		{"dev", "0.3.0", 0, false}, {"", "0.3.0", 0, false}, {"v0.3.0-5-gabc123-dirty", "0.3.0", 0, false}, {"0.3.0-rc1", "0.3.0", 0, false}, {"abc1234", "0.3.0", 0, false},
	} {
		if cmp, ok := compareVersions(c.a, c.b); cmp != c.cmp || ok != c.ok {
			t.Errorf("compareVersions(%q, %q) = %d, %v", c.a, c.b, cmp, ok)
		}
	}
}

// The login shell check sees what a new terminal there gets, not setup's
// own PATH additions.
func TestCheckLoginShellIgnoresSetupPath(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	f.localInstalled("0.3.0")
	f.loginPath("@INHERIT@") // a profile that keeps the inherited PATH
	err := f.setup().checkLoginShell(context.Background(), "$HOME/.local/bin", "0.3.0")
	if err == nil || !strings.Contains(err.Error(), "doesn't find spotexec") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckLoginShellShadowedSx(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	f.localInstalled("0.3.0")
	old := filepath.Join(f.dir, "oldsx")
	f.write("oldsx/sx", strings.Replace(fakeSpotexec, "VERSION", "0.1.0", 1), 0o755)
	f.loginPath(old, filepath.Join(f.home, ".local", "bin"))
	err := f.setup().checkLoginShell(context.Background(), "$HOME/.local/bin", "0.3.0")
	if err == nil || !strings.Contains(err.Error(), "runs sx from "+filepath.Join(old, "sx")) {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoteInstallReleaseKeepsNewerLocalCopy(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	withLatest(t, "v0.3.0")
	f.release("0.3.0")
	f.localInstalled("0.3.0-4-gabc123")
	in, err := f.install(f.setup())
	if err != nil || in.want != "0.3.0-4-gabc123" || in.bin != localBin {
		t.Fatalf("%+v %v", in, err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.3.0-4-gabc123" {
		t.Fatalf("~/.local/bin has %q", v)
	}
	// An older one is replaced.
	f.localInstalled("0.2.0")
	if in, err := f.install(f.setup()); err != nil || in.want != "0.3.0" {
		t.Fatalf("%+v %v", in, err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.3.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
}

// A download that doesn't run as the release it claims to be replaces
// nothing.
func TestRemoteInstallReleaseBadDownloadKeepsOld(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	withLatest(t, "v0.3.0")
	b := f.release("0.2.9") // reports the wrong version
	f.write("dl/spotexec_0.3.0_"+f.goos()+"_"+f.goarch()+".tar.gz", string(b), 0o644)
	f.localInstalled("0.2.0")
	if _, err := f.install(f.setup()); err == nil || !strings.Contains(err.Error(), "doesn't run") {
		t.Fatalf("err = %v", err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.2.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
	if m, _ := filepath.Glob(filepath.Join(f.home, ".local/bin/.spotexec-install.*")); len(m) > 0 {
		t.Fatalf("left %v", m)
	}
}

// Homebrew's cleanup only removes a copy known to be no newer.
func TestRemoteInstallBrewKeepsDevBuilds(t *testing.T) {
	for _, v := range []string{"dev", "0.3.0-2-gabc123", "0.3.0-2-gabc123-dirty"} {
		f := newFakeHost(t, t.TempDir())
		withLatest(t, "v0.3.0")
		f.brewOffers("0.3.0")
		f.localInstalled(v)
		if _, err := f.install(f.setup()); err != nil {
			t.Fatal(err)
		}
		if got := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); got != v {
			t.Fatalf("%s: ~/.local/bin has %q", v, got)
		}
	}
}

// A spotexec that prints the right version but fails doesn't count.
func TestRemoteInstallFailingBinaries(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	withLatest(t, "v0.3.0")
	f.release("0.3.0")
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	body := strings.Replace(fakeSpotexec, "VERSION", "0.3.0", 1) + "exit 1\n"
	_ = tw.WriteHeader(&tar.Header{Name: "spotexec", Mode: 0o755, Size: int64(len(body))})
	_, _ = io.WriteString(tw, body)
	_ = tw.Close()
	_ = gz.Close()
	f.write("dl/spotexec_0.3.0_"+f.goos()+"_"+f.goarch()+".tar.gz", tb.String(), 0o644)
	f.localInstalled("0.2.0")
	if _, err := f.install(f.setup()); err == nil {
		t.Fatal("installed a failing binary")
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.2.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
	// And the login shell check needs both commands to succeed.
	f.write("bad/spotexec", body, 0o755)
	_ = os.Symlink("spotexec", filepath.Join(f.dir, "bad", "sx"))
	f.loginPath(filepath.Join(f.dir, "bad"))
	if err := f.setup().checkLoginShell(context.Background(), "$HOME/.local/bin", "0.3.0"); err == nil {
		t.Fatal("a failing spotexec passed")
	}
}

// Real login shells, each with its own profile syntax.
func TestCheckLoginShellRealShells(t *testing.T) {
	for _, c := range []struct{ shell, file, line string }{
		{"/bin/bash", ".bash_profile", `export PATH="$HOME/.local/bin:$PATH"`},
		{"/bin/zsh", ".zprofile", `export PATH="$HOME/.local/bin:$PATH"`},
		{"/bin/tcsh", ".tcshrc", `set path = ( ~/.local/bin $path )`},
	} {
		if _, err := os.Stat(c.shell); err != nil {
			continue
		}
		t.Run(filepath.Base(c.shell), func(t *testing.T) {
			f := newFakeHost(t, filepath.Join(t.TempDir(), "my home"))
			t.Setenv("SHELL", c.shell)
			t.Setenv("ZDOTDIR", f.home) // zsh reads its profiles from the fake home
			f.localInstalled("99.0.0")  // a version no real install on this machine has
			r := f.setup()
			err := r.checkLoginShell(context.Background(), "$HOME/.local/bin", "99.0.0")
			if err == nil { // not found, or some other spotexec installed here
				t.Fatal("ready without a profile")
			}
			if err := os.WriteFile(filepath.Join(f.home, c.file), []byte(c.line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := r.checkLoginShell(context.Background(), "$HOME/.local/bin", "99.0.0"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A download that doesn't match the release's checksum replaces nothing.
func TestRemoteInstallReleaseChecksumMismatch(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	withLatest(t, "v0.3.0")
	f.release("0.3.0")
	f.localInstalled("0.2.0")
	name := "spotexec_0.3.0_" + f.goos() + "_" + f.goarch() + ".tar.gz"
	f.write("checksums.txt", strings.Repeat("ab", 32)+"  "+name+"\n", 0o644)
	_, err := f.install(f.setup())
	if err == nil || !strings.Contains(err.Error(), "doesn't match the release's checksum") {
		t.Fatalf("err = %v", err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.2.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
	if m, _ := filepath.Glob(filepath.Join(f.home, ".local/bin/.spotexec-install.*")); len(m) > 0 {
		t.Fatalf("left %v", m)
	}
	// No checksum for the archive at all: nothing is downloaded.
	f.write("checksums.txt", strings.Repeat("ab", 32)+"  other.tar.gz\n", 0o644)
	if _, err := f.install(f.setup()); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("err = %v", err)
	}
	// The right checksum installs it.
	_ = os.Remove(filepath.Join(f.dir, "checksums.txt"))
	if in, err := f.install(f.setup()); err != nil || in.want != "0.3.0" {
		t.Fatalf("%+v %v", in, err)
	}
	if v := versionAt(t, filepath.Join(f.home, ".local/bin/spotexec")); v != "0.3.0" {
		t.Fatalf("~/.local/bin has %q", v)
	}
}

// A host already on the latest release downloads nothing.
func TestRemoteInstallAlreadyCurrent(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	withLatest(t, "v0.3.0")
	f.localInstalled("0.3.0")
	in, err := f.install(f.setup())
	if err != nil || in.want != "0.3.0" || !strings.Contains(in.how, "already installed") {
		t.Fatalf("%+v %v", in, err)
	}
	if fileExists(filepath.Join(f.dir, "curl.log")) {
		t.Fatal("downloaded it again")
	}
	// ... even when it says v0.3.0, which setup then accepts; a missing sx
	// is put back.
	f.localInstalled("v0.3.0")
	_ = os.Remove(filepath.Join(f.home, ".local/bin/sx"))
	if err := f.setup().do(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if fileExists(filepath.Join(f.dir, "curl.log")) {
		t.Fatal("downloaded it again")
	}
	if l, _ := os.Readlink(filepath.Join(f.home, ".local/bin/sx")); l != "spotexec" {
		t.Fatalf("sx -> %q", l)
	}
	// Homebrew's copy: no update or upgrade.
	g := newFakeHost(t, t.TempDir())
	withLatest(t, "v0.3.0")
	g.brewInstalled("0.3.0")
	if in, err := g.install(g.setup()); err != nil || in.want != "0.3.0" {
		t.Fatalf("%+v %v", in, err)
	}
	if b, _ := os.ReadFile(filepath.Join(g.dir, "brew.log")); strings.Contains(string(b), "update") || strings.Contains(string(b), "upgrade") {
		t.Fatalf("brew ran: %s", b)
	}
}

func TestParseChecksums(t *testing.T) {
	h := strings.Repeat("0f", 32)
	got := parseChecksums(h + "  spotexec_0.3.0_linux_amd64.tar.gz\n" + strings.ToUpper(h) + " *b.tar.gz\nnot a line\n" + "abc  short.tar.gz\n")
	if len(got) != 2 || got["spotexec_0.3.0_linux_amd64.tar.gz"] != h || got["b.tar.gz"] != h {
		t.Fatalf("%v", got)
	}
}

// Remote setup passes only this machine's session, and says how to get one
// when a host needs it.
func TestRemoteSetupAdminSession(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	f.localInstalled("0.3.0")
	withLatest(t, "v0.3.0")
	r := f.setup()
	r.aws, r.adminEnv, r.adminARN = true, []string{"AKIAHERE", "secret", "token"}, "arn:aws:iam::1:user/me"
	// AWS setup there leaves a machine ID, which setup must read back.
	if err := r.do(context.Background()); err == nil || !strings.Contains(err.Error(), "machine ID can't be read") {
		t.Fatalf("err = %v\n%s", err, f.out.String())
	}
	f.write("local-config/owner-id", "aaaa-1111\n", 0o644)
	f.out.Reset()
	r = f.setup()
	r.aws, r.adminEnv, r.adminARN = true, []string{"AKIAHERE", "secret", "token"}, "arn:aws:iam::1:user/me"
	if err := r.do(context.Background()); err != nil || r.machineID != "aaaa-1111" {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if out := f.out.String(); !strings.Contains(out, "--admin-from-env") || !strings.Contains(out, "admin key: AKIAHERE") {
		t.Fatalf("output:\n%s", out)
	}
	// Not signed in here: nothing is passed, and a failure says what to do.
	f.out.Reset()
	r = f.setup()
	r.aws, r.adminErr, r.adminProfile = true, errors.New("token expired"), "admin"
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIALEAK")
	f.write("setup-fails", "", 0o644)
	r.profile, r.noVPC = "team", true
	err := r.do(context.Background())
	if err == nil || !strings.Contains(err.Error(), "`aws sso login --profile 'admin'`") || !strings.Contains(err.Error(), "`spotexec setup --admin-profile 'admin' --profile 'team' --no-vpc box`") {
		t.Fatalf("err = %v", err)
	}
	if out := f.out.String(); !strings.Contains(out, "--admin-from-env") || !strings.Contains(out, "admin key: none") {
		t.Fatalf("output:\n%s", out)
	}
}

// A remote spotexec too old for --admin-from-env isn't run with AWS.
func TestRemoteSetupTooOld(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	f.localInstalled("0.2.0")
	f.write("old-spotexec", "", 0o644)
	r := f.setup()
	r.install, r.aws = "none", true
	if err := r.do(context.Background()); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("err = %v\n%s", err, f.out.String())
	}
	if strings.Contains(f.out.String(), "remote setup") {
		t.Fatalf("ran setup:\n%s", f.out.String())
	}
	// Without AWS runners it's fine.
	r = f.setup()
	r.install = "none"
	if err := r.do(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
}

// A session that is a machine's own spotexec user isn't passed on: it can't
// manage IAM users.
func TestSetupHostsSpotexecUserSession(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	f.localInstalled("0.3.0")
	withLatest(t, "v0.3.0")
	old := adminSession
	var gotProfile string
	var gotRegions []string
	adminSession = func(_ context.Context, profile string, regions []string) ([]string, string, error) {
		gotProfile, gotRegions = profile, regions
		return []string{"AKIASPOTEXEC", "s", ""}, "arn:aws:iam::1:user/spotexec/spotexec-here", nil
	}
	t.Cleanup(func() { adminSession = old })
	f.write("local-config/owner-id", "aaaa-1111\n", 0o644)
	// The runners' regions pick the partition the session is checked in.
	f.write("local-config.toml", "version = 1\n\n[runners.gov]\ntype = \"aws-spot\"\nregion = \"us-gov-west-1\"\n\n[runners.gov2]\ntype = \"aws-spot\"\nregion = \"us-gov-east-1\"\n", 0o644)
	out := captureStdout(t, func() {
		if err := setupHosts(context.Background(), []string{"box"}, remoteSetup{install: "auto", profile: "spotexec", aws: true}); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "can't manage IAM users") || !strings.Contains(out, "admin key: none") {
		t.Fatalf("output:\n%s", out)
	}
	if gotProfile != "" || strings.Join(gotRegions, ",") != "us-gov-east-1,us-gov-west-1" && strings.Join(gotRegions, ",") != "us-gov-west-1,us-gov-east-1" {
		t.Fatalf("admin session checked for %q in %v", gotProfile, gotRegions)
	}
}

func TestRetryCommand(t *testing.T) {
	for _, c := range []struct {
		r    remoteSetup
		want string
	}{
		{remoteSetup{host: "box", install: "auto", profile: "spotexec"}, "spotexec setup box"},
		{remoteSetup{host: "box", install: "release", profile: "spotexec"}, "spotexec setup --install release box"},
		{remoteSetup{host: "box", install: "release", fellBack: true, profile: "spotexec"}, "spotexec setup box"},
		{remoteSetup{host: "me@box", install: "copy", profile: "team", adminProfile: "adm", rotate: true, noVPC: true, noHooks: true},
			"spotexec setup --admin-profile 'adm' --profile 'team' --install copy --rotate --no-vpc --no-hooks me@box"},
		{remoteSetup{host: "box", install: "auto", profile: "spotexec", hooks: true}, "spotexec setup --hooks box"},
		{remoteSetup{host: "[2001:db8::1]", install: "auto", profile: "spotexec"}, "spotexec setup '[2001:db8::1]'"},
	} {
		if got := c.r.retryCommand(); got != c.want {
			t.Errorf("retryCommand() = %q, want %q", got, c.want)
		}
	}
}

// Hosts are connected to one at a time, in the foreground, before the
// parallel setup, which then never prompts.
func TestSetupHostsConnectsFirst(t *testing.T) {
	f := newFakeHost(t, filepath.Join(t.TempDir(), "my home"))
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	f.localInstalled("0.3.0")
	withLatest(t, "v0.3.0")
	f.write("unreachable", "gone\n", 0o644)
	var err error
	out := captureStdout(t, func() {
		err = setupHosts(context.Background(), []string{"a", "gone", "b"}, remoteSetup{install: "auto", profile: "spotexec"})
	})
	if err == nil || err.Error() != "setup failed on gone" {
		t.Fatalf("err = %v\n%s\n%s", err, out, readFile(filepath.Join(f.dir, "ssh.log")))
	}
	if !strings.Contains(out, "✓ a is ready") || !strings.Contains(out, "✓ b is ready") || !strings.Contains(out, "✗ gone: can't connect over SSH") {
		t.Fatalf("output:\n%s", out)
	}
	if strings.Contains(out, "--no-hooks") || strings.Contains(out, "--hooks") {
		t.Fatalf("a hook flag passed:\n%s", out)
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, "ssh.log"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 4 || lines[0] != "prompts a true" || lines[1] != "prompts gone true" || lines[2] != "prompts b true" {
		t.Fatalf("ssh calls:\n%s", b)
	}
	for _, l := range lines[3:] {
		if !strings.HasPrefix(l, "batch ") || strings.HasPrefix(l, "batch gone") {
			t.Fatalf("ssh calls:\n%s", b)
		}
	}
	// The machines were remembered.
	var hosts []string
	for _, m := range loadMachines() {
		hosts = append(hosts, m.Host)
	}
	if strings.Join(hosts, ",") != "a,b" && strings.Join(hosts, ",") != "b,a" {
		t.Fatalf("remembered %v", hosts)
	}
}

// Paths with spaces, both $HOME and an existing install.
func TestRemoteSetupPathsWithSpaces(t *testing.T) {
	f := newFakeHost(t, filepath.Join(t.TempDir(), "my home"))
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	withLatest(t, "v0.3.0")
	f.release("0.3.0")
	r := f.setup()
	if err := r.do(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), "remote setup setup --yes") {
		t.Fatalf("output:\n%s", f.out.String())
	}
	if _, err := os.Stat(filepath.Join(f.home, ".config", "spotexec", "config.toml")); err != nil {
		t.Fatal(err)
	}
	// --install none runs whatever is on PATH there, by its full path.
	r = f.setup()
	r.install, r.noHooks = "none", true
	f.out.Reset()
	if err := r.do(context.Background()); err != nil || !strings.Contains(f.out.String(), "--no-hooks") {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	// A hook asked for here is asked for there.
	r = f.setup()
	r.install, r.hooks = "none", true
	f.out.Reset()
	if err := r.do(context.Background()); err != nil || !strings.Contains(f.out.String(), "--yes --profile spotexec --skip-aws --hooks") {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
}

// Without Claude Code here, the hook isn't turned down for other machines,
// and --hooks still asks for it there.
func TestSetupClaudeHookNotHere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	captureStdout(t, func() {
		for _, c := range [][3]bool{{true, false, false}, {false, false, false}, {false, false, true}} {
			if in, declined := setupClaudeHook(c[0], c[1], c[2]); in || declined {
				t.Errorf("setupClaudeHook%v = %v, %v", c, in, declined)
			}
		}
		if in, _ := setupClaudeHook(true, true, false); !in {
			t.Error("--hooks not passed on")
		}
	})
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")); err == nil {
		t.Fatal("installed without Claude Code")
	}
}

// The hook is opt-in: --yes and a run without a terminal leave it out, and
// only --hooks (or a yes when asked) installs it.
func TestSetupClaudeHookOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("SPOTEXEC_SETUP_NESTED", "")
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	out := captureStdout(t, func() {
		for _, yes := range []bool{true, false} { // tests have no terminal on stdin
			if in, declined := setupClaudeHook(yes, false, false); in || declined {
				t.Errorf("yes=%v: %v, %v", yes, in, declined)
			}
		}
	})
	if _, err := os.Stat(settings); err == nil {
		t.Fatalf("installed without --hooks:\n%s", out)
	}
	if !strings.Contains(out, "spotexec setup --hooks") {
		t.Fatalf("no hint:\n%s", out)
	}
	// Nested in another machine's setup, that machine says what to do.
	t.Setenv("SPOTEXEC_SETUP_NESTED", "1")
	if out := captureStdout(t, func() { setupClaudeHook(true, false, false) }); strings.Contains(out, "--hooks") {
		t.Fatalf("hint in nested setup:\n%s", out)
	}
	out = captureStdout(t, func() {
		if in, declined := setupClaudeHook(true, true, false); !in || declined {
			t.Errorf("--hooks: %v, %v", in, declined)
		}
	})
	if !hooks.HasClaude(settings) || !strings.Contains(out, "✓ installed the Claude Code hook") {
		t.Fatalf("--hooks didn't install it:\n%s", out)
	}
	// Once installed, a plain run leaves it and doesn't pass it on.
	out = captureStdout(t, func() {
		if in, _ := setupClaudeHook(true, false, false); in {
			t.Error("passed on without --hooks")
		}
	})
	if !hooks.HasClaude(settings) || !strings.Contains(out, "✓ Claude Code hook in") {
		t.Fatalf("output:\n%s", out)
	}
	// A hook left calling spotexec where an old install put it is updated
	// to call this one.
	stale := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/gone/bin/spotexec hook claude","timeout":10}]}]}}`
	if err := os.WriteFile(settings, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		// A refresh is no opt-in to pass on.
		if in, declined := setupClaudeHook(true, false, false); in || declined {
			t.Errorf("refresh: %v, %v", in, declined)
		}
	})
	want := `"command": "` + transportQuote(hookBinary()) + ` hook claude"`
	if got := readFile(settings); !strings.Contains(got, want) || !strings.Contains(out, "✓ updated the Claude Code hook") {
		t.Fatalf("want %s:\n%s\n%s", want, got, out)
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// The machine list records the machine a destination reached.
func TestSetupHostsRecordsIdentity(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	f.localInstalled("0.3.0")
	withLatest(t, "v0.3.0")
	// The fake "remote" shares this environment, so its config dir is ours.
	f.write("local-config/owner-id", "aaaa-1111\n", 0o644)
	captureStdout(t, func() {
		if err := setupHosts(context.Background(), []string{"builder"}, remoteSetup{install: "auto", profile: "spotexec"}); err != nil {
			t.Error(err)
		}
	})
	ms := loadMachines()
	h, _ := os.Hostname()
	if len(ms) != 1 || ms[0].Host != "builder" || ms[0].MachineID != "aaaa-1111" || ms[0].Hostname != h {
		t.Fatalf("machines %+v", ms)
	}
}

// claudeHome isolates Claude Code's settings in a fresh home that has
// Claude Code, optionally with PreToolUse hook commands already there.
func claudeHome(t *testing.T, commands ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("SPOTEXEC_SETUP_NESTED", "")
	p := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if len(commands) > 0 {
		var hs []string
		for _, c := range commands {
			hs = append(hs, `{"type":"command","command":"`+c+`"}`)
		}
		b := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[` + strings.Join(hs, ",") + `]}]}}`
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// In a terminal the question defaults to no, and only a yes installs.
func TestSetupClaudeHookPrompt(t *testing.T) {
	for _, c := range []struct {
		answer            string
		yes               bool
		installed, asked  bool
		optedIn, declined bool
	}{
		{"\n", false, false, true, false, true},
		{"n\n", false, false, true, false, true},
		{"", false, false, true, false, true}, // EOF
		{"y\n", false, true, true, true, false},
		{"YES\n", false, true, true, true, false},
		{"y\n", true, false, false, false, false}, // --yes doesn't ask
	} {
		settings := claudeHome(t)
		var in, declined bool
		out := captureStdout(t, func() {
			in, declined = setupClaudeHookFrom(c.yes, false, false, true, strings.NewReader(c.answer))
		})
		if in != c.optedIn || declined != c.declined || hooks.HasClaude(settings) != c.installed || strings.Contains(out, "[y/N]") != c.asked {
			t.Errorf("%q yes=%v: optedIn %v, declined %v, installed %v\n%s", c.answer, c.yes, in, declined, hooks.HasClaude(settings), out)
		}
	}
}

// --no-hooks neither installs the hook nor touches an installed one, which
// is otherwise kept current.
func TestSetupClaudeHookMaintenance(t *testing.T) {
	settings := claudeHome(t)
	captureStdout(t, func() { setupClaudeHookFrom(false, false, true, true, strings.NewReader("y\n")) })
	if _, err := os.Stat(settings); err == nil {
		t.Fatal("--no-hooks installed it")
	}
	// v0.2.0's hook, and RTK's beside spotexec's, which setup chains.
	for _, old := range [][]string{{"sx hook claude"}, {"spotexec hook claude", "rtk hook claude"}} {
		settings = claudeHome(t, old...)
		before, _ := os.ReadFile(settings)
		captureStdout(t, func() { setupClaudeHookFrom(true, false, true, false, nil) })
		if after, _ := os.ReadFile(settings); string(after) != string(before) {
			t.Fatalf("%s: --no-hooks changed it:\n%s", old, after)
		}
		var in bool
		out := captureStdout(t, func() { in, _ = setupClaudeHookFrom(true, false, false, false, nil) })
		after, _ := os.ReadFile(settings)
		if in || string(after) == string(before) || hooks.HasLegacyClaude(settings) || !strings.Contains(out, "✓") {
			t.Fatalf("%s: optedIn %v, not kept current:\n%s\n%s", old, in, after, out)
		}
		// Asked for here, an installed hook is asked for on other machines.
		if in, _ := setupClaudeHookFrom(true, true, false, false, nil); !in {
			t.Fatalf("%s: --hooks not passed on", old)
		}
	}
}

func TestSetupHooksFlagsConflict(t *testing.T) {
	for _, args := range [][]string{{"--hooks", "--no-hooks"}, {"--hooks", "--no-hooks", "--print-policy"}} {
		c := cmdSetup()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "can't be used together") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

// A spotexec there from before the hook was opt-in would install it with
// --yes and doesn't know --hooks.
func TestRemoteSetupOldHookDefault(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
	f.localInstalled("0.3.0")
	f.write("old-hooks", "", 0o644)
	for _, c := range []struct {
		hooks bool
		want  string
	}{{false, "--skip-aws --no-hooks\n"}, {true, "--skip-aws\n"}} {
		r := f.setup()
		r.install, r.hooks = "none", c.hooks
		f.out.Reset()
		if err := r.do(context.Background()); err != nil || !strings.Contains(f.out.String(), c.want) {
			t.Fatalf("hooks=%v: %v\n%s", c.hooks, err, f.out.String())
		}
	}
}

// copyFrom makes --install copy send each body in turn, once each.
func copyFrom(t *testing.T, bodies ...io.Reader) {
	t.Helper()
	var mu sync.Mutex
	old := selfBinary
	selfBinary = func() (io.ReadCloser, int64, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			return nil, 0, errors.New("test: no more copies to send")
		}
		b := bodies[0]
		bodies = bodies[1:]
		n := int64(-1)
		if l, ok := b.(interface{ Len() int }); ok {
			n = int64(l.Len())
		}
		return io.NopCloser(b), n, nil
	}
	t.Cleanup(func() { selfBinary = old })
}

// sizedReader reports a size other than what it holds.
type sizedReader struct {
	io.Reader
	n int
}

func (s sizedReader) Len() int { return s.n }

// Two copy installs that overlap each stage their own copy: both succeed,
// and ~/.local/bin ends with one complete spotexec and its sx link.
func TestRemoteInstallCopyConcurrent(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	f.localInstalled("0.1.0")
	bin := filepath.Join(f.home, ".local", "bin")
	// An sx that links to a directory is replaced, not descended into.
	other := filepath.Join(f.dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bin, "sx")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(bin, "sx")); err != nil {
		t.Fatal(err)
	}
	stub := strings.Replace(fakeSpotexec, "VERSION", Version, 1)
	a := stub + "# installer A\n"
	b := stub + "# installer B, which has a longer body\n"
	pr, pw := io.Pipe()
	copyFrom(t, sizedReader{pr, len(a)}, strings.NewReader(b))
	i, err := f.setup().probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ra, rb := f.setup(), f.setup()
	ra.install, rb.install = "copy", "copy"
	ra.out, rb.out = io.Discard, io.Discard
	done := make(chan error, 1)
	go func() {
		_, err := ra.installSpotexec(context.Background(), i)
		// Writes to A's pipe fail once A is done, so the test never blocks.
		pr.CloseWithError(fmt.Errorf("installer A returned: %v", err))
		done <- err
	}()
	// A has finished, whatever happens, before the fake host goes.
	var aErr error
	joined := false
	join := func() error {
		if !joined {
			pw.CloseWithError(errors.New("test over"))
			aErr, joined = <-done, true
		}
		return aErr
	}
	t.Cleanup(func() { join() })
	// A's copy is half written there when B runs from start to finish.
	half := len(a) / 2
	if _, err := io.WriteString(pw, a[:half]); err != nil {
		t.Fatal(err)
	}
	waitForStaged(t, bin, half)
	if in, err := rb.installSpotexec(context.Background(), i); err != nil || in.bin != localBin {
		t.Fatalf("B: %+v %v", in, err)
	}
	// B's copy is in place while A is still sending.
	if got := readFile(filepath.Join(bin, "spotexec")); got != b {
		t.Fatalf("after B, ~/.local/bin/spotexec is\n%s", got)
	}
	if l, err := os.Readlink(filepath.Join(bin, "sx")); err != nil || l != "spotexec" {
		t.Fatalf("after B, sx -> %q %v", l, err)
	}
	if _, err := io.WriteString(pw, a[half:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	if err := join(); err != nil {
		t.Fatalf("A: %v", err)
	}
	if got := readFile(filepath.Join(bin, "spotexec")); got != a {
		t.Fatalf("~/.local/bin/spotexec is\n%s", got)
	}
	if l, err := os.Readlink(filepath.Join(bin, "sx")); err != nil || l != "spotexec" {
		t.Fatalf("sx -> %q %v", l, err)
	}
	if v := versionAt(t, filepath.Join(bin, "sx")); v != Version {
		t.Fatalf("sx runs as %q", v)
	}
	if left := hiddenEntries(t, bin); len(left) > 0 {
		t.Fatalf("left %v", left)
	}
	if es, err := os.ReadDir(other); err != nil || len(es) > 0 {
		t.Fatalf("the directory sx linked to now holds %v (%v)", es, err)
	}
}

// A real directory where the sx link goes is left as it is: the new
// spotexec is still installed, and nothing is written into the directory.
func TestRemoteInstallCopySxDirectory(t *testing.T) {
	f := newFakeHost(t, t.TempDir())
	f.localInstalled("0.1.0")
	bin := filepath.Join(f.home, ".local", "bin")
	if err := os.Remove(filepath.Join(bin, "sx")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "sx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sx", "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	copyFrom(t, strings.NewReader(strings.Replace(fakeSpotexec, "VERSION", Version, 1)))
	r := f.setup()
	r.install = "copy"
	if _, err := f.install(r); err != nil {
		t.Fatal(err)
	}
	if v := versionAt(t, filepath.Join(bin, "spotexec")); v != Version {
		t.Fatalf("~/.local/bin has %q", v)
	}
	if es, err := os.ReadDir(filepath.Join(bin, "sx")); err != nil || len(es) != 1 || es[0].Name() != "keep" {
		t.Fatalf("sx/ now holds %v (%v)", es, err)
	}
	if left := hiddenEntries(t, bin); len(left) > 0 {
		t.Fatalf("left %v", left)
	}
}

// A copy that arrives short, or doesn't run, replaces nothing and leaves
// nothing behind.
func TestRemoteInstallCopyBadCopyKeepsOld(t *testing.T) {
	stub := strings.Replace(fakeSpotexec, "VERSION", Version, 1)
	for name, c := range map[string]struct {
		body io.Reader
		err  string
	}{
		"short":     {sizedReader{strings.NewReader(stub), len(stub) + 10}, "not this spotexec's"},
		"long":      {sizedReader{strings.NewReader(stub + "# extra\n"), len(stub)}, "not this spotexec's"},
		"wrong":     {strings.NewReader(strings.Replace(fakeSpotexec, "VERSION", "0.0.1", 1)), "doesn't run"},
		"failing":   {strings.NewReader(stub + "exit 1\n"), "doesn't run"},
		"truncated": {sizedReader{strings.NewReader(stub[:20]), len(stub)}, "not this spotexec's"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t, t.TempDir())
			f.localInstalled("0.1.0")
			copyFrom(t, c.body)
			r := f.setup()
			r.install = "copy"
			if _, err := f.install(r); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v", err)
			}
			bin := filepath.Join(f.home, ".local", "bin")
			if v := versionAt(t, filepath.Join(bin, "spotexec")); v != "0.1.0" {
				t.Fatalf("~/.local/bin has %q", v)
			}
			if l, err := os.Readlink(filepath.Join(bin, "sx")); err != nil || l != "spotexec" {
				t.Fatalf("sx -> %q %v", l, err)
			}
			if left := hiddenEntries(t, bin); len(left) > 0 {
				t.Fatalf("left %v", left)
			}
		})
	}
}

// fakeBusyBoxMv fails like BusyBox's mv, which looks through the entry it
// replaces: a link there that loops or leads nowhere stops it.
const fakeBusyBoxMv = `#!/bin/sh
for last; do :; done
for a; do case "$a" in -*) ;; *) src=$a; break ;; esac; done
dest=$last; [ -d "$dest" ] && [ ! -L "$dest" ] && dest="$dest/$(basename "$src")"
dest=${dest%/}
if [ -L "$dest" ] && [ ! -e "$dest" ]; then echo "mv: can't stat '$dest': Too many levels of symbolic links" >&2; exit 1; fi
exec /bin/mv "$@"
`

// Both release installs, a new download and an already current one, put
// back an sx that links to a directory or to nothing without touching what
// it pointed at, and leave a real directory named sx alone.
func TestRemoteInstallReleaseRepairsSx(t *testing.T) {
	for _, current := range []bool{false, true} {
		for _, target := range []string{"dir", "loop", "realdir"} {
			t.Run(fmt.Sprintf("current=%v/%s", current, target), func(t *testing.T) {
				f := newFakeHost(t, t.TempDir())
				_ = os.Remove(filepath.Join(f.dir, "stub", "brew"))
				f.write("stub/mv", fakeBusyBoxMv, 0o755)
				withLatest(t, "v0.3.0")
				f.release("0.3.0")
				if current {
					f.localInstalled("0.3.0")
				} else {
					f.localInstalled("0.2.0")
				}
				bin := filepath.Join(f.home, ".local", "bin")
				sx := filepath.Join(bin, "sx")
				other := filepath.Join(f.dir, "other")
				if err := os.Mkdir(other, 0o755); err != nil {
					t.Fatal(err)
				}
				_ = os.Remove(sx)
				to := other
				if target == "loop" {
					to = "sx"
				}
				if target == "realdir" {
					if err := os.Mkdir(sx, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(to, sx); err != nil {
					t.Fatal(err)
				}
				if in, err := f.install(f.setup()); err != nil || in.want != "0.3.0" {
					t.Fatalf("%+v %v", in, err)
				}
				if v := versionAt(t, filepath.Join(bin, "spotexec")); v != "0.3.0" {
					t.Fatalf("~/.local/bin has %q", v)
				}
				if target == "realdir" {
					if es, err := os.ReadDir(sx); err != nil || len(es) > 0 {
						t.Fatalf("sx/ now holds %v (%v)", es, err)
					}
					if left := hiddenEntries(t, bin); len(left) > 0 {
						t.Fatalf("left %v", left)
					}
					return
				}
				if l, err := os.Readlink(sx); err != nil || l != "spotexec" {
					t.Fatalf("sx -> %q %v", l, err)
				}
				if v := versionAt(t, sx); v != "0.3.0" {
					t.Fatalf("sx runs as %q", v)
				}
				if es, err := os.ReadDir(other); err != nil || len(es) > 0 {
					t.Fatalf("the directory sx linked to now holds %v (%v)", es, err)
				}
				if left := hiddenEntries(t, bin); len(left) > 0 {
					t.Fatalf("left %v", left)
				}
				if current && fileExists(filepath.Join(f.dir, "curl.log")) {
					t.Fatal("downloaded it again")
				}
			})
		}
	}
}

// waitForStaged waits until some hidden file under dir holds n bytes.
func waitForStaged(t *testing.T, dir string, n int) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		found := false
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			rel, _ := filepath.Rel(dir, p)
			if err == nil && d.Type().IsRegular() && strings.HasPrefix(rel, ".") {
				if st, err := d.Info(); err == nil && st.Size() == int64(n) {
					found = true
				}
			}
			return nil
		})
		if found {
			return
		}
	}
	t.Fatalf("nothing staged under %s", dir)
}

// hiddenEntries lists what an install left in dir besides spotexec and sx.
func hiddenEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range es {
		if n := e.Name(); n != "spotexec" && n != "sx" {
			left = append(left, n)
		}
	}
	return left
}
