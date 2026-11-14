package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type fakeAcctInstance struct {
	id, state string
	tags      map[string]string
}

// fakeAccount serves STS and EC2 for several profiles (keys "AK-<profile>")
// and regions, as account-wide discovery sees them.
type fakeAccount struct {
	mu           sync.Mutex
	accounts     map[string]string                        // key -> account
	instances    map[string]map[string][]fakeAcctInstance // key -> region -> instances
	failRegion   map[string]bool                          // DescribeInstances fails there
	terminated   []string                                 // "key region id"
	failTerm     map[string]bool                          // TerminateInstances fails for these ids
	stsFail      map[string]bool                          // GetCallerIdentity fails in these regions
	notFoundOnce map[string]bool                          // the first DescribeInstances by id says NotFound
	filters      url.Values
}

func (f *fakeAccount) install(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		m := regexp.MustCompile(`Credential=([^/]+)/[^/]+/([^/]+)/`).FindStringSubmatch(r.Header.Get("Authorization"))
		key, region := m[1], m[2]
		body, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(body))
		fail := func(code string) {
			w.WriteHeader(400)
			fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors></Response>`, code, code)
		}
		acct, ok := f.accounts[key]
		if !ok {
			fail("AuthFailure")
			return
		}
		item := func(in fakeAcctInstance) string {
			var tags strings.Builder
			for k, val := range in.tags {
				fmt.Fprintf(&tags, `<item><key>%s</key><value>%s</value></item>`, k, val)
			}
			return fmt.Sprintf(`<item><instancesSet><item><instanceId>%s</instanceId><instanceState><name>%s</name></instanceState><instanceType>c7a.large</instanceType><tagSet>%s</tagSet></item></instancesSet></item>`, in.id, in.state, tags.String())
		}
		switch v.Get("Action") {
		case "GetCallerIdentity":
			if f.stsFail[region] {
				fail("RegionDisabledException")
				return
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>%s</Account><Arn>arn:aws:iam::%s:user/x</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, acct, acct)
		case "DescribeInstances":
			var b strings.Builder
			for _, in := range f.instances[key][region] {
				if id := v.Get("InstanceId.1"); id == "" || id == in.id {
					b.WriteString(item(in))
				}
			}
			if id := v.Get("InstanceId.1"); f.notFoundOnce[id] {
				delete(f.notFoundOnce, id)
				fail("InvalidInstanceID.NotFound")
				return
			}
			if v.Get("InstanceId.1") == "" {
				if f.failRegion[region] {
					fail("UnauthorizedOperation")
					return
				}
				f.filters = v
			} else if b.Len() == 0 {
				fail("InvalidInstanceID.NotFound")
				return
			}
			fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet>%s</reservationSet></DescribeInstancesResponse>`, b.String())
		case "TerminateInstances":
			if f.failTerm[v.Get("InstanceId.1")] {
				fail("UnauthorizedOperation")
				return
			}
			f.terminated = append(f.terminated, key+" "+region+" "+v.Get("InstanceId.1"))
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)
		default:
			t.Errorf("unexpected action %s", v.Get("Action"))
			fail("InvalidAction")
		}
	}))
	t.Cleanup(srv.Close)
	oldEP, oldRes := accountEndpoints, accountResolver
	accountEndpoints = map[string]string{"ec2": srv.URL, "sts": srv.URL}
	accountResolver = func(profile string) *awsCredResolver {
		return staticResolver(awsCreds{AccessKeyID: "AK-" + profile, SecretAccessKey: "s"})
	}
	t.Cleanup(func() { accountEndpoints, accountResolver = oldEP, oldRes })
	dir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
	os.WriteFile(filepath.Join(dir, "owner-id"), []byte("me-1\n"), 0o600)
}

// Discovery covers fleet VMs and bake builders in any live state, records
// where each was found, and reports regions it couldn't list.
func TestFindAccountInstances(t *testing.T) {
	f := &fakeAccount{
		accounts: map[string]string{"AK-a": "111111111111", "AK-b": "222222222222"},
		instances: map[string]map[string][]fakeAcctInstance{
			"AK-a": {"us-east-2": {
				{"i-run", "running", map[string]string{tagRunner: "aws", tagOwner: "me-1", tagHost: "mac"}},
				{"i-bake", "stopped", map[string]string{tagBake: "aws", tagOwner: "other-2", tagHost: "studio"}},
			}},
			"AK-b": {"us-east-2": {
				{"i-other", "stopped", map[string]string{tagRunner: "work", tagOwner: "other-2", tagHost: "studio"}},
			}},
		},
		failRegion: map[string]bool{"us-west-2": true},
	}
	f.install(t)
	res := FindAccountInstances(context.Background(), []AccountScope{
		{Region: "us-east-2", Profile: "a"}, {Region: "us-east-2", Profile: "b"}, {Region: "us-west-2", Profile: "a"}, {Region: "us-east-2", Profile: "a"},
	})
	if got := strings.Join(f.filters["Filter.1.Value.1"], ",") + "," + strings.Join(f.filters["Filter.1.Value.2"], ","); got != tagRunner+","+tagBake {
		t.Errorf("tag filter %v", f.filters)
	}
	var states []string
	for i := 1; f.filters.Get(fmt.Sprintf("Filter.2.Value.%d", i)) != ""; i++ {
		states = append(states, f.filters.Get(fmt.Sprintf("Filter.2.Value.%d", i)))
	}
	if strings.Join(states, ",") != "pending,running,shutting-down,stopping,stopped" {
		t.Errorf("states %v", states)
	}
	got := map[string]AccountInstance{}
	for _, in := range res.Instances {
		got[in.ID] = in
	}
	if len(res.Instances) != 3 {
		t.Fatalf("instances %+v", res.Instances)
	}
	if in := got["i-run"]; in.Account != "111111111111" || in.Profile != "a" || in.Builder || !in.Mine || in.Runner != "aws" {
		t.Errorf("i-run %+v", in)
	}
	if in := got["i-bake"]; !in.Builder || in.Runner != "aws" || in.State != "stopped" || in.Mine {
		t.Errorf("i-bake %+v", in)
	}
	if in := got["i-other"]; in.Account != "222222222222" || in.Profile != "b" {
		t.Errorf("i-other %+v", in)
	}
	if len(res.Searched) != 2 || len(res.Failed) != 1 || res.Failed[0].Region != "us-west-2" || res.Err() == nil ||
		!strings.Contains(res.Err().Error(), "us-west-2 (account 111111111111, profile a)") {
		t.Fatalf("searched %+v, failed %+v, err %v", res.Searched, res.Failed, res.Err())
	}

	// Termination goes through the profile that found the instance.
	if err := TerminateAccountInstance(context.Background(), got["i-other"]); err != nil {
		t.Fatal(err)
	}
	if err := TerminateAccountInstance(context.Background(), got["i-bake"]); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.terminated, ";") != "AK-b us-east-2 i-other;AK-a us-east-2 i-bake" {
		t.Fatalf("terminated %v", f.terminated)
	}
}

// A profile that can't sign in fails its scopes instead of vanishing.
func TestFindAccountInstancesBadProfile(t *testing.T) {
	f := &fakeAccount{accounts: map[string]string{"AK-a": "111111111111"}}
	f.install(t)
	res := FindAccountInstances(context.Background(), []AccountScope{{Region: "us-east-2", Profile: "a"}, {Region: "us-east-2", Profile: "expired"}})
	if len(res.Searched) != 1 || len(res.Failed) != 1 || res.Failed[0].Profile != "expired" {
		t.Fatalf("searched %+v, failed %+v", res.Searched, res.Failed)
	}
}

// STS failing in one region doesn't stop the same profile's other regions,
// and an eventually-consistent NotFound doesn't skip a termination.
func TestFindAccountInstancesRegionalSTS(t *testing.T) {
	f := &fakeAccount{
		accounts:     map[string]string{"AK-a": "111111111111"},
		instances:    map[string]map[string][]fakeAcctInstance{"AK-a": {"us-west-2": {{"i-new", "pending", map[string]string{tagRunner: "aws", tagOwner: "x"}}}}},
		stsFail:      map[string]bool{"us-east-2": true},
		notFoundOnce: map[string]bool{"i-new": true},
	}
	f.install(t)
	res := FindAccountInstances(context.Background(), []AccountScope{{Region: "us-east-2", Profile: "a"}, {Region: "us-west-2", Profile: "a"}})
	if len(res.Failed) != 1 || res.Failed[0].Region != "us-east-2" || len(res.Searched) != 1 || len(res.Instances) != 1 {
		t.Fatalf("searched %+v, failed %+v, instances %+v", res.Searched, res.Failed, res.Instances)
	}
	if err := TerminateAccountInstance(context.Background(), res.Instances[0]); err != nil || len(f.terminated) != 1 {
		t.Fatalf("err %v, terminated %v", err, f.terminated)
	}
}
