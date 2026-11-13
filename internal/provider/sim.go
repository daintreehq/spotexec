package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spotexec/spotexec/internal/cloudinit"
	"github.com/spotexec/spotexec/internal/config"
)

// Sim is a local, Docker-backed simulation of a Spot fleet. Each "instance"
// is a container that boots with the real cloud-init user-data (systemctl
// stubbed, the watchdog run in a 2s loop), runs sshd, and exits when the
// watchdog self-destructs, exactly where a real VM would terminate. Spot
// interruptions and rebalance notices are injected with Interrupt/Rebalance.
type Sim struct {
	r     *config.Runner
	owner string
}

// NewSim constructs the simulator provider. The default image is tagged
// with a hash of its build files, so changes rebuild it automatically.
func NewSim(r *config.Runner) (Provider, error) {
	if r.Image == "spotexec-sim" {
		h := sha256.Sum256([]byte(SimDockerfile + SimEntrypoint))
		rr := *r
		rr.Image = "spotexec-sim:" + hex.EncodeToString(h[:])[:10]
		r = &rr
	}
	return &Sim{r: r}, nil
}

// SimDockerfile builds the simulated instance image.
const SimDockerfile = `FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server ca-certificates util-linux procps sudo && \
    rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd && \
    useradd -m -s /bin/bash dev
COPY entrypoint.sh /entrypoint.sh
CMD ["/bin/bash", "/entrypoint.sh"]
`

// SimEntrypoint plays the role of the VM's init: boot delay, sshd, the
// cloud-init user-data, then the watchdog timer.
const SimEntrypoint = `#!/bin/bash
trap 'exit 0' TERM INT
sleep "${BOOT_DELAY:-0}"
/usr/sbin/sshd -e 2>/var/log/sshd.log
mkdir -p /tmp/fakebin
for b in systemctl systemd-run; do printf '#!/bin/sh\nexit 0\n' > /tmp/fakebin/$b; chmod +x /tmp/fakebin/$b; done
printf '%s' "$USER_DATA_B64" | base64 -d > /tmp/user-data.sh
# Like cloud-init, user-data runs while the watchdog timer is already ticking.
PATH=/tmp/fakebin:$PATH bash /tmp/user-data.sh >/var/log/spotexec-userdata.log 2>&1 &
mkdir -p /var/tmp/spotexec/sim && chmod 1777 /var/tmp/spotexec/sim  # marks the simulator (no IMDS)
while true; do
  sleep 2 & wait $!
  [ -x /usr/local/sbin/spotexec-watchdog ] && /usr/local/sbin/spotexec-watchdog
done
`

func (s *Sim) Runner() *config.Runner { return s.r }

func (s *Sim) ownerTag() (string, error) {
	if s.owner == "" {
		o, err := ownerID()
		if err != nil {
			return "", err
		}
		s.owner = o
	}
	return s.owner, nil
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// EnsureImage builds the simulator image if it is missing.
func (s *Sim) EnsureImage(ctx context.Context) error {
	if _, err := docker(ctx, "image", "inspect", s.r.Image); err == nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "spotexec-sim-image")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(SimDockerfile), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "entrypoint.sh"), []byte(SimEntrypoint), 0o755); err != nil {
		return err
	}
	_, err = docker(ctx, "build", "-q", "-t", s.r.Image, dir)
	return err
}

func (s *Sim) target(port int) Target {
	return Target{
		Host: "127.0.0.1", Port: port, User: s.r.User, IdentityFile: KeyPath(),
		Options: append(append([]string{}, s.r.SSHOptions...), "StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null"),
	}
}

// List returns the runner's live containers.
func (s *Sim) List(ctx context.Context) ([]Instance, error) {
	owner, err := s.ownerTag()
	if err != nil {
		return nil, err
	}
	out, err := docker(ctx, "ps", "--filter", "label=spotexec-runner="+s.r.Name, "--filter", "label=spotexec-owner="+owner,
		"--format", "{{.Names}}\t{{.Ports}}\t{{.Label \"spotexec-launched\"}}")
	if err != nil {
		return nil, err
	}
	var res []Instance
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 3 {
			continue
		}
		in := Instance{ID: f[0], State: StatePending, Type: "sim", Spot: true}
		if ms, err := strconv.ParseInt(f[2], 10, 64); err == nil {
			in.LaunchedAt = time.UnixMilli(ms)
		}
		if port := mappedPort(f[1]); port > 0 {
			in.State, in.Target = StateRunning, s.target(port)
		}
		res = append(res, in)
	}
	return res, nil
}

// mappedPort extracts the host port from "127.0.0.1:55012->22/tcp".
func mappedPort(ports string) int {
	for _, p := range strings.Split(ports, ",") {
		p = strings.TrimSpace(p)
		if !strings.HasSuffix(p, "->22/tcp") {
			continue
		}
		hp := strings.TrimSuffix(p, "->22/tcp")
		if i := strings.LastIndex(hp, ":"); i >= 0 {
			n, _ := strconv.Atoi(hp[i+1:])
			return n
		}
	}
	return 0
}

// Launch starts one simulated instance.
func (s *Sim) Launch(ctx context.Context, log Logger) (Instance, error) {
	if err := s.EnsureImage(ctx); err != nil {
		return Instance{}, err
	}
	owner, err := s.ownerTag()
	if err != nil {
		return Instance{}, err
	}
	pub, err := EnsureKey()
	if err != nil {
		return Instance{}, err
	}
	ud, err := cloudinit.Render(cloudinit.Options{
		User: s.r.User, PublicKey: pub, IdleTimeout: s.r.IdleTimeout.Duration, MaxLifetime: s.r.MaxLifetime.Duration,
		SelfDestruct: "kill -TERM 1", BootstrapScript: s.r.BootstrapScript, UserBootstrap: s.r.UserBootstrap,
	})
	if err != nil {
		return Instance{}, err
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	name := "sx-" + s.r.Name + "-" + hex.EncodeToString(b[:])
	_, err = docker(ctx, "run", "-d", "--rm", "--name", name,
		"--label", "spotexec-runner="+s.r.Name, "--label", "spotexec-owner="+owner,
		"--label", "spotexec-launched="+strconv.FormatInt(time.Now().UnixMilli(), 10),
		"-e", "BOOT_DELAY="+strconv.FormatFloat(s.r.BootDelay.Seconds(), 'f', 1, 64),
		"-e", "USER_DATA_B64="+base64.StdEncoding.EncodeToString([]byte(ud)),
		"-p", "127.0.0.1::22", s.r.Image)
	if err != nil {
		return Instance{}, err
	}
	if log != nil {
		log("launched simulated instance %s", name)
	}
	return Instance{ID: name, State: StatePending, Type: "sim", LaunchedAt: time.Now(), Spot: true}, nil
}

// WaitRunning waits for the container's SSH port mapping.
func (s *Sim) WaitRunning(ctx context.Context, id string, log Logger) (Instance, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		insts, err := s.List(ctx)
		if err != nil {
			return Instance{}, err
		}
		found := false
		for _, in := range insts {
			if in.ID == id {
				found = true
				if in.State == StateRunning {
					return in, nil
				}
			}
		}
		if !found && time.Since(time.Now()) > 0 {
			return Instance{}, fmt.Errorf("simulated instance %s is gone", id)
		}
		select {
		case <-ctx.Done():
			return Instance{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return Instance{}, fmt.Errorf("simulated instance %s did not start", id)
}

// Terminate removes one of our containers.
func (s *Sim) Terminate(ctx context.Context, id string) error {
	insts, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, in := range insts {
		if in.ID == id {
			_, err := docker(ctx, "rm", "-f", id)
			return err
		}
	}
	return nil
}

// Status reports the first running instance.
func (s *Sim) Status(ctx context.Context) (Info, error) {
	insts, err := s.List(ctx)
	if err != nil {
		return Info{State: StateUnknown, Detail: err.Error()}, err
	}
	for _, in := range insts {
		if in.State == StateRunning {
			info := Info{State: StateRunning, Target: in.Target, InstanceID: in.ID, InstanceType: "sim", LaunchedAt: in.LaunchedAt, Spot: true}
			if len(insts) > 1 {
				info.Detail = fmt.Sprintf("+%d more instance(s)", len(insts)-1)
			}
			return info, nil
		}
	}
	if len(insts) > 0 {
		return Info{State: StatePending, InstanceID: insts[0].ID}, nil
	}
	return Info{State: StateAbsent}, nil
}

// Up returns a running instance, launching one if there is none.
func (s *Sim) Up(ctx context.Context, log Logger) (Info, error) {
	insts, err := s.List(ctx)
	if err != nil {
		return Info{}, err
	}
	launched := false
	id := ""
	for _, in := range insts {
		id = in.ID
		break
	}
	if id == "" {
		in, err := s.Launch(ctx, log)
		if err != nil {
			return Info{}, err
		}
		id, launched = in.ID, true
	}
	in, err := s.WaitRunning(ctx, id, log)
	if err != nil {
		return Info{}, err
	}
	return Info{State: StateRunning, Target: in.Target, InstanceID: in.ID, InstanceType: "sim", LaunchedAt: in.LaunchedAt, Spot: true, Launched: launched}, nil
}

// Down removes every instance of the runner.
func (s *Sim) Down(ctx context.Context, log Logger) error {
	insts, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, in := range insts {
		if _, err := docker(ctx, "rm", "-f", in.ID); err != nil {
			return err
		}
		if log != nil {
			log("terminated %s", in.ID)
		}
	}
	return nil
}

func (s *Sim) Forget() {}

// Doctor checks the docker CLI and daemon.
func (s *Sim) Doctor(ctx context.Context) []Check {
	if _, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return []Check{{Name: "docker", Msg: err.Error()}}
	}
	return []Check{{Name: "docker", OK: true, Msg: "daemon reachable (image " + s.r.Image + ")"}}
}

// Interrupt simulates an EC2 Spot interruption: the notice appears in the
// instance's metadata (a file the helper checks before IMDS), and the
// instance is terminated `after` later, like AWS's two-minute warning.
func (s *Sim) Interrupt(ctx context.Context, id string, after time.Duration) error {
	at := time.Now().Add(after).UTC().Format(time.RFC3339)
	if _, err := docker(ctx, "exec", id, "sh", "-c",
		`mkdir -p /var/tmp/spotexec/sim && printf '{"action":"terminate","time":"`+at+`"}' > /var/tmp/spotexec/sim/instance-action`); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(after):
	}
	_, err := docker(context.Background(), "rm", "-f", id)
	return err
}

// Rebalance simulates an EC2 rebalance recommendation.
func (s *Sim) Rebalance(ctx context.Context, id string) error {
	_, err := docker(ctx, "exec", id, "sh", "-c",
		`mkdir -p /var/tmp/spotexec/sim && printf '{"noticeTime":"now"}' > /var/tmp/spotexec/sim/rebalance`)
	return err
}

// ErrNoInstance is returned when an instance id is not part of the fleet.
var ErrNoInstance = errors.New("no such instance")
