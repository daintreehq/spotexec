// Package provider abstracts where runners come from: a static SSH machine,
// or an ephemeral AWS Spot instance provisioned on demand.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// Target is how to reach a runner over SSH.
type Target struct {
	Host           string   `json:"host"` // ssh destination (alias, host, or user@host)
	User           string   `json:"user,omitempty"`
	Port           int      `json:"port,omitempty"`
	IdentityFile   string   `json:"identity_file,omitempty"`
	KnownHostsFile string   `json:"known_hosts_file,omitempty"`
	Options        []string `json:"options,omitempty"` // extra -o options
}

// String is a display form.
func (t Target) String() string {
	if t.User != "" && !strings.Contains(t.Host, "@") {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// State of a runner.
type State string

const (
	StateRunning  State = "running"
	StatePending  State = "pending"
	StateStopping State = "stopping"
	StateAbsent   State = "absent"
	StateUnknown  State = "unknown"
)

// Info describes a runner instance.
type Info struct {
	State        State     `json:"state"`
	Target       Target    `json:"target"`
	InstanceID   string    `json:"instance_id,omitempty"`
	InstanceType string    `json:"instance_type,omitempty"`
	LaunchedAt   time.Time `json:"launched_at,omitempty"`
	Region       string    `json:"region,omitempty"`
	Spot         bool      `json:"spot,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	Launched     bool      `json:"-"` // Up launched this instance (vs. found one)
}

// Logger receives human-readable progress lines.
type Logger func(format string, args ...any)

// Provider manages one configured runner.
type Provider interface {
	Runner() *config.Runner
	// Status reports current state without changing anything.
	Status(ctx context.Context) (Info, error)
	// Up makes the runner reachable (provisioning if needed) and returns its
	// target. It is idempotent and safe to call concurrently from several
	// local processes. It does not wait for SSH readiness.
	Up(ctx context.Context, log Logger) (Info, error)
	// Down terminates an ephemeral runner (no-op for static runners).
	Down(ctx context.Context, log Logger) error
	// Doctor checks credentials/config; returns human-readable findings.
	Doctor(ctx context.Context) []Check
	// Forget drops cached connection state after SSH failures (e.g. the
	// instance was reclaimed) so the next Up re-discovers.
	Forget()
}

// Instance is one VM of a fleet.
type Instance struct {
	ID         string    `json:"id"`
	State      State     `json:"state"` // pending | running
	Target     Target    `json:"target"`
	Type       string    `json:"type,omitempty"`
	LaunchedAt time.Time `json:"launched_at"`
	Spot       bool      `json:"spot,omitempty"`
}

// Fleet is implemented by ephemeral, autoscaled runner types: a pool of 0 to
// max_instances identical VMs. Scheduling across the fleet is done by the
// client (see docs/POOL.md); instances terminate themselves when idle.
type Fleet interface {
	Provider
	// List returns live (pending or running) instances of this runner.
	List(ctx context.Context) ([]Instance, error)
	// Launch starts one more instance; it may still be pending.
	Launch(ctx context.Context, log Logger) (Instance, error)
	// WaitRunning waits until an instance is running and reachable by IP.
	WaitRunning(ctx context.Context, id string, log Logger) (Instance, error)
	// Terminate ends one specific instance (only if it is ours).
	Terminate(ctx context.Context, instanceID string) error
}

// ErrLaunchAmbiguous wraps a launch failure after which an instance may
// nevertheless exist (e.g. the RunInstances response was lost).
var ErrLaunchAmbiguous = errors.New("launch outcome unknown")

// Terminator is implemented by providers that can end one specific instance.
type Terminator interface {
	Terminate(ctx context.Context, instanceID string) error
}

// Check is a doctor finding.
type Check struct {
	Name string
	OK   bool
	Msg  string
}

// New constructs the provider for a runner.
func New(r *config.Runner) (Provider, error) {
	switch r.Type {
	case config.TypeSSH:
		return &SSH{r: r}, nil
	case config.TypeAWSSpot:
		return NewAWSSpot(r)
	case config.TypeSim:
		return NewSim(r)
	}
	return nil, fmt.Errorf("runner %s: unsupported type %q", r.Name, r.Type)
}

// KeyPath is the spotexec-managed SSH key injected into ephemeral runners.
func KeyPath() string { return filepath.Join(config.Dir(), "keys", "id_ed25519") }

// EnsureKey creates the managed key pair if missing and returns the public key.
func EnsureKey() (string, error) {
	p := KeyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	// Concurrent first launches must not race two ssh-keygens.
	lf, err := os.OpenFile(filepath.Join(filepath.Dir(p), ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	if _, err := os.Stat(p); err != nil {
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "spotexec", "-f", p).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	b, err := os.ReadFile(p + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// RunnerStateDir is where a runner keeps local state (instance record,
// known_hosts, provisioning lock).
func RunnerStateDir(name string) string {
	return filepath.Join(config.StateDir(), "runners", name)
}
