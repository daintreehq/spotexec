package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DepsCache is implemented by providers that can host a shared dependency
// cache: a fresh VM downloads an archive of the install instead of running
// it. URLs are handed to the VM, which never holds cloud credentials.
type DepsCache interface {
	CacheURLs(ctx context.Context, repoID, key string) (get, put string, err error)
}

// CacheURLs presigns GET and PUT URLs for s3://<bucket>/<repo>/<key>.tar,
// creating the cache bucket on first use. Disabled with cache_bucket = "off".
// Every request asserts the bucket belongs to this account
// (x-amz-expected-bucket-owner), so a same-named bucket created by someone
// else can never receive or serve our archives.
func (a *AWSSpot) CacheURLs(ctx context.Context, repoID, key string) (string, string, error) {
	bucket, acct, err := a.ensureBucket(ctx)
	if err != nil || bucket == "" {
		return "", "", err
	}
	creds, err := a.api.creds.get(ctx)
	if err != nil {
		return "", "", err
	}
	// The PUT happens after the install (up to deps.timeout): make sure the
	// signing credentials will still be valid then.
	if !creds.Expiration.IsZero() && time.Until(creds.Expiration) < 30*time.Minute {
		a.api.creds.invalidate()
		if c, err := a.api.creds.get(ctx); err == nil {
			creds = c
		}
	}
	obj := repoID + "/" + key + ".tar"
	ttl := time.Hour
	if !creds.Expiration.IsZero() {
		if left := time.Until(creds.Expiration) - time.Minute; left < ttl {
			ttl = left
		}
	}
	if ttl < 2*time.Minute {
		return "", "", fmt.Errorf("AWS credentials expire too soon to sign cache URLs")
	}
	now := time.Now()
	host := bucket + ".s3." + a.r.Region + ".amazonaws.com"
	get := presignS3Owner(creds, a.r.Region, host, obj, http.MethodGet, ttl, now, acct)
	put := presignS3Owner(creds, a.r.Region, host, obj, http.MethodPut, ttl, now, acct)
	return get, put, nil
}

// accountID returns (and caches) the AWS account of these credentials.
func (a *AWSSpot) accountID(ctx context.Context) (string, error) {
	// Keyed by the access key too: switching credentials (a different
	// account under the same profile name, or none) must not reuse it.
	keyID := ""
	if c, err := a.api.creds.get(ctx); err == nil {
		h := sha256.Sum256([]byte(c.AccessKeyID))
		keyID = hex.EncodeToString(h[:])[:12]
	}
	p := filepath.Join(a.dir, "account-id-"+a.r.Profile+"-"+keyID)
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) == 12 {
		return strings.TrimSpace(string(b)), nil
	}
	b, err := a.api.query(ctx, "sts", stsAPIVersion, "GetCallerIdentity", nil)
	if err != nil {
		return "", err
	}
	acct := xmlFirst(b, "GetCallerIdentityResult>Account")
	if len(acct) != 12 {
		return "", fmt.Errorf("could not determine AWS account id")
	}
	_ = os.MkdirAll(a.dir, 0o700)
	_ = os.WriteFile(p, []byte(acct+"\n"), 0o600)
	return acct, nil
}

// ensureBucket returns the cache bucket (and owning account), creating it
// (private, 14-day expiry) when it is the auto-named one. Setup is
// remembered per (account, region, bucket), and every check asserts
// ownership.
func (a *AWSSpot) ensureBucket(ctx context.Context) (string, string, error) {
	bucket, acct, auto, err := a.cacheBucket(ctx)
	if err != nil || bucket == "" {
		return "", "", err
	}
	owner := map[string]string{"x-amz-expected-bucket-owner": acct}
	marker := filepath.Join(a.dir, "cache-bucket-"+acct+"-"+a.r.Region+"-"+bucket)
	if _, err := os.Stat(marker); err == nil {
		return bucket, acct, nil
	}
	status, _, err := a.s3(ctx, http.MethodHead, bucket, "", nil, owner)
	if err != nil {
		return "", "", err
	}
	switch {
	case status == 200:
	case status == 404 && auto:
		var body []byte
		if a.r.Region != "us-east-1" {
			body = []byte(`<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>` + a.r.Region + `</LocationConstraint></CreateBucketConfiguration>`)
		}
		st, b, err := a.s3Settled(ctx, http.MethodPut, bucket, "", body, nil)
		if err != nil {
			return "", "", err
		}
		if st/100 != 2 && !bytes.Contains(b, []byte("BucketAlreadyOwnedByYou")) {
			return "", "", fmt.Errorf("creating cache bucket %s: HTTP %d: %s", bucket, st, firstLineOf(b))
		}
	case status == 404:
		return "", "", fmt.Errorf("cache bucket %s does not exist (create it, or use cache_bucket = \"auto\")", bucket)
	case status == 403:
		return "", "", fmt.Errorf("cache bucket %s is not owned by account %s or not accessible (set cache_bucket to another name, or \"off\")", bucket, acct)
	default:
		return "", "", fmt.Errorf("checking cache bucket %s: HTTP %d", bucket, status)
	}
	if auto {
		// (Re)apply the expiry rule until it has succeeded once.
		lifecycle := []byte(`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ID>spotexec-expire</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Expiration><Days>14</Days></Expiration><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`)
		sum := md5.Sum(lifecycle)
		hdr := map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:]), "x-amz-expected-bucket-owner": acct}
		st, b, err := a.s3Settled(ctx, http.MethodPut, bucket, "?lifecycle", lifecycle, hdr)
		if err != nil {
			return "", "", err
		}
		if st/100 != 2 {
			return "", "", fmt.Errorf("setting cache expiry on %s: HTTP %d: %s", bucket, st, firstLineOf(b))
		}
	}
	_ = os.MkdirAll(a.dir, 0o700)
	_ = os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
	return bucket, acct, nil
}

// cacheBucket names the region's cache bucket and the account that must
// own it, without creating it ("" when cache_bucket is off). auto: the
// bucket is spotexec's own, named after the account and region.
func (a *AWSSpot) cacheBucket(ctx context.Context) (bucket, acct string, auto bool, err error) {
	switch a.r.CacheBucket {
	case "off", "none", "false":
		return "", "", false, nil
	}
	if acct, err = a.accountID(ctx); err != nil {
		return "", "", false, err
	}
	bucket = a.r.CacheBucket
	auto = bucket == "" || bucket == "auto"
	if auto {
		bucket = "spotexec-cache-" + acct + "-" + a.r.Region
	}
	return bucket, acct, auto, nil
}

// hasCacheEntry reports whether the region's cache bucket holds the
// archive for key, creating nothing: a missing bucket holds nothing.
// Anything but a clear answer is an error.
func (a *AWSSpot) hasCacheEntry(ctx context.Context, repoID, key string) (bool, error) {
	bucket, acct, _, err := a.cacheBucket(ctx)
	if err != nil {
		return false, err
	}
	if bucket == "" {
		return false, nil
	}
	status, _, err := a.s3(ctx, http.MethodHead, bucket, s3EscapePath(repoID+"/"+key+".tar"), nil, map[string]string{"x-amz-expected-bucket-owner": acct})
	switch {
	case err != nil:
		return false, err
	case status == 200:
		return true, nil
	case status == 404:
		return false, nil
	}
	return false, fmt.Errorf("checking the dependency cache in %s: HTTP %d", bucket, status)
}

// s3OperationAbortedDelay is the first wait before retrying a bucket
// operation S3 aborted; each retry waits longer.
var s3OperationAbortedDelay = 500 * time.Millisecond

// s3Settled is s3 for bucket configuration calls, retried while S3 answers
// 409 OperationAborted. S3 serializes changes to one bucket's configuration
// and aborts the rest, so the parts of a parallel or sharded task setting up
// the cache bucket at once (on a machine that hasn't set it up yet) would
// otherwise lose the shared cache for every part but one.
func (a *AWSSpot) s3Settled(ctx context.Context, method, bucket, sub string, body []byte, extra map[string]string) (int, []byte, error) {
	delay := s3OperationAbortedDelay
	for attempt := 1; ; attempt++ {
		st, b, err := a.s3(ctx, method, bucket, sub, body, extra)
		if err != nil || st != http.StatusConflict || !bytes.Contains(b, []byte("OperationAborted")) || attempt == 6 {
			return st, b, err
		}
		select {
		case <-ctx.Done():
			return st, b, nil
		case <-time.After(delay):
		}
		delay *= 2
	}
}

func firstLineOf(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// s3 performs a signed S3 REST call on bucket (virtual-hosted style).
// sub is the path+query after the bucket host (e.g. "" or "?lifecycle").
func (a *AWSSpot) s3(ctx context.Context, method, bucket, sub string, body []byte, extra map[string]string) (int, []byte, error) {
	creds, err := a.api.creds.get(ctx)
	if err != nil {
		return 0, nil, err
	}
	u := "https://" + bucket + ".s3." + a.r.Region + ".amazonaws.com/" + strings.TrimPrefix(sub, "/")
	if e, ok := a.api.endpoints["s3"]; ok { // tests: path style
		u = strings.TrimSuffix(e, "/") + "/" + bucket + "/" + strings.TrimPrefix(sub, "/")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
	signV4(req, body, creds, a.r.Region, "s3", time.Now())
	resp, err := a.api.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// presignS3 builds a SigV4 query-string-authenticated URL (UNSIGNED-PAYLOAD).
func presignS3Host(c awsCreds, region, host, key, method string, ttl time.Duration, now time.Time) string {
	return presignS3Owner(c, region, host, key, method, ttl, now, "")
}

// presignS3Owner presigns, optionally binding the expected bucket owner into
// the signed query (S3 rejects the request if the bucket isn't theirs).
func presignS3Owner(c awsCreds, region, host, key, method string, ttl time.Duration, now time.Time, owner string) string {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + region + "/s3/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprint(int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	if c.SessionToken != "" {
		q.Set("X-Amz-Security-Token", c.SessionToken)
	}
	if owner != "" {
		q.Set("x-amz-expected-bucket-owner", owner)
	}
	path := "/" + s3EscapePath(key)
	canonical := strings.Join([]string{method, path, s3CanonicalQuery(q), "host:" + host + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	mac := hmac.New(sha256.New, sigV4SigningKey(c.SecretAccessKey, date, region, "s3"))
	mac.Write([]byte(sts))
	q.Set("X-Amz-Signature", hex.EncodeToString(mac.Sum(nil)))
	return "https://" + host + path + "?" + s3CanonicalQuery(q)
}

// s3EscapePath escapes each segment per SigV4 (RFC 3986, '/' kept).
func s3EscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func s3CanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k)+"="+uriEncode(q.Get(k)))
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
