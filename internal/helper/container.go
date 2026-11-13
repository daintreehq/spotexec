package helper

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/fsid"
	"github.com/spotexec/spotexec/internal/protocol"
)

// Project containers: commands run via `docker run` with the slot workspace
// bind-mounted at the same absolute path, so path rewriting, snapshots and
// write-back work exactly as on the host. Commands run as the runner user by
// default (files keep the right owner), with host networking (per-slot port
// ranges still apply).

var imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,255}$`)

// dockerPrefix returns how to invoke docker: directly, or via passwordless
// sudo when the runner user is not in the docker group.
func dockerPrefix() (string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", errors.New("docker is not installed")
	}
	if exec.Command("docker", "info").Run() == nil {
		return "docker", nil
	}
	if exec.Command("sudo", "-n", "docker", "info").Run() == nil {
		return "sudo -n docker", nil
	}
	return "", errors.New("docker is installed but not usable by this user (add it to the docker group, or allow passwordless sudo)")
}

// ensureDocker installs Docker on Debian/Ubuntu runners that lack it
// (ephemeral VMs have passwordless sudo). Output streams to the client.
func (s *Session) ensureDocker() (string, bool, error) {
	if p, err := dockerPrefix(); err == nil {
		return p, false, nil
	} else if _, lerr := exec.LookPath("docker"); lerr == nil {
		return "", false, err
	}
	if exec.Command("sudo", "-n", "true").Run() != nil {
		return "", false, errors.New("docker is not installed and passwordless sudo is unavailable; install docker on this runner")
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return "", false, errors.New("docker is not installed (automatic install supports Debian/Ubuntu runners only)")
	}
	_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "docker_install", Message: "installing docker on the runner (preinstall it in bootstrap_script to skip this)"})
	script := `set -e
export DEBIAN_FRONTEND=noninteractive
sudo -n apt-get update -o DPkg::Lock::Timeout=300 -qq
sudo -n apt-get install -y -qq -o DPkg::Lock::Timeout=300 docker.io
sudo -n systemctl enable --now docker >/dev/null 2>&1 || sudo -n service docker start || true`
	pr := s.runProc(script, s.layout.Root, nil, 10*time.Minute, 5*time.Second, false)
	if pr.code != 0 {
		return "", false, fmt.Errorf("installing docker failed (exit %d)", pr.code)
	}
	p, err := dockerPrefix()
	return p, true, err
}

func (s *Session) image(m protocol.Image) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before image")
	}
	start := time.Now()
	docker, installed, err := s.ensureDocker()
	if err != nil {
		return perr(protocol.CodeImageFailed, "%v", err)
	}
	res := protocol.ImageResult{InstalledDocker: installed}
	timeout := time.Duration(m.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	exists := func(ref string) bool {
		return exec.Command("sh", "-c", docker+" image inspect "+shq(ref)+" >/dev/null 2>&1").Run() == nil
	}
	run := func(script string) error {
		pr := s.runProc(script, s.workspace, nil, timeout, 5*time.Second, false)
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
		if !exists(m.Ref) {
			_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_pull", Message: m.Ref})
			if err := run(docker + " pull " + shq(m.Ref)); err != nil {
				return perr(protocol.CodeImageFailed, "docker pull %s: %v", m.Ref, err)
			}
			res.Pulled = true
		}
		res.Image = m.Ref
	case m.Dockerfile != "":
		if !regexp.MustCompile(`^[a-f0-9]{8,64}$`).MatchString(m.Tag) {
			return perr(protocol.CodeBadRequest, "invalid image tag")
		}
		df, err := fsid.SafeJoin(s.workspace, m.Dockerfile)
		if err != nil {
			return perr(protocol.CodeBadRequest, "%v", err)
		}
		ctxDir := filepath.Dir(df)
		if m.Context != "" {
			if ctxDir, err = fsid.SafeJoin(s.workspace, m.Context); err != nil {
				return perr(protocol.CodeBadRequest, "%v", err)
			}
		}
		local := "spotexec-local/" + s.repoID + ":" + m.Tag
		res.Image = local
		if exists(local) {
			break
		}
		// One build per tag per runner; other slots wait for it.
		lk, err := s.waitLock(filepath.Join(s.layout.Root, "images", strings.ReplaceAll(local, "/", "_")+".lock"))
		if err != nil {
			return err
		}
		defer lk.Unlock()
		if exists(local) {
			break
		}
		cfgDir := ""
		if m.Registry != nil {
			if cfgDir, err = dockerLogin(docker, m.Registry); err != nil {
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_registry_error", Message: err.Error()})
			} else {
				defer func() {
					if !res.Pushing {
						os.RemoveAll(cfgDir)
					}
				}()
				remote := m.Registry.Repo + ":" + m.Tag
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_pull", Message: remote})
				pull := "DOCKER_CONFIG=" + shq(cfgDir) + " " + docker + " pull -q " + shq(remote) + " >/dev/null 2>&1 && " + docker + " tag " + shq(remote) + " " + shq(local)
				if exec.Command("sh", "-c", pull).Run() == nil {
					res.Pulled = true
					break
				}
			}
		}
		_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "image_build", Message: m.Dockerfile})
		if err := run(docker + " build -f " + shq(df) + " -t " + shq(local) + " " + shq(ctxDir)); err != nil {
			return perr(protocol.CodeImageFailed, "docker build %s: %v", m.Dockerfile, err)
		}
		res.Built = true
		if m.Registry != nil && cfgDir != "" {
			remote := m.Registry.Repo + ":" + m.Tag
			if err := startImagePush(s.layout.Root, docker, local, remote, cfgDir); err == nil {
				res.Pushing = true
			}
		}
	default:
		return perr(protocol.CodeBadRequest, "image: nothing to do")
	}
	res.MS = time.Since(start).Milliseconds()
	return s.c.Send(protocol.TypeImageResult, res)
}

// waitLock takes an exclusive lock, polling while the client is alive.
func (s *Session) waitLock(path string) (*flock, error) {
	for {
		l, ok, err := tryLock(path)
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		if s.clientGone() {
			return nil, perr(protocol.CodeImageFailed, "client went away while waiting for an image build")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dockerLogin logs into the registry with a private DOCKER_CONFIG dir, so
// the short-lived token never lands in the user's default docker config.
func dockerLogin(docker string, r *protocol.RegistryAuth) (string, error) {
	dir, err := os.MkdirTemp("", "spotexec-docker-")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("sh", "-c", "DOCKER_CONFIG="+shq(dir)+" "+docker+" login --username "+shq(r.User)+" --password-stdin "+shq(r.Server))
	cmd.Stdin = strings.NewReader(r.Password)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("registry login: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

// startImagePush pushes a freshly built image in a detached process (the
// job doesn't wait). It holds the lifecycle lock so the VM stays up.
func startImagePush(root, docker, local, remote, cfgDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, _ := os.OpenFile(filepath.Join(root, "image-push.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	cmd := exec.Command(self, "image-push", "--work-root", root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(stdin, "%s\n%s\n%s\n%s\n", docker, local, remote, cfgDir)
	stdin.Close()
	if logf != nil {
		logf.Close()
	}
	return cmd.Process.Release()
}

// ImagePush is the detached push (spotexec-helper image-push; arguments on
// stdin).
func ImagePush(root string) error {
	sc := bufio.NewScanner(os.Stdin)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) < 4 {
		return errors.New("image-push: expected docker, local, remote, config dir on stdin")
	}
	docker, local, remote, cfgDir := lines[0], lines[1], lines[2], lines[3]
	defer os.RemoveAll(cfgDir)
	if _, err := os.Stat(LifecycleLock); err == nil {
		if l, err := lockShared(LifecycleLock, 10*time.Second); err == nil {
			defer l.Unlock()
		}
	}
	script := docker + " tag " + shq(local) + " " + shq(remote) + " && DOCKER_CONFIG=" + shq(cfgDir) + " timeout 1200 " + docker + " push -q " + shq(remote)
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	fmt.Printf("%s push %s: %s\n", time.Now().Format(time.RFC3339), remote, strings.TrimSpace(string(out)))
	return err
}

// containerScript wraps a command so it runs inside run.Image. The command
// is written to a file (no quoting games), the environment to an env-file,
// and the container gets a deterministic name so it can be force-removed
// after the run (cancellation, timeouts, a dead client).
func (s *Session) containerScript(run *protocol.ContainerRun, script, dir string, env map[string]string) (string, string, func(), error) {
	docker, err := dockerPrefix()
	if err != nil {
		return "", "", nil, err
	}
	if !imageRefRe.MatchString(run.Image) {
		return "", "", nil, fmt.Errorf("invalid image reference %q", run.Image)
	}
	runDir := filepath.Join(s.slotDir, "run")
	home := filepath.Join(s.slotDir, "home")
	for _, d := range []string{runDir, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", nil, err
		}
	}
	id := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "sx-" + filepath.Base(s.slotDir) + "-" + id
	scriptPath := filepath.Join(runDir, id+".sh")
	envPath := filepath.Join(runDir, id+".env")
	if err := os.WriteFile(scriptPath, []byte(script+"\n"), 0o644); err != nil {
		return "", "", nil, err
	}
	var envb strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.ContainsAny(env[k], "\n\r") || strings.ContainsAny(k, "=\n ") {
			continue // env-files are line-based
		}
		envb.WriteString(k + "=" + env[k] + "\n")
	}
	envb.WriteString("HOME=/home/spotexec\n")
	if err := os.WriteFile(envPath, []byte(envb.String()), 0o600); err != nil {
		return "", "", nil, err
	}
	args := []string{
		"run", "--rm", "--init", "--name", name, "--network", "host",
		"-v", s.workspace + ":" + s.workspace,
		"-v", home + ":/home/spotexec",
		"-v", scriptPath + ":/tmp/spotexec-run.sh:ro",
		"--env-file", envPath, "-w", dir,
	}
	if !run.Root {
		args = append(args, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()))
	}
	args = append(args, run.Options...)
	args = append(args, run.Image, "/bin/sh", "-c",
		`if command -v bash >/dev/null 2>&1; then exec bash /tmp/spotexec-run.sh; else exec sh /tmp/spotexec-run.sh; fi`)
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	host := "exec " + docker + " " + strings.Join(q, " ")
	cleanup := func() {
		_ = exec.Command("sh", "-c", docker+" rm -f "+shq(name)+" >/dev/null 2>&1").Run()
		os.Remove(scriptPath)
		os.Remove(envPath)
	}
	return host, name, cleanup, nil
}

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var _ = io.Discard
