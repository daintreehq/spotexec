package provider

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
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
	"syscall"
	"time"
)

// Credential sources that work without the AWS CLI, read from
// ~/.aws/config: credential_process, and IAM Identity Center (SSO) profiles
// using the token cached by a previous `aws sso login` (on this machine).

var (
	errNoProfile         = errors.New("profile not found")
	errNoCredsConfigured = errors.New("no credential settings")
)

func awsConfigPath() (string, error) {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return expandAWSPath(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot locate ~/.aws/config: HOME is not set")
	}
	return filepath.Join(home, ".aws", "config"), nil
}

func awsHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("HOME is not set")
	}
	return filepath.Join(home, ".aws"), nil
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
		if name, ok := iniHeader(line); ok {
			cur = configSectionName(name)
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
		return nil, fmt.Errorf("%w: no [%s] in %s", errNoProfile, section, path)
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
	cfg, err := awsConfigPath()
	if err != nil {
		return awsCreds{}, err
	}
	vals, err := readINI(cfg, profileSection(profile))
	if errors.Is(err, errNoProfile) && profile == "default" {
		vals, err = readINI(cfg, "profile default") // common spelling
	}
	if err != nil {
		return awsCreds{}, err
	}
	if id, sec := vals["aws_access_key_id"], vals["aws_secret_access_key"]; id != "" && sec != "" {
		return awsCreds{AccessKeyID: id, SecretAccessKey: sec, SessionToken: vals["aws_session_token"], Source: cfg + " [" + profile + "]"}, nil
	}
	if cp := vals["credential_process"]; cp != "" {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "sh", "-c", cp)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return awsCreds{}, fmt.Errorf("credential_process: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		c, err := parseProcessCredentials(out)
		c.Source = "credential_process [" + profile + "]"
		return c, err
	}
	if sess := vals["login_session"]; sess != "" {
		return loginCacheCredentials(profile, sess)
	}
	account, role := vals["sso_account_id"], vals["sso_role_name"]
	if account == "" || role == "" {
		return awsCreds{}, fmt.Errorf("%w for profile %q", errNoCredsConfigured, profile)
	}
	region, startURL, cacheKey := vals["sso_region"], vals["sso_start_url"], ""
	if sess := vals["sso_session"]; sess != "" {
		sv, err := readINI(cfg, "sso-session "+sess)
		if err != nil {
			return awsCreds{}, fmt.Errorf("profile %q: sso_session %q: %v", profile, sess, err)
		}
		region, startURL, cacheKey = sv["sso_region"], sv["sso_start_url"], sess
	} else {
		cacheKey = startURL
	}
	if region == "" || cacheKey == "" || startURL == "" {
		return awsCreds{}, fmt.Errorf("profile %q: incomplete SSO configuration", profile)
	}
	tok, err := ssoToken(ctx, cacheKey, region, profile)
	if err != nil {
		return awsCreds{}, err
	}
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://portal.sso."+region+".amazonaws.com/federation/credentials?"+q.Encode(), nil)
	req.Header.Set("x-amz-sso_bearer_token", tok)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return awsCreds{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401:
		return awsCreds{}, fmt.Errorf("SSO session rejected; run `aws sso login --profile %s`", profile)
	case resp.StatusCode == 403:
		return awsCreds{}, fmt.Errorf("SSO: not allowed to assume role %s in account %s", role, account)
	case resp.StatusCode != 200:
		return awsCreds{}, fmt.Errorf("SSO GetRoleCredentials: HTTP %d", resp.StatusCode)
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
	if r.AccessKeyID == "" || r.SecretAccessKey == "" {
		return awsCreds{}, errors.New("SSO returned empty credentials")
	}
	return awsCreds{AccessKeyID: r.AccessKeyID, SecretAccessKey: r.SecretAccessKey, SessionToken: r.SessionToken,
		Expiration: time.UnixMilli(r.Expiration), Source: "SSO [" + profile + "]"}, nil
}

func parseExpiry(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "UTC") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "UTC")) + "Z"
	}
	return time.Parse(time.RFC3339, s)
}

// ssoToken returns a valid SSO access token from the CLI's cache,
// refreshing it with the cached refresh token when it has expired (the
// refresh token rotates, so the cache file is rewritten under a lock).
func ssoToken(ctx context.Context, cacheKey, region, profile string) (string, error) {
	home, err := awsHome()
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(cacheKey))
	path := filepath.Join(home, "sso", "cache", hex.EncodeToString(sum[:])+".json")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no cached SSO login; run `aws sso login --profile %s`", profile)
	}
	// Refresh tokens rotate: concurrent sx processes must not race. (The
	// native path only runs without the AWS CLI installed.)
	lf, err := os.OpenFile(path+".spotexec-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("SSO cache lock: %w", err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("SSO cache lock: %w", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no cached SSO login; run `aws sso login --profile %s`", profile)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return "", errors.New("unreadable SSO token cache")
	}
	str := func(k string) string { v, _ := raw[k].(string); return v }
	if exp, err := parseExpiry(str("expiresAt")); err == nil && time.Until(exp) > time.Minute && str("accessToken") != "" {
		return str("accessToken"), nil
	}
	// Expired: refresh if this login has a refresh token and a live client
	// registration (sso-session logins do).
	if str("refreshToken") == "" || str("clientId") == "" || str("clientSecret") == "" {
		return "", fmt.Errorf("SSO session expired; run `aws sso login --profile %s`", profile)
	}
	if reg, err := parseExpiry(str("registrationExpiresAt")); err == nil && time.Now().After(reg) {
		return "", fmt.Errorf("SSO client registration expired; run `aws sso login --profile %s`", profile)
	}
	body, _ := json.Marshal(map[string]string{"clientId": str("clientId"), "clientSecret": str("clientSecret"),
		"grantType": "refresh_token", "refreshToken": str("refreshToken")})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+region+".amazonaws.com/token", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("SSO token refresh failed (HTTP %d); run `aws sso login --profile %s`", resp.StatusCode, profile)
	}
	var t struct {
		AccessToken  string `json:"accessToken"`
		ExpiresIn    int    `json:"expiresIn"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil || t.AccessToken == "" {
		return "", errors.New("SSO token refresh returned no token")
	}
	raw["accessToken"] = t.AccessToken
	raw["expiresAt"] = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	if t.RefreshToken != "" {
		raw["refreshToken"] = t.RefreshToken
	}
	// The server already rotated the refresh token: failing to persist it
	// would strand every later refresh, so report it.
	nb, _ := json.Marshal(raw)
	f, err := os.CreateTemp(filepath.Dir(path), ".spotexec-sso-*")
	if err != nil {
		return "", fmt.Errorf("saving refreshed SSO token: %w", err)
	}
	if _, err := f.Write(nb); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("saving refreshed SSO token: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("saving refreshed SSO token: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("saving refreshed SSO token: %w", err)
	}
	return t.AccessToken, nil
}

// loginCacheCredentials reads credentials cached by `aws login` (AWS CLI
// 2.32+): short-lived (15 min) keys under ~/.aws/login/cache. Refreshing
// them needs the CLI; with it installed, export-credentials is used instead.
func loginCacheCredentials(profile, session string) (awsCreds, error) {
	dir := os.Getenv("AWS_LOGIN_CACHE_DIRECTORY")
	if dir == "" {
		home, err := awsHome()
		if err != nil {
			return awsCreds{}, err
		}
		dir = filepath.Join(home, "login", "cache")
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(session)))
	b, err := os.ReadFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".json"))
	if err != nil {
		return awsCreds{}, fmt.Errorf("no cached `aws login` session; run `aws login --profile %s`", profile)
	}
	var v struct {
		AccessToken struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			ExpiresAt       string `json:"expiresAt"`
		} `json:"accessToken"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.AccessToken.AccessKeyID == "" {
		return awsCreds{}, errors.New("unreadable `aws login` cache")
	}
	exp, err := parseExpiry(v.AccessToken.ExpiresAt)
	if err != nil || time.Until(exp) < time.Minute {
		return awsCreds{}, fmt.Errorf("`aws login` session expired; run `aws login --profile %s` (or install the AWS CLI, which refreshes it)", profile)
	}
	return awsCreds{AccessKeyID: v.AccessToken.AccessKeyID, SecretAccessKey: v.AccessToken.SecretAccessKey,
		SessionToken: v.AccessToken.SessionToken, Expiration: exp, Source: "aws login cache [" + profile + "]"}, nil
}
