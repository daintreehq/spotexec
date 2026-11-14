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
