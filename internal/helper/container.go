package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/daintreehq/spotexec/internal/dockerignore"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// Project containers: commands run via `docker run` with the slot workspace
// bind-mounted at the same absolute path, so path rewriting, snapshots and
// write-back work exactly as on the host. Commands run as the runner user by
// default (files keep the right owner), with host networking (per-slot port
// ranges still apply) unless container.network is "private" (networkArgs).
//
// Docker is always driven as the runner user (directly, or via `sg docker`
// when the docker group was granted after this login), never via sudo: the
// helper must be able to signal every process it starts, and sudo would
// also strip the private registry config.

var imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,255}$`)

// dockerRunner composes shell commands that run docker. info is what the
// probe's `docker info` printed (stdout and stderr), for the daemon's CPUs.
type dockerRunner struct {
	viaSG bool
	info  string
}

// cmd returns a shell command running `docker <args>` (args already quoted).
func (d dockerRunner) cmd(args string) string {
	if d.viaSG {
		return "sg docker -c " + shq("exec docker "+args)
	}
	return "docker " + args
}

// runShell runs script in its own process group; cancellation kills the
// whole group (docker under sh, sg wrappers), and output pipes are not
// waited on forever.
func runShell(ctx context.Context, script string) ([]byte, error) {
	return shellCmd(ctx, script).CombinedOutput()
}

func shellCmd(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// probeDocker finds a way to use docker as this user.
func probeDocker(ctx context.Context) (dockerRunner, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return dockerRunner{}, errors.New("docker is not installed")
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info := func(name string, arg ...string) (string, error) {
		cmd := exec.CommandContext(pctx, name, arg...)
		cmd.WaitDelay = 5 * time.Second // a docker left behind by a killed sg can't hold the output pipe open
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := info("docker", "info"); err == nil {
		return dockerRunner{info: out}, nil
	}
	if inDockerGroup() {
		if out, err := info("sg", "docker", "-c", "docker info"); err == nil {
			return dockerRunner{viaSG: true, info: out}, nil
		}
	}
	return dockerRunner{}, errors.New("docker is installed but this user cannot use it (add it to the docker group)")
}

func inDockerGroup() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	g, err := user.LookupGroup("docker")
	if err != nil {
		return false
	}
	ids, _ := u.GroupIds()
	for _, id := range ids {
		if id == g.Gid {
			return true
		}
	}
	return false
}

// ensureDocker installs Docker (plus buildx) on Debian/Ubuntu runners that
// lack it, once per VM under a lock, and grants the runner user the docker
// group. Output streams to the client.
func (s *Session) ensureDocker(ctx context.Context) (dockerRunner, bool, error) {
	if d, err := probeDocker(ctx); err == nil {
		return d, false, nil
	}
	lk, err := s.waitLockCtx(ctx, filepath.Join(s.layout.Root, "docker-install.lock"))
	if err != nil {
		return dockerRunner{}, false, err
	}
	defer lk.Unlock()
	if d, err := probeDocker(ctx); err == nil {
		return d, false, nil // another slot just installed it
	}
	if exec.Command("sudo", "-n", "true").Run() != nil {
		return dockerRunner{}, false, errors.New("docker is not usable and passwordless sudo is unavailable to set it up; install docker and add this user to the docker group")
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return dockerRunner{}, false, errors.New("docker is not installed (automatic install supports Debian/Ubuntu runners only)")
	}
	u, _ := user.Current()
	_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "docker_install", Message: "installing docker on the runner (add `apt-get install -y docker.io docker-buildx` and `usermod -aG docker " + u.Username + "` to bootstrap_script to skip this)"})
	// apt's output goes to a log, shown only if the install fails: the
	// event above already says what is happening, and an agent reading the
	// run's output shouldn't get dpkg's progress. sudo resets the
	// environment, so DEBIAN_FRONTEND goes through env. Chained with &&
	// rather than set -e, which an if condition would disable.
	script := `log=` + shq(filepath.Join(s.layout.Root, "docker-install.log")) + `
if ! (
  { command -v docker >/dev/null 2>&1 || {
    sudo -n env DEBIAN_FRONTEND=noninteractive apt-get update -o DPkg::Lock::Timeout=300 -qq &&
    sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq -o DPkg::Lock::Timeout=300 docker.io docker-buildx; }; } &&
  { sudo -n systemctl enable --now docker || sudo -n service docker start || true; } &&
  sudo -n usermod -aG docker ` + shq(u.Username) + `
) >"$log" 2>&1; then
  echo "installing docker failed; the end of $log:" >&2
  tail -n 40 "$log" >&2
  exit 1
fi`
	left := min(10*time.Minute, time.Until(deadlineOf(ctx)))
	if left <= 0 {
		return dockerRunner{}, false, errors.New("no time left to install docker")
	}
	pr := s.runProc(script, s.layout.Root, nil, left, 5*time.Second, false, nil)
	if pr.code != 0 {
		return dockerRunner{}, false, fmt.Errorf("installing docker failed (exit %d)", pr.code)
	}
	d, err := probeDocker(ctx)
	return d, true, err
}

// imageName turns a repo id into a valid Docker/ECR repository component.
func imageName(repoID string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(repoID) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Session) image(m protocol.Image) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before image")
	}
	start := time.Now()
	timeout := time.Duration(m.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d, installed, err := s.ensureDocker(ctx)
	if err != nil {
		return perr(protocol.CodeImageFailed, "%v", err)
	}
	res := protocol.ImageResult{InstalledDocker: installed}
	exists := func(ref string) bool {
		_, err := runShell(ctx, d.cmd("image inspect "+shq(ref))+" >/dev/null 2>&1")
		return err == nil
	}
	stream := func(script string) error {
		left := time.Until(deadlineOf(ctx))
		if left <= 0 {
			return errors.New("image preparation timed out")
		}
		pr := s.runProc(script, s.workspace, nil, left, 5*time.Second, false, nil)
		if pr.startErr != nil {
			return pr.startErr
		}
		if pr.code != 0 {
			return fmt.Errorf("exit %d", pr.code)
		}
		return nil
	}
	switch {
	case m.Ref != "":
		if !imageRefRe.MatchString(m.Ref) {
			return perr(protocol.CodeBadRequest, "invalid image reference %q", m.Ref)
		}
		res.Image = m.Ref
		if exists(m.Ref) {
			break
		}
		lk, err := s.waitLockCtx(ctx, filepath.Join(s.layout.Root, "images", "pull-"+imageName(m.Ref)+".lock"))
		if err != nil {
			return err
		}
		defer lk.Unlock()
		if !exists(m.Ref) {
			_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_pull", Message: m.Ref})
			if err := stream(d.cmd("pull -q " + shq(m.Ref))); err != nil {
				return perr(protocol.CodeImageFailed, "docker pull %s: %v", m.Ref, err)
			}
			res.Pulled = true
		}
	case m.Dockerfile != "":
		if !regexp.MustCompile(`^[a-f0-9]{8,64}$`).MatchString(m.Tag) {
			return perr(protocol.CodeBadRequest, "invalid image tag")
		}
		local := "spotexec-local/" + imageName(s.repoID) + ":" + m.Tag
		res.Image = local
		// container build: an image already here (built by an earlier run
		// whose push may have failed) is still pushed, so the registry has it.
		pushExisting := func() error {
			if !m.WaitPush || m.Registry == nil {
				return nil
			}
			cfgDir, err := dockerLogin(ctx, d, m.Registry)
			if err != nil {
				return perr(protocol.CodeImageFailed, "registry login: %v", err)
			}
			defer os.RemoveAll(cfgDir)
			remote := m.Registry.Repo + ":" + m.Tag
			_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_push", Message: remote})
			if err := stream(d.cmd("tag "+shq(local)+" "+shq(remote)) + " && " + d.cmd("--config "+shq(cfgDir)+" push -q "+shq(remote))); err != nil {
				return perr(protocol.CodeImageFailed, "could not push %s to %s: %v", m.Dockerfile, remote, err)
			}
			res.Pushed = true
			return nil
		}
		if exists(local) {
			if err := pushExisting(); err != nil {
				return err
			}
			break
		}
		// One build per tag per runner; other slots wait for it.
		lk, err := s.waitLockCtx(ctx, filepath.Join(s.layout.Root, "images", "build-"+imageName(s.repoID)+"-"+m.Tag+".lock"))
		if err != nil {
			return err
		}
		defer lk.Unlock()
		if exists(local) {
			if err := pushExisting(); err != nil {
				return err
			}
			break
		}
		cfgDir := ""
		if m.Registry != nil {
			if cfgDir, err = dockerLogin(ctx, d, m.Registry); err != nil {
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_registry_error", Message: err.Error()})
			} else {
				defer func() {
					if !res.Pushing {
						os.RemoveAll(cfgDir)
					}
				}()
				remote := m.Registry.Repo + ":" + m.Tag
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_pull", Message: remote})
				out, perr2 := runShell(ctx, d.cmd("--config "+shq(cfgDir)+" pull -q "+shq(remote))+" && "+d.cmd("tag "+shq(remote)+" "+shq(local)))
				if perr2 == nil {
					res.Pulled = true
					break
				}
				if msg := string(out); !strings.Contains(msg, "not found") && !strings.Contains(msg, "manifest unknown") {
					_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_registry_error", Message: firstLines(msg, 3)})
				}
			}
		}
		if m.Probe {
			// The client decides who builds (one run for every runner
			// waiting on this tag), and asks again.
			res.Missing = true
			res.MS = time.Since(start).Milliseconds()
			return s.c.Send(protocol.TypeImageResult, res)
		}
		buildCtx, dockerfile, cleanup, err := s.stageContext(m)
		if err != nil {
			return perr(protocol.CodeImageFailed, "%v", err)
		}
		defer cleanup()
		_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_build", Message: m.Dockerfile})
		if err := stream(d.cmd("build -f " + shq(dockerfile) + " -t " + shq(local) + " " + shq(buildCtx))); err != nil {
			return perr(protocol.CodeImageFailed, "docker build %s: %v", m.Dockerfile, err)
		}
		res.Built = true
		if m.Registry != nil && cfgDir != "" {
			remote := m.Registry.Repo + ":" + m.Tag
			if m.WaitPush {
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_push", Message: remote})
				if err := stream(d.cmd("tag "+shq(local)+" "+shq(remote)) + " && " + d.cmd("--config "+shq(cfgDir)+" push -q "+shq(remote))); err != nil {
					return perr(protocol.CodeImageFailed, "built %s but could not push it to %s: %v", m.Dockerfile, remote, err)
				}
				res.Pushed = true
			} else if t, err := startImagePush(s.layout.Root, d, local, remote, cfgDir); err == nil {
				s.track(protocol.ShareImage, t)
				res.Pushing = true
			}
		}
	default:
		return perr(protocol.CodeBadRequest, "image: nothing to do")
	}
	// Report the immutable image ID: dependency caches are keyed by it, so a
	// moving tag (node:22, :latest) can never restore modules built against
	// a different image.
	if out, err := runShell(ctx, d.cmd("image inspect --format '{{.Id}}' "+shq(res.Image))); err == nil {
		res.ImageID = strings.TrimSpace(string(out))
	}
	res.MS = time.Since(start).Milliseconds()
	return s.c.Send(protocol.TypeImageResult, res)
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(20 * time.Minute)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

// stageContext builds the docker build context from exactly the synced
// files under the context directory (hardlinked from the workspace), so the
// image depends only on the inputs the content tag hashed, never on
// leftovers (node_modules, build output) in the slot. The Dockerfile (and a
// companion <Dockerfile>.dockerignore) are copied to a separate directory,
// never written inside the hardlinked tree.
func (s *Session) stageContext(m protocol.Image) (string, string, func(), error) {
	s.loadStored()
	ctxRel, err := normContext(m.Context, m.Dockerfile)
	if err != nil {
		return "", "", nil, err
	}
	root, err := os.MkdirTemp(s.slotDir, "build-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := func() { os.RemoveAll(root) }
	dir := filepath.Join(root, "context")
	meta := filepath.Join(root, "meta")
	for _, d := range []string{dir, meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			cleanup()
			return "", "", nil, err
		}
	}
	// Apply .dockerignore exactly as the client did when computing the tag,
	// so the image depends only on what was hashed.
	var rules []byte
	for _, cand := range dockerignore.Source(m.Dockerfile, ctxRel) {
		if _, ok := s.stored[cand]; ok {
			p, err := fsid.SafeJoin(s.workspace, cand)
			if err == nil {
				if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
					err = fmt.Errorf("%s is a symlink", cand)
				} else {
					rules, err = os.ReadFile(p)
				}
			}
			if err != nil {
				cleanup()
				return "", "", nil, fmt.Errorf("reading %s: %w", cand, err)
			}
			break
		}
	}
	ign, err := dockerignore.New(rules)
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("parsing .dockerignore: %w", err)
	}
	for rel := range s.stored {
		if ctxRel != "" && rel != ctxRel && !strings.HasPrefix(rel, ctxRel+"/") {
			continue
		}
		inner := strings.TrimPrefix(strings.TrimPrefix(rel, ctxRel), "/")
		if ign.Excluded(inner) {
			continue
		}
		// Every hashed file must be staged: an image missing one would be
		// pushed under a tag that claims to include it.
		src, err := fsid.SafeJoin(s.workspace, rel)
		if err != nil {
			cleanup()
			return "", "", nil, perr(protocol.CodeImageFailed, "build context file %s: %v", rel, err)
		}
		dst, err := fsid.PrepareParent(dir, inner)
		if err != nil {
			cleanup()
			return "", "", nil, err
		}
		// Symlinks are recreated as links (link(2) follows them on macOS,
		// which would copy a file from outside the workspace into the image).
		fi, err := os.Lstat(src)
		if err != nil {
			cleanup()
			return "", "", nil, perr(protocol.CodeImageFailed, "build context file %s is missing from the workspace: %v", rel, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err == nil {
				err = os.Symlink(target, dst)
			}
			if err != nil {
				cleanup()
				return "", "", nil, fmt.Errorf("staging %s: %v", rel, err)
			}
			continue
		}
		if err := os.Link(src, dst); err != nil {
			if out, cerr := exec.Command("cp", "-p", src, dst).CombinedOutput(); cerr != nil {
				cleanup()
				return "", "", nil, fmt.Errorf("staging %s: %v: %s", rel, cerr, out)
			}
		}
	}
	if _, ok := s.stored[m.Dockerfile]; !ok {
		cleanup()
		return "", "", nil, perr(protocol.CodeImageFailed, "dockerfile %s is not in the synced files", m.Dockerfile)
	}
	df, err := fsid.SafeJoin(s.workspace, m.Dockerfile)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	if fi, err := os.Lstat(df); err != nil || !fi.Mode().IsRegular() {
		cleanup()
		return "", "", nil, perr(protocol.CodeImageFailed, "dockerfile %s must be a regular file (not a symlink)", m.Dockerfile)
	}
	b, err := os.ReadFile(df)
	if err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("dockerfile: %w", err)
	}
	dockerfile := filepath.Join(meta, "Dockerfile")
	if err := os.WriteFile(dockerfile, b, 0o644); err != nil {
		cleanup()
		return "", "", nil, err
	}
	// The companion ignore file only if it was synced (as the client hashed),
	// and as BuildKit would use it on top of our staging.
	if _, ok := s.stored[m.Dockerfile+".dockerignore"]; ok {
		if fi, err := os.Lstat(df + ".dockerignore"); err == nil && fi.Mode().IsRegular() {
			if ign, err := os.ReadFile(df + ".dockerignore"); err == nil {
				_ = os.WriteFile(dockerfile+".dockerignore", ign, 0o644)
			}
		}
	}
	return dir, dockerfile, cleanup, nil
}

// normContext resolves the build context exactly as the client hashes it.
func normContext(ctx, dockerfile string) (string, error) {
	c := ctx
	if c == "" {
		c = path.Dir(dockerfile)
	}
	// Validate before cleaning: "/"+".." would silently become the root.
	c = strings.TrimSuffix(strings.TrimPrefix(c, "./"), "/")
	if c == "" || c == "." {
		return "", nil
	}
	clean, err := fsid.CleanRel(c)
	if err != nil {
		return "", perr(protocol.CodeImageFailed, "container.context %q must stay inside the repository", ctx)
	}
	return clean, nil
}

// waitLock takes an exclusive lock, polling while the client is alive.
func (s *Session) waitLock(path string) (*flock, error) {
	return s.waitLockCtx(context.Background(), path)
}

func (s *Session) waitLockCtx(ctx context.Context, path string) (*flock, error) {
	for {
		if ctx.Err() != nil {
			return nil, perr(protocol.CodeImageFailed, "timed out waiting for %s", filepath.Base(path))
		}
		l, ok, err := tryLock(path)
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		if s.clientGone() {
			return nil, perr(protocol.CodeImageFailed, "client went away while waiting for a lock")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dockerLogin logs into the registry with a private --config directory, so
// the short-lived token never lands in the user's (or root's) docker config.
func dockerLogin(ctx context.Context, d dockerRunner, r *protocol.RegistryAuth) (string, error) {
	dir, err := os.MkdirTemp("", "spotexec-docker-")
	if err != nil {
		return "", err
	}
	cmd := shellCmd(ctx, d.cmd("--config "+shq(dir)+" login --username "+shq(r.User)+" --password-stdin "+shq(r.Server)))
	cmd.Stdin = strings.NewReader(r.Password)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("registry login: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

// startImagePush pushes a freshly built image in a detached process (the
// job doesn't wait), and waits for the child to take the VM's lifecycle lock
// before returning, so a draining watchdog can't slip in between.
func startImagePush(root string, d dockerRunner, local, remote, cfgDir string) (*transfer, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, _ := os.OpenFile(filepath.Join(root, "image-push.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	ready := filepath.Join(root, "tmp", fmt.Sprintf("push-ready-%d-%d", os.Getpid(), time.Now().UnixNano()))
	_ = os.MkdirAll(filepath.Dir(ready), 0o700)
	cmd := exec.Command(self, "image-push", "--work-root", root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	via := "direct"
	if d.viaSG {
		via = "sg"
	}
	fmt.Fprintf(stdin, "%s\n%s\n%s\n%s\n%s\n", via, local, remote, cfgDir, ready)
	stdin.Close()
	if logf != nil {
		logf.Close()
	}
	for i := 0; i < 40; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return watch(cmd), nil
}

// ImagePush is the detached push (spotexec-helper image-push; arguments on
// stdin). It holds the lifecycle lock, is bounded, and stops if the VM
// starts draining.
func ImagePush(root string) error {
	resetOwnOOMScore() // started by a protected helper: don't keep its score
	sc := bufio.NewScanner(os.Stdin)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) < 5 {
		return errors.New("image-push: expected 5 lines on stdin")
	}
	d := dockerRunner{viaSG: lines[0] == "sg"}
	local, remote, cfgDir, ready := lines[1], lines[2], lines[3], lines[4]
	defer os.RemoveAll(cfgDir)
	if hasLifecycleLock() {
		l, err := lockShared(LifecycleLock, 10*time.Second)
		if err != nil {
			return fmt.Errorf("image-push: VM is shutting down: %w", err)
		}
		defer l.Unlock()
	}
	_ = os.WriteFile(ready, nil, 0o600)
	defer os.Remove(ready)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if draining(root) {
				cancel()
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()
	out, err := runShell(ctx, d.cmd("tag "+shq(local)+" "+shq(remote))+" && "+d.cmd("--config "+shq(cfgDir)+" push -q "+shq(remote)))
	fmt.Printf("%s push %s: %s\n", time.Now().Format(time.RFC3339), remote, strings.TrimSpace(string(out)))
	return err
}

// containerScript wraps a command so it runs inside run.Image. The command
// is written to a file (no quoting games) and the environment is passed by
// name (-e KEY, values taken from the docker client's environment, so
// multi-line values survive). The container has a deterministic name so it
// can be force-removed on every exit path.
func (s *Session) containerScript(run *protocol.ContainerRun, script, dir string, env map[string]string) (string, map[string]string, func() error, error) {
	d, err := probeDocker(context.Background())
	if err != nil {
		return "", nil, nil, err
	}
	if !imageRefRe.MatchString(run.Image) {
		return "", nil, nil, fmt.Errorf("invalid image reference %q", run.Image)
	}
	// Checked before anything (env-file with secrets) is written.
	if bad := forbiddenOption(run.Options); bad != "" {
		// These would change what spotexec relies on to stop, find and
		// clean up the container, or who it runs as (use container.user).
		return "", nil, nil, perr(protocol.CodeImageFailed, "container.options may not set %s", bad)
	}
	// Before procMu: these may send an event.
	limits := append(s.cpusetArgs(run, d), s.memoryArgs(run, d)...)
	limits = append(limits, s.shmArgs(run, d)...)
	s.procMu.Lock()
	if s.stopping.Load() {
		s.procMu.Unlock()
		select {}
	}
	defer s.procMu.Unlock()
	runDir := filepath.Join(s.slotDir, "run")
	home := filepath.Join(s.slotDir, "home")
	for _, dd := range []string{runDir, home} {
		if err := os.MkdirAll(dd, 0o755); err != nil {
			return "", nil, nil, err
		}
	}
	id := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	name := s.containerPrefix() + id
	scriptPath := filepath.Join(runDir, id+".sh")
	// The task environment goes to the container only (never into the
	// docker client's own environment, where e.g. DOCKER_HOST would
	// redirect `docker run` itself): single-line values via a 0600
	// env-file, multi-line values as quoted -e KEY=value arguments.
	envPath := filepath.Join(runDir, id+".env")
	if run.Root && s.state.GitOwned {
		env = withSafeDirectory(env, s.workspace)
	}
	if run.Docker {
		env = maps.Clone(env)
		// The run's own socket, whatever the image's ENV says; with host
		// networking, Testcontainers would otherwise look for the ports it
		// maps at the VPC's gateway rather than on this machine.
		// Exec runs get COMPOSE_PROJECT_NAME from the client; the dependency
		// install gets one here, so Compose stacks of concurrent slots differ.
		defaults := map[string]string{
			"DOCKER_HOST":                  "unix:///var/run/docker.sock",
			"TESTCONTAINERS_HOST_OVERRIDE": "localhost",
			"COMPOSE_PROJECT_NAME":         strings.Trim(s.containerPrefix(), "-"),
		}
		for k, v := range defaults {
			if _, set := env[k]; !set {
				env[k] = v
			}
		}
	}
	var envb strings.Builder
	var multi []string
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envKeyRe.MatchString(k) {
			continue
		}
		if strings.ContainsAny(env[k], "\n\r") {
			multi = append(multi, k+"="+env[k])
			continue
		}
		envb.WriteString(k + "=" + env[k] + "\n")
	}
	if err := os.WriteFile(envPath, []byte(envb.String()), 0o600); err != nil {
		return "", nil, nil, err
	}
	// Multi-line values (PEM keys, JSON credentials) can't go in the
	// env-file and must not go on the docker command line (visible in ps):
	// the 0600 run script exports them.
	var sb strings.Builder
	for _, kv := range multi {
		k, v, _ := strings.Cut(kv, "=")
		sb.WriteString("export " + k + "=" + shq(v) + "\n")
	}
	sb.WriteString(script + "\n")
	if err := os.WriteFile(scriptPath, []byte(sb.String()), 0o600); err != nil {
		os.Remove(envPath)
		return "", nil, nil, err
	}
	args := append([]string{"run", "--rm", "--init", "--name", name}, networkArgs(run)...)
	args = append(args,
		"-v", s.workspace+":"+s.workspace,
		"-v", scriptPath+":/tmp/spotexec-run.sh:ro",
		"--env-file", envPath,
		"-w", dir, "--entrypoint", "/bin/sh",
	)
	var proxy *dockerProxy
	if run.Docker {
		pdir, err := newProxyDir()
		if err == nil {
			// Cleared once what the run started is removed: if the run is
			// aborted first, the slot's next holder does it, and removes the
			// socket's directory.
			err = appendLine(filepath.Join(s.slotDir, dockerRunMarker), pdir)
			if err != nil {
				os.RemoveAll(pdir)
			}
		}
		if err == nil {
			proxy, err = startDockerProxy(d, pdir, s.workspace, map[string]string{labelSlot: s.containerPrefix(), labelRun: name})
		}
		if err != nil {
			os.Remove(scriptPath)
			os.Remove(envPath)
			return "", nil, nil, err
		}
		// --mount, not -v: a missing socket fails the run instead of
		// becoming an empty directory.
		args = append(args, "--mount", "type=bind,source="+proxy.sock+",target=/var/run/docker.sock")
	}

	if !run.Root {
		args = append(args, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()),
			"-v", home+":/home/spotexec", "-e", "HOME=/home/spotexec")
	}
	args = append(args, limits...)
	args = append(args, run.Options...)
	args = append(args, run.Image, "-c",
		`if command -v bash >/dev/null 2>&1; then exec bash /tmp/spotexec-run.sh; else exec sh /tmp/spotexec-run.sh; fi`)
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	host := "exec " + d.cmd(strings.Join(q, " "))
	marker := filepath.Join(s.slotDir, rootRunMarker)
	// Containers the run starts itself usually run as root, and may write
	// into the workspace through a bind mount, so their files are given
	// back too.
	giveBack := run.Root || run.Docker
	if giveBack {
		// Cleared once the run's files are given back: if the run is
		// aborted first, the slot's next holder does it (reclaimFiles).
		if err := os.WriteFile(marker, []byte(run.Image+"\n"), 0o644); err != nil {
			if proxy != nil {
				proxy.stop()
			}
			os.Remove(scriptPath)
			os.Remove(envPath)
			return "", nil, nil, err
		}
	}
	s.containers.Store(name, d)
	if proxy != nil {
		s.nested.Store(name, nestedRun{d, proxy})
	}
	s.lastContainer.name, s.lastContainer.d, s.lastContainer.started = name, d, time.Now()
	cleanup := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := removeContainer(ctx, d, name); err != nil {
			return err // keep tracking it; the caller must not snapshot
		}
		s.containers.Delete(name)
		if proxy != nil {
			// Nothing in the run can call Docker any more, so nothing it
			// starts can appear after this sweep.
			proxy.stop()
			if left := removeLabelled(ctx, d, labelRun+"="+name); left != "" {
				return fmt.Errorf("containers, networks or volumes the run started could not be removed: %s", left)
			}
			s.nested.Delete(name)
			pending := false
			s.nested.Range(func(any, any) bool { pending = true; return false })
			if !pending {
				os.Remove(filepath.Join(s.slotDir, dockerRunMarker))
			}
		}
		os.Remove(scriptPath)
		os.Remove(envPath)
		if giveBack {
			switch err := s.giveBackFiles(ctx, &d, run.Image); {
			case err == nil:
				os.Remove(marker)
			case errors.Is(err, errRepairRunning):
				return err
			default:
				// Reported on this run, which made them; the marker stays,
				// so the slot's next holder tries again (reclaimFiles).
				return fmt.Errorf("%w to %s (%v); fix their owner on the runner (sudo chown -R) or remove %s", errFilesNotGivenBack, currentUser(), err, s.workspace)
			}
		}
		return nil
	}
	return host, nil, cleanup, nil
}

// networkArgs picks the container's network. By default it is the
// runner's own (host networking: SPOTEXEC_PORT_BASE ranges apply, and
// services on the runner are reachable). "private" gives each run a
// network namespace of its own on Docker's default bridge, so fixed ports
// (a test server on 3000, a suite's own Postgres on 5432) can't collide
// with another slot's, and localhost reaches only the run's own; outbound
// traffic goes through NAT. It separates ports, not traffic: containers on
// the bridge can still reach one another by IP.
// A --network or --net in container.options wins: the user decides.
func networkArgs(run *protocol.ContainerRun) []string {
	for _, f := range dockerFlags(run.Options) {
		if f == "--network" || f == "--net" {
			return nil
		}
	}
	if run.Network == "private" {
		return []string{"--network", "bridge"}
	}
	return []string{"--network", "host"}
}

// rootRunMarker, in a slot directory, names the image of a root container
// run whose files haven't been given back to the runner user yet.
const rootRunMarker = "root-run"

// dockerRunMarker, in a slot directory, says a container.docker run may
// have left containers, networks or volumes labelled with the slot. It
// lists the directories of those runs' proxy sockets.
const dockerRunMarker = "docker-run"

// nestedRun is a container.docker run whose own containers aren't removed
// yet.
type nestedRun struct {
	d     dockerRunner
	proxy *dockerProxy
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// errRepairRunning: a timed-out ownership repair's container couldn't be
// removed, so it may still be changing files.
var errRepairRunning = errors.New("the ownership fix-up container is still running")

// errFilesNotGivenBack: the run's container is gone, but files it left in
// the workspace are still not the runner user's.
var errFilesNotGivenBack = errors.New("files the container run left in the workspace couldn't be given back")

// giveBackFiles gives files a root container run created in the workspace
// back to the runner user: root-owned files would block later cleanup,
// hydration and write-back reads (EACCES on every sync). Only files with
// another owner are touched (chown -R would bump every file's ctime and
// make untouched files look changed), and it runs through Docker itself
// (d, with the run's image), so no host sudo is needed; `sudo -n` is the
// fallback. It reports what is still not ours afterwards.
func (s *Session) giveBackFiles(ctx context.Context, d *dockerRunner, image string) error {
	u, g := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	fix := "find " + shq(s.workspace) + " \\( ! -user " + u + " -o ! -group " + g + " \\) -exec chown -h " + u + ":" + g + " {} +"
	ok := false
	if d != nil && image != "" {
		// Named like the slot's containers, so if this helper dies midway
		// the next holder removes it before doing the same; the image is
		// the run's, already here (never pulled, so no slow start); and the
		// docker client dies with the helper, so it can't start the
		// container after the next holder's sweep.
		name := s.containerPrefix() + "chown-" + strconv.Itoa(os.Getpid())
		cmd := shellCmd(ctx, d.cmd("run --rm --pull never --name "+shq(name)+" --user 0:0 --network none --entrypoint /bin/sh -v "+shq(s.workspace+":"+s.workspace)+" "+shq(image)+" -c "+shq(fix))+" >/dev/null 2>&1")
		cmd.SysProcAttr = childSysProcAttr()
		// Marked like the run's own processes, so the next holder reaps the
		// client even where the parent-death signal doesn't hold (sg).
		cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE="+s.workspace)
		ok = cmd.Run() == nil
		if ctx.Err() != nil {
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := removeContainer(rctx, *d, name)
			cancel()
			if err != nil {
				return fmt.Errorf("%w: %v", errRepairRunning, err)
			}
		}
	}
	if !ok {
		_, _ = runShell(ctx, "sudo -n "+fix+" 2>/dev/null")
	}
	return foreignFile(s.workspace)
}

// foreignFile reports the first file under dir not owned by the runner
// user and group (or not readable to check).
func foreignFile(dir string) error {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	stop := errors.New("stop")
	var found error
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			found = err
			return stop
		}
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // gone meanwhile
		}
		if err != nil {
			found = err
			return stop
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && (st.Uid != uid || st.Gid != gid) {
			found = fmt.Errorf("%s is owned by %d:%d", p, st.Uid, st.Gid)
			return stop
		}
		return nil
	})
	if found != nil {
		return found
	}
	if err != nil && !errors.Is(err, stop) && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// reclaimFiles finishes what an aborted root container run in this slot
// (client gone, sshd teardown, unlock) skipped: giving its files back. A
// slot whose files can't be given back is not handed out.
func (s *Session) reclaimFiles(slot int) error {
	marker := filepath.Join(s.slotDir, rootRunMarker)
	b, err := os.ReadFile(marker)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := probeDocker(ctx)
	if err != nil {
		// Its container (root, so invisible to reapWorkspace) may still be
		// running, and only docker can tell.
		return perr(protocol.CodeInternal, "workspace slot %d: can't check on the containers of an aborted root container run: %v", slot, err)
	}
	// Swept here again with a docker known to work (the sweep at acquire
	// skips a docker it can't reach).
	if left := s.sweepSlotContainers(ctx, d); left != "" {
		return perr(protocol.CodeInternal, "workspace slot %d: containers from an earlier run can't be removed: %s", slot, left)
	}
	dp := &d
	if err := s.giveBackFiles(ctx, dp, strings.TrimSpace(string(b))); err != nil {
		return perr(protocol.CodeInternal, "workspace slot %d holds files an aborted root container run left behind that couldn't be given back to %s (%v); fix their owner on the runner (sudo chown -R) or remove %s", slot, currentUser(), err, s.workspace)
	}
	os.Remove(marker)
	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "uid " + strconv.Itoa(os.Getuid())
}

// removeContainer force-removes a container and confirms it is gone.
func removeContainer(ctx context.Context, d dockerRunner, name string) error {
	for attempt := 0; attempt < 3; attempt++ {
		_, _ = runShell(ctx, d.cmd("rm -f "+shq(name))+" >/dev/null 2>&1")
		out, err := runShell(ctx, d.cmd("ps -aq --filter "+shq("name=^"+name+"$"))+" 2>/dev/null")
		if err == nil && strings.TrimSpace(string(out)) == "" {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("container %s could not be removed", name)
}

// containerPrefix names this slot's containers (used to reap leftovers).
func (s *Session) containerPrefix() string {
	return "sx-" + imageName(s.repoID) + "-" + filepath.Base(s.slotDir) + "-"
}

// removeContainers force-removes containers started by this session (client
// loss) or, with prefixOnly, all of this slot's (slot reacquisition by a
// new helper, after one died). It returns the slot's containers that still
// exist afterwards ("" if none, or if docker isn't usable by this user, in
// which case none could have been started).
func (s *Session) removeContainers(prefixOnly bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.containers.Range(func(k, v any) bool {
		if removeContainer(ctx, v.(dockerRunner), k.(string)) == nil {
			s.containers.Delete(k)
		}
		return true
	})
	// What container.docker runs started (their proxies are gone with the
	// run's processes, or with this helper).
	s.nested.Range(func(k, v any) bool {
		n := v.(nestedRun)
		n.proxy.stop()
		if removeLabelled(ctx, n.d, labelRun+"="+k.(string)) == "" {
			s.nested.Delete(k)
		}
		return true
	})
	if !prefixOnly {
		return ""
	}
	d, err := probeDocker(ctx)
	if err != nil {
		return ""
	}
	return s.sweepSlotContainers(ctx, d)
}

// sweepSlotContainers force-removes all of this slot's containers, and the
// containers, networks and volumes its container.docker runs started, and
// returns those that still exist.
func (s *Session) sweepSlotContainers(ctx context.Context, d dockerRunner) string {
	filter := "--filter " + shq("name=^"+s.containerPrefix())
	_, _ = runShell(ctx, d.cmd("ps -aq "+filter+" | xargs -r docker rm -f")+" >/dev/null 2>&1")
	out, err := runShell(ctx, d.cmd("ps -a --format '{{.Names}}' "+filter))
	if err != nil {
		return "(docker ps failed: " + strings.TrimSpace(string(out)) + ")"
	}
	left := strings.Fields(string(out))
	marker := filepath.Join(s.slotDir, dockerRunMarker)
	if b, err := os.ReadFile(marker); err == nil {
		// The proxies are gone with the runs' processes; their socket
		// directories may not be (a helper that died).
		for _, dir := range strings.Fields(string(b)) {
			if isProxyDir(dir) {
				os.RemoveAll(dir)
			}
		}
		if l := removeLabelled(ctx, d, labelSlot+"="+s.containerPrefix()); l != "" {
			left = append(left, l)
		} else {
			os.Remove(marker)
		}
	}
	return strings.Join(left, " ")
}

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// forbiddenOptions are the docker run flags container.options may not set.
// A lone "--" would end the options, making spotexec's own arguments the
// command's.
var forbiddenOptions = []string{"--", "--name", "--detach", "--entrypoint", "--user", "--rm", "--workdir", "-d", "-u", "-w"}

// forbiddenOption returns the first docker run flag opts sets that
// container.options may not set ("" if none). Flags are read as docker reads
// them (dockerFlags): clustered shorthands count (in "-itd" the d detaches,
// in "-iu" the u sets the user), and the values flags take don't
// (`-e NODE_OPTIONS=-d`, `--label -u`).
func forbiddenOption(opts []string) string {
	for _, f := range dockerFlags(opts) {
		if slices.Contains(forbiddenOptions, f) {
			return f
		}
	}
	return ""
}
