package provider

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// AWS docs: "Examples of how to derive a signing key for Signature Version 4".
func TestSigV4SigningKey(t *testing.T) {
	k := sigV4SigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got := hex.EncodeToString(k); got != want {
		t.Fatalf("signing key = %s, want %s", got, want)
	}
}

// AWS SigV4 test suite vectors (aws-sig-v4-test-suite).
func TestSigV4Suite(t *testing.T) {
	creds := awsCreds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	cases := []struct {
		name, method, ctype, body, signed, sig string
	}{
		{"get-vanilla", "GET", "", "", "host;x-amz-date", "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"post-x-www-form-urlencoded", "POST", "application/x-www-form-urlencoded", "Param1=value1", "content-type;host;x-amz-date", "ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, "https://example.amazonaws.com/", strings.NewReader(c.body))
		if c.ctype != "" {
			req.Header.Set("Content-Type", c.ctype)
		}
		signV4(req, []byte(c.body), creds, "us-east-1", "service", now)
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=" + c.signed + ", Signature=" + c.sig
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
	}
}

func TestSigV4SessionTokenSigned(t *testing.T) {
	creds := awsCreds{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"}
	req, _ := http.NewRequest("POST", "https://ec2.us-east-1.amazonaws.com/", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	signV4(req, []byte("Action=DescribeInstances"), creds, "us-east-1", "ec2", time.Unix(0, 0))
	if req.Header.Get("X-Amz-Security-Token") != "tok" {
		t.Fatal("missing security token header")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token,") {
		t.Fatalf("token not signed: %s", req.Header.Get("Authorization"))
	}
}

func TestParseAWSXMLError(t *testing.T) {
	ec2 := `<?xml version="1.0" encoding="UTF-8"?>
<Response><Errors><Error><Code>InsufficientInstanceCapacity</Code><Message>We currently do not have sufficient c7a.4xlarge capacity.</Message></Error></Errors><RequestID>abc</RequestID></Response>`
	e := parseAWSXMLError(500, []byte(ec2))
	if e.Code != "InsufficientInstanceCapacity" || !strings.Contains(e.Message, "c7a.4xlarge") {
		t.Fatalf("ec2 error parsed as %+v", e)
	}
	sts := `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>ExpiredToken</Code><Message>The security token included in the request is expired</Message></Error><RequestId>x</RequestId></ErrorResponse>`
	e = parseAWSXMLError(403, []byte(sts))
	if e.Code != "ExpiredToken" {
		t.Fatalf("sts error parsed as %+v", e)
	}
	e = parseAWSXMLError(502, []byte("<html>bad gateway</html>"))
	if e.Code != "" || classifyAWSError(e) != errRetryable {
		t.Fatalf("unparseable 502 should be retryable: %+v", e)
	}
}

func TestClassifyAWSError(t *testing.T) {
	cases := map[string]errClass{
		"InsufficientInstanceCapacity": errNextType,
		"SpotMaxPriceTooLow":           errNextType,
		"Unsupported":                  errNextType,
		"MaxSpotInstanceCountExceeded": errQuota,
		"VcpuLimitExceeded":            errQuota,
		"InstanceLimitExceeded":        errQuota,
		"RequestLimitExceeded":         errRetryable,
		"Unavailable":                  errRetryable,
		"UnauthorizedOperation":        errFatal,
		"InvalidAMIID.NotFound":        errFatal,
	}
	for code, want := range cases {
		if got := classifyAWSError(&awsError{Status: 400, Code: code}); got != want {
			t.Errorf("%s: got %v want %v", code, got, want)
		}
	}
	arch := &awsError{Status: 400, Code: "InvalidParameterValue", Message: "The architecture 'arm64' of the specified instance type does not match the architecture 'x86_64' of the specified AMI."}
	if classifyAWSError(arch) != errNextType {
		t.Error("arch mismatch should move to the next type")
	}
	if classifyAWSError(errors.New("dial tcp: timeout")) != errRetryable {
		t.Error("network errors should be retryable")
	}
}

const describeXML = `<?xml version="1.0" encoding="UTF-8"?>
<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>r</requestId>
  <reservationSet>
    <item>
      <reservationId>r-1</reservationId>
      <instancesSet>
        <item>
          <instanceId>i-0abc</instanceId>
          <imageId>ami-1</imageId>
          <instanceState><code>16</code><name>running</name></instanceState>
          <instanceType>c7a.4xlarge</instanceType>
          <launchTime>2026-11-13T10:00:00.000Z</launchTime>
          <ipAddress>203.0.113.7</ipAddress>
          <instanceLifecycle>spot</instanceLifecycle>
          <tagSet>
            <item><key>spotexec-runner</key><value>burst</value></item>
            <item><key>spotexec-owner</key><value>u-1</value></item>
          </tagSet>
          <blockDeviceMapping><item><deviceName>/dev/sda1</deviceName></item></blockDeviceMapping>
        </item>
      </instancesSet>
    </item>
    <item>
      <instancesSet>
        <item>
          <instanceId>i-0def</instanceId>
          <instanceState><code>0</code><name>pending</name></instanceState>
          <instanceType>c6a.4xlarge</instanceType>
          <launchTime>2026-11-13T11:00:00.000Z</launchTime>
        </item>
      </instancesSet>
    </item>
  </reservationSet>
  <nextToken>tok2</nextToken>
</DescribeInstancesResponse>`

func TestParseDescribeInstances(t *testing.T) {
	insts, next, err := parseDescribeInstances([]byte(describeXML))
	if err != nil {
		t.Fatal(err)
	}
	if next != "tok2" || len(insts) != 2 {
		t.Fatalf("next=%q n=%d", next, len(insts))
	}
	i := insts[0]
	if i.InstanceID != "i-0abc" || i.State != "running" || i.IPAddress != "203.0.113.7" || i.Lifecycle != "spot" ||
		i.tag("spotexec-runner") != "burst" || i.tag("spotexec-owner") != "u-1" || i.launched().IsZero() {
		t.Fatalf("bad parse: %+v", i)
	}
	if insts[1].IPAddress != "" || mapEC2State(insts[1].State) != StatePending {
		t.Fatalf("bad second instance: %+v", insts[1])
	}
}

func TestXMLFirst(t *testing.T) {
	b := []byte(`<DescribeImagesResponse><imagesSet><item><imageId>ami-1</imageId><rootDeviceName>/dev/sda1</rootDeviceName><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName></item></blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`)
	if got := xmlFirst(b, "imagesSet>item>rootDeviceName"); got != "/dev/sda1" {
		t.Fatalf("got %q", got)
	}
	if got := xmlFirst([]byte(`<CreateSecurityGroupResponse><return>true</return><groupId>sg-9</groupId></CreateSecurityGroupResponse>`), "groupId"); got != "sg-9" {
		t.Fatalf("got %q", got)
	}
}

func TestParseCredentials(t *testing.T) {
	c, err := parseProcessCredentials([]byte(`{"Version":1,"AccessKeyId":"ASIA1","SecretAccessKey":"s","SessionToken":"t","Expiration":"2026-11-13T19:00:00+00:00"}`))
	if err != nil || c.AccessKeyID != "ASIA1" || c.SessionToken != "t" || c.Expiration.IsZero() {
		t.Fatalf("process creds: %+v %v", c, err)
	}
	ini := "[default]\naws_access_key_id = AKIA0\naws_secret_access_key = s0\n\n[work]\naws_access_key_id=AKIA1\naws_secret_access_key=s1\naws_session_token=t1\n"
	c, err = parseSharedCredentials(bufio.NewScanner(strings.NewReader(ini)), "work", "creds")
	if err != nil || c.AccessKeyID != "AKIA1" || c.SessionToken != "t1" {
		t.Fatalf("shared creds: %+v %v", c, err)
	}
	if _, err := parseSharedCredentials(bufio.NewScanner(strings.NewReader(ini)), "missing", "creds"); err == nil {
		t.Fatal("expected error for missing profile")
	}
}

func TestHasSSHIngressIgnoresEgress(t *testing.T) {
	body := []byte(`<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-1</groupId>
<ipPermissions><item><ipProtocol>tcp</ipProtocol><fromPort>22</fromPort><toPort>22</toPort>
<ipRanges><item><cidrIp>203.0.113.4/32</cidrIp></item></ipRanges></item></ipPermissions>
<ipPermissionsEgress><item><ipProtocol>-1</ipProtocol><ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges></item></ipPermissionsEgress>
</item></securityGroupInfo></DescribeSecurityGroupsResponse>`)
	if hasSSHIngress(body, "0.0.0.0/0") {
		t.Fatal("egress 0.0.0.0/0 must not count as SSH ingress")
	}
	if !hasSSHIngress(body, "203.0.113.4/32") {
		t.Fatal("expected ingress match")
	}
}

// AWS docs example: "Authenticating Requests: Using Query Parameters".
func TestPresignS3DocsVector(t *testing.T) {
	c := awsCreds{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	u := presignS3Host(c, "us-east-1", "examplebucket.s3.amazonaws.com", "test.txt", "GET", 86400*time.Second, now)
	want := "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.Contains(u, "X-Amz-Signature="+want) {
		t.Fatalf("signature mismatch:\n%s", u)
	}
}

func TestConfigCredentialProcess(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config"
	os.WriteFile(cfg, []byte(`[profile proc]
credential_process = printf '{"Version":1,"AccessKeyId":"AKIDPROC","SecretAccessKey":"sek","SessionToken":"tok","Expiration":"2099-01-01T00:00:00Z"}'
[profile ssoonly]
sso_session = corp
sso_account_id = 123456789012
sso_role_name = Dev
[sso-session corp]
sso_region = us-east-1
sso_start_url = https://example.awsapps.com/start
`), 0o644)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("HOME", dir)
	c, err := configCredentials(context.Background(), "proc")
	if err != nil || c.AccessKeyID != "AKIDPROC" || c.SessionToken != "tok" || c.Expiration.Year() != 2099 {
		t.Fatalf("credential_process: %+v %v", c, err)
	}
	if _, err := configCredentials(context.Background(), "ssoonly"); err == nil || !strings.Contains(err.Error(), "aws sso login") {
		t.Fatalf("expected an `aws sso login` hint without a cached token, got %v", err)
	}
}
