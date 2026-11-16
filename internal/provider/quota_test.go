package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	_, err := a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
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
	_, err := a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
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
	_, err = a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
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
	_, err := a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
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
	inst, err := a.launchAcross([]bool{true, false}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge": "MaxSpotInstanceCountExceeded",
	}, &calls))
	if err != nil || inst.InstanceID == "" {
		t.Fatalf("explicit on-demand fallback should launch: %v", err)
	}
	// On-demand short of capacity: no longer purely a quota problem.
	calls = nil
	_, err = a.launchAcross([]bool{true, false}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
		"c7a.2xlarge":    "MaxSpotInstanceCountExceeded",
		"c7a.2xlarge/od": "InsufficientInstanceCapacity",
	}, &calls))
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("mixed refusal reported as %v", err)
	}
	// Both quotas full: the Spot one is reported.
	_, err = a.launchAcross([]bool{true, false}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
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
	more      string  // a second page of items, when set
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
			items, next := f.instances, ""
			if r.Form.Get("NextToken") == "p2" {
				items = f.more
			} else if f.more != "" {
				next = "<nextToken>p2</nextToken>"
			}
			fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet>%s</instancesSet></item></reservationSet>%s</DescribeInstancesResponse>`, items, next)
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
	_, err := a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
		"c7a.xlarge": "MaxSpotInstanceCountExceeded",
		"g5.xlarge":  "InsufficientInstanceCapacity",
	}, &calls))
	var qe *QuotaError
	if err == nil || errors.As(err, &qe) {
		t.Fatalf("got %v, want a capacity error", err)
	}
	// Graviton mediums have one vCPU, smaller than a large.
	a = quotaRunner("c6g.large", "c6g.medium")
	_, err = a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, tries(map[string]string{
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

func TestQuotaAdmit(t *testing.T) {
	cases := []struct {
		name     string
		u        QuotaUsage
		need     int
		ok, fair bool
	}{
		{"unknown quota: EC2 decides", QuotaUsage{Quota: -1, Used: 100}, 8, true, false},
		{"fits", QuotaUsage{Quota: 16, Used: 8, Mine: 8}, 8, true, false},
		{"full", QuotaUsage{Quota: 16, Used: 12, Mine: 12}, 8, false, false},
		{"new account below one VM", QuotaUsage{Quota: 5}, 8, false, false},
		{"alone: may use all of it", QuotaUsage{Quota: 32, Used: 24, Mine: 24}, 8, true, false},
		{"over its share, last room kept for others", QuotaUsage{Quota: 32, Used: 24, Mine: 16, Machines: map[string]int{"b": 8}}, 8, false, true},
		{"over its share with room to spare", QuotaUsage{Quota: 64, Used: 24, Mine: 16, Machines: map[string]int{"b": 8}}, 8, true, false},
		{"below its share takes the last room", QuotaUsage{Quota: 32, Used: 24, Mine: 8, Machines: map[string]int{"b": 16}}, 8, true, false},
		{"no VMs yet: below its share", QuotaUsage{Quota: 16, Used: 8, Machines: map[string]int{"b": 8}}, 8, true, false},
		{"no VMs and no room", QuotaUsage{Quota: 16, Used: 16, Machines: map[string]int{"b": 16}}, 8, false, false},
	}
	for _, c := range cases {
		if ok, fair := c.u.Admit(c.need); ok != c.ok || fair != c.fair {
			t.Errorf("%s: Admit = %v, %v; want %v, %v", c.name, ok, fair, c.ok, c.fair)
		}
	}
}

const quotaInstancesXML = `
<item><instanceId>i-mine</instanceId><instanceType>c7a.2xlarge</instanceType><instanceState><name>running</name></instanceState>
  <cpuOptions><coreCount>4</coreCount><threadsPerCore>2</threadsPerCore></cpuOptions>
  <tagSet><item><key>spotexec-runner</key><value>aws</value></item><item><key>spotexec-owner</key><value>me-1</value></item></tagSet></item>
<item><instanceId>i-studio</instanceId><instanceType>c7a.xlarge</instanceType><instanceState><name>pending</name></instanceState>
  <tagSet><item><key>spotexec-runner</key><value>aws</value></item><item><key>spotexec-owner</key><value>them-2</value></item><item><key>spotexec-host</key><value>studio-01</value></item></tagSet></item>
<item><instanceId>i-twin</instanceId><instanceType>c7a.xlarge</instanceType><instanceState><name>running</name></instanceState>
  <tagSet><item><key>spotexec-runner</key><value>aws</value></item><item><key>spotexec-owner</key><value>twin-3</value></item><item><key>spotexec-host</key><value>studio-01</value></item></tagSet></item>
<item><instanceId>i-other</instanceId><instanceType>m6i.large</instanceType><instanceState><name>running</name></instanceState>
  <cpuOptions><coreCount>1</coreCount><threadsPerCore>1</threadsPerCore></cpuOptions></item>
<item><instanceId>i-gpu</instanceId><instanceType>g5.xlarge</instanceType><instanceState><name>running</name></instanceState></item>`

func TestAWSQuotaUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
	if err := os.WriteFile(dir+"/owner-id", []byte("me-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := (&fakeAWS{quota: 20, instances: quotaInstancesXML}).server(t)
	u, err := a.QuotaUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Quota != 20 || u.Used != 18 || u.Mine != 8 || u.Other != 2 || len(u.Machines) != 2 ||
		u.Machines["studio-01"] != 4 || u.Machines["studio-01 (owner twin)"] != 4 {
		t.Fatalf("usage %+v", u)
	}
	want := "18 of 20 vCPUs in use: 8 by this machine, 4 by studio-01, 4 by studio-01 (owner twin), 2 by Spot instances spotexec didn't launch"
	if u.String() != want {
		t.Fatalf("got  %q\nwant %q", u.String(), want)
	}

	// The pre-launch check refuses what EC2 would, explaining the usage.
	qe := a.admitSpot(u, 8)
	if qe == nil || qe.Fair || qe.Usage == nil {
		t.Fatalf("launch past the quota admitted: %+v", qe)
	}
	msg := qe.Error()
	for _, w := range []string{"has no room", "18 of 20 vCPUs in use", "4 by studio-01", "--desired-value 42"} {
		if !strings.Contains(msg, w) {
			t.Errorf("message lacks %q:\n%s", w, msg)
		}
	}
	// Room for one more VM: below its share (30/3 machines) this machine
	// may take it; at its share (26/3), it's kept for the others.
	u.Quota = 30
	if qe := a.admitSpot(u, 8); qe != nil {
		t.Fatalf("refused below its share: %v", qe)
	}
	u.Quota = 26
	qe = a.admitSpot(u, 8)
	if qe == nil || !qe.Fair || !strings.Contains(qe.Error(), "kept for the other machines") {
		t.Fatalf("last room not kept for the others: %v", qe)
	}
	// An unreadable quota is reported as such.
	a = (&fakeAWS{quota: -1, instances: quotaInstancesXML}).server(t)
	if u, err := a.QuotaUsage(context.Background()); err == nil || u.Quota != -1 || u.Used != 18 {
		t.Fatalf("unreadable quota: %+v %v", u, err)
	}
}

// Each Spot candidate is admitted on its own: a big type can't take the
// others' room just because a small one would fit, a type under another
// quota is still tried, and on-demand (opted in) is unaffected.
func TestLaunchAdmitsEachCandidate(t *testing.T) {
	u := QuotaUsage{Region: "us-east-2", Quota: 32, Used: 24, Mine: 16, Machines: map[string]int{"studio-01": 8}}
	a := quotaRunner("c7a.2xlarge", "c7a.large")
	var calls []string
	inst, err := a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, a.admitting(&u, tries(nil, &calls)))
	if err != nil || inst.InstanceID != "i-c7a.large" || strings.Join(calls, ",") != "c7a.large" {
		t.Fatalf("launched %q (calls %v, %v); want only the 2-vCPU type", inst.InstanceID, calls, err)
	}
	u = QuotaUsage{Region: "us-east-2", Quota: 16, Used: 16, Mine: 16}
	a = quotaRunner("c7a.xlarge", "g5.xlarge")
	calls = nil
	inst, err = a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, a.admitting(&u, tries(nil, &calls)))
	if err != nil || inst.InstanceID != "i-g5.xlarge" {
		t.Fatalf("G type under its own quota not tried: %v %v", calls, err)
	}
	a = quotaRunner("c7a.xlarge")
	a.r.OnDemandFallback = true
	calls = nil
	inst, err = a.launchAcross([]bool{true, false}, a.r.InstanceTypes, func(string, ...any) {}, a.admitting(&u, tries(nil, &calls)))
	if err != nil || strings.Join(calls, ",") != "c7a.xlarge/od" {
		t.Fatalf("on-demand fallback: %v %v", calls, err)
	}
	a.r.OnDemandFallback = false
	_, err = a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, a.admitting(&u, tries(nil, &calls)))
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Usage == nil || !strings.Contains(err.Error(), "16 of 16 vCPUs in use: 16 by this machine") {
		t.Fatalf("want the checked quota error with its usage, got %v", err)
	}
}

func TestQuotaUsageReadsEveryPage(t *testing.T) {
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	a := (&fakeAWS{quota: 64, instances: quotaInstancesXML, more: `<item><instanceId>i-late</instanceId><instanceType>c7a.4xlarge</instanceType></item>`}).server(t)
	u, err := a.QuotaUsage(context.Background())
	if err != nil || u.Used != 18+16 {
		t.Fatalf("usage %+v, %v", u, err)
	}
}

// The suggested quota fits this machine's fleet next to what the other
// machines use, however launchAcross reports the refusal.
func TestCheckedQuotaKeepsItsDesiredValue(t *testing.T) {
	u := QuotaUsage{Region: "us-east-2", Quota: 64, Used: 64, Mine: 8, Machines: map[string]int{"studio-01": 56}}
	a := quotaRunner("c7a.2xlarge")
	var calls []string
	_, err := a.launchAcross([]bool{true}, a.r.InstanceTypes, func(string, ...any) {}, a.admitting(&u, tries(nil, &calls)))
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Desired != 88 || !strings.Contains(err.Error(), "--desired-value 88") {
		t.Fatalf("want desired 88 (56 others + 32 fleet), got %v", err)
	}
}

func TestMinStandardVCPUs(t *testing.T) {
	if n := minStandardVCPUs([]string{"c7a.2xlarge", "m7a.xlarge", "g5.large"}); n != 4 {
		t.Fatalf("got %d", n)
	}
	if n := minStandardVCPUs([]string{"g5.xlarge"}); n != 0 {
		t.Fatalf("got %d", n)
	}
}

func TestVCPUsOf(t *testing.T) {
	for it, want := range map[string]int{"t2.micro": 1, "t3.micro": 2, "t3.medium": 2, "c6g.medium": 1, "c7a.large": 2,
		"c7a.xlarge": 4, "c7a.2xlarge": 8, "m7a.48xlarge": 192, "c4.8xlarge": 36, "c5.metal": 96, "m7i.metal-24xl": 96, "c7a.metal-48xl": 192} {
		if got := vcpusOf(it); got != want {
			t.Errorf("%s: %d, want %d", it, got, want)
		}
	}
}

func TestDoctorQuotaFinding(t *testing.T) {
	// Enough for two of the fleet's four VMs: passes, with the command.
	c := quotaFinding(QuotaFit{Region: "us-east-2", Quota: 16, OneVM: 8, Fleet: 32})
	if !c.OK || !strings.Contains(c.Msg, "fewer VMs run at once") || !strings.Contains(c.Msg, "--desired-value 32") {
		t.Fatalf("limited: %+v", c)
	}
	c = quotaFinding(QuotaFit{Region: "us-east-2", Quota: 5, OneVM: 8, Fleet: 32})
	if c.OK || !strings.Contains(c.Msg, "not enough for one VM (8 vCPUs)") || !strings.Contains(c.Msg, "request-service-quota-increase --region us-east-2") {
		t.Fatalf("blocked: %+v", c)
	}
	c = quotaFinding(QuotaFit{Region: "us-east-2", Quota: 5, OneVM: 8, Fleet: 32, Others: true})
	if !c.OK || !strings.Contains(c.Msg, "only this runner's other instance types can launch") {
		t.Fatalf("mixed families: %+v", c)
	}
	c = quotaFinding(QuotaFit{Region: "us-east-2", Quota: 64, OneVM: 8, Fleet: 32})
	if !c.OK || strings.Contains(c.Msg, "request-service-quota-increase") {
		t.Fatalf("fine: %+v", c)
	}
	// No max_instances: the quota is the limit, which is not a problem.
	c = quotaFinding(QuotaFit{Region: "us-east-2", Quota: 16, OneVM: 8, Uncapped: true})
	if !c.OK || !strings.Contains(c.Msg, "with no max_instances, this quota is what limits") || strings.Contains(c.Msg, "request-service-quota-increase") {
		t.Fatalf("uncapped: %+v", c)
	}
	// Through the provider: a 4-vCPU fleet on a new account's 5 vCPUs.
	a := (&fakeAWS{quota: 5}).server(t)
	a.r.InstanceTypes = []string{"c7a.xlarge"}
	if c, ok := a.quotaCheck(context.Background()); !ok || !c.OK {
		t.Fatalf("one 4-vCPU VM fits 5 vCPUs: %+v", c)
	}
	a.r.MaxInstances = 0
	if c, ok := a.quotaCheck(context.Background()); !ok || !c.OK || !strings.Contains(c.Msg, "with no max_instances") {
		t.Fatalf("uncapped runner: %+v", c)
	}
	a.r.InstanceTypes = []string{"g5.xlarge"}
	if _, ok := a.quotaCheck(context.Background()); ok {
		t.Fatal("G instances don't use the standard quota")
	}
}

func TestRequestQuotaIncrease(t *testing.T) {
	var got map[string]any
	answer := `{"RequestedQuota":{"Id":"r-1","Status":"PENDING"}}`
	code := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "ServiceQuotasV20190624.RequestServiceQuotaIncrease" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(code)
		fmt.Fprint(w, answer)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/credentials")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/config")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKADMIN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	s := &AWSUserSetup{endpoints: map[string]string{"servicequotas": srv.URL}}
	st, err := s.RequestQuotaIncrease(context.Background(), "us-east-2", 16)
	if err != nil || st != "PENDING" {
		t.Fatalf("%q %v", st, err)
	}
	if got["QuotaCode"] != QuotaCodeSpotStandard || got["ServiceCode"] != "ec2" || got["DesiredValue"] != float64(16) {
		t.Fatalf("request %v", got)
	}
	// The admin session setup signed in with is reused, even if the
	// profile it came from now holds the spotexec key.
	var auth string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"RequestedQuota":{"Status":"PENDING"}}`)
	}))
	t.Cleanup(srv2.Close)
	s2 := &AWSUserSetup{endpoints: map[string]string{"servicequotas": srv2.URL},
		adminCreds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKSESSION", SecretAccessKey: "s"}}}
	if _, err := s2.RequestQuotaIncrease(context.Background(), "us-east-2", 16); err != nil || !strings.Contains(auth, "Credential=AKSESSION/") {
		t.Fatalf("signed with %q (%v), want the admin session", auth, err)
	}
	code, answer = 400, `{"__type":"ResourceAlreadyExistsException","message":"already"}`
	if st, err := s.RequestQuotaIncrease(context.Background(), "us-east-2", 16); err != nil || st != "already requested" {
		t.Fatalf("pending request: %q %v", st, err)
	}
	code, answer = 400, `{"__type":"AccessDeniedException","Message":"no"}`
	if _, err := s.RequestQuotaIncrease(context.Background(), "us-east-2", 16); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("denied: %v", err)
	}
}
