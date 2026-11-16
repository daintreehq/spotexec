package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ec2APIVersion = "2016-11-15"
	stsAPIVersion = "2011-06-15"
)

// awsError is an AWS API error.
type awsError struct {
	Status  int
	Code    string
	Message string
}

func (e *awsError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("AWS HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func awsCode(err error) string {
	var ae *awsError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// Error classification for RunInstances.
type errClass int

const (
	errFatal     errClass = iota
	errNextType           // try the next instance type (capacity, price, unsupported)
	errRetryable          // transient: retry the same request with backoff
	errQuota              // the account's vCPU quota: a smaller type may fit, but waiting for capacity won't help
)

func classifyAWSError(err error) errClass {
	var ae *awsError
	if !errors.As(err, &ae) {
		return errRetryable // network errors
	}
	switch ae.Code {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "SpotMaxPriceTooLow",
		"Unsupported", "UnsupportedOperation", "InsufficientHostCapacity":
		return errNextType
	case "MaxSpotInstanceCountExceeded", "VcpuLimitExceeded", "InstanceLimitExceeded":
		// The account's (or region's) limit, not the capacity pool's: it
		// lifts only when instances in the account end or the quota is raised.
		return errQuota
	case "RequestLimitExceeded", "Throttling", "ThrottlingException", "InternalError",
		"InternalFailure", "Unavailable", "ServiceUnavailable", "InsufficientAddressCapacity":
		return errRetryable
	}
	if ae.Code == "InvalidParameterValue" && strings.Contains(strings.ToLower(ae.Message), "architecture") {
		return errNextType // instance type arch doesn't match the AMI
	}
	if ae.Status >= 500 {
		return errRetryable
	}
	return errFatal
}

// parseAWSXMLError handles both EC2 (<Response><Errors><Error>) and STS/IAM
// (<ErrorResponse><Error>) shapes.
func parseAWSXMLError(status int, body []byte) *awsError {
	var ec2 struct {
		Errors []struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Errors>Error"`
	}
	if xml.Unmarshal(body, &ec2) == nil && len(ec2.Errors) > 0 {
		return &awsError{Status: status, Code: ec2.Errors[0].Code, Message: ec2.Errors[0].Message}
	}
	var sts struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	if xml.Unmarshal(body, &sts) == nil && sts.Error.Code != "" {
		return &awsError{Status: status, Code: sts.Error.Code, Message: sts.Error.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return &awsError{Status: status, Message: msg}
}

// queryAttemptTimeout bounds one attempt of a read-only Query call.
var queryAttemptTimeout = 20 * time.Second

func readOnlyAction(action string) bool {
	return strings.HasPrefix(action, "Describe") || strings.HasPrefix(action, "Get")
}

// awsClient performs signed requests.
type awsClient struct {
	region string
	creds  *awsCredResolver
	http   *http.Client
	// endpoint overrides (tests): service -> base URL
	endpoints map[string]string
	now       func() time.Time
}

func (c *awsClient) endpoint(service string) string {
	if e, ok := c.endpoints[service]; ok {
		return e
	}
	return "https://" + service + "." + c.region + ".amazonaws.com/"
}

func (c *awsClient) do(ctx context.Context, service, contentType string, extra map[string]string, body []byte) (int, []byte, error) {
	creds, err := c.creds.get(ctx)
	if err != nil {
		return 0, nil, err
	}
	if _, overridden := c.endpoints[service]; service == "ec2" && !overridden {
		ctx = traceConnect(ctx, c.region) // how far the region is (see RunCost)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(service), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	signV4(req, body, creds, c.region, service, now())
	req.Header.Set("User-Agent", "spotexec")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp.StatusCode, b, err
}

// query calls an AWS Query-protocol API (EC2, STS), retrying transient
// failures. It returns the raw XML body.
func (c *awsClient) query(ctx context.Context, service, version, action string, params url.Values) ([]byte, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("Action", action)
	params.Set("Version", version)
	body := []byte(params.Encode())
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 300 * time.Millisecond):
			}
		}
		// Each attempt of a read gets its own deadline, so one stuck
		// connection (a far region's endpoint that stops answering) is
		// retried within the caller's deadline instead of using it all up.
		// Writes keep the caller's: retrying one that went through (an
		// image already created) would fail as a duplicate. Credentials
		// resolve first under the caller's deadline, since a credential
		// helper can be slow; do then finds them cached. If they don't
		// resolve, do reports it as before.
		actx, cancel := ctx, context.CancelFunc(func() {})
		if readOnlyAction(action) {
			if _, err := c.creds.get(ctx); err == nil {
				actx, cancel = context.WithTimeout(ctx, queryAttemptTimeout)
			}
		}
		status, b, err := c.do(actx, service, "application/x-www-form-urlencoded; charset=utf-8", nil, body)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("%s %s: %w", service, action, err)
			var ce *CredentialError
			if errors.As(err, &ce) {
				return nil, err // retrying can't fix credentials
			}
			continue
		}
		if status/100 == 2 {
			return b, nil
		}
		ae := parseAWSXMLError(status, b)
		lastErr = ae
		if isExpiredCreds(ae.Code) && attempt == 0 {
			c.creds.invalidate()
			continue
		}
		if isExpiredCreds(ae.Code) || ae.Code == "AuthFailure" {
			// Still rejected with fresh credentials: no retry or cached
			// view will help, so callers stop and say so.
			return nil, &CredentialError{ae}
		}
		if classifyAWSError(ae) != errRetryable {
			return nil, ae
		}
	}
	return nil, lastErr
}

func (c *awsClient) ec2(ctx context.Context, action string, params url.Values) ([]byte, error) {
	return c.query(ctx, "ec2", ec2APIVersion, action, params)
}

func isExpiredCreds(code string) bool {
	switch code {
	case "ExpiredToken", "ExpiredTokenException", "RequestExpired", "InvalidClientTokenId", "UnrecognizedClientException", "AuthFailure":
		return true
	}
	return false
}

// ssmGetParameter reads an SSM parameter value, retrying transient failures.
func (c *awsClient) ssmGetParameter(ctx context.Context, name string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 300 * time.Millisecond):
			}
		}
		v, err := c.ssmGetParameterOnce(ctx, name)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var ce *CredentialError
		if errors.As(err, &ce) {
			return "", err // retrying can't fix credentials
		}
		if code := awsCode(err); isExpiredCreds(code) {
			if attempt == 0 {
				c.creds.invalidate()
				continue
			}
			// Still rejected with fresh credentials, as in query: say so
			// rather than let the caller blame the region.
			return "", &CredentialError{err}
		}
		if awsCode(err) != "" && classifyAWSError(err) != errRetryable {
			return "", err
		}
	}
	return "", lastErr
}

func (c *awsClient) ssmGetParameterOnce(ctx context.Context, name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"Name": name})
	status, b, err := c.do(ctx, "ssm", "application/x-amz-json-1.1", map[string]string{"X-Amz-Target": "AmazonSSM.GetParameter"}, body)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg2    string `json:"Message"`
		}
		_ = json.Unmarshal(b, &e)
		code := e.Type
		if i := strings.LastIndex(code, "#"); i >= 0 {
			code = code[i+1:]
		}
		return "", &awsError{Status: status, Code: code, Message: e.Message + e.Msg2}
	}
	var v struct {
		Parameter struct{ Value string }
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	if v.Parameter.Value == "" {
		return "", fmt.Errorf("SSM parameter %s is empty", name)
	}
	return v.Parameter.Value, nil
}

// ---- EC2 XML shapes ----

type ec2Tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type ec2Instance struct {
	InstanceID   string   `xml:"instanceId"`
	ImageID      string   `xml:"imageId"`
	InstanceType string   `xml:"instanceType"`
	LaunchTime   string   `xml:"launchTime"`
	IPAddress    string   `xml:"ipAddress"`
	State        string   `xml:"instanceState>name"`
	Lifecycle    string   `xml:"instanceLifecycle"`
	Zone         string   `xml:"placement>availabilityZone"`
	Tags         []ec2Tag `xml:"tagSet>item"`
}

func (i ec2Instance) tag(k string) string {
	for _, t := range i.Tags {
		if t.Key == k {
			return t.Value
		}
	}
	return ""
}

func (i ec2Instance) launched() time.Time {
	t, _ := time.Parse(time.RFC3339, i.LaunchTime)
	return t
}

type describeInstancesResp struct {
	Reservations []struct {
		Instances []ec2Instance `xml:"instancesSet>item"`
	} `xml:"reservationSet>item"`
	NextToken string `xml:"nextToken"`
}

func parseDescribeInstances(b []byte) ([]ec2Instance, string, error) {
	var r describeInstancesResp
	if err := xml.Unmarshal(b, &r); err != nil {
		return nil, "", fmt.Errorf("parse DescribeInstances: %w", err)
	}
	var out []ec2Instance
	for _, res := range r.Reservations {
		out = append(out, res.Instances...)
	}
	return out, r.NextToken, nil
}

func parseRunInstances(b []byte) (ec2Instance, error) {
	var r struct {
		Instances []ec2Instance `xml:"instancesSet>item"`
	}
	if err := xml.Unmarshal(b, &r); err != nil {
		return ec2Instance{}, fmt.Errorf("parse RunInstances: %w", err)
	}
	if len(r.Instances) == 0 {
		return ec2Instance{}, errors.New("RunInstances returned no instances")
	}
	return r.Instances[0], nil
}

// xmlField extracts the first occurrence of a simple element by path.
func xmlFirst(b []byte, path string) string {
	// Minimal: decode into a generic struct for the known paths we use.
	dec := xml.NewDecoder(bytes.NewReader(b))
	want := strings.Split(path, ">")
	var stack []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if hasSuffixPath(stack, want) {
				var s string
				if err := dec.DecodeElement(&s, &t); err == nil {
					return strings.TrimSpace(s)
				}
				return ""
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
}

func hasSuffixPath(stack, want []string) bool {
	if len(stack) < len(want) {
		return false
	}
	off := len(stack) - len(want)
	for i, w := range want {
		if stack[off+i] != w {
			return false
		}
	}
	return true
}

// hasSSHIngress reports whether a DescribeSecurityGroups response grants
// inbound tcp/22 from cidr within a single permission (egress rules are a
// separate element and never count).
func hasSSHIngress(b []byte, cidr string) bool {
	var v struct {
		Groups []struct {
			Perms []struct {
				Proto string `xml:"ipProtocol"`
				From  int    `xml:"fromPort"`
				To    int    `xml:"toPort"`
				V4    []struct {
					CIDR string `xml:"cidrIp"`
				} `xml:"ipRanges>item"`
				V6 []struct {
					CIDR string `xml:"cidrIpv6"`
				} `xml:"ipv6Ranges>item"`
			} `xml:"ipPermissions>item"`
		} `xml:"securityGroupInfo>item"`
	}
	if xml.Unmarshal(b, &v) != nil {
		return false
	}
	for _, g := range v.Groups {
		for _, p := range g.Perms {
			if !(p.Proto == "tcp" || p.Proto == "-1") || (p.Proto == "tcp" && (p.From > 22 || p.To < 22)) {
				continue
			}
			for _, r := range p.V4 {
				if r.CIDR == cidr {
					return true
				}
			}
			for _, r := range p.V6 {
				if r.CIDR == cidr {
					return true
				}
			}
		}
	}
	return false
}
