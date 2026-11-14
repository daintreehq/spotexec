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
	"sort"
	"strings"
	"sync"
	"time"

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
	noHooks      bool
	aws          bool
	ctlDir       string
	out          io.Writer // this host's output (buffered when hosts run in parallel)
	// The AWS sign-in passed to each host's setup, resolved once.
	adminEnv []string
	adminARN string
	adminErr error
}

func (r *remoteSetup) printf(format string, args ...any) { fmt.Fprintf(r.out, format, args...) }

const remotePath = `PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:$PATH"; export PATH; `

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (r *remoteSetup) sshArgs(script string) []string {
	return []string{
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + filepath.Join(r.ctlDir, "%C"), "-o", "ControlPersist=120",
		"-o", "ConnectTimeout=15", r.host, "/bin/sh -c " + shq(remotePath+script),
	}
}

// run runs a shell script on the host, returning its stdout.
func (r *remoteSetup) run(ctx context.Context, script string, stdin io.Reader) (string, error) {
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
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(script)...)
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

// latestRelease returns the newest release tag (e.g. "v0.2.0").
func latestRelease(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "https://github.com/daintreehq/spotexec/releases/latest", nil)
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

// installSpotexec installs or upgrades spotexec and returns its path.
func (r *remoteSetup) installSpotexec(ctx context.Context, i remoteInfo) (string, string, error) {
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
			return "", "", errors.New("Homebrew isn't installed there (use --install release)")
		}
		r.printf("%s\n", "  installing spotexec with Homebrew (latest)…")
		out, err := r.run(ctx, `brew update --quiet >/dev/null 2>&1; `+
			`if brew list --versions spotexec >/dev/null 2>&1; then verb=upgrade; else verb=install; fi; `+
			`out=$(brew "$verb" daintreehq/tap/spotexec 2>&1); rc=$?; `+
			`if ! brew list --versions spotexec >/dev/null 2>&1; then printf '%s\n' "$out" | tail -n 20; exit 1; fi; `+
			`[ $rc -eq 0 ] || echo "warning: brew $verb failed; keeping the installed version: $(printf '%s\n' "$out" | grep -m1 '^Error')"; `+
			// An earlier copy in ~/.local/bin would shadow Homebrew's.
			`if [ -f "$HOME/.local/bin/spotexec" ]; then rm -f "$HOME/.local/bin/spotexec"; `+
			`[ -L "$HOME/.local/bin/sx" ] && rm -f "$HOME/.local/bin/sx"; echo "removed an older spotexec from ~/.local/bin"; fi`, nil)
		if err != nil {
			why := brewError(out + err.Error())
			if r.install == "auto" {
				r.printf("  Homebrew couldn't install it (%s); using the release binary instead\n", why)
				r.install = "release"
				return r.installSpotexec(ctx, i)
			}
			return "", "", fmt.Errorf("brew install failed: %s", why)
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" {
				r.printf("%s\n", "  "+l)
			}
		}
		return "spotexec", "Homebrew", nil
	case "release":
		tag, err := latestRelease(ctx)
		if err != nil {
			return "", "", err
		}
		url := fmt.Sprintf("https://github.com/daintreehq/spotexec/releases/download/%s/spotexec_%s_%s_%s.tar.gz", tag, strings.TrimPrefix(tag, "v"), i.os, i.arch)
		r.printf("  installing spotexec %s into ~/.local/bin…\n", tag)
		if _, err := r.run(ctx, `set -e; mkdir -p "$HOME/.local/bin"; t=$(mktemp -d); `+
			`curl -fsSL `+shq(url)+` | tar -xz -C "$t" spotexec; mv "$t/spotexec" "$HOME/.local/bin/spotexec"; rmdir "$t"; `+
			`ln -sf spotexec "$HOME/.local/bin/sx"`, nil); err != nil {
			return "", "", fmt.Errorf("downloading %s: %w", url, err)
		}
		return "$HOME/.local/bin/spotexec", "release " + tag + " in ~/.local/bin" + r.pathHint(ctx), nil
	case "copy":
		if i.os != runtime.GOOS || i.arch != runtime.GOARCH {
			return "", "", fmt.Errorf("--install copy needs the same platform (this is %s/%s, that is %s/%s)", runtime.GOOS, runtime.GOARCH, i.os, i.arch)
		}
		self, err := os.Executable()
		if err != nil {
			return "", "", err
		}
		f, err := os.Open(self)
		if err != nil {
			return "", "", err
		}
		defer f.Close()
		r.printf("  copying this spotexec (%s) into ~/.local/bin…\n", Version)
		if _, err := r.run(ctx, `set -e; mkdir -p "$HOME/.local/bin"; cat > "$HOME/.local/bin/.spotexec.tmp"; `+
			`chmod 755 "$HOME/.local/bin/.spotexec.tmp"; mv "$HOME/.local/bin/.spotexec.tmp" "$HOME/.local/bin/spotexec"; `+
			`ln -sf spotexec "$HOME/.local/bin/sx"`, f); err != nil {
			return "", "", err
		}
		return "$HOME/.local/bin/spotexec", "copied from this machine into ~/.local/bin" + r.pathHint(ctx), nil
	case "none":
		if i.spotexec == "" {
			return "", "", errors.New("spotexec isn't installed there (drop --install none)")
		}
		return i.spotexec, "already installed", nil
	}
	return "", "", fmt.Errorf("--install must be auto, brew, release, copy or none (got %q)", r.install)
}

// do sets up one host.
func (r *remoteSetup) do(ctx context.Context) error {
	r.printf("\n→ %s\n", r.host)
	i, err := r.probe(ctx)
	if err != nil {
		return err
	}
	bin, how, err := r.installSpotexec(ctx, i)
	if err != nil {
		return err
	}
	ver, err := r.run(ctx, `command -v `+bin+` >/dev/null || { echo NOT-INSTALLED; exit 0; }; `+
		bin+` version && { `+bin+` setup --help >/dev/null 2>&1 || echo NO-SETUP; }`, nil)
	if err != nil {
		return fmt.Errorf("spotexec doesn't run there: %w", err)
	}
	if strings.Contains(ver, "NOT-INSTALLED") {
		return errors.New("spotexec isn't installed there")
	}
	if strings.Contains(ver, "NO-SETUP") {
		return fmt.Errorf("%s there has no `setup` command; it needs spotexec 0.2.0 or later", strings.TrimSpace(strings.Split(ver, "\n")[0]))
	}
	r.printf("  ✓ %s (%s)\n", strings.TrimSpace(strings.Split(ver, "\n")[0]), how)

	// The runner config, keeping a backup of a different one. SSH keys and
	// the owner id stay per machine (each machine owns its own VMs).
	cfg, err := os.ReadFile(config.GlobalPath())
	if err != nil {
		return err
	}
	out, err := r.run(ctx, `set -e; d="${XDG_CONFIG_HOME:-$HOME/.config}/spotexec"; mkdir -p "$d"; f="$d/config.toml"; `+
		`cat > "$d/.config.toml.new"; if [ -f "$f" ] && ! cmp -s "$f" "$d/.config.toml.new"; then `+
		`b="$f.bak-$(date +%Y%m%d%H%M%S)"; cp "$f" "$b"; echo "backed up the previous config to $b"; fi; `+
		`mv "$d/.config.toml.new" "$f"; echo "$f"`, bytes.NewReader(cfg))
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
	// that machine's own user; the Claude Code hook too, like here.
	env := []string{"", "", ""}
	flags := " --yes --profile " + shq(r.profile)
	if r.aws {
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
	if r.noHooks {
		flags += " --no-hooks"
	}
	script := `IFS= read -r k; IFS= read -r s; IFS= read -r t; ` +
		`AWS_ACCESS_KEY_ID="$k" AWS_SECRET_ACCESS_KEY="$s" AWS_SESSION_TOKEN="$t" AWS_PROFILE= AWS_DEFAULT_PROFILE= SPOTEXEC_SETUP_NESTED=1 ` +
		`exec ` + bin + ` setup` + flags
	if err := r.stream(ctx, script, strings.NewReader(strings.Join(env, "\n")+"\n")); err != nil {
		return fmt.Errorf("spotexec setup failed there: %w", err)
	}
	return nil
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

// pathHint says how to put ~/.local/bin on PATH when a login shell there
// doesn't have it.
func (r *remoteSetup) pathHint(ctx context.Context) string {
	out, err := r.run(ctx, `"${SHELL:-/bin/sh}" -lc 'echo ":$PATH:"' 2>/dev/null`, nil)
	if err != nil || strings.Contains(out, "/.local/bin:") {
		return ""
	}
	rc := "~/.profile"
	if sh, _ := r.run(ctx, `echo "$SHELL"`, nil); strings.HasSuffix(strings.TrimSpace(sh), "zsh") {
		rc = "~/.zprofile"
	} else if strings.HasSuffix(strings.TrimSpace(sh), "bash") {
		rc = "~/.bash_profile"
	}
	return fmt.Sprintf("; it isn't on PATH there yet: echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> %s", rc)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func (r *remoteSetup) close() {
	_ = exec.Command("ssh", "-o", "ControlPath="+filepath.Join(r.ctlDir, "%C"), "-O", "exit", r.host).Run()
}

// setupHosts sets up the hosts, four at a time, printing each one's output
// as a block when it finishes, and records them in the machine list.
func setupHosts(ctx context.Context, hosts []string, base remoteSetup) error {
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
		base.adminEnv, base.adminARN, base.adminErr = provider.AdminSession(ctx, base.adminProfile)
	}
	if len(hosts) > 1 {
		fmt.Printf("\nSetting up %s…\n", strings.Join(hosts, ", "))
	}
	var mu sync.Mutex
	var failed []string
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var buf bytes.Buffer
			r := base
			r.host, r.ctlDir, r.out = h, dir, &buf
			if len(hosts) == 1 {
				r.out = os.Stdout // one host: stream it
			}
			err := r.do(ctx)
			r.close()
			if err != nil {
				r.printf("  ✗ %s: %v\n", h, err)
			} else {
				r.printf("  ✓ %s is ready: run `sx <command>` in a project there\n", h)
				rememberMachine(h)
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
	Host      string    `toml:"host"`
	LastSetup time.Time `toml:"last_setup"`
	Version   string    `toml:"version"`
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

func rememberMachine(host string) {
	machinesMu.Lock()
	defer machinesMu.Unlock()
	ms := loadMachines()
	found := false
	for i := range ms {
		if strings.EqualFold(ms[i].Host, host) {
			ms[i].LastSetup, ms[i].Version, found = time.Now().UTC().Truncate(time.Second), Version, true
		}
	}
	if !found {
		ms = append(ms, savedMachine{Host: host, LastSetup: time.Now().UTC().Truncate(time.Second), Version: Version})
	}
	_ = saveMachines(ms)
}

func forgetMachine(match func(host string) bool) []string {
	machinesMu.Lock()
	defer machinesMu.Unlock()
	var keep []savedMachine
	var gone []string
	for _, m := range loadMachines() {
		if match(m.Host) {
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
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.TrimSuffix(strings.TrimSpace(h), ".")
		k := strings.ToLower(h)
		if h == "" || seen[k] || isSelf(k) {
			return
		}
		seen[k] = true
		out = append(out, h)
	}
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
	return out
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
