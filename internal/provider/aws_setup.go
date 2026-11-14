package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const iamAPIVersion = "2010-05-08"

// SpotexecPolicy is the least-privilege IAM policy spotexec needs. The
// docs/aws.md "Grant permissions" section shows the same document (a test
// keeps them in step).
const SpotexecPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": [
        "ec2:RunInstances", "ec2:CreateTags", "ec2:DescribeInstances", "ec2:TerminateInstances",
        "ec2:DescribeImages", "ec2:DescribeVpcs", "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups", "ec2:CreateSecurityGroup", "ec2:AuthorizeSecurityGroupIngress"
      ], "Resource": "*" },
    { "Effect": "Allow", "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:*::parameter/aws/service/canonical/*" },
    { "Effect": "Allow", "Action": "iam:CreateServiceLinkedRole", "Resource": "*",
      "Condition": { "StringEquals": { "iam:AWSServiceName": "spot.amazonaws.com" } } },
    { "Effect": "Allow", "Action": [
        "s3:CreateBucket", "s3:PutLifecycleConfiguration", "s3:ListBucket",
        "s3:GetObject", "s3:PutObject"
      ], "Resource": ["arn:aws:s3:::spotexec-cache-*", "arn:aws:s3:::spotexec-cache-*/*"] },
    { "Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*" },
    { "Effect": "Allow", "Action": [
        "ecr:DescribeRepositories", "ecr:CreateRepository", "ecr:TagResource", "ecr:PutLifecyclePolicy",
        "ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
        "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"
      ], "Resource": "arn:aws:ecr:*:*:repository/spotexec/*" },
    { "Effect": "Allow", "Action": "servicequotas:GetServiceQuota", "Resource": "*" },
    { "Effect": "Allow", "Action": [
        "ec2:StopInstances", "ec2:CreateImage", "ec2:DeregisterImage", "ec2:DeleteSnapshot"
      ], "Resource": "*" }
  ]
}`

// iamUserPath groups the users spotexec creates.
const iamUserPath = "/spotexec/"

// AWSUserSetup creates (or refreshes) a dedicated IAM user for spotexec and
// stores its access key as an AWS CLI profile, so spotexec keeps working
// without interactive sign-ins. It is idempotent.
type AWSUserSetup struct {
	AdminProfile string   // profile with IAM rights; "" = AWS_PROFILE / env / default
	Profile      string   // AWS CLI profile to write (e.g. "spotexec")
	UserName     string   // IAM user name; "" = keep the profile's spotexec user, else DefaultName
	DefaultName  string   // e.g. spotexec-<hostname>
	MachineID    string   // this machine's owner id ("" = read it); tags the user so machines never share one
	Region       string   // region for the verification call and quota check
	Rotate       bool     // replace a working key with a new one
	VPCRegions   []string // regions to give a default VPC when they have none
	QuotaRegions []string // regions to report the Spot vCPU quota for (default: Region)
	Log          func(format string, args ...any)
	// Confirm is asked before anything is created in the account.
	Confirm func(plan AWSSetupPlan) bool

	endpoints      map[string]string // tests
	resolveProfile func(ctx context.Context, profile string) (awsCreds, error)
	pollEvery      time.Duration
}

// AWSSetupPlan is what setup is about to do, for confirmation.
type AWSSetupPlan struct {
	Account, AdminARN string
	UserName          string
	CreateVPCIn       []string // regions without a default VPC; setup will create one
}

// AWSUserSetupResult reports what setup did.
type AWSUserSetupResult struct {
	Account      string
	UserARN      string
	AccessKeyID  string
	KeyCreated   bool
	KeysDeleted  []string
	PolicyStale  bool               // the policy could not be refreshed (no admin session)
	Quotas       map[string]float64 // Spot vCPU quota by region (missing when unknown)
	CredsFile    string
	ConfigFile   string
	AlreadyReady bool              // a working key for the user was already in the profile
	CreatedVPCs  map[string]string // default VPCs setup created, by region
	NoVPC        []string          // regions still without a default VPC
}

// ErrSetupDeclined is returned when Confirm says no.
var ErrSetupDeclined = errors.New("setup cancelled")

func (s *AWSUserSetup) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

func (s *AWSUserSetup) client(region string, creds *awsCredResolver) *awsClient {
	ep := map[string]string{"iam": "https://iam.amazonaws.com/"}
	for k, v := range s.endpoints {
		ep[k] = v
	}
	return &awsClient{region: region, creds: creds, http: &http.Client{Timeout: 60 * time.Second}, endpoints: ep}
}

func staticResolver(c awsCreds) *awsCredResolver { return &awsCredResolver{fixed: &c} }

// Run performs the setup.
func (s *AWSUserSetup) Run(ctx context.Context) (*AWSUserSetupResult, error) {
	if s.Profile == "" || (s.UserName == "" && s.DefaultName == "") || s.Region == "" {
		return nil, errors.New("aws setup: profile, user name and region are required")
	}
	for _, n := range []string{s.UserName, s.DefaultName} {
		if n != "" && !validIAMName.MatchString(n) {
			return nil, fmt.Errorf("%q is not a valid IAM user name (letters, digits and +=,.@_- only, at most 64)", n)
		}
	}
	machine := s.MachineID
	if machine == "" {
		var err error
		if machine, err = ownerID(); err != nil {
			return nil, err
		}
	}
	name := s.UserName
	if s.resolveProfile == nil {
		s.resolveProfile = func(ctx context.Context, p string) (awsCreds, error) {
			return (&awsCredResolver{profile: p}).resolve(ctx)
		}
	}
	if s.pollEvery == 0 {
		s.pollEvery = 3 * time.Second
	}
	res := &AWSUserSetupResult{Quotas: map[string]float64{}, CreatedVPCs: map[string]string{}, CredsFile: awsCredentialsFile(), ConfigFile: awsConfigFile()}
	if err := checkProfileIsPlain(res.ConfigFile, s.Profile); err != nil {
		return nil, err
	}

	// Does the profile already hold a working key for a spotexec user? Then
	// keep that user, even if this machine's hostname has changed since.
	var existing *awsCreds
	if c, err := s.resolveProfile(ctx, s.Profile); err == nil {
		if acct, arn, err := callerIdentity(ctx, s.client(s.Region, staticResolver(c))); err == nil {
			cur := ""
			if i := strings.Index(arn, ":user"+iamUserPath); i >= 0 {
				cur = arn[i+len(":user"+iamUserPath):]
			}
			if cur != "" && (name == "" || cur == name) {
				existing, name = &c, cur
				res.Account, res.UserARN, res.AccessKeyID = acct, arn, c.AccessKeyID
			} else {
				s.logf("profile %s currently signs in as %s; it will be replaced", s.Profile, arn)
			}
		}
	}
	if name == "" {
		name = s.DefaultName
	}

	admin := s.client("us-east-1", &awsCredResolver{profile: s.AdminProfile})
	acct, adminARN, adminErr := callerIdentity(ctx, admin)
	if adminErr != nil {
		if existing != nil && !s.Rotate {
			// Already set up; without an admin session the policy just
			// can't be refreshed.
			res.AlreadyReady, res.PolicyStale = true, true
			s.quota(ctx, *existing, res)
			return res, nil
		}
		return nil, fmt.Errorf("spotexec setup needs an AWS session that can manage IAM users, once: %w", adminErr)
	}
	if existing != nil && strings.HasSuffix(adminARN, ":user"+iamUserPath+name) {
		// The "admin" identity is the spotexec user itself (AWS_PROFILE or
		// default points at it): it can't manage IAM, and needn't.
		res.AlreadyReady, res.PolicyStale = true, true
		if s.Rotate {
			return nil, errors.New("rotating the key needs an admin session: pass --admin-profile (or set AWS_PROFILE) to a profile that can manage IAM users")
		}
		s.quota(ctx, *existing, res)
		return res, nil
	}
	res.Account = acct
	plan := AWSSetupPlan{Account: acct, AdminARN: adminARN, UserName: name}
	for _, region := range s.VPCRegions {
		if b, err := s.client(region, admin.creds).ec2(ctx, "DescribeVpcs", url.Values{"Filter.1.Name": {"isDefault"}, "Filter.1.Value.1": {"true"}}); err == nil && xmlFirst(b, "vpcSet>item>vpcId") == "" {
			plan.CreateVPCIn = append(plan.CreateVPCIn, region)
		}
	}
	if s.Confirm != nil && !s.Confirm(plan) {
		return nil, ErrSetupDeclined
	}
	for _, region := range plan.CreateVPCIn {
		// AWS's standard default VPC: public subnets in every AZ, an internet
		// gateway and routes. It costs nothing by itself.
		if b, err := s.client(region, admin.creds).ec2(ctx, "CreateDefaultVpc", nil); err != nil {
			res.NoVPC = append(res.NoVPC, region)
			s.logf("warning: couldn't create a default VPC in %s (%v); set subnet_id on the runner instead", region, err)
		} else {
			res.CreatedVPCs[region] = xmlFirst(b, "vpc>vpcId")
			s.logf("created the default VPC %s in %s", res.CreatedVPCs[region], region)
		}
	}

	// 1. The user, under /spotexec/ and tagged with this machine's id. A
	// name another machine already uses (same hostname) gets a suffix, so
	// setting up one machine can never touch another's key.
	host, _ := os.Hostname()
	owned := false
	for attempt := 0; ; attempt++ {
		p := url.Values{"UserName": {name}, "Path": {iamUserPath}}
		for i, kv := range [][2]string{{"spotexec-managed", "true"}, {tagMachine, machine}, {"spotexec-host", strings.Split(host, ".")[0]}} {
			p.Set(fmt.Sprintf("Tags.member.%d.Key", i+1), kv[0])
			p.Set(fmt.Sprintf("Tags.member.%d.Value", i+1), kv[1])
		}
		b, err := admin.query(ctx, "iam", iamAPIVersion, "CreateUser", p)
		if err == nil {
			res.UserARN, owned = xmlFirst(b, "CreateUserResult>User>Arn"), true
			s.logf("created IAM user %s", name)
			break
		}
		if awsCode(err) != "EntityAlreadyExists" {
			return nil, iamHint(fmt.Errorf("creating IAM user %s: %w", name, err))
		}
		u, err := getUser(ctx, admin, name)
		if err != nil {
			return nil, fmt.Errorf("IAM user %s: %w", name, err)
		}
		res.UserARN = u.ARN
		if !strings.HasSuffix(u.ARN, ":user"+iamUserPath+name) {
			return nil, fmt.Errorf("IAM user %s already exists and wasn't created by spotexec (%s); pass --user to pick another name", name, u.ARN)
		}
		tag := u.Tags[tagMachine]
		switch {
		case tag == machine:
			owned = true
		case tag == "" && existing != nil:
			// Made by an older spotexec; this machine's profile signs in as
			// it, so it's ours: label it.
			if _, err := admin.query(ctx, "iam", iamAPIVersion, "TagUser", url.Values{"UserName": {name},
				"Tags.member.1.Key": {tagMachine}, "Tags.member.1.Value": {machine}}); err == nil {
				owned = true
			}
		case existing == nil && s.UserName == "" && attempt == 0:
			// Another machine's user (or an unlabelled one): use our own.
			other := name
			name = suffixName(s.DefaultName, machine)
			plan.UserName = name
			s.logf("IAM user %s belongs to another machine; using %s", other, name)
			continue
		case existing == nil:
			return nil, fmt.Errorf("IAM user %s belongs to another machine (host %s); pass a different --user", name, orDash(u.Tags["spotexec-host"]))
		}
		s.logf("IAM user %s exists", name)
		break
	}

	// 2. Its policy (inline, so re-running replaces it with this version's).
	if _, err := admin.query(ctx, "iam", iamAPIVersion, "PutUserPolicy", url.Values{
		"UserName": {name}, "PolicyName": {"spotexec"}, "PolicyDocument": {compactJSON(SpotexecPolicy)},
	}); err != nil {
		return nil, iamHint(fmt.Errorf("attaching the spotexec policy: %w", err))
	}
	s.logf("attached the spotexec policy")

	// 3. The Spot service-linked role (a one-time account prerequisite).
	if _, err := admin.query(ctx, "iam", iamAPIVersion, "CreateServiceLinkedRole", url.Values{"AWSServiceName": {"spot.amazonaws.com"}}); err == nil {
		s.logf("created the EC2 Spot service-linked role")
	} else if !strings.Contains(err.Error(), "has been taken") {
		s.logf("warning: couldn't create the EC2 Spot service-linked role (%v); the first Spot launch will try again", err)
	}

	if existing != nil && !s.Rotate {
		res.AlreadyReady = true
		s.logf("profile %s already has a working key (%s)", s.Profile, existing.AccessKeyID)
		s.quota(ctx, *existing, res)
		return res, nil
	}

	// 4. A new access key. The user belongs to this machine, so keys not in
	// the local profile are stale; make room if both slots are used.
	keys, err := listAccessKeys(ctx, admin, name)
	if err != nil {
		return nil, err
	}
	if len(keys) >= 2 && !owned {
		return nil, fmt.Errorf("IAM user %s already has two access keys and isn't labelled as this machine's, so setup won't delete one; delete one in the IAM console or pass a different --user", name)
	}
	if len(keys) >= 2 {
		old := keys[0] // oldest
		if err := deleteAccessKey(ctx, admin, name, old.ID); err != nil {
			return nil, err
		}
		res.KeysDeleted = append(res.KeysDeleted, old.ID)
		keys = keys[1:]
	}
	b, err := admin.query(ctx, "iam", iamAPIVersion, "CreateAccessKey", url.Values{"UserName": {name}})
	if err != nil {
		return nil, iamHint(fmt.Errorf("creating an access key: %w", err))
	}
	newKey := awsCreds{
		AccessKeyID:     xmlFirst(b, "CreateAccessKeyResult>AccessKey>AccessKeyId"),
		SecretAccessKey: xmlFirst(b, "CreateAccessKeyResult>AccessKey>SecretAccessKey"),
		Source:          "new access key",
	}
	if newKey.AccessKeyID == "" || newKey.SecretAccessKey == "" {
		return nil, errors.New("CreateAccessKey returned no key")
	}
	res.AccessKeyID, res.KeyCreated = newKey.AccessKeyID, true
	s.logf("created access key %s", newKey.AccessKeyID)

	// 5. Save it as the profile.
	if err := upsertINISection(res.CredsFile, s.Profile, [][2]string{
		{"aws_access_key_id", newKey.AccessKeyID}, {"aws_secret_access_key", newKey.SecretAccessKey},
	}, []string{"aws_session_token"}); err != nil {
		return nil, fmt.Errorf("saving the key to %s: %w (the key %s exists in IAM; re-run setup to replace it)", res.CredsFile, err, newKey.AccessKeyID)
	}
	if err := upsertINISection(res.ConfigFile, configSection(s.Profile), [][2]string{{"region", s.Region}}, nil); err != nil {
		return nil, fmt.Errorf("writing %s: %w", res.ConfigFile, err)
	}
	s.logf("saved it as AWS profile %s (%s)", s.Profile, res.CredsFile)

	// 6. Wait for IAM to propagate the key and policy, then check that the
	// profile resolves to this key (nothing in ~/.aws shadows it).
	c := s.client(s.Region, staticResolver(newKey))
	if err := s.poll(ctx, 90*time.Second, func() error {
		if _, _, err := callerIdentity(ctx, c); err != nil {
			return err
		}
		_, err := c.ec2(ctx, "DescribeInstances", url.Values{"MaxResults": {"5"}})
		return err
	}); err != nil {
		return nil, fmt.Errorf("the new key doesn't work yet: %w", err)
	}
	if got, err := s.resolveProfile(ctx, s.Profile); err != nil {
		return nil, fmt.Errorf("profile %s doesn't resolve after writing it: %w", s.Profile, err)
	} else if got.AccessKeyID != newKey.AccessKeyID {
		return nil, fmt.Errorf("profile %s resolves to key %s, not the new %s: check %s and %s for overriding settings", s.Profile, got.AccessKeyID, newKey.AccessKeyID, res.ConfigFile, res.CredsFile)
	}
	s.logf("verified: the key signs in and can reach EC2 in %s", s.Region)

	// 7. Retire the user's other keys: none of them is in use here.
	for _, k := range keys {
		if !owned {
			break // not provably this machine's user: leave its keys alone
		}
		if k.ID == newKey.AccessKeyID {
			continue
		}
		if err := deleteAccessKey(ctx, admin, name, k.ID); err != nil {
			s.logf("warning: couldn't delete old access key %s: %v", k.ID, err)
			continue
		}
		res.KeysDeleted = append(res.KeysDeleted, k.ID)
	}
	s.quota(ctx, newKey, res)
	return res, nil
}

func (s *AWSUserSetup) poll(ctx context.Context, limit time.Duration, f func() error) error {
	deadline := time.Now().Add(limit)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.pollEvery):
		}
	}
}

// quota records each region's Spot vCPU quota (best effort).
func (s *AWSUserSetup) quota(ctx context.Context, c awsCreds, res *AWSUserSetupResult) {
	regions := s.QuotaRegions
	if len(regions) == 0 {
		regions = []string{s.Region}
	}
	for _, region := range regions {
		cl := s.client(region, staticResolver(c))
		qb, _ := json.Marshal(map[string]string{"ServiceCode": "ec2", "QuotaCode": "L-34B43A08"})
		status, b, err := cl.do(ctx, "servicequotas", "application/x-amz-json-1.1", map[string]string{"X-Amz-Target": "ServiceQuotasV20190624.GetServiceQuota"}, qb)
		if err != nil || status != 200 {
			continue
		}
		var v struct{ Quota struct{ Value float64 } }
		if json.Unmarshal(b, &v) == nil {
			res.Quotas[region] = v.Quota.Value
		}
	}
}

func callerIdentity(ctx context.Context, c *awsClient) (account, arn string, err error) {
	b, err := c.query(ctx, "sts", stsAPIVersion, "GetCallerIdentity", nil)
	if err != nil {
		return "", "", err
	}
	return xmlFirst(b, "GetCallerIdentityResult>Account"), xmlFirst(b, "GetCallerIdentityResult>Arn"), nil
}

type accessKey struct {
	ID      string `xml:"AccessKeyId"`
	Status  string `xml:"Status"`
	Created string `xml:"CreateDate"`
}

// listAccessKeys returns the user's keys, oldest first.
func listAccessKeys(ctx context.Context, c *awsClient, user string) ([]accessKey, error) {
	b, err := c.query(ctx, "iam", iamAPIVersion, "ListAccessKeys", url.Values{"UserName": {user}})
	if err != nil {
		return nil, iamHint(fmt.Errorf("listing access keys: %w", err))
	}
	var v struct {
		Keys []accessKey `xml:"ListAccessKeysResult>AccessKeyMetadata>member"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	sort.Slice(v.Keys, func(i, j int) bool { return v.Keys[i].Created < v.Keys[j].Created })
	return v.Keys, nil
}

func deleteAccessKey(ctx context.Context, c *awsClient, user, id string) error {
	if _, err := c.query(ctx, "iam", iamAPIVersion, "DeleteAccessKey", url.Values{"UserName": {user}, "AccessKeyId": {id}}); err != nil {
		return fmt.Errorf("deleting old access key %s: %w", id, err)
	}
	return nil
}

func iamHint(err error) error {
	if c := awsCode(err); c == "AccessDenied" || c == "AccessDeniedException" {
		return fmt.Errorf("%w\nthis AWS identity can't manage IAM users: sign in as an administrator (or pass --admin-profile), or follow the manual steps in https://github.com/daintreehq/spotexec/blob/main/docs/aws.md", err)
	}
	return err
}

var validIAMName = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)

func compactJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func awsConfigFile() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "config")
}

func awsCredentialsFile() string { return sharedCredentialsPath() }

func configSection(profile string) string {
	if profile == "default" {
		return "default"
	}
	return "profile " + profile
}

// checkProfileIsPlain refuses a profile whose ~/.aws/config section signs in
// another way (SSO, a role, a process): the AWS CLI would prefer that over
// the stored key.
func checkProfileIsPlain(configFile, profile string) error {
	vals, err := readINISection(configFile, configSection(profile))
	if err != nil {
		return nil
	}
	for k := range vals {
		for _, bad := range []string{"sso_", "role_arn", "credential_process", "login_session", "web_identity_token_file", "source_profile"} {
			if strings.HasPrefix(k, bad) {
				return fmt.Errorf("AWS profile %q in %s already signs in another way (%s); pass --profile with a new name", profile, configFile, k)
			}
		}
	}
	return nil
}

func readINISection(path, section string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vals := map[string]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if name, ok := iniHeader(line); ok {
			in = name == section
			continue
		}
		if !in || line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return vals, sc.Err()
}

func iniHeader(line string) (string, bool) {
	if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
		return strings.Join(strings.Fields(line[1:len(line)-1]), " "), true
	}
	return "", false
}

// upsertINISection sets keys in [section] of an AWS-style INI file, keeping
// everything else (other sections, comments, unrelated keys) as it was, and
// removes the keys in drop. The file is replaced atomically with mode 0600.
func upsertINISection(path, section string, set [][2]string, drop []string) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // keep a symlinked dotfile a symlink
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		lines = strings.Split(strings.TrimRight(s, "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	start, end := -1, len(lines)
	for i, l := range lines {
		name, ok := iniHeader(strings.TrimSpace(l))
		if !ok {
			continue
		}
		if start >= 0 {
			end = i
			break
		}
		if name == section {
			start = i
		}
	}
	keyOf := func(l string) string {
		t := strings.TrimSpace(l)
		if t == "" || t[0] == '#' || t[0] == ';' {
			return ""
		}
		k, _, ok := strings.Cut(t, "=")
		if !ok {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(k))
	}
	if start < 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "["+section+"]")
		for _, kv := range set {
			lines = append(lines, kv[0]+" = "+kv[1])
		}
	} else {
		body := append([]string(nil), lines[start+1:end]...)
		done := map[string]bool{}
		var out []string
		for _, l := range body {
			k := keyOf(l)
			skip := false
			for _, d := range drop {
				if k == d {
					skip = true
				}
			}
			for _, kv := range set {
				if k == kv[0] {
					if !done[k] {
						out = append(out, kv[0]+" = "+kv[1])
						done[k] = true
					}
					skip = true
				}
			}
			if !skip {
				out = append(out, l)
			}
		}
		// New keys go after the section's last non-blank line.
		last := len(out)
		for last > 0 && strings.TrimSpace(out[last-1]) == "" {
			last--
		}
		var add []string
		for _, kv := range set {
			if !done[kv[0]] {
				add = append(add, kv[0]+" = "+kv[1])
			}
		}
		out = append(append(append([]string(nil), out[:last]...), add...), out[last:]...)
		lines = append(append(append([]string(nil), lines[:start+1]...), out...), lines[end:]...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spotexec-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// AdminSession returns the current AWS sign-in for profile ("" = the
// default chain) and who it is, so setup can run on another machine with
// it. The credentials are usually short-lived session credentials.
func AdminSession(ctx context.Context, profile string) (env []string, arn string, err error) {
	r := &awsCredResolver{profile: profile}
	c, err := r.get(ctx)
	if err != nil {
		return nil, "", err
	}
	cl := (&AWSUserSetup{}).client("us-east-1", staticResolver(c))
	if _, arn, err = callerIdentity(ctx, cl); err != nil {
		return nil, "", err
	}
	return []string{c.AccessKeyID, c.SecretAccessKey, c.SessionToken}, arn, nil
}

const tagMachine = "spotexec-machine"

// MachineID is this machine's spotexec owner id.
func MachineID() (string, error) { return ownerID() }

type iamUser struct {
	ARN  string
	Tags map[string]string
}

func getUser(ctx context.Context, c *awsClient, name string) (iamUser, error) {
	b, err := c.query(ctx, "iam", iamAPIVersion, "GetUser", url.Values{"UserName": {name}})
	if err != nil {
		return iamUser{}, err
	}
	var v struct {
		ARN  string `xml:"GetUserResult>User>Arn"`
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"GetUserResult>User>Tags>member"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return iamUser{}, err
	}
	u := iamUser{ARN: v.ARN, Tags: map[string]string{}}
	for _, t := range v.Tags {
		u.Tags[t.Key] = t.Value
	}
	return u, nil
}

// suffixName makes a per-machine variant of name that fits IAM's 64 chars.
func suffixName(name, machine string) string {
	id := strings.ReplaceAll(machine, "-", "")
	if len(id) > 6 {
		id = id[:6]
	}
	if len(name) > 64-7 {
		name = name[:64-7]
	}
	return name + "-" + id
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Machine is one machine's spotexec IAM user, as seen from the account.
type Machine struct {
	User      string
	Host      string // from the spotexec-host tag
	MachineID string // from the spotexec-machine tag
	Created   string
	Keys      []MachineKey
	VMs       int // live spotexec instances this machine owns, in the regions asked about
	This      bool
}

// MachineKey is one access key and when it was last used.
type MachineKey struct {
	ID, Status, Created, LastUsed string
}

// ListMachines lists the IAM users under /spotexec/ with their keys and the
// spotexec VMs each machine has running in regions.
func ListMachines(ctx context.Context, adminProfile string, regions []string) ([]Machine, error) {
	s := &AWSUserSetup{}
	admin := s.client("us-east-1", &awsCredResolver{profile: adminProfile})
	b, err := admin.query(ctx, "iam", iamAPIVersion, "ListUsers", url.Values{"PathPrefix": {iamUserPath}, "MaxItems": {"1000"}})
	if err != nil {
		return nil, iamHint(fmt.Errorf("listing spotexec's IAM users: %w", err))
	}
	var v struct {
		Users []struct {
			Name    string `xml:"UserName"`
			Created string `xml:"CreateDate"`
		} `xml:"ListUsersResult>Users>member"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	me, _ := ownerID()
	vms := map[string]int{}
	for _, region := range regions {
		for _, in := range spotexecInstances(ctx, s.client(region, admin.creds)) {
			vms[in.tag(tagOwner)]++
		}
	}
	var out []Machine
	for _, u := range v.Users {
		m := Machine{User: u.Name, Created: u.Created}
		if gu, err := getUser(ctx, admin, u.Name); err == nil {
			m.Host, m.MachineID = gu.Tags["spotexec-host"], gu.Tags[tagMachine]
		}
		m.This = m.MachineID != "" && m.MachineID == me
		m.VMs = vms[m.MachineID]
		if keys, err := listAccessKeys(ctx, admin, u.Name); err == nil {
			for _, k := range keys {
				mk := MachineKey{ID: k.ID, Status: k.Status, Created: k.Created, LastUsed: "never"}
				if lb, err := admin.query(ctx, "iam", iamAPIVersion, "GetAccessKeyLastUsed", url.Values{"AccessKeyId": {k.ID}}); err == nil {
					if d := xmlFirst(lb, "GetAccessKeyLastUsedResult>AccessKeyLastUsed>LastUsedDate"); d != "" {
						mk.LastUsed = d
					}
				}
				m.Keys = append(m.Keys, mk)
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// spotexecInstances lists live spotexec instances in one region (any owner).
func spotexecInstances(ctx context.Context, c *awsClient) []ec2Instance {
	p := url.Values{"Filter.1.Name": {"tag-key"}, "Filter.1.Value.1": {tagRunner},
		"Filter.2.Name": {"instance-state-name"}, "Filter.2.Value.1": {"pending"}, "Filter.2.Value.2": {"running"}}
	var out []ec2Instance
	for page := 0; page < 20; page++ {
		b, err := c.ec2(ctx, "DescribeInstances", p)
		if err != nil {
			return out
		}
		ins, next, err := parseDescribeInstances(b)
		if err != nil {
			return out
		}
		out = append(out, ins...)
		if next == "" {
			break
		}
		p.Set("NextToken", next)
	}
	return out
}

// RevokeMachine deletes a machine's spotexec IAM user (its keys and policy
// first, so the key stops working at once) and terminates the VMs that
// machine owns in regions. user is the IAM user name.
func RevokeMachine(ctx context.Context, adminProfile, user string, regions []string, log func(string, ...any)) error {
	s := &AWSUserSetup{}
	admin := s.client("us-east-1", &awsCredResolver{profile: adminProfile})
	u, err := getUser(ctx, admin, user)
	if err != nil {
		return iamHint(fmt.Errorf("IAM user %s: %w", user, err))
	}
	if !strings.HasSuffix(u.ARN, ":user"+iamUserPath+user) {
		return fmt.Errorf("IAM user %s wasn't created by spotexec (%s); not touching it", user, u.ARN)
	}
	keys, err := listAccessKeys(ctx, admin, user)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := deleteAccessKey(ctx, admin, user, k.ID); err != nil {
			return err
		}
		log("deleted access key %s", k.ID)
	}
	if _, err := admin.query(ctx, "iam", iamAPIVersion, "DeleteUserPolicy", url.Values{"UserName": {user}, "PolicyName": {"spotexec"}}); err != nil && awsCode(err) != "NoSuchEntity" {
		return iamHint(fmt.Errorf("deleting %s's policy: %w", user, err))
	}
	if _, err := admin.query(ctx, "iam", iamAPIVersion, "DeleteUser", url.Values{"UserName": {user}}); err != nil {
		return iamHint(fmt.Errorf("deleting IAM user %s: %w", user, err))
	}
	log("deleted IAM user %s", user)
	if m := u.Tags[tagMachine]; m != "" {
		for _, region := range regions {
			c := s.client(region, admin.creds)
			for _, in := range spotexecInstances(ctx, c) {
				if in.tag(tagOwner) != m {
					continue
				}
				if _, err := c.ec2(ctx, "TerminateInstances", url.Values{"InstanceId.1": {in.InstanceID}}); err != nil {
					log("warning: couldn't terminate %s in %s: %v", in.InstanceID, region, err)
					continue
				}
				log("terminated %s (%s) in %s", in.InstanceID, in.tag(tagRunner), region)
			}
		}
	}
	return nil
}
