package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

func TestPrintSpotQuotas(t *testing.T) {
	var b bytes.Buffer
	printSpotQuotas(&b, []regionQuota{
		{Region: "us-east-2", Usage: provider.QuotaUsage{Quota: 16, Used: 12, Mine: 4, Machines: map[string]int{"studio-01": 8}}},
		{Region: "eu-west-1", Usage: provider.QuotaUsage{Quota: -1, Used: 4, Mine: 4}, Err: errors.New("AccessDeniedException")},
		{Region: "ap-south-1", Err: errors.New("network down")},
	})
	out := b.String()
	for _, w := range []string{
		"us-east-2      12 of 16 vCPUs in use: 4 by this machine, 8 by studio-01",
		"eu-west-1      4 vCPUs in use (quota unknown): 4 by this machine (quota unreadable: AccessDeniedException)",
		"ap-south-1     couldn't read it: network down",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	b.Reset()
	printSpotQuotas(&b, []regionQuota{
		{Region: "us-east-2", Profile: "work", Usage: provider.QuotaUsage{Quota: 16, Used: 8, Mine: 8}},
		{Region: "us-east-2", Profile: "personal", Usage: provider.QuotaUsage{Quota: 5}},
	})
	if out := b.String(); !strings.Contains(out, "us-east-2 (profile work) 8 of 16") || !strings.Contains(out, "us-east-2 (profile personal) 0 of 5") {
		t.Errorf("two accounts in one region not told apart:\n%s", out)
	}
	b.Reset()
	printSpotQuotas(&b, nil)
	if b.Len() != 0 {
		t.Errorf("no AWS runners should print nothing: %q", b.String())
	}
}

func TestAccountStatusJSON(t *testing.T) {
	b, err := json.Marshal(newAccountStatus(
		&provider.AccountSearch{Instances: []provider.AccountInstance{{ID: "i-1", Region: "us-east-2"}}},
		[]regionQuota{{Region: "us-east-2", Usage: provider.QuotaUsage{Region: "us-east-2", Quota: 16, Used: 8, Mine: 8}},
			{Region: "eu-west-1", Usage: provider.QuotaUsage{Quota: -1}, Error: "AccessDenied", Err: errors.New("AccessDenied")}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), `{"format_version":1,`) {
		t.Errorf("JSON doesn't open with its format version:\n%s", b)
	}
	for _, w := range []string{`"instances":[{"id":"i-1"`, `"spot_quotas":[{"region":"us-east-2","usage":{"region":"us-east-2","quota":16,"used":8,"mine":8,"other":0}}`, `"error":"AccessDenied"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("JSON lacks %s:\n%s", w, b)
		}
	}
}

func TestStarterTemplateSizing(t *testing.T) {
	for _, r := range starterSmall {
		if strings.Count(globalTemplate, r[0]) != 1 {
			t.Fatalf("the template no longer has the starter line %q", r[0])
		}
	}
	dir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(globalTemplate, "{{REGION}}", "us-east-2")), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	big := g.Runners["aws"]
	if v := provider.VCPUsOf(big.InstanceTypes[0]); v != starterVCPUs {
		t.Fatalf("starter VMs have %d vCPUs, want %d", v, starterVCPUs)
	}
	if ok, err := sizeStarterFleet(p); err != nil || !ok {
		t.Fatalf("sizing the fresh template: %v %v", ok, err)
	}
	g, err = config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	small := g.Runners["aws"]
	if provider.VCPUsOf(small.InstanceTypes[0]) != 4 || small.MaxSlots != 2 || small.MaxInstances != big.MaxInstances {
		t.Fatalf("sized runner: %v slots %d max %d", small.InstanceTypes, small.MaxSlots, small.MaxInstances)
	}
	if fits := quotaFits([]*config.Runner{small}, nil, map[string]float64{"us-east-2": 5}); len(fits) != 1 || fits[0].Blocked() || fits[0].Limited() || !fits[0].Uncapped {
		t.Fatalf("a 5-vCPU quota should fit one 4-vCPU VM, and limit the uncapped starter fleet: %+v", fits)
	}
	// Edited since: left alone.
	if ok, _ := sizeStarterFleet(p); ok {
		t.Fatal("resized a config that isn't the starter any more")
	}
}

func TestConfigDefaultMatchesStarter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("version = 1\n[runners.x]\ntype = \"aws-spot\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := config.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	want := `instance_types = ["` + strings.Join(g.Runners["x"].InstanceTypes, `", "`) + `"]`
	if !strings.Contains(globalTemplate, want) {
		t.Fatalf("default %s isn't the starter template's", want)
	}
}

func TestQuotaFits(t *testing.T) {
	rs := []*config.Runner{
		{Name: "a", Region: "us-east-2", MaxInstances: 4, InstanceTypes: []string{"c7a.2xlarge", "c7a.xlarge"}},
		{Name: "b", Region: "us-east-2", MaxInstances: 2, InstanceTypes: []string{"m7a.large"}},
		{Name: "g", Region: "us-east-2", MaxInstances: 2, InstanceTypes: []string{"g5.xlarge"}}, // its own quota
		{Name: "c", Region: "eu-west-1", MaxInstances: 1, InstanceTypes: []string{"c7a.xlarge"}},
		{Name: "d", Region: "ap-south-1", MaxInstances: 1, InstanceTypes: []string{"c7a.xlarge"}}, // quota unknown
	}
	fits := quotaFits(rs, nil, map[string]float64{"us-east-2": 16, "eu-west-1": 64})
	if len(fits) != 2 || fits[0] != (provider.QuotaFit{Region: "us-east-2", Quota: 16, OneVM: 2, Fleet: 36, Others: true}) ||
		fits[1] != (provider.QuotaFit{Region: "eu-west-1", Quota: 64, OneVM: 4, Fleet: 4}) {
		t.Fatalf("fits %+v", fits)
	}
}

func TestReportQuotas(t *testing.T) {
	blocked := provider.QuotaFit{Region: "us-east-2", Quota: 0, OneVM: 4, Fleet: 16}
	limited := provider.QuotaFit{Region: "eu-west-1", Quota: 5, OneVM: 4, Fleet: 16}
	fine := provider.QuotaFit{Region: "ap-south-1", Quota: 64, OneVM: 4, Fleet: 16}
	var b bytes.Buffer
	requested := 0
	req := func(f provider.QuotaFit) (string, error) { requested++; return "PENDING", nil }
	reportQuotas(&b, []provider.QuotaFit{blocked, limited, fine}, nil, req)
	out := b.String()
	for _, w := range []string{
		"✗ Spot vCPU quota in us-east-2 is 0: not enough for one VM (4 vCPUs)",
		"Request more (as an AWS administrator): aws service-quotas request-service-quota-increase --region us-east-2 --service-code ec2 --quota-code L-34B43A08 --desired-value 16",
		"✓ Spot vCPU quota in eu-west-1: 5. Note: your runners there may use up to 16, so fewer VMs run at once",
		"--region eu-west-1 --service-code ec2 --quota-code L-34B43A08 --desired-value 16",
		"✓ Spot vCPU quota in ap-south-1: 64 (runners need up to 16)",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if requested != 0 {
		t.Fatal("requested without asking")
	}
	// Without max_instances the quota is the limit: a note, no command,
	// unless a capped fleet in the same region wants more.
	b.Reset()
	uncapped := provider.QuotaFit{Region: "us-east-2", Quota: 16, OneVM: 4, Uncapped: true}
	mixed := provider.QuotaFit{Region: "eu-west-1", Quota: 16, OneVM: 4, Fleet: 32, Uncapped: true}
	reportQuotas(&b, []provider.QuotaFit{uncapped, mixed}, nil, req)
	out = b.String()
	for _, w := range []string{
		"✓ Spot vCPU quota in us-east-2: 16 (with no max_instances, this is what limits how many VMs run at once)",
		"✓ Spot vCPU quota in eu-west-1: 16. Note: your runners there may use up to 32",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "--region us-east-2") {
		t.Errorf("an uncapped fleet needs no increase command:\n%s", out)
	}
	// Offered for the blocker only, and done when accepted.
	asked := 0
	b.Reset()
	reportQuotas(&b, []provider.QuotaFit{blocked, limited}, func(provider.QuotaFit) bool { asked++; return true }, req)
	if asked != 1 || requested != 1 || !strings.Contains(b.String(), "✓ requested 16 vCPUs in us-east-2 (pending)") ||
		!strings.Contains(b.String(), "--region us-east-2 --service-code ec2 --quota-code L-34B43A08 --desired-value 16") {
		t.Fatalf("asked %d, requested %d:\n%s", asked, requested, b.String())
	}
	b.Reset()
	reportQuotas(&b, []provider.QuotaFit{blocked}, func(provider.QuotaFit) bool { return true },
		func(provider.QuotaFit) (string, error) { return "", errors.New("AccessDenied") })
	if !strings.Contains(b.String(), "The request failed (AccessDenied). Ask an administrator to run the command above.") {
		t.Fatalf("failure not explained:\n%s", b.String())
	}
	// Runners that can also launch other families aren't blocked outright.
	b.Reset()
	partial := blocked
	partial.Others = true
	reportQuotas(&b, []provider.QuotaFit{partial}, func(provider.QuotaFit) bool { t.Fatal("offered for a partial block"); return false }, req)
	if !strings.Contains(b.String(), "! Spot vCPU quota in us-east-2 is 0: not enough for one standard-family VM") || strings.Contains(b.String(), "✗") {
		t.Fatalf("partial block:\n%s", b.String())
	}
}

func TestSizeStarterFleetKeepsConfigOnFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	orig := strings.ReplaceAll(globalTemplate, "{{REGION}}", "us-east-2")
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if ok, err := sizeStarterFleet(p); err == nil || ok {
		t.Fatalf("resize in a read-only directory: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(p); string(b) != orig {
		t.Fatal("a failed resize changed the config")
	}
}

func TestQuotaFitsRequirements(t *testing.T) {
	rs := []*config.Runner{{Name: "r", Type: config.TypeAWSSpot, Region: "us-east-2", MaxInstances: 3, VCPUs: 8}}
	fits := quotaFits(rs, nil, map[string]float64{"us-east-2": 16})
	if len(fits) != 1 || fits[0] != (provider.QuotaFit{Region: "us-east-2", Quota: 16, OneVM: 8, Fleet: 24}) {
		t.Fatalf("fits %+v", fits)
	}
}

// A multi-region runner's quota is fitted in each region it launches in,
// not its home region: the whole fleet may land in any one of them.
func TestQuotaFitsMultiRegion(t *testing.T) {
	rs := []*config.Runner{
		{Name: "all", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{config.AllRegions}, MaxInstances: 10, InstanceTypes: []string{"c7a.2xlarge"}},
		{Name: "two", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{"eu-west-1", "us-west-2"}, MaxInstances: 1, InstanceTypes: []string{"c7a.xlarge"}},
	}
	launches := map[string][]string{"all": {"ap-south-1", "us-west-2"}, "two": {"eu-west-1", "us-west-2"}}
	fits := quotaFits(rs, launches, map[string]float64{"us-east-1": 640, "ap-south-1": 5, "us-west-2": 64, "eu-west-1": 32})
	want := []provider.QuotaFit{
		{Region: "ap-south-1", Quota: 5, OneVM: 8, Fleet: 80},
		{Region: "us-west-2", Quota: 64, OneVM: 4, Fleet: 84},
		{Region: "eu-west-1", Quota: 32, OneVM: 4, Fleet: 4},
	}
	if len(fits) != len(want) {
		t.Fatalf("fits %+v", fits)
	}
	for i := range want {
		if fits[i] != want[i] {
			t.Fatalf("fits %+v, want %+v", fits, want)
		}
	}
	// A runner the map leaves out is fitted in its own region; a listed
	// runner only where its launches go (an enabled listed region).
	single := &config.Runner{Name: "one", Type: config.TypeAWSSpot, Region: "us-east-1", MaxInstances: 2, InstanceTypes: []string{"c7a.xlarge"}}
	fits = quotaFits([]*config.Runner{rs[1], single}, map[string][]string{"two": {"us-west-2"}}, map[string]float64{"us-east-1": 640, "us-west-2": 64, "eu-west-1": 32})
	if len(fits) != 2 || fits[0] != (provider.QuotaFit{Region: "us-west-2", Quota: 64, OneVM: 4, Fleet: 4}) ||
		fits[1] != (provider.QuotaFit{Region: "us-east-1", Quota: 640, OneVM: 4, Fleet: 8}) {
		t.Fatalf("fits %+v", fits)
	}
	// Other instance families count where the runner launches, not at home.
	gpu := &config.Runner{Name: "gpu", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{config.AllRegions}, MaxInstances: 1, InstanceTypes: []string{"c7a.xlarge", "g5.xlarge"}}
	fits = quotaFits([]*config.Runner{gpu}, map[string][]string{"gpu": {"eu-west-1"}}, map[string]float64{"eu-west-1": 32})
	if len(fits) != 1 || fits[0].Region != "eu-west-1" || !fits[0].Others {
		t.Fatalf("fits %+v", fits)
	}
	// An any-region runner whose launch regions couldn't be ranked is
	// checked nowhere, not in its home region.
	if fits := quotaFits(rs[:1], map[string][]string{"all": nil}, map[string]float64{"us-east-1": 640}); len(fits) != 0 {
		t.Fatalf("fits %+v", fits)
	}
}

func TestDescribeAWSRunners(t *testing.T) {
	rs := []*config.Runner{
		{Name: "aws", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{config.AllRegions}, ExcludeRegions: []string{"me-south-1", "af-south-1"}},
		{Name: "every", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{config.AllRegions}},
		{Name: "two", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{"eu-west-1", "us-west-2"}},
		{Name: "one", Type: config.TypeAWSSpot, Region: "us-east-2"},
	}
	want := "aws (all enabled regions except me-south-1, af-south-1), every (all enabled regions), two (eu-west-1, us-west-2), one (us-east-2)"
	if got := describeAWSRunners(rs); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
