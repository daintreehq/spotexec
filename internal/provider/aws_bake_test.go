package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

type fakeImage struct {
	id, name, state, arch string
	created               time.Time
	tags                  map[string]string
}

type fakeInstance struct {
	id, image, state string
	launched         time.Time
}

// fakeBakeAPI serves the EC2 image calls and the SSM base-image lookup.
type fakeBakeAPI struct {
	mu           sync.Mutex
	images       []*fakeImage
	instances    []fakeInstance
	base         string // what Canonical's SSM parameter resolves to
	deregistered []string
	gone         map[string]bool // images RunInstances reports as deleted
	runs         []url.Values    // RunInstances requests
}

func (f *fakeBakeAPI) image(id string) *fakeImage {
	for _, i := range f.images {
		if i.id == id {
			return i
		}
	}
	return nil
}

func (f *fakeBakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Amz-Target") == "AmazonSSM.GetParameter" {
			if f.base == "" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"__type":"ParameterNotFound","message":"not found"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"Parameter": map[string]string{"Value": f.base}})
			return
		}
		v, _ := url.ParseQuery(string(body))
		filters := map[string][]string{}
		for i := 1; v.Get(fmt.Sprintf("Filter.%d.Name", i)) != ""; i++ {
			name := v.Get(fmt.Sprintf("Filter.%d.Name", i))
			for j := 1; v.Get(fmt.Sprintf("Filter.%d.Value.%d", i, j)) != ""; j++ {
				filters[name] = append(filters[name], v.Get(fmt.Sprintf("Filter.%d.Value.%d", i, j)))
			}
		}
		in := func(s string, vals []string) bool {
			for _, x := range vals {
				if x == s {
					return true
				}
			}
			return false
		}
		switch action := v.Get("Action"); action {
		case "DescribeImages":
			var b strings.Builder
			b.WriteString("<DescribeImagesResponse><imagesSet>")
			for _, img := range f.images {
				if id := v.Get("ImageId.1"); id != "" && id != img.id {
					continue
				}
				ok := true
				for name, vals := range filters {
					var got string
					switch {
					case name == "state":
						got = img.state
					case name == "architecture":
						got = img.arch
					case name == "name":
						got = img.name
					case strings.HasPrefix(name, "tag:"):
						var has bool
						got, has = img.tags[strings.TrimPrefix(name, "tag:")]
						if !has {
							ok = false
							continue
						}
					default:
						t.Errorf("unexpected image filter %s", name)
					}
					ok = ok && in(got, vals)
				}
				if !ok {
					continue
				}
				fmt.Fprintf(&b, "<item><imageId>%s</imageId><name>%s</name><imageState>%s</imageState><creationDate>%s</creationDate><architecture>%s</architecture><rootDeviceName>/dev/sda1</rootDeviceName><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeSize>8</volumeSize></ebs></item></blockDeviceMapping><tagSet>",
					img.id, img.name, img.state, img.created.UTC().Format("2006-01-02T15:04:05.000Z"), img.arch)
				keys := make([]string, 0, len(img.tags))
				for k := range img.tags {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Fprintf(&b, "<item><key>%s</key><value>%s</value></item>", k, img.tags[k])
				}
				b.WriteString("</tagSet></item>")
			}
			b.WriteString("</imagesSet></DescribeImagesResponse>")
			fmt.Fprint(w, b.String())
		case "DescribeInstances":
			var b strings.Builder
			b.WriteString("<DescribeInstancesResponse><reservationSet><item><instancesSet>")
			for _, i := range f.instances {
				if id := v.Get("InstanceId.1"); id != "" && id != i.id {
					continue
				}
				if vals, ok := filters["image-id"]; ok && !in(i.image, vals) {
					continue
				}
				if vals, ok := filters["instance-state-name"]; ok && !in(i.state, vals) {
					continue
				}
				fmt.Fprintf(&b, "<item><instanceId>%s</instanceId><imageId>%s</imageId><instanceState><name>%s</name></instanceState><launchTime>%s</launchTime></item>",
					i.id, i.image, i.state, i.launched.UTC().Format(time.RFC3339))
			}
			b.WriteString("</instancesSet></item></reservationSet></DescribeInstancesResponse>")
			fmt.Fprint(w, b.String())
		case "RunInstances":
			f.runs = append(f.runs, v)
			if f.gone[v.Get("ImageId")] {
				w.WriteHeader(400)
				fmt.Fprintf(w, "<Response><Errors><Error><Code>InvalidAMIID.NotFound</Code><Message>The image id '[%s]' does not exist</Message></Error></Errors></Response>", v.Get("ImageId"))
				return
			}
			fmt.Fprintf(w, "<RunInstancesResponse><instancesSet><item><instanceId>i-new%d</instanceId><imageId>%s</imageId><instanceState><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>", len(f.runs), v.Get("ImageId"))
		case "DeregisterImage":
			id := v.Get("ImageId")
			f.deregistered = append(f.deregistered, id)
			for i, img := range f.images {
				if img.id == id {
					f.images = append(f.images[:i], f.images[i+1:]...)
					break
				}
			}
			fmt.Fprint(w, "<DeregisterImageResponse><return>true</return></DeregisterImageResponse>")
		default:
			t.Errorf("unexpected call %s", action)
			w.WriteHeader(400)
		}
	}
}

const testOwner = "owner-me"

func newBakeEnv(t *testing.T, f *fakeBakeAPI) *AWSSpot {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	r := &config.Runner{Name: "aws", Type: "aws-spot", Region: "us-east-2", Arch: "amd64", User: "ubuntu",
		InstanceTypes: []string{"c7a.xlarge"}, BootstrapScript: "apt-get install -y make"}
	r.BootTimeout.Duration = 15 * time.Minute
	return &AWSSpot{r: r, dir: t.TempDir(), owner: testOwner, api: &awsClient{
		region: r.Region, creds: &awsCredResolver{fixed: &awsCreds{AccessKeyID: "AK", SecretAccessKey: "s"}},
		http: srv.Client(), endpoints: map[string]string{"ec2": srv.URL, "ssm": srv.URL},
	}}
}

// bakeImg is a bake of runner "aws" made by owner, of bootstrap hash, from base.
func bakeImg(id, owner, hash, base string, created time.Time) *fakeImage {
	tags := map[string]string{tagRunner: "aws", tagOwner: owner, tagBakeHash: hash}
	if base != "" {
		tags[tagBakeBase] = base
	}
	return &fakeImage{id: id, name: "spotexec-aws-" + hash + "-" + id, state: "available", arch: "x86_64", created: created, tags: tags}
}

func ubuntuImg(id string, created time.Time) *fakeImage {
	return &fakeImage{id: id, name: "ubuntu-noble-" + id, state: "available", arch: "x86_64", created: created, tags: map[string]string{}}
}

func TestJudgeBake(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name                 string
		age                  time.Duration
		base, cur            string
		sinceAgo             time.Duration
		wantStale, wantExpir bool
	}{
		{"fresh, same base", 3 * day, "ami-b1", "ami-b1", 0, false, false},
		{"newer base just out: stale, still used", 10 * day, "ami-b1", "ami-b2", 2 * day, true, false},
		{"newer base out past the grace: expired", 12 * day, "ami-b1", "ami-b2", 8 * day, true, true},
		{"past max age: expired", 31 * day, "ami-b1", "ami-b1", 0, true, true},
		{"base unknown (SSM failed): age only", 10 * day, "ami-b1", "", 0, false, false},
		{"no recorded base: stale for a week", 10 * day, "", "ami-b2", 3 * day, true, false},
	}
	for _, c := range cases {
		b := Bake{ID: "ami-bake", Created: now.Add(-c.age), Base: c.base}
		judgeBake(&b, c.cur, now.Add(-c.sinceAgo), now)
		if (b.Stale != "") != c.wantStale || b.Expired != c.wantExpir {
			t.Errorf("%s: stale=%q expired=%v", c.name, b.Stale, b.Expired)
		}
	}
}

func TestSupersededSinceIsAnchored(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	created := now.Add(-20 * day)
	// First seen superseded by a base published ten days ago.
	if got := supersededSince("ami-x", created, now.Add(-10*day), now); !got.Equal(now.Add(-10 * day)) {
		t.Fatalf("first anchor = %v", got)
	}
	// Canonical publishes yet another base today: the grace doesn't restart.
	if got := supersededSince("ami-x", created, now, now.Add(time.Hour)); !got.Equal(now.Add(-10 * day)) {
		t.Fatalf("a later release moved the anchor to %v", got)
	}
	b := Bake{ID: "ami-x", Created: created, Base: "ami-b1"}
	judgeBake(&b, "ami-b3", supersededSince("ami-x", created, now, now), now)
	if !b.Expired {
		t.Fatalf("a later release reactivated an expired bake: %+v", b)
	}
	// Never before the bake itself, nor in the future.
	if got := supersededSince("ami-y", created, created.Add(-5*day), now); !got.Equal(created) {
		t.Fatalf("anchor before the bake: %v", got)
	}
	if got := supersededSince("ami-z", created, time.Time{}, now); !got.Equal(now) {
		t.Fatalf("unknown publication date should anchor at now: %v", got)
	}
	// A damaged anchor (a crash mid-write) is replaced once, then kept.
	p := filepath.Join(os.Getenv("SPOTEXEC_STATE_DIR"), "bake", "superseded-ami-d")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := supersededSince("ami-d", created, time.Time{}, now); !got.Equal(now) {
		t.Fatalf("damaged anchor: %v", got)
	}
	if got := supersededSince("ami-d", created, time.Time{}, now.Add(48*time.Hour)); !got.Equal(now) {
		t.Fatalf("the grace restarted after a damaged anchor was replaced: %v", got)
	}
}

func TestFindBakeStopsUsingStaleBake(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &fakeBakeAPI{base: "ami-b2"}
	a := newBakeEnv(t, f)
	h := a.BakeHash()
	f.images = []*fakeImage{ubuntuImg("ami-b1", now.Add(-40*24*time.Hour)), ubuntuImg("ami-b2", now.Add(-2*24*time.Hour)),
		bakeImg("ami-bake", testOwner, h, "ami-b1", now.Add(-10*24*time.Hour))}

	// A newer base has been out two days: used, but reported stale.
	b, err := a.findBake(context.Background())
	if err != nil || b == nil || !b.InUse || b.Expired || !strings.Contains(b.Stale, "ami-b2") {
		t.Fatalf("want an in-use stale bake, got %+v %v", b, err)
	}
	if c := a.bakeCheck(context.Background()); !strings.Contains(c.Msg, "base ami-b1") || !strings.Contains(c.Msg, "10d old") || !strings.Contains(c.Msg, "image bake aws") {
		t.Fatalf("doctor should show base, age and the prompt: %q", c.Msg)
	}

	// Eight days on, launches stop using it; doctor still shows base and age.
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	f.image("ami-b2").created = now.Add(-8 * 24 * time.Hour)
	b, _ = a.findBake(context.Background())
	if b == nil || b.InUse || !b.Expired {
		t.Fatalf("want an expired bake, got %+v", b)
	}
	if c := a.bakeCheck(context.Background()); !strings.Contains(c.Msg, "not used") || !strings.Contains(c.Msg, "base ami-b1") || !strings.Contains(c.Msg, "10d old") {
		t.Fatalf("doctor should say the bake is not used, with its base and age: %q", c.Msg)
	}

	// A fresh bake of the current base is used without complaint.
	f.images = append(f.images, bakeImg("ami-new", testOwner, h, "ami-b2", now.Add(-time.Hour)))
	b, _ = a.findBake(context.Background())
	if b == nil || b.ID != "ami-new" || !b.InUse || b.Stale != "" {
		t.Fatalf("want the fresh bake in use, got %+v", b)
	}
}

func TestFindBakeBaseLookupFails(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &fakeBakeAPI{} // no base: the SSM lookup fails
	a := newBakeEnv(t, f)
	f.images = []*fakeImage{bakeImg("ami-bake", testOwner, a.BakeHash(), "ami-b1", now.Add(-3*24*time.Hour))}
	b, err := a.findBake(context.Background())
	if err != nil || b == nil || !b.InUse || b.CheckErr == "" {
		t.Fatalf("a failed base lookup should still use the bake and say so: %+v %v", b, err)
	}
	if c := a.bakeCheck(context.Background()); !strings.Contains(c.Msg, "couldn't look up") {
		t.Fatalf("doctor should report the failed lookup: %q", c.Msg)
	}
	// ...but the age limit still applies.
	f.image("ami-bake").created = now.Add(-31 * 24 * time.Hour)
	if b, _ := a.findBake(context.Background()); b == nil || !b.Expired {
		t.Fatalf("age limit not enforced without a base lookup: %+v", b)
	}
}

func TestBakesListBaseAndAge(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &fakeBakeAPI{base: "ami-b2"}
	a := newBakeEnv(t, f)
	h := a.BakeHash()
	f.images = []*fakeImage{ubuntuImg("ami-b2", now.Add(-24*time.Hour)),
		bakeImg("ami-new", testOwner, h, "ami-b2", now.Add(-time.Hour)),
		bakeImg("ami-old", testOwner, h, "ami-b1", now.Add(-35*24*time.Hour))}
	bakes, err := a.Bakes(context.Background())
	if err != nil || len(bakes) != 2 {
		t.Fatalf("Bakes: %+v %v", bakes, err)
	}
	if bakes[0].ID != "ami-new" || !bakes[0].InUse || bakes[0].Base != "ami-b2" {
		t.Fatalf("newest bake: %+v", bakes[0])
	}
	if bakes[1].InUse || !bakes[1].Expired || bakes[1].Base != "ami-b1" {
		t.Fatalf("old bake: %+v", bakes[1])
	}
	// Another machine's newer bake of the same bootstrap is listed, with
	// its maker, and is what launches pick.
	theirs := bakeImg("ami-theirs", "owner-other", h, "ami-b2", now.Add(-time.Minute))
	theirs.tags[tagHost] = "mac-a"
	f.images = append(f.images, theirs)
	bakes, _ = a.Bakes(context.Background())
	if len(bakes) != 3 || bakes[0].ID != "ami-theirs" || !bakes[0].InUse || bakes[0].Mine || bakes[0].Maker() != "mac-a" {
		t.Fatalf("another machine's bake not listed as the one in use: %+v", bakes)
	}
	for _, b := range bakes[1:] {
		if b.InUse || !b.Mine || b.Maker() != "this machine" {
			t.Fatalf("%+v", b)
		}
	}
}

// sharedBakes is a runner's bakes from two machines (made "ago" before now).
func sharedBakes(now time.Time, h string) []*fakeImage {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	theirs := func(id, hash string, created time.Time) *fakeImage {
		i := bakeImg(id, "owner-other", hash, "ami-b2", created)
		i.tags[tagHost] = "mac-a"
		return i
	}
	// Their bake of another bootstrap on a base superseded 8 days ago:
	// expired, though only 12 days old.
	old := bakeImg("ami-v0", "owner-other", "h-v", "ami-b1", ago(12*24*time.Hour))
	return []*fakeImage{
		ubuntuImg("ami-b2", ago(8*24*time.Hour)),
		bakeImg("ami-cur0", testOwner, h, "ami-b2", ago(10*time.Minute)),
		theirs("ami-cur1", h, ago(3*time.Hour)),
		theirs("ami-cur2", h, ago(5*time.Hour)),                       // superseded 3h ago: prunable
		bakeImg("ami-cur3", testOwner, h, "ami-b2", ago(6*time.Hour)), // VM booting from it
		theirs("ami-x0", "h-x", ago(10*time.Minute)),
		theirs("ami-x1", "h-x", ago(20*time.Minute)),
		theirs("ami-x2", "h-x", ago(48*time.Hour)),    // superseded only 20m ago: kept
		theirs("ami-y0", "h-y", ago(48*time.Hour)),    // their current bake of another bootstrap
		theirs("ami-z0", "h-z", ago(40*24*time.Hour)), // expired: nobody boots it
		theirs("ami-w0", "h-w", ago(10*time.Minute)),
		theirs("ami-w1", "h-w", ago(75*time.Minute)), // created 75m ago, maybe available only lately
		theirs("ami-w2", "h-w", ago(72*time.Hour)),   // so not yet prunable
		old,
	}
}

// remaining lists the bakes not deregistered.
func remaining(f *fakeBakeAPI) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, i := range f.images {
		if i.tags[tagRunner] != "" {
			out[i.id] = true
		}
	}
	return out
}

func TestPruneBakesManagesSharedBakes(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &fakeBakeAPI{base: "ami-b2", instances: []fakeInstance{{id: "i-boot", image: "ami-cur3", state: "pending", launched: now}}}
	a := newBakeEnv(t, f)
	f.images = sharedBakes(now, a.BakeHash())
	a.pruneBakes(context.Background(), 2, t.Logf)
	left := remaining(f)
	for _, id := range []string{"ami-cur0", "ami-cur1", "ami-cur3", "ami-x0", "ami-x1", "ami-x2", "ami-y0", "ami-w0", "ami-w1", "ami-w2", "ami-v0"} {
		if !left[id] {
			t.Errorf("%s was pruned", id)
		}
	}
	for _, id := range []string{"ami-cur2", "ami-z0"} {
		if left[id] {
			t.Errorf("%s was not pruned", id)
		}
	}
}

func TestClearBakesSharedScope(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &fakeBakeAPI{base: "ami-b2", instances: []fakeInstance{{id: "i-boot", image: "ami-cur3", state: "pending", launched: now}}}
	a := newBakeEnv(t, f)
	f.images = sharedBakes(now, a.BakeHash())
	baking := bakeImg("ami-baking", "owner-other", a.BakeHash(), "ami-b2", now)
	baking.state = "pending"
	f.images = append(f.images, baking)

	res, err := a.ClearBakes(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	left := remaining(f)
	// This runner's bootstrap (whoever made them) and expired bakes go...
	for _, id := range []string{"ami-cur0", "ami-cur1", "ami-cur2", "ami-z0", "ami-v0"} {
		if left[id] {
			t.Errorf("%s was not cleared", id)
		}
	}
	// ...other machines' bakes of other bootstraps, a bake being booted and
	// one still being made stay.
	for _, id := range []string{"ami-cur3", "ami-x0", "ami-x1", "ami-x2", "ami-y0", "ami-w0", "ami-w1", "ami-w2", "ami-baking"} {
		if !left[id] {
			t.Errorf("%s was cleared", id)
		}
	}
	if res.Removed != 5 || len(res.Kept) != 9 {
		t.Fatalf("result %+v", res)
	}
	kept := strings.Join(res.Kept, "\n")
	for _, want := range []string{"i-boot is booting from it", "another machine is still baking it", "made by mac-a for another bootstrap (`spotexec image clear aws --all`"} {
		if !strings.Contains(kept, want) {
			t.Errorf("kept list lacks %q:\n%s", want, kept)
		}
	}

	// --all removes other machines' bakes too, still sparing the busy ones.
	if _, err := a.ClearBakes(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	left = remaining(f)
	if len(left) != 2 || !left["ami-cur3"] || !left["ami-baking"] {
		t.Fatalf("after --all: %v", left)
	}
}

func TestBakeScriptAppliesSecurityUpdates(t *testing.T) {
	a := newBakeEnv(t, &fakeBakeAPI{})
	s := a.bakeRootScript(nil)
	// Installed if the bootstrap removed it, then run unguarded, so a
	// failure fails the bake rather than baking unpatched packages.
	if !strings.Contains(s, "apt-get update --error-on=any") || !strings.Contains(s, "|| apt-get install -y -qq -o DPkg::Lock::Timeout=300 unattended-upgrades\n") || !strings.Contains(s, "\nunattended-upgrade\n") {
		t.Fatalf("bake does not apply security updates:\n%s", s)
	}
}

func TestBuilderBaseRemembersLaunch(t *testing.T) {
	a := newBakeEnv(t, &fakeBakeAPI{}) // DescribeInstances knows no builder
	builderBases.Store("i-launched", "ami-b9")
	t.Cleanup(func() { builderBases.Delete("i-launched") })
	if got := a.builderBase(context.Background(), "i-launched"); got != "ami-b9" {
		t.Fatalf("builder base = %q", got)
	}
}

func TestBuilderBase(t *testing.T) {
	f := &fakeBakeAPI{instances: []fakeInstance{{id: "i-builder", image: "ami-b1", state: "stopped", launched: time.Now()}}}
	a := newBakeEnv(t, f)
	if got := a.builderBase(context.Background(), "i-builder"); got != "ami-b1" {
		t.Fatalf("builder base = %q", got)
	}
}

func TestLaunchFallsBackWhenBakeIsGone(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	now := time.Now()
	f := &fakeBakeAPI{base: "ami-b2"}
	a := newBakeEnv(t, f)
	a.r.SecurityGroupIDs = []string{"sg-1"}
	f.images = []*fakeImage{ubuntuImg("ami-b2", now.Add(-48*time.Hour)), bakeImg("ami-bake", "owner-other", a.BakeHash(), "ami-b2", now.Add(-time.Hour))}
	// Another machine's `image clear` deregisters it after it was picked.
	f.gone = map[string]bool{"ami-bake": true}
	inst, err := a.launchOrUnbaked(context.Background(), t.Logf, launchSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if len(f.runs) != 2 || f.runs[0].Get("ImageId") != "ami-bake" || f.runs[1].Get("ImageId") != "ami-b2" || inst.ImageID != "ami-b2" {
		t.Fatalf("want a retry from the base image, got %d requests (%+v)", len(f.runs), inst)
	}
	ud, _ := base64.StdEncoding.DecodeString(f.runs[1].Get("UserData"))
	if !strings.Contains(string(ud), "apt-get install -y make") {
		t.Fatal("the fallback launch skipped the bootstrap")
	}
}

func TestLaunchPinnedAMINoFallback(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	t.Setenv("SPOTEXEC_CONFIG_DIR", t.TempDir())
	f := &fakeBakeAPI{base: "ami-b2"}
	a := newBakeEnv(t, f)
	a.r.SecurityGroupIDs = []string{"sg-1"}
	a.r.AMI = "ami-pinned"
	f.images = []*fakeImage{ubuntuImg("ami-pinned", time.Now())}
	f.gone = map[string]bool{"ami-pinned": true}
	if _, err := a.launchOrUnbaked(context.Background(), t.Logf, launchSpec{}); err == nil || len(f.runs) != 1 {
		t.Fatalf("a pinned AMI that is gone must fail, not fall back: %v (%d requests)", err, len(f.runs))
	}
}
