package provider

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// fakeCacheBucketAWS answers STS and the cache bucket's HEAD, and hands each
// lifecycle PUT to lifecycle (attempt counts from 1).
func fakeCacheBucketAWS(t *testing.T, lifecycle func(attempt int) (int, string)) (*AWSSpot, func() int) {
	t.Helper()
	old := s3OperationAbortedDelay
	s3OperationAbortedDelay = time.Millisecond
	t.Cleanup(func() { s3OperationAbortedDelay = old })
	var mu sync.Mutex
	puts := 0
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
			return resp(200, "")
		case r.Method == http.MethodPut && r.URL.RawQuery == "lifecycle":
			mu.Lock()
			puts++
			n := puts
			mu.Unlock()
			return resp(lifecycle(n))
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		return resp(500, "")
	})
	a := &AWSSpot{
		r: &config.Runner{Name: "aws", Region: "us-east-2", Profile: "p"},
		api: &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
			http: &http.Client{Transport: rt}},
		dir: t.TempDir(),
	}
	return a, func() int { mu.Lock(); defer mu.Unlock(); return puts }
}

const s3OperationAborted = `<Error><Code>OperationAborted</Code><Message>A conflicting conditional operation is currently in progress against this resource. Please try again.</Message></Error>`

// Parts setting up the cache bucket at once make S3 abort all but one
// lifecycle change: the others retry instead of losing the shared cache.
func TestEnsureBucketRetriesOperationAborted(t *testing.T) {
	a, puts := fakeCacheBucketAWS(t, func(n int) (int, string) {
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
func TestEnsureBucketGivesUp(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"other conflict", `<Error><Code>InvalidBucketState</Code></Error>`, 1},
		{"still aborted", s3OperationAborted, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, puts := fakeCacheBucketAWS(t, func(int) (int, string) { return http.StatusConflict, tc.body })
			if _, _, err := a.ensureBucket(context.Background()); err == nil || !strings.Contains(err.Error(), "setting cache expiry") {
				t.Fatalf("ensureBucket error = %v", err)
			}
			if got := puts(); got != tc.want {
				t.Fatalf("lifecycle PUTs = %d, want %d", got, tc.want)
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
	a, _ := fakeCacheBucketAWS(t, func(int) (int, string) { return 200, "" })
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
	q := url.Values{"a-b": {"1"}, "a": {"2", "1"}, "delete": {""}, "t": {"x~y z"}}
	if got, want := s3CanonicalQuery(q), "a=1&a=2&a-b=1&delete=&t=x~y%20z"; got != want {
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
