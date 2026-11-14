package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
# Fake ssh: run the remote command here.
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; break ;;
    -O) exit 0 ;;
    -o) shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$FAKE_SSH_LOG" ] && printf '%s\n' "$1" >> "$FAKE_SSH_LOG"
shift
cd "$HOME" || exit 1
exec /bin/sh -c "$*"
`

// The fake spotexec: version from the file next to it (or baked in).
const fakeSpotexec = `#!/bin/sh
v=$(cat "$(dirname "$0")/.version" 2>/dev/null || echo VERSION)
case "$1" in
  version|--version) echo "spotexec $v" ;;
  setup) [ "$2" = --help ] && exit 0; echo "remote setup $*"; echo "admin key: ${AWS_ACCESS_KEY_ID:-none}" ;;
esac
`

const fakeBrew = `#!/bin/sh
d="$FAKE/brew"
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

func withLatest(t *testing.T, tag string) {
	old := latestRelease
	latestRelease = func(context.Context) (string, error) { return tag, nil }
	t.Cleanup(func() { latestRelease = old })
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
			f.localInstalled("0.3.0")
			r := f.setup()
			err := r.checkLoginShell(context.Background(), "$HOME/.local/bin", "0.3.0")
			if err == nil { // not found, or some other spotexec installed here
				t.Fatal("ready without a profile")
			}
			if err := os.WriteFile(filepath.Join(f.home, c.file), []byte(c.line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := r.checkLoginShell(context.Background(), "$HOME/.local/bin", "0.3.0"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
