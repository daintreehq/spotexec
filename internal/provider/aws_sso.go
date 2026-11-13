package provider

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Credential sources that work without the AWS CLI, read from
// ~/.aws/config: credential_process, and IAM Identity Center (SSO) profiles
// using the token cached by a previous `aws sso login` (on this machine).

func awsConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "config")
}

// readINI returns the key/values of one section ("profile x", "sso-session y", or "default").
func readINI(path, section string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vals := map[string]string{}
	cur := ""
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(strings.Trim(line, "[]"))
			if cur == section {
				found = true
			}
			continue
		}
		if cur != section {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if !found {
		return nil, fmt.Errorf("no [%s] in %s", section, path)
	}
	return vals, sc.Err()
}

func profileSection(profile string) string {
	if profile == "default" {
		return "default"
	}
	return "profile " + profile
}

// configCredentials resolves credential_process and SSO profiles from
// ~/.aws/config without the AWS CLI.
func configCredentials(ctx context.Context, profile string) (awsCreds, error) {
	vals, err := readINI(awsConfigPath(), profileSection(profile))
	if err != nil {
		return awsCreds{}, err
	}
	if cp := vals["credential_process"]; cp != "" {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "sh", "-c", cp).Output()
		if err != nil {
			return awsCreds{}, fmt.Errorf("credential_process: %w", err)
		}
		c, err := parseProcessCredentials(out)
		c.Source = "credential_process [" + profile + "]"
		return c, err
	}
	account, role := vals["sso_account_id"], vals["sso_role_name"]
	if account == "" || role == "" {
		return awsCreds{}, fmt.Errorf("profile %q has no credential_process or SSO settings", profile)
	}
	region, startURL, cacheKey := vals["sso_region"], vals["sso_start_url"], ""
	if sess := vals["sso_session"]; sess != "" {
		sv, err := readINI(awsConfigPath(), "sso-session "+sess)
		if err != nil {
			return awsCreds{}, err
		}
		region, startURL, cacheKey = sv["sso_region"], sv["sso_start_url"], sess
	} else {
		cacheKey = startURL
	}
	if region == "" || cacheKey == "" {
		return awsCreds{}, fmt.Errorf("profile %q: incomplete SSO configuration", profile)
	}
	sum := sha1.Sum([]byte(cacheKey))
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json"))
	if err != nil {
		return awsCreds{}, fmt.Errorf("no cached SSO login for %s; run `aws sso login --profile %s`", startURL, profile)
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if err := json.Unmarshal(b, &tok); err != nil || tok.AccessToken == "" {
		return awsCreds{}, errors.New("unreadable SSO token cache")
	}
	if t, err := time.Parse(time.RFC3339, tok.ExpiresAt); err == nil && time.Now().After(t) {
		return awsCreds{}, fmt.Errorf("SSO session expired; run `aws sso login --profile %s`", profile)
	}
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://portal.sso."+region+".amazonaws.com/federation/credentials?"+q.Encode(), nil)
	req.Header.Set("x-amz-sso_bearer_token", tok.AccessToken)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return awsCreds{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return awsCreds{}, fmt.Errorf("SSO GetRoleCredentials: HTTP %d (run `aws sso login --profile %s`)", resp.StatusCode, profile)
	}
	var rc struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"` // ms
		} `json:"roleCredentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rc); err != nil {
		return awsCreds{}, err
	}
	r := rc.RoleCredentials
	return awsCreds{AccessKeyID: r.AccessKeyID, SecretAccessKey: r.SecretAccessKey, SessionToken: r.SessionToken,
		Expiration: time.UnixMilli(r.Expiration), Source: "SSO [" + profile + "]"}, nil
}
