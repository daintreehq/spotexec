package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
)

// strList is an IAM policy value that may be a string or a list of them.
type strList []string

func (l *strList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*l = strList{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(l))
}

type policyStatement struct {
	Effect      string
	Action      strList
	Resource    strList
	NotResource strList
	Condition   map[string]map[string]strList
}

func iamGlob(pattern, s string, fold bool) bool {
	re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$"
	if fold {
		re = "(?i)" + re
	}
	return regexp.MustCompile(re).MatchString(s)
}

// allowed is a small model of IAM's evaluation of an allow-only policy for
// one resource of a request: some statement must match the action and the
// resource, and all of its conditions must hold for ctx.
func allowed(t *testing.T, stmts []policyStatement, action, arn string, ctx map[string]string) bool {
	t.Helper()
	anyGlob := func(pats strList, s string, fold bool) bool {
		for _, p := range pats {
			if iamGlob(p, s, fold) {
				return true
			}
		}
		return false
	}
	for _, st := range stmts {
		if st.Effect != "Allow" || !anyGlob(st.Action, action, true) {
			continue
		}
		if len(st.Resource) > 0 && !anyGlob(st.Resource, arn, false) || len(st.NotResource) > 0 && anyGlob(st.NotResource, arn, false) {
			continue
		}
		ok := true
		for op, kv := range st.Condition {
			for k, want := range kv {
				got, present := ctx[k]
				switch op {
				case "Null":
					ok = ok && (want[0] == "true") == !present
				case "StringEquals":
					hit := false
					for _, w := range want {
						hit = hit || (present && got == w)
					}
					ok = ok && hit
				default:
					t.Fatalf("condition operator %s isn't modelled", op)
				}
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// The machine key may only change EC2 resources spotexec created and
// tagged, and may only tag resources while creating them.
func TestPolicyScopesMutationsToSpotexecResources(t *testing.T) {
	var p struct{ Statement []policyStatement }
	if err := json.Unmarshal([]byte(SpotexecPolicy), &p); err != nil {
		t.Fatal(err)
	}
	const acct = "111122223333"
	arn := func(kind string) string {
		if kind == "image" || kind == "snapshot" {
			return "arn:aws:ec2:us-east-2::" + kind + "/x-1"
		}
		return "arn:aws:ec2:us-east-2:" + acct + ":" + kind + "/x-1"
	}
	tagged := map[string]string{"aws:ResourceTag/spotexec-owner": "u-1"}
	untagged := map[string]string{"aws:ResourceTag/Name": "prod-db"}
	requested := map[string]string{"aws:RequestTag/spotexec-owner": "u-1"}
	cases := []struct {
		action, kind string
		ctx          map[string]string
		want         bool
	}{
		// Reads stay unconditional.
		{"ec2:DescribeInstances", "instance", nil, true},
		{"ec2:DescribeImages", "image", nil, true},
		{"ec2:DescribeSecurityGroups", "security-group", nil, true},
		// Launching: the instance and its volumes must be requested with the
		// owner tag; what the launch uses needn't be spotexec's.
		{"ec2:RunInstances", "instance", requested, true},
		{"ec2:RunInstances", "volume", requested, true},
		{"ec2:RunInstances", "instance", nil, false},
		{"ec2:RunInstances", "volume", nil, false},
		{"ec2:RunInstances", "image", nil, true},
		{"ec2:RunInstances", "subnet", nil, true},
		{"ec2:RunInstances", "security-group", nil, true},
		{"ec2:RunInstances", "network-interface", nil, true},
		{"ec2:RunInstances", "spot-instances-request", nil, true},
		// Tags only as part of creating something.
		{"ec2:CreateTags", "instance", map[string]string{"ec2:CreateAction": "RunInstances"}, true},
		{"ec2:CreateTags", "security-group", map[string]string{"ec2:CreateAction": "CreateSecurityGroup"}, true},
		{"ec2:CreateTags", "image", map[string]string{"ec2:CreateAction": "CreateImage"}, true},
		{"ec2:CreateTags", "instance", nil, false},
		{"ec2:CreateTags", "instance", map[string]string{"ec2:CreateAction": "CreateVolume"}, false},
		// Changing or deleting: spotexec's resources only.
		{"ec2:TerminateInstances", "instance", tagged, true},
		{"ec2:TerminateInstances", "instance", untagged, false},
		{"ec2:StopInstances", "instance", tagged, true},
		{"ec2:StopInstances", "instance", untagged, false},
		{"ec2:CreateImage", "instance", tagged, true},
		{"ec2:CreateImage", "instance", untagged, false},
		{"ec2:CreateImage", "image", requested, true},
		{"ec2:CreateImage", "snapshot", requested, true},
		{"ec2:DeregisterImage", "image", tagged, true},
		{"ec2:DeregisterImage", "image", untagged, false},
		{"ec2:DeleteSnapshot", "snapshot", tagged, true},
		{"ec2:DeleteSnapshot", "snapshot", untagged, false},
		{"ec2:AuthorizeSecurityGroupIngress", "security-group", tagged, true},
		{"ec2:AuthorizeSecurityGroupIngress", "security-group", untagged, false},
		{"ec2:AuthorizeSecurityGroupIngress", "security-group-rule", nil, true},
		{"ec2:CreateSecurityGroup", "security-group", nil, true},
		// Never granted.
		{"ec2:DeleteTags", "instance", tagged, false},
		{"ec2:ModifyInstanceAttribute", "instance", tagged, false},
		{"ec2:DeleteSecurityGroup", "security-group", tagged, false},
		{"ec2:RevokeSecurityGroupIngress", "security-group", tagged, false},
		{"ec2:DeleteVolume", "volume", tagged, false},
	}
	for _, c := range cases {
		if got := allowed(t, p.Statement, c.action, arn(c.kind), c.ctx); got != c.want {
			t.Errorf("%s on %s with %v: allowed = %v, want %v", c.action, c.kind, c.ctx, got, c.want)
		}
	}
	// Every action the code calls with the machine key is granted somewhere.
	for _, a := range []string{"ec2:DescribeVpcs", "ec2:DescribeSubnets", "ssm:GetParameter", "ecr:GetAuthorizationToken", "servicequotas:GetServiceQuota"} {
		found := false
		for _, st := range p.Statement {
			for _, pat := range st.Action {
				found = found || iamGlob(pat, a, true)
			}
		}
		if !found {
			t.Errorf("%s isn't granted", a)
		}
	}
}

// IAM caps a user's inline policies at 2,048 characters, not counting
// whitespace; PutUserPolicy rejects anything longer.
func TestPolicyFitsInline(t *testing.T) {
	n := len(strings.Join(strings.Fields(compactJSON(SpotexecPolicy)), ""))
	if n > 2048 {
		t.Fatalf("SpotexecPolicy is %d characters; an inline user policy may have 2048", n)
	}
}

// fakeEC2 serves the EC2 query calls securityGroups makes.
type fakeEC2 struct {
	mu        sync.Mutex
	groups    string // DescribeSecurityGroups securityGroupInfo body
	denyAuth  bool
	createReq url.Values
}

func (f *fakeEC2) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(body))
		switch v.Get("Action") {
		case "DescribeVpcs":
			fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId></item></vpcSet></DescribeVpcsResponse>`)
		case "DescribeSecurityGroups":
			fmt.Fprintf(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>%s</securityGroupInfo></DescribeSecurityGroupsResponse>`, f.groups)
		case "CreateSecurityGroup":
			f.createReq = v
			fmt.Fprint(w, `<CreateSecurityGroupResponse><return>true</return><groupId>sg-made</groupId></CreateSecurityGroupResponse>`)
		case "AuthorizeSecurityGroupIngress":
			if f.denyAuth {
				w.WriteHeader(403)
				fmt.Fprint(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>You are not authorized to perform this operation.</Message></Error></Errors></Response>`)
				return
			}
			fmt.Fprint(w, `<AuthorizeSecurityGroupIngressResponse><return>true</return></AuthorizeSecurityGroupIngressResponse>`)
		default:
			t.Errorf("unexpected EC2 action %s", v.Get("Action"))
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeSpot(t *testing.T, srv *httptest.Server) *AWSSpot {
	return &AWSSpot{
		r:     &config.Runner{Name: "aws", Region: "us-east-2", SSHCIDR: "0.0.0.0/0"},
		owner: "u-1",
		api: &awsClient{region: "us-east-2", creds: staticResolver(awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}),
			http: srv.Client(), endpoints: map[string]string{"ec2": srv.URL}},
	}
}

// The group spotexec creates carries its tags from creation, so the
// scoped policy lets the key add its SSH rule.
func TestSecurityGroupTaggedOnCreate(t *testing.T) {
	f := &fakeEC2{}
	a := fakeSpot(t, f.server(t))
	ids, err := a.securityGroups(context.Background(), func(string, ...any) {})
	if err != nil || len(ids) != 1 || ids[0] != "sg-made" {
		t.Fatalf("groups %v, err %v", ids, err)
	}
	tags := map[string]string{}
	for i := 1; f.createReq.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i)) != ""; i++ {
		tags[f.createReq.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))] = f.createReq.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))
	}
	if f.createReq.Get("TagSpecification.1.ResourceType") != "security-group" || tags[tagOwner] != "u-1" || tags["Name"] != "spotexec-ssh" {
		t.Fatalf("CreateSecurityGroup %v", f.createReq)
	}
}

// An older, untagged group that already allows SSH keeps working without
// any mutating call; one that needs a rule the policy now refuses says how
// to fix it.
func TestSecurityGroupUntaggedLegacy(t *testing.T) {
	withRule := `<item><groupId>sg-old</groupId><groupName>spotexec-ssh</groupName><ipPermissions><item><ipProtocol>tcp</ipProtocol><fromPort>22</fromPort><toPort>22</toPort><ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges></item></ipPermissions></item>`
	f := &fakeEC2{groups: withRule, denyAuth: true}
	a := fakeSpot(t, f.server(t))
	if ids, err := a.securityGroups(context.Background(), func(string, ...any) {}); err != nil || ids[0] != "sg-old" {
		t.Fatalf("groups %v, err %v", ids, err)
	}
	f.groups = `<item><groupId>sg-old</groupId><groupName>spotexec-ssh</groupName></item>`
	_, err := a.securityGroups(context.Background(), func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "run `spotexec setup` once") {
		t.Fatalf("err = %v", err)
	}
	f.groups = `<item><groupId>sg-old</groupId><groupName>spotexec-ssh</groupName><tagSet><item><key>spotexec-owner</key><value>u-1</value></item></tagSet></item>`
	if _, err := a.securityGroups(context.Background(), func(string, ...any) {}); err == nil || strings.Contains(err.Error(), "spotexec setup") {
		t.Fatalf("tagged group: err = %v", err)
	}
}
