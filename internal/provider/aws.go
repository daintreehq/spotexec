package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/cloudinit"
	"github.com/daintreehq/spotexec/internal/config"
)

const (
	tagRunner  = "spotexec-runner"
	tagBake    = "spotexec-bake-runner"
	tagOwner   = "spotexec-owner"
	sgBaseName = "spotexec-ssh"
)

// AWSSpot manages a fleet of one-time EC2 Spot instances for one runner. The
// cloud API is the source of truth: instances are found by their runner and
// owner tags. Scheduling across the fleet happens in the client (see
// docs/POOL.md); each instance terminates itself when idle.
type AWSSpot struct {
	r   *config.Runner
	api *awsClient
	dir string
	// ownerMu guards owner, filled lazily by concurrent callers.
	ownerMu sync.Mutex
	owner   string
	// placed: one region of a multi-region runner, which uses default VPCs
	// only (messages mustn't suggest subnet_id).
	placed bool
	// launching: for a region of a multi-region runner, records in the
	// runner's placement state that a launch is being sent here (nil: none).
	launching func() error
}

// NewAWSSpot constructs the provider.
func NewAWSSpot(r *config.Runner) (Provider, error) {
	if err := checkAWSRunner(r); err != nil {
		return nil, err
	}
	return newAWSSpot(r, &awsCredResolver{profile: r.Profile}), nil
}

func checkAWSRunner(r *config.Runner) error {
	if len(r.InstanceTypes) == 0 && r.VCPUs <= 0 {
		return fmt.Errorf("runner %s: instance_types is empty (or set vcpus)", r.Name)
	}
	if r.Arch != "amd64" && r.Arch != "arm64" {
		return fmt.Errorf("runner %s: arch must be amd64 or arm64 (got %q)", r.Name, r.Arch)
	}
	return nil
}

// newAWSSpot builds a provider for r.Region; creds may be shared between
// the regional providers of one multi-region runner.
func newAWSSpot(r *config.Runner, creds *awsCredResolver) *AWSSpot {
	return &AWSSpot{
		r:   r,
		dir: RunnerStateDir(r.Name),
		api: &awsClient{
			region: r.Region,
			creds:  creds,
			http:   &http.Client{Timeout: 60 * time.Second},
			// ECR's API host is api.ecr.<region>, not ecr.<region>.
			endpoints: map[string]string{"ecr": "https://api.ecr." + r.Region + ".amazonaws.com/"},
		},
	}
}

func (a *AWSSpot) Runner() *config.Runner { return a.r }

// ownerID returns this machine's persistent spotexec owner UUID.
func ownerID() (string, error) {
	p := filepath.Join(config.Dir(), "owner-id")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	h := hex.EncodeToString(u[:])
	id := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	// O_EXCL so two first-time processes agree on one ID.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			for i := 0; i < 50; i++ { // the creator may not have written it yet
				b, rerr := os.ReadFile(p)
				if rerr == nil && strings.TrimSpace(string(b)) != "" {
					return strings.TrimSpace(string(b)), nil
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		return "", err
	}
	return id, nil
}

func (a *AWSSpot) ownerTag() (string, error) {
	a.ownerMu.Lock()
	defer a.ownerMu.Unlock()
	if a.owner == "" {
		o, err := ownerID()
		if err != nil {
			return "", err
		}
		a.owner = o
	}
	return a.owner, nil
}

func (a *AWSSpot) knownHosts(id string) string {
	return filepath.Join(a.dir, "known_hosts-"+id)
}

func (a *AWSSpot) info(inst ec2Instance) Info {
	st := mapEC2State(inst.State)
	info := Info{
		State: st, InstanceID: inst.InstanceID, InstanceType: inst.InstanceType,
		LaunchedAt: inst.launched(), Region: a.r.Region, Spot: inst.Lifecycle == "spot",
		Detail: inst.State,
	}
	if inst.IPAddress != "" {
		info.Target = a.target(inst.InstanceID, inst.IPAddress)
	}
	return info
}

func (a *AWSSpot) target(id, ip string) Target {
	return Target{
		Host: ip, User: a.r.User, IdentityFile: KeyPath(), KnownHostsFile: a.knownHosts(id),
		Options: append(append([]string{}, a.r.SSHOptions...), "StrictHostKeyChecking=accept-new"),
	}
}

func mapEC2State(s string) State {
	switch s {
	case "pending":
		return StatePending
	case "running":
		return StateRunning
	case "stopping", "shutting-down", "stopped":
		return StateStopping
	case "terminated", "":
		return StateAbsent
	}
	return StateUnknown
}

// describe lists this owner's instances for this runner in the given states.
func (a *AWSSpot) describe(ctx context.Context, states ...string) ([]ec2Instance, error) {
	owner, err := a.ownerTag()
	if err != nil {
		return nil, err
	}
	var out []ec2Instance
	token := ""
	for {
		p := url.Values{}
		p.Set("Filter.1.Name", "tag:"+tagRunner)
		p.Set("Filter.1.Value.1", a.r.Name)
		p.Set("Filter.2.Name", "tag:"+tagOwner)
		p.Set("Filter.2.Value.1", owner)
		p.Set("Filter.3.Name", "instance-state-name")
		for i, s := range states {
			p.Set("Filter.3.Value."+strconv.Itoa(i+1), s)
		}
		// Paginated calls are throttled in a far more generous bucket.
		p.Set("MaxResults", "100")
		if token != "" {
			p.Set("NextToken", token)
		}
		b, err := a.api.ec2(ctx, "DescribeInstances", p)
		if err != nil {
			return nil, err
		}
		insts, next, err := parseDescribeInstances(b)
		if err != nil {
			return nil, err
		}
		for _, i := range insts {
			// Belt and braces: never act on anything missing our tags.
			if i.tag(tagRunner) == a.r.Name && i.tag(tagOwner) == owner {
				out = append(out, i)
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	// Prefer running over pending, newest first.
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := stateRank(out[i].State), stateRank(out[j].State)
		if ri != rj {
			return ri < rj
		}
		return out[i].launched().After(out[j].launched())
	})
	return out, nil
}

func stateRank(s string) int {
	switch s {
	case "running":
		return 0
	case "pending":
		return 1
	}
	return 2
}

// Status reports the current instance state from the EC2 API.
func (a *AWSSpot) Status(ctx context.Context) (Info, error) {
	insts, err := a.describe(ctx, "pending", "running", "stopping", "stopped", "shutting-down")
	if err != nil {
		return Info{State: StateUnknown, Region: a.r.Region, Detail: err.Error()}, err
	}
	if len(insts) == 0 {
		return Info{State: StateAbsent, Region: a.r.Region}, nil
	}
	info := a.info(insts[0])
	if len(insts) > 1 {
		info.Detail = fmt.Sprintf("%s (+%d more instance(s))", info.Detail, len(insts)-1)
	}
	return info, nil
}

// Forget is a no-op: the AWS provider keeps no cached instance state.
func (a *AWSSpot) Forget() {}

// List returns this runner's pending and running instances.
func (a *AWSSpot) List(ctx context.Context) ([]Instance, error) {
	insts, err := a.describe(ctx, "pending", "running")
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(insts))
	for _, i := range insts {
		out = append(out, a.instance(i))
	}
	return out, nil
}

func (a *AWSSpot) instance(i ec2Instance) Instance {
	in := Instance{ID: i.InstanceID, State: mapEC2State(i.State), Type: i.InstanceType, LaunchedAt: i.launched(), Spot: i.Lifecycle == "spot", Region: a.r.Region, Zone: i.Zone}
	if i.IPAddress != "" {
		in.Target = a.target(i.InstanceID, i.IPAddress)
	}
	return in
}

// Launch starts one new instance (it may still be pending). Each call uses
// fresh ClientTokens, reused only for retries inside the call.
func (a *AWSSpot) Launch(ctx context.Context, log Logger) (Instance, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	// Shares the provisioning lock with other launches and with Down, so
	// `spotexec down` cannot finish while a launch is about to create an
	// instance.
	unlock, err := a.lockLaunch(ctx)
	if err != nil {
		return Instance{}, err
	}
	defer unlock()
	return a.ranker().launch(ctx, log)
}

// ranker places this region's launches: the cheapest current Spot price
// among its instance types and availability zones first, sharing prices
// and cooldowns with `spotexec prices`.
func (a *AWSSpot) ranker() *AWSGlobal {
	return &AWSGlobal{r: a.r, creds: a.api.creds, dir: a.dir, regional: map[string]*AWSSpot{a.r.Region: a}, single: true}
}

// WaitRunning waits until the instance is running with a public IP.
func (a *AWSSpot) WaitRunning(ctx context.Context, id string, log Logger) (Instance, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	info, err := a.waitRunning(ctx, ec2Instance{InstanceID: id, State: "pending"}, log)
	if err != nil {
		return Instance{}, err
	}
	return Instance{ID: info.InstanceID, State: info.State, Target: info.Target, Type: info.InstanceType, LaunchedAt: info.LaunchedAt, Spot: info.Spot, Region: a.r.Region}, nil
}

// Up returns a running instance of the fleet, launching one if there is
// none (used by `spotexec up`).
func (a *AWSSpot) Up(ctx context.Context, log Logger) (Info, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	unlock, err := a.lockUp(ctx, log)
	if err != nil {
		return Info{}, err
	}
	defer unlock()
	insts, err := a.describe(ctx, "pending", "running")
	if err != nil {
		return Info{}, err
	}
	if len(insts) > 0 {
		return a.waitRunning(ctx, insts[0], log)
	}
	// Stopped leftovers (e.g. an on-demand fallback stopped from the console)
	// still bill for their volume: terminate them.
	if stopped, err := a.describe(ctx, "stopped"); err == nil && len(stopped) > 0 {
		ids := make([]string, len(stopped))
		for i, s := range stopped {
			ids[i] = s.InstanceID
		}
		done, err := a.terminate(ctx, ids)
		if len(done) > 0 {
			log("terminated %d stopped instance(s) left over for %s", len(done), a.r.Name)
		}
		if err != nil {
			log("stopped leftovers for %s: %v", a.r.Name, err)
		}
	}
	in, err := a.ranker().launch(ctx, log)
	if err != nil {
		return Info{}, err
	}
	info, err := a.waitRunning(ctx, ec2Instance{InstanceID: in.ID, State: string(in.State), InstanceType: in.Type}, log)
	info.Launched = true
	if info.InstanceID == "" {
		info.InstanceID = in.ID
	}
	return info, err
}

// bootPollInterval spaces waitRunning's status reads.
var bootPollInterval = 3 * time.Second

// waitRunning polls until the instance is running with a public IP.
//
// A status read that fails transiently (throttling, a 5xx, the network)
// says nothing about the instance, so the wait rides it out until the boot
// deadline instead of giving up a VM that may be booting fine: a burst of
// launches can throttle DescribeInstances for longer than one Query call's
// retries. Only a fresh read decides the instance is gone or unusable.
func (a *AWSSpot) waitRunning(ctx context.Context, inst ec2Instance, log Logger) (Info, error) {
	timeout := a.r.BootTimeout.Duration
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	logged := ""
	var runningNoIP time.Time
	var readErr error // the latest failure of an ongoing status-read outage
	delay := bootPollInterval
	fresh := true // inst is an observation not yet acted on
	for {
		if fresh {
			fresh = false
			switch inst.State {
			case "running":
				if inst.IPAddress != "" {
					a.pruneKnownHosts()
					return a.info(inst), nil
				}
				if runningNoIP.IsZero() {
					runningNoIP = time.Now()
				} else if time.Since(runningNoIP) > 45*time.Second {
					_ = a.Terminate(ctx, inst.InstanceID)
					return Info{}, fmt.Errorf("instance %s is running without a public IPv4 address (terminated it); use a subnet that can assign public IPs (subnet_id)", inst.InstanceID)
				}
			case "shutting-down", "terminated", "stopping", "stopped":
				return Info{}, fmt.Errorf("instance %s entered state %q while booting (spot capacity reclaimed?)", inst.InstanceID, inst.State)
			}
			if logged != inst.State {
				if inst.InstanceType != "" {
					log("waiting for %s (%s, %s)", inst.InstanceID, inst.InstanceType, inst.State)
				} else {
					log("waiting for %s (%s)", inst.InstanceID, inst.State)
				}
				logged = inst.State
			}
		}
		select {
		case <-bctx.Done():
			if ctx.Err() != nil {
				return Info{}, ctx.Err()
			}
			if readErr != nil {
				return Info{}, fmt.Errorf("instance %s not running with a public IP after %s: its status could not be read: %w", inst.InstanceID, timeout, readErr)
			}
			return Info{}, fmt.Errorf("instance %s not running with a public IP after %s (state %s)", inst.InstanceID, timeout, inst.State)
		case <-time.After(delay):
		}
		p := url.Values{}
		p.Set("InstanceId.1", inst.InstanceID)
		var tried error
		b, err := a.api.ec2(withQueryErrSink(bctx, &tried), "DescribeInstances", p)
		if err != nil {
			if ctx.Err() != nil {
				return Info{}, ctx.Err()
			}
			if bctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
				// The boot deadline cut the read short: report what its
				// attempts were failing with.
				switch {
				case tried != nil:
					readErr = tried
				case readErr == nil:
					readErr = errors.New("DescribeInstances did not answer")
				}
				continue
			}
			if awsCode(err) == "InvalidInstanceID.NotFound" {
				continue // eventual consistency right after launch
			}
			var ce *CredentialError
			if errors.As(err, &ce) || classifyAWSError(err) != errRetryable {
				return Info{}, err
			}
			if readErr == nil {
				log("reading %s's status failed (%v); retrying until its boot deadline", inst.InstanceID, err)
			} else {
				delay = min(2*delay, 4*bootPollInterval) // Query backs off within a call; this eases off between them
			}
			readErr = err
			// How long it has really lacked an IP is unknown until reads
			// work again.
			runningNoIP = time.Time{}
			continue
		}
		got, _, err := parseDescribeInstances(b)
		if err != nil {
			return Info{}, err
		}
		if len(got) > 0 {
			inst, fresh = got[0], true
			readErr, delay = nil, bootPollInterval
		}
	}
}

// launchOrUnbaked launches a fleet instance as spec says, booting the base
// image instead when the bake it picked was deregistered (`image clear` or
// pruning on another machine) between picking it and launching.
func (a *AWSSpot) launchOrUnbaked(ctx context.Context, log Logger, spec launchSpec) (ec2Instance, error) {
	inst, err := a.launchWith(ctx, log, spec)
	if err != nil && !spec.bake && !spec.noBake && (a.r.AMI == "" || a.r.AMI == "auto") && strings.HasPrefix(awsCode(err), "InvalidAMIID") {
		log("baked image is gone (%s); launching from the current Ubuntu image", awsCode(err))
		spec.noBake = true
		inst, err = a.launchWith(ctx, log, spec)
	}
	if err == nil && !spec.bake {
		// Under the provisioning lock still, so a Down after this launch
		// finds it even before tag-filtered listings show it.
		a.noteLaunched(inst.InstanceID)
	}
	return inst, err
}

// launchSpec varies a launch: normal fleet instances (spot, from the newest
// matching baked AMI when there is one) or an image-bake builder.
type launchSpec struct {
	bake    bool
	prepull []string
	noBake  bool // boot the base image even if a bake matches
	// Multi-region placement: exactly this Spot type, in this availability
	// zone (through the default VPC's subnet there), with no on-demand
	// fallback (the coordinator decides that).
	itype    string
	zone     string
	onDemand bool // on-demand only, any configured type (multi-region fallback)
	// types to try in order without an itype (default: the configured
	// ones, or for vcpus every eligible type).
	types []string
	// beforeSend runs once preparation succeeded, just before the first
	// RunInstances; an error stops the launch with nothing sent. It runs
	// again once the launch has stopped sending, whatever came of it.
	beforeSend func() error
}

// regionError marks a failure to prepare a launch in one region (no
// default VPC, AMI or security group trouble): another region may work.
type regionError struct{ err error }

func (e *regionError) Error() string { return e.err.Error() }
func (e *regionError) Unwrap() error { return e.err }

// asRegionError wraps err unless it is a credential failure or a
// cancellation, which no other region can fix.
func asRegionError(ctx context.Context, err error) error {
	var ce *CredentialError
	if err == nil || ctx.Err() != nil || errors.As(err, &ce) {
		return err
	}
	return &regionError{err}
}

func (a *AWSSpot) launchWith(ctx context.Context, log Logger, spec launchSpec) (ec2Instance, error) {
	owner, err := a.ownerTag()
	if err != nil {
		return ec2Instance{}, err
	}
	pub, err := EnsureKey()
	if err != nil {
		return ec2Instance{}, fmt.Errorf("spotexec SSH key: %w", err)
	}
	opts := cloudinit.Options{
		User: a.r.User, PublicKey: pub, IdleTimeout: a.r.IdleTimeout.Duration,
		MaxLifetime: a.r.MaxLifetime.Duration, BootstrapScript: a.r.BootstrapScript,
		UserBootstrap: a.r.UserBootstrap, BootstrapTimeout: a.r.BootTimeout.Duration,
	}
	var ami, rootDev string
	var img ec2Image
	volume := a.r.VolumeGB
	if spec.bake {
		opts.IdleTimeout, opts.MaxLifetime = 60*time.Minute, 60*time.Minute
		opts.BootstrapScript = a.bakeRootScript(spec.prepull)
		opts.BootstrapTimeout = a.BakeBootstrapTimeout(len(spec.prepull) > 0)
		opts.MaxLifetime = 2*opts.BootstrapTimeout + 30*time.Minute
		opts.IdleTimeout = opts.MaxLifetime
	} else if (a.r.AMI == "" || a.r.AMI == "auto") && !spec.noBake {
		// A baked image with this runner's bootstrap already applied?
		if b, err := a.findBake(ctx); err == nil && b != nil {
			switch {
			case b.Expired:
				// Its OS packages are too old: boot the current Ubuntu image
				// and run the bootstrap instead.
				log("not using baked image %s: %s; booting the current Ubuntu image (run `spotexec image bake %s`)", b.ID, b.Stale, a.r.Name)
			default:
				ami, rootDev, img = b.ID, b.img.RootDevice, b.img
				opts.BootstrapScript, opts.UserBootstrap = "", ""
				if size := b.img.rootSize(); size > volume {
					volume = size // can't launch with a volume smaller than the image's snapshot
				}
				switch {
				case b.Stale != "":
					log("using baked image %s (stale: %s; run `spotexec image bake %s`)", b.ID, b.Stale, a.r.Name)
				case b.CheckErr != "":
					log("using baked image %s (%s)", b.ID, b.CheckErr)
				default:
					log("using baked image %s", b.ID)
				}
			}
		}
	}
	// User-data that doesn't fit even without the work volume fails here,
	// before any AWS call can turn it into a regional error.
	plain, err := cloudinit.Render(opts)
	if err != nil {
		return ec2Instance{}, err
	}
	if ami == "" {
		if ami, err = a.resolveAMI(ctx); err != nil {
			return ec2Instance{}, asRegionError(ctx, err)
		}
	}
	if rootDev == "" {
		if img, err = a.describeImage(ctx, ami); err != nil {
			return ec2Instance{}, asRegionError(ctx, err)
		}
		rootDev = img.RootDevice
	}
	workDev := ""
	// The bake builder gets no work volume: CreateImage would capture it.
	if gb := a.r.WorkVolume(); gb > 0 && !spec.bake {
		root := a.r.WorkRoot
		if root == "" {
			root = "~/.cache/spotexec"
		}
		var why string
		workDev, why = workDevice(img)
		switch {
		case !cloudinit.WorkRootOK(root):
			log("no work volume: work_root %q isn't a plain path (dependency caches are copied into each slot)", root)
		case workDev == "":
			log("no work volume: %s (dependency caches are copied into each slot)", why)
		default:
			opts.WorkVolumeGB, opts.WorkRoot = gb, root
		}
	}
	ud := plain
	if opts.WorkVolumeGB > 0 {
		// The work volume's setup must not cost a bootstrap script that
		// fitted the 16 KiB of user-data without it.
		if withVol, err := cloudinit.Render(opts); err == nil {
			ud = withVol
		} else {
			log("no work volume: %v (dependency caches are copied into each slot)", err)
			opts.WorkVolumeGB, opts.WorkRoot = 0, ""
		}
	}
	sgs, err := a.securityGroups(ctx, log)
	if err != nil {
		return ec2Instance{}, asRegionError(ctx, err)
	}
	subnet := a.r.SubnetID
	if spec.zone != "" && subnet == "" {
		if subnet, err = a.defaultSubnet(ctx, spec.zone); err != nil {
			var ze *zoneError
			if errors.As(err, &ze) {
				return ec2Instance{}, err
			}
			return ec2Instance{}, asRegionError(ctx, err)
		}
	}
	base := url.Values{}
	base.Set("ImageId", ami)
	base.Set("MinCount", "1")
	base.Set("MaxCount", "1")
	base.Set("UserData", base64.StdEncoding.EncodeToString([]byte(ud)))
	base.Set("InstanceInitiatedShutdownBehavior", "terminate")
	base.Set("MetadataOptions.HttpEndpoint", "enabled")
	base.Set("MetadataOptions.HttpTokens", "required")
	base.Set("MetadataOptions.HttpPutResponseHopLimit", "1")
	base.Set("BlockDeviceMapping.1.DeviceName", rootDev)
	base.Set("BlockDeviceMapping.1.Ebs.VolumeType", "gp3")
	base.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.Itoa(volume))
	base.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")
	if opts.WorkVolumeGB > 0 {
		// Blank: user-data finds it by size, whatever the guest calls it.
		base.Set("BlockDeviceMapping.2.DeviceName", workDev)
		base.Set("BlockDeviceMapping.2.Ebs.VolumeType", "gp3")
		base.Set("BlockDeviceMapping.2.Ebs.VolumeSize", strconv.Itoa(opts.WorkVolumeGB))
		base.Set("BlockDeviceMapping.2.Ebs.DeleteOnTermination", "true")
	}
	tags := [][2]string{{tagRunner, a.r.Name}, {tagOwner, owner}, {tagHost, HostLabel()}, {"Name", "spotexec-" + a.r.Name}}
	if spec.bake {
		// Never part of the fleet: the scheduler must not place work on it.
		tags = [][2]string{{tagBake, a.r.Name}, {tagOwner, owner}, {tagHost, HostLabel()}, {"Name", "spotexec-bake-" + a.r.Name}}
	}
	for i, rt := range []string{"instance", "volume"} {
		pfx := "TagSpecification." + strconv.Itoa(i+1)
		base.Set(pfx+".ResourceType", rt)
		for j, t := range tags {
			tp := pfx + ".Tag." + strconv.Itoa(j+1)
			base.Set(tp+".Key", t[0])
			base.Set(tp+".Value", t[1])
		}
	}
	// Always request a public IPv4 explicitly: default-VPC subnets usually
	// map one on launch, but not when MapPublicIpOnLaunch was turned off.
	base.Set("NetworkInterface.1.DeviceIndex", "0")
	base.Set("NetworkInterface.1.AssociatePublicIpAddress", "true")
	base.Set("NetworkInterface.1.DeleteOnTermination", "true")
	if subnet != "" {
		base.Set("NetworkInterface.1.SubnetId", subnet)
	}
	for i, sg := range sgs {
		base.Set("NetworkInterface.1.SecurityGroupId."+strconv.Itoa(i+1), sg)
	}

	markets := []bool{true}
	if a.r.OnDemandFallback {
		markets = append(markets, false)
	}
	if spec.bake {
		markets = []bool{false} // on-demand: a reclaimed builder wastes the bake
	}
	// The standard Spot quota is shared by every machine using the account:
	// read its live use once, then admit each Spot candidate against it, so
	// a launch that won't fit (or would take the others' last room) is
	// explained, not just refused.
	if spec.onDemand {
		markets = []bool{false} // multi-region fallback: on-demand only
	}
	types := spec.types
	switch {
	case spec.itype != "":
		types, markets = []string{spec.itype}, []bool{true}
	case len(types) == 0:
		if types, err = a.launchTypes(ctx); err != nil {
			return ec2Instance{}, asRegionError(ctx, err)
		}
	}
	var usage *QuotaUsage
	if !spec.bake && markets[0] && minStandardVCPUs(types) > 0 {
		if u, err := a.QuotaUsage(ctx); err == nil && u.Quota >= 0 {
			usage = &u
		}
	}
	if spec.beforeSend != nil {
		if err := spec.beforeSend(); err != nil {
			return ec2Instance{}, err
		}
		// A long run of attempts, or a lost response, still leaves a
		// recent record of the launch.
		defer func() { _ = spec.beforeSend() }()
	}
	try := a.admitting(usage, func(itype string, spot bool) (ec2Instance, error) {
		return a.runOne(ctx, base, itype, spot, log)
	})
	if spec.itype != "" {
		// A placed launch: exactly one Spot type in one zone. The
		// coordinator reports it, and decides from the error (an AWS code,
		// or a *QuotaError from the pre-launch check) what to try next.
		inst, err := try(spec.itype, true)
		if err != nil {
			return ec2Instance{}, fmt.Errorf("launch %s: %w", spec.itype, err)
		}
		return inst, nil
	}
	inst, err := a.launchAcross(markets, types, log, try)
	var qe *QuotaError
	if errors.As(err, &qe) && qe.Code != "" && qe.Usage == nil {
		// Suggest a quota above the current one: other runners or machines
		// may be what fills it.
		if q, qerr := a.quotaValue(ctx, qe.Code); qerr == nil {
			qe.Desired = max(qe.Desired, q+qe.VCPUs)
		}
	}
	return inst, err
}

// quotaLookupTimeout bounds a Service Quotas read: it only adds detail.
var quotaLookupTimeout = 5 * time.Second

// quotaValue reads a vCPU quota for the runner's region from Service Quotas.
func (a *AWSSpot) quotaValue(ctx context.Context, code string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, quotaLookupTimeout)
	defer cancel()
	qb, _ := json.Marshal(map[string]string{"ServiceCode": "ec2", "QuotaCode": code})
	status, b, err := a.api.do(ctx, "servicequotas", "application/x-amz-json-1.1", map[string]string{"X-Amz-Target": "ServiceQuotasV20190624.GetServiceQuota"}, qb)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("GetServiceQuota: HTTP %d: %s", status, strings.TrimSpace(string(b)))
	}
	var v struct{ Quota struct{ Value float64 } }
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, err
	}
	return int(v.Quota.Value), nil
}

// launchAcross tries each market (spot, then on-demand when allowed) and
// each instance type in turn until one launches. Capacity and price
// rejections move on to the next type, and so do quota rejections (a
// smaller type may still fit). When the quota is what stopped every market
// tried (no type smaller than the smallest refused one fell short only on
// capacity), the error is a *QuotaError, so callers stop waiting for
// capacity that the quota won't allow.
func (a *AWSSpot) launchAcross(markets []bool, types []string, log Logger, try func(itype string, spot bool) (ec2Instance, error)) (ec2Instance, error) {
	var errs []string
	var quota *QuotaError
	quotaEveryMarket := true
	// Types refused on Spot at max_price: a Spot price is never above the
	// on-demand price, so on-demand would launch them above the cap too.
	overCap := map[string]bool{}
	for _, spot := range markets {
		var mq *QuotaError        // this market's quota refusal (smallest launch)
		short := map[string]int{} // smallest type refused for capacity or price, by quota
		for _, itype := range types {
			if !spot && overCap[itype] {
				continue
			}
			inst, err := try(itype, spot)
			if err == nil {
				kind := "spot"
				if !spot {
					kind = "on-demand"
				}
				log("launched %s %s instance %s in %s", kind, itype, inst.InstanceID, a.r.Region)
				return inst, nil
			}
			class := classifyAWSError(err)
			code := awsCode(err)
			checked, isChecked := err.(*QuotaError) // refused by the pre-launch quota check
			if isChecked {
				class, code = errQuota, "quota"
			}
			if errors.Is(err, ErrLaunchAmbiguous) || (class != errNextType && class != errQuota) {
				return ec2Instance{}, fmt.Errorf("launch %s: %w", itype, err)
			}
			if isChecked {
				log("skipping %s: %s", itype, firstLineOfErr(err))
			} else {
				log("skipping %s: %v", itype, err)
			}
			errs = append(errs, itype+": "+code)
			if spot && code == "SpotMaxPriceTooLow" {
				overCap[itype] = true
			}
			switch {
			case class == errQuota && (mq == nil || vcpusOf(itype) < mq.VCPUs):
				if isChecked {
					mq = checked
				} else {
					mq = &QuotaError{Region: a.r.Region, Spot: spot, Code: quotaCodeFor(itype, spot), VCPUs: vcpusOf(itype)}
				}
			case class == errNextType && capacityCode(awsCode(err)):
				if c := quotaCodeFor(itype, spot); short[c] == 0 || vcpusOf(itype) < short[c] {
					short[c] = vcpusOf(itype)
				}
			}
		}
		if mq != nil {
			for code, n := range short {
				if code != mq.Code || code == "" || n < mq.VCPUs {
					// A type under another quota, or a smaller one, may
					// launch once capacity returns.
					mq = nil
					break
				}
			}
		}
		if mq == nil {
			quotaEveryMarket = false
		} else if quota == nil {
			quota = mq // the Spot quota is the one to report when both refused
		}
		if spot && a.r.OnDemandFallback {
			left := false
			for _, it := range types {
				left = left || !overCap[it]
			}
			switch {
			case !left:
				log("every type's Spot price is above max_price, so its on-demand price is too; not falling back to on-demand")
			case mq != nil:
				log("spot vCPU quota reached; falling back to on-demand")
			default:
				log("no spot capacity; falling back to on-demand")
			}
		}
	}
	if quota != nil && quotaEveryMarket {
		if quota.Usage == nil { // the pre-launch check already says what uses the quota
			quota.Detail = strings.Join(errs, ", ")
		}
		quota.Desired = max(quota.Desired, quota.VCPUs, a.r.MaxInstances*VMVCPUs(a.r))
		return ec2Instance{}, quota
	}
	hint := "raise max_price, or set on_demand_fallback = true"
	if a.r.OnDemandFallback {
		hint = "or raise max_price"
	}
	return ec2Instance{}, fmt.Errorf("no instance type could be launched in %s (%s); %s, %s", a.r.Region, strings.Join(errs, ", "), widenHint(a.r), hint)
}

// capacityCode reports refusals that pass with time (capacity, price), as
// opposed to types that can never launch here.
func capacityCode(code string) bool {
	switch code {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientHostCapacity", "SpotMaxPriceTooLow":
		return true
	}
	return false
}

// runOne calls RunInstances for one instance type. The ClientToken makes the
// call's own retries idempotent; a new token is minted per call.
func (a *AWSSpot) runOne(ctx context.Context, base url.Values, itype string, spot bool, log Logger) (ec2Instance, error) {
	newToken := func() string {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return "sx-" + hex.EncodeToString(b[:])
	}
	p := url.Values{}
	for k, v := range base {
		p[k] = append([]string(nil), v...)
	}
	p.Set("InstanceType", itype)
	p.Set("ClientToken", newToken())
	if spot {
		p.Set("InstanceMarketOptions.MarketType", "spot")
		p.Set("InstanceMarketOptions.SpotOptions.SpotInstanceType", "one-time")
		p.Set("InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior", "terminate")
		if a.r.MaxPrice.Set {
			p.Set("InstanceMarketOptions.SpotOptions.MaxPrice", a.r.MaxPrice.String())
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		b, err := a.api.ec2(ctx, "RunInstances", p)
		if err == nil {
			inst, perr := parseRunInstances(b)
			if perr == nil && inst.InstanceID == "" {
				perr = errors.New("no instance ID in the response")
			}
			if perr != nil {
				// EC2 accepted the request: it may have made an instance.
				return ec2Instance{}, fmt.Errorf("%w: unreadable RunInstances response: %v", ErrLaunchAmbiguous, perr)
			}
			return inst, nil
		}
		lastErr = err
		if c := awsCode(err); c == "IdempotentInstanceTerminated" || c == "IdempotentParameterMismatch" {
			p.Set("ClientToken", newToken())
			continue
		}
		// SG eventual consistency right after CreateSecurityGroup.
		if awsCode(err) == "InvalidGroup.NotFound" {
			select {
			case <-ctx.Done():
				return ec2Instance{}, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		break
	}
	if awsCode(lastErr) == "" && lastErr != nil {
		// Includes cancellation after the request was sent.
		// No API verdict (network error after retries): the request may
		// have created an instance.
		return ec2Instance{}, fmt.Errorf("%w: %v", ErrLaunchAmbiguous, lastErr)
	}
	return ec2Instance{}, lastErr
}

func (a *AWSSpot) amiParam() string {
	return "/aws/service/canonical/ubuntu/server/24.04/stable/current/" + a.r.Arch + "/hvm/ebs-gp3/ami-id"
}

func (a *AWSSpot) resolveAMI(ctx context.Context) (string, error) {
	if a.r.AMI != "" && a.r.AMI != "auto" && a.r.AMI != "base" {
		return a.r.AMI, nil
	}
	ami, err := a.api.ssmGetParameter(ctx, a.amiParam())
	if err != nil {
		return "", fmt.Errorf("resolve Ubuntu 24.04 AMI via SSM %s: %w (or set ami = \"ami-...\" on the runner)", a.amiParam(), err)
	}
	return ami, nil
}

// rootDevice reads the AMI's root device name (Ubuntu uses /dev/sda1,
// Amazon Linux /dev/xvda); a wrong name would attach a second disk instead
// of growing the root volume.
func (a *AWSSpot) describeImage(ctx context.Context, ami string) (ec2Image, error) {
	p := url.Values{}
	p.Set("ImageId.1", ami)
	b, err := a.api.ec2(ctx, "DescribeImages", p)
	if err != nil {
		return ec2Image{}, fmt.Errorf("describe AMI %s: %w", ami, err)
	}
	var out struct {
		Images []ec2Image `xml:"imagesSet>item"`
	}
	if err := xml.Unmarshal(b, &out); err == nil && len(out.Images) > 0 && out.Images[0].RootDevice != "" {
		return out.Images[0], nil
	}
	return ec2Image{}, fmt.Errorf("AMI %s not found in %s", ami, a.r.Region)
}

// workDevice names the work volume's mapping: the first of /dev/sdf to
// /dev/sdp the image doesn't map (as sdX or xvdX). None when the image maps
// an EBS volume besides its root: user-data tells the work volume by its
// size and blankness, which a data volume of the image could share.
func workDevice(img ec2Image) (dev, why string) {
	used := map[string]bool{}
	norm := func(n string) string {
		n = strings.TrimPrefix(n, "/dev/")
		n = strings.TrimPrefix(n, "xv")
		n = strings.TrimPrefix(n, "s")
		return strings.TrimRight(n, "0123456789") // "da1" -> "da"
	}
	for _, d := range img.Devices {
		used[norm(d.Name)] = true
		if d.EBS != nil && norm(d.Name) != norm(img.RootDevice) {
			return "", fmt.Sprintf("image %s maps an EBS volume at %s besides its root", img.ID, d.Name)
		}
	}
	used[norm(img.RootDevice)] = true
	for c := 'f'; c <= 'p'; c++ {
		if !used["d"+string(c)] {
			return "/dev/sd" + string(c), ""
		}
	}
	return "", fmt.Sprintf("image %s maps every device from /dev/sdf to /dev/sdp", img.ID)
}

// securityGroups returns configured SGs or finds/creates spotexec-ssh.
func (a *AWSSpot) securityGroups(ctx context.Context, log Logger) ([]string, error) {
	if len(a.r.SecurityGroupIDs) > 0 {
		return a.r.SecurityGroupIDs, nil
	}
	// One group per allowed CIDR: changing ssh_cidr moves new instances to a
	// new group instead of piling rules onto (or leaving 0.0.0.0/0 open on)
	// the old one.
	sgName := sgBaseName
	if a.r.SSHCIDR != "0.0.0.0/0" {
		h := sha256.Sum256([]byte(a.r.SSHCIDR))
		sgName = sgBaseName + "-" + hex.EncodeToString(h[:])[:8]
	}
	vpc, err := a.vpcID(ctx)
	if err != nil {
		return nil, err
	}
	body := ""
	find := func() (string, error) {
		p := url.Values{}
		p.Set("Filter.1.Name", "group-name")
		p.Set("Filter.1.Value.1", sgName)
		p.Set("Filter.2.Name", "vpc-id")
		p.Set("Filter.2.Value.1", vpc)
		b, err := a.api.ec2(ctx, "DescribeSecurityGroups", p)
		if err != nil {
			return "", err
		}
		body = string(b)
		return xmlFirst(b, "securityGroupInfo>item>groupId"), nil
	}
	id, err := find()
	if err != nil {
		return nil, err
	}
	// Existing group that already allows SSH from our CIDR: no mutating call.
	if id != "" && hasSSHIngress([]byte(body), a.r.SSHCIDR) {
		return []string{id}, nil
	}
	// Overlapping launches on this machine create or open it one at a
	// time, so the second finds the first's group rather than racing its
	// creation (a fresh group may not be listed yet).
	if a.dir != "" {
		unlock, err := waitLockFile(ctx, a.lockPath("sg-"+a.r.Region+".lock"), syscall.LOCK_EX, nil)
		if err != nil {
			return nil, err
		}
		defer unlock()
		if id, err = find(); err != nil {
			return nil, err
		}
		if id != "" && hasSSHIngress([]byte(body), a.r.SSHCIDR) {
			return []string{id}, nil
		}
	}
	if id == "" {
		owner, err := a.ownerTag()
		if err != nil {
			return nil, err
		}
		p := url.Values{}
		p.Set("GroupName", sgName)
		p.Set("GroupDescription", sgDescription)
		p.Set("VpcId", vpc)
		// Tagged at creation: the IAM policy only lets spotexec's key change
		// resources that carry spotexec-owner, and only tag them as they're made.
		p.Set("TagSpecification.1.ResourceType", "security-group")
		for i, t := range [][2]string{{tagOwner, owner}, {"Name", sgName}} {
			p.Set("TagSpecification.1.Tag."+strconv.Itoa(i+1)+".Key", t[0])
			p.Set("TagSpecification.1.Tag."+strconv.Itoa(i+1)+".Value", t[1])
		}
		b, err := a.api.ec2(ctx, "CreateSecurityGroup", p)
		switch {
		case err == nil:
			id = xmlFirst(b, "groupId")
			log("created security group %s (%s) in %s", sgName, id, vpc)
		case awsCode(err) == "InvalidGroup.Duplicate":
			// Made meanwhile (another runner or machine), and maybe not
			// listed yet.
			for try := 0; ; try++ {
				if id, err = find(); err != nil {
					return nil, err
				}
				if id != "" || try == 5 {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Second):
				}
			}
		default:
			return nil, fmt.Errorf("create security group: %w", err)
		}
		if id == "" {
			return nil, errors.New("security group creation returned no id")
		}
	}
	p := url.Values{}
	p.Set("GroupId", id)
	p.Set("IpPermissions.1.IpProtocol", "tcp")
	p.Set("IpPermissions.1.FromPort", "22")
	p.Set("IpPermissions.1.ToPort", "22")
	if strings.Contains(a.r.SSHCIDR, ":") {
		p.Set("IpPermissions.1.Ipv6Ranges.1.CidrIpv6", a.r.SSHCIDR)
	} else {
		p.Set("IpPermissions.1.IpRanges.1.CidrIp", a.r.SSHCIDR)
	}
	if _, err := a.api.ec2(ctx, "AuthorizeSecurityGroupIngress", p); err != nil && awsCode(err) != "InvalidPermission.Duplicate" {
		if c := awsCode(err); (c == "UnauthorizedOperation" || c == "AccessDenied") && body != "" && !sgTagged([]byte(body)) {
			return nil, fmt.Errorf("authorize SSH ingress on %s: %w\nsecurity group %s was made by an older spotexec and lacks the %s tag spotexec's IAM policy now requires: run `spotexec setup` once (it tags it), or delete the group", id, err, sgName, tagOwner)
		}
		return nil, fmt.Errorf("authorize SSH ingress on %s: %w", id, err)
	}
	return []string{id}, nil
}

// ec2SecurityGroup is one DescribeSecurityGroups item.
type ec2SecurityGroup struct {
	ID          string   `xml:"groupId"`
	Name        string   `xml:"groupName"`
	Description string   `xml:"groupDescription"`
	VPC         string   `xml:"vpcId"`
	Tags        []ec2Tag `xml:"tagSet>item"`
}

const sgDescription = "spotexec runners: SSH ingress"

var sgNameRE = regexp.MustCompile(`^` + sgBaseName + `(-[0-9a-f]{8})?$`)

// madeBySpotexec reports whether the group has exactly the name and
// description securityGroups gives the groups it creates, so a group that
// merely starts with "spotexec-ssh" is never adopted.
func (g ec2SecurityGroup) madeBySpotexec() bool {
	return sgNameRE.MatchString(g.Name) && g.Description == sgDescription
}

func (g ec2SecurityGroup) tag(k string) string {
	for _, t := range g.Tags {
		if t.Key == k {
			return t.Value
		}
	}
	return ""
}

func parseSecurityGroups(b []byte) ([]ec2SecurityGroup, string, error) {
	var v struct {
		Groups []ec2SecurityGroup `xml:"securityGroupInfo>item"`
		Next   string             `xml:"nextToken"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, "", fmt.Errorf("parse DescribeSecurityGroups: %w", err)
	}
	return v.Groups, v.Next, nil
}

// sgTagged reports whether the (first) group in a DescribeSecurityGroups
// response carries spotexec's owner tag.
func sgTagged(b []byte) bool {
	gs, _, err := parseSecurityGroups(b)
	return err == nil && len(gs) > 0 && gs[0].tag(tagOwner) != ""
}

// spotexecSGFilter selects spotexec's own security groups by name.
func spotexecSGFilter() url.Values {
	return url.Values{"Filter.1.Name": {"group-name"}, "Filter.1.Value.1": {sgBaseName}, "Filter.1.Value.2": {sgBaseName + "-*"}}
}

// vpcID returns the configured subnet's VPC, or the default VPC.
func (a *AWSSpot) vpcID(ctx context.Context) (string, error) {
	p := url.Values{}
	if a.r.SubnetID != "" {
		p.Set("SubnetId.1", a.r.SubnetID)
		b, err := a.api.ec2(ctx, "DescribeSubnets", p)
		if err != nil {
			return "", fmt.Errorf("describe subnet %s: %w", a.r.SubnetID, err)
		}
		if v := xmlFirst(b, "subnetSet>item>vpcId"); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("subnet %s not found", a.r.SubnetID)
	}
	p.Set("Filter.1.Name", "is-default")
	p.Set("Filter.1.Value.1", "true")
	b, err := a.api.ec2(ctx, "DescribeVpcs", p)
	if err != nil {
		return "", fmt.Errorf("describe default VPC: %w", err)
	}
	if v := xmlFirst(b, "vpcSet>item>vpcId"); v != "" {
		return v, nil
	}
	if a.placed {
		return "", fmt.Errorf("no default VPC in %s (to use it: aws ec2 create-default-vpc --region %s)", a.r.Region, a.r.Region)
	}
	return "", fmt.Errorf("no default VPC in %s; set subnet_id (and optionally security_group_ids) on the runner", a.r.Region)
}

// subnetZone returns the availability zone of the configured subnet_id.
func (a *AWSSpot) subnetZone(ctx context.Context) (string, error) {
	p := url.Values{}
	p.Set("SubnetId.1", a.r.SubnetID)
	b, err := a.api.ec2(ctx, "DescribeSubnets", p)
	if err != nil {
		return "", fmt.Errorf("describe subnet %s: %w", a.r.SubnetID, err)
	}
	if z := xmlFirst(b, "subnetSet>item>availabilityZone"); z != "" {
		return z, nil
	}
	return "", fmt.Errorf("subnet %s not found in %s", a.r.SubnetID, a.r.Region)
}

// defaultSubnet returns the default VPC's subnet in one availability zone,
// so a launch lands in the zone whose Spot price was chosen.
func (a *AWSSpot) defaultSubnet(ctx context.Context, zone string) (string, error) {
	p := url.Values{}
	p.Set("Filter.1.Name", "availability-zone")
	p.Set("Filter.1.Value.1", zone)
	p.Set("Filter.2.Name", "default-for-az")
	p.Set("Filter.2.Value.1", "true")
	b, err := a.api.ec2(ctx, "DescribeSubnets", p)
	if err != nil {
		return "", fmt.Errorf("describe default subnet in %s: %w", zone, err)
	}
	if id := xmlFirst(b, "subnetSet>item>subnetId"); id != "" {
		return id, nil
	}
	// Default VPCs made before a zone opened have no subnet there; the
	// region's other zones may still work.
	return "", &zoneError{fmt.Errorf("no default subnet in %s", zone)}
}

// zoneError marks a launch that can't use one availability zone.
type zoneError struct{ err error }

func (e *zoneError) Error() string { return e.err.Error() }
func (e *zoneError) Unwrap() error { return e.err }

// Terminate ends one specific instance, but only if it carries our tags.
func (a *AWSSpot) Terminate(ctx context.Context, id string) error {
	// Looked up by ID (tag-filtered listings lag for brand-new instances);
	// the owner and runner tags are still checked before terminating.
	err := a.terminateAny(ctx, id)
	if err == nil {
		_ = os.Remove(a.knownHosts(id))
		// Its vCPUs are free once listings stop showing it, as after Down:
		// a replacement near the Spot quota needn't wait out the claim.
		a.releaseClaims([]string{id})
	}
	return err
}

// pruneKnownHosts removes known_hosts files older than max_lifetime (their
// instances are certainly gone).
func (a *AWSSpot) pruneKnownHosts() {
	ents, _ := os.ReadDir(a.dir)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "known_hosts-") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > a.r.MaxLifetime.Duration+time.Hour {
			_ = os.Remove(filepath.Join(a.dir, e.Name()))
		}
	}
}

// Down terminates every instance tagged for this runner and owner.
func (a *AWSSpot) Down(ctx context.Context, log Logger) error {
	if log == nil {
		log = func(string, ...any) {}
	}
	a.SweepBuilders(ctx, log)
	unlock, err := a.lockUp(ctx, log)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = a.downLocked(ctx, log)
	return err
}

// downLocked is Down for a caller holding the provisioning lock (and
// handling builders). It returns how many instances it terminated.
func (a *AWSSpot) downLocked(ctx context.Context, log Logger) (int, error) {
	insts, err := a.describe(ctx, "pending", "running", "stopping", "stopped")
	if err != nil {
		return 0, err
	}
	recent, lerr := a.unlistedLaunches(ctx, insts)
	insts = append(insts, recent...)
	if len(insts) == 0 {
		if lerr == nil {
			log("no running instances for %s", a.r.Name)
		}
		return 0, lerr
	}
	ids := make([]string, len(insts))
	types := make(map[string]string, len(insts))
	for i, inst := range insts {
		ids[i] = inst.InstanceID
		types[inst.InstanceID] = inst.InstanceType
	}
	done, err := a.terminate(ctx, ids)
	for _, id := range done {
		log("terminating %s (%s)", id, types[id])
		_ = os.Remove(a.knownHosts(id))
	}
	if len(done) > 0 {
		a.releaseClaims(done)
		a.forgetLaunched(done)
	}
	return len(done), errors.Join(err, lerr)
}

// unlistedLaunches looks up by ID the instances this machine launched in
// the region moments ago that listed (a tag-filtered listing) leaves out:
// EC2's tag index can lag a brand-new instance. Those still ours and not
// yet ending are returned; an error names the ones that couldn't be looked
// up, which may still be running.
func (a *AWSSpot) unlistedLaunches(ctx context.Context, listed []ec2Instance) ([]ec2Instance, error) {
	ids := a.recentLaunches()
	if len(ids) == 0 {
		return nil, nil
	}
	owner, err := a.ownerTag()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, i := range listed {
		seen[i.InstanceID] = true
	}
	var out []ec2Instance
	var errs []error
	for _, id := range ids {
		if seen[id] {
			continue
		}
		i, err := a.describeID(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("can't look up %s, launched moments ago: %w", id, err))
			continue
		}
		switch i.State {
		case "pending", "running", "stopping", "stopped":
		default:
			continue // already ending
		}
		if i.tag(tagOwner) == owner && i.tag(tagRunner) == a.r.Name {
			out = append(out, i)
		}
	}
	return out, errors.Join(errs...)
}

// maxTerminateIDs is the most instance IDs EC2 takes in one
// TerminateInstances request.
const maxTerminateIDs = 1000

// terminateRefill is how long EC2's TerminateInstances resource bucket takes
// to regain one instance (20 a second by default). A full batch empties the
// default bucket, so the next one can be throttled for longer than query's
// own retries cover.
var terminateRefill = 50 * time.Millisecond

// terminate ends the given instances in batches EC2 accepts. It sends every
// batch even after one is refused, and returns the IDs of the batches EC2
// accepted, with an error naming those it refused or never sent. A refused
// batch's outcome isn't known (EC2 can end part of it, and a lost response
// hides an accepted one), but listing and terminating again is safe:
// instances already shutting down drop out of the listing.
func (a *AWSSpot) terminate(ctx context.Context, ids []string) ([]string, error) {
	var done []string
	var errs []error
	failed := 0
	batches := (len(ids) + maxTerminateIDs - 1) / maxTerminateIDs
	for b := 0; b < batches; b++ {
		lo := b * maxTerminateIDs
		if err := ctx.Err(); err != nil {
			failed += len(ids) - lo
			errs = append(errs, fmt.Errorf("batches %d to %d of %d (%d instances) not sent: %w", b+1, batches, batches, len(ids)-lo, err))
			break
		}
		batch := ids[lo:min(lo+maxTerminateIDs, len(ids))]
		err := a.terminateBatch(ctx, batch)
		if awsCode(err) == "RequestLimitExceeded" {
			// Wait for the bucket to refill enough for this batch, then try
			// once more.
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(len(batch)) * terminateRefill):
			}
			if ctx.Err() != nil {
				err = errors.Join(err, ctx.Err())
			} else {
				err = a.terminateBatch(ctx, batch)
			}
		}
		if err != nil {
			failed += len(batch)
			errs = append(errs, fmt.Errorf("batch %d of %d (%d instances): %w", b+1, batches, len(batch), err))
			continue
		}
		done = append(done, batch...)
	}
	if len(errs) > 0 {
		return done, fmt.Errorf("terminate: %d of %d instances not confirmed: %w", failed, len(ids), errors.Join(errs...))
	}
	return done, nil
}

func (a *AWSSpot) terminateBatch(ctx context.Context, ids []string) error {
	p := url.Values{}
	for i, id := range ids {
		p.Set("InstanceId."+strconv.Itoa(i+1), id)
	}
	_, err := a.api.ec2(ctx, "TerminateInstances", p)
	return err
}

// Doctor checks credentials, identity, AMI and networking.
func (a *AWSSpot) Doctor(ctx context.Context) []Check {
	var out []Check
	creds, err := a.api.creds.get(ctx)
	if err != nil {
		return append(out, Check{Name: "aws credentials", Msg: err.Error()})
	}
	out = append(out, Check{Name: "aws credentials", OK: true, Msg: creds.Source})
	b, err := a.api.query(ctx, "sts", stsAPIVersion, "GetCallerIdentity", nil)
	if err != nil {
		out = append(out, Check{Name: "aws identity", Msg: err.Error()})
	} else {
		out = append(out, Check{Name: "aws identity", OK: true,
			Msg: "account " + xmlFirst(b, "GetCallerIdentityResult>Account") + " · " + xmlFirst(b, "GetCallerIdentityResult>Arn")})
	}
	if ami, err := a.resolveAMI(ctx); err != nil {
		out = append(out, Check{Name: "ami", Msg: err.Error()})
	} else {
		out = append(out, Check{Name: "ami", OK: true, Msg: ami + " (" + a.r.Arch + ", " + a.r.Region + ")"})
	}
	if vpc, err := a.vpcID(ctx); err != nil {
		out = append(out, Check{Name: "network", Msg: err.Error()})
	} else {
		msg := "default VPC " + vpc
		if a.r.SubnetID != "" {
			msg = "subnet " + a.r.SubnetID + " in " + vpc
		}
		out = append(out, Check{Name: "network", OK: true, Msg: msg})
	}
	// New accounts start with a small Spot vCPU quota (often 5 vCPUs).
	if c, ok := a.quotaCheck(ctx); ok {
		out = append(out, c)
	}
	out = append(out, a.priceCheck(ctx))
	// Instances live in the runner's region, which is easy to miss when the
	// console (or the AWS CLI default) points elsewhere.
	where := fmt.Sprintf("instances appear in EC2 → %s; filter by tag %s=%s", a.r.Region, tagRunner, a.r.Name)
	if out, err := exec.Command("aws", "configure", "get", "region").Output(); err == nil {
		if def := strings.TrimSpace(string(out)); def != "" && def != a.r.Region {
			where += fmt.Sprintf(" (your AWS CLI default region is %s; switch the console's region selector)", def)
		}
	}
	out = append(out, Check{Name: "console", OK: true, Msg: where})
	out = append(out, a.bakeCheck(ctx))
	for _, t := range a.r.InstanceTypes {
		if strings.HasPrefix(t, "t2.") || strings.HasPrefix(t, "t3.") || strings.HasPrefix(t, "t3a.") || strings.HasPrefix(t, "t4g.") {
			out = append(out, Check{Name: "instance types", OK: true, Msg: "note: " + t + " is burstable; sustained CPU on spot t-family instances bills surplus credits. Prefer c7a/m7a/c6a."})
			break
		}
	}
	return out
}

// spotQuota reads the region's standard Spot vCPU quota.
func (a *AWSSpot) spotQuota(ctx context.Context) (int, error) {
	return a.quotaValue(ctx, QuotaCodeSpotStandard)
}

// vcpusOf estimates vCPUs from an EC2 size suffix (large=2, xlarge=4, NxNlarge=4N).
// VCPUsOf estimates an EC2 instance type's vCPUs from its size name.
func VCPUsOf(t string) int { return vcpusOf(t) }

func vcpusOf(t string) int {
	_, size, _ := strings.Cut(t, ".")
	switch size {
	case "nano", "micro", "small":
		if strings.HasPrefix(t, "t2.") {
			return 1
		}
		return 2
	case "medium":
		if strings.HasPrefix(t, "t") {
			return 2 // burstable mediums; the others (Graviton, a1) have 1
		}
		return 1
	case "large":
		return 2
	case "xlarge":
		return 4
	}
	if t == "c4.8xlarge" {
		return 36 // the odd one out
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(size, "xlarge")); err == nil {
		return 4 * n
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(size, "metal-"), "xl")); err == nil {
		return 4 * n // metal-48xl
	}
	if size == "metal" {
		return 96 // varies by family (64-448); the common size
	}
	return 4
}

// tagHost names the machine that launched an instance, for people (and
// `status -A`); ownership is still tagOwner.
const tagHost = "spotexec-host"

// HostLabel is this machine's short, stable name: macOS's LocalHostName,
// else the hostname up to the first dot.
func HostLabel() string {
	if b, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return strings.Split(h, ".")[0]
}

// AccountInstance is a spotexec instance (a fleet VM or an image-bake
// builder) launched by any machine, and where it was found.
type AccountInstance struct {
	ID       string    `json:"id"`
	Account  string    `json:"account,omitempty"`
	Region   string    `json:"region"`
	Profile  string    `json:"profile,omitempty"` // the AWS profile it was found (and is terminated) through
	Runner   string    `json:"runner"`
	Builder  bool      `json:"builder,omitempty"` // an `image bake` builder, not part of the fleet
	Host     string    `json:"host,omitempty"`
	Owner    string    `json:"owner"`
	Type     string    `json:"type"`
	Market   string    `json:"market"`
	State    string    `json:"state"`
	IP       string    `json:"ip,omitempty"`
	Launched time.Time `json:"launched"`
	Mine     bool      `json:"mine"`
}
