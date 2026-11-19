package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Malformed sso_region values: ones that break URL parsing, and ones that
// would move the endpoint (and the bearer or refresh token) off AWS.
var malformedSSORegions = []string{
	"us east 1",
	"%zz",
	"evil.example/",
	"us-east-1.evil.example#",
	"evil.example?",
	"US-EAST-1",
	"us-east-1:8443",
	"us-" + strings.Repeat("a", 60) + "-1", // well-formed, but too long for a DNS label
}

// noNetwork fails the test if any request leaves the process through the
// default transport, which the SSO clients use.
func noNetwork(t *testing.T) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, errors.New("network disabled in test")
	})
	t.Cleanup(func() { http.DefaultTransport = old })
}

// writeSSOCache writes a synthetic `aws sso login` token cache entry for key
// under $HOME/.aws/sso/cache.
func writeSSOCache(t *testing.T, home, key string, entry map[string]string) {
	t.Helper()
	sum := sha1.Sum([]byte(key))
	dir := filepath.Join(home, ".aws", "sso", "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Role credential lookup with a valid cached token: a malformed sso_region,
// set on the profile or on its sso-session, is a configuration error naming
// the profile, never a panic or a request to a non-AWS host.
func TestConfigCredentialsRejectsMalformedSSORegion(t *testing.T) {
	noNetwork(t)
	fresh := map[string]string{"accessToken": "tok", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	for _, region := range malformedSSORegions {
		for _, viaSession := range []bool{false, true} {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "config")
			var body, key string
			if viaSession {
				body = "[profile dev]\nsso_session = corp\nsso_account_id = 123456789012\nsso_role_name = Dev\n" +
					"[sso-session corp]\nsso_region = " + region + "\nsso_start_url = https://example.awsapps.com/start\n"
				key = "corp"
			} else {
				body = "[profile dev]\nsso_start_url = https://example.awsapps.com/start\nsso_region = " + region +
					"\nsso_account_id = 123456789012\nsso_role_name = Dev\n"
				key = "https://example.awsapps.com/start"
			}
			if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AWS_CONFIG_FILE", cfg)
			t.Setenv("HOME", dir)
			writeSSOCache(t, dir, key, fresh)
			_, err := configCredentials(context.Background(), "dev")
			if err == nil || !strings.Contains(err.Error(), `profile "dev"`) || !strings.Contains(err.Error(), "sso_region") {
				t.Errorf("region %q (sso-session %v): want a profile-specific sso_region error, got %v", region, viaSession, err)
			}
			if locks, _ := filepath.Glob(filepath.Join(dir, ".aws", "sso", "cache", "*.spotexec-lock")); len(locks) > 0 {
				t.Errorf("region %q: rejected before the token cache, but a lock was created: %v", region, locks)
			}
		}
	}
}

// Token refresh with an expired cached token: a malformed region is rejected
// before the refresh token is sent anywhere.
func TestSSOTokenRefreshRejectsMalformedRegion(t *testing.T) {
	noNetwork(t)
	for _, region := range malformedSSORegions {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		writeSSOCache(t, dir, "corp", map[string]string{
			"accessToken": "old", "expiresAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"refreshToken": "refresh", "clientId": "cid", "clientSecret": "secret",
			"registrationExpiresAt": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		})
		_, err := ssoToken(context.Background(), "corp", region, "dev")
		if err == nil || !strings.Contains(err.Error(), `profile "dev"`) || !strings.Contains(err.Error(), "sso_region") {
			t.Errorf("region %q: want a profile-specific sso_region error, got %v", region, err)
		}
	}
}

// Legitimate regions, including other partitions' names, still build their
// endpoints.
func TestSSOEndpointAcceptsRegions(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-central-2", "ap-southeast-4", "us-gov-west-1", "il-central-1"} {
		u, err := ssoEndpoint("oidc", region, "/token", "dev")
		if err != nil || u != "https://oidc."+region+".amazonaws.com/token" {
			t.Errorf("region %q: got %q, %v", region, u, err)
		}
	}
}

// Partitions whose SSO endpoints are not under amazonaws.com get an explicit
// unsupported error rather than a misleading "invalid" or a DNS failure.
func TestSSOEndpointRejectsOtherPartitions(t *testing.T) {
	for _, region := range []string{"cn-north-1", "cn-northwest-1", "eusc-de-east-1", "us-iso-east-1", "us-isob-east-1", "eu-isoe-west-1"} {
		if _, err := ssoEndpoint("portal.sso", region, "/federation/credentials", "dev"); err == nil || !strings.Contains(err.Error(), "AWS CLI") {
			t.Errorf("region %q: want an unsupported-partition error, got %v", region, err)
		}
	}
}
