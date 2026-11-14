package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

func quotaRunner(types ...string) *AWSSpot {
	return &AWSSpot{r: &config.Runner{Name: "aws", Region: "us-east-2", MaxInstances: 4, InstanceTypes: types}}
}

// tries answers RunInstances per (type, market) from a table; missing
// entries launch.
func tries(codes map[string]string, calls *[]string) func(string, bool) (ec2Instance, error) {
	return func(itype string, spot bool) (ec2Instance, error) {
		key := itype
		if !spot {
			key += "/od"
		}
		*calls = append(*calls, key)
		if c, ok := codes[key]; ok {
			return ec2Instance{}, &awsError{Status: 400, Code: c, Message: "refused"}
		}
		return ec2Instance{InstanceID: "i-" + itype}, nil
	}
}

func TestLaunchAcrossQuotaRefusal(t *testing.T) {
	a := quotaRunner("c7a.2xlarge", "m7a.2xlarge", "c7a.xlarge")
	var calls []string
	_, err := a.launchAcross([]bool{true}, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge": "MaxSpotInstanceCountExceeded",
		"m7a.2xlarge": "InsufficientInstanceCapacity",
		"c7a.xlarge":  "MaxSpotInstanceCountExceeded",
	}, &calls))
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("want a QuotaError, got %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("a quota refusal should still try smaller types: %v", calls)
	}
	if !qe.Spot || qe.Region != "us-east-2" || qe.Code != QuotaCodeSpotStandard || qe.VCPUs != 4 || qe.Desired != 32 {
		t.Fatalf("quota error fields: %+v", qe)
	}
	msg := err.Error()
	for _, want := range []string{"us-east-2", "needs 4 vCPUs",
		"aws service-quotas request-service-quota-increase --region us-east-2 --service-code ec2 --quota-code L-34B43A08 --desired-value 32"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestLaunchAcrossSmallerTypeShortOfCapacityIsNotQuota(t *testing.T) {
	// The big type exceeds the quota, but the small one only lacks
	// capacity: it may launch later, so this is not a quota refusal.
	a := quotaRunner("c7a.4xlarge", "c7a.xlarge")
	var calls []string
	_, err := a.launchAcross([]bool{true}, func(string, ...any) {}, tries(map[string]string{
		"c7a.4xlarge": "MaxSpotInstanceCountExceeded",
		"c7a.xlarge":  "InsufficientInstanceCapacity",
	}, &calls))
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("got %v, want a capacity error", err)
	}
	// A type that can never launch here doesn't count against the quota
	// verdict.
	a = quotaRunner("c7a.2xlarge", "x9z.large")
	_, err = a.launchAcross([]bool{true}, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge": "MaxSpotInstanceCountExceeded",
		"x9z.large":   "Unsupported",
	}, &calls))
	if !errors.As(err, &qe) || qe.VCPUs != 8 {
		t.Fatalf("got %v, want a quota error for 8 vCPUs", err)
	}
}

func TestLaunchAcrossCapacityIsNotQuota(t *testing.T) {
	a := quotaRunner("c7a.2xlarge", "m7a.2xlarge")
	var calls []string
	_, err := a.launchAcross([]bool{true}, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge": "InsufficientInstanceCapacity",
		"m7a.2xlarge": "InsufficientInstanceCapacity",
	}, &calls))
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("capacity shortage reported as %v", err)
	}
}

func TestLaunchAcrossOnDemandFallbackAfterQuota(t *testing.T) {
	a := quotaRunner("c7a.2xlarge")
	a.r.OnDemandFallback = true
	var calls []string
	inst, err := a.launchAcross([]bool{true, false}, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge": "MaxSpotInstanceCountExceeded",
	}, &calls))
	if err != nil || inst.InstanceID == "" {
		t.Fatalf("explicit on-demand fallback should launch: %v", err)
	}
	// On-demand short of capacity: no longer purely a quota problem.
	calls = nil
	_, err = a.launchAcross([]bool{true, false}, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge":    "MaxSpotInstanceCountExceeded",
		"c7a.2xlarge/od": "InsufficientInstanceCapacity",
	}, &calls))
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("mixed refusal reported as %v", err)
	}
	// Both quotas full: the Spot one is reported.
	_, err = a.launchAcross([]bool{true, false}, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge":    "MaxSpotInstanceCountExceeded",
		"c7a.2xlarge/od": "VcpuLimitExceeded",
	}, &calls))
	if !errors.As(err, &qe) || !qe.Spot {
		t.Fatalf("want the Spot quota error, got %v", err)
	}
}

func TestQuotaCodeFor(t *testing.T) {
	cases := map[string]string{"c7a.xlarge": QuotaCodeSpotStandard, "m7g.large": QuotaCodeSpotStandard, "t3.micro": QuotaCodeSpotStandard,
		"g5.xlarge": "", "p4d.24xlarge": "", "inf2.xlarge": "", "dl1.24xlarge": "", "trn1.2xlarge": "", "hpc6a.48xlarge": ""}
	for it, want := range cases {
		if got := quotaCodeFor(it, true); got != want {
			t.Errorf("%s: %q, want %q", it, got, want)
		}
	}
	if quotaCodeFor("c7a.xlarge", false) != QuotaCodeOnDemandStandard {
		t.Error("on-demand standard code")
	}
	e := &QuotaError{Region: "eu-west-1", Spot: true, VCPUs: 8}
	if !strings.Contains(e.Error(), "Service Quotas console") {
		t.Errorf("no code: should point at the console: %s", e.Error())
	}
}

// fakeAWS answers Service Quotas and EC2 DescribeInstances.
type fakeAWS struct {
	quota     float64 // <0: AccessDenied
	instances string  // DescribeInstances item XML
}

func (f *fakeAWS) server(t *testing.T) *AWSSpot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("X-Amz-Target"), "ServiceQuotasV20190624.GetServiceQuota") {
			if f.quota < 0 {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"__type":"AccessDeniedException","Message":"no"}`)
				return
			}
			fmt.Fprintf(w, `{"Quota":{"QuotaCode":"L-34B43A08","Value":%g}}`, f.quota)
			return
		}
		_ = r.ParseForm()
		switch r.Form.Get("Action") {
		case "DescribeInstances":
			fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet>%s</instancesSet></item></reservationSet></DescribeInstancesResponse>`, f.instances)
		default:
			w.WriteHeader(400)
			fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidAction</Code><Message>x</Message></Error></Errors></Response>`)
		}
	}))
	t.Cleanup(srv.Close)
	a := quotaRunner("c7a.2xlarge")
	a.api = &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}}, http: srv.Client(),
		endpoints: map[string]string{"ec2": srv.URL, "servicequotas": srv.URL}}
	return a
}

func TestQuotaValue(t *testing.T) {
	a := (&fakeAWS{quota: 64}).server(t)
	if q, err := a.quotaValue(context.Background(), QuotaCodeSpotStandard); err != nil || q != 64 {
		t.Fatalf("quota %d, %v", q, err)
	}
	a = (&fakeAWS{quota: -1}).server(t)
	if _, err := a.quotaValue(context.Background(), QuotaCodeSpotStandard); err == nil {
		t.Fatal("access denied read as a quota")
	}
}

func TestLaunchAcrossCapacityUnderAnotherQuota(t *testing.T) {
	// G instances have a Spot quota of their own: one short of capacity
	// may launch later, whatever the standard quota says.
	a := quotaRunner("c7a.xlarge", "g5.xlarge")
	var calls []string
	_, err := a.launchAcross([]bool{true}, func(string, ...any) {}, tries(map[string]string{
		"c7a.xlarge": "MaxSpotInstanceCountExceeded",
		"g5.xlarge":  "InsufficientInstanceCapacity",
	}, &calls))
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("got %v, want a capacity error", err)
	}
	// Graviton mediums have one vCPU, smaller than a large.
	a = quotaRunner("c6g.large", "c6g.medium")
	_, err = a.launchAcross([]bool{true}, func(string, ...any) {}, tries(map[string]string{
		"c6g.large":  "MaxSpotInstanceCountExceeded",
		"c6g.medium": "InsufficientInstanceCapacity",
	}, &calls))
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("got %v, want a capacity error", err)
	}
}

func TestQuotaValueStalled(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	old := quotaLookupTimeout
	quotaLookupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { quotaLookupTimeout = old })
	a := quotaRunner("c7a.xlarge")
	a.api = &awsClient{region: "us-east-2", creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}}, http: srv.Client(),
		endpoints: map[string]string{"servicequotas": srv.URL}}
	start := time.Now()
	if _, err := a.quotaValue(context.Background(), QuotaCodeSpotStandard); err == nil {
		t.Fatal("stalled lookup returned a value")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("quota lookup not bounded")
	}
}
