package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// remoteSetup copies this machine's spotexec setup to another machine over
// SSH: it installs spotexec there, copies the runner config, and (for AWS
// runners) runs `spotexec setup` there with the current AWS sign-in, so the
// machine gets its own IAM user and key.
type remoteSetup struct {
	host         string
	install      string // auto | brew | release | copy | none
	adminProfile string
	profile      string
	rotate       bool
	noVPC        bool
	hooks        bool // install the Claude Code hook there (asked for here)
	noHooks      bool
	aws          bool
	ctlDir       string
	out          io.Writer // this host's output (buffered when hosts run in parallel)
	// The AWS sign-in passed to each host's setup, resolved once.
	adminEnv []string
	adminARN string
	adminErr error
	fellBack bool // auto install fell back from Homebrew to the release
	// The machine the host reached: its hostname and spotexec machine ID.
	hostname, machineID string
	// The newest release, looked up once.
	latestTag string
	latestErr error
}

func (r *remoteSetup) printf(format string, args ...any) { fmt.Fprintf(r.out, format, args...) }

var remotePath = `PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:$PATH"; export PATH; `

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// checkSSHHost rejects a destination that ssh could read as an option, or
// that can't be one host: setup takes hosts from the command line,
// ~/.ssh/config and Bonjour, which anything on the local network can answer.
func checkSSHHost(h string) error {
	if h == "" {
		return errors.New("empty host name")
	}
	if strings.HasPrefix(h, "-") {
		return fmt.Errorf("invalid host %q: it starts with '-'", h)
	}
	for _, c := range h {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return fmt.Errorf("invalid host %q: it contains whitespace or control characters", h)
		}
	}
	return nil
}

func checkSSHHosts(hosts []string) error {
	for _, h := range hosts {
		if err := checkSSHHost(h); err != nil {
			return err
		}
	}
	return nil
}

// sshOpts are the options of every ssh to the host: one shared connection,
// opened by connect and kept until close, however long the host waits for
// its turn (the limit only matters if setup is killed).
func (r *remoteSetup) sshOpts() []string {
	return []string{
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + filepath.Join(r.ctlDir, "%C"), "-o", "ControlPersist=4h",
		"-o", "ConnectTimeout=15",
	}
}

// sshArgs runs script with /bin/sh there, exactly as given, over the
// connection connect opened. BatchMode: it never prompts, so hosts set up in
// parallel can't ask for passwords on the same terminal at once.
func (r *remoteSetup) sshArgs(script string) []string {
	// "--" ends option parsing, so the host is never read as an option.
	return append(r.sshOpts(), "-o", "BatchMode=yes", "--", r.host, "/bin/sh -c "+shq(script))
}

// connect opens the shared connection in the foreground, so ssh can ask
// about the host key or for a password on the terminal.
func (r *remoteSetup) connect(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "ssh", append(r.sshOpts(), "--", r.host, "true")...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// run runs a shell script on the host, with the usual install locations on
// PATH, returning its stdout.
func (r *remoteSetup) run(ctx context.Context, script string, stdin io.Reader) (string, error) {
	return r.runAsIs(ctx, remotePath+script, stdin)
}

// runAsIs is run without the PATH additions.
func (r *remoteSetup) runAsIs(ctx context.Context, script string, stdin io.Reader) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(script)...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 1500 {
			msg = "…" + msg[len(msg)-1500:]
		}
		return out.String(), fmt.Errorf("%v: %s", err, msg)
	}
	return out.String(), nil
}

// stream runs a script, printing its output indented as it arrives.
func (r *remoteSetup) stream(ctx context.Context, script string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(remotePath+script)...)
	cmd.Stdin = stdin
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			r.printf("%s\n", "    "+sc.Text())
		}
		close(done)
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	return err
}

type remoteInfo struct {
	os, arch, hostname, brew, spotexec, version string
}

func (r *remoteSetup) probe(ctx context.Context) (remoteInfo, error) {
	out, err := r.run(ctx, `uname -s; uname -m; hostname; command -v brew || echo -; `+
		`p=$(command -v spotexec || echo -); echo "$p"; if [ "$p" != - ]; then "$p" version 2>/dev/null | head -1; fi`, nil)
	if err != nil {
		return remoteInfo{}, fmt.Errorf("can't run commands over SSH: %w", err)
	}
	f := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(f) < 6 {
		f = append(f, "")
	}
	i := remoteInfo{os: strings.ToLower(f[0]), arch: f[1], hostname: f[2], brew: f[3], spotexec: f[4], version: strings.TrimPrefix(f[5], "spotexec ")}
	switch i.arch {
	case "x86_64", "amd64":
		i.arch = "amd64"
	case "aarch64", "arm64":
		i.arch = "arm64"
	}
	if i.brew == "-" {
		i.brew = ""
	}
	if i.spotexec == "-" {
		i.spotexec = ""
	}
	if i.os != "darwin" && i.os != "linux" {
		return i, fmt.Errorf("unsupported system %s/%s", f[0], f[1])
	}
	return i, nil
}

const releasesURL = "https://github.com/daintreehq/spotexec/releases"

// latestRelease returns the newest release tag (e.g. "v0.2.0").
var latestRelease = func(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, releasesURL+"/latest", nil)
	c := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if i := strings.LastIndex(loc, "/tag/"); i >= 0 {
		return loc[i+5:], nil
	}
	return "", fmt.Errorf("can't find the latest release (HTTP %d)", resp.StatusCode)
}

// releaseChecksums returns the SHA-256 of each file in a release, from its
// checksums.txt.
var releaseChecksums = func(ctx context.Context, tag string) (map[string]string, error) {
	u := releasesURL + "/download/" + tag + "/checksums.txt"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseChecksums(string(b)), nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseChecksums reads sha256sum output: "<hash>  <file>" per line.
func parseChecksums(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && sha256Hex.MatchString(strings.ToLower(f[0])) {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

// latest returns the newest release tag, looked up once per host.
func (r *remoteSetup) latest(ctx context.Context) (string, error) {
	if r.latestTag == "" && r.latestErr == nil {
		r.latestTag, r.latestErr = latestRelease(ctx)
	}
	return r.latestTag, r.latestErr
}

// versionOf picks the version out of `spotexec version` output ("" if none).
func versionOf(out string) string {
	v := ""
	for _, l := range strings.Split(out, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(l), "spotexec "); ok {
			v = strings.TrimSpace(s)
		}
	}
	return v
}

// compareVersions compares two spotexec versions: releases ("0.3.0") and
// builds between them (`git describe`: "v0.3.0-5-gabc123", which is newer
// than 0.3.0). ok is false when either can't be placed, such as "dev" or a
// modified build.
func compareVersions(a, b string) (cmp int, ok bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

// localCopyNoNewer reports whether a spotexec reporting version v is known
// to be no newer than release want. A modified build ("-dirty", as an
// earlier `--install copy` can leave) is placed by the commit it was built
// from: it counts only when that commit is older than the release, since
// its changes could be newer than the release itself.
func localCopyNoNewer(v, want string) bool {
	if base, dirty := strings.CutSuffix(v, "-dirty"); dirty {
		c, ok := compareVersions(base, want)
		return ok && c < 0
	}
	c, ok := compareVersions(v, want)
	return ok && c <= 0
}

// parseVersion reads X.Y.Z, with `git describe`'s "-N-gHASH" as a fourth
// part.
func parseVersion(v string) ([4]int, bool) {
	var p [4]int
	v = strings.TrimPrefix(v, "v")
	base, rest, _ := strings.Cut(v, "-")
	f := strings.Split(base, ".")
	if len(f) != 3 {
		return p, false
	}
	if rest != "" {
		n, hash, ok := strings.Cut(rest, "-")
		if !ok || !strings.HasPrefix(hash, "g") || strings.Contains(hash, "-") {
			return p, false // a pre-release or a modified ("-dirty") build
		}
		f = append(f, n)
	}
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p, false
		}
		p[i] = n
	}
	return p, true
}

// installed is a spotexec installed on a host.
type installed struct {
	bin  string // the shell word that runs it there
	dir  string // its directory, as a PATH entry in a shell profile
	how  string // how it was installed
	want string // the version it must report ("" when that isn't known)
}

const localBin = `"$HOME/.local/bin/spotexec"`

// installSpotexec installs or upgrades spotexec.
func (r *remoteSetup) installSpotexec(ctx context.Context, i remoteInfo) (installed, error) {
	mode := r.install
	if mode == "auto" {
		mode = "release"
		if i.brew != "" {
			mode = "brew"
		}
	}
	switch mode {
	case "brew":
		if i.brew == "" {
			return installed{}, errors.New("Homebrew isn't installed there (use --install release)")
		}
		want := ""
		if tag, err := r.latest(ctx); err == nil {
			want = strings.TrimPrefix(tag, "v")
		}
		r.printf("%s\n", "  installing spotexec with Homebrew (latest)…")
		// Left alone when Homebrew's copy is already the latest release.
		out, err := r.run(ctx, `p="$(brew --prefix)/bin/spotexec"; `+
			`if [ -z `+shq(want)+` ] || [ "$("$p" version 2>/dev/null)" != `+shq("spotexec "+want)+` ]; then `+
			`brew update --quiet >/dev/null 2>&1; `+
			`if brew list --versions spotexec >/dev/null 2>&1; then verb=upgrade; else verb=install; fi; `+
			`out=$(brew "$verb" daintreehq/tap/spotexec 2>&1) || { printf '%s\n' "$out" | tail -n 20; exit 1; }; fi; `+
			`echo "path=$p"; v=$("$p" version 2>&1) || { echo "Error: Homebrew's spotexec doesn't run: $v"; exit 1; }; echo "$v"`, nil)
		path, got := "", ""
		if err == nil {
			for _, l := range strings.Split(out, "\n") {
				if s, ok := strings.CutPrefix(l, "path="); ok {
					path = s
				}
			}
			switch got = versionOf(out); {
			case path == "" || got == "":
				err = fmt.Errorf("Homebrew's spotexec doesn't run: %s", firstLine(strings.TrimSpace(out)))
			case want != "" && got != want:
				err = fmt.Errorf("Homebrew installed spotexec %s, not the latest release (%s)", got, want)
			}
		} else {
			err = errors.New(brewError(out + err.Error()))
		}
		if err != nil {
			if r.install == "auto" {
				r.printf("  Homebrew couldn't install it (%v); using the release binary instead\n", err)
				r.install, r.fellBack = "release", true
				return r.installSpotexec(ctx, i)
			}
			return installed{}, fmt.Errorf("brew install failed: %v", err)
		}
		// Homebrew's copy works and is current: an earlier one in
		// ~/.local/bin would shadow it, unless that one is newer still.
		old, _ := r.run(ctx, `f="$HOME/.local/bin/spotexec"; if [ -f "$f" ]; then echo present; "$f" version 2>/dev/null | head -n 1; fi`, nil)
		if strings.HasPrefix(old, "present") {
			// Only one known to be no newer goes, with the sx link setup
			// made beside it (never another sx).
			v := versionOf(old)
			if !localCopyNoNewer(v, got) {
				r.printf("  kept spotexec %s in ~/.local/bin (not known to be older than Homebrew's)\n", orDash(v))
			} else if _, err := r.run(ctx, `d="$HOME/.local/bin"; rm -f "$d/spotexec"; if [ -L "$d/sx" ] && [ "$(readlink "$d/sx")" = spotexec ]; then rm -f "$d/sx"; fi`, nil); err == nil {
				r.printf("  removed an older spotexec from ~/.local/bin\n")
			}
		}
		return installed{bin: shq(path), dir: filepath.Dir(path), how: "Homebrew", want: got}, nil
	case "release":
		tag, err := r.latest(ctx)
		if err != nil {
			return installed{}, err
		}
		want := strings.TrimPrefix(tag, "v")
		// The same version stays as it is; so does a newer build (a
		// development copy).
		old, _ := r.run(ctx, `f="$HOME/.local/bin/spotexec"; if [ -x "$f" ] && v=$("$f" version 2>&1); then echo "$v"; fi`, nil)
		if v := versionOf(old); v != "" {
			if c, ok := compareVersions(v, want); ok && c > 0 {
				return installed{bin: localBin, dir: "$HOME/.local/bin", how: "kept, newer than release " + tag, want: v}, nil
			} else if ok && c == 0 {
				// Only the sx link is put right, in case it went missing.
				if _, err := r.run(ctx, `d="$HOME/.local/bin"; [ "$(readlink "$d/sx")" = spotexec ] || ln -sf spotexec "$d/sx"`, nil); err != nil {
					return installed{}, err
				}
				return installed{bin: localBin, dir: "$HOME/.local/bin", how: "release " + tag + " in ~/.local/bin, already installed", want: v}, nil
			}
		}
		name := fmt.Sprintf("spotexec_%s_%s_%s.tar.gz", want, i.os, i.arch)
		sums, err := releaseChecksums(ctx, tag)
		if err != nil {
			return installed{}, fmt.Errorf("getting the checksums of release %s: %w", tag, err)
		}
		sum := sums[name]
		if sum == "" {
			return installed{}, fmt.Errorf("release %s lists no checksum for %s", tag, name)
		}
		url := releasesURL + "/download/" + tag + "/" + name
		r.printf("  installing spotexec %s into ~/.local/bin…\n", tag)
		// Downloaded and unpacked next to the old one, which is replaced
		// only once the archive matches the release's checksum and the new
		// binary runs.
		if _, err := r.run(ctx, `set -e; d="$HOME/.local/bin"; mkdir -p "$d"; t=$(mktemp -d "$d/.spotexec-install.XXXXXX"); trap 'rm -rf "$t"' EXIT; `+
			`curl -fsSL -o "$t/a.tar.gz" `+shq(url)+`; `+
			`if command -v sha256sum >/dev/null 2>&1; then s=$(sha256sum "$t/a.tar.gz"); `+
			`elif command -v shasum >/dev/null 2>&1; then s=$(shasum -a 256 "$t/a.tar.gz"); `+
			`else echo "neither sha256sum nor shasum is installed, so the download can't be verified" >&2; exit 1; fi; `+
			`[ "${s%% *}" = `+shq(sum)+` ] || { echo "the download doesn't match the release's checksum (got ${s%% *})" >&2; exit 1; }; `+
			`tar -xzf "$t/a.tar.gz" -C "$t" spotexec; `+
			`v=$("$t/spotexec" version 2>&1) && [ "$v" = `+shq("spotexec "+want)+` ] || { echo "the downloaded spotexec doesn't run: $v" >&2; exit 1; }; `+
			`mv -f "$t/spotexec" "$d/spotexec"; ln -sf spotexec "$d/sx"`, nil); err != nil {
			return installed{}, fmt.Errorf("installing %s: %w", url, err)
		}
		return installed{bin: localBin, dir: "$HOME/.local/bin", how: "release " + tag + " in ~/.local/bin", want: want}, nil
	case "copy":
		if i.os != runtime.GOOS || i.arch != runtime.GOARCH {
			return installed{}, fmt.Errorf("--install copy needs the same platform (this is %s/%s, that is %s/%s)", runtime.GOOS, runtime.GOARCH, i.os, i.arch)
		}
		self, err := os.Executable()
		if err != nil {
			return installed{}, err
		}
		f, err := os.Open(self)
		if err != nil {
			return installed{}, err
		}
		defer f.Close()
		r.printf("  copying this spotexec (%s) into ~/.local/bin…\n", Version)
		if _, err := r.run(ctx, `set -e; mkdir -p "$HOME/.local/bin"; cat > "$HOME/.local/bin/.spotexec.tmp"; `+
			`chmod 755 "$HOME/.local/bin/.spotexec.tmp"; mv "$HOME/.local/bin/.spotexec.tmp" "$HOME/.local/bin/spotexec"; `+
			`ln -sf spotexec "$HOME/.local/bin/sx"`, f); err != nil {
			return installed{}, err
		}
		return installed{bin: localBin, dir: "$HOME/.local/bin", how: "copied from this machine into ~/.local/bin", want: Version}, nil
	case "none":
		if i.spotexec == "" {
			return installed{}, errors.New("spotexec isn't installed there (drop --install none)")
		}
		return installed{bin: shq(i.spotexec), dir: filepath.Dir(i.spotexec), how: "already installed"}, nil
	}
	return installed{}, fmt.Errorf("--install must be auto, brew, release, copy or none (got %q)", r.install)
}

// notReadyError marks a host that was set up but where spotexec can't be
// used from a fresh shell yet.
type notReadyError struct{ msg string }

func (e *notReadyError) Error() string { return e.msg }

// checkLoginShell confirms that a fresh login shell there finds spotexec and
// sx, and runs the version just installed.
func (r *remoteSetup) checkLoginShell(ctx context.Context, dir, want string) error {
	// The PATH a new terminal there gets (not setup's own additions), read
	// with `env` so any shell's syntax works; the checks then run in sh.
	out, err := r.runAsIs(ctx, `sh="${SHELL:-/bin/sh}"; case "$sh" in *csh) e=$("$sh" -c env </dev/null 2>/dev/null) ;; `+
		`*) e=$("$sh" -lc env </dev/null 2>/dev/null) ;; esac; `+
		`PATH=$(printf '%s\n' "$e" | sed -n 's/^PATH=//p' | tail -n 1); `+
		`[ -n "$PATH" ] || { echo "@@nopath="; exit 0; }; export PATH; `+
		`echo "@@spotexec=$(command -v spotexec)"; echo "@@sx=$(command -v sx)"; `+
		`if v=$(spotexec version 2>&1); then echo "@@version=$v"; fi; if v=$(sx --version 2>&1); then echo "@@sxversion=$v"; fi`, nil)
	if err != nil {
		return &notReadyError{fmt.Sprintf("can't check a login shell there: %v", err)}
	}
	if strings.Contains(out, "@@nopath=") {
		return &notReadyError{"a login shell there doesn't start (`$SHELL -l`)"}
	}
	v := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, val, ok := strings.Cut(strings.TrimPrefix(l, "@@"), "="); ok && strings.HasPrefix(l, "@@") {
			v[k] = strings.TrimSpace(val)
		}
	}
	got := versionOf(v["version"])
	switch {
	case v["spotexec"] == "":
		return &notReadyError{"a login shell there doesn't find spotexec" + r.pathHint(ctx, dir)}
	case want != "" && got != want:
		what := "spotexec " + got
		if got == "" {
			what = "a spotexec that doesn't run"
		}
		msg := fmt.Sprintf("a login shell there runs %s (%s), not spotexec %s", v["spotexec"], what, want)
		if strings.Contains(v["spotexec"], "brew") || strings.HasPrefix(v["spotexec"], "/opt/homebrew/") || strings.HasPrefix(v["spotexec"], "/usr/local/") {
			msg += "; fix Homebrew there (`brew doctor`) and run `brew upgrade spotexec`, or remove its copy with `brew uninstall spotexec`"
		} else {
			msg += "; remove that copy, or put the new one's directory first on PATH"
		}
		return &notReadyError{msg}
	case v["sx"] == "":
		return &notReadyError{fmt.Sprintf("a login shell there finds %s but not sx: ln -s spotexec %s", v["spotexec"], shq(filepath.Join(filepath.Dir(v["spotexec"]), "sx")))}
	case versionOf(v["sxversion"]) != got:
		return &notReadyError{fmt.Sprintf("a login shell there runs sx from %s, which isn't this spotexec (%s): remove it, or ln -sf spotexec %s",
			v["sx"], orDash(v["sxversion"]), shq(filepath.Join(filepath.Dir(v["spotexec"]), "sx")))}
	}
	return nil
}

// do sets up one host.
func (r *remoteSetup) do(ctx context.Context) error {
	r.printf("\n→ %s\n", r.host)
	i, err := r.probe(ctx)
	if err != nil {
		return err
	}
	in, err := r.installSpotexec(ctx, i)
	if err != nil {
		return err
	}
	bin := in.bin
	setupProbe := ""
	if r.aws {
		setupProbe = " --admin-from-env"
	}
	ver, err := r.run(ctx, `command -v `+bin+` >/dev/null || { echo NOT-INSTALLED; exit 0; }; `+
		bin+` version && { h=$(`+bin+` setup`+setupProbe+` --help 2>&1) || echo NO-SETUP; `+
		`case "$h" in *" --hooks "*) ;; *) echo NO-HOOK-OPT-IN ;; esac; }`, nil)
	if err != nil {
		return fmt.Errorf("spotexec doesn't run there: %w", err)
	}
	if strings.Contains(ver, "NOT-INSTALLED") {
		return errors.New("spotexec isn't installed there")
	}
	got := versionOf(ver)
	if r.aws && strings.Contains(ver, "NO-SETUP") {
		// Too old for --admin-from-env, so it could fall back to its own
		// AWS login.
		return fmt.Errorf("%s there is too old to be set up from here; let setup upgrade it (drop --install none)", strings.TrimSpace(strings.Split(ver, "\n")[0]))
	}
	if in.want != "" && got != in.want {
		// An older one may not understand the flags passed below.
		return fmt.Errorf("spotexec there reports version %q, not %s as installed", got, in.want)
	}
	if strings.Contains(ver, "NO-SETUP") {
		return fmt.Errorf("%s there has no `setup` command; it needs spotexec 0.2.0 or later", strings.TrimSpace(strings.Split(ver, "\n")[0]))
	}
	r.printf("  ✓ %s (%s)\n", strings.TrimSpace(strings.Split(ver, "\n")[0]), in.how)

	// The runner config, keeping a backup of a different one. SSH keys and
	// the owner id stay per machine (each machine owns its own VMs).
	cfg, err := os.ReadFile(config.GlobalPath())
	if err != nil {
		return err
	}
	out, err := r.run(ctx, `set -e; d="${XDG_CONFIG_HOME:-$HOME/.config}/spotexec"; mkdir -p "$d"; f="$d/config.toml"; `+
		`n="$d/.config.toml.new.$$"; cat > "$n"; if [ -f "$f" ] && ! cmp -s "$f" "$n"; then `+
		`b="$f.bak-$(date +%Y%m%d%H%M%S)"; cp "$f" "$b"; echo "backed up the previous config to $b"; fi; `+
		`mv "$n" "$f"; echo "$f"`, bytes.NewReader(cfg))
	if err != nil {
		return fmt.Errorf("copying the runner config: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines[:len(lines)-1] {
		r.printf("%s\n", "  "+l)
	}
	r.printf("  ✓ copied the runner config to %s\n", lines[len(lines)-1])

	// Run setup there: with AWS runners, using this machine's AWS sign-in
	// (passed on stdin, so it never appears in a command line) to create
	// that machine's own user; the Claude Code hook too, if it was asked for here.
	env := []string{"", "", ""}
	flags := " --yes --profile " + shq(r.profile)
	if r.aws {
		// Only the session passed here, never that machine's own AWS login.
		flags += " --admin-from-env"
		if r.adminErr != nil {
			// A machine that already has its key needs no admin session; one
			// that doesn't will say so.
			r.printf("  checking AWS access there (not signed in to AWS here, so it can't create a key: %v)…\n", firstLine(r.adminErr.Error()))
		} else {
			env = r.adminEnv
			r.printf("  setting up AWS access there (as %s)…\n", r.adminARN)
		}
	} else {
		flags += " --skip-aws"
	}
	if r.rotate {
		flags += " --rotate"
	}
	if r.noVPC {
		flags += " --no-vpc"
	}
	// A spotexec there from before the hook was opt-in doesn't know
	// --hooks, and installs the hook with --yes unless told not to.
	optIn := !strings.Contains(ver, "NO-HOOK-OPT-IN")
	if r.hooks && optIn {
		flags += " --hooks"
	}
	if r.noHooks || (!r.hooks && !optIn) {
		flags += " --no-hooks"
	}
	script := `IFS= read -r k; IFS= read -r s; IFS= read -r t; ` +
		`AWS_ACCESS_KEY_ID="$k" AWS_SECRET_ACCESS_KEY="$s" AWS_SESSION_TOKEN="$t" AWS_PROFILE= AWS_DEFAULT_PROFILE= SPOTEXEC_SETUP_NESTED=1 ` +
		`exec ` + bin + ` setup` + flags
	if err := r.stream(ctx, script, strings.NewReader(strings.Join(env, "\n")+"\n")); err != nil {
		if r.aws && r.adminErr != nil {
			return fmt.Errorf("spotexec setup failed there: %w\n    if it needs a new AWS key: sign in to AWS here (%s), then run `%s`", err, r.signInHint(), r.retryCommand())
		}
		return fmt.Errorf("spotexec setup failed there: %w", err)
	}
	// Which machine this destination reached, for the machine list.
	r.hostname = i.hostname
	id, err := r.run(ctx, `cat "${SPOTEXEC_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/spotexec}/owner-id"`, nil)
	if id = strings.TrimSpace(id); err == nil && id != "" && !strings.ContainsAny(id, " \t\n") {
		r.machineID = id
	} else if r.aws {
		// AWS setup there made one; without it, revoking this machine
		// couldn't find this entry.
		return fmt.Errorf("set up, but its machine ID can't be read (%v); run setup for it again", firstLine(fmt.Sprint(err)))
	}
	return r.checkLoginShell(ctx, in.dir, got)
}

// signInHint says how to sign in to AWS here for the admin profile.
func (r *remoteSetup) signInHint() string {
	if r.adminProfile == "" {
		return "`aws login` or `aws sso login`"
	}
	p := " --profile " + shq(r.adminProfile)
	return "`aws login" + p + "` or `aws sso login" + p + "`"
}

// retryCommand is the command that sets this host up again with the same
// choices.
func (r *remoteSetup) retryCommand() string {
	c := "spotexec setup"
	if r.adminProfile != "" {
		c += " --admin-profile " + shq(r.adminProfile)
	}
	if r.profile != "spotexec" {
		c += " --profile " + shq(r.profile)
	}
	if r.install != "auto" && !r.fellBack {
		c += " --install " + r.install
	}
	for _, f := range []struct {
		on   bool
		flag string
	}{{r.rotate, " --rotate"}, {r.noVPC, " --no-vpc"}, {r.hooks, " --hooks"}, {r.noHooks, " --no-hooks"}} {
		if f.on {
			c += f.flag
		}
	}
	return c + " " + shellWord(r.host)
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// shellWord quotes s for a shell when it needs it.
func shellWord(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return shq(s)
}

// brewError picks Homebrew's error line out of its output.
func brewError(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Error:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "Error:"))
		}
	}
	return firstLine(strings.TrimSpace(out))
}

// pathHint says how to put dir on PATH for a login shell there.
func (r *remoteSetup) pathHint(ctx context.Context, dir string) string {
	rc := "~/.profile"
	sh, _ := r.run(ctx, `echo "$SHELL"`, nil)
	switch sh = strings.TrimSpace(sh); {
	case strings.HasSuffix(sh, "fish"):
		return fmt.Sprintf("; add it to PATH there: fish -c 'fish_add_path %s'", dir)
	case strings.HasSuffix(sh, "csh"):
		return fmt.Sprintf("; add it to PATH there: echo 'set path = (%s $path)' >> ~/.cshrc", dir)
	case strings.HasSuffix(sh, "zsh"):
		rc = "~/.zprofile"
	case strings.HasSuffix(sh, "bash"):
		rc = "~/.bash_profile"
	}
	return fmt.Sprintf("; add it to PATH there: echo 'export PATH=\"%s:$PATH\"' >> %s", dir, rc)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func (r *remoteSetup) close() {
	_ = exec.Command("ssh", "-o", "ControlPath="+filepath.Join(r.ctlDir, "%C"), "-O", "exit", "--", r.host).Run()
}

var adminSession = provider.AdminSession // tests

// runnerRegions are the AWS runners' home regions here, which say the
// partition (commercial or GovCloud) the admin session signs in to.
func runnerRegions() []string {
	g, err := config.LoadGlobal()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range awsRunners(g) {
		if r.Region != "" && !slices.Contains(out, r.Region) {
			out = append(out, r.Region)
		}
	}
	return out
}

// setupHosts connects to each host in turn, then sets them up four at a
// time, printing each one's output as a block when it finishes, and records
// them in the machine list.
func setupHosts(ctx context.Context, hosts []string, base remoteSetup) error {
	if err := checkSSHHosts(hosts); err != nil {
		return err
	}
	// /tmp, not $TMPDIR: a socket path must stay under ~104 bytes.
	dir, err := os.MkdirTemp("/tmp", "sx-")
	if err != nil {
		dir, err = os.MkdirTemp("", "sx-")
	}
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if base.aws {
		base.adminEnv, base.adminARN, base.adminErr = adminSession(ctx, base.adminProfile, runnerRegions())
		if base.adminErr == nil && strings.Contains(base.adminARN, ":user/spotexec/") {
			// A machine's own spotexec user can't manage IAM users: setting
			// up others with it would only fail on AccessDenied.
			base.adminEnv, base.adminErr = nil, fmt.Errorf("signed in as %s, spotexec's own user here, which can't manage IAM users", base.adminARN)
		}
	}
	var mu sync.Mutex
	var failed []string
	// Connect to each host in turn first, so host-key and password prompts
	// come one at a time; the parallel work below reuses these connections.
	var ready []string
	for _, h := range hosts {
		r := base
		r.host, r.ctlDir = h, dir
		defer r.close()
		if err := r.connect(ctx); err != nil {
			fmt.Printf("  ✗ %s: can't connect over SSH (%v)\n", h, err)
			failed = append(failed, h)
			continue
		}
		ready = append(ready, h)
	}
	if len(ready) > 1 {
		fmt.Printf("\nSetting up %s…\n", strings.Join(ready, ", "))
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, h := range ready {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var buf bytes.Buffer
			r := base
			r.host, r.ctlDir, r.out = h, dir, &buf
			if len(ready) == 1 {
				r.out = os.Stdout // one host: stream it
			}
			err := r.do(ctx)
			r.close()
			var notReady *notReadyError
			if errors.As(err, &notReady) {
				// Set up, but not usable from a fresh shell yet: remembered,
				// so `setup --all` checks it again.
				r.printf("  ✗ %s isn't ready: %v\n", h, err)
				rememberMachine(h, r.machineID, r.hostname)
			} else if err != nil {
				r.printf("  ✗ %s: %v\n", h, err)
			} else {
				r.printf("  ✓ %s is ready: run `sx <command>` in a project there\n", h)
				rememberMachine(h, r.machineID, r.hostname)
			}
			mu.Lock()
			defer mu.Unlock()
			os.Stdout.Write(buf.Bytes())
			if err != nil {
				failed = append(failed, h)
			}
		}(h)
	}
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("setup failed on %s", strings.Join(failed, ", "))
	}
	return nil
}

// The machines setup has set up from here, so `setup --all` can update them
// and the picker can show them.
type machineList struct {
	Machines []savedMachine `toml:"machine"`
}

type savedMachine struct {
	Host      string    `toml:"host"` // the SSH destination
	LastSetup time.Time `toml:"last_setup"`
	Version   string    `toml:"version"`
	// The machine it reached: its spotexec machine ID (which tags its IAM
	// user) and hostname.
	MachineID string `toml:"machine_id,omitempty"`
	Hostname  string `toml:"hostname,omitempty"`
}

var machinesMu sync.Mutex

func machinesPath() string { return filepath.Join(config.Dir(), "machines.toml") }

func loadMachines() []savedMachine {
	var m machineList
	if _, err := toml.DecodeFile(machinesPath(), &m); err != nil {
		return nil
	}
	return m.Machines
}

func saveMachines(ms []savedMachine) error {
	var b bytes.Buffer
	b.WriteString("# Machines `spotexec setup` has set up from here; `spotexec setup --all` updates them.\n")
	if err := toml.NewEncoder(&b).Encode(machineList{Machines: ms}); err != nil {
		return err
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(machinesPath(), b.Bytes(), 0o600)
}

// rememberMachine records a destination and the machine it reached. Another
// destination for the same machine (an alias, an address) is dropped, so
// `setup --all` sets each machine up once.
func rememberMachine(host, machineID, hostname string) {
	machinesMu.Lock()
	defer machinesMu.Unlock()
	now := time.Now().UTC().Truncate(time.Second)
	var ms []savedMachine
	found := false
	for _, m := range loadMachines() {
		switch {
		case strings.EqualFold(m.Host, host):
			if found {
				continue
			}
			found = true
			// What it reaches now, even if unknown: never a stale identity.
			m.LastSetup, m.Version, m.MachineID, m.Hostname = now, Version, machineID, hostname
		case machineID != "" && m.MachineID == machineID:
			continue
		}
		ms = append(ms, m)
	}
	if !found {
		ms = append(ms, savedMachine{Host: host, LastSetup: now, Version: Version, MachineID: machineID, Hostname: hostname})
	}
	_ = saveMachines(ms)
}

func forgetMachine(match func(savedMachine) bool) []string {
	machinesMu.Lock()
	defer machinesMu.Unlock()
	var keep []savedMachine
	var gone []string
	for _, m := range loadMachines() {
		if match(m) {
			gone = append(gone, m.Host)
		} else {
			keep = append(keep, m)
		}
	}
	if len(gone) > 0 {
		_ = saveMachines(keep)
	}
	return gone
}

// discoverHosts lists machines that look reachable over SSH: Bonjour
// (Remote Login on Macs, avahi on Linux) and Host entries in ~/.ssh/config.
func discoverHosts(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var l hostList
	add := l.add
	for _, m := range loadMachines() {
		add(m.Host) // set up before: listed first
	}
	var bonjour []string
	if _, err := exec.LookPath("dns-sd"); err == nil {
		bonjour = browseDNSSD(ctx)
	} else if _, err := exec.LookPath("avahi-browse"); err == nil {
		b, _ := exec.CommandContext(ctx, "avahi-browse", "-rpt", "_ssh._tcp").Output()
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Split(l, ";"); len(f) > 8 && f[0] == "=" && f[8] == "22" {
				bonjour = append(bonjour, f[6])
			}
		}
	}
	sort.Strings(bonjour)
	for _, h := range bonjour {
		add(h)
	}
	for _, h := range sshConfigHosts() {
		add(h)
	}
	return l.hosts
}

// hostList collects discovered hosts once each, dropping this machine and
// names that aren't safe to hand to ssh (a Bonjour answer can be anything).
type hostList struct {
	seen  map[string]bool
	hosts []string
}

func (l *hostList) add(h string) {
	// Checked as given: a name padded with whitespace is not trusted either.
	h = strings.TrimSuffix(h, ".")
	k := strings.ToLower(h)
	if checkSSHHost(h) != nil || l.seen[k] || isSelf(k) {
		return
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[k] = true
	l.hosts = append(l.hosts, h)
}

var dnssdAdd = regexp.MustCompile(`\sAdd\s+\d+\s+\d+\s+\S+\s+_ssh\._tcp\.\s+(.+)$`)
var dnssdAt = regexp.MustCompile(`can be reached at (\S+?)\.?:(\d+)`)

func browseDNSSD(ctx context.Context) []string {
	bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b, _ := exec.CommandContext(bctx, "dns-sd", "-B", "_ssh._tcp", "local").Output()
	names := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if m := dnssdAdd.FindStringSubmatch(l); m != nil {
			names[strings.TrimSpace(m[1])] = true
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var hosts []string
	for n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer cancel()
			b, _ := exec.CommandContext(lctx, "dns-sd", "-L", n, "_ssh._tcp", "local").Output()
			if m := dnssdAt.FindStringSubmatch(string(b)); m != nil && m[2] == "22" {
				mu.Lock()
				hosts = append(hosts, m[1])
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return hosts
}

func sshConfigHosts() []string {
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "host") {
			continue
		}
		for _, h := range fields[1:] {
			if !strings.ContainsAny(h, "*?!") {
				out = append(out, h)
			}
		}
	}
	return out
}

func isSelf(h string) bool {
	short := strings.Split(h, ".")[0]
	names := []string{"localhost"}
	if n, err := os.Hostname(); err == nil {
		names = append(names, strings.ToLower(strings.Split(n, ".")[0]))
	}
	if b, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		names = append(names, strings.ToLower(strings.TrimSpace(string(b))))
	}
	for _, n := range names {
		if short == n {
			return true
		}
	}
	return false
}

// chooseHosts asks which of the discovered hosts to set up.
func chooseHosts(found []string, in *bufio.Reader) []string {
	fmt.Println("\nSet up other machines too? spotexec installs itself there over SSH, copies this config, and gives each machine its own AWS key.")
	if len(found) > 0 {
		fmt.Println("Found on your network, in ~/.ssh/config, and set up before:")
		saved := map[string]savedMachine{}
		for _, m := range loadMachines() {
			saved[strings.ToLower(m.Host)] = m
		}
		for i, h := range found {
			note := ""
			if m, ok := saved[strings.ToLower(h)]; ok {
				note = fmt.Sprintf("   (set up %s, spotexec %s)", m.LastSetup.Local().Format("Jan 2"), m.Version)
			}
			fmt.Printf("  %2d. %s%s\n", i+1, h, note)
		}
		fmt.Print("Numbers, `all`, or host names, separated by spaces (Enter to skip): ")
	} else {
		fmt.Print("Host names, separated by spaces (Enter to skip): ")
	}
	line, _ := in.ReadString('\n')
	return parseHostChoice(line, found)
}

func parseHostChoice(line string, found []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' }) {
		if strings.EqualFold(tok, "all") {
			for _, h := range found {
				add(h)
			}
			continue
		}
		var n int
		if _, err := fmt.Sscanf(tok, "%d", &n); err == nil && fmt.Sprint(n) == tok {
			if n >= 1 && n <= len(found) {
				add(found[n-1])
			}
			continue
		}
		add(tok)
	}
	return out
}
