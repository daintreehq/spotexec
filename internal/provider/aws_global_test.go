package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// fakeRegionsEC2 serves just enough of EC2/SSM, per region, for multi-region
// placement: prices, launches, listings and terminations.
type fakeRegionsEC2 struct {
	mu          sync.Mutex
	prices      map[string][][3]string       // region -> {zone, type, price}
	runErr      map[string]string            // "region/type" -> AWS error code for RunInstances
	listErr     map[string]string            // region -> error code for tag-filtered DescribeInstances
	noSubnet    map[string]bool              // zones without a default subnet
	hang        bool                         // RunInstances never answers (until the client gives up)
	fullSubnet  map[string]bool              // zones whose subnet has no free addresses
	launched    string                       // launch time reported for listed instances
	state       map[string]string            // instance ID -> state (default running)
	builders    map[string]bool              // regions holding a bake builder
	instances   map[string]map[string]string // region -> id -> type
	quota       map[string]int               // region -> standard Spot vCPU quota (unset: GetServiceQuota fails)
	dead        map[string]bool              // regions whose endpoint never answers (not enabled); set before use
	hangOwned   map[string]bool              // regions whose by-owner (leftovers) listing never answers
	deny        map[string]bool              // regions answering AuthFailure to everything (not enabled); set before use
	noRegions   bool                         // DescribeRegions is denied everywhere
	priceErr    map[string]string            // region -> error code DescribeSpotPriceHistory answers
	types       map[string][]string          // region -> DescribeInstanceTypes items (typeXML)
	typesErr    map[string]string            // region -> error code DescribeInstanceTypes answers
	subnetZone  map[string]string            // subnet ID -> its zone (DescribeSubnets by ID)
	bakes       map[string]bool              // regions holding a current bake of the runner
	images      map[string]map[string]string // region -> ECR tag -> config digest (nil: no repository)
	imageErr    map[string]string            // region -> error type BatchGetImage answers
	pushed      map[string]map[string]int64  // region -> ECR tag -> push time (Unix ms); DescribeImages pages one image at a time
	describeErr map[string]string            // region -> error type DescribeImages answers
	loginErr    map[string]bool              // regions whose GetAuthorizationToken fails
	policyErr   map[string]bool              // regions whose PutLifecyclePolicy is denied
	cached      map[string]map[string]bool   // region -> dependency cache object keys held
	cacheErr    map[string]int               // region -> HTTP status the cache bucket answers
	onOwned     func(region string)          // called (unlocked) on each by-owner (leftovers) listing
	onRun       func(region string)          // called (unlocked) on each RunInstances
	onList      func(region string)          // called (unlocked) on each fleet (tag-filtered) listing
	calls       []string                     // "region Action [detail]"
	next        int
}

func (f *fakeRegionsEC2) log(region, s string) {
	f.calls = append(f.calls, region+" "+s)
}

func (f *fakeRegionsEC2) callsOf(action string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, " "+action) {
			out = append(out, c)
		}
	}
	return out
}

// describeImages answers ECR DescribeImages from f.images and f.pushed,
// one image per page, newest tag name first (unsorted by time, as ECR).
func (f *fakeRegionsEC2) describeImages(w http.ResponseWriter, region string, body []byte) {
	var in struct {
		Repo   string            `json:"repositoryName"`
		Filter map[string]string `json:"filter"`
		Token  string            `json:"nextToken"`
	}
	_ = json.Unmarshal(body, &in)
	f.log(region, "DescribeImages "+in.Repo+" "+in.Token)
	tags, ok := f.images[region]
	switch {
	case f.describeErr[region] != "":
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"__type":%q,"message":"no"}`, f.describeErr[region])
		return
	case !ok:
		w.WriteHeader(400)
		fmt.Fprint(w, `{"__type":"RepositoryNotFoundException","message":"no repository"}`)
		return
	case in.Filter["tagStatus"] != "TAGGED":
		w.WriteHeader(400)
		return
	}
	var names []string
	for tag := range tags {
		names = append(names, tag)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	i := 0
	if in.Token != "" {
		i, _ = strconv.Atoi(strings.TrimPrefix(in.Token, "page-"))
	}
	out := map[string]any{"imageDetails": []any{}}
	if i < len(names) {
		tag := names[i]
		out["imageDetails"] = []any{map[string]any{"imageDigest": "sha256:m-" + tag, "imageTags": []string{tag},
			"imagePushedAt": float64(f.pushed[region][tag]) / 1000, "imageSizeInBytes": 1000}}
		if i+1 < len(names) {
			out["nextToken"] = fmt.Sprintf("page-%d", i+1)
		}
	}
	b, _ := json.Marshal(out)
	w.Write(b)
}

func (f *fakeRegionsEC2) launchTime() string {
	if f.launched != "" {
		return f.launched
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func ec2Err(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors></Response>`, code, code)
}

func (f *fakeRegionsEC2) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		region, service, _ := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
		body, _ := io.ReadAll(r.Body)
		if f.dead[region] {
			f.mu.Lock()
			f.log(region, "dead")
			f.mu.Unlock()
			<-r.Context().Done()
			return
		}
		if f.deny[region] {
			f.mu.Lock()
			f.log(region, "denied")
			f.mu.Unlock()
			ec2Err(w, 401, "AuthFailure")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if service == "servicequotas" {
			q, ok := f.quota[region]
			if !ok {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"__type":"NoSuchResourceException"}`)
				return
			}
			fmt.Fprintf(w, `{"Quota":{"Value":%d}}`, q)
			return
		}
		if service == "sts" {
			fmt.Fprint(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
			return
		}
		if rest, ok := strings.CutPrefix(service, "s3/"); ok {
			bucket, key, _ := strings.Cut(rest, "/")
			f.log(region, r.Method+" s3 "+rest)
			switch {
			case r.Method != http.MethodHead || r.Header.Get("x-amz-expected-bucket-owner") != "123456789012" || bucket != "spotexec-cache-123456789012-"+region:
				t.Errorf("unexpected S3 request %s %s (owner %q)", r.Method, r.URL, r.Header.Get("x-amz-expected-bucket-owner"))
				w.WriteHeader(400)
			case f.cacheErr[region] != 0:
				w.WriteHeader(f.cacheErr[region])
			case f.cached[region][key]:
			default:
				w.WriteHeader(404)
			}
			return
		}
		if service == "ecr" {
			var in struct {
				Repo  string              `json:"repositoryName"`
				IDs   []map[string]string `json:"imageIds"`
				Types []string            `json:"acceptedMediaTypes"`
			}
			_ = json.Unmarshal(body, &in)
			if r.Header.Get("X-Amz-Target") == "AmazonEC2ContainerRegistry_V20150921.DescribeImages" {
				f.describeImages(w, region, body)
				return
			}
			switch r.Header.Get("X-Amz-Target") {
			case "AmazonEC2ContainerRegistry_V20150921.DescribeRepositories":
				var d struct {
					Names []string `json:"repositoryNames"`
				}
				_ = json.Unmarshal(body, &d)
				f.log(region, "DescribeRepositories "+strings.Join(d.Names, ","))
				if _, ok := f.images[region]; !ok || len(d.Names) != 1 {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"__type":"RepositoryNotFoundException","message":"no repository"}`)
					return
				}
				fmt.Fprintf(w, `{"repositories":[{"repositoryUri":"123456789012.dkr.ecr.%s.amazonaws.com/%s"}]}`, region, d.Names[0])
				return
			case "AmazonEC2ContainerRegistry_V20150921.PutLifecyclePolicy":
				f.log(region, "PutLifecyclePolicy "+in.Repo)
				if f.policyErr[region] {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"__type":"AccessDeniedException","message":"not authorized to perform ecr:PutLifecyclePolicy"}`)
					return
				}
				fmt.Fprint(w, `{}`)
				return
			}
			if r.Header.Get("X-Amz-Target") == "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken" {
				f.log(region, "GetAuthorizationToken")
				if f.loginErr[region] {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"__type":"AccessDeniedException","message":"no"}`)
					return
				}
				fmt.Fprintf(w, `{"authorizationData":[{"authorizationToken":%q,"proxyEndpoint":"https://123456789012.dkr.ecr.%s.amazonaws.com"}]}`,
					base64.StdEncoding.EncodeToString([]byte("AWS:pw-"+region)), region)
				return
			}
			if len(in.IDs) == 0 {
				t.Errorf("unexpected ECR request %s %s", r.Header.Get("X-Amz-Target"), body)
				w.WriteHeader(400)
				return
			}
			f.log(region, "BatchGetImage "+in.Repo+":"+in.IDs[0]["imageTag"])
			if r.Header.Get("X-Amz-Target") != "AmazonEC2ContainerRegistry_V20150921.BatchGetImage" || len(in.Types) == 0 {
				t.Errorf("unexpected ECR request %s %s", r.Header.Get("X-Amz-Target"), body)
			}
			tags, ok := f.images[region]
			switch {
			case f.imageErr[region] != "":
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"__type":%q,"message":"no"}`, f.imageErr[region])
			case !ok:
				w.WriteHeader(400)
				fmt.Fprint(w, `{"__type":"RepositoryNotFoundException","message":"no repository"}`)
			case tags[in.IDs[0]["imageTag"]] == "":
				fmt.Fprint(w, `{"images":[],"failures":[{"failureCode":"ImageNotFound","failureReason":"Requested image not found"}]}`)
			default:
				m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]string{"digest": tags[in.IDs[0]["imageTag"]]}})
				out, _ := json.Marshal(map[string]any{"images": []map[string]any{{"imageId": map[string]string{"imageDigest": "sha256:manifest"},
					"imageManifest": string(m), "imageManifestMediaType": "application/vnd.docker.distribution.manifest.v2+json"}}})
				w.Write(out)
			}
			return
		}
		if service == "ssm" {
			f.log(region, "GetParameter")
			fmt.Fprint(w, `{"Parameter":{"Value":"ami-ubuntu"}}`)
			return
		}
		p, _ := url.ParseQuery(string(body))
		act := p.Get("Action")
		switch act {
		case "DescribeRegions":
			f.log(region, act)
			if f.noRegions {
				ec2Err(w, 403, "UnauthorizedOperation")
				return
			}
			fmt.Fprint(w, `<DescribeRegionsResponse><regionInfo>
<item><regionName>r-one-1</regionName><optInStatus>opt-in-not-required</optInStatus></item>
<item><regionName>r-two-1</regionName><optInStatus>opted-in</optInStatus></item>
<item><regionName>r-three-1</regionName><optInStatus>opt-in-not-required</optInStatus></item>
<item><regionName>r-off-1</regionName><optInStatus>not-opted-in</optInStatus></item>
</regionInfo></DescribeRegionsResponse>`)
		case "DescribeSpotPriceHistory":
			f.log(region, act)
			if code := f.priceErr[region]; code != "" {
				ec2Err(w, 403, code)
				return
			}
			fmt.Fprint(w, `<DescribeSpotPriceHistoryResponse><spotPriceHistorySet>`)
			asked := map[string]bool{} // like AWS: only the requested instance types
			for i := 1; p.Get(fmt.Sprintf("InstanceType.%d", i)) != ""; i++ {
				asked[p.Get(fmt.Sprintf("InstanceType.%d", i))] = true
			}
			for _, row := range f.prices[region] {
				if len(asked) > 0 && !asked[row[1]] {
					continue
				}
				fmt.Fprintf(w, `<item><instanceType>%s</instanceType><productDescription>Linux/UNIX</productDescription><spotPrice>%s</spotPrice><timestamp>2026-11-14T00:00:00.000Z</timestamp><availabilityZone>%s</availabilityZone></item>`, row[1], row[2], row[0])
			}
			fmt.Fprint(w, `</spotPriceHistorySet></DescribeSpotPriceHistoryResponse>`)
		case "DescribeImages":
			if p.Get("Filter.4.Name") == "tag:spotexec-bootstrap-hash" {
				f.log(region, act+" bakes")
				if f.bakes[region] {
					fmt.Fprintf(w, `<DescribeImagesResponse><imagesSet><item><imageId>ami-bake-%s</imageId><imageState>available</imageState><creationDate>%s</creationDate><architecture>x86_64</architecture><tagSet><item><key>spotexec-bootstrap-hash</key><value>%s</value></item><item><key>spotexec-base-ami</key><value>ami-ubuntu</value></item></tagSet></item></imagesSet></DescribeImagesResponse>`,
						region, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), p.Get("Filter.4.Value.1"))
					return
				}
				fmt.Fprint(w, `<DescribeImagesResponse><imagesSet/></DescribeImagesResponse>`)
				return
			}
			if p.Get("ImageId.1") != "" {
				fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item><rootDeviceName>/dev/sda1</rootDeviceName></item></imagesSet></DescribeImagesResponse>`)
			} else {
				fmt.Fprint(w, `<DescribeImagesResponse><imagesSet/></DescribeImagesResponse>`)
			}
		case "DescribeVpcs":
			fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId></item></vpcSet></DescribeVpcsResponse>`)
		case "DescribeSecurityGroups":
			fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-1</groupId><ipPermissions><item><ipProtocol>tcp</ipProtocol><fromPort>22</fromPort><toPort>22</toPort><ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges></item></ipPermissions></item></securityGroupInfo></DescribeSecurityGroupsResponse>`)
		case "DescribeInstanceTypes":
			f.log(region, fmt.Sprintf("%s vcpus=%s arch=%s current=%s", act, p.Get("Filter.1.Value.1"), p.Get("Filter.2.Value.1"), p.Get("Filter.3.Value.1")))
			if code := f.typesErr[region]; code != "" {
				ec2Err(w, 403, code)
				return
			}
			fmt.Fprint(w, `<DescribeInstanceTypesResponse><instanceTypeSet>`+strings.Join(f.types[region], "")+`</instanceTypeSet></DescribeInstanceTypesResponse>`)
		case "DescribeSubnets":
			if id := p.Get("SubnetId.1"); id != "" {
				f.log(region, act+" "+id)
				fmt.Fprintf(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>%s</subnetId><vpcId>vpc-1</vpcId><availabilityZone>%s</availabilityZone></item></subnetSet></DescribeSubnetsResponse>`, id, f.subnetZone[id])
				return
			}
			if f.noSubnet[p.Get("Filter.1.Value.1")] {
				fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet/></DescribeSubnetsResponse>`)
				return
			}
			fmt.Fprintf(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-%s</subnetId></item></subnetSet></DescribeSubnetsResponse>`, p.Get("Filter.1.Value.1"))
		case "RunInstances":
			typ := p.Get("InstanceType")
			market := "spot"
			if p.Get("InstanceMarketOptions.MarketType") == "" {
				market = "on-demand"
			}
			f.log(region, act+" "+typ+" "+p.Get("NetworkInterface.1.SubnetId"))
			f.log(region, "market "+market)
			if bid := p.Get("InstanceMarketOptions.SpotOptions.MaxPrice"); bid != "" {
				f.log(region, "bid "+bid)
			}
			if hook := f.onRun; hook != nil {
				f.mu.Unlock()
				hook(region)
				f.mu.Lock()
			}
			if f.hang {
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			if f.fullSubnet[strings.TrimPrefix(p.Get("NetworkInterface.1.SubnetId"), "subnet-")] {
				ec2Err(w, 400, "InsufficientFreeAddressesInSubnet")
				return
			}
			if code := f.runErr[region+"/"+typ+"/"+market]; code != "" {
				ec2Err(w, 400, code)
				return
			}
			if code := f.runErr[region+"/"+typ]; code != "" && market == "spot" {
				ec2Err(w, 400, code)
				return
			}
			f.next++
			id := fmt.Sprintf("i-%s-%d", region, f.next)
			if f.instances[region] == nil {
				f.instances[region] = map[string]string{}
			}
			f.instances[region][id] = typ
			fmt.Fprintf(w, `<RunInstancesResponse><instancesSet><item><instanceId>%s</instanceId><instanceType>%s</instanceType><instanceState><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`, id, typ)
		case "DescribeInstances":
			if id := p.Get("InstanceId.1"); id != "" {
				f.log(region, act+" "+id)
				typ, ok := f.instances[region][id]
				if !ok {
					ec2Err(w, 400, "InvalidInstanceID.NotFound")
					return
				}
				owner, _ := ownerID()
				fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>%s</instanceId><instanceType>%s</instanceType><instanceState><name>running</name></instanceState><tagSet><item><key>spotexec-runner</key><value>g</value></item><item><key>spotexec-owner</key><value>%s</value></item></tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`, id, typ, owner)
				return
			}
			if p.Get("Filter.1.Name") == "tag:spotexec-bake-runner" {
				f.log(region, act+" builders")
				if f.builders[region] {
					fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-builder</instanceId><instanceState><name>stopped</name></instanceState></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
					return
				}
				fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`)
				return
			}
			want := map[string]bool{}
			for n := 1; p.Get(fmt.Sprintf("Filter.%d.Name", n)) != ""; n++ {
				if p.Get(fmt.Sprintf("Filter.%d.Name", n)) != "instance-state-name" {
					continue
				}
				for i := 1; p.Get(fmt.Sprintf("Filter.%d.Value.%d", n, i)) != ""; i++ {
					want[p.Get(fmt.Sprintf("Filter.%d.Value.%d", n, i))] = true
				}
			}
			// Listed by owner alone (leftovers): bake builders as well.
			byOwner := p.Get("Filter.1.Name") == "tag:spotexec-owner"
			if byOwner {
				f.log(region, act+" owned")
				if hook := f.onOwned; hook != nil {
					f.mu.Unlock()
					hook(region)
					f.mu.Lock()
				}
				if f.hangOwned[region] {
					f.mu.Unlock()
					<-r.Context().Done()
					f.mu.Lock()
					return
				}
			} else {
				f.log(region, act)
				if hook := f.onList; hook != nil {
					f.mu.Unlock()
					hook(region)
					f.mu.Lock()
				}
			}
			if code := f.listErr[region]; code != "" {
				ec2Err(w, 403, code)
				return
			}
			owner, _ := ownerID()
			fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet>`)
			if byOwner && f.builders[region] {
				fmt.Fprintf(w, `<item><instanceId>i-builder</instanceId><instanceState><name>stopped</name></instanceState><tagSet><item><key>spotexec-bake-runner</key><value>g</value></item><item><key>spotexec-owner</key><value>%s</value></item></tagSet></item>`, owner)
			}
			for id, typ := range f.instances[region] {
				st := f.state[id]
				if st == "" {
					st = "running"
				}
				if len(want) > 0 && !want[st] {
					continue
				}
				fmt.Fprintf(w, `<item><instanceId>%s</instanceId><instanceType>%s</instanceType><instanceState><name>%s</name></instanceState><launchTime>%s</launchTime><placement><availabilityZone>%sa</availabilityZone></placement><tagSet><item><key>spotexec-runner</key><value>g</value></item><item><key>spotexec-owner</key><value>%s</value></item></tagSet></item>`, id, typ, st, f.launchTime(), region, owner)
			}
			fmt.Fprint(w, `</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		case "TerminateInstances":
			ids := []string{}
			for i := 1; p.Get(fmt.Sprintf("InstanceId.%d", i)) != ""; i++ {
				id := p.Get(fmt.Sprintf("InstanceId.%d", i))
				ids = append(ids, id)
				delete(f.instances[region], id)
			}
			f.log(region, act+" "+strings.Join(ids, " "))
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)
		default:
			t.Errorf("unexpected %s %s", region, act)
			ec2Err(w, 400, "InvalidAction")
		}
	})
}

func newGlobalEnv(t *testing.T, f *fakeRegionsEC2, regions ...string) *AWSGlobal {
	t.Helper()
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	if f.instances == nil {
		f.instances = map[string]map[string]string{}
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	r := &config.Runner{Name: "g", Type: config.TypeAWSSpot, Regions: regions, Region: "r-one-1",
		InstanceTypes: []string{"c7a.2xlarge", "c6a.2xlarge"}, Arch: "amd64", User: "ubuntu", VolumeGB: 64,
		SSHCIDR: "0.0.0.0/0", MaxInstances: 4, MaxLifetime: config.Duration{Duration: 2 * time.Hour},
		BootTimeout: config.Duration{Duration: time.Minute}, IdleTimeout: config.Duration{Duration: time.Minute}}
	p, err := NewAWSGlobal(r)
	if err != nil {
		t.Fatal(err)
	}
	g := p.(*AWSGlobal)
	g.creds = &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKID", SecretAccessKey: "secret"}}
	g.endpoints = func(region string) map[string]string {
		return map[string]string{"ec2": srv.URL + "/" + region + "/ec2", "ssm": srv.URL + "/" + region + "/ssm",
			"servicequotas": srv.URL + "/" + region + "/servicequotas", "sts": srv.URL + "/" + region + "/sts",
			"ecr": srv.URL + "/" + region + "/ecr", "s3": srv.URL + "/" + region + "/s3"}
	}
	return g
}

// discovered marks the one-time listing of every region as done.
func discovered(t *testing.T, g *AWSGlobal) {
	t.Helper()
	if err := g.withState(func(st *placementState) { st.Discovered = true }); err != nil {
		t.Fatal(err)
	}
}

func active(t *testing.T, g *AWSGlobal) string {
	t.Helper()
	rs, err := g.activeRegions()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(rs, ",")
}

func TestGlobalLaunchCheapestWithFallback(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.0500"}, {"r-one-1b", "c6a.2xlarge", "0.0550"}},
			"r-two-1":   {{"r-two-1c", "c6a.2xlarge", "0.0600"}},
			"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.0400"}, {"r-three-1b", "c6a.2xlarge", "0.0450"}},
		},
		runErr: map[string]string{
			"r-three-1/c7a.2xlarge": "VcpuLimitExceeded",            // whole region out of quota
			"r-one-1/c7a.2xlarge":   "InsufficientInstanceCapacity", // just this zone/type
		},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// r-three-1's second option is skipped (its quota is regional);
	// r-one-1's second option is tried (capacity is per zone and type).
	if in.Region != "r-one-1" || in.Type != "c6a.2xlarge" || in.Zone != "r-one-1b" {
		t.Fatalf("launched %+v, want c6a.2xlarge in r-one-1b", in)
	}
	runs := f.callsOf("RunInstances")
	want := []string{
		"r-three-1 RunInstances c7a.2xlarge subnet-r-three-1a",
		"r-one-1 RunInstances c7a.2xlarge subnet-r-one-1a",
		"r-one-1 RunInstances c6a.2xlarge subnet-r-one-1b",
	}
	if strings.Join(runs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("launch order:\n%s\nwant:\n%s", strings.Join(runs, "\n"), strings.Join(want, "\n"))
	}
	// Every region a request went to is remembered, so listings look there.
	if got := active(t, g); got != "r-one-1,r-three-1" {
		t.Fatalf("active regions %s", got)
	}
	// The capacity cooldown stops the next launch retrying r-one-1a; the
	// quota refusal is asked again (a VM may have ended since), then it
	// goes to the pool that worked.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := g.Launch(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if runs := f.callsOf("RunInstances"); strings.Join(runs, "\n") != "r-three-1 RunInstances c7a.2xlarge subnet-r-three-1a\nr-one-1 RunInstances c6a.2xlarge subnet-r-one-1b" {
		t.Fatalf("second launch: %v", runs)
	}
	// Prices came from the cache the second time.
	if n := len(f.callsOf("DescribeSpotPriceHistory")); n != 0 {
		t.Fatalf("re-fetched prices %d times", n)
	}
}

// A region whose Spot quota has no room is skipped by the pre-launch check,
// with nothing sent there, and the next-cheapest region is used.
func TestGlobalLaunchSkipsRegionWithoutQuota(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.0500"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.0400"}, {"r-two-1b", "c6a.2xlarge", "0.0450"}},
		},
		quota: map[string]int{"r-one-1": 64, "r-two-1": 4}, // r-two-1 can't fit one 8-vCPU VM
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Region != "r-one-1" {
		t.Fatalf("launched %+v, want r-one-1", in)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 || !strings.HasPrefix(runs[0], "r-one-1 ") {
		t.Fatalf("RunInstances calls %v: nothing should be sent to the region without quota", runs)
	}
}

func TestGlobalLaunchStopsOnConfigError(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.06"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InvalidUserData.Malformed"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g.Launch(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "InvalidUserData.Malformed") {
		t.Fatalf("err = %v", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 {
		t.Fatalf("a request that fails everywhere must not be retried in other regions: %v", runs)
	}
}

func TestGlobalAllRegionsSkipsNotOptedIn(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.05"}},
	}}
	g := newGlobalEnv(t, f, config.AllRegions)
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tbl.Regions, ","); got != "r-one-1,r-three-1,r-two-1" {
		t.Fatalf("regions %s", got)
	}
	if tbl.Prices[0].Region != "r-two-1" {
		t.Fatalf("cheapest %+v", tbl.Prices[0])
	}
	for _, c := range f.callsOf("DescribeSpotPriceHistory") {
		if strings.HasPrefix(c, "r-off-1") {
			t.Fatal("priced a region that isn't enabled")
		}
	}
}

// exclude_regions leaves enabled regions out of regions = ["all"]: they are
// never priced or launched in, and show as not used with the reason.
func TestGlobalAllRegionsExcluded(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.07"}},
		"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.01"}},
		"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.06"}},
	}}
	g := newGlobalEnv(t, f, config.AllRegions)
	g.r.ExcludeRegions = []string{"r-two-1"}
	placed, err := g.PlacementRegions(context.Background())
	if err != nil || strings.Join(placed, ",") != "r-one-1,r-three-1" {
		t.Fatalf("placement regions %v, %v", placed, err)
	}
	if w := g.Where(); !strings.Contains(w, "except r-two-1") {
		t.Fatalf("where %q", w)
	}
	for i, refresh := range []bool{false, false, true} { // fresh, cached, refreshed
		tbl, err := g.Prices(context.Background(), refresh)
		if err != nil {
			t.Fatal(err)
		}
		if tbl.Cached != (i == 1) {
			t.Fatalf("call %d: cached = %v", i, tbl.Cached)
		}
		if got := strings.Join(tbl.Regions, ","); got != "r-one-1,r-three-1,r-two-1" {
			t.Fatalf("regions %s", got)
		}
		if why := tbl.Unpriced["r-two-1"]; !strings.Contains(why, "exclude_regions") || !lastingUnpriced(why) {
			t.Fatalf("unpriced %v", tbl.Unpriced)
		}
		if len(tbl.Prices) != 2 || tbl.Prices[0].Region != "r-three-1" {
			t.Fatalf("prices %+v", tbl.Prices)
		}
	}
	for _, c := range f.callsOf("DescribeSpotPriceHistory") {
		if strings.HasPrefix(c, "r-two-1") {
			t.Fatal("priced an excluded region")
		}
	}
	// Excluding a region that isn't enabled leaves placement as it was, but
	// the cached table must not hide the new exclusion.
	g.r.ExcludeRegions = []string{"r-off-1", "r-two-1"}
	tbl, err := g.Prices(context.Background(), false)
	if err != nil || tbl.Cached || tbl.Unpriced["r-off-1"] != excludedWhy {
		t.Fatalf("after excluding r-off-1: cached %v, unpriced %v, %v", tbl.Cached, tbl.Unpriced, err)
	}
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if in.Region != "r-three-1" {
		t.Fatalf("launched in %s", in.Region)
	}
}

// A VM launched in a region before it was excluded is still listed (and so
// counted and torn down), even with no local record of it.
func TestGlobalExcludedRegionStillListed(t *testing.T) {
	f := &fakeRegionsEC2{
		prices:    map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}},
		instances: map[string]map[string]string{"r-two-1": {"i-old": "c7a.2xlarge"}},
	}
	g := newGlobalEnv(t, f, config.AllRegions)
	g.r.ExcludeRegions = []string{"r-two-1"}
	ins, err := g.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].ID != "i-old" || ins[0].Region != "r-two-1" {
		t.Fatalf("listed %+v", ins)
	}
}

// Quota refusals in every region left after the exclusions are still a
// quota verdict: an excluded region isn't an option that might have worked.
func TestGlobalExcludedRegionKeepsQuotaVerdict(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.04"}},
			"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.01"}},
			"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.05"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-three-1/c7a.2xlarge": "VcpuLimitExceeded"},
	}
	g := newGlobalEnv(t, f, config.AllRegions)
	g.r.ExcludeRegions = []string{"r-two-1"}
	_, err := g.Launch(context.Background(), t.Logf)
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("want a QuotaError, got %v", err)
	}
}

func TestGlobalUnofferedRegionUnpriced(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	tbl, err := g.Prices(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Prices) != 1 || tbl.Unpriced["r-two-1"] == "" {
		t.Fatalf("table %+v", tbl)
	}
}

func TestGlobalListFailsIfARegionFails(t *testing.T) {
	f := &fakeRegionsEC2{
		instances: map[string]map[string]string{"r-one-1": {"i-a": "c7a.2xlarge"}, "r-two-1": {"i-b": "c7a.2xlarge"}},
		listErr:   map[string]string{"r-two-1": "UnauthorizedOperation"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	g.markActive("r-one-1", time.Now().Add(time.Hour))
	g.markActive("r-two-1", time.Now().Add(time.Hour))
	if _, err := g.List(context.Background()); err == nil {
		t.Fatal("a listing missing one region must fail, not report its instances gone")
	}
	f.mu.Lock()
	delete(f.listErr, "r-two-1")
	f.mu.Unlock()
	ins, err := g.List(context.Background())
	if err != nil || len(ins) != 2 {
		t.Fatalf("list = %v, %v", ins, err)
	}
	where := map[string]string{}
	for _, in := range ins {
		where[in.ID] = in.Region + "/" + in.Zone
	}
	if where["i-a"] != "r-one-1/r-one-1a" || where["i-b"] != "r-two-1/r-two-1a" {
		t.Fatalf("locations %v", where)
	}
}

// Status lists what it can reach: a stalled region is reported by name
// after a short wait, the others' instances still show, and the stalled
// region is neither forgotten nor allowed to make List itself lenient.
func TestGlobalListReachableSkipsStalledRegion(t *testing.T) {
	f := &fakeRegionsEC2{
		instances: map[string]map[string]string{"r-one-1": {"i-a": "c7a.2xlarge"}, "r-two-1": {"i-b": "c7a.2xlarge"}},
		dead:      map[string]bool{"r-two-1": true},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	g.markActive("r-one-1", time.Now().Add(time.Hour))
	g.markActive("r-two-1", time.Now().Add(-time.Minute)) // due to be dropped once seen empty
	defer func(d time.Duration) { regionListTimeout = d }(regionListTimeout)
	regionListTimeout = 200 * time.Millisecond
	start := time.Now()
	ins, unlisted, err := g.ListReachable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %s on the stalled region", d)
	}
	if len(ins) != 1 || ins[0].ID != "i-a" {
		t.Fatalf("listed %v", ins)
	}
	if len(unlisted) != 1 || unlisted["r-two-1"] == nil || !strings.Contains(unlisted["r-two-1"].Error(), "no answer within 200ms") || !errors.Is(unlisted["r-two-1"], context.DeadlineExceeded) {
		t.Fatalf("unlisted %v", unlisted)
	}
	if got := active(t, g); got != "r-one-1,r-two-1" {
		t.Fatalf("a region that couldn't be listed was forgotten: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := g.List(ctx); err == nil {
		t.Fatal("List must still fail when a region can't be listed")
	}
}

// A region that answers with an error is reported the same way, at once.
func TestGlobalListReachableReportsRegionError(t *testing.T) {
	f := &fakeRegionsEC2{
		instances: map[string]map[string]string{"r-one-1": {"i-a": "c7a.2xlarge"}, "r-two-1": {"i-b": "c7a.2xlarge"}},
		listErr:   map[string]string{"r-two-1": "UnauthorizedOperation"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	g.markActive("r-one-1", time.Now().Add(time.Hour))
	g.markActive("r-two-1", time.Now().Add(time.Hour))
	ins, unlisted, err := g.ListReachable(context.Background())
	if err != nil || len(ins) != 1 || ins[0].ID != "i-a" {
		t.Fatalf("list = %v, %v", ins, err)
	}
	if e := unlisted["r-two-1"]; e == nil || !strings.Contains(e.Error(), "UnauthorizedOperation") {
		t.Fatalf("unlisted %v", unlisted)
	}
}

// The first listing only counts as discovery once every region answered:
// a region that couldn't be listed is looked at again next time.
func TestGlobalListReachableKeepsDiscoveringFailedRegion(t *testing.T) {
	f := &fakeRegionsEC2{
		instances: map[string]map[string]string{"r-one-1": {"i-a": "c7a.2xlarge"}, "r-two-1": {"i-b": "c7a.2xlarge"}},
		listErr:   map[string]string{"r-two-1": "RequestLimitExceeded"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	ins, unlisted, err := g.ListReachable(context.Background())
	if err != nil || len(ins) != 1 || unlisted["r-two-1"] == nil {
		t.Fatalf("list = %v, %v, %v", ins, unlisted, err)
	}
	f.mu.Lock()
	delete(f.listErr, "r-two-1")
	f.mu.Unlock()
	ins, unlisted, err = g.ListReachable(context.Background())
	if err != nil || len(ins) != 2 || len(unlisted) != 0 {
		t.Fatalf("list = %v, %v, %v", ins, unlisted, err)
	}
}

// A first listing doesn't wait out a home region that never answers the
// enabled-regions lookup: it lists what it can and stays undiscovered.
func TestGlobalListReachableBoundsDiscovery(t *testing.T) {
	f := &fakeRegionsEC2{
		instances: map[string]map[string]string{"r-one-1": {"i-a": "c7a.2xlarge"}},
		dead:      map[string]bool{"r-off-1": true},
	}
	g := newGlobalEnv(t, f, "r-off-1", "r-one-1")
	g.r.Region = "r-off-1"
	defer func(d time.Duration) { regionListTimeout = d }(regionListTimeout)
	regionListTimeout = 300 * time.Millisecond
	start := time.Now()
	ins, unlisted, err := g.ListReachable(context.Background())
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("waited %s on discovery", d)
	}
	if err != nil || len(ins) != 1 || ins[0].ID != "i-a" || unlisted["r-off-1"] == nil {
		t.Fatalf("list = %v, %v, %v", ins, unlisted, err)
	}
	var disc bool
	_ = g.withState(func(st *placementState) { disc = st.Discovered })
	if disc {
		t.Fatal("discovery counted done with a region unlisted")
	}
}

// A region found empty but whose leftovers check stalls is kept, and the
// listing doesn't wait the stall out.
func TestGlobalListReachableBoundsLeftovers(t *testing.T) {
	f := &fakeRegionsEC2{
		instances: map[string]map[string]string{"r-one-1": {"i-a": "c7a.2xlarge"}},
		hangOwned: map[string]bool{"r-two-1": true},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	g.markActive("r-one-1", time.Now().Add(time.Hour))
	g.markActive("r-two-1", time.Now().Add(-time.Minute)) // empty and due to be dropped
	defer func(d time.Duration) { regionListTimeout = d }(regionListTimeout)
	regionListTimeout = 300 * time.Millisecond
	start := time.Now()
	ins, unlisted, err := g.ListReachable(context.Background())
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("waited %s on leftovers", d)
	}
	if err != nil || len(ins) != 1 || len(unlisted) != 0 {
		t.Fatalf("list = %v, %v, %v", ins, unlisted, err)
	}
	if len(f.callsOf("DescribeInstances owned")) == 0 {
		t.Fatal("leftovers never checked")
	}
	if got := active(t, g); got != "r-one-1,r-two-1" {
		t.Fatalf("a region whose leftovers weren't checked was forgotten: %q", got)
	}
}

func TestGlobalListOnlyQueriesActiveRegions(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-b": "c7a.2xlarge"}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	discovered(t, g)
	g.markActive("r-two-1", time.Now().Add(time.Hour))
	ins, err := g.List(context.Background())
	if err != nil || len(ins) != 1 || ins[0].ID != "i-b" {
		t.Fatalf("list = %v, %v", ins, err)
	}
	if got := f.callsOf("DescribeInstances"); len(got) != 1 || got[0] != "r-two-1 DescribeInstances" {
		t.Fatalf("listed %v", got)
	}
}

// First use (or an upgrade from a single-region runner, or a lost state
// file): an existing VM must be found, not replaced.
func TestGlobalFirstListDiscoversEverywhere(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-old": "c7a.2xlarge"}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	ins, err := g.List(context.Background())
	if err != nil || len(ins) != 1 || ins[0].ID != "i-old" {
		t.Fatalf("list = %v, %v", ins, err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("DescribeInstances"); len(got) != 1 || got[0] != "r-two-1 DescribeInstances" {
		t.Fatalf("after discovery only the region with a VM is listed: %v", got)
	}
}

// A region past its expected lifetime is kept while a VM is still there
// (a failed watchdog) and dropped only once a listing finds it empty.
func TestGlobalExpiredRegionKeptUntilEmpty(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-zombie": "c7a.2xlarge"}},
		launched: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	discovered(t, g)
	g.markActive("r-two-1", time.Now().Add(-time.Minute))
	if ins, err := g.List(context.Background()); err != nil || len(ins) != 1 {
		t.Fatalf("list = %v, %v", ins, err)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("a region with a live VM was forgotten: %q", got)
	}
	f.mu.Lock()
	delete(f.instances["r-two-1"], "i-zombie")
	f.mu.Unlock()
	// Still tracked for an hour after it was last seen.
	g.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "" {
		t.Fatalf("an expired, empty region should be dropped: %q", got)
	}
}

func TestGlobalCorruptStateIsAnError(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	os.MkdirAll(g.dir, 0o700)
	os.WriteFile(filepath.Join(g.dir, "placement.json"), []byte("{not json"), 0o600)
	if _, err := g.List(context.Background()); err == nil {
		t.Fatal("unreadable placement state must not look like an empty fleet")
	}
	if _, err := g.Launch(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("must not launch without recording where: %v", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 0 {
		t.Fatalf("launched with unrecordable state: %v", runs)
	}
}

// A request whose response is lost (here: cancelled after sending) may
// have created a VM: report it as ambiguous and launch nothing elsewhere.
func TestGlobalAmbiguousLaunchStops(t *testing.T) {
	f := &fakeRegionsEC2{hang: true, prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.06"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g.Prices(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := g.Launch(ctx, t.Logf)
	if !errors.Is(err, ErrLaunchAmbiguous) {
		t.Fatalf("err = %v, want ErrLaunchAmbiguous", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 {
		t.Fatalf("launched again elsewhere: %v", runs)
	}
	if got := active(t, g); got != "r-one-1" {
		t.Fatalf("the region of the ambiguous request must be listed: %q", got)
	}
}

// Spot quota exhausted everywhere: on-demand fallback still has regions.
func TestGlobalOnDemandFallbackAfterSpotQuota(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.06"}},
		},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "MaxSpotInstanceCountExceeded", "r-two-1/c7a.2xlarge": "MaxSpotInstanceCountExceeded"},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.r.OnDemandFallback = true
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	markets := f.callsOf("market")
	if in.Region != "r-one-1" || len(markets) != 3 || markets[2] != "r-one-1 market on-demand" {
		t.Fatalf("launched %+v via %v", in, markets)
	}
	// Without the fallback, the same failures are an error.
	g2 := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g2.Launch(context.Background(), t.Logf); err == nil {
		t.Fatal("no Spot quota and no fallback should fail")
	}
}

func TestGlobalFullSubnetTriesNextZone(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {
			{"r-one-1a", "c7a.2xlarge", "0.05"}, {"r-one-1a", "c6a.2xlarge", "0.051"}, {"r-one-1b", "c7a.2xlarge", "0.06"},
		}},
		fullSubnet: map[string]bool{"r-one-1a": true},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// The whole zone is skipped (c6a in r-one-1a is never tried).
	runs := f.callsOf("RunInstances")
	want := "r-one-1 RunInstances c7a.2xlarge subnet-r-one-1a\nr-one-1 RunInstances c7a.2xlarge subnet-r-one-1b"
	if strings.Join(runs, "\n") != want || in.Zone != "r-one-1b" {
		t.Fatalf("launched %+v via %v", in, runs)
	}
}

func TestGlobalTerminateFindsRegion(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-b": "c7a.2xlarge"}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	// Not in local state (e.g. launched by a process whose state was lost):
	// found by asking each region.
	if err := g.Terminate(context.Background(), "i-b"); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("TerminateInstances"); len(got) != 1 || got[0] != "r-two-1 TerminateInstances i-b" {
		t.Fatalf("terminate calls %v", got)
	}
	if err := g.Terminate(context.Background(), "i-missing"); err == nil {
		t.Fatal("terminating an unknown instance should fail")
	}
}

func TestGlobalDownCoversRemovedRegion(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-old-1": {"i-old": "c7a.2xlarge"}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	// r-old-1 was in the config when i-old launched; it has since been
	// removed. down must still end it.
	g.markActive("r-old-1", time.Now().Add(time.Hour))
	if err := g.Down(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOf("TerminateInstances"); len(got) != 1 || !strings.HasPrefix(got[0], "r-old-1 ") {
		t.Fatalf("terminate calls %v", got)
	}
}

func TestPlacementStateFile(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.07"}}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g.Launch(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(g.dir, "placement.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st placementState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Where) != 1 || st.Active["r-one-1"].IsZero() {
		t.Fatalf("state %s", b)
	}
}

func TestLatestPricesNotHistoricalMinimum(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	rows := []SpotPrice{
		{Region: "r", Zone: "ra", Type: "c7a.2xlarge", Price: "0.0100", Since: now.Add(-48 * time.Hour)}, // no longer applies
		{Region: "r", Zone: "ra", Type: "c7a.2xlarge", Price: "0.0800", Since: now.Add(-time.Hour)},
		{Region: "r", Zone: "rb", Type: "c7a.2xlarge", Price: "0.0600", Since: now.Add(-90 * 24 * time.Hour)}, // old but current
		{Region: "r", Zone: "rb", Type: "c7a.2xlarge", Price: "0.0001", Since: now.Add(time.Hour)},            // future: ignore
	}
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}} {
		var in []SpotPrice
		for _, i := range order {
			in = append(in, rows[i])
		}
		got := latestPrices(in, now)
		if len(got) != 2 || got[0].Zone != "rb" || got[0].Price != "0.0600" || got[1].Price != "0.0800" {
			t.Fatalf("order %v: latest = %+v", order, got)
		}
	}
}

func TestParseSpotPriceHistoryFilters(t *testing.T) {
	b := []byte(`<DescribeSpotPriceHistoryResponse><spotPriceHistorySet>
<item><instanceType>c7a.2xlarge</instanceType><productDescription>Windows</productDescription><spotPrice>0.01</spotPrice><timestamp>2026-11-14T00:00:00.000Z</timestamp><availabilityZone>ra</availabilityZone></item>
<item><instanceType>c7a.2xlarge</instanceType><productDescription>Linux/UNIX</productDescription><spotPrice>0</spotPrice><timestamp>2026-11-14T00:00:00.000Z</timestamp><availabilityZone>ra</availabilityZone></item>
<item><instanceType>c7a.2xlarge</instanceType><productDescription>Linux/UNIX</productDescription><spotPrice>bogus</spotPrice><timestamp>2026-11-14T00:00:00.000Z</timestamp><availabilityZone>ra</availabilityZone></item>
<item><instanceType>c7a.2xlarge</instanceType><productDescription>Linux/UNIX (Amazon VPC)</productDescription><spotPrice>0.0712</spotPrice><timestamp>2026-11-14T00:00:00.000Z</timestamp><availabilityZone>rb</availabilityZone></item>
</spotPriceHistorySet><nextToken>tok</nextToken></DescribeSpotPriceHistoryResponse>`)
	ps, next, err := parseSpotPriceHistory("r", b)
	if err != nil || next != "tok" {
		t.Fatal(err, next)
	}
	if len(ps) != 1 || ps[0].Zone != "rb" || ps[0].Price != "0.0712" {
		t.Fatalf("parsed %+v (Windows, zero and malformed prices must be dropped)", ps)
	}
}

func TestRankPricesDeterministic(t *testing.T) {
	ps := []SpotPrice{
		{Region: "b", Zone: "b1", Type: "x", Price: "0.05"},
		{Region: "a", Zone: "a2", Type: "y", Price: "0.050"},
		{Region: "a", Zone: "a2", Type: "x", Price: "0.05"},
		{Region: "c", Zone: "c1", Type: "x", Price: "0.04"},
	}
	rankPrices(ps)
	var got []string
	for _, p := range ps {
		got = append(got, poolKey(p))
	}
	if s := strings.Join(got, " "); s != "c/c1/x a/a2/x a/a2/y b/b1/x" {
		t.Fatal(s)
	}
}

func TestForInstanceFallsBack(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-b": "c7a.2xlarge"}}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	p := ForInstance(context.Background(), g, "i-b")
	if p.Runner().Region != "r-two-1" {
		t.Fatalf("cache provider in %s, want the instance's region", p.Runner().Region)
	}
	if _, ok := p.(DepsCache); !ok {
		t.Fatal("the instance's regional provider should offer the cache")
	}
	if _, ok := p.(ImageRegistry); !ok {
		t.Fatal("the instance's regional provider should offer the registry")
	}
	q := ForInstance(context.Background(), g, "i-gone")
	if q != Provider(g) {
		t.Fatalf("an unknown instance should fall back to the coordinator, got %T", q)
	}
	if _, ok := q.(DepsCache); ok {
		t.Fatal("an unknown instance must get no cache, not the wrong region's")
	}
}

func TestGlobalZoneWithoutSubnetTriesNextZone(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}, {"r-one-1b", "c7a.2xlarge", "0.06"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.07"}},
		},
		noSubnet: map[string]bool{"r-one-1a": true},
	}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 || runs[0] != "r-one-1 RunInstances c7a.2xlarge subnet-r-one-1b" || in.Region != "r-one-1" {
		t.Fatalf("a zone without a subnet should only skip that zone: %v", runs)
	}
}

func TestGlobalMaxPriceFiltersOptions(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.30"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.r.MaxPrice = config.Dollars{V: 0.04, Set: true}
	_, err := g.Launch(context.Background(), t.Logf)
	// Typed, so a run with no VM to wait for fails at once naming it.
	var pe *PriceCapError
	if !errors.As(err, &pe) || pe.Cheapest.Zone != "r-one-1a" || !strings.Contains(err.Error(), "above max_price 0.04 (cheapest: $0.05/h") {
		t.Fatalf("err = %v", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 0 {
		t.Fatalf("launched above max_price: %v", runs)
	}
	// A region that couldn't be priced just now may be under the cap:
	// not a lasting verdict.
	f.mu.Lock()
	f.priceErr = map[string]string{"r-three-1": "RequestLimitExceeded"}
	f.mu.Unlock()
	gu := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	gu.r.MaxPrice = config.Dollars{V: 0.04, Set: true}
	if _, err := gu.Launch(context.Background(), t.Logf); err == nil || errors.As(err, &pe) {
		t.Fatalf("with a region unpriced: %v", err)
	}
	f.mu.Lock()
	f.priceErr = nil
	f.mu.Unlock()
	// With launch_order = "listed", the first option isn't the cheapest:
	// the error still names the cheapest.
	fl := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.10"}, {"r-one-1b", "c6a.2xlarge", "0.02"}},
	}}
	gl := newGlobalEnv(t, fl, "r-one-1", "r-two-1")
	gl.r.MaxPrice, gl.r.LaunchOrder = config.Dollars{V: 0.01, Set: true}, config.LaunchListed
	if _, err := gl.Launch(context.Background(), t.Logf); !errors.As(err, &pe) || pe.Cheapest.Type != "c6a.2xlarge" {
		t.Fatalf("listed order: %v", err)
	}
	// At the cap is fine.
	g2 := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g2.r.MaxPrice = config.Dollars{V: 0.05, Set: true}
	if in, err := g2.Launch(context.Background(), t.Logf); err != nil || in.Region != "r-one-1" {
		t.Fatalf("at the cap: %+v %v", in, err)
	}
	// The option under the cap fails: the one above it must not be tried.
	f.mu.Lock()
	f.calls, f.runErr = nil, map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity"}
	f.mu.Unlock()
	g3 := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g3.r.MaxPrice = config.Dollars{V: 0.05, Set: true}
	if _, err := g3.Launch(context.Background(), t.Logf); err == nil || errors.As(err, &pe) {
		t.Fatalf("fell back above max_price, or blamed the cap for a capacity refusal: %v", err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 1 || !strings.HasPrefix(runs[0], "r-one-1 ") {
		t.Fatalf("runs %v", runs)
	}
}

// A Spot price is never above the on-demand price, so a type priced above
// max_price on Spot is above it on-demand too: on_demand_fallback must not
// launch it, and a launch with every option over the cap stays a
// PriceCapError rather than a fallback (#153).
func TestGlobalMaxPriceCapsOnDemandFallback(t *testing.T) {
	capUSD := config.Dollars{V: 0.04, Set: true}
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.05"}},
		"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.30"}},
	}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	g.r.MaxPrice, g.r.OnDemandFallback = capUSD, true
	var pe *PriceCapError
	if in, err := g.Launch(context.Background(), t.Logf); !errors.As(err, &pe) {
		t.Fatalf("every option above max_price: launched %+v, err %v", in, err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 0 {
		t.Fatalf("fell back to on-demand above max_price: %v", runs)
	}
	// One region: the same.
	a := newSingleEnv(t, f, func(r *config.Runner) { r.MaxPrice, r.OnDemandFallback = capUSD, true })
	if in, err := a.Launch(context.Background(), t.Logf); !errors.As(err, &pe) {
		t.Fatalf("one region: launched %+v, err %v", in, err)
	}
	if runs := f.callsOf("RunInstances"); len(runs) != 0 {
		t.Fatalf("one region fell back to on-demand above max_price: %v", runs)
	}
	// Under the cap but out of Spot capacity: on-demand tries only the type
	// whose Spot price is under the cap.
	fm := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c6a.2xlarge", "0.03"}, {"r-one-1b", "c7a.2xlarge", "0.10"}}},
		runErr: map[string]string{"r-one-1/c6a.2xlarge/on-demand": "InsufficientInstanceCapacity", "r-one-1/c6a.2xlarge": "InsufficientInstanceCapacity"},
	}
	gm := newGlobalEnv(t, fm, "r-one-1", "r-two-1")
	gm.r.MaxPrice, gm.r.OnDemandFallback = capUSD, true
	if in, err := gm.Launch(context.Background(), t.Logf); err == nil || errors.As(err, &pe) {
		t.Fatalf("capacity short under the cap: launched %+v, err %v", in, err)
	}
	for _, run := range fm.callsOf("RunInstances") {
		if strings.Contains(run, "c7a.2xlarge") {
			t.Fatalf("tried the type above max_price: %v", fm.callsOf("RunInstances"))
		}
	}
	if markets := fm.callsOf("market"); len(markets) != 2 || markets[1] != "r-one-1 market on-demand" {
		t.Fatalf("markets %v", markets)
	}
	// A type above the cap in any zone of the region is above it
	// on-demand there, and so is one EC2 just refused at the cap however
	// cheap its cached price: neither falls back.
	fz := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.03"}, {"r-one-1b", "c7a.2xlarge", "0.10"}, {"r-one-1a", "c6a.2xlarge", "0.02"}}},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity", "r-one-1/c6a.2xlarge": "SpotMaxPriceTooLow"},
	}
	gz := newGlobalEnv(t, fz, "r-one-1", "r-two-1")
	gz.r.MaxPrice, gz.r.OnDemandFallback = capUSD, true
	if in, err := gz.Launch(context.Background(), t.Logf); err == nil || errors.As(err, &pe) || strings.Contains(err.Error(), "set on_demand_fallback") {
		t.Fatalf("over the cap in another zone, or refused at it: launched %+v, err %v", in, err)
	}
	if markets := fz.callsOf("market"); len(markets) != 2 || len(fz.callsOf("market on-demand")) != 0 {
		t.Fatalf("markets %v", markets)
	}
	// A refusal at the cap still cooling down from an earlier launch counts
	// too, even for an option this launch never reaches (here skipped for
	// quota once the cheaper type is refused for it).
	fq := &fakeRegionsEC2{
		prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.02"}, {"r-one-1a", "c6a.2xlarge", "0.03"}}},
		runErr: map[string]string{"r-one-1/c7a.2xlarge": "VcpuLimitExceeded", "r-one-1/c7a.2xlarge/on-demand": "InsufficientInstanceCapacity"},
	}
	gq := newGlobalEnv(t, fq, "r-one-1", "r-two-1")
	gq.r.MaxPrice, gq.r.OnDemandFallback = capUSD, true
	gq.coolPool(SpotPrice{Region: "r-one-1", Zone: "r-one-1a", Type: "c6a.2xlarge"}, "SpotMaxPriceTooLow")
	if in, err := gq.Launch(context.Background(), t.Logf); err == nil {
		t.Fatalf("launched a type refused at the cap: %+v via %v", in, fq.callsOf("RunInstances"))
	}
	for _, run := range fq.callsOf("RunInstances") {
		if strings.Contains(run, "c6a") {
			t.Fatalf("tried the type refused at the cap: %v", fq.callsOf("RunInstances"))
		}
	}
	// Per region: the same type over the cap in one region and at it in
	// another falls back only where it's at the cap.
	fr := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.10"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.04"}},
		},
		runErr: map[string]string{"r-two-1/c7a.2xlarge": "InsufficientInstanceCapacity"},
	}
	gr := newGlobalEnv(t, fr, "r-one-1", "r-two-1")
	gr.r.MaxPrice, gr.r.OnDemandFallback = capUSD, true
	in, err := gr.Launch(context.Background(), t.Logf)
	if err != nil || in.Region != "r-two-1" {
		t.Fatalf("at the cap in r-two-1: launched %+v, err %v", in, err)
	}
	if markets := fr.callsOf("market"); strings.Join(markets, ",") != "r-two-1 market spot,r-two-1 market on-demand" {
		t.Fatalf("markets %v", markets)
	}
	// A vcpus runner: types without a Spot price have no known price, so
	// a capped fallback leaves them out (uncapped, they're tried last).
	newVCPUs := func(capped bool) (*AWSGlobal, *fakeRegionsEC2) {
		fv := &fakeRegionsEC2{
			prices: map[string][][3]string{"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.03"}}},
			types:  map[string][]string{"r-one-1": {typeXML("c7a.2xlarge", 8, 16, "x86_64", ""), typeXML("m7a.2xlarge", 8, 32, "x86_64", "")}},
			runErr: map[string]string{"r-one-1/c7a.2xlarge": "InsufficientInstanceCapacity", "r-one-1/c7a.2xlarge/on-demand": "InsufficientInstanceCapacity"},
		}
		gv := newGlobalEnv(t, fv, "r-one-1")
		gv.r.InstanceTypes, gv.r.VCPUs, gv.r.OnDemandFallback = nil, 8, true
		if capped {
			gv.r.MaxPrice = capUSD
		}
		return gv, fv
	}
	gv, fv := newVCPUs(true)
	if in, err := gv.Launch(context.Background(), t.Logf); err == nil {
		t.Fatalf("capped vcpus runner launched an unpriced type: %+v", in)
	}
	if runs := fv.callsOf("RunInstances"); len(runs) != 2 || strings.Contains(strings.Join(runs, ","), "m7a") {
		t.Fatalf("capped vcpus runner: runs %v", runs)
	}
	gv, fv = newVCPUs(false)
	if in, err := gv.Launch(context.Background(), t.Logf); err != nil || in.Type != "m7a.2xlarge" {
		t.Fatalf("uncapped vcpus runner: launched %+v, err %v via %v", in, err, fv.callsOf("RunInstances"))
	}
}

// provider.New picks the coordinator for several regions, and keeps it for
// a runner moved back to one region while VMs may remain in the others.
func TestNewPicksRegionalOrGlobal(t *testing.T) {
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	base := config.Runner{Name: "g", Type: config.TypeAWSSpot, Region: "r-one-1", InstanceTypes: []string{"c7a.2xlarge", "c6a.2xlarge"}, Arch: "amd64"}
	single := base
	if p, err := New(&single); err != nil || fmt.Sprintf("%T", p) != "*provider.AWSSpot" {
		t.Fatalf("single region: %T %v", p, err)
	}
	multi := base
	multi.Regions = []string{"r-one-1", "r-two-1"}
	p, err := New(&multi)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := p.(*AWSGlobal); !ok || g.legacy {
		t.Fatalf("several regions: %T", p)
	}
	// It launched in r-two-1, then the config went back to r-one-1 only.
	g := p.(*AWSGlobal)
	if err := g.markActive("r-two-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, err = New(&single)
	if err != nil {
		t.Fatal(err)
	}
	lg, ok := p.(*AWSGlobal)
	if !ok || !lg.legacy {
		t.Fatalf("back to one region with VMs elsewhere: %T", p)
	}
	regions, _ := lg.managedRegions(context.Background())
	if strings.Join(regions, ",") != "r-one-1,r-two-1" {
		t.Fatalf("managed regions %v", regions)
	}
	if pr, _ := lg.PlacementRegions(context.Background()); strings.Join(pr, ",") != "r-one-1" {
		t.Fatalf("placement regions %v", pr)
	}
}

// A single-region runner still tidying up old regions launches as a
// single-region runner does: only in its own region, cheapest first, and
// it still records where it launched.
func TestLegacyLaunchKeepsSingleRegionBehaviour(t *testing.T) {
	f := &fakeRegionsEC2{
		prices: map[string][][3]string{
			"r-one-1": {{"r-one-1a", "c7a.2xlarge", "0.06"}, {"r-one-1b", "c6a.2xlarge", "0.05"}, {"r-one-1c", "c7a.2xlarge", "0.07"}},
			"r-two-1": {{"r-two-1a", "c7a.2xlarge", "0.01"}},
		}}
	g := newGlobalEnv(t, f, "r-one-1")
	g.legacy = true
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	runs := f.callsOf("RunInstances")
	if in.Region != "r-one-1" || strings.Join(runs, "|") != "r-one-1 RunInstances c6a.2xlarge subnet-r-one-1b" {
		t.Fatalf("launched %+v via %v", in, runs)
	}
	for _, c := range f.callsOf("DescribeSpotPriceHistory") {
		if !strings.HasPrefix(c, "r-one-1 ") {
			t.Fatalf("priced another region: %s", c)
		}
	}
	if got := active(t, g); got != "r-one-1" {
		t.Fatalf("active regions %s", got)
	}
}

// Plain single-region use launches without recording anything in
// placement.json, so going back to several regions must rediscover.
func TestSingleRegionUseForgetsDiscovery(t *testing.T) {
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	r := config.Runner{Name: "g", Type: config.TypeAWSSpot, Region: "r-one-1", Regions: []string{"r-one-1", "r-two-1"}, InstanceTypes: []string{"c7a.2xlarge"}, Arch: "amd64"}
	p, _ := New(&r)
	g := p.(*AWSGlobal)
	if err := g.withState(func(st *placementState) { st.Discovered = true }); err != nil {
		t.Fatal(err)
	}
	single := r
	single.Regions = nil
	if p, _ := New(&single); fmt.Sprintf("%T", p) != "*provider.AWSSpot" {
		t.Fatalf("got %T", p)
	}
	var d bool
	_ = g.withState(func(st *placementState) { d = st.Discovered })
	if d {
		t.Fatal("single-region use must reset discovery")
	}
}

// An expired region is kept while it still holds something that bills
// even though it isn't schedulable: a stopped VM, or a bake builder.
func TestGlobalExpiredRegionKeptForStoppedAndBuilders(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-stopped": "c7a.2xlarge"}},
		state: map[string]string{"i-stopped": "stopped"}, builders: map[string]bool{"r-three-1": true}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	discovered(t, g)
	g.markActive("r-two-1", time.Now().Add(-time.Minute))
	g.markActive("r-three-1", time.Now().Add(-time.Minute))
	ins, err := g.List(context.Background())
	if err != nil || len(ins) != 0 {
		t.Fatalf("list = %v, %v (stopped VMs aren't schedulable)", ins, err)
	}
	if got := active(t, g); got != "r-three-1,r-two-1" {
		t.Fatalf("regions with billable leftovers were forgotten: %q", got)
	}
}

// Bakes of one runner in different regions run side by side; a second
// bake in the same region is refused, and a sweep skips only the regions
// with a bake in progress.
func TestBakeLockPerRegion(t *testing.T) {
	f := &fakeRegionsEC2{builders: map[string]bool{"r-one-1": true, "r-two-1": true}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	unlockOne, err := g.Regional("r-one-1").LockBake()
	if err != nil {
		t.Fatal(err)
	}
	unlockTwo, err := g.Regional("r-two-1").LockBake()
	if err != nil {
		t.Fatalf("a bake in another region was refused: %v", err)
	}
	if _, err := g.Regional("r-one-1").LockBake(); err == nil || !strings.Contains(err.Error(), "r-one-1") {
		t.Fatalf("a second bake in the same region: %v", err)
	}
	// An older spotexec (one lock per runner) still running, e.g. its pool
	// manager, can't sweep while a bake runs, nor bake alongside.
	legacy := filepath.Join(os.Getenv("SPOTEXEC_STATE_DIR"), "bake", "g.lock")
	if unlock, err := flockFile(legacy, syscall.LOCK_EX); err == nil {
		unlock()
		t.Fatal("an older version's runner-wide lock was free during a bake")
	}
	unlockTwo()

	g.sweepBuilders(context.Background(), []string{"r-one-1", "r-two-1"}, t.Logf)
	if got := f.callsOf("TerminateInstances"); len(got) != 1 || got[0] != "r-two-1 TerminateInstances i-builder" {
		t.Fatalf("sweep terminated %v; want only r-two-1's builder", got)
	}
	unlockOne()
	unlockOld, err := flockFile(legacy, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockOld()
	if _, err := g.Regional("r-two-1").LockBake(); err == nil {
		t.Fatal("bake started while an older version's bake holds the runner-wide lock")
	}
}

// Discovery records a region that holds only a stopped VM.
func TestGlobalDiscoveryFindsStoppedOnly(t *testing.T) {
	f := &fakeRegionsEC2{instances: map[string]map[string]string{"r-two-1": {"i-stopped": "c7a.2xlarge"}},
		state: map[string]string{"i-stopped": "stopped"}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1")
	if _, err := g.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := active(t, g); got != "r-two-1" {
		t.Fatalf("active %q", got)
	}
}

// A region found broken (no network) stays excluded from on-demand
// fallback on later launches, when its Spot options are skipped by
// cooldown before they are tried.
func TestGlobalOnDemandSkipsRegionsCoolingForSetup(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.05"}},
		"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.06"}},
		"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.07"}},
	}, runErr: map[string]string{"r-three-1/c7a.2xlarge": "InsufficientInstanceCapacity"}}
	g := newGlobalEnv(t, f, "r-one-1", "r-two-1", "r-three-1")
	g.r.OnDemandFallback = true
	g.coolRegion("r-one-1", "no default VPC", true)
	g.coolRegion("r-two-1", "no default VPC", true)
	in, err := g.Launch(context.Background(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if markets := f.callsOf("market"); in.Region != "r-three-1" || markets[len(markets)-1] != "r-three-1 market on-demand" {
		t.Fatalf("launched %+v via %v", in, markets)
	}
}

// LaunchRegions are the first few regions in launch order, those doctor
// and setup check the Spot quota of, never an excluded region.
func TestGlobalLaunchRegions(t *testing.T) {
	f := &fakeRegionsEC2{prices: map[string][][3]string{
		"r-one-1":   {{"r-one-1a", "c7a.2xlarge", "0.09"}},
		"r-two-1":   {{"r-two-1a", "c7a.2xlarge", "0.01"}},
		"r-three-1": {{"r-three-1a", "c7a.2xlarge", "0.06"}, {"r-three-1b", "c7a.2xlarge", "0.03"}},
		"r-off-1":   {{"r-off-1a", "c7a.2xlarge", "0.001"}}, // cheapest, but not enabled
	}}
	g := newGlobalEnv(t, f, config.AllRegions)
	g.r.LaunchOrder = config.LaunchCheapest
	g.r.ExcludeRegions = []string{"r-two-1"}
	got, err := g.LaunchRegions(context.Background())
	if err != nil || strings.Join(got, ",") != "r-three-1,r-one-1" {
		t.Fatalf("launch regions %v, %v", got, err)
	}
	if w := RunnerWhere(g.r); w != "all enabled regions except r-two-1" {
		t.Fatalf("where %q", w)
	}
	// A listed runner: its enabled regions, cheapest first.
	g.r.Regions, g.r.ExcludeRegions = []string{"r-one-1", "r-off-1", "r-two-1", "r-three-1"}, nil
	got, err = g.LaunchRegions(context.Background())
	if err != nil || strings.Join(got, ",") != "r-two-1,r-three-1,r-one-1" {
		t.Fatalf("launch regions %v, %v", got, err)
	}
	if w := RunnerWhere(g.r); w != "r-one-1, r-off-1, r-two-1, r-three-1" {
		t.Fatalf("where %q", w)
	}
	if w := RunnerWhere(&config.Runner{Type: config.TypeAWSSpot, Region: "r-one-1"}); w != "r-one-1" {
		t.Fatalf("where %q", w)
	}
}

// Setup run from another machine ranks with only the session it passed,
// never this machine's own AWS login.
func TestLaunchRegionsWithEnvOnly(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	r := &config.Runner{Name: "g", Type: config.TypeAWSSpot, Regions: []string{config.AllRegions}, Region: "us-east-1",
		InstanceTypes: []string{"c7a.2xlarge"}, Arch: "amd64", User: "ubuntu", VolumeGB: 64, SSHCIDR: "0.0.0.0/0", MaxInstances: 1,
		MaxLifetime: config.Duration{Duration: time.Hour}, BootTimeout: config.Duration{Duration: time.Minute}, IdleTimeout: config.Duration{Duration: time.Minute}}
	if _, err := LaunchRegionsWith(context.Background(), r, "", true); !errors.Is(err, errNoPassedSession) {
		t.Fatalf("err %v", err)
	}
}

// firstRegions keeps the first three distinct regions in launch order (not
// price order).
func TestFirstRegions(t *testing.T) {
	k := Ranking{Prices: []SpotPrice{
		{Region: "r-b", Price: "0.09"}, {Region: "r-a", Price: "0.01"}, {Region: "r-b", Price: "0.02"},
		{Region: "r-d", Price: "0.05"}, {Region: "r-c", Price: "0.03"},
	}}
	if got := strings.Join(firstRegions(k), ","); got != "r-b,r-a,r-d" {
		t.Fatalf("first regions %s", got)
	}
	if got := firstRegions(Ranking{}); len(got) != 0 {
		t.Fatalf("first regions %v", got)
	}
}

// Setup ranks with the admin session as the runner would launch: its own
// profile's warm regions count, and nothing the other session reads is
// cached in the runner's placement state.
func TestLaunchRegionsAsKeepsRunnerState(t *testing.T) {
	g, f := twoRegionEnv(t, "0.100", "0.096", func(r *config.Runner) { r.Profile = "spotexec" })
	warmUp(t, g, f, "r-one-1")
	snapshot := func() map[string]string {
		out := map[string]string{}
		ents, err := os.ReadDir(g.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			b, _ := os.ReadFile(filepath.Join(g.dir, e.Name()))
			out[e.Name()] = string(b)
		}
		return out
	}
	before := snapshot()
	admin := &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AKADMIN", SecretAccessKey: "secret"}}
	got, err := g.launchRegionsAs(context.Background(), admin)
	if err != nil || strings.Join(got, ",") != "r-one-1,r-two-1" {
		t.Fatalf("launch regions %v, %v (want the warm r-one-1 first)", got, err)
	}
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("ranking with the admin session changed the runner's state: %v", after)
	}
	// A vcpus runner discovers instance types per region: those caches
	// stay out of the runner's state too.
	g.r.InstanceTypes, g.r.VCPUs = nil, 8
	f.types = map[string][]string{"r-one-1": {typeXML("c7a.2xlarge", 8, 16, "x86_64", "")}, "r-two-1": {typeXML("c7a.2xlarge", 8, 16, "x86_64", "")}}
	before = snapshot()
	if _, err := g.launchRegionsAs(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("ranking a vcpus runner with the admin session changed the runner's state: %v", after)
	}
	if g.r.Profile != "spotexec" {
		t.Fatalf("profile %q", g.r.Profile)
	}
}
