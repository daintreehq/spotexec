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
	"sort"
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
	userPaths    map[string]string                        // IAM user -> path, when not /spotexec/
	keys         map[string][2]string                     // access key -> user, status
	log          []string                                 // IAM and EC2 mutations, in order
	late         map[string][]fakeAcctInstance            // region -> instances that appear from the third listing on
	hideAt       map[string]int                           // instance -> a listing (by number) that misses it
	listings     map[string]int                           // key+region -> DescribeInstances listings so far
	stuck        bool                                     // terminating instances never finish
	filters      url.Values
	signed       []string // "service Action region" of every IAM and STS call
	// extra serves anything else first (S3, ECR, more EC2); true = handled.
	extra func(w http.ResponseWriter, r *http.Request, key, region, service string, body []byte) bool
}

func (f *fakeAccount) install(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		m := regexp.MustCompile(`Credential=([^/]+)/[^/]+/([^/]+)/([^/]+)/`).FindStringSubmatch(r.Header.Get("Authorization"))
		key, region := m[1], m[2]
		body, _ := io.ReadAll(r.Body)
		if _, ok := f.accounts[key]; ok && f.extra != nil && f.extra(w, r, key, region, m[3], body) {
			return
		}
		v, _ := url.ParseQuery(string(body))
		if m[3] == "iam" || m[3] == "sts" {
			f.signed = append(f.signed, m[3]+" "+v.Get("Action")+" "+region)
		}
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
			path := "/spotexec/"
			if pp := f.userPaths[iamUser]; pp != "" {
				path = pp
			}
			fmt.Fprintf(w, `<GetUserResponse><GetUserResult><User><Arn>arn:aws:iam::%s:user%s%s</Arn><Tags>%s</Tags></User></GetUserResult></GetUserResponse>`, acct, path, iamUser, b.String())
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
		case "ListUsers":
			// One user per page, to exercise paging. PathPrefix matches
			// nested paths too, as IAM's does.
			var names []string
			for name := range f.users {
				path := "/spotexec/"
				if pp := f.userPaths[name]; pp != "" {
					path = pp
				}
				if strings.HasPrefix(path, v.Get("PathPrefix")) {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			page := 0
			fmt.Sscanf(v.Get("Marker"), "user-%d", &page)
			var b strings.Builder
			if page < len(names) {
				path := "/spotexec/"
				if pp := f.userPaths[names[page]]; pp != "" {
					path = pp
				}
				fmt.Fprintf(&b, `<member><UserName>%s</UserName><Path>%s</Path><CreateDate>2026-01-0%dT00:00:00Z</CreateDate></member>`, names[page], path, page+1)
			}
			more := "<IsTruncated>false</IsTruncated>"
			if page+1 < len(names) {
				more = fmt.Sprintf("<IsTruncated>true</IsTruncated><Marker>user-%d</Marker>", page+1)
			}
			fmt.Fprintf(w, `<ListUsersResponse><ListUsersResult><Users>%s</Users>%s</ListUsersResult></ListUsersResponse>`, b.String(), more)
		case "GetAccessKeyLastUsed":
			fmt.Fprint(w, `<GetAccessKeyLastUsedResponse><GetAccessKeyLastUsedResult><AccessKeyLastUsed/></GetAccessKeyLastUsedResult></GetAccessKeyLastUsedResponse>`)
		case "DeleteUser":
			delete(f.users, iamUser)
			f.log = append(f.log, "DeleteUser "+iamUser)
			fmt.Fprint(w, `<DeleteUserResponse/>`)
		case "GetCallerIdentity":
			if f.stsFail[region] {
				fail("RegionDisabledException")
				return
			}
			who := "x"
			if key == "AK-spotexec" {
				who = "spotexec/spotexec-mac" // a machine's own key
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>%s</Account><Arn>arn:aws:iam::%s:user/%s</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, acct, acct, who)
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
				if listing && in.state == "terminated" || listing && in.tags[tagRunner] == "" && in.tags[tagBake] == "" {
					continue // (the tag-key filter)
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
	oldTWait, oldTPoll := teardownWait, teardownPoll
	revokeWait, revokeSettle, revokeQuiet = 2*time.Second, time.Millisecond, 20*time.Millisecond
	teardownWait, teardownPoll = 2*time.Second, time.Millisecond
	accountEndpoints = map[string]string{"ec2": srv.URL, "sts": srv.URL, "iam": srv.URL, "ecr": srv.URL, "s3": srv.URL}
	accountResolver = func(profile string) *awsCredResolver {
		return staticResolver(awsCreds{AccessKeyID: "AK-" + profile, SecretAccessKey: "s"})
	}
	t.Cleanup(func() {
		accountEndpoints, accountResolver, revokeWait, revokeSettle, revokeQuiet = oldEP, oldRes, oldWait, oldSettle, oldQuiet
		teardownWait, teardownPoll = oldTWait, oldTPoll
	})
	dir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
	// No ambient AWS region or profile picks the partition.
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
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

// ListMachines reads every page of IAM users, and only those exactly
// under /spotexec/ (PathPrefix also matches /spotexec/nested/).
func TestListMachinesPagesAndFiltersPath(t *testing.T) {
	f := &fakeAccount{
		accounts: map[string]string{"AK-admin": "111111111111"},
		users: map[string]map[string]string{
			"spotexec-a": {tagMachine: "m-1"}, "spotexec-b": {tagMachine: "m-2"},
			"spotexec-c": {tagMachine: "m-3"}, "nested": {},
		},
		userPaths: map[string]string{"nested": "/spotexec/nested/"},
		keys:      map[string][2]string{"AKA": {"spotexec-a", "Active"}},
	}
	f.install(t)
	ms, err := ListMachines(context.Background(), "admin", []string{"us-east-2"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range ms {
		got = append(got, m.User+"="+m.MachineID)
	}
	if strings.Join(got, ",") != "spotexec-a=m-1,spotexec-b=m-2,spotexec-c=m-3" {
		t.Fatalf("machines %v", got)
	}
	if n := strings.Count(strings.Join(f.signed, ";"), "ListUsers"); n != 4 {
		t.Fatalf("ListUsers called %d times, want a call per page: %v", n, f.signed)
	}
}

// For GovCloud regions, IAM and STS are signed for us-gov-west-1, the
// partition's home, whichever GovCloud region the VMs are in.
func TestRevokeMachineGovCloud(t *testing.T) {
	f := &fakeAccount{
		accounts: map[string]string{"AK-admin": "111111111111"},
		instances: map[string]map[string][]fakeAcctInstance{"AK-admin": {
			"us-gov-east-1": {{"i-run", "running", map[string]string{tagRunner: "aws", tagOwner: "m-1"}}},
		}},
		users: map[string]map[string]string{"spotexec-mac": {tagMachine: "m-1"}},
		keys:  map[string][2]string{"AKMAC": {"spotexec-mac", "Active"}},
	}
	f.install(t)
	iamHomes := func() []string {
		var out []string
		for _, s := range f.signed {
			out = append(out, s[strings.LastIndex(s, " ")+1:])
		}
		f.signed = nil
		return out
	}
	// Setup on another machine checks the admin session in the runners'
	// partition, or the profile's when there are no runners.
	env, arn, err := AdminSession(context.Background(), "admin", []string{"us-gov-east-1"})
	if err != nil || env[0] != "AK-admin" || arn == "" || strings.Join(iamHomes(), ",") != "us-gov-west-1" {
		t.Fatalf("AdminSession: %v %q %v", env, arn, err)
	}
	t.Setenv("AWS_REGION", "us-gov-east-1")
	if _, _, err := AdminSession(context.Background(), "admin", nil); err != nil || strings.Join(iamHomes(), ",") != "us-gov-west-1" {
		t.Fatalf("AdminSession from the profile's region: %v", err)
	}
	t.Setenv("AWS_REGION", "")
	ms, err := ListMachines(context.Background(), "admin", []string{"us-gov-east-1"})
	if err != nil || len(ms) != 1 || ms[0].VMs != 1 {
		t.Fatalf("ListMachines %+v %v", ms, err)
	}
	for _, s := range f.signed {
		if strings.HasPrefix(s, "iam ") && !strings.HasSuffix(s, " us-gov-west-1") {
			t.Fatalf("ListMachines: IAM call outside GovCloud's home: %v", f.signed)
		}
	}
	if err := RevokeMachine(context.Background(), "admin", "spotexec-mac", []string{"us-gov-east-1"}, t.Logf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.log, ";"), "TerminateInstances i-run") || f.users["spotexec-mac"] != nil {
		t.Fatalf("log %v, users %v", f.log, f.users)
	}
	for _, s := range f.signed {
		if strings.HasPrefix(s, "iam ") && !strings.HasSuffix(s, " us-gov-west-1") {
			t.Fatalf("IAM call outside GovCloud's home: %v", f.signed)
		}
	}
	if _, err := ListMachines(context.Background(), "admin", []string{"us-gov-east-1", "us-east-1"}); err == nil || !strings.Contains(err.Error(), "different AWS partitions") {
		t.Fatalf("mixed partitions: %v", err)
	}
}

// A listing IAM says is truncated but gives no marker for is an error, not
// a complete list; what was read is kept (teardown reports the failure and
// still lists those users).
func TestListSpotexecUsersIncomplete(t *testing.T) {
	pages := []string{
		`<ListUsersResponse><ListUsersResult><Users><member><UserName>spotexec-a</UserName><Path>/spotexec/</Path></member></Users><IsTruncated>true</IsTruncated><Marker>m1</Marker></ListUsersResult></ListUsersResponse>`,
		`<ListUsersResponse><ListUsersResult><Users><member><UserName>spotexec-b</UserName><Path>/spotexec/</Path></member></Users><IsTruncated>true</IsTruncated></ListUsersResult></ListUsersResponse>`,
	}
	var markers []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		markers = append(markers, v.Get("Marker"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(pages[len(markers)-1])), Request: r}, nil
	})
	cl := &awsClient{region: "us-east-1", creds: staticResolver(awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}), http: &http.Client{Transport: rt}}
	p := &TeardownPlan{}
	err := p.listUsers(context.Background(), cl)
	if err == nil || !strings.Contains(err.Error(), "without a marker") || strings.Join(markers, ",") != ",m1" {
		t.Fatalf("err %v, markers %v", err, markers)
	}
	if len(p.Items) != 2 || p.Items[0].ID != "spotexec-a" || p.Items[1].ID != "spotexec-b" {
		t.Fatalf("items %+v", p.Items)
	}
}
