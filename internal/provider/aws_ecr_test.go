package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// #215: every repository, new or found again, gets the rule keeping the
// newest three builds, counted by their tagged entries, with what older
// builds left untagged expired too.
func TestRegistryForLifecyclePolicy(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(map[bool]string{true: "reapplied to an existing repository", false: "set on a new repository"}[exists], func(t *testing.T) {
			var policies, calls []string
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				st, body := 200, "{}"
				action := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonEC2ContainerRegistry_V20150921.")
				calls = append(calls, action)
				switch action {
				case "DescribeRepositories":
					if exists {
						body = `{"repositories":[{"repositoryUri":"1.dkr.ecr.us-east-2.amazonaws.com/spotexec/x"}]}`
					} else {
						st, body = 400, `{"__type":"RepositoryNotFoundException","message":"none"}`
					}
				case "CreateRepository":
					body = `{"repository":{"repositoryUri":"1.dkr.ecr.us-east-2.amazonaws.com/spotexec/x"}}`
				case "PutLifecyclePolicy":
					var in struct {
						Repo   string `json:"repositoryName"`
						Policy string `json:"lifecyclePolicyText"`
					}
					if err := json.Unmarshal(b, &in); err != nil || in.Repo != "spotexec/x" {
						t.Errorf("PutLifecyclePolicy %s: %v", b, err)
					}
					policies = append(policies, in.Policy)
				case "GetAuthorizationToken":
					body = `{"authorizationData":[{"authorizationToken":"QVdTOnB3","proxyEndpoint":"https://1.dkr.ecr.us-east-2.amazonaws.com"}]}`
				default:
					t.Errorf("unexpected request %q %s", action, b)
				}
				return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			a := &AWSSpot{
				r: &config.Runner{Name: "aws", Region: "us-east-2", Profile: "p"},
				api: &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
					http: &http.Client{Transport: rt}},
				dir: t.TempDir(),
			}
			if _, err := a.RegistryFor(context.Background(), "x"); err != nil {
				t.Fatal(err)
			}
			want := "DescribeRepositories PutLifecyclePolicy GetAuthorizationToken"
			if !exists {
				want = "DescribeRepositories CreateRepository PutLifecyclePolicy GetAuthorizationToken"
			}
			if got := strings.Join(calls, " "); got != want || len(policies) != 1 {
				t.Fatalf("calls %s, want %s", got, want)
			}
			// With the rule set, the repository's URI is cached.
			calls = nil
			if _, err := a.RegistryFor(context.Background(), "x"); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(calls, " "); got != "GetAuthorizationToken" {
				t.Fatalf("second lookup %s", got)
			}
			var pol struct {
				Rules []struct {
					Priority  int `json:"rulePriority"`
					Selection struct {
						TagStatus   string   `json:"tagStatus"`
						TagPatterns []string `json:"tagPatternList"`
						CountType   string   `json:"countType"`
						CountUnit   string   `json:"countUnit"`
						CountNumber int      `json:"countNumber"`
					} `json:"selection"`
					Action struct {
						Type string `json:"type"`
					} `json:"action"`
				} `json:"rules"`
			}
			if err := json.Unmarshal([]byte(policies[0]), &pol); err != nil {
				t.Fatalf("policy %s: %v", policies[0], err)
			}
			if len(pol.Rules) != 2 {
				t.Fatalf("policy %s", policies[0])
			}
			keep, left := pol.Rules[0], pol.Rules[1]
			if keep.Priority != 1 || keep.Selection.TagStatus != "tagged" || len(keep.Selection.TagPatterns) != 1 || keep.Selection.TagPatterns[0] != "*" ||
				keep.Selection.CountType != "imageCountMoreThan" || keep.Selection.CountNumber != 3 || ECRKeepBuilds != 3 || keep.Action.Type != "expire" {
				t.Errorf("builds rule %+v", keep)
			}
			if left.Priority != 2 || left.Selection.TagStatus != "untagged" || len(left.Selection.TagPatterns) != 0 ||
				left.Selection.CountType != "sinceImagePushed" || left.Selection.CountUnit != "days" || left.Selection.CountNumber != 1 || left.Action.Type != "expire" {
				t.Errorf("untagged rule %+v", left)
			}
		})
	}
}

// #218: a repository whose expiry rule can't be set (denied, or the call
// hangs) still gives the login, so the runner pulls (or pushes) the image
// rather than building it. The next lookup sets the rule again.
func TestRegistryForSurvivesALifecyclePolicyFailure(t *testing.T) {
	old := ecrLifecycleTimeout
	ecrLifecycleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { ecrLifecycleTimeout = old })
	for _, tc := range []struct {
		exists, hangs bool
	}{{true, false}, {false, false}, {true, true}} {
		name := map[bool]string{true: "existing repository", false: "new repository"}[tc.exists] + map[bool]string{true: ", policy call hangs", false: ", policy denied"}[tc.hangs]
		t.Run(name, func(t *testing.T) {
			exists := tc.exists
			var calls []string
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				st, body := 200, "{}"
				action := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonEC2ContainerRegistry_V20150921.")
				calls = append(calls, action)
				switch action {
				case "DescribeRepositories":
					if exists {
						body = `{"repositories":[{"repositoryUri":"1.dkr.ecr.ap-south-1.amazonaws.com/spotexec/x"}]}`
					} else {
						st, body = 400, `{"__type":"RepositoryNotFoundException","message":"none"}`
					}
				case "CreateRepository":
					body = `{"repository":{"repositoryUri":"1.dkr.ecr.ap-south-1.amazonaws.com/spotexec/x"}}`
				case "PutLifecyclePolicy":
					if tc.hangs {
						// Bounded by its own deadline, not the lookup's.
						if d, ok := r.Context().Deadline(); !ok || time.Until(d) > ecrLifecycleTimeout {
							t.Errorf("policy call deadline %v (%v)", d, ok)
							return nil, errors.New("no deadline of its own") // fail, don't hang
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					st, body = 400, `{"__type":"AccessDeniedException","message":"not authorized to perform ecr:PutLifecyclePolicy"}`
				case "GetAuthorizationToken":
					body = `{"authorizationData":[{"authorizationToken":"QVdTOnB3","proxyEndpoint":"https://1.dkr.ecr.ap-south-1.amazonaws.com"}]}`
				}
				return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			a := &AWSSpot{
				r: &config.Runner{Name: "aws", Region: "ap-south-1", Profile: "p"},
				api: &awsClient{region: "ap-south-1", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "s"}},
					http: &http.Client{Transport: rt}},
				dir: t.TempDir(),
			}
			// The lookup's own deadline: a policy call that held it would
			// leave the login none.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			auth, err := a.RegistryFor(ctx, "x")
			if err != nil {
				t.Fatal(err)
			}
			if auth.Repo != "1.dkr.ecr.ap-south-1.amazonaws.com/spotexec/x" || auth.User != "AWS" || auth.Password != "pw" {
				t.Fatalf("auth %+v", auth)
			}
			if !strings.Contains(strings.Join(calls, " "), "PutLifecyclePolicy GetAuthorizationToken") {
				t.Fatalf("calls %v", calls)
			}
			// The repository is now found, and its rule tried again.
			exists, calls = true, nil
			if _, err := a.RegistryFor(ctx, "x"); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(calls, " "); got != "DescribeRepositories PutLifecyclePolicy GetAuthorizationToken" {
				t.Fatalf("second lookup %s", got)
			}
		})
	}
}

// #218: on a multi-region runner whose instance's region can't set the
// expiry rule, both the region's own registry and the copy from another
// region holding the tag are still to be had.
func TestImageLookupsSurviveALifecyclePolicyFailure(t *testing.T) {
	f := &fakeRegionsEC2{
		images:    map[string]map[string]string{"r-one-1": {}, "r-two-1": {"t1": "sha256:a"}},
		policyErr: map[string]bool{"r-one-1": true},
	}
	g := newGlobalEnv(t, f, "r-two-1", "r-one-1")
	discovered(t, g)
	if err := g.remember([]Instance{{ID: "i-1", Region: "r-one-1", LaunchedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	reg, ok := ForInstance(context.Background(), g, "i-1").(ImageRegistry)
	if !ok {
		t.Fatal("the instance's region has no registry")
	}
	home, err := reg.RegistryFor(context.Background(), "repo1")
	if err != nil {
		t.Fatal(err)
	}
	if home.Repo != "123456789012.dkr.ecr.r-one-1.amazonaws.com/spotexec/repo1" {
		t.Fatalf("home %+v", home)
	}
	if got := f.callsOf("PutLifecyclePolicy"); len(got) != 1 || !strings.HasPrefix(got[0], "r-one-1 ") {
		t.Fatalf("policy calls %v", got)
	}
	src, err := g.ImageSource(context.Background(), "i-1", "repo1", "t1")
	if err != nil || src == nil || src.Repo != "123456789012.dkr.ecr.r-two-1.amazonaws.com/spotexec/repo1" {
		t.Fatalf("src %+v, %v", src, err)
	}
}

// #192: a multi-region runner finds the project's image in another of its
// regions, in the order it lists them, to copy rather than build it.
func TestImageSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		regions  []string // default r-two-1, r-one-1, r-three-1
		noList   bool     // the account's regions can't be listed
		images   map[string]map[string]string
		imageErr map[string]string
		loginErr map[string]bool
		want     string // region of the source, "" none
		err      string
	}{
		{name: "the first listed region holding it",
			images: map[string]map[string]string{"r-one-1": {"t1": "sha256:a"}, "r-two-1": {"t1": "sha256:a"}, "r-three-1": {"t1": "sha256:a"}},
			want:   "r-two-1"},
		{name: "another tag only is no source",
			images: map[string]map[string]string{"r-two-1": {"t0": "sha256:a"}}},
		{name: "a region that can't be logged into is skipped",
			images:   map[string]map[string]string{"r-two-1": {"t1": "sha256:a"}, "r-three-1": {"t1": "sha256:a"}},
			loginErr: map[string]bool{"r-two-1": true},
			want:     "r-three-1"},
		{name: "no usable source is an error when one holds it",
			images:   map[string]map[string]string{"r-two-1": {"t1": "sha256:a"}},
			loginErr: map[string]bool{"r-two-1": true},
			err:      "AccessDeniedException"},
		{name: "a region that can't be asked is an error",
			imageErr: map[string]string{"r-three-1": "AccessDeniedException"},
			err:      "AccessDeniedException"},
		{name: "regions that can't be listed may hold it",
			regions: []string{"all"}, noList: true,
			err: "UnauthorizedOperation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRegionsEC2{images: tc.images, imageErr: tc.imageErr, loginErr: tc.loginErr, noRegions: tc.noList}
			regions := tc.regions
			if regions == nil {
				regions = []string{"r-two-1", "r-one-1", "r-three-1"}
			}
			g := newGlobalEnv(t, f, regions...)
			discovered(t, g)
			if err := g.remember([]Instance{{ID: "i-1", Region: "r-one-1", LaunchedAt: time.Now()}}); err != nil {
				t.Fatal(err)
			}
			src, err := g.ImageSource(context.Background(), "i-1", "repo1", "t1")
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) || src != nil {
					t.Fatalf("src %+v, err %v", src, err)
				}
			case err != nil:
				t.Fatal(err)
			case tc.want == "":
				if src != nil {
					t.Fatalf("src %+v", src)
				}
			default:
				server := "123456789012.dkr.ecr." + tc.want + ".amazonaws.com"
				if src == nil || src.Server != server || src.Repo != server+"/spotexec/repo1" || src.User != "AWS" || src.Password != "pw-"+tc.want {
					t.Fatalf("src %+v", src)
				}
			}
			// The instance's own region is never asked, and nothing is created.
			for _, c := range f.callsOf("BatchGetImage") {
				if strings.HasPrefix(c, "r-one-1 ") {
					t.Fatalf("asked the instance's own region: %v", c)
				}
			}
		})
	}
}
