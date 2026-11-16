package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

const testMachine = "aaaaaaaa-1111-4222-8333-444444444444"

type fakeUser struct {
	path string
	tags map[string]string
}

type fakeKey struct {
	user    string
	created string
}

// fakeIAM serves the STS, IAM, EC2 and Service Quotas calls setup makes.
type fakeIAM struct {
	mu          sync.Mutex
	users       map[string]*fakeUser
	keys        map[string]fakeKey
	created     int
	calls       []string
	denyCreate  bool
	denyKey     bool           // CreateAccessKey fails
	neverWorks  bool           // new keys are never accepted
	propagation map[string]int // new key -> rejections left
	noVPC       bool
	foreign     map[string]string            // key -> ARN it signs in as, in another account
	stsDown     map[string]string            // key -> error code GetCallerIdentity fails with (not a key rejection)
	selfGetUser string                       // a user's key may GetUser itself (the current policy): "ok", or an error code
	sgs         map[string]map[string]string // security group id -> tags; "name"/"desc" override spotexec's
	sgFailFrom  int                          // DescribeSecurityGroups fails from this page on (0: never)
	policy      string                       // the last PutUserPolicy document
	signedIn    map[string]string            // action -> the region its last call was signed for
}

func (f *fakeIAM) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		cred := regexp.MustCompile(`Credential=([^/]+)/[^/]+/([^/]+)/`).FindStringSubmatch(r.Header.Get("Authorization"))
		key := cred[1]
		if tgt := r.Header.Get("X-Amz-Target"); tgt != "" {
			f.calls = append(f.calls, tgt)
			fmt.Fprint(w, `{"Quota":{"Value":16}}`)
			return
		}
		v, _ := url.ParseQuery(string(body))
		action := v.Get("Action")
		f.calls = append(f.calls, action)
		if f.signedIn == nil {
			f.signedIn = map[string]string{}
		}
		f.signedIn[action] = cred[2]
		iamErr := func(status int, code string) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `<ErrorResponse><Error><Code>%s</Code><Message>%s</Message></Error></ErrorResponse>`, code, code)
		}
		if n := f.propagation[key]; n > 0 {
			f.propagation[key] = n - 1
			iamErr(403, "InvalidClientTokenId")
			return
		}
		if a, ok := f.foreign[key]; ok {
			if action != "GetCallerIdentity" {
				iamErr(403, "AccessDenied")
				return
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>999988887777</Account><Arn>%s</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, a)
			return
		}
		if code := f.stsDown[key]; code != "" {
			iamErr(403, code)
			return
		}
		isAdmin := key == "AKIADMIN"
		if !isAdmin && f.keys[key].user == "" {
			iamErr(403, "InvalidClientTokenId")
			return
		}
		arn := func(name string) string {
			return "arn:aws:iam::111122223333:user" + f.users[name].path + name
		}
		user := func() *fakeUser { return f.users[v.Get("UserName")] }
		switch action {
		case "GetCallerIdentity":
			a := "arn:aws:iam::111122223333:root"
			if !isAdmin {
				a = arn(f.keys[key].user)
			}
			fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>111122223333</Account><Arn>%s</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`, a)
			return
		case "DescribeInstances":
			fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`)
			return
		case "DescribeVpcs":
			if f.noVPC {
				fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet/></DescribeVpcsResponse>`)
			} else {
				fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-default</vpcId></item></vpcSet></DescribeVpcsResponse>`)
			}
			return
		}
		if !isAdmin && f.selfGetUser != "" && action == "GetUser" && v.Get("UserName") == f.keys[key].user {
			if f.selfGetUser != "ok" {
				iamErr(400, f.selfGetUser)
				return
			}
		} else if !isAdmin {
			iamErr(403, "AccessDenied")
			return
		}
		switch action {
		case "DescribeSecurityGroups":
			if v.Get("Filter.1.Name") != "group-name" || v.Get("Filter.1.Value.1") != "spotexec-ssh" || v.Get("Filter.1.Value.2") != "spotexec-ssh-*" {
				t.Errorf("DescribeSecurityGroups filter %v", v)
			}
			// One group per page, to exercise paging.
			ids := make([]string, 0, len(f.sgs))
			for id := range f.sgs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			page, next := 0, ""
			fmt.Sscanf(v.Get("NextToken"), "page-%d", &page)
			if f.sgFailFrom > 0 && page >= f.sgFailFrom {
				iamErr(403, "UnauthorizedOperation")
				return
			}
			if page+1 < len(ids) {
				next = fmt.Sprintf("<nextToken>page-%d</nextToken>", page+1)
			}
			if page < len(ids) {
				ids = ids[page : page+1]
			} else {
				ids = nil
			}
			var b strings.Builder
			for _, id := range ids {
				tags := f.sgs[id]
				name, desc := "spotexec-ssh", "spotexec runners: SSH ingress"
				if tags["name"] != "" {
					name, desc = tags["name"], tags["desc"]
				}
				fmt.Fprintf(&b, `<item><groupId>%s</groupId><groupName>%s</groupName><groupDescription>%s</groupDescription><tagSet>`, id, name, desc)
				for k, val := range tags {
					if k == "name" || k == "desc" {
						continue
					}
					fmt.Fprintf(&b, `<item><key>%s</key><value>%s</value></item>`, k, val)
				}
				b.WriteString(`</tagSet></item>`)
			}
			fmt.Fprintf(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>%s</securityGroupInfo>%s</DescribeSecurityGroupsResponse>`, b.String(), next)
		case "CreateTags":
			tags := f.sgs[v.Get("ResourceId.1")]
			if tags == nil || v.Get("ResourceId.2") != "" {
				t.Errorf("CreateTags %v", v)
				iamErr(400, "InvalidID")
				return
			}
			tags[v.Get("Tag.1.Key")] = v.Get("Tag.1.Value")
			fmt.Fprint(w, `<CreateTagsResponse><return>true</return></CreateTagsResponse>`)
		case "CreateDefaultVpc":
			f.noVPC = false
			fmt.Fprint(w, `<CreateDefaultVpcResponse><vpc><vpcId>vpc-new</vpcId></vpc></CreateDefaultVpcResponse>`)
		case "CreateUser":
			if f.denyCreate {
				iamErr(403, "AccessDenied")
				return
			}
			if user() != nil {
				iamErr(409, "EntityAlreadyExists")
				return
			}
			u := &fakeUser{path: v.Get("Path"), tags: map[string]string{}}
			for i := 1; v.Get(fmt.Sprintf("Tags.member.%d.Key", i)) != ""; i++ {
				u.tags[v.Get(fmt.Sprintf("Tags.member.%d.Key", i))] = v.Get(fmt.Sprintf("Tags.member.%d.Value", i))
			}
			f.users[v.Get("UserName")] = u
			fmt.Fprintf(w, `<CreateUserResponse><CreateUserResult><User><Arn>%s</Arn></User></CreateUserResult></CreateUserResponse>`, arn(v.Get("UserName")))
		case "GetUser":
			if user() == nil {
				iamErr(404, "NoSuchEntity")
				return
			}
			var tags strings.Builder
			for k, val := range user().tags {
				fmt.Fprintf(&tags, `<member><Key>%s</Key><Value>%s</Value></member>`, k, val)
			}
			fmt.Fprintf(w, `<GetUserResponse><GetUserResult><User><Arn>%s</Arn><Tags>%s</Tags></User></GetUserResult></GetUserResponse>`, arn(v.Get("UserName")), tags.String())
		case "TagUser":
			user().tags[v.Get("Tags.member.1.Key")] = v.Get("Tags.member.1.Value")
			fmt.Fprint(w, `<TagUserResponse/>`)
		case "PutUserPolicy":
			var doc map[string]any
			if err := json.Unmarshal([]byte(v.Get("PolicyDocument")), &doc); err != nil || v.Get("PolicyName") != "spotexec" {
				t.Errorf("PutUserPolicy: %v %v", v.Get("PolicyName"), err)
			}
			f.policy = v.Get("PolicyDocument")
			fmt.Fprint(w, `<PutUserPolicyResponse/>`)
		case "CreateServiceLinkedRole":
			iamErr(400, "InvalidInput") // message lacks "has been taken": logged as a warning
		case "ListAccessKeys":
			var b strings.Builder
			for id, k := range f.keys {
				if k.user == v.Get("UserName") {
					fmt.Fprintf(&b, `<member><AccessKeyId>%s</AccessKeyId><CreateDate>%s</CreateDate></member>`, id, k.created)
				}
			}
			fmt.Fprintf(w, `<ListAccessKeysResponse><ListAccessKeysResult><AccessKeyMetadata>%s</AccessKeyMetadata></ListAccessKeysResult></ListAccessKeysResponse>`, b.String())
		case "CreateAccessKey":
			if len(f.keysOf(v.Get("UserName"))) >= 2 {
				iamErr(409, "LimitExceeded")
				return
			}
			if f.denyKey {
				iamErr(400, "ServiceFailure")
				return
			}
			f.created++
			id := fmt.Sprintf("AKNEW%d", f.created)
			f.keys[id] = fakeKey{user: v.Get("UserName"), created: time.Now().UTC().Format(time.RFC3339Nano)}
			f.propagation[id] = 2
			if f.neverWorks {
				f.propagation[id] = 1 << 30
			}
			fmt.Fprintf(w, `<CreateAccessKeyResponse><CreateAccessKeyResult><AccessKey><AccessKeyId>%s</AccessKeyId><SecretAccessKey>secret-%s</SecretAccessKey></AccessKey></CreateAccessKeyResult></CreateAccessKeyResponse>`, id, id)
		case "DeleteAccessKey":
			if f.keys[v.Get("AccessKeyId")].user != v.Get("UserName") {
				t.Errorf("DeleteAccessKey %s of the wrong user", v.Get("AccessKeyId"))
			}
			delete(f.keys, v.Get("AccessKeyId"))
			fmt.Fprint(w, `<DeleteAccessKeyResponse/>`)
		default:
			t.Errorf("unexpected action %s", action)
			iamErr(400, "InvalidAction")
		}
	}
}

func (f *fakeIAM) keysOf(user string) []string {
	var out []string
	for id, k := range f.keys {
		if k.user == user {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeIAM) count(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == action {
			n++
		}
	}
	return n
}

func newSetupEnv(t *testing.T, f *fakeIAM) (*AWSUserSetup, string, string) {
	t.Helper()
	if f.users == nil {
		f.users = map[string]*fakeUser{}
	}
	if f.keys == nil {
		f.keys = map[string]fakeKey{}
	}
	f.propagation = map[string]int{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	creds, cfg := filepath.Join(dir, "credentials"), filepath.Join(dir, "config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIADMIN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "admin-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	ep := map[string]string{"iam": srv.URL, "sts": srv.URL, "ec2": srv.URL, "servicequotas": srv.URL}
	s := &AWSUserSetup{
		Profile: "spotexec", DefaultName: "spotexec-test", MachineID: testMachine, Region: "us-east-2",
		Log: t.Logf, endpoints: ep, pollEvery: time.Millisecond,
		resolveProfile: func(_ context.Context, p string) (awsCreds, error) { return readSharedCredentials(p) },
	}
	return s, creds, cfg
}

func TestAWSUserSetupFresh(t *testing.T) {
	f := &fakeIAM{noVPC: true}
	s, creds, cfg := newSetupEnv(t, f)
	s.VPCRegions = []string{"us-east-2"}
	if err := os.WriteFile(creds, []byte("# mine\n[default]\naws_access_key_id = AKOTHER\naws_secret_access_key = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	confirmed := false
	s.Confirm = func(p AWSSetupPlan) bool {
		confirmed = p.Account == "111122223333" && len(p.CreateVPCIn) == 1 && p.UserName == "spotexec-test"
		return true
	}
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed || res.CreatedVPCs["us-east-2"] != "vpc-new" || !res.KeyCreated || res.AccessKeyID != "AKNEW1" || res.UserARN != "arn:aws:iam::111122223333:user/spotexec/spotexec-test" || res.Quotas["us-east-2"] != 16 {
		t.Fatalf("result %+v", res)
	}
	if f.users["spotexec-test"].tags[tagMachine] != testMachine {
		t.Fatalf("user tags %v", f.users["spotexec-test"].tags)
	}
	b, _ := os.ReadFile(creds)
	want := "# mine\n[default]\naws_access_key_id = AKOTHER\naws_secret_access_key = x\n\n[spotexec]\naws_access_key_id = AKNEW1\naws_secret_access_key = secret-AKNEW1\n"
	if string(b) != want {
		t.Fatalf("credentials file:\n%s", b)
	}
	if fi, _ := os.Stat(creds); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(cfg); string(b) != "[profile spotexec]\nregion = us-east-2\n" {
		t.Fatalf("config file:\n%s", b)
	}

	// Re-running keeps the working key and refreshes the policy.
	res, err = s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyReady || res.KeyCreated || res.PolicyStale || len(res.CreatedVPCs) != 0 || f.count("CreateDefaultVpc") != 1 || f.count("PutUserPolicy") != 2 || f.count("CreateAccessKey") != 1 {
		t.Fatalf("re-run: %+v, calls %v", res, f.calls)
	}

	// Without an admin session it still reports ready.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKEXPIRED")
	res, err = s.Run(context.Background())
	if err != nil || !res.AlreadyReady || !res.PolicyStale {
		t.Fatalf("no admin: %+v %v", res, err)
	}
}

func TestAWSUserSetupRotateMakesRoom(t *testing.T) {
	f := &fakeIAM{
		users: map[string]*fakeUser{"spotexec-test": {path: "/spotexec/", tags: map[string]string{tagMachine: testMachine}}},
		keys: map[string]fakeKey{
			"AKOLD1": {"spotexec-test", "2025-01-01T00:00:00Z"}, "AKOLD2": {"spotexec-test", "2025-06-01T00:00:00Z"},
		},
	}
	s, _, _ := newSetupEnv(t, f)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Both slots were taken: the oldest went first, the other after the new key worked.
	if strings.Join(res.KeysDeleted, ",") != "AKOLD1,AKOLD2" || strings.Join(f.keysOf("spotexec-test"), ",") != "AKNEW1" {
		t.Fatalf("deleted %v, keys %v", res.KeysDeleted, f.keys)
	}
	s.Rotate = true
	res, err = s.Run(context.Background())
	if err != nil || res.AccessKeyID != "AKNEW2" || strings.Join(res.KeysDeleted, ",") != "AKNEW1" {
		t.Fatalf("rotate: %+v %v", res, err)
	}
}

// With both key slots used, rotation makes room by deleting the key the
// profile doesn't use (even when it's the newer one), and retires the
// profile's old key only once the replacement works.
func TestAWSUserSetupRotateKeepsLocalKey(t *testing.T) {
	newEnv := func(t *testing.T) (*fakeIAM, *AWSUserSetup, string) {
		f := &fakeIAM{
			users: map[string]*fakeUser{"spotexec-test": {path: "/spotexec/", tags: map[string]string{tagMachine: testMachine}}},
			keys: map[string]fakeKey{
				"AKLOCAL": {"spotexec-test", "2025-01-01T00:00:00Z"}, "AKLEFTOVER": {"spotexec-test", "2025-06-01T00:00:00Z"},
			},
		}
		s, creds, _ := newSetupEnv(t, f)
		os.WriteFile(creds, []byte("[spotexec]\naws_access_key_id = AKLOCAL\naws_secret_access_key = x\n"), 0o600)
		s.Rotate = true
		return f, s, creds
	}
	t.Run("success", func(t *testing.T) {
		f, s, _ := newEnv(t)
		res, err := s.Run(context.Background())
		if err != nil || strings.Join(res.KeysDeleted, ",") != "AKLEFTOVER,AKLOCAL" || strings.Join(f.keysOf("spotexec-test"), ",") != "AKNEW1" {
			t.Fatalf("result %+v %v, keys %v", res, err, f.keys)
		}
	})
	t.Run("replacement never works", func(t *testing.T) {
		f, s, creds := newEnv(t)
		f.neverWorks, s.verifyFor = true, 50*time.Millisecond
		if _, err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "was left as it was") {
			t.Fatalf("err = %v", err)
		}
		if b, _ := os.ReadFile(creds); !strings.Contains(string(b), "AKLOCAL") || f.keys["AKLOCAL"].user == "" {
			t.Fatalf("credentials:\n%s\nkeys %v", b, f.keys)
		}
		if strings.Join(f.keysOf("spotexec-test"), ",") != "AKLOCAL" {
			t.Fatalf("the failed key wasn't cleaned up: %v", f.keys)
		}
	})
	for _, where := range []string{"credentials", "config"} {
		t.Run("profile doesn't resolve, key in "+where, func(t *testing.T) {
			// The key in the credentials or config file is still protected.
			f, s, creds := newEnv(t)
			if where == "config" {
				os.Remove(creds)
				os.WriteFile(os.Getenv("AWS_CONFIG_FILE"), []byte("[profile spotexec]\naws_access_key_id = AKLOCAL\naws_secret_access_key = x\n"), 0o600)
			}
			f.denyKey = true
			s.resolveProfile = func(context.Context, string) (awsCreds, error) { return awsCreds{}, errors.New("aws: exit status 255") }
			if _, err := s.Run(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			if f.keys["AKLOCAL"].user == "" || f.keys["AKLEFTOVER"].user != "" {
				t.Fatalf("keys %v", f.keys)
			}
		})
	}
	t.Run("replacement fails", func(t *testing.T) {
		f, s, creds := newEnv(t)
		f.denyKey = true
		if _, err := s.Run(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
		if f.keys["AKLOCAL"].user == "" {
			t.Fatalf("the working key was deleted: %v", f.keys)
		}
		if b, _ := os.ReadFile(creds); !strings.Contains(string(b), "AKLOCAL") {
			t.Fatalf("credentials:\n%s", b)
		}
	})
}

func TestAWSUserSetupLock(t *testing.T) {
	s := &AWSUserSetup{}
	creds := filepath.Join(t.TempDir(), "credentials")
	os.WriteFile(creds, nil, 0o600)
	link := filepath.Join(t.TempDir(), "credentials") // a symlinked dotfile in another directory
	if err := os.Symlink(creds, link); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(link)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		u, _ := s.lock(creds)
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("a second setup didn't wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the second setup never got the lock")
	}
}

func TestRoomKey(t *testing.T) {
	keys := []accessKey{{ID: "A", Status: "Active"}, {ID: "B", Status: "Inactive"}, {ID: "C", Status: "Active"}}
	for local, want := range map[string]int{"": 1, "B": 0, "A": 1} {
		if got := roomKey(keys, local); got != want {
			t.Errorf("roomKey(local %q) = %d, want %d", local, got, want)
		}
	}
	if roomKey(keys[:1], "A") != -1 {
		t.Error("the only key is local: nothing to delete")
	}
}

// Two machines with the same hostname must never share (or delete each
// other's) keys.
func TestAWSUserSetupHostnameCollision(t *testing.T) {
	for name, tags := range map[string]map[string]string{
		"other machine": {tagMachine: "bbbbbbbb-2222-4333-8444-555555555555"},
		"unlabelled":    {"spotexec-managed": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeIAM{
				users: map[string]*fakeUser{"spotexec-test": {path: "/spotexec/", tags: tags}},
				keys:  map[string]fakeKey{"AKTHEIRS": {"spotexec-test", "2025-01-01T00:00:00Z"}},
			}
			s, _, _ := newSetupEnv(t, f)
			res, err := s.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(res.UserARN, "/spotexec/spotexec-test-aaaaaa") || len(res.KeysDeleted) != 0 || f.keys["AKTHEIRS"].user == "" {
				t.Fatalf("result %+v, keys %v", res, f.keys)
			}
		})
	}
	t.Run("explicit name", func(t *testing.T) {
		f := &fakeIAM{users: map[string]*fakeUser{"spotexec-test": {path: "/spotexec/", tags: map[string]string{tagMachine: "other", "spotexec-host": "studio-09"}}}}
		s, _, _ := newSetupEnv(t, f)
		s.UserName = "spotexec-test"
		if _, err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "belongs to another machine (host studio-09)") {
			t.Fatalf("err = %v", err)
		}
	})
}

// A machine whose hostname changed keeps the user its profile signs in as,
// and an unlabelled one from an older spotexec gets labelled.
func TestAWSUserSetupKeepsProfileUser(t *testing.T) {
	f := &fakeIAM{
		users: map[string]*fakeUser{"spotexec-oldname": {path: "/spotexec/", tags: map[string]string{"spotexec-managed": "true"}}},
		keys:  map[string]fakeKey{"AKMINE": {"spotexec-oldname", "2025-01-01T00:00:00Z"}},
	}
	s, creds, _ := newSetupEnv(t, f)
	os.WriteFile(creds, []byte("[spotexec]\naws_access_key_id = AKMINE\naws_secret_access_key = x\n"), 0o600)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyReady || !strings.HasSuffix(res.UserARN, "/spotexec-oldname") || f.users["spotexec-test"] != nil || f.users["spotexec-oldname"].tags[tagMachine] != testMachine {
		t.Fatalf("result %+v, users %v", res, f.users)
	}
}

// A credentials file copied from another machine signs in as that
// machine's user: this one gets its own user and key, and the other
// machine's key is left alone, unless the user is named explicitly.
func TestAWSUserSetupCopiedCredentials(t *testing.T) {
	newEnv := func(t *testing.T) (*fakeIAM, *AWSUserSetup, string) {
		f := &fakeIAM{
			users: map[string]*fakeUser{"spotexec-mac-a": {path: "/spotexec/", tags: map[string]string{tagMachine: "bbbbbbbb-2222-4333-8444-555555555555", "spotexec-host": "mac-a"}}},
			keys:  map[string]fakeKey{"AKMACA": {"spotexec-mac-a", "2025-01-01T00:00:00Z"}},
		}
		s, creds, _ := newSetupEnv(t, f)
		os.WriteFile(creds, []byte("[spotexec]\naws_access_key_id = AKMACA\naws_secret_access_key = x\n"), 0o600)
		return f, s, creds
	}
	t.Run("own user", func(t *testing.T) {
		f, s, creds := newEnv(t)
		res, err := s.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.AlreadyReady || !res.KeyCreated || !strings.HasSuffix(res.UserARN, "/spotexec/spotexec-test") || len(res.KeysDeleted) != 0 {
			t.Fatalf("result %+v", res)
		}
		if f.keys["AKMACA"].user != "spotexec-mac-a" || f.users["spotexec-test"].tags[tagMachine] != testMachine {
			t.Fatalf("keys %v, users %v", f.keys, f.users)
		}
		if b, _ := os.ReadFile(creds); !strings.Contains(string(b), "aws_access_key_id = "+res.AccessKeyID) {
			t.Fatalf("credentials:\n%s", b)
		}
	})
	// Without an administrator, the key reads its own user's tags and setup
	// refuses to report it ready for this machine.
	for name, adminKey := range map[string]string{"no admin session": "AKEXPIRED", "profile as admin": "AKMACA"} {
		t.Run(name, func(t *testing.T) {
			for selfGetUser, want := range map[string]string{
				"ok":              "belongs to another machine (host mac-a), so this machine needs its own",
				"ValidationError": "couldn't check which machine IAM user spotexec-mac-a",
			} {
				f, s, creds := newEnv(t)
				f.selfGetUser = selfGetUser
				t.Setenv("AWS_ACCESS_KEY_ID", adminKey)
				before, _ := os.ReadFile(creds)
				_, err := s.Run(context.Background())
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: err = %v", selfGetUser, err)
				}
				if after, _ := os.ReadFile(creds); string(after) != string(before) || f.count("CreateAccessKey") != 0 {
					t.Fatalf("changed: %s %v", after, f.calls)
				}
			}
		})
	}
	t.Run("named explicitly", func(t *testing.T) {
		f, s, _ := newEnv(t)
		s.UserName = "spotexec-mac-a"
		res, err := s.Run(context.Background())
		if err != nil || !res.AlreadyReady || res.AccessKeyID != "AKMACA" || f.count("CreateAccessKey") != 0 {
			t.Fatalf("result %+v %v", res, err)
		}
	})
}

// A profile for one account and an admin session for another: setup stops
// before changing anything.
func TestAWSUserSetupAccountMismatch(t *testing.T) {
	f := &fakeIAM{foreign: map[string]string{"AKOTHERACCT": "arn:aws:iam::999988887777:user/spotexec/spotexec-test"}}
	s, creds, _ := newSetupEnv(t, f)
	before := "[spotexec]\naws_access_key_id = AKOTHERACCT\naws_secret_access_key = x\n"
	os.WriteFile(creds, []byte(before), 0o600)
	_, err := s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signs in to account 999988887777") || !strings.Contains(err.Error(), "admin session is for account 111122223333") {
		t.Fatalf("err = %v", err)
	}
	for _, a := range []string{"CreateUser", "PutUserPolicy", "CreateAccessKey", "CreateDefaultVpc", "TagUser"} {
		if f.count(a) != 0 {
			t.Fatalf("%s called: %v", a, f.calls)
		}
	}
	if b, _ := os.ReadFile(creds); string(b) != before {
		t.Fatalf("credentials changed:\n%s", b)
	}
}

// A profile whose account can't be determined (STS failing for another
// reason than a bad key) is not replaced.
func TestAWSUserSetupProfileAccountUnknown(t *testing.T) {
	// AccessDenied (e.g. an endpoint policy) doesn't prove the key is bad.
	for _, code := range []string{"RegionDisabledException", "AccessDenied"} {
		t.Run(code, func(t *testing.T) {
			f := &fakeIAM{stsDown: map[string]string{"AKMAYBE": code}}
			s, creds, _ := newSetupEnv(t, f)
			before := "[spotexec]\naws_access_key_id = AKMAYBE\naws_secret_access_key = x\n"
			os.WriteFile(creds, []byte(before), 0o600)
			_, err := s.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "couldn't check which AWS account profile") || f.count("CreateUser") != 0 {
				t.Fatalf("err = %v, calls %v", err, f.calls)
			}
			if b, _ := os.ReadFile(creds); string(b) != before {
				t.Fatalf("credentials changed:\n%s", b)
			}
		})
	}
}

func TestAWSUserSetupRefusals(t *testing.T) {
	t.Run("not an admin", func(t *testing.T) {
		s, _, _ := newSetupEnv(t, &fakeIAM{denyCreate: true})
		_, err := s.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "can't manage IAM users") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("user not made by spotexec", func(t *testing.T) {
		s, _, _ := newSetupEnv(t, &fakeIAM{users: map[string]*fakeUser{"spotexec-test": {path: "/", tags: map[string]string{}}}})
		s.UserName = "spotexec-test" // asked for by name: refuse rather than adopt it
		_, err := s.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "wasn't created by spotexec") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("sso profile", func(t *testing.T) {
		s, _, cfg := newSetupEnv(t, &fakeIAM{})
		os.WriteFile(cfg, []byte("[profile spotexec]\nsso_session = corp\n"), 0o600)
		_, err := s.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "signs in another way") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("declined", func(t *testing.T) {
		f := &fakeIAM{}
		s, _, _ := newSetupEnv(t, f)
		s.Confirm = func(AWSSetupPlan) bool { return false }
		if _, err := s.Run(context.Background()); err != ErrSetupDeclined || f.count("CreateUser") != 0 {
			t.Fatalf("err = %v, calls %v", err, f.calls)
		}
	})
}

func TestUpsertINISection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials")
	os.WriteFile(p, []byte("[default]\nregion=x\n\n[spotexec]\n# keep\naws_access_key_id = OLD\naws_session_token = tok\n\n[other]\nk = v\n"), 0o644)
	if err := upsertINISection(p, "spotexec", [][2]string{{"aws_access_key_id", "NEW"}, {"aws_secret_access_key", "S"}}, []string{"aws_session_token"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "[default]\nregion=x\n\n[spotexec]\n# keep\naws_access_key_id = NEW\naws_secret_access_key = S\n\n[other]\nk = v\n"
	if string(b) != want {
		t.Fatalf("got:\n%s", b)
	}
	// A symlinked file stays a symlink.
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	os.WriteFile(real, nil, 0o600)
	os.Symlink(real, link)
	if err := upsertINISection(link, "profile a", [][2]string{{"region", "r"}}, nil); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if b, _ := os.ReadFile(real); string(b) != "[profile a]\nregion = r\n" {
		t.Fatalf("got %q", b)
	}
}

// Section headers may carry a trailing comment, as the AWS CLI allows: an
// upsert must find its own section and never run into (and rewrite) the next.
func TestINIHeaderComments(t *testing.T) {
	for _, c := range []struct {
		line, name string
		ok         bool
	}{
		{"[work]", "work", true},
		{"[work] # prod keys", "work", true},
		{"[profile  work] ; note", "profile  work", true},
		{"[ spotexec ]#x", " spotexec ", true},
		{"[]", "", false},
		{"# [work]", "", false},
		{"key = [v]", "", false},
	} {
		if name, ok := iniHeader(c.line); name != c.name || ok != c.ok {
			t.Errorf("iniHeader(%q) = %q, %v", c.line, name, ok)
		}
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "credentials")
	// [spotexec] followed by a commented header: work's keys must survive.
	os.WriteFile(p, []byte("[spotexec]\naws_access_key_id = OLD\n\n[work] # prod keys\naws_access_key_id = AKWORK\naws_secret_access_key = w\n"), 0o600)
	if err := upsertINISection(p, "spotexec", [][2]string{{"aws_access_key_id", "NEW"}, {"aws_secret_access_key", "S"}}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "[spotexec]\naws_access_key_id = NEW\naws_secret_access_key = S\n\n[work] # prod keys\naws_access_key_id = AKWORK\naws_secret_access_key = w\n"
	if string(b) != want {
		t.Fatalf("got:\n%s", b)
	}
	if c, err := parseSharedCredentials(bufio.NewScanner(strings.NewReader(string(b))), "work", "creds"); err != nil || c.AccessKeyID != "AKWORK" {
		t.Fatalf("work after upsert: %+v %v", c, err)
	}

	// A commented [spotexec] header is updated in place, not duplicated.
	os.WriteFile(p, []byte("[default]\nregion = x\n\n[spotexec] # spotexec's machine key\naws_access_key_id = OLD\naws_secret_access_key = O\n"), 0o600)
	if err := upsertINISection(p, "spotexec", [][2]string{{"aws_access_key_id", "NEW"}, {"aws_secret_access_key", "S"}}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	want = "[default]\nregion = x\n\n[spotexec] # spotexec's machine key\naws_access_key_id = NEW\naws_secret_access_key = S\n"
	if string(b) != want {
		t.Fatalf("got:\n%s", b)
	}
	if c, err := parseSharedCredentials(bufio.NewScanner(strings.NewReader(string(b))), "spotexec", "creds"); err != nil || c.AccessKeyID != "NEW" {
		t.Fatalf("spotexec after upsert: %+v %v", c, err)
	}

	// Credentials section names are matched verbatim, as the AWS CLI does:
	// "[team  prod]" and "[ spotexec ]" are other profiles, left untouched.
	os.WriteFile(p, []byte("[team prod]\naws_access_key_id = A1\naws_secret_access_key = a\n[team  prod] # two spaces\naws_access_key_id = A2\naws_secret_access_key = b\n[ spotexec ]#x\naws_access_key_id = KEEP\n"), 0o600)
	if err := upsertINISection(p, "spotexec", [][2]string{{"aws_access_key_id", "NEW"}}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if !strings.HasSuffix(string(b), "[ spotexec ]#x\naws_access_key_id = KEEP\n\n[spotexec]\naws_access_key_id = NEW\n") {
		t.Fatalf("got:\n%s", b)
	}
	for name, want := range map[string]string{"team prod": "A1", "team  prod": "A2"} {
		if c, err := parseSharedCredentials(bufio.NewScanner(strings.NewReader(string(b))), name, "creds"); err != nil || c.AccessKeyID != want {
			t.Fatalf("%q: %+v %v", name, c, err)
		}
	}

	// The config readers see commented headers too.
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte("[profile spotexec] # machine key\nsso_session = corp\n[profile other] # x\nregion = r\n"), 0o600)
	if vals, err := readINISection(cfg, "profile spotexec"); err != nil || vals["sso_session"] != "corp" || vals["region"] != "" {
		t.Fatalf("readINISection: %v %v", vals, err)
	}
	if vals, err := readINI(cfg, "profile other"); err != nil || vals["region"] != "r" || vals["sso_session"] != "" {
		t.Fatalf("readINI: %v %v", vals, err)
	}
}

// Security groups from before spotexec tagged them at creation get the
// owner tag on the next setup, so the scoped policy still lets the key
// add rules to them; tagged ones are left alone.
func TestAWSUserSetupTagsOldSecurityGroups(t *testing.T) {
	f := &fakeIAM{sgs: map[string]map[string]string{
		"sg-old":   {"Name": "spotexec-ssh"},
		"sg-cidr":  {"name": "spotexec-ssh-0a1b2c3d", "desc": "spotexec runners: SSH ingress"},
		"sg-new":   {tagOwner: "someone-else"},
		"sg-prod":  {"name": "spotexec-ssh-production", "desc": "spotexec runners: SSH ingress"},
		"sg-other": {"name": "spotexec-ssh", "desc": "hand-made"},
	}}
	s, _, _ := newSetupEnv(t, f)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.sgs["sg-old"][tagOwner] != testMachine || f.sgs["sg-old"]["Name"] != "spotexec-ssh" || f.sgs["sg-cidr"][tagOwner] != testMachine ||
		f.sgs["sg-new"][tagOwner] != "someone-else" || f.sgs["sg-prod"][tagOwner] != "" || f.sgs["sg-other"][tagOwner] != "" || f.count("CreateTags") != 2 {
		t.Fatalf("groups %v, calls %v", f.sgs, f.calls)
	}
}

// A listing that fails part-way still tags the groups already read, and
// says so; the rest wait for the next setup.
func TestAWSUserSetupTagsOldSecurityGroupsPartly(t *testing.T) {
	f := &fakeIAM{sgFailFrom: 1, sgs: map[string]map[string]string{
		"sg-a": {"Name": "spotexec-ssh"},
		"sg-b": {"Name": "spotexec-ssh"},
	}}
	s, _, _ := newSetupEnv(t, f)
	var logged []string
	s.Log = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.sgs["sg-a"][tagOwner] != testMachine || f.sgs["sg-b"][tagOwner] != "" {
		t.Fatalf("groups %v", f.sgs)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "couldn't list all of spotexec's security groups in us-east-2") {
		t.Fatalf("no warning: %v", logged)
	}
}

// In GovCloud, setup signs in to that partition's IAM and STS, and the
// policy it attaches names GovCloud ARNs.
func TestAWSUserSetupGovCloud(t *testing.T) {
	f := &fakeIAM{}
	s, _, _ := newSetupEnv(t, f)
	s.Region, s.QuotaRegions = "us-gov-east-1", []string{"us-gov-east-1"}
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.policy != compactJSON(PolicyFor("us-gov-east-1")) || strings.Contains(f.policy, `"arn:aws:`) {
		t.Fatalf("policy %s", f.policy)
	}
	for _, a := range []string{"CreateUser", "PutUserPolicy", "CreateAccessKey"} {
		if f.signedIn[a] != "us-gov-west-1" {
			t.Fatalf("%s signed for %q, want us-gov-west-1 (%v)", a, f.signedIn[a], f.signedIn)
		}
	}
	if f.signedIn["DescribeSecurityGroups"] != "us-gov-east-1" {
		t.Fatalf("regional calls moved: %v", f.signedIn)
	}
}

// One setup serves one partition: regions from two are refused before
// anything is called.
func TestAWSUserSetupMixedPartitions(t *testing.T) {
	f := &fakeIAM{}
	s, _, _ := newSetupEnv(t, f)
	for _, set := range []func(){
		func() { s.VPCRegions, s.QuotaRegions = []string{"us-gov-west-1"}, nil },
		func() { s.VPCRegions, s.QuotaRegions = nil, []string{"us-east-2", "us-gov-east-1"} },
	} {
		set()
		if _, err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "different AWS partitions") || len(f.calls) != 0 {
			t.Fatalf("err %v, calls %v", err, f.calls)
		}
	}
}

// docs/aws.md shows the policy for manual setup; it must match what
// `spotexec setup` attaches.
func TestPolicyMatchesREADME(t *testing.T) {
	b, err := os.ReadFile("../../docs/aws.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```json\n(\\{\n  \"Version\": \"2012-10-17\".*?\n\\})\n```").FindSubmatch(b)
	if m == nil {
		t.Fatal("policy JSON block not found in docs/aws.md")
	}
	if compactJSON(string(m[1])) != compactJSON(SpotexecPolicy) {
		t.Fatal("docs/aws.md policy differs from SpotexecPolicy")
	}
}

// Setup run from another machine acts only with the session that machine
// passed, never this machine's own AWS login.
func TestAWSUserSetupAdminFromEnv(t *testing.T) {
	f := &fakeIAM{}
	s, creds, _ := newSetupEnv(t, f)
	s.AdminFromEnv = true
	// This machine's own login could manage IAM users...
	if err := os.WriteFile(creds, []byte("[default]\naws_access_key_id = AKIADMIN\naws_secret_access_key = admin-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_PROFILE", "default")
	// ...but nothing was passed.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	_, err := s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no AWS sign-in was passed") {
		t.Fatalf("err = %v", err)
	}
	if f.count("CreateUser") != 0 || f.count("GetCallerIdentity") != 0 {
		t.Fatalf("calls %v", f.calls)
	}

	// A machine whose key still works is refreshed without one.
	f.users = map[string]*fakeUser{"spotexec-test": {path: "/spotexec/", tags: map[string]string{tagMachine: testMachine}}}
	f.keys = map[string]fakeKey{"AKMINE": {"spotexec-test", "2025-01-01T00:00:00Z"}}
	if err := os.WriteFile(creds, []byte("[default]\naws_access_key_id = AKIADMIN\naws_secret_access_key = admin-secret\n[spotexec]\naws_access_key_id = AKMINE\naws_secret_access_key = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := s.Run(context.Background())
	if err != nil || !res.AlreadyReady || !res.PolicyStale || f.count("PutUserPolicy") != 0 {
		t.Fatalf("%+v %v, calls %v", res, err, f.calls)
	}

	// With the passed session, the policy is refreshed.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIADMIN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "admin-secret")
	if res, err := s.Run(context.Background()); err != nil || res.PolicyStale || f.count("PutUserPolicy") != 1 {
		t.Fatalf("%+v %v, calls %v", res, err, f.calls)
	}
}

// Setup writes the key where ~ and $VAR in the AWS file variables point,
// never to a literal ./~/ path under the working directory.
func TestAWSUserSetupExpandsFilePaths(t *testing.T) {
	f := &fakeIAM{}
	s, creds, cfg := newSetupEnv(t, f)
	home := filepath.Dir(creds)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SPOTEXEC_TEST_AWS", filepath.Dir(cfg))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "~/credentials")
	t.Setenv("AWS_CONFIG_FILE", "${SPOTEXEC_TEST_AWS}/config")
	cwd := t.TempDir()
	t.Chdir(cwd)
	s.Confirm = func(AWSSetupPlan) bool { return true }
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(creds); !strings.Contains(string(b), "aws_secret_access_key = secret-AKNEW1") {
		t.Fatalf("credentials file:\n%s", b)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "[profile spotexec]\nregion = us-east-2") {
		t.Fatalf("config file:\n%s", b)
	}
	if ents, _ := os.ReadDir(cwd); len(ents) != 0 {
		t.Fatalf("working directory holds %v", ents)
	}
}
