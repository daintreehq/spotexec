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
	r     *config.Runner
	api   *awsClient
	dir   string
	owner string
}

// NewAWSSpot constructs the provider.
func NewAWSSpot(r *config.Runner) (Provider, error) {
	if len(r.InstanceTypes) == 0 {
		return nil, fmt.Errorf("runner %s: instance_types is empty", r.Name)
	}
	if r.Arch != "amd64" && r.Arch != "arm64" {
		return nil, fmt.Errorf("runner %s: arch must be amd64 or arm64 (got %q)", r.Name, r.Arch)
	}
	return &AWSSpot{
		r:   r,
		dir: RunnerStateDir(r.Name),
		api: &awsClient{
			region: r.Region,
			creds:  &awsCredResolver{profile: r.Profile},
			http:   &http.Client{Timeout: 60 * time.Second},
			// ECR's API host is api.ecr.<region>, not ecr.<region>.
			endpoints: map[string]string{"ecr": "https://api.ecr." + r.Region + ".amazonaws.com/"},
		},
	}, nil
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

// lockUp takes the per-runner provisioning lock, honoring ctx.
func (a *AWSSpot) lockUp(ctx context.Context, log Logger) (func(), error) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(a.dir, "up.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	logged := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if !logged && log != nil {
			log("waiting for another spotexec process that is provisioning %s", a.r.Name)
			logged = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

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
	in := Instance{ID: i.InstanceID, State: mapEC2State(i.State), Type: i.InstanceType, LaunchedAt: i.launched(), Spot: i.Lifecycle == "spot"}
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
	// Shares the provisioning lock with Down, so `spotexec down` cannot
	// finish while a launch is about to create an instance.
	unlock, err := a.lockUp(ctx, nil)
	if err != nil {
		return Instance{}, err
	}
	defer unlock()
	inst, err := a.launch(ctx, log)
	if err != nil {
		return Instance{}, err
	}
	return a.instance(inst), nil
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
	return Instance{ID: info.InstanceID, State: info.State, Target: info.Target, Type: info.InstanceType, LaunchedAt: info.LaunchedAt, Spot: info.Spot}, nil
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
		p := url.Values{}
		for i, s := range stopped {
			p.Set("InstanceId."+strconv.Itoa(i+1), s.InstanceID)
		}
		if _, err := a.api.ec2(ctx, "TerminateInstances", p); err == nil {
			log("terminated %d stopped instance(s) left over for %s", len(stopped), a.r.Name)
		}
	}
	inst, err := a.launch(ctx, log)
	if err != nil {
		return Info{}, err
	}
	info, err := a.waitRunning(ctx, inst, log)
	info.Launched = true
	if info.InstanceID == "" {
		info.InstanceID = inst.InstanceID
	}
	return info, err
}

// waitRunning polls until the instance is running with a public IP.
func (a *AWSSpot) waitRunning(ctx context.Context, inst ec2Instance, log Logger) (Info, error) {
	deadline := time.Now().Add(a.r.BootTimeout.Duration)
	if a.r.BootTimeout.Duration <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	logged := ""
	var runningNoIP time.Time
	for {
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
		if time.Now().After(deadline) {
			return Info{}, fmt.Errorf("instance %s not running with a public IP after %s (state %s)", inst.InstanceID, a.r.BootTimeout.Duration, inst.State)
		}
		select {
		case <-ctx.Done():
			return Info{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		p := url.Values{}
		p.Set("InstanceId.1", inst.InstanceID)
		b, err := a.api.ec2(ctx, "DescribeInstances", p)
		if err != nil {
			if awsCode(err) == "InvalidInstanceID.NotFound" {
				continue // eventual consistency right after launch
			}
			return Info{}, err
		}
		got, _, err := parseDescribeInstances(b)
		if err != nil {
			return Info{}, err
		}
		if len(got) > 0 {
			inst = got[0]
		}
	}
}

// launch performs RunInstances across the configured instance types.
func (a *AWSSpot) launch(ctx context.Context, log Logger) (ec2Instance, error) {
	inst, err := a.launchWith(ctx, log, launchSpec{})
	if err != nil && (a.r.AMI == "" || a.r.AMI == "auto") && strings.HasPrefix(awsCode(err), "InvalidAMIID") {
		// The bake was deregistered (`image clear` or pruning on another
		// machine) between picking it and launching: boot without it.
		log("baked image is gone (%s); launching from the current Ubuntu image", awsCode(err))
		return a.launchWith(ctx, log, launchSpec{noBake: true})
	}
	return inst, err
}

// launchSpec varies a launch: normal fleet instances (spot, from the newest
// matching baked AMI when there is one) or an image-bake builder.
type launchSpec struct {
	bake    bool
	prepull []string
	noBake  bool // boot the base image even if a bake matches
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
				ami, rootDev = b.ID, b.img.RootDevice
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
	ud, err := cloudinit.Render(opts)
	if err != nil {
		return ec2Instance{}, err
	}
	if ami == "" {
		if ami, err = a.resolveAMI(ctx); err != nil {
			return ec2Instance{}, err
		}
	}
	if rootDev == "" {
		if rootDev, err = a.rootDevice(ctx, ami); err != nil {
			return ec2Instance{}, err
		}
	}
	sgs, err := a.securityGroups(ctx, log)
	if err != nil {
		return ec2Instance{}, err
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
	if a.r.SubnetID != "" {
		base.Set("NetworkInterface.1.SubnetId", a.r.SubnetID)
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
	inst, err := a.launchAcross(markets, log, func(itype string, spot bool) (ec2Instance, error) {
		return a.runOne(ctx, base, itype, spot, log)
	})
	var qe *QuotaError
	if errors.As(err, &qe) && qe.Code != "" {
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
func (a *AWSSpot) launchAcross(markets []bool, log Logger, try func(itype string, spot bool) (ec2Instance, error)) (ec2Instance, error) {
	var errs []string
	var quota *QuotaError
	quotaEveryMarket := true
	for _, spot := range markets {
		var mq *QuotaError        // this market's quota refusal (smallest launch)
		short := map[string]int{} // smallest type refused for capacity or price, by quota
		for _, itype := range a.r.InstanceTypes {
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
			if errors.Is(err, ErrLaunchAmbiguous) || (class != errNextType && class != errQuota) {
				return ec2Instance{}, fmt.Errorf("launch %s: %w", itype, err)
			}
			log("skipping %s: %v", itype, err)
			errs = append(errs, itype+": "+awsCode(err))
			switch {
			case class == errQuota && (mq == nil || vcpusOf(itype) < mq.VCPUs):
				mq = &QuotaError{Region: a.r.Region, Spot: spot, Code: quotaCodeFor(itype, spot), VCPUs: vcpusOf(itype)}
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
			if mq != nil {
				log("spot vCPU quota reached; falling back to on-demand")
			} else {
				log("no spot capacity; falling back to on-demand")
			}
		}
	}
	if quota != nil && quotaEveryMarket {
		quota.Detail = strings.Join(errs, ", ")
		quota.Desired = max(quota.VCPUs, a.r.MaxInstances*vcpusOf(a.r.InstanceTypes[0]))
		return ec2Instance{}, quota
	}
	return ec2Instance{}, fmt.Errorf("no instance type could be launched in %s (%s); add more instance_types, raise max_price, or set on_demand_fallback = true", a.r.Region, strings.Join(errs, ", "))
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
		if a.r.MaxPrice != "" {
			p.Set("InstanceMarketOptions.SpotOptions.MaxPrice", a.r.MaxPrice)
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		b, err := a.api.ec2(ctx, "RunInstances", p)
		if err == nil {
			return parseRunInstances(b)
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
func (a *AWSSpot) rootDevice(ctx context.Context, ami string) (string, error) {
	p := url.Values{}
	p.Set("ImageId.1", ami)
	b, err := a.api.ec2(ctx, "DescribeImages", p)
	if err != nil {
		return "", fmt.Errorf("describe AMI %s: %w", ami, err)
	}
	if dev := xmlFirst(b, "imagesSet>item>rootDeviceName"); dev != "" {
		return dev, nil
	}
	return "", fmt.Errorf("AMI %s not found in %s", ami, a.r.Region)
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
			if id, err = find(); err != nil {
				return nil, err
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
	return "", fmt.Errorf("no default VPC in %s; set subnet_id (and optionally security_group_ids) on the runner", a.r.Region)
}

// Terminate ends one specific instance, but only if it carries our tags.
func (a *AWSSpot) Terminate(ctx context.Context, id string) error {
	// Looked up by ID (tag-filtered listings lag for brand-new instances);
	// the owner and runner tags are still checked before terminating.
	err := a.terminateAny(ctx, id)
	if err == nil {
		_ = os.Remove(a.knownHosts(id))
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
	insts, err := a.describe(ctx, "pending", "running", "stopping", "stopped")
	if err != nil {
		return err
	}
	if len(insts) > 0 {
		p := url.Values{}
		for i, inst := range insts {
			p.Set("InstanceId."+strconv.Itoa(i+1), inst.InstanceID)
		}
		if _, err := a.api.ec2(ctx, "TerminateInstances", p); err != nil {
			return fmt.Errorf("terminate: %w", err)
		}
		for _, inst := range insts {
			log("terminating %s (%s)", inst.InstanceID, inst.InstanceType)
			_ = os.Remove(a.knownHosts(inst.InstanceID))
		}
	} else {
		log("no running instances for %s", a.r.Name)
	}
	return nil
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
	qb, _ := json.Marshal(map[string]string{"ServiceCode": "ec2", "QuotaCode": "L-34B43A08"})
	if status, b, err := a.api.do(ctx, "servicequotas", "application/x-amz-json-1.1", map[string]string{"X-Amz-Target": "ServiceQuotasV20190624.GetServiceQuota"}, qb); err == nil && status == 200 {
		var v struct{ Quota struct{ Value float64 } }
		_ = json.Unmarshal(b, &v)
		need := a.r.MaxInstances * vcpusOf(a.r.InstanceTypes[0])
		msg := fmt.Sprintf("%.0f vCPUs for standard Spot instances in %s; this runner may use up to %d", v.Quota.Value, a.r.Region, need)
		out = append(out, Check{Name: "spot vCPU quota", OK: v.Quota.Value >= float64(need), Msg: msg})
	}
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

// vcpusOf estimates vCPUs from an EC2 size suffix (large=2, xlarge=4, NxNlarge=4N).
// VCPUsOf estimates an EC2 instance type's vCPUs from its size name.
func VCPUsOf(t string) int { return vcpusOf(t) }

func vcpusOf(t string) int {
	_, size, _ := strings.Cut(t, ".")
	switch size {
	case "nano", "micro", "small":
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
	if n, err := strconv.Atoi(strings.TrimSuffix(size, "xlarge")); err == nil {
		return 4 * n
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
