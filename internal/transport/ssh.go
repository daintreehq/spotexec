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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/helperbin"
	"github.com/daintreehq/spotexec/internal/provider"
)

// ControlPersist keeps the master connection alive between runs.
const ControlPersist = "15m"

// muxTimeout bounds a control command (-O check/stop/exit). They only talk
// to the local master process, so this is generous.
const muxTimeout = 5 * time.Second

// dialTimeout bounds starting a control master.
const dialTimeout = time.Minute

// Client runs commands on one target.
type Client struct {
	T       provider.Target
	ctlPath string
	// ctlErr is why there is no private directory for the control socket.
	// Every call that would hand ctlPath to ssh fails with it instead.
	ctlErr   error
	WorkRoot string // remote work root (may start with ~/)
}

// SetupErr is why the client can't connect from this machine at all (a
// *SetupError), or nil.
func (c *Client) SetupErr() error { return c.ctlErr }

// setup re-checks the client's control directory before its path goes to
// ssh: one checked when the client was made may since have been cleaned
// away, and its name taken by another user. One cleaned away is made anew.
func (c *Client) setup() error {
	if c.ctlErr != nil {
		return c.ctlErr
	}
	if err := privateDir(filepath.Dir(c.ctlPath)); err != nil {
		return &SetupError{err}
	}
	return nil
}

// New builds a client for a target. Without a private directory for its
// control socket the client can't connect: its calls return that error.
func New(t provider.Target, workRoot string) *Client {
	c := &Client{T: t, WorkRoot: workRoot}
	dir, err := controlDir()
	if err != nil {
		// ctlPath still names the connection (helper markers key on it),
		// but nothing hands it to ssh.
		dir, c.ctlErr = "unavailable", err
	}
	c.ctlPath = filepath.Join(dir, connID(t))
	return c
}

// connID identifies a target's effective connection settings: it names the
// control socket (and so the helper markers). Everything that can change
// where or how ssh connects is part of it, the runner's ssh_options included
// (a Port or ProxyJump there makes a different endpoint). The known_hosts
// file is per instance for ephemeral VMs, so a Spot IP reused by a new
// instance gets its own master and helper markers.
func connID(t provider.Target) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%s|%s", t.Host, t.User, t.Port, t.IdentityFile, t.KnownHostsFile)
	for _, o := range t.Options {
		// Options keep their order (ssh uses the first value it sees);
		// a NUL can't occur in an argv string, so it separates them safely.
		fmt.Fprintf(h, "\x00%s", o)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// controlDir holds ControlMaster sockets. OpenSSH appends 17 bytes to the
// path while binding and macOS limits socket paths to 104 bytes, so a long
// (or missing) home falls back to a short per-user directory in /tmp.
// controlDir is re-checked on every call: a long-running process (the
// daemon) must survive the directory being cleaned away, and in shared /tmp
// it must still be ours (a reaper may delete it and another user recreate
// it). It never returns a directory it couldn't make private: a socket path
// anyone else can reach would let them stand in for the master.
var (
	ctlMu  sync.Mutex
	ctlDir string
	// ctlShared says ctlDir is in shared tmpRoot, where another user may
	// get to the name first.
	ctlShared bool
	// tmpRoot is where the short fallback lives (a variable for tests).
	tmpRoot = "/tmp"
)

func controlDir() (string, error) {
	ctlMu.Lock()
	defer ctlMu.Unlock()
	shared := filepath.Join(tmpRoot, fmt.Sprintf("sx-cm-%d", os.Getuid()))
	if ctlDir == "" {
		home, _ := os.UserHomeDir()
		// OpenSSH expands %-tokens and ${VAR} in ControlPath: a home with
		// either would name another directory than the one checked here.
		if d := filepath.Join(home, ".cache", "spotexec", "cm"); home != "" && len(d)+17+17 <= 100 && !strings.ContainsAny(d, "%$") {
			ctlDir, ctlShared = d, false
		} else {
			ctlDir, ctlShared = shared, true
		}
	}
	var herr error
	if !ctlShared {
		herr = os.MkdirAll(filepath.Dir(ctlDir), 0o700)
		if herr == nil {
			herr = privateDir(ctlDir)
		}
		if herr == nil {
			return ctlDir, nil
		}
		// A home that can't hold a private directory (under a group-writable
		// /home, say): the short one in tmp serves as well.
		ctlDir, ctlShared = shared, true
	}
	perr := privateDir(ctlDir)
	if perr == nil {
		return ctlDir, nil
	}
	d, err := os.MkdirTemp(tmpRoot, "sx-cm-")
	if err == nil {
		err = privateDir(d) // the umask may have taken the owner's bits
	}
	if err != nil {
		if herr != nil {
			perr = fmt.Errorf("%v; %v", herr, perr)
			ctlDir = "" // the home may recover: try it first again
		}
		return "", &SetupError{fmt.Errorf("%v, and no private alternative: %w", perr, err)}
	}
	ctlDir = d
	return ctlDir, nil
}

// SetupError means this machine has no private directory for SSH control
// sockets, so no connection can be set up from it. It says nothing about
// any target: the cause is local.
type SetupError struct{ Err error }

func (e *SetupError) Error() string { return "ssh control directory: " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

// IsSetupError reports whether err is (or wraps) a SetupError.
func IsSetupError(err error) bool {
	var se *SetupError
	return errors.As(err, &se)
}

// CheckControlDir reports whether this machine can set up SSH connections
// at all: callers about to launch VMs check it first, rather than launch
// ones they could never reach.
func CheckControlDir() error {
	_, err := controlDir()
	return err
}

// privateDir makes sure dir exists, is a real directory (not a symlink),
// owned by us, mode 0700, and that no other user can put anything in its
// place or in it. One others could write to is refused, not fixed, as is
// one holding anything of theirs: a socket they left there would stand in
// for the master.
func privateDir(dir string) error {
	if err := safeAncestors(dir); err != nil {
		return err
	}
	created := true
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		created = false
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by this user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users (remove it, and spotexec makes a private one)", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// Once ours and private, nobody else can add to it: one scan per
	// directory per process is enough, and a new one needs none. (Lock
	// files pile up per VM, and a client is made per VM per daemon round.)
	// Keyed by inode and mtime: anything added to it since changes the time,
	// even if its mode was opened and closed again meanwhile.
	key := scanKey{uint64(st.Ino), fi.ModTime()}
	if k, _ := scannedDirs.Load(dir); created || k == key {
		scannedDirs.Store(dir, key)
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		efi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // gone meanwhile
		}
		if err != nil {
			return err // one we can't even look at isn't known to be ours
		}
		if est, ok := efi.Sys().(*syscall.Stat_t); !ok || !trustedUID(est.Uid) {
			return fmt.Errorf("%s holds %s, which is another user's (remove it, and spotexec makes a private one)", dir, e.Name())
		}
	}
	scannedDirs.Store(dir, key)
	return nil
}

// scannedDirs maps the private directories privateDir has found holding
// nothing of another user's to how they were then (a scanKey).
var scannedDirs sync.Map

type scanKey struct {
	ino   uint64
	mtime time.Time
}

func trustedUID(uid uint32) bool { return uid == 0 || int(uid) == os.Getuid() }

// safeAncestors checks that no other user can swap dir for one of theirs by
// changing a directory on the way to it.
func safeAncestors(dir string) error {
	_, err := safePath(filepath.Dir(dir), 0)
	return err
}

// safePath checks every directory from the root to p as the kernel walks
// it, following each symlink through every directory on its way: each is
// owned by root or us, and one others may write to is sticky (as /tmp is),
// so they can't move our entries in it. A symlink's own mode means nothing,
// but its owner does. It returns p's real path.
func safePath(p string, depth int) (string, error) {
	if depth > 32 {
		return "", fmt.Errorf("%s: too many symlinks", p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s is not an absolute path", p)
	}
	cur := "/"
	if err := safeDir(cur); err != nil {
		return "", err
	}
	for _, name := range strings.Split(p, "/") {
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur) // cur is a real path: no symlink to undo
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			if err := safeDir(next); err != nil {
				return "", err
			}
			cur = next
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !trustedUID(st.Uid) {
			return "", fmt.Errorf("%s is owned by another user", next)
		}
		t, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(t, "/") {
			t = cur + "/" + t // not filepath.Join: ".." must follow what came before it
		}
		if cur, err = safePath(t, depth+1); err != nil {
			return "", err
		}
	}
	return cur, nil
}

// safeDir checks one directory on the way to the control directory.
func safeDir(d string) error {
	fi, err := os.Lstat(d)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !trustedUID(st.Uid) {
		return fmt.Errorf("%s is owned by another user", d)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("%s is writable by other users", d)
	}
	return nil
}

// baseArgs builds the ssh options. ssh uses the first value it sees for an
// option, so the order is precedence: the control settings spotexec depends
// on can't be overridden, the target's own settings come next, then the
// runner's ssh_options, and spotexec's tunable defaults last, so ssh_options
// can change them.
func (c *Client) baseArgs(master string) []string {
	ctl := c.ctlPath
	if c.ctlErr != nil {
		ctl = "none" // never a path whose directory isn't private
	}
	a := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=" + master,
		"-o", "ControlPath=" + ctl,
		"-o", "ControlPersist=" + ControlPersist,
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
	return append(a,
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	)
}

// Args returns full ssh argv for running remoteCmd, a POSIX shell command.
func (c *Client) Args(remoteCmd string) []string {
	// "--" ends options: the host can never be read as one.
	return append(append(c.baseArgs("no"), "--", c.T.Host), posixCommand(remoteCmd))
}

// posixCommand wraps a POSIX shell command so that any login shell runs it
// with /bin/sh. sshd hands the command to the user's login shell, which on
// a homelab runner may be fish, nushell or tcsh. All of them read
// `/bin/sh -c '...'` the same way as long as the quoted text has no single
// quote, backslash, newline or `!`; a command with any of those is passed as
// octal escapes (a backslash followed by a digit means the same in every one
// of those shells' single quotes), which sh's printf decodes.
func posixCommand(cmd string) string {
	if !strings.ContainsAny(cmd, "'\\\n!") {
		return "/bin/sh -c '" + cmd + "'"
	}
	var b strings.Builder
	for i := 0; i < len(cmd); i++ {
		fmt.Fprintf(&b, "\\%03o", cmd[i])
	}
	return `/bin/sh -c 'eval "$(printf "` + b.String() + `")"'`
}

// MasterAlive reports whether a control master is up. It only asks the
// local master process: a master whose connection died (the laptop slept or
// changed networks) still answers. Open finds those out and replaces them.
func (c *Client) MasterAlive(ctx context.Context) bool {
	_, ok := c.masterPID(ctx)
	return ok
}

// masterPID asks the control master whether it is running, and its pid (0
// if ssh didn't say).
func (c *Client) masterPID(ctx context.Context) (int, bool) {
	if c.setup() != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()
	var errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", "-o", "ControlPath="+c.ctlPath, "-O", "check", "--", c.T.Host)
	cmd.Stderr = &errb
	if cmd.Run() != nil {
		return 0, false
	}
	// "Master running (pid=12345)"
	_, after, _ := strings.Cut(errb.String(), "(pid=")
	pid, _ := strconv.Atoi(strings.TrimRight(strings.TrimSpace(after), ")"))
	return pid, true
}

// lockMaster serializes starting and retiring this target's master across
// processes. Without flock (some network filesystems) it goes unlocked.
func (c *Client) lockMaster(ctx context.Context) (func(), error) {
	if err := c.setup(); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(c.ctlPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, nil
	}
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN); lf.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			lf.Close()
			return func() {}, nil
		}
		select {
		case <-ctx.Done():
			lf.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// EnsureMaster starts the control master if needed.
func (c *Client) EnsureMaster(ctx context.Context) error {
	_, _, err := c.ensureMaster(ctx)
	return err
}

// ensureMaster starts the control master if needed. It returns the master's
// pid (0 if unknown) and whether it dialed a new connection (false: an
// existing master is reused). The master is started with -f -N and
// file-backed stderr: a backgrounded master inheriting our pipes would
// otherwise hold them open and hang every caller.
func (c *Client) ensureMaster(ctx context.Context) (int, bool, error) {
	if err := c.setup(); err != nil {
		return 0, false, err
	}
	if pid, ok := c.masterPID(ctx); ok {
		return pid, false, nil
	}
	// One process starts the master; the others wait and reuse it. (Racing
	// starters lose the socket bind and leave stray `ssh -N` processes.)
	unlock, err := c.lockMaster(ctx)
	if err != nil {
		return 0, false, err
	}
	defer unlock()
	if pid, ok := c.masterPID(ctx); ok {
		return pid, false, nil
	}
	errf, err := os.CreateTemp("", "spotexec-ssh-*.err")
	if err != nil {
		return 0, false, err
	}
	defer os.Remove(errf.Name())
	defer errf.Close()
	args := append(c.baseArgs("auto"), "-f", "-N", "--", c.T.Host)
	// ConnectTimeout covers connecting; this also bounds a server that
	// accepts but never finishes the handshake.
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	cmd := exec.CommandContext(dctx, "ssh", args...)
	cmd.Stdout = nil
	cmd.Stderr = errf
	if err := cmd.Run(); err != nil {
		b, _ := os.ReadFile(errf.Name())
		return 0, false, &ConnError{Target: c.T.String(), Msg: strings.TrimSpace(string(b)), Err: err}
	}
	pid, _ := c.masterPID(ctx)
	return pid, true, nil
}

// CloseMaster stops the control master, ending every session on it.
func (c *Client) CloseMaster() {
	if c.setup() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), muxTimeout)
	defer cancel()
	_ = exec.CommandContext(ctx, "ssh", "-o", "ControlPath="+c.ctlPath, "-O", "exit", "--", c.T.Host).Run()
}

// dropMaster retires the master with this pid (0: whichever is running)
// after it failed to open a session: it stops accepting new sessions and
// frees the control path at once, so the next attempt dials afresh. Unlike
// CloseMaster it leaves sessions already on it alone: if the master was only
// slow, other runs using it carry on, and if it is dead its own keepalives
// end it. A master another run has already put in its place is kept.
func (c *Client) dropMaster(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), muxTimeout)
	defer cancel()
	unlock, err := c.lockMaster(ctx)
	if err != nil {
		return
	}
	defer unlock()
	if cur, ok := c.masterPID(ctx); !ok || (pid != 0 && cur != 0 && cur != pid) {
		return
	}
	_ = exec.CommandContext(ctx, "ssh", "-o", "ControlPath="+c.ctlPath, "-O", "stop", "--", c.T.Host).Run()
}

// retireMaster retires the master with this pid as dropMaster does, but
// only while that exact master is the one in use: a session can outlive its
// master's replacement, which it must never stop. pid 0 (unknown) retires
// nothing.
func (c *Client) retireMaster(pid int) {
	if pid <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), muxTimeout)
	defer cancel()
	unlock, err := c.lockMaster(ctx)
	if err != nil {
		return
	}
	defer unlock()
	if cur, ok := c.masterPID(ctx); !ok || cur != pid {
		return
	}
	_ = exec.CommandContext(ctx, "ssh", "-o", "ControlPath="+c.ctlPath, "-O", "stop", "--", c.T.Host).Run()
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
	if err := c.setup(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh", c.Args(remoteCmd)...)
	cmd.Stdin = stdin
	// A session on a master hands its descriptors to the master, so killing
	// the client (on ctx's deadline) doesn't close our pipes while the
	// master holds a stuck channel: don't wait on them for long.
	cmd.WaitDelay = 2 * time.Second
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

// probeTimeout bounds the first remote command of a helper install, which
// may dial a new connection (ConnectTimeout=10 by default), and the check
// after it; uploadTimeout bounds the upload (a few MB).
var (
	probeTimeout  = 20 * time.Second
	uploadTimeout = 5 * time.Minute
)

// runWithin is Run bounded by d: running out of time (while ctx itself is
// still live) is a connection failure, since nothing got through.
func (c *Client) runWithin(ctx context.Context, d time.Duration, remoteCmd string, stdin io.Reader) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	out, err := c.Run(rctx, remoteCmd, stdin)
	if err != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return out, &ConnError{Target: c.T.String(), Msg: "no answer from the runner", Err: context.DeadlineExceeded}
	}
	return out, err
}

// Reach is what a reachability probe found (Client.Reachable).
type Reach int

const (
	// ReachUnknown: the probe failed in a way that says nothing about the
	// network (authentication, the remote shell, ssh itself).
	ReachUnknown Reach = iota
	// Reachable: the target answered.
	Reachable
	// Unreachable: no answer, or the network said there is no way to it.
	Unreachable
)

// Reachable probes the target with a trivial command over a connection of
// its own (never the runner's ControlMaster, whose state proves nothing
// about the network now), within d. Only a timeout, or ssh reporting that
// the network can't get there, counts as Unreachable.
func (c *Client) Reachable(ctx context.Context, d time.Duration) Reach {
	rctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	own := *c
	own.ctlPath, own.ctlErr = "none", nil
	cmd := exec.CommandContext(rctx, "ssh", own.Args("true")...)
	cmd.WaitDelay = 2 * time.Second
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	switch {
	case err == nil:
		return Reachable
	case ctx.Err() != nil:
		return ReachUnknown // given up on, not timed out
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		return Unreachable
	}
	return probeResult(err, errb.String())
}

// probeResult classifies a probe that failed in time: ssh's own failure
// (exit 255) by what it printed, the remote shell failing as an answer,
// and anything else (ssh couldn't start, or was killed) as unknown.
func probeResult(err error, stderr string) Reach {
	var ee *exec.ExitError
	switch {
	case !errors.As(err, &ee) || ee.ExitCode() < 0:
		return ReachUnknown
	case ee.ExitCode() != 255:
		return Reachable // the remote shell ran, and failed
	}
	return networkFailure(stderr)
}

// networkFailure classifies the error ssh printed when it couldn't connect.
func networkFailure(stderr string) Reach {
	s := strings.ToLower(stderr)
	for _, m := range []string{"timed out", "no route to host", "network is unreachable", "host is down"} {
		if strings.Contains(s, m) {
			return Unreachable
		}
	}
	return ReachUnknown
}

// EnsureHelper makes sure the right helper is installed and returns the
// remote command prefix that invokes it.
// HelperUploadBudget is the extra time a first contact gets to upload the
// helper (a few MB): over a slow link that alone can take most of a short
// probe deadline, and an upload cut short is started over from nothing.
const HelperUploadBudget = 2 * time.Minute

// HelperInstalled reports whether this exact helper was installed on the
// target before (so connecting needs no upload). It makes no remote call.
func (c *Client) HelperInstalled() bool {
	_, ok := c.installedHash()
	return ok
}

// installedHash is the hash of the helper the markers say is installed, if
// this binary still embeds exactly that helper. One read of each marker:
// another process may rewrite or remove them at any time.
func (c *Client) installedHash() (string, bool) {
	b, err := os.ReadFile(markerPathAny(c))
	if err != nil {
		return "", false
	}
	f := strings.Fields(string(b))
	if len(f) != 3 {
		return "", false
	}
	_, cur, err := helperbin.Get(f[0], f[1])
	if err != nil || cur != f[2] {
		return "", false
	}
	if _, err := os.Stat(markerPath(c, cur)); err != nil {
		return "", false
	}
	return cur, true
}

func (c *Client) EnsureHelper(ctx context.Context, force bool) (string, error) {
	return c.ensureHelper(ctx, force, probeTimeout)
}

// ensureHelper is EnsureHelper with a bound on the first remote command:
// over a reused master it is short, so a dead one is found out quickly.
func (c *Client) ensureHelper(ctx context.Context, force bool, probe time.Duration) (string, error) {
	// Fast path: we've installed this exact helper on this host before.
	// The marker records the remote platform and the hash we installed; it
	// is only trusted if this binary still embeds that exact helper, so an
	// upgraded CLI reinstalls instead of driving a stale helper.
	if err := c.setup(); err != nil {
		return "", err // before the markers: their fast path must not hide it
	}
	if !force {
		if hash, ok := c.installedHash(); ok {
			return c.helperPath(hash), nil
		}
	}
	// The first command through the master doubles as a liveness probe: a
	// dead connection must fail (and be replaced by Open) rather than hang.
	out, err := c.runWithin(ctx, probe, "uname -sm", nil)
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
		check, err = c.runWithin(ctx, probe, "test -x "+path+" && echo present || true", nil)
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(check)) != "present" {
		dir := RemoteRoot(c.WorkRoot) + "/bin"
		// An upload is activity (at its start and end): an ephemeral VM's
		// idle watchdog must not stop a VM still being readied over a slow
		// link. The upload is bounded by the probe's HelperUploadBudget,
		// well under any idle_timeout. Only a root-owned state directory is
		// a spotexec VM's (see helper.onSpotexecVM): on a shared runner,
		// anyone can create that path.
		script := fmt.Sprintf(`set -e; a=; [ -n "$(find /var/tmp/spotexec -maxdepth 0 -type d -user 0 ! -perm -0002 2>/dev/null)" ] && a=/var/tmp/spotexec/activity || true; [ -n "$a" ] && [ -w "$a" ] && touch "$a" || true; mkdir -p %[1]s; t=%[1]s/.upload.$$; gzip -dc > "$t"; chmod 755 "$t"; mv -f "$t" %[2]s; [ -n "$a" ] && [ -w "$a" ] && touch "$a" || true; find %[1]s -maxdepth 1 -name 'spotexec-helper-*' ! -name %[3]s -mmin +1440 -exec rm -f {} + 2>/dev/null || true`, dir, path, ShellQuote(filepath.Base(path)))
		if _, err := c.runWithin(ctx, uploadTimeout, script, bytes.NewReader(gz)); err != nil {
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

// ErrHelperNotInstalled: HelperCmdInstalled found no helper this machine
// installed on the target.
var ErrHelperNotInstalled = errors.New("helper not installed on this runner")

// HelperCmdInstalled runs a helper subcommand like HelperCmd, but only if
// this machine already installed this exact helper on the target: it never
// uploads or reinstalls one, which would touch an ephemeral VM's idle clock.
func (c *Client) HelperCmdInstalled(ctx context.Context, args ...string) ([]byte, error) {
	if err := c.setup(); err != nil {
		return nil, err
	}
	hash, ok := c.installedHash()
	if !ok {
		return nil, ErrHelperNotInstalled
	}
	q := []string{c.helperPath(hash)}
	for _, a := range args {
		q = append(q, ShellQuote(a))
	}
	q = append(q, "--work-root", ShellQuote(c.WorkRoot))
	return c.Run(ctx, strings.Join(q, " "), nil)
}

func isConn(err error) bool {
	var ce *ConnError
	return errors.As(err, &ce)
}

// IsConnError reports whether err is an SSH connectivity failure.
func IsConnError(err error) bool { return isConn(err) }
