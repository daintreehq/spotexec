package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// fakeCacheBucketAWS answers STS and the cache bucket's HEAD, and hands each
// lifecycle PUT to lifecycle (attempt counts from 1). heads counts the
// bucket checks.
func fakeCacheBucketAWS(t *testing.T, lifecycle func(attempt int) (int, string)) (a *AWSSpot, puts func() int, heads func() int) {
	t.Helper()
	old := s3OperationAbortedDelay
	s3OperationAbortedDelay = time.Millisecond
	t.Cleanup(func() { s3OperationAbortedDelay = old })
	var mu sync.Mutex
	nput, nhead := 0, 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := func(st int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		switch {
		case strings.HasPrefix(r.URL.Host, "sts."):
			return resp(200, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case r.URL.Host != "spotexec-cache-123456789012-us-east-2.s3.us-east-2.amazonaws.com":
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			return resp(500, "")
		case r.Method == http.MethodHead:
			mu.Lock()
			nhead++
			mu.Unlock()
			return resp(200, "")
		case r.Method == http.MethodPut && r.URL.RawQuery == "lifecycle":
			mu.Lock()
			nput++
			n := nput
			mu.Unlock()
			return resp(lifecycle(n))
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		return resp(500, "")
	})
	a = &AWSSpot{
		r: &config.Runner{Name: "aws", Region: "us-east-2", Profile: "p"},
		api: &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
			http: &http.Client{Transport: rt}},
		dir: t.TempDir(),
	}
	return a, func() int { mu.Lock(); defer mu.Unlock(); return nput }, func() int { mu.Lock(); defer mu.Unlock(); return nhead }
}

const s3OperationAborted = `<Error><Code>OperationAborted</Code><Message>A conflicting conditional operation is currently in progress against this resource. Please try again.</Message></Error>`

// Parts setting up the cache bucket at once make S3 abort all but one
// lifecycle change: the others retry instead of losing the shared cache.
func TestEnsureBucketRetriesOperationAborted(t *testing.T) {
	a, puts, _ := fakeCacheBucketAWS(t, func(n int) (int, string) {
		if n <= 2 {
			return http.StatusConflict, s3OperationAborted
		}
		return 200, ""
	})
	bucket, acct, err := a.ensureBucket(context.Background())
	if err != nil || bucket != "spotexec-cache-123456789012-us-east-2" || acct != "123456789012" {
		t.Fatalf("ensureBucket = %q %q %v", bucket, acct, err)
	}
	if got := puts(); got != 3 {
		t.Fatalf("lifecycle PUTs = %d, want 3", got)
	}
	// Set up once: the next call trusts the marker and asks S3 nothing.
	if _, _, err := a.ensureBucket(context.Background()); err != nil || puts() != 3 {
		t.Fatalf("second ensureBucket: %v, PUTs %d", err, puts())
	}
}

// Other conflicts aren't retried, and a lasting OperationAborted gives up.
// The expiry rule is best effort (#228): the bucket is still returned, and
// the next lookup applies the rule again without checking the bucket.
func TestEnsureBucketGivesUp(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"other conflict", `<Error><Code>InvalidBucketState</Code></Error>`, 1},
		{"still aborted", s3OperationAborted, 6},
		{"denied", `<Error><Code>AccessDenied</Code></Error>`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := http.StatusConflict
			if tc.name == "denied" {
				st = http.StatusForbidden
			}
			fail := true
			a, puts, heads := fakeCacheBucketAWS(t, func(int) (int, string) {
				if fail {
					return st, tc.body
				}
				return 200, ""
			})
			if bucket, _, err := a.ensureBucket(context.Background()); err != nil || bucket != "spotexec-cache-123456789012-us-east-2" {
				t.Fatalf("ensureBucket = %q, %v", bucket, err)
			}
			if got := puts(); got != tc.want {
				t.Fatalf("lifecycle PUTs = %d, want %d", got, tc.want)
			}
			fail = false
			if _, _, err := a.ensureBucket(context.Background()); err != nil || puts() != tc.want+1 || heads() != 1 {
				t.Fatalf("second ensureBucket: %v, PUTs %d, HEADs %d; want the rule again and no bucket check", err, puts(), heads())
			}
			// Set now: nothing more is asked.
			if _, _, err := a.ensureBucket(context.Background()); err != nil || puts() != tc.want+1 || heads() != 1 {
				t.Fatalf("third ensureBucket: %v, PUTs %d, HEADs %d", err, puts(), heads())
			}
		})
	}
}

// A profile name is one file name component: "corp/dev" neither needs a
// directory nor shares a file with "corp_dev"; ordinary names keep theirs.
func TestFileSafe(t *testing.T) {
	for in, want := range map[string]string{"work": "work", "corp/dev": "corp%2Fdev", "a\\b": "a%5Cb", "..": "..", "x%y": "x%25y", "us-east-2": "us-east-2"} {
		if got := fileSafe(in); got != want {
			t.Errorf("fileSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAccountIDSlashProfile(t *testing.T) {
	a, _, _ := fakeCacheBucketAWS(t, func(int) (int, string) { return 200, "" })
	a.r.Profile = "corp/../dev"
	if acct, err := a.accountID(context.Background()); err != nil || acct != "123456789012" {
		t.Fatalf("accountID = %q, %v", acct, err)
	}
	ents, _ := os.ReadDir(a.dir)
	if len(ents) != 1 || ents[0].IsDir() || !strings.HasPrefix(ents[0].Name(), "account-id-corp%2F..%2Fdev-") {
		t.Fatalf("state dir holds %v", ents)
	}
}

// The S3 canonical query sorts by encoded name and keeps repeated values.
func TestS3CanonicalQuery(t *testing.T) {
	q := url.Values{"a-b": {"1"}, "a": {"2", "1"}, "a/b": {"1"}, "delete": nil, "t": {"x~y z+"}}
	if got, want := s3CanonicalQuery(q), "a=1&a=2&a%2Fb=1&a-b=1&delete=&t=x~y%20z%2B"; got != want {
		t.Fatalf("s3CanonicalQuery = %q, want %q", got, want)
	}
}

// The ECR marker for a slash profile is one file in the state directory.
func TestRegistryForSlashProfile(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "{}"
		switch strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonEC2ContainerRegistry_V20150921.") {
		case "DescribeRepositories":
			body = `{"repositories":[{"repositoryUri":"1.dkr.ecr.us-east-2.amazonaws.com/spotexec/x"}]}`
		case "GetAuthorizationToken":
			body = `{"authorizationData":[{"authorizationToken":"QVdTOnB3","proxyEndpoint":"https://1.dkr.ecr.us-east-2.amazonaws.com"}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	a := &AWSSpot{
		r: &config.Runner{Name: "aws", Region: "us-east-2", Profile: "corp/dev"},
		api: &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
			http: &http.Client{Transport: rt}},
		dir: t.TempDir(),
	}
	if auth, err := a.RegistryFor(context.Background(), "x"); err != nil || auth.User != "AWS" {
		t.Fatalf("RegistryFor = %+v, %v", auth, err)
	}
	ents, _ := os.ReadDir(a.dir)
	if len(ents) != 1 || ents[0].IsDir() || !strings.HasPrefix(ents[0].Name(), "ecr-us-east-2-corp%2Fdev-") {
		t.Fatalf("state dir holds %v", ents)
	}
}

// A 403 on the cache bucket says why it may be refused: spotexec's policy
// covers only its own spotexec-cache-* buckets, so a named bucket needs a
// grant of its own.
func TestEnsureBucketForbidden(t *testing.T) {
	auto := []string{"S3 statement of spotexec's policy", "another account owns", "spotexec-cache-123456789012-us-east-2"}
	named := []string{"policy only covers spotexec-cache-* buckets", "isn't owned by account 123456789012", "s3:ListBucket", "s3:GetObject", "s3:PutObject"}
	for _, tc := range []struct {
		bucket, host string
		want         []string
	}{
		{"", "spotexec-cache-123456789012-us-east-2.s3.us-east-2.amazonaws.com", auto},
		{"auto", "spotexec-cache-123456789012-us-east-2.s3.us-east-2.amazonaws.com", auto},
		{"my-team-cache", "my-team-cache.s3.us-east-2.amazonaws.com", named},
		// Named by hand, however spotexec-like: the setting decides.
		{"spotexec-cache-123456789012-us-east-2", "spotexec-cache-123456789012-us-east-2.s3.us-east-2.amazonaws.com", named},
	} {
		t.Run("bucket="+tc.bucket, func(t *testing.T) {
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				st, body := 500, ""
				switch {
				case strings.HasPrefix(r.URL.Host, "sts."):
					st, body = 200, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`
				case r.URL.Host == tc.host && r.Method == http.MethodHead:
					st = http.StatusForbidden
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			a := &AWSSpot{
				r: &config.Runner{Name: "aws", Region: "us-east-2", Profile: "p", CacheBucket: tc.bucket},
				api: &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
					http: &http.Client{Transport: rt}},
				dir: t.TempDir(),
			}
			_, _, err := a.ensureBucket(context.Background())
			if err == nil || !strings.Contains(err.Error(), "access denied") {
				t.Fatalf("ensureBucket error = %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
		})
	}
}

// The client can't reach the region's S3 or STS (#228): a bucket check or
// expiry rule that fails or hangs, or an account lookup the region doesn't
// answer. The URLs are signed anyway, for the VM in that region to use, so
// a fresh VM still restores the archive the bucket holds.
func TestCacheURLsSurviveAnUnreachableRegion(t *testing.T) {
	oldAcct, oldRule := accountLookupTimeout, cacheRuleTimeout
	accountLookupTimeout, cacheRuleTimeout = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { accountLookupTimeout, cacheRuleTimeout = oldAcct, oldRule })
	const identity = `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`
	for _, tc := range []struct {
		name         string
		sts, head    string // "ok", "fail", "hang" or "503"
		rule         string
		heads, rules int      // calls made by two lookups
		stsAsked     []string // STS regions asked, in order
		budget       time.Duration
	}{
		// Unchecked: the next lookup checks again. The bucket check has no
		// timeout of its own: the caller's budget bounds it.
		{name: "bucket check fails", sts: "ok", head: "fail", heads: 2},
		{name: "bucket check hangs", sts: "ok", head: "hang", heads: 2, budget: 300 * time.Millisecond},
		{name: "bucket check answers 503", sts: "ok", head: "503", heads: 2},
		// The bucket exists: only the rule is tried again, within its own
		// timeout.
		{name: "expiry rule fails", sts: "ok", head: "ok", rule: "fail", heads: 1, rules: 2},
		{name: "expiry rule hangs", sts: "ok", head: "ok", rule: "hang", heads: 1, rules: 2},
		// The account comes from the partition's home region, after the
		// runner's own region gets its bounded turn.
		{name: "regional STS hangs", sts: "hang", head: "ok", rule: "ok", heads: 1, rules: 1, stsAsked: []string{"ap-south-1", "us-east-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var heads, rules atomic.Int32
			var mu sync.Mutex
			var stsAsked []string
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp := func(st int, body string) (*http.Response, error) {
					return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}
				act := func(how string) (*http.Response, error) {
					switch how {
					case "fail":
						return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: errors.New("connect: connection timed out")}
					case "hang":
						<-r.Context().Done()
						return nil, r.Context().Err()
					case "503":
						return resp(503, "")
					}
					return resp(200, "")
				}
				if rg, ok := strings.CutPrefix(r.URL.Host, "sts."); ok {
					mu.Lock()
					stsAsked = append(stsAsked, strings.TrimSuffix(rg, ".amazonaws.com"))
					mu.Unlock()
				}
				switch {
				case r.URL.Host == "sts.us-east-1.amazonaws.com":
					return resp(200, identity)
				case r.URL.Host == "sts.ap-south-1.amazonaws.com":
					if tc.sts == "ok" {
						return resp(200, identity)
					}
					return act(tc.sts)
				case r.URL.Host != "spotexec-cache-123456789012-ap-south-1.s3.ap-south-1.amazonaws.com":
				case r.Method == http.MethodHead:
					heads.Add(1)
					return act(tc.head)
				case r.Method == http.MethodPut && r.URL.RawQuery == "lifecycle":
					rules.Add(1)
					return act(tc.rule)
				}
				t.Errorf("unexpected request %s %s", r.Method, r.URL)
				return resp(500, "")
			})
			a := &AWSSpot{
				r: &config.Runner{Name: "aws", Region: "ap-south-1", Profile: "p"},
				api: &awsClient{region: "ap-south-1", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
					http: &http.Client{Transport: rt}},
				dir: t.TempDir(),
			}
			budget := tc.budget
			if budget == 0 {
				// Long enough that only the steps' own timeouts keep a
				// lookup under a second.
				budget = 5 * time.Second
			}
			for range 2 {
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				start := time.Now()
				get, put, err := a.CacheURLs(ctx, "repo", "k")
				took := time.Since(start)
				cancel()
				if err != nil {
					t.Fatalf("CacheURLs: %v", err)
				}
				if took > time.Second {
					t.Fatalf("lookup took %s: a step outlived its own timeout", took)
				}
				for _, u := range []string{get, put} {
					p, err := url.Parse(u)
					if err != nil || p.Host != "spotexec-cache-123456789012-ap-south-1.s3.ap-south-1.amazonaws.com" || p.Path != "/repo/k.tar" ||
						p.Query().Get("x-amz-expected-bucket-owner") != "123456789012" {
						t.Fatalf("URL %q: want the region's bucket, bound to its owner", u)
					}
				}
			}
			if int(heads.Load()) != tc.heads || int(rules.Load()) != tc.rules {
				t.Fatalf("bucket checks %d, expiry rules %d; want %d, %d", heads.Load(), rules.Load(), tc.heads, tc.rules)
			}
			want := tc.stsAsked
			if want == nil {
				want = []string{"ap-south-1"} // once: the account is remembered
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(stsAsked, want) {
				t.Fatalf("STS asked in %q, want %q", stsAsked, want)
			}
		})
	}
}

// Neither the region's STS nor the home region's answers: the error says
// where the account was looked for.
func TestAccountIDTriesBothRegions(t *testing.T) {
	var mu sync.Mutex
	var hosts []string
	old := accountLookupTimeout
	accountLookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { accountLookupTimeout = old })
	a := &AWSSpot{
		r: &config.Runner{Name: "aws", Region: "ap-south-1", Profile: "p"},
		api: &awsClient{region: "ap-south-1", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
			http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				hosts = append(hosts, r.URL.Host)
				mu.Unlock()
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}},
		dir: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := a.accountID(ctx)
	if err == nil || !strings.Contains(err.Error(), "ap-south-1 and us-east-1") {
		t.Fatalf("accountID error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"sts.ap-south-1.amazonaws.com", "sts.us-east-1.amazonaws.com"}; !slices.Equal(hosts, want) {
		t.Fatalf("asked %q, want %q", hosts, want)
	}
}

// A bucket whose expiry rule couldn't be set, and which has been deleted
// since, is checked (and created) again rather than trusted as existing.
func TestEnsureBucketForgetsADeletedBucket(t *testing.T) {
	a, puts, heads := fakeCacheBucketAWS(t, func(n int) (int, string) {
		switch n {
		case 1:
			return http.StatusInternalServerError, ""
		case 2:
			return http.StatusNotFound, `<Error><Code>NoSuchBucket</Code></Error>`
		}
		return 200, ""
	})
	for range 3 {
		if _, _, err := a.ensureBucket(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if puts() != 3 || heads() != 2 {
		t.Fatalf("PUTs %d, HEADs %d; want 3, 2 (checked again after the 404)", puts(), heads())
	}
}
