// Package transport drives the system OpenSSH client: a persistent
// ControlMaster per runner (so each run's SSH channel costs ~15ms), helper
// binary bootstrap, and the framed RPC session.
package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/helperbin"
	"github.com/spotexec/spotexec/internal/provider"
)

// ControlPersist keeps the master connection alive between runs.
const ControlPersist = "15m"

// Client runs commands on one target.
type Client struct {
	T        provider.Target
	ctlPath  string
	WorkRoot string // remote work root (may start with ~/)
}

// New builds a client for a target.
func New(t provider.Target, workRoot string) *Client {
	// The known_hosts file is per instance for ephemeral VMs, so a Spot IP
	// reused by a new instance gets its own master and helper markers.
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s|%s", t.Host, t.User, t.Port, t.IdentityFile, t.KnownHostsFile)))
	return &Client{T: t, ctlPath: filepath.Join(controlDir(), hex.EncodeToString(h[:])[:16]), WorkRoot: workRoot}
}

// controlDir holds ControlMaster sockets. OpenSSH appends 17 bytes to the
// path while binding and macOS limits socket paths to 104 bytes, so a long
// (or missing) home falls back to a short per-user directory in /tmp.
// controlDir is re-checked on every call: a long-running process (the
// daemon) must survive the directory being cleaned away (tmp reapers).
func controlDir() string {
	d := controlDirOnce()
	if _, err := os.Stat(d); err != nil {
		_ = os.MkdirAll(d, 0o700)
	}
	return d
}

var controlDirOnce = sync.OnceValue(func() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "spotexec", "cm")
	if home != "" && len(dir)+17+17 <= 100 {
		_ = os.MkdirAll(dir, 0o700)
		return dir
	}
	// /tmp is shared: use the per-user dir only if it's really ours and
	// private; otherwise a fresh private one (short, under /tmp).
	dir = filepath.Join("/tmp", fmt.Sprintf("sx-cm-%d", os.Getuid()))
	_ = os.Mkdir(dir, 0o700)
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() && os.Chmod(dir, 0o700) == nil {
			return dir
		}
	}
	if d, err := os.MkdirTemp("/tmp", "sx-cm-"); err == nil {
		return d
	}
	return os.TempDir()
})

func (c *Client) baseArgs(master string) []string {
	a := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=" + master,
		"-o", "ControlPath=" + c.ctlPath,
		"-o", "ControlPersist=" + ControlPersist,
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	}
	if c.T.IdentityFile != "" {
		a = append(a, "-i", c.T.IdentityFile, "-o", "IdentitiesOnly=yes")
	}
	if c.T.KnownHostsFile != "" {
		a = append(a, "-o", "UserKnownHostsFile="+c.T.KnownHostsFile)
	}
	if c.T.Port != 0 {
		a = append(a, "-p", strconv.Itoa(c.T.Port))
	}
	if c.T.User != "" && !strings.Contains(c.T.Host, "@") {
		a = append(a, "-l", c.T.User)
	}
	for _, o := range c.T.Options {
		a = append(a, "-o", o)
	}
	return a
}

// Args returns full ssh argv for running remoteCmd.
func (c *Client) Args(remoteCmd string) []string {
	// "--" ends options: the host can never be read as one.
	return append(append(c.baseArgs("no"), "--", c.T.Host), remoteCmd)
}

// MasterAlive reports whether a control master is up.
func (c *Client) MasterAlive(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "ControlPath="+c.ctlPath, "-O", "check", "--", c.T.Host)
	return cmd.Run() == nil
}

// EnsureMaster starts the control master if needed. The master is started
// with -f -N and file-backed stderr: a backgrounded master inheriting our
// pipes would otherwise hold them open and hang every caller.
func (c *Client) EnsureMaster(ctx context.Context) error {
	if c.MasterAlive(ctx) {
		return nil
	}
	// One process starts the master; the others wait and reuse it. (Racing
	// starters lose the socket bind and leave stray `ssh -N` processes.)
	if lf, err := os.OpenFile(c.ctlPath+".lock", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		defer lf.Close()
		locked := false
		for {
			err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				locked = true
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
				break // no flock here (some network filesystems): go unlocked
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		if locked {
			defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			if c.MasterAlive(ctx) {
				return nil
			}
		}
	}
	errf, err := os.CreateTemp("", "spotexec-ssh-*.err")
	if err != nil {
		return err
	}
	defer os.Remove(errf.Name())
	defer errf.Close()
	args := append(c.baseArgs("auto"), "-f", "-N", "--", c.T.Host)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = nil
	cmd.Stderr = errf
	if err := cmd.Run(); err != nil {
		b, _ := os.ReadFile(errf.Name())
		return &ConnError{Target: c.T.String(), Msg: strings.TrimSpace(string(b)), Err: err}
	}
	return nil
}

// CloseMaster stops the control master.
func (c *Client) CloseMaster() {
	_ = exec.Command("ssh", "-o", "ControlPath="+c.ctlPath, "-O", "exit", "--", c.T.Host).Run()
}

// ConnError is an SSH-level failure (exit 255 or master start failure).
type ConnError struct {
	Target string
	Msg    string
	Err    error
}

func (e *ConnError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("ssh %s: %s", e.Target, e.Msg)
	}
	return fmt.Sprintf("ssh %s: %v", e.Target, e.Err)
}

// Run executes remoteCmd and returns stdout. stdin may be nil.
func (c *Client) Run(ctx context.Context, remoteCmd string, stdin io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", c.Args(remoteCmd)...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 255 {
			return out.Bytes(), &ConnError{Target: c.T.String(), Msg: strings.TrimSpace(errb.String()), Err: err}
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("remote command failed: %s", msg)
	}
	return out.Bytes(), nil
}

// RemoteRoot renders the work root for use in a remote shell command.
func RemoteRoot(p string) string {
	if p == "" {
		p = "~/.cache/spotexec"
	}
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME"/` + ShellQuote(p[2:])
	}
	return ShellQuote(p)
}

// ShellQuote single-quotes s for POSIX shells.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Platform is a remote OS/arch in Go terms.
type Platform struct{ OS, Arch string }

func parseUname(s string) (Platform, error) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return Platform{}, fmt.Errorf("unexpected uname output %q", s)
	}
	p := Platform{OS: strings.ToLower(f[0])}
	switch f[1] {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64":
		p.Arch = "arm64"
	default:
		return p, fmt.Errorf("unsupported remote architecture %q", f[1])
	}
	if p.OS != "linux" && p.OS != "darwin" {
		return p, fmt.Errorf("unsupported remote OS %q", f[0])
	}
	return p, nil
}

// HelperPath returns the remote helper path expression for a hash.
func (c *Client) helperPath(hash string) string {
	return RemoteRoot(c.WorkRoot) + "/bin/spotexec-helper-" + hash
}

func markerPath(c *Client, hash string) string {
	h := sha256.Sum256([]byte(c.ctlPath + "|" + c.WorkRoot + "|" + hash))
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "spotexec", "helpers", hex.EncodeToString(h[:])[:20])
}

// EnsureHelper makes sure the right helper is installed and returns the
// remote command prefix that invokes it.
func (c *Client) EnsureHelper(ctx context.Context, force bool) (string, error) {
	// Fast path: we've installed this exact helper on this host before.
	// The marker records the remote platform and the hash we installed; it
	// is only trusted if this binary still embeds that exact helper, so an
	// upgraded CLI reinstalls instead of driving a stale helper.
	if !force {
		if b, err := os.ReadFile(markerPathAny(c)); err == nil {
			f := strings.Fields(string(b))
			if len(f) == 3 {
				if _, cur, err := helperbin.Get(f[0], f[1]); err == nil && cur == f[2] {
					if _, err := os.Stat(markerPath(c, cur)); err == nil {
						return c.helperPath(cur), nil
					}
				}
			}
		}
	}
	out, err := c.Run(ctx, "uname -sm", nil)
	if err != nil {
		return "", err
	}
	plat, err := parseUname(string(out))
	if err != nil {
		return "", err
	}
	gz, hash, err := helperbin.Get(plat.OS, plat.Arch)
	if err != nil {
		return "", err
	}
	path := c.helperPath(hash)
	check := []byte("")
	if !force { // a forced reinstall re-uploads even over a present (but broken) binary
		check, err = c.Run(ctx, "test -x "+path+" && echo present || true", nil)
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(check)) != "present" {
		dir := RemoteRoot(c.WorkRoot) + "/bin"
		script := fmt.Sprintf(`set -e; mkdir -p %[1]s; t=%[1]s/.upload.$$; gzip -dc > "$t"; chmod 755 "$t"; mv -f "$t" %[2]s; find %[1]s -maxdepth 1 -name 'spotexec-helper-*' ! -name %[3]s -mmin +1440 -exec rm -f {} + 2>/dev/null || true`, dir, path, ShellQuote(filepath.Base(path)))
		if _, err := c.Run(ctx, script, bytes.NewReader(gz)); err != nil {
			return "", fmt.Errorf("uploading helper: %w", err)
		}
	}
	_ = os.MkdirAll(filepath.Dir(markerPath(c, hash)), 0o755)
	_ = os.WriteFile(markerPath(c, hash), []byte(time.Now().Format(time.RFC3339)), 0o644)
	_ = os.WriteFile(markerPathAny(c), []byte(plat.OS+" "+plat.Arch+" "+hash), 0o644)
	return path, nil
}

func markerPathAny(c *Client) string {
	h := sha256.Sum256([]byte(c.ctlPath + "|" + c.WorkRoot))
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "spotexec", "helpers", "host-"+hex.EncodeToString(h[:])[:20])
}

// ForgetHelper drops the local install markers (after a failed start).
func (c *Client) ForgetHelper() {
	_ = os.Remove(markerPathAny(c))
}

// HelperCmd runs a non-RPC helper subcommand (status, unlock, prune).
func (c *Client) HelperCmd(ctx context.Context, args ...string) ([]byte, error) {
	path, err := c.EnsureHelper(ctx, false)
	if err != nil {
		return nil, err
	}
	q := []string{path}
	for _, a := range args {
		q = append(q, ShellQuote(a))
	}
	q = append(q, "--work-root", ShellQuote(c.WorkRoot))
	out, err := c.Run(ctx, strings.Join(q, " "), nil)
	if err != nil && !isConn(err) && ctx.Err() == nil {
		// Helper may have been deleted remotely: reinstall once.
		c.ForgetHelper()
		path, err2 := c.EnsureHelper(ctx, true)
		if err2 != nil {
			return out, err2
		}
		q[0] = path
		return c.Run(ctx, strings.Join(q, " "), nil)
	}
	return out, err
}

func isConn(err error) bool {
	var ce *ConnError
	return errors.As(err, &ce)
}

// IsConnError reports whether err is an SSH connectivity failure.
func IsConnError(err error) bool { return isConn(err) }
