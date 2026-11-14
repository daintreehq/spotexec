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
	"time"
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
	users        map[string]map[string]string             // IAM user -> tags (all under /spotexec/)
	keys         map[string][2]string                     // access key -> user, status
	log          []string                                 // IAM and EC2 mutations, in order
	late         map[string][]fakeAcctInstance            // region -> instances that appear from the third listing on
	hideAt       map[string]int                           // instance -> a listing (by number) that misses it
	listings     map[string]int                           // key+region -> DescribeInstances listings so far
	stuck        bool                                     // terminating instances never finish
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
		iamUser := v.Get("UserName")
		switch v.Get("Action") {
		case "GetUser":
			tags, ok := f.users[iamUser]
			if !ok {
				w.WriteHeader(404)
				fmt.Fprint(w, `<ErrorResponse><Error><Code>NoSuchEntity</Code><Message>no</Message></Error></ErrorResponse>`)
				return
			}
			var b strings.Builder
			for k, val := range tags {
				fmt.Fprintf(&b, `<member><Key>%s</Key><Value>%s</Value></member>`, k, val)
			}
			fmt.Fprintf(w, `<GetUserResponse><GetUserResult><User><Arn>arn:aws:iam::%s:user/spotexec/%s</Arn><Tags>%s</Tags></User></GetUserResult></GetUserResponse>`, acct, iamUser, b.String())
		case "ListAccessKeys":
			var b strings.Builder
			for id, k := range f.keys {
				if k[0] == iamUser {
					fmt.Fprintf(&b, `<member><AccessKeyId>%s</AccessKeyId><Status>%s</Status></member>`, id, k[1])
				}
			}
			fmt.Fprintf(w, `<ListAccessKeysResponse><ListAccessKeysResult><AccessKeyMetadata>%s</AccessKeyMetadata></ListAccessKeysResult></ListAccessKeysResponse>`, b.String())
		case "UpdateAccessKey":
			id := v.Get("AccessKeyId")
			f.keys[id] = [2]string{f.keys[id][0], v.Get("Status")}
			f.log = append(f.log, "UpdateAccessKey "+id+" "+v.Get("Status"))
			fmt.Fprint(w, `<UpdateAccessKeyResponse/>`)
		case "DeleteAccessKey":
			delete(f.keys, v.Get("AccessKeyId"))
			f.log = append(f.log, "DeleteAccessKey "+v.Get("AccessKeyId"))
			fmt.Fprint(w, `<DeleteAccessKeyResponse/>`)
		case "DeleteUserPolicy":
			f.log = append(f.log, "DeleteUserPolicy "+iamUser)
			fmt.Fprint(w, `<DeleteUserPolicyResponse/>`)
		case "DeleteUser":
			delete(f.users, iamUser)
			f.log = append(f.log, "DeleteUser "+iamUser)
			fmt.Fprint(w, `<DeleteUserResponse/>`)
		case "GetCallerIdentity":
			if f.stsFail[region] {
				fail("RegionDisabledException")
				return
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>%s</Account><Arn>arn:aws:iam::%s:user/x</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, acct, acct)
		case "DescribeInstances":
			listing := v.Get("InstanceId.1") == ""
			if listing {
				// Time passes between listings: shutting-down instances
				// finish terminating, and late launches show up.
				f.listings[key+region]++
				if f.listings[key+region] == 3 {
					f.instances[key][region] = append(f.instances[key][region], f.late[region]...)
				}
			}
			var b strings.Builder
			for i, in := range f.instances[key][region] {
				if listing && f.hideAt[in.id] == f.listings[key+region] {
					continue // a lagging listing
				}
				if listing && in.state == "shutting-down" && !f.stuck {
					f.instances[key][region][i].state = "terminated"
					continue
				}
				if listing && in.state == "terminated" {
					continue
				}
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
			for i, in := range f.instances[key][region] {
				if in.id == v.Get("InstanceId.1") && in.state != "terminated" {
					f.instances[key][region][i].state = "shutting-down"
				}
			}
			f.terminated = append(f.terminated, key+" "+region+" "+v.Get("InstanceId.1"))
			f.log = append(f.log, "TerminateInstances "+v.Get("InstanceId.1"))
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)
		default:
			t.Errorf("unexpected action %s", v.Get("Action"))
			fail("InvalidAction")
		}
	}))
	t.Cleanup(srv.Close)
	if f.listings == nil {
		f.listings = map[string]int{}
	}
	oldEP, oldRes, oldWait, oldSettle, oldQuiet := accountEndpoints, accountResolver, revokeWait, revokeSettle, revokeQuiet
	revokeWait, revokeSettle, revokeQuiet = 2*time.Second, time.Millisecond, 20*time.Millisecond
	accountEndpoints = map[string]string{"ec2": srv.URL, "sts": srv.URL, "iam": srv.URL}
	accountResolver = func(profile string) *awsCredResolver {
		return staticResolver(awsCreds{AccessKeyID: "AK-" + profile, SecretAccessKey: "s"})
	}
	t.Cleanup(func() {
		accountEndpoints, accountResolver, revokeWait, revokeSettle, revokeQuiet = oldEP, oldRes, oldWait, oldSettle, oldQuiet
	})
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

// Revoking a machine stops its VMs (running, stopped and bake builders)
// before deleting its IAM user; a failure keeps the user so a re-run can
// finish.
func TestRevokeMachine(t *testing.T) {
	newFake := func() *fakeAccount {
		return &fakeAccount{
			accounts: map[string]string{"AK-admin": "111111111111"},
			instances: map[string]map[string][]fakeAcctInstance{"AK-admin": {
				"us-east-2": {
					{"i-run", "running", map[string]string{tagRunner: "aws", tagOwner: "m-1"}},
					{"i-stopped", "stopped", map[string]string{tagRunner: "aws", tagOwner: "m-1"}},
					{"i-builder", "stopped", map[string]string{tagBake: "aws", tagOwner: "m-1"}},
					{"i-theirs", "running", map[string]string{tagRunner: "aws", tagOwner: "m-2"}},
				},
				"us-west-2": {{"i-west", "running", map[string]string{tagRunner: "aws", tagOwner: "m-1"}}},
			}},
			late:     map[string][]fakeAcctInstance{"us-west-2": {{"i-late", "pending", map[string]string{tagRunner: "aws", tagOwner: "m-1"}}}},
			users:    map[string]map[string]string{"spotexec-mac": {tagMachine: "m-1"}},
			keys:     map[string][2]string{"AKMAC": {"spotexec-mac", "Active"}},
			failTerm: map[string]bool{},
		}
	}
	regions := []string{"us-east-2", "us-west-2"}
	logf := func(f string, a ...any) { t.Logf(f, a...) }

	t.Run("clean", func(t *testing.T) {
		f := newFake()
		f.hideAt = map[string]int{"i-west": 2}
		f.install(t)
		if err := RevokeMachine(context.Background(), "admin", "spotexec-mac", regions, logf); err != nil {
			t.Fatal(err)
		}
		// i-late was launched just before the key was deactivated and only
		// shows up on the third search; i-west is missing from the second
		// listing while it's still shutting down.
		want := "UpdateAccessKey AKMAC Inactive;TerminateInstances i-run;TerminateInstances i-stopped;TerminateInstances i-builder;TerminateInstances i-west;TerminateInstances i-late;DeleteAccessKey AKMAC;DeleteUserPolicy spotexec-mac;DeleteUser spotexec-mac"
		if got := strings.Join(f.log, ";"); got != want {
			t.Fatalf("calls:\n%s\nwant\n%s", got, want)
		}
	})
	for name, breakIt := range map[string]func(f *fakeAccount){
		"failed termination":  func(f *fakeAccount) { f.failTerm["i-builder"] = true },
		"failed region":       func(f *fakeAccount) { f.failRegion = map[string]bool{"us-west-2": true} },
		"stuck shutting down": func(f *fakeAccount) { f.stuck = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			breakIt(f)
			f.install(t)
			err := RevokeMachine(context.Background(), "admin", "spotexec-mac", regions, logf)
			if err == nil || !strings.Contains(err.Error(), "was kept") {
				t.Fatalf("err = %v", err)
			}
			if f.users["spotexec-mac"] == nil || f.keys["AKMAC"][1] != "Inactive" {
				t.Fatalf("users %v, keys %v", f.users, f.keys)
			}
			// Once the problem is gone, running it again finishes the job.
			f.mu.Lock()
			f.failTerm, f.failRegion, f.stuck = map[string]bool{}, nil, false
			f.mu.Unlock()
			if err := RevokeMachine(context.Background(), "admin", "spotexec-mac", regions, logf); err != nil {
				t.Fatal(err)
			}
			if f.users["spotexec-mac"] != nil || len(f.keys) != 0 {
				t.Fatalf("users %v, keys %v", f.users, f.keys)
			}
			for _, id := range []string{"i-run", "i-stopped", "i-builder", "i-west", "i-late"} {
				if !strings.Contains(strings.Join(f.log, ";"), "TerminateInstances "+id) {
					t.Fatalf("%s never terminated: %v", id, f.log)
				}
			}
			if strings.Contains(strings.Join(f.log, ";"), "i-theirs") {
				t.Fatalf("another machine's VM was terminated: %v", f.log)
			}
		})
	}
}

// An unlabelled (older) user can't have its VMs identified: revoke stops
// with its keys deactivated and the user kept.
func TestRevokeMachineUnlabelled(t *testing.T) {
	f := &fakeAccount{
		accounts: map[string]string{"AK-admin": "111111111111"},
		users:    map[string]map[string]string{"spotexec-old": {}},
		keys:     map[string][2]string{"AKOLD": {"spotexec-old", "Active"}},
	}
	f.install(t)
	err := RevokeMachine(context.Background(), "admin", "spotexec-old", []string{"us-east-2"}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "can't be identified") || f.users["spotexec-old"] == nil || f.keys["AKOLD"][1] != "Inactive" {
		t.Fatalf("err %v, users %v, keys %v", err, f.users, f.keys)
	}
	// The recovery the error describes: tag the user with the owner ID.
	f.users["spotexec-old"][tagMachine] = "m-9"
	if err := RevokeMachine(context.Background(), "admin", "spotexec-old", nil, t.Logf); err == nil || f.users["spotexec-old"] == nil {
		t.Fatalf("no regions: err %v", err)
	}
}
