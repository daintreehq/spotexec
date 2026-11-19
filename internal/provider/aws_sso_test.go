package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Malformed sso_region values: ones that break URL parsing, ones that would
// move the endpoint (and the bearer or refresh token) off AWS, and ones that
// only look like an unsupported partition.
var malformedSSORegions = []string{
	"us east 1",
	"%zz",
	"evil.example/",
	"us-east-1.evil.example#",
	"evil.example?",
	"US-EAST-1",
	"us-east-1:8443",
	"cn-",
	"us-" + strings.Repeat("a", 59) + "-1", // well-formed, but 64 bytes: too long for a DNS label
}

// stubNetwork routes every request through the default transport, which the
// SSO clients use, to fn.
func stubNetwork(t *testing.T, fn func(*http.Request) (*http.Response, error)) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(fn)
	t.Cleanup(func() { http.DefaultTransport = old })
}

// noNetwork fails the test if any request leaves the process.
func noNetwork(t *testing.T) {
	t.Helper()
	stubNetwork(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, errors.New("network disabled in test")
	})
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func ssoCachePath(home, key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")
}

// writeSSOCache writes a synthetic `aws sso login` token cache entry for key
// under home.
func writeSSOCache(t *testing.T, home, key string, entry map[string]string) {
	t.Helper()
	p := ssoCachePath(home, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entry)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func noLock(t *testing.T, home, key string) {
	t.Helper()
	if _, err := os.Stat(ssoCachePath(home, key) + ".spotexec-lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rejected region still reached the token cache lock (stat: %v)", err)
	}
}

func freshToken(tok string) map[string]string {
	return map[string]string{"accessToken": tok, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
}

func expiredRefreshable() map[string]string {
	return map[string]string{
		"accessToken": "old", "expiresAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"refreshToken": "refresh", "clientId": "cid", "clientSecret": "secret",
		"registrationExpiresAt": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
}

const (
	profileStartURL = "https://profile.awsapps.com/start"
	sessionStartURL = "https://session.awsapps.com/start"
)

// ssoProfile writes ~/.aws/config for profile "dev" in a temp HOME and
// returns the config path, HOME, and the token cache key the profile uses.
// With viaSession, the region comes from [sso-session corp], and the profile's
// own sso_region and sso_start_url (which must be ignored) are set to values
// that would fail.
func ssoProfile(t *testing.T, region string, viaSession bool) (cfg, home, key string) {
	t.Helper()
	home = t.TempDir()
	cfg = filepath.Join(home, "config")
	body := "[profile dev]\nsso_account_id = 123456789012\nsso_role_name = Dev\n"
	if viaSession {
		body += "sso_session = corp\nsso_region = us east 1\nsso_start_url = " + profileStartURL + "\n" +
			"[sso-session corp]\nsso_region = " + region + "\nsso_start_url = " + sessionStartURL + "\n"
		key = "corp"
	} else {
		body += "sso_region = " + region + "\nsso_start_url = " + profileStartURL + "\n"
		key = profileStartURL
	}
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("HOME", home)
	return cfg, home, key
}

func wantRegionError(t *testing.T, err error, region string, extra ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("region %q: want an error", region)
	}
	for _, s := range append([]string{`profile "dev"`, fmt.Sprintf("sso_region %q", region)}, extra...) {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("region %q: error %q does not mention %q", region, err, s)
		}
	}
}

// Role credential lookup with a valid cached token: a malformed sso_region,
// on the profile or on its sso-session, is a configuration error naming the
// profile and the config file, raised before the token cache is touched;
// never a panic or a request to a non-AWS host.
func TestConfigCredentialsRejectsMalformedSSORegion(t *testing.T) {
	for _, region := range malformedSSORegions {
		for _, viaSession := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/session=%v", region, viaSession), func(t *testing.T) {
				noNetwork(t)
				cfg, home, key := ssoProfile(t, region, viaSession)
				writeSSOCache(t, home, key, freshToken("tok"))
				_, err := configCredentials(context.Background(), "dev")
				wantRegionError(t, err, region, cfg)
				noLock(t, home, key)
			})
		}
	}
}

// The positive control: a valid region sends the cached token to the region's
// portal endpoint, and an sso-session's region and start URL win over the
// profile's own.
func TestConfigCredentialsSSOLookup(t *testing.T) {
	for _, tc := range []struct {
		region     string
		viaSession bool
	}{{"eu-central-1", false}, {"ap-southeast-2", true}} {
		t.Run(tc.region, func(t *testing.T) {
			_, home, key := ssoProfile(t, tc.region, tc.viaSession)
			writeSSOCache(t, home, key, freshToken("tok-"+key))
			var got *http.Request
			stubNetwork(t, func(r *http.Request) (*http.Response, error) {
				got = r
				return jsonResponse(`{"roleCredentials":{"accessKeyId":"AKIDSSO","secretAccessKey":"sek","sessionToken":"st","expiration":4102444800000}}`), nil
			})
			c, err := configCredentials(context.Background(), "dev")
			if err != nil || c.AccessKeyID != "AKIDSSO" || c.SessionToken != "st" {
				t.Fatalf("lookup: %+v %v", c, err)
			}
			if got.Method != http.MethodGet || got.URL.Host != "portal.sso."+tc.region+".amazonaws.com" || got.URL.Path != "/federation/credentials" ||
				got.URL.Query().Get("account_id") != "123456789012" || got.URL.Query().Get("role_name") != "Dev" {
				t.Errorf("request: %s %s", got.Method, got.URL)
			}
			if tok := got.Header.Get("x-amz-sso_bearer_token"); tok != "tok-"+key {
				t.Errorf("bearer token %q, want the one cached under %q", tok, key)
			}
		})
	}
}

// ssoToken validates the region itself, whatever state the cache is in, and
// before taking the cache lock.
func TestSSOTokenRejectsMalformedRegion(t *testing.T) {
	caches := map[string]map[string]string{"expired": expiredRefreshable(), "fresh": freshToken("tok"), "none": nil}
	for _, region := range malformedSSORegions {
		for state, entry := range caches {
			t.Run(fmt.Sprintf("%q/%s", region, state), func(t *testing.T) {
				noNetwork(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				if entry != nil {
					writeSSOCache(t, home, "corp", entry)
				}
				_, err := ssoToken(context.Background(), "corp", region, "dev")
				wantRegionError(t, err, region)
				noLock(t, home, "corp")
			})
		}
	}
}

// The positive control: an expired token is refreshed against the region's
// OIDC endpoint and the rotated refresh token is saved.
func TestSSOTokenRefresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSSOCache(t, home, "corp", expiredRefreshable())
	stubNetwork(t, func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.String() != "https://oidc.us-west-2.amazonaws.com/token" || !strings.Contains(string(b), `"refreshToken":"refresh"`) {
			t.Errorf("request: %s %s %s", r.Method, r.URL, b)
		}
		return jsonResponse(`{"accessToken":"new","expiresIn":3600,"refreshToken":"rotated"}`), nil
	})
	tok, err := ssoToken(context.Background(), "corp", "us-west-2", "dev")
	if err != nil || tok != "new" {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	b, _ := os.ReadFile(ssoCachePath(home, "corp"))
	if !strings.Contains(string(b), `"refreshToken":"rotated"`) {
		t.Errorf("rotated refresh token not saved: %s", b)
	}
}

// A request that cannot be constructed is an error, not a nil request whose
// headers are then set. A nil context is the one way left to make the
// constructor fail once the endpoint is valid.
func TestSSORequestConstructionErrors(t *testing.T) {
	var nilCtx context.Context
	t.Run("lookup", func(t *testing.T) {
		noNetwork(t)
		_, home, key := ssoProfile(t, "us-east-1", false)
		writeSSOCache(t, home, key, freshToken("tok"))
		if _, err := configCredentials(nilCtx, "dev"); err == nil || !strings.Contains(err.Error(), `profile "dev": SSO GetRoleCredentials request`) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("refresh", func(t *testing.T) {
		noNetwork(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeSSOCache(t, home, "corp", expiredRefreshable())
		if _, err := ssoToken(nilCtx, "corp", "us-east-1", "dev"); err == nil || !strings.Contains(err.Error(), `profile "dev": SSO token refresh request`) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSSOEndpoint(t *testing.T) {
	long := "us-" + strings.Repeat("a", 58) + "-1" // 63 bytes: the longest DNS label
	for _, region := range []string{"us-east-1", "eu-central-2", "ap-southeast-4", "us-gov-west-1", "il-central-1", long} {
		for _, ep := range [][2]string{{"oidc", "/token"}, {"portal.sso", "/federation/credentials"}} {
			u, err := ssoEndpoint(ep[0], region, ep[1], "dev")
			if want := "https://" + ep[0] + "." + region + ".amazonaws.com" + ep[1]; err != nil || u != want {
				t.Errorf("region %q: got %q, %v; want %q", region, u, err, want)
			}
		}
	}
	// Partitions whose SSO endpoints are not under amazonaws.com get an
	// explicit unsupported error rather than a DNS failure.
	for _, region := range []string{"cn-north-1", "cn-northwest-1", "eusc-de-east-1", "us-iso-east-1", "us-isob-east-1", "eu-isoe-west-1"} {
		_, err := ssoEndpoint("portal.sso", region, "/federation/credentials", "dev")
		wantRegionError(t, err, region, "AWS CLI")
	}
	// A malformed name that merely starts like one of them is invalid, not
	// unsupported.
	for _, region := range []string{"cn-", "eusc-", "eusc-DE-1"} {
		_, err := ssoEndpoint("portal.sso", region, "/federation/credentials", "dev")
		wantRegionError(t, err, region, "invalid")
		if err != nil && strings.Contains(err.Error(), "AWS CLI") {
			t.Errorf("region %q: reported as unsupported: %v", region, err)
		}
	}
	// The built URL is checked too, not just the region.
	for _, ep := range [][2]string{{"oidc", "/%zz"}, {"oidc%25", "/token"}, {"oidc", "/%74oken"}, {"oidc", "/token?x"}} {
		if _, err := ssoEndpoint(ep[0], "us-east-1", ep[1], "dev"); err == nil || !strings.Contains(err.Error(), "does not form a valid SSO endpoint") {
			t.Errorf("service %q path %q: got %v", ep[0], ep[1], err)
		}
	}
}
