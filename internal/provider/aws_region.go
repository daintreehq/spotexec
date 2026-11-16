package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProfileRegion returns the region the AWS CLI would use for profile
// (empty: AWS_PROFILE, else "default"), found without the AWS CLI:
// AWS_REGION, AWS_DEFAULT_REGION, then the profile's region in the shared
// credentials file or the AWS config file (SSO, role and plain profiles
// alike). from says where the region came from, or, when region is empty,
// why none was found.
func ProfileRegion(profile string) (region, from string) {
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if v := os.Getenv(k); v != "" {
			return v, k
		}
	}
	if profile == "" {
		profile = os.Getenv("AWS_PROFILE")
	}
	if profile == "" {
		profile = "default"
	}
	// As in the AWS CLI, a profile's settings in the credentials file win
	// over those in the config file.
	if r, src := iniRegion(sharedCredentialsPath(), []string{profile, `"` + profile + `"`}, profile); r != "" {
		return r, src
	}
	cfg, err := awsConfigPath()
	if err != nil {
		return "", fmt.Sprintf("no AWS config file (%v)", err)
	}
	sections := []string{profileSection(profile), `profile "` + profile + `"`}
	if profile == "default" {
		sections = append(sections, "profile default") // common spelling
	}
	return iniRegion(cfg, sections, profile)
}

// iniRegion reads region from the first of sections found in path.
func iniRegion(path string, sections []string, profile string) (region, from string) {
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return "", fmt.Sprintf("cannot read %s: not a regular file", path)
	}
	for _, sec := range sections {
		vals, err := readINI(path, sec)
		switch {
		case errors.Is(err, errNoProfile):
			continue
		case errors.Is(err, os.ErrNotExist):
			return "", "no " + path
		case err != nil:
			return "", fmt.Sprintf("cannot read %s: %v", path, err)
		case vals["region"] == "":
			return "", fmt.Sprintf("profile %q in %s sets no region", profile, path)
		}
		return vals["region"], fmt.Sprintf("profile %q in %s", profile, path)
	}
	return "", fmt.Sprintf("no profile %q in %s", profile, path)
}

// expandAWSPath expands ~ and environment variables in an AWS_CONFIG_FILE
// or AWS_SHARED_CREDENTIALS_FILE value, as the AWS CLI does.
func expandAWSPath(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	return p
}
