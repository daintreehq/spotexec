package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileRegion(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte(`[default]
region = us-west-2

[profile work]
region = eu-west-1
output = json

[profile corp]
sso_session = corp
sso_account_id = 123456789012
sso_role_name = Admin
region = ap-southeast-2

[sso-session corp]
sso_region = us-east-1
sso_start_url = https://example.awsapps.com/start

[profile bare]
output = json
`), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	for _, c := range []struct {
		profile, env, want, from string
	}{
		{"", "", "us-west-2", `profile "default"`},
		{"work", "", "eu-west-1", `profile "work"`},
		{"corp", "", "ap-southeast-2", `profile "corp"`}, // SSO: its region, not sso_region
		{"", "work", "eu-west-1", `profile "work"`},      // AWS_PROFILE
		{"corp", "work", "ap-southeast-2", `"corp"`},     // the explicit profile wins
		{"bare", "", "", "sets no region"},
		{"missing", "", "", `no profile "missing"`},
	} {
		t.Setenv("AWS_PROFILE", c.env)
		r, from := ProfileRegion(c.profile)
		if r != c.want || !strings.Contains(from, c.from) {
			t.Errorf("ProfileRegion(%q, AWS_PROFILE=%q) = %q, %q; want %q from %s", c.profile, c.env, r, from, c.want, c.from)
		}
	}
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_REGION", "ca-central-1")
	if r, from := ProfileRegion("work"); r != "ca-central-1" || from != "AWS_DEFAULT_REGION" {
		t.Errorf("env region: %q %q", r, from)
	}
	t.Setenv("AWS_REGION", "sa-east-1")
	if r, from := ProfileRegion("work"); r != "sa-east-1" || from != "AWS_REGION" {
		t.Errorf("env region: %q %q", r, from)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "nope"))
	if r, from := ProfileRegion(""); r != "" || !strings.Contains(from, "no ") {
		t.Errorf("no config: %q %q", r, from)
	}

	// A config path written with ~ (as the AWS CLI accepts).
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".aws"), 0o700)
	os.WriteFile(filepath.Join(home, ".aws", "work-config"), []byte("[profile \"work\"]\nregion = eu-north-1\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", "~/.aws/work-config")
	if r, from := ProfileRegion("work"); r != "eu-north-1" {
		t.Errorf("~ path, quoted profile: %q %q", r, from)
	}
	// The credentials file's region wins over the config file's.
	os.WriteFile(filepath.Join(dir, "credentials"), []byte("[work]\naws_access_key_id = x\nregion = me-south-1\n"), 0o600)
	if r, from := ProfileRegion("work"); r != "me-south-1" || !strings.Contains(from, "credentials") {
		t.Errorf("credentials file region: %q %q", r, from)
	}
	// An unreadable config file is reported as such.
	os.Remove(filepath.Join(dir, "credentials"))
	t.Setenv("AWS_CONFIG_FILE", dir)
	if r, from := ProfileRegion("work"); r != "" || !strings.Contains(from, "cannot read") {
		t.Errorf("unreadable config: %q %q", r, from)
	}
}

// AWS_SHARED_CREDENTIALS_FILE and AWS_CONFIG_FILE expand ~ and $VARS
// wherever they are read or written, as the AWS CLI does: setup must not
// write a key to a literal ./~/.aws/credentials.
func TestAWSFileEnvExpanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SPOTEXEC_TEST_AWS", filepath.Join(home, "elsewhere"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "~/.aws/credentials")
	t.Setenv("AWS_CONFIG_FILE", "$SPOTEXEC_TEST_AWS/config")
	wantCreds := filepath.Join(home, ".aws", "credentials")
	wantCfg := filepath.Join(home, "elsewhere", "config")
	if got := sharedCredentialsPath(); got != wantCreds {
		t.Errorf("sharedCredentialsPath = %q, want %q", got, wantCreds)
	}
	if got := awsCredentialsFile(); got != wantCreds {
		t.Errorf("awsCredentialsFile = %q, want %q", got, wantCreds)
	}
	if got, err := awsConfigPath(); err != nil || got != wantCfg {
		t.Errorf("awsConfigPath = %q, %v; want %q", got, err, wantCfg)
	}
	if got := awsConfigFile(); got != wantCfg {
		t.Errorf("awsConfigFile = %q, want %q", got, wantCfg)
	}
	os.MkdirAll(filepath.Dir(wantCreds), 0o700)
	os.WriteFile(wantCreds, []byte("[work]\naws_access_key_id = AK\naws_secret_access_key = s\nregion = eu-west-3\n"), 0o600)
	if c, err := readSharedCredentials("work"); err != nil || c.AccessKeyID != "AK" {
		t.Errorf("readSharedCredentials = %+v, %v", c, err)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if r, _ := ProfileRegion("work"); r != "eu-west-3" {
		t.Errorf("ProfileRegion = %q", r)
	}
}

// Account-wide calls go to the partition's home: the regions' partition,
// else the admin profile's, else the commercial one.
func TestAccountRegion(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte("[profile gov]\nregion = us-gov-east-1\n\n[profile work]\nregion = eu-west-1\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	for _, c := range []struct {
		profile string
		regions []string
		want    string
	}{
		{"work", []string{"eu-west-1", "us-east-2"}, "us-east-1"},
		{"work", []string{"us-gov-east-1"}, "us-gov-west-1"},
		{"gov", []string{"us-gov-west-1", "us-gov-east-1"}, "us-gov-west-1"},
		{"gov", nil, "us-gov-west-1"},
		{"work", nil, "us-east-1"},
		{"missing", nil, "us-east-1"},
	} {
		if got, err := AccountRegion(c.profile, c.regions); err != nil || got != c.want {
			t.Errorf("AccountRegion(%q, %v) = %q, %v; want %q", c.profile, c.regions, got, err, c.want)
		}
	}
	if _, err := AccountRegion("work", []string{"us-east-2", "us-gov-west-1"}); err == nil || !strings.Contains(err.Error(), "different AWS partitions") {
		t.Errorf("mixed partitions: %v", err)
	}
}

// IAM has one endpoint per partition, signed for the partition's home
// region whichever region the client is for; other services stay regional.
func TestIAMEndpointAndSigningRegion(t *testing.T) {
	for _, c := range []struct{ region, iam, signed string }{
		{"us-east-1", "https://iam.amazonaws.com/", "us-east-1"},
		{"eu-west-1", "https://iam.amazonaws.com/", "us-east-1"},
		{"us-gov-east-1", "https://iam.us-gov.amazonaws.com/", "us-gov-west-1"},
		{"us-gov-west-1", "https://iam.us-gov.amazonaws.com/", "us-gov-west-1"},
	} {
		cl := accountClient(c.region, nil)
		if got := cl.endpoint("iam"); got != c.iam {
			t.Errorf("%s: IAM endpoint %s, want %s", c.region, got, c.iam)
		}
		if got := cl.signingRegion("iam"); got != c.signed {
			t.Errorf("%s: IAM signed for %s, want %s", c.region, got, c.signed)
		}
		if got := cl.endpoint("sts"); got != "https://sts."+c.region+".amazonaws.com/" || cl.signingRegion("sts") != c.region {
			t.Errorf("%s: STS %s signed for %s", c.region, got, cl.signingRegion("sts"))
		}
	}
	if got := (&AWSUserSetup{}).client("us-gov-west-1", nil).endpoint("iam"); got != "https://iam.us-gov.amazonaws.com/" {
		t.Errorf("setup's IAM endpoint %s", got)
	}
}
