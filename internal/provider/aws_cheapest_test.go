package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// newSingleEnv is a plain single-region runner (region r-one-1) served by
// the fake EC2.
func newSingleEnv(t *testing.T, f *fakeRegionsEC2, edit func(r *config.Runner)) *AWSSpot {
	t.Helper()
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	if f.instances == nil {
		f.instances = map[string]map[string]string{}
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	r := &config.Runner{Name: "s", Type: config.TypeAWSSpot, Region: "r-one-1",
		InstanceTypes: []string{"c7a.2xlarge", "c6a.2xlarge"}, Arch: "amd64", User: "ubuntu", VolumeGB: 64,
		SSHCIDR: "0.0.0.0/0", MaxInstances: 4, MaxLifetime: config.Duration{Duration: 2 * time.Hour},
		BootTimeout: config.Duration{Duration: time.Minute}, IdleTimeout: config.Duration{Duration: time.Minute},
		LaunchOrder: config.LaunchCheapest}
	if edit != nil {
		edit(r)
	}
	if r.VCPUs > 0 && r.MinGeneration == 0 {
		r.MinGeneration = config.DefaultMinGeneration
	}
	a := newAWSSpot(r, &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "secret"}})
	a.api.endpoints = map[string]string{"ec2": srv.URL + "/r-one-1/ec2", "ssm": srv.URL + "/r-one-1/ssm",
		"servicequotas": srv.URL + "/r-one-1/servicequotas"}
	return a
}

func resetCalls(f *fakeRegionsEC2) {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func runsOf(f *fakeRegionsEC2) string { return strings.Join(f.callsOf("RunInstances"), "\n") }

// typeXML is one DescribeInstanceTypes item; extra adds raw elements.
func typeXML(name string, vcpus, memGiB int, arch, extra string) string {
	return fmt.Sprintf(`<item><instanceType>%s</instanceType><currentGeneration>true</currentGeneration><bareMetal>false</bareMetal><burstablePerformanceSupported>false</burstablePerformanceSupported><supportedUsageClasses><item>on-demand</item><item>spot</item></supportedUsageClasses><processorInfo><supportedArchitectures><item>%s</item></supportedArchitectures></processorInfo><vCpuInfo><defaultVCpus>%d</defaultVCpus></vCpuInfo><memoryInfo><sizeInMiB>%d</sizeInMiB></memoryInfo>%s</item>`,
		name, arch, vcpus, memGiB*1024, extra)
}

// A single-region runner launches at the cheapest price among its types and
// zones, not in the listed order, and moves down the ranking on capacity
// errors; the failed option cools down for the next launch.
func TestSingleRegionLaunchCheapestFirst(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {
			{"r-one-1a", "c7a.2xlarge", "0.0500"}, {"r-one-1c", "c7a.2xlarge", "0.0450"}, {"r-one-1b", "c6a.2xlarge", "0.0400"},
		}},
		runErr: map[string]string{"r-one-1/c6a.2xlarge": "InsufficientInstanceCapacity"},
	}
	a := newSingleEnv(t, f, nil)
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != "c7a.2xlarge" || in.Zone != "r-one-1c" || in.Region != "r-one-1" {
		t.Fatalf("launched %+v, want c7a.2xlarge in r-one-1c", in)
	}
	if got, want := runsOf(f), "r-one-1 RunInstances c6a.2xlarge subnet-r-one-1b\nr-one-1 RunInstances c7a.2xlarge subnet-r-one-1c"; got != want {
		t.Fatalf("launch order:\n%s\nwant:\n%s", got, want)
	}
	if n := len(f.callsOf("DescribeRegions")); n != 0 {
		t.Fatalf("a single-region runner asked for the enabled regions %d times", n)
	}
	// Nothing about where instances are is recorded for a plain
	// single-region runner (it always lists its one region).
	g := a.ranker()
	if rs, _ := g.activeRegions(); len(rs) != 0 {
		t.Fatalf("recorded active regions %v", rs)
	}
	resetCalls(f)
	if in, err = a.Launch(context.Background(), t.Logf); err != nil || in.Zone != "r-one-1c" {
		t.Fatalf("second launch: %+v, %v", in, err)
	}
	if got := runsOf(f); got != "r-one-1 RunInstances c7a.2xlarge subnet-r-one-1c" {
		t.Fatalf("second launch retried the cooling option: %s", got)
	}
	if n := len(f.callsOf("DescribeSpotPriceHistory")); n != 0 {
		t.Fatal("prices are reused for 15 minutes")
	}
}

// A pinned subnet_id fixes the zone: other zones' prices are not options,
// and launches use the subnet as configured.
func TestSingleRegionSubnetPinsZone(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {
			{"r-one-1a", "c7a.2xlarge", "0.0300"}, {"r-one-1b", "c7a.2xlarge", "0.0600"}, {"r-one-1b", "c6a.2xlarge", "0.0500"},
		}},
		subnetZone: map[string]string{"subnet-mine": "r-one-1b"},
	}
	a := newSingleEnv(t, f, func(r *config.Runner) { r.SubnetID = "subnet-mine" })
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != "c6a.2xlarge" || runsOf(f) != "r-one-1 RunInstances c6a.2xlarge subnet-mine" {
		t.Fatalf("launched %+v via %s", in, runsOf(f))
	}
	tbl, err := a.ranker().Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range tbl.Prices {
		if p.Zone != "r-one-1b" {
			t.Fatalf("an option outside the subnet's zone: %+v", p)
		}
	}
}

// launch_order = "listed" keeps the listed preference: each type in its
// cheapest zone, before the next type.
func TestSingleRegionListedOrder(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {
			{"r-one-1a", "c7a.2xlarge", "0.0500"}, {"r-one-1c", "c7a.2xlarge", "0.0450"}, {"r-one-1b", "c6a.2xlarge", "0.0400"},
		}},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity"},
	}
	a := newSingleEnv(t, f, func(r *config.Runner) { r.LaunchOrder = config.LaunchListed })
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	want := "r-one-1 RunInstances c7a.2xlarge subnet-r-one-1c\nr-one-1 RunInstances c7a.2xlarge subnet-r-one-1a\nr-one-1 RunInstances c6a.2xlarge subnet-r-one-1b"
	if in.Type != "c6a.2xlarge" || runsOf(f) != want {
		t.Fatalf("launched %+v via:\n%s\nwant:\n%s", in, runsOf(f), want)
	}
}

func TestOrderPricesTieBreaksByListedOrder(t *testing.T) {
	r := &config.Runner{InstanceTypes: []string{"m7a.2xlarge", "c7a.2xlarge"}, LaunchOrder: config.LaunchCheapest}
	ps := []SpotPrice{
		{Region: "r", Zone: "r-a", Type: "c7a.2xlarge", Price: "0.05"},
		{Region: "r", Zone: "r-b", Type: "m7a.2xlarge", Price: "0.05"},
		{Region: "r", Zone: "r-a", Type: "c6a.2xlarge", Price: "0.04"},
	}
	orderPrices(r, ps)
	var got []string
	for _, p := range ps {
		got = append(got, p.Type)
	}
	if strings.Join(got, ",") != "c6a.2xlarge,m7a.2xlarge,c7a.2xlarge" {
		t.Fatalf("order %v", got)
	}
}

// Without permission to read prices (an older IAM policy), a fixed-list
// single-region runner still launches, in the listed order.
func TestSingleRegionWithoutPricesUsesListedOrder(t *testing.T) {
	f := &fakeRegionsEC2{priceErr: map[string]string{"r-one-1": "UnauthorizedOperation"},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity"}}
	a := newSingleEnv(t, f, nil)
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != "c6a.2xlarge" || runsOf(f) != "r-one-1 RunInstances c7a.2xlarge \nr-one-1 RunInstances c6a.2xlarge " {
		t.Fatalf("launched %+v via %s", in, runsOf(f))
	}
}

// Without prices to compare, EC2 refuses a bid over max_price
// (SpotMaxPriceTooLow), and on_demand_fallback must not launch that type
// on-demand: its on-demand price is above the cap too (#153). Every Spot
// request carries the cap as the plain decimal EC2 takes.
func TestSingleRegionWithoutPricesKeepsMaxPriceOnDemand(t *testing.T) {
	capped := func(r *config.Runner) {
		r.MaxPrice, r.OnDemandFallback = config.Dollars{V: 1.25, Set: true}, true
	}
	f := &fakeRegionsEC2{priceErr: map[string]string{"r-one-1": "UnauthorizedOperation"},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "SpotMaxPriceTooLow", "r-one-1/c6a.2xlarge": "InsufficientInstanceCapacity"}}
	a := newSingleEnv(t, f, capped)
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != "c6a.2xlarge" || in.Spot || len(f.callsOf("RunInstances")) != 3 {
		t.Fatalf("launched %+v via %s", in, runsOf(f))
	}
	if bids := f.callsOf("bid"); strings.Join(bids, ",") != "r-one-1 bid 1.25,r-one-1 bid 1.25" {
		t.Fatalf("bids %v", bids)
	}
	if markets := f.callsOf("market"); strings.Join(markets, ",") != "r-one-1 market spot,r-one-1 market spot,r-one-1 market on-demand" {
		t.Fatalf("markets %v", markets)
	}
	// Every type above the cap: no on-demand at all, and the error names
	// the price refusal.
	f = &fakeRegionsEC2{priceErr: map[string]string{"r-one-1": "UnauthorizedOperation"},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "SpotMaxPriceTooLow", "r-one-1/c6a.2xlarge": "SpotMaxPriceTooLow"}}
	a = newSingleEnv(t, f, capped)
	_, err = a.Launch(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "SpotMaxPriceTooLow") || strings.Contains(err.Error(), "set on_demand_fallback") {
		t.Fatalf("err %v", err)
	}
	if markets := f.callsOf("market on-demand"); len(markets) != 0 {
		t.Fatalf("fell back to on-demand above max_price: %s", runsOf(f))
	}
}

// With no Spot capacity anywhere, on_demand_fallback tries the types by
// their Spot rank (a stand-in for the on-demand price).
func TestSingleRegionOnDemandByPriceRank(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.0500"}, {"r-one-1b", "c6a.2xlarge", "0.0400"}}},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity", "r-one-1/c6a.2xlarge": "InsufficientInstanceCapacity"},
	}
	a := newSingleEnv(t, f, func(r *config.Runner) { r.OnDemandFallback = true })
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != "c6a.2xlarge" || in.Spot {
		t.Fatalf("launched %+v", in)
	}
	runs := f.callsOf("RunInstances")
	if len(runs) != 3 || runs[2] != "r-one-1 RunInstances c6a.2xlarge " {
		t.Fatalf("runs %v", runs)
	}
}

// A refusal no other zone or type can fix comes back as is, with no
// cooldown keeping the next launch from trying again.
func TestSingleRegionAccountErrorReturnsAtOnce(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.0500"}, {"r-one-1b", "c6a.2xlarge", "0.0400"}}},
		runErr: map[string]string{"r-one-1/c6a.2xlarge": "PendingVerification"},
	}
	a := newSingleEnv(t, f, nil)
	_, err := a.Launch(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "PendingVerification") || len(f.callsOf("RunInstances")) != 1 {
		t.Fatalf("err %v after %v", err, f.callsOf("RunInstances"))
	}
	g := a.ranker()
	tbl, _ := g.Prices(context.Background(), false)
	if c := g.Cooldowns(tbl.Prices); len(c) != 0 {
		t.Fatalf("cooldowns %v", c)
	}
}

func requirementTypes() []string {
	gpu := `<gpuInfo><gpus><item><name>A10G</name><count>1</count></item></gpus></gpuInfo>`
	return []string{
		typeXML("m7a.2xlarge", 8, 32, "x86_64", ""),
		typeXML("r7a.2xlarge", 8, 64, "x86_64", ""),
		typeXML("m6i.2xlarge", 8, 32, "x86_64", ""),
		typeXML("c7a.2xlarge", 8, 16, "x86_64", ""),      // too little memory
		typeXML("m5a.2xlarge", 8, 32, "x86_64", ""),      // generation 5
		typeXML("m7i-flex.2xlarge", 8, 32, "x86_64", ""), // flex
		typeXML("g5.2xlarge", 8, 32, "x86_64", gpu),      // GPU
		typeXML("x2iedn.2xlarge", 8, 256, "x86_64", ""),  // another quota
		typeXML("m7g.2xlarge", 8, 32, "arm64", ""),       // other arch
		typeXML("m7a.4xlarge", 16, 64, "x86_64", ""),     // other size
		strings.Replace(typeXML("t3.2xlarge", 8, 32, "x86_64", ""), "<burstablePerformanceSupported>false", "<burstablePerformanceSupported>true", 1),
		strings.Replace(typeXML("m4.2xlarge", 8, 32, "x86_64", ""), "<currentGeneration>true", "<currentGeneration>false", 1),
		strings.Replace(typeXML("m7a.metal-48xl", 8, 32, "x86_64", ""), "<bareMetal>false", "<bareMetal>true", 1),
	}
}

func reqRunner(r *config.Runner) {
	r.InstanceTypes, r.VCPUs, r.MinMemoryGiB = nil, 8, 32
}

func TestParseDescribeInstanceTypesExclusions(t *testing.T) {
	r := &config.Runner{Type: config.TypeAWSSpot, VCPUs: 8, MinMemoryGiB: 32, MinGeneration: 6, Arch: "amd64"}
	b := []byte(`<DescribeInstanceTypesResponse><instanceTypeSet>` + strings.Join(requirementTypes(), "") + `</instanceTypeSet><nextToken>tok</nextToken></DescribeInstanceTypesResponse>`)
	infos, next, err := parseDescribeInstanceTypes(b, r)
	if err != nil || next != "tok" {
		t.Fatalf("%v %q", err, next)
	}
	var ok []string
	why := map[string]string{}
	for _, in := range infos {
		if in.Excluded == "" {
			ok = append(ok, in.Type)
		}
		why[in.Type] = in.Excluded
	}
	if strings.Join(ok, ",") != "m7a.2xlarge,r7a.2xlarge,m6i.2xlarge" {
		t.Fatalf("eligible %v (%v)", ok, why)
	}
	for typ, want := range map[string]string{"c7a.2xlarge": "16 GiB", "m5a.2xlarge": "generation 5", "m7i-flex.2xlarge": "flex", "g5.2xlarge": "accelerator",
		"x2iedn.2xlarge": "standard Spot quota", "m7g.2xlarge": "not amd64", "m7a.4xlarge": "16 vCPUs", "t3.2xlarge": "burstable",
		"m4.2xlarge": "previous generation", "m7a.metal-48xl": "bare metal"} {
		if !strings.Contains(why[typ], want) {
			t.Errorf("%s excluded for %q, want %q", typ, why[typ], want)
		}
	}
	// The other exclusions, each on its own, and an arm64 runner.
	noSpot := strings.Replace(typeXML("m7a.xlarge", 8, 32, "x86_64", ""), "<item>spot</item>", "", 1)
	for _, c := range []struct{ item, want string }{
		{noSpot, "no Spot"},
		{typeXML("f2.2xlarge", 8, 32, "x86_64", `<fpgaInfo><fpgas><item><name>x</name></item></fpgas></fpgaInfo>`), "accelerator"},
		{typeXML("m7a.2xlarge", 8, 32, "x86_64", `<inferenceAcceleratorInfo><accelerators><item><name>x</name></item></accelerators></inferenceAcceleratorInfo>`), "accelerator"},
		{typeXML("m7a.2xlarge", 8, 32, "x86_64", `<neuronInfo><neuronDevices><item><name>x</name></item></neuronDevices></neuronInfo>`), "accelerator"},
		{typeXML("m7a.2xlarge", 8, 32, "x86_64", `<mediaAcceleratorInfo><accelerators><item><name>x</name></item></accelerators></mediaAcceleratorInfo>`), "accelerator"},
	} {
		got, _, _ := parseDescribeInstanceTypes([]byte(`<DescribeInstanceTypesResponse><instanceTypeSet>`+c.item+`</instanceTypeSet></DescribeInstanceTypesResponse>`), r)
		if len(got) != 1 || !strings.Contains(got[0].Excluded, c.want) {
			t.Errorf("%s: %+v", c.want, got)
		}
	}
	arm := &config.Runner{Type: config.TypeAWSSpot, VCPUs: 8, MinGeneration: 6, Arch: "arm64"}
	got, _, _ := parseDescribeInstanceTypes(b, arm)
	for _, in := range got {
		if (in.Excluded == "") != (in.Type == "m7g.2xlarge") {
			t.Errorf("arm64: %s excluded %q", in.Type, in.Excluded)
		}
	}
	// Patterns narrow (instance_types) and exclude.
	r.InstanceTypes = []string{"m*"}
	r.ExcludeInstanceTypes = []string{"m6*"}
	infos, _, _ = parseDescribeInstanceTypes(b, r)
	ok = nil
	for _, in := range infos {
		if in.Excluded == "" {
			ok = append(ok, in.Type)
		}
	}
	if strings.Join(ok, ",") != "m7a.2xlarge" {
		t.Fatalf("with patterns: %v", ok)
	}
}

func TestTypeGeneration(t *testing.T) {
	for typ, want := range map[string]int{"m7a.2xlarge": 7, "x2iedn.xlarge": 2, "c6in.large": 6, "r7iz.2xlarge": 7, "m10a.large": 10, "weird": 0} {
		if got := typeGeneration(typ); got != want {
			t.Errorf("%s: %d, want %d", typ, got, want)
		}
	}
}

// A vcpus runner considers every eligible type in the region, ranks them by
// Spot price, and never launches one the requirements leave out, however
// cheap.
func TestRequirementLaunchCheapestEligible(t *testing.T) {
	f := &fakeRegionsEC2{
		types: map[string][]string{"r-one-1": requirementTypes()},
		prices: map[string][][3]string{"r-one-1": {
			{"r-one-1a", "m5a.2xlarge", "0.0900"}, // older generation: never
			{"r-one-1a", "c7a.2xlarge", "0.1000"}, // too small: never
			{"r-one-1a", "m7a.2xlarge", "0.1900"},
			{"r-one-1b", "r7a.2xlarge", "0.1400"},
			{"r-one-1c", "m6i.2xlarge", "0.1120"},
		}},
		runErr: map[string]string{"r-one-1/m6i.2xlarge": "InsufficientInstanceCapacity"},
	}
	a := newSingleEnv(t, f, reqRunner)
	in, err := a.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if want := "r-one-1 RunInstances m6i.2xlarge subnet-r-one-1c\nr-one-1 RunInstances r7a.2xlarge subnet-r-one-1b"; runsOf(f) != want || in.Type != "r7a.2xlarge" {
		t.Fatalf("launched %+v via:\n%s\nwant:\n%s", in, runsOf(f), want)
	}
	// The eligible types and prices are cached: the next launch asks for
	// neither.
	resetCalls(f)
	if _, err := a.Launch(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")) + len(f.callsOf("DescribeSpotPriceHistory")); n != 0 {
		t.Fatalf("%d lookups on the second launch, want 0", n)
	}
	tbl, err := a.ranker().Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.MemoryMiB) != 3 || tbl.MemoryMiB["r7a.2xlarge"] != 64*1024 {
		t.Fatalf("memory %v", tbl.MemoryMiB)
	}
}

// The eligible-type list lasts a day, and is fetched again when the
// requirements change.
func TestEligibleTypesCache(t *testing.T) {
	f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()}}
	a := newSingleEnv(t, f, reqRunner)
	ctx := context.Background()
	ts, err := a.EligibleTypes(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range ts {
		names = append(names, x.Type)
	}
	if strings.Join(names, ",") != "m6i.2xlarge,m7a.2xlarge,r7a.2xlarge" {
		t.Fatalf("eligible, smallest memory first: %v", names)
	}
	if got := f.callsOf("DescribeInstanceTypes"); len(got) != 1 || !strings.Contains(got[0], "vcpus=8 arch=x86_64 current=true") {
		t.Fatalf("request %v", got)
	}
	if _, err := a.EligibleTypes(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 1 {
		t.Fatalf("described %d times, want 1 (cached)", n)
	}
	a.r.MinMemoryGiB = 64
	if ts, err = a.EligibleTypes(ctx, false); err != nil || len(ts) != 1 || ts[0].Type != "r7a.2xlarge" {
		t.Fatalf("after raising min_memory_gib: %v %v", ts, err)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 2 {
		t.Fatalf("described %d times, want 2 (requirements changed)", n)
	}
	// A day-old list is described again.
	var c typesCache
	b, _ := os.ReadFile(a.typesCachePath())
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	c.At = time.Now().Add(-25 * time.Hour)
	b, _ = json.Marshal(c)
	if err := os.WriteFile(a.typesCachePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = a.EligibleTypes(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsOf("DescribeInstanceTypes")); n != 3 {
		t.Fatalf("described %d times, want 3 (expired)", n)
	}
	a.r.MinMemoryGiB = 512
	if _, err := a.EligibleTypes(ctx, false); err == nil || !strings.Contains(err.Error(), "no instance type") {
		t.Fatalf("nothing matching: %v", err)
	}
}

// An old IAM policy without DescribeInstanceTypes says how to fix it.
func TestEligibleTypesDeniedHint(t *testing.T) {
	f := &fakeRegionsEC2{typesErr: map[string]string{"r-one-1": "UnauthorizedOperation"}}
	a := newSingleEnv(t, f, reqRunner)
	if _, err := a.Launch(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "ec2:DescribeInstanceTypes") || !strings.Contains(err.Error(), "spotexec setup") {
		t.Fatalf("err %v", err)
	}
	if n := len(f.callsOf("RunInstances")); n != 0 {
		t.Fatalf("%d launches without a type list", n)
	}
}

// Multi-region vcpus runners rank every region's own eligible types.
func TestGlobalRequirementPrices(t *testing.T) {
	f := &fakeRegionsEC2{
		types: map[string][]string{
			"r-one-1": requirementTypes(),
			"r-two-1": {typeXML("m7a.2xlarge", 8, 32, "x86_64", "")},
		},
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "m6i.2xlarge", "0.1100"}, {"r-one-1a", "m5a.2xlarge", "0.0100"}},
			"r-two-1": {{"r-two-1a", "m7a.2xlarge", "0.1000"}, {"r-two-1a", "m6i.2xlarge", "0.0500"}},
		},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	reqRunner(g.r)
	g.r.MinGeneration = config.DefaultMinGeneration
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range tbl.Prices {
		got = append(got, p.Region+"/"+p.Type)
	}
	// r-two-1 doesn't offer m6i (not in its list), and m5a is too old.
	if strings.Join(got, ",") != "r-two-1/m7a.2xlarge,r-one-1/m6i.2xlarge" {
		t.Fatalf("prices %v", got)
	}
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-two-1" || in.Type != "m7a.2xlarge" {
		t.Fatalf("launched %+v %v", in, err)
	}
}

func TestVMVCPUs(t *testing.T) {
	if n := VMVCPUs(&config.Runner{Type: config.TypeAWSSpot, VCPUs: 8, InstanceTypes: []string{"m*"}}); n != 8 {
		t.Fatalf("vcpus runner: %d", n)
	}
	if n := VMVCPUs(&config.Runner{Type: config.TypeAWSSpot, InstanceTypes: []string{"c7a.xlarge"}}); n != 4 {
		t.Fatalf("listed: %d", n)
	}
	r := &config.Runner{Type: config.TypeAWSSpot, VCPUs: 16}
	if MinLaunchVCPUsFor(r) != 16 || HasOtherFamiliesFor(r) {
		t.Fatal("vcpus runners launch standard-quota types of exactly vcpus")
	}
}

func TestDoctorPriceCheckSingleRegion(t *testing.T) {
	f := &fakeRegionsEC2{types: map[string][]string{"r-one-1": requirementTypes()},
		prices: map[string][][3]string{"r-one-1": {{"r-one-1b", "r7a.2xlarge", "0.1400"}, {"r-one-1c", "m6i.2xlarge", "0.1120"}}}}
	a := newSingleEnv(t, f, reqRunner)
	c := a.priceCheck(context.Background())
	if !c.OK || !strings.Contains(c.Msg, "next launch: m6i.2xlarge (32 GiB) in r-one-1c at $0.1120/h") || !strings.Contains(c.Msg, "3 instance types matching 8 vCPUs") {
		t.Fatalf("%+v", c)
	}
	f.priceErr = map[string]string{"r-one-1": "UnauthorizedOperation"}
	if c := a.priceCheck(context.Background()); c.OK || !strings.Contains(c.Msg, "spotexec setup") {
		t.Fatalf("%+v", c)
	}
}

// A quota refusal in one region suggests a quota for the whole fleet, as
// the listed-order launch did.
func TestSingleRegionQuotaSuggestsFleet(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.0500"}, {"r-one-1b", "c6a.2xlarge", "0.0400"}}},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-one-1/c6a.2xlarge": "VcpuLimitExceeded"},
	}
	a := newSingleEnv(t, f, nil)
	_, err := a.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Desired < 4*8 {
		t.Fatalf("err %v (%+v)", err, qe)
	}
}

// A full pinned subnet cools down that subnet, not the zone: another
// subnet there is tried at once.
func TestSubnetCooldownFollowsSubnet(t *testing.T) {
	f := &fakeRegionsEC2{
		prices:     map[string][][3]string{"r-one-1": {{"r-one-1b", "c6a.2xlarge", "0.0400"}}},
		subnetZone: map[string]string{"subnet-full": "r-one-1b", "subnet-ok": "r-one-1b"},
		fullSubnet: map[string]bool{"full": true},
	}
	a := newSingleEnv(t, f, func(r *config.Runner) { r.SubnetID = "subnet-full" })
	if _, err := a.Launch(context.Background(), t.Logf); err == nil {
		t.Fatal("launched into a full subnet")
	}
	a.r.SubnetID = "subnet-ok"
	resetCalls(f)
	if _, err := a.Launch(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if runsOf(f) != "r-one-1 RunInstances c6a.2xlarge subnet-ok" {
		t.Fatalf("runs %s", runsOf(f))
	}
}
