package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// awsCredResolver resolves and caches credentials for one profile.
type awsCredResolver struct {
	profile string
	mu      sync.Mutex
	cached  *awsCreds
}

func (r *awsCredResolver) get(ctx context.Context) (awsCreds, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.cached; c != nil && (c.Expiration.IsZero() || time.Until(c.Expiration) > 5*time.Minute) {
		return *c, nil
	}
	c, err := r.resolve(ctx)
	if err != nil {
		// Refresh failed (e.g. SSO session just expired) but the cached
		// credentials are still valid for a while: keep using them.
		if old := r.cached; old != nil && time.Until(old.Expiration) > 30*time.Second {
			return *old, nil
		}
		return awsCreds{}, err
	}
	r.cached = &c
	return c, nil
}

// invalidate drops cached credentials (after ExpiredToken).
func (r *awsCredResolver) invalidate() {
	r.mu.Lock()
	r.cached = nil
	r.mu.Unlock()
}

func (r *awsCredResolver) profileName() string {
	if r.profile != "" {
		return r.profile
	}
	if p := os.Getenv("AWS_PROFILE"); p != "" {
		return p
	}
	return "default"
}

func (r *awsCredResolver) resolve(ctx context.Context) (awsCreds, error) {
	// 1. Environment (only when no explicit profile is configured).
	if r.profile == "" {
		if id, sec := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"); id != "" && sec != "" {
			return awsCreds{AccessKeyID: id, SecretAccessKey: sec, SessionToken: os.Getenv("AWS_SESSION_TOKEN"), Source: "environment"}, nil
		}
	}
	// 2. AWS CLI v2: handles SSO, assume-role, credential_process and static
	// keys. When it is installed its answer is authoritative: falling back to
	// parsing ~/.aws/credentials after, say, an expired SSO session could
	// silently pick up different (stale or other-account) keys.
	if path, err := exec.LookPath("aws"); err == nil {
		c, err := exportCLICredentials(ctx, path, r.profile, os.Getenv("AWS_PROFILE"))
		if err == nil {
			return c, nil
		}
		return awsCreds{}, fmt.Errorf("AWS credentials for profile %q: %v. Fix: run `aws sso login%s` (or `aws configure`), or set `profile = \"...\"` on the runner", r.profileName(), err, profileFlag(r.profileName()))
	}
	// 3. No aws CLI: static keys in the shared credentials file, then
	// credential_process / SSO profiles from ~/.aws/config.
	c, err := readSharedCredentials(r.profileName())
	if err == nil {
		return c, nil
	}
	if cc, cerr := configCredentials(ctx, r.profileName()); cerr == nil {
		return cc, nil
	} else if !strings.Contains(cerr.Error(), "no [") && !os.IsNotExist(cerr) {
		return awsCreds{}, cerr
	}
	msg := fmt.Sprintf("no AWS credentials for profile %q: %v", r.profileName(), err)
	return awsCreds{}, errors.New(msg + ". Fix: export AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, run `aws sso login" +
		profileFlag(r.profileName()) + "`, or set `profile = \"...\"` on the runner in ~/.config/spotexec/config.toml")
}

func profileFlag(p string) string {
	if p == "" || p == "default" {
		return ""
	}
	return " --profile " + p
}

func exportCLICredentials(ctx context.Context, awsPath, profile, envProfile string) (awsCreds, error) {
	args := []string{"configure", "export-credentials", "--format", "process"}
	p := profile
	if p == "" {
		p = envProfile
	}
	if p != "" {
		args = append(args, "--profile", p)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, awsPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return awsCreds{}, fmt.Errorf("aws %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return parseProcessCredentials(out)
}

func parseProcessCredentials(b []byte) (awsCreds, error) {
	var v struct {
		Version         int
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		SessionToken    string
		Expiration      string
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return awsCreds{}, fmt.Errorf("parse credential_process output: %w", err)
	}
	if v.AccessKeyID == "" || v.SecretAccessKey == "" {
		return awsCreds{}, errors.New("credential_process output missing AccessKeyId/SecretAccessKey")
	}
	c := awsCreds{AccessKeyID: v.AccessKeyID, SecretAccessKey: v.SecretAccessKey, SessionToken: v.SessionToken, Source: "aws configure export-credentials"}
	if v.Expiration != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05-0700", "2006-01-02 15:04:05Z07:00"} {
			if t, err := time.Parse(layout, v.Expiration); err == nil {
				c.Expiration = t
				break
			}
		}
		if c.Expiration.IsZero() {
			c.Expiration = time.Now().Add(10 * time.Minute) // unparseable: refresh soon
		}
	} else if v.SessionToken != "" {
		c.Expiration = time.Now().Add(15 * time.Minute) // temporary creds without expiry info
	}
	return c, nil
}

func sharedCredentialsPath() string {
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "credentials")
}

func readSharedCredentials(profile string) (awsCreds, error) {
	f, err := os.Open(sharedCredentialsPath())
	if err != nil {
		return awsCreds{}, err
	}
	defer f.Close()
	return parseSharedCredentials(bufio.NewScanner(f), profile, sharedCredentialsPath())
}

func parseSharedCredentials(sc *bufio.Scanner, profile, src string) (awsCreds, error) {
	section := ""
	vals := map[string]string{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimPrefix(strings.Trim(line, "[]"), "profile "))
			continue
		}
		if section != profile {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			vals[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return awsCreds{}, err
	}
	id, sec := vals["aws_access_key_id"], vals["aws_secret_access_key"]
	if id == "" || sec == "" {
		return awsCreds{}, fmt.Errorf("profile %q has no static keys in %s", profile, src)
	}
	return awsCreds{AccessKeyID: id, SecretAccessKey: sec, SessionToken: vals["aws_session_token"], Source: src + " [" + profile + "]"}, nil
}
