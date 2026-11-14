package provider

import (
	"context"
	"encoding/json"
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
	propagation map[string]int // new key -> rejections left
	noVPC       bool
}

func (f *fakeIAM) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		key := regexp.MustCompile(`Credential=([^/]+)/`).FindStringSubmatch(r.Header.Get("Authorization"))[1]
		if tgt := r.Header.Get("X-Amz-Target"); tgt != "" {
			f.calls = append(f.calls, tgt)
			fmt.Fprint(w, `{"Quota":{"Value":16}}`)
			return
		}
		v, _ := url.ParseQuery(string(body))
		action := v.Get("Action")
		f.calls = append(f.calls, action)
		iamErr := func(status int, code string) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `<ErrorResponse><Error><Code>%s</Code><Message>%s</Message></Error></ErrorResponse>`, code, code)
		}
		if n := f.propagation[key]; n > 0 {
			f.propagation[key] = n - 1
			iamErr(403, "InvalidClientTokenId")
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
		if !isAdmin {
			iamErr(403, "AccessDenied")
			return
		}
		switch action {
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
			f.created++
			id := fmt.Sprintf("AKNEW%d", f.created)
			f.keys[id] = fakeKey{user: v.Get("UserName"), created: time.Now().UTC().Format(time.RFC3339Nano)}
			f.propagation[id] = 2
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
