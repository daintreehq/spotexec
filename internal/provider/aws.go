package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spotexec/spotexec/internal/cloudinit"
	"github.com/spotexec/spotexec/internal/config"
)

const (
	tagRunner  = "spotexec-runner"
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
			log("waiting for %s (%s, %s)", inst.InstanceID, inst.InstanceType, inst.State)
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
	owner, err := a.ownerTag()
	if err != nil {
		return ec2Instance{}, err
	}
	pub, err := EnsureKey()
	if err != nil {
		return ec2Instance{}, fmt.Errorf("spotexec SSH key: %w", err)
	}
	ud, err := cloudinit.Render(cloudinit.Options{
		User: a.r.User, PublicKey: pub, IdleTimeout: a.r.IdleTimeout.Duration,
		MaxLifetime: a.r.MaxLifetime.Duration, BootstrapScript: a.r.BootstrapScript,
		UserBootstrap: a.r.UserBootstrap,
	})
	if err != nil {
		return ec2Instance{}, err
	}
	ami, err := a.resolveAMI(ctx)
	if err != nil {
		return ec2Instance{}, err
	}
	rootDev, err := a.rootDevice(ctx, ami)
	if err != nil {
		return ec2Instance{}, err
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
	base.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.Itoa(a.r.VolumeGB))
	base.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")
	tags := [][2]string{{tagRunner, a.r.Name}, {tagOwner, owner}, {"Name", "spotexec-" + a.r.Name}}
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
	var errs []string
	for _, spot := range markets {
		for _, itype := range a.r.InstanceTypes {
			inst, err := a.runOne(ctx, base, itype, spot, log)
			if err == nil {
				kind := "spot"
				if !spot {
					kind = "on-demand"
				}
				log("launched %s %s instance %s in %s", kind, itype, inst.InstanceID, a.r.Region)
				return inst, nil
			}
			if classifyAWSError(err) != errNextType {
				return ec2Instance{}, fmt.Errorf("launch %s: %w", itype, err)
			}
			log("skipping %s: %v", itype, err)
			errs = append(errs, itype+": "+awsCode(err))
		}
		if spot && a.r.OnDemandFallback {
			log("no spot capacity; falling back to on-demand")
		}
	}
	return ec2Instance{}, fmt.Errorf("no instance type could be launched in %s (%s); add more instance_types, raise max_price, or set on_demand_fallback = true", a.r.Region, strings.Join(errs, ", "))
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
	return ec2Instance{}, lastErr
}

func (a *AWSSpot) amiParam() string {
	return "/aws/service/canonical/ubuntu/server/24.04/stable/current/" + a.r.Arch + "/hvm/ebs-gp3/ami-id"
}

func (a *AWSSpot) resolveAMI(ctx context.Context) (string, error) {
	if a.r.AMI != "" {
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
		return xmlFirst(b, "securityGroupInfo>item>groupId"), nil
	}
	id, err := find()
	if err != nil {
		return nil, err
	}
	if id == "" {
		p := url.Values{}
		p.Set("GroupName", sgName)
		p.Set("GroupDescription", "spotexec runners: SSH ingress")
		p.Set("VpcId", vpc)
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
		return nil, fmt.Errorf("authorize SSH ingress on %s: %w", id, err)
	}
	return []string{id}, nil
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
	insts, err := a.describe(ctx, "pending", "running", "stopping", "stopped")
	if err != nil {
		return err
	}
	for _, i := range insts {
		if i.InstanceID == id {
			p := url.Values{}
			p.Set("InstanceId.1", id)
			_, err := a.api.ec2(ctx, "TerminateInstances", p)
			_ = os.Remove(a.knownHosts(id))
			return err
		}
	}
	return nil
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
	return out
}
