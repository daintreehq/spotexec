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
	Region     string    `json:"region,omitempty"` // cloud region (aws-spot)
	Zone       string    `json:"zone,omitempty"`   // availability zone, when known
	// Placement says why a launch went where it did (set by Launch only).
	Placement *Placement `json:"placement,omitempty"`
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

// RegionLister is implemented by fleets spread over regions that can list
// the regions they reach even when another can't be listed. It is for
// showing the fleet (`spotexec status`); the pool uses List, which fails
// as a whole so that it never believes an unlisted region's VMs gone.
type RegionLister interface {
	// ListReachable returns the live instances of every region it could
	// list and, for each region it couldn't, why. It waits on no region
	// longer than a short timeout. err is for failures that leave nothing
	// listed at all.
	ListReachable(ctx context.Context) (insts []Instance, unlisted map[string]error, err error)
}

// Matcher is implemented by fleets whose VMs can stop satisfying the
// runner's requirements when its config changes (a raised min_memory_gib,
// a type dropped from instance_types): such a VM should take no new runs
// and idle out, rather than keep serving under the old requirements.
type Matcher interface {
	// Mismatches judges each instance against the runner's current
	// requirements: "" when it still satisfies them, otherwise why not. An
	// instance it can't judge (unknown type, a failed lookup) is left out,
	// so a lookup failure never retires VMs.
	Mismatches(ctx context.Context, insts []Instance) map[string]string
}

// ErrLaunchAmbiguous wraps a launch failure after which an instance may
// nevertheless exist (e.g. the RunInstances response was lost).
var ErrLaunchAmbiguous = errors.New("launch outcome unknown")

// Terminator is implemented by providers that can end one specific instance.
type Terminator interface {
	Terminate(ctx context.Context, instanceID string) error
}

// Placed is implemented by providers whose instances live in different
// places (multi-region fleets). For returns the provider that owns one
// instance, for location-bound services such as the dependency cache and
// the image registry, which must be in the instance's own region.
type Placed interface {
	For(ctx context.Context, instanceID string) (Provider, error)
}

// ForInstance returns the provider serving instanceID: p itself, unless p
// is Placed. When the owner can't be determined it returns p, which (being
// Placed) offers no location-bound services: the run goes without them.
func ForInstance(ctx context.Context, p Provider, instanceID string) Provider {
	pl, ok := p.(Placed)
	if !ok || instanceID == "" {
		return p
	}
	if q, err := pl.For(ctx, instanceID); err == nil && q != nil {
		return q
	}
	return p
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
		if r.MultiRegion() {
			return NewAWSGlobal(r)
		}
		if heldElsewhere(r) {
			return newLegacyAWSGlobal(r)
		}
		forgetDiscovery(r)
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
