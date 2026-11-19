package provider

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
)

type teardownImage struct {
	id   string
	tags map[string]string
	snap string
}

// fakeObjVersion is a noncurrent object version or a delete marker.
type fakeObjVersion struct {
	key, id string
	marker  bool // a delete marker
	locked  bool // under Object Lock retention: DeleteObjects refuses it
	sticky  bool // DeleteObjects reports it deleted but it stays
}

type fakeSG struct {
	id, name, desc string
	tags           map[string]string
}

// fakeCloud adds what teardown touches beyond instances and IAM users.
type fakeCloud struct {
	*fakeAccount
	images    map[string][]teardownImage
	snapshots map[string]map[string]map[string]string // region -> snapshot -> tags
	sgs       map[string][]fakeSG
	repos     map[string][]string         // region -> repository names
	buckets   map[string]map[string]bool  // bucket -> current objects (version "null")
	versions  map[string][]fakeObjVersion // bucket -> noncurrent versions and delete markers
	versioned map[string]bool             // deleting a key without a version leaves a delete marker
	failRepo  map[string]bool             // DeleteRepository fails
	busySG    map[string]int              // DependencyViolation this many times
	deleted   []string                    // "kind id", in order
	s3ops     []string                    // "list <bucket> <entries>", "delete <bucket> <n>", "drop <bucket>"
	s3owner   []string                    // x-amz-expected-bucket-owner seen
	listedIn  []string                    // the region each ListBuckets call was signed for
}

func (c *fakeCloud) serve(w http.ResponseWriter, r *http.Request, key, region, service string, body []byte) bool {
	fail := func(status int, code string) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors></Response>`, code, code)
	}
	switch service {
	case "s3":
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		bucket := parts[0]
		if bucket != "" {
			c.s3owner = append(c.s3owner, r.Header.Get("x-amz-expected-bucket-owner"))
		}
		switch {
		case bucket == "" && r.Method == http.MethodGet:
			c.listedIn = append(c.listedIn, region)
			// Two buckets per page, to exercise paging.
			names := make([]string, 0, len(c.buckets))
			for b := range c.buckets {
				names = append(names, b)
			}
			sort.Strings(names)
			start := 0
			fmt.Sscanf(r.URL.Query().Get("continuation-token"), "from-%d", &start)
			fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets>`)
			for i := start; i < len(names) && i < start+2; i++ {
				fmt.Fprintf(w, `<Bucket><Name>%s</Name></Bucket>`, names[i])
			}
			fmt.Fprint(w, `</Buckets>`)
			if start+2 < len(names) {
				fmt.Fprintf(w, `<ContinuationToken>from-%d</ContinuationToken>`, start+2)
			}
			fmt.Fprint(w, `</ListAllMyBucketsResult>`)
		case r.Method == http.MethodGet && r.URL.Query().Has("versions"):
			// Every version and delete marker, two per page by
			// key-marker/version-id-marker, to exercise paging.
			all := c.versions[bucket][:len(c.versions[bucket]):len(c.versions[bucket])]
			for k := range c.buckets[bucket] {
				all = append(all, fakeObjVersion{key: k, id: "null"})
			}
			sort.Slice(all, func(i, j int) bool {
				return all[i].key < all[j].key || all[i].key == all[j].key && all[i].id < all[j].id
			})
			km, vm := r.URL.Query().Get("key-marker"), r.URL.Query().Get("version-id-marker")
			if km != "" {
				c.s3ops = append(c.s3ops, "list-from-marker "+bucket)
			}
			if vm != "" && km == "" {
				w.WriteHeader(400)
				fmt.Fprint(w, `<Error><Code>InvalidArgument</Code></Error>`)
				return true
			}
			var page []fakeObjVersion
			for _, v := range all {
				if km != "" && (v.key < km || v.key == km && (vm == "" || v.id <= vm)) {
					continue
				}
				page = append(page, v)
			}
			more := len(page) > 2
			if more {
				page = page[:2]
			}
			c.s3ops = append(c.s3ops, fmt.Sprintf("list %s %d", bucket, len(page)))
			fmt.Fprint(w, `<ListVersionsResult>`)
			fmt.Fprintf(w, `<IsTruncated>%v</IsTruncated>`, more)
			for _, v := range page {
				el := "Version"
				if v.marker {
					el = "DeleteMarker"
				}
				fmt.Fprintf(w, `<%s><Key>`, el)
				xml.EscapeText(w, []byte(v.key))
				fmt.Fprint(w, `</Key><VersionId>`)
				xml.EscapeText(w, []byte(v.id))
				fmt.Fprintf(w, `</VersionId></%s>`, el)
			}
			if more {
				last := page[len(page)-1]
				fmt.Fprint(w, `<NextKeyMarker>`)
				xml.EscapeText(w, []byte(last.key))
				fmt.Fprint(w, `</NextKeyMarker><NextVersionIdMarker>`)
				xml.EscapeText(w, []byte(last.id))
				fmt.Fprint(w, `</NextVersionIdMarker>`)
			}
			fmt.Fprint(w, `</ListVersionsResult>`)
		case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
			if sum := md5.Sum(body); r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
				w.WriteHeader(400)
				fmt.Fprint(w, `<Error><Code>BadDigest</Code></Error>`)
				return true
			}
			var d struct {
				Objects []struct {
					Key       string `xml:"Key"`
					VersionID string `xml:"VersionId"`
				} `xml:"Object"`
			}
			xml.Unmarshal(body, &d)
			c.s3ops = append(c.s3ops, fmt.Sprintf("delete %s %d", bucket, len(d.Objects)))
			fmt.Fprint(w, `<DeleteResult>`)
			for _, o := range d.Objects {
				switch o.VersionID {
				case "":
					// Without a version, a versioned bucket keeps the
					// data and adds a delete marker.
					if c.versioned[bucket] {
						if c.buckets[bucket][o.Key] {
							c.versions[bucket] = append(c.versions[bucket], fakeObjVersion{key: o.Key, id: "old-" + o.Key})
						}
						c.versions[bucket] = append(c.versions[bucket], fakeObjVersion{key: o.Key, id: fmt.Sprintf("dm-%d", len(c.versions[bucket])), marker: true})
					}
					delete(c.buckets[bucket], o.Key)
				case "null":
					delete(c.buckets[bucket], o.Key)
				default:
					vs := c.versions[bucket]
					for i, v := range vs {
						if v.key != o.Key || v.id != o.VersionID {
							continue
						}
						if v.locked {
							fmt.Fprint(w, `<Error><Key>`)
							xml.EscapeText(w, []byte(o.Key))
							fmt.Fprint(w, `</Key><VersionId>`)
							xml.EscapeText(w, []byte(o.VersionID))
							fmt.Fprint(w, `</VersionId><Code>AccessDenied</Code><Message>Access Denied because object protected by object lock.</Message></Error>`)
							break
						}
						if !v.sticky {
							c.versions[bucket] = append(vs[:i:i], vs[i+1:]...)
						}
						break
					}
				}
			}
			fmt.Fprint(w, `</DeleteResult>`)
		case r.Method == http.MethodDelete:
			c.s3ops = append(c.s3ops, "drop "+bucket)
			if len(c.buckets[bucket]) > 0 || len(c.versions[bucket]) > 0 {
				w.WriteHeader(409)
				fmt.Fprint(w, `<Error><Code>BucketNotEmpty</Code></Error>`)
				return true
			}
			delete(c.buckets, bucket)
			c.deleted = append(c.deleted, "bucket "+bucket)
			w.WriteHeader(204)
		default:
			w.WriteHeader(400)
		}
		return true
	case "ecr":
		var in map[string]any
		json.Unmarshal(body, &in)
		switch strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonEC2ContainerRegistry_V20150921.") {
		case "DescribeRepositories":
			var repos []map[string]string
			for _, n := range c.repos[region] {
				repos = append(repos, map[string]string{"repositoryName": n})
			}
			json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
		case "DeleteRepository":
			name := in["repositoryName"].(string)
			if c.failRepo[name] || in["force"] != true {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"__type":"AccessDeniedException","message":"no"}`)
				return true
			}
			for i, n := range c.repos[region] {
				if n == name {
					c.repos[region] = append(c.repos[region][:i:i], c.repos[region][i+1:]...)
					break
				}
			}
			c.deleted = append(c.deleted, "repository "+name)
			fmt.Fprint(w, `{}`)
		}
		return true
	}
	v, _ := url.ParseQuery(string(body))
	tagged := func(tags map[string]string) bool { return tags[tagOwner] != "" }
	switch v.Get("Action") {
	case "DescribeRegions":
		if strings.HasPrefix(region, "us-gov-") {
			fmt.Fprint(w, `<DescribeRegionsResponse><regionInfo><item><regionName>us-gov-east-1</regionName></item><item><regionName>us-gov-west-1</regionName></item></regionInfo></DescribeRegionsResponse>`)
			break
		}
		fmt.Fprint(w, `<DescribeRegionsResponse><regionInfo><item><regionName>us-west-2</regionName></item><item><regionName>us-east-2</regionName><optInStatus>opt-in-not-required</optInStatus></item><item><regionName>ap-east-1</regionName><optInStatus>not-opted-in</optInStatus></item></regionInfo></DescribeRegionsResponse>`)
	case "DescribeImages":
		if v.Get("Owner.1") != "self" || v.Get("Filter.1.Value.1") != tagOwner {
			fail(400, "BadFilter")
			return true
		}
		// One image per page, to exercise paging.
		var ids []string
		for _, im := range c.images[region] {
			if tagged(im.tags) && (im.tags["disabled"] == "" || v.Get("IncludeDisabled") == "true") {
				ids = append(ids, im.id)
			}
		}
		page, next := 0, ""
		fmt.Sscanf(v.Get("NextToken"), "page-%d", &page)
		fmt.Fprint(w, `<DescribeImagesResponse><imagesSet>`)
		if page < len(ids) {
			fmt.Fprintf(w, `<item><imageId>%s</imageId><name>bake</name></item>`, ids[page])
			if page+1 < len(ids) {
				next = fmt.Sprintf("page-%d", page+1)
			}
		}
		fmt.Fprintf(w, `</imagesSet><nextToken>%s</nextToken></DescribeImagesResponse>`, next)
	case "DescribeSnapshots":
		fmt.Fprint(w, `<DescribeSnapshotsResponse><snapshotSet>`)
		for id, tags := range c.snapshots[region] {
			if tagged(tags) {
				fmt.Fprintf(w, `<item><snapshotId>%s</snapshotId></item>`, id)
			}
		}
		fmt.Fprint(w, `</snapshotSet></DescribeSnapshotsResponse>`)
	case "DescribeSecurityGroups":
		fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>`)
		for _, g := range c.sgs[region] {
			fmt.Fprintf(w, `<item><groupId>%s</groupId><groupName>%s</groupName><groupDescription>%s</groupDescription><vpcId>vpc-1</vpcId><tagSet>`, g.id, g.name, g.desc)
			for k, val := range g.tags {
				fmt.Fprintf(w, `<item><key>%s</key><value>%s</value></item>`, k, val)
			}
			fmt.Fprint(w, `</tagSet></item>`)
		}
		fmt.Fprint(w, `</securityGroupInfo></DescribeSecurityGroupsResponse>`)
	case "DeregisterImage":
		for i, im := range c.images[region] {
			if im.id == v.Get("ImageId") {
				if v.Get("DeleteAssociatedSnapshots") == "true" {
					delete(c.snapshots[region], im.snap)
				}
				c.images[region] = append(c.images[region][:i:i], c.images[region][i+1:]...)
				c.deleted = append(c.deleted, "image "+im.id)
				fmt.Fprint(w, `<DeregisterImageResponse/>`)
				return true
			}
		}
		fail(400, "InvalidAMIID.NotFound")
	case "DeleteSnapshot":
		if _, ok := c.snapshots[region][v.Get("SnapshotId")]; !ok {
			fail(400, "InvalidSnapshot.NotFound")
			return true
		}
		delete(c.snapshots[region], v.Get("SnapshotId"))
		c.deleted = append(c.deleted, "snapshot "+v.Get("SnapshotId"))
		fmt.Fprint(w, `<DeleteSnapshotResponse/>`)
	case "DeleteSecurityGroup":
		id := v.Get("GroupId")
		for _, in := range c.instances[key][region] {
			if in.state != "terminated" && (in.tags[tagRunner] != "" || in.tags[tagBake] != "") {
				fail(400, "DependencyViolation") // still attached
				return true
			}
		}
		if c.busySG[id] > 0 {
			c.busySG[id]--
			fail(400, "DependencyViolation")
			return true
		}
		for i, g := range c.sgs[region] {
			if g.id == id {
				c.sgs[region] = append(c.sgs[region][:i:i], c.sgs[region][i+1:]...)
				break
			}
		}
		c.deleted = append(c.deleted, "sg "+id)
		fmt.Fprint(w, `<DeleteSecurityGroupResponse/>`)
	default:
		return false
	}
	return true
}

func newFakeCloud(t *testing.T) *fakeCloud {
	c := &fakeCloud{
		fakeAccount: &fakeAccount{
			accounts: map[string]string{"AK-admin": "111111111111"},
			instances: map[string]map[string][]fakeAcctInstance{"AK-admin": {
				"us-east-2": {
					{"i-run", "running", map[string]string{tagRunner: "aws", tagOwner: "m-1", tagHost: "mac"}},
					{"i-bake", "stopped", map[string]string{tagBake: "aws", tagOwner: "m-2"}},
					{"i-prod", "running", map[string]string{"Name": "prod-db"}},
				},
			}},
			users:     map[string]map[string]string{"spotexec-mac": {tagMachine: "m-1"}, "ops-bot": {}},
			userPaths: map[string]string{"ops-bot": "/spotexec/prod/"}, // not setup's path
			keys:      map[string][2]string{"AKMAC": {"spotexec-mac", "Active"}, "AKOPS": {"ops-bot", "Active"}},
			failTerm:  map[string]bool{},
			// Launched by a machine after the plan was made.
			late: map[string][]fakeAcctInstance{"us-east-2": {{"i-late", "pending", map[string]string{tagRunner: "aws", tagOwner: "m-1"}}}},
		},
		images: map[string][]teardownImage{"us-east-2": {
			{"ami-bake", map[string]string{tagOwner: "m-1"}, "snap-bake"},
			{"ami-bake2", map[string]string{tagOwner: "m-1"}, "snap-untagged"}, // its snapshot isn't tagged: kept
			{"ami-disabled", map[string]string{tagOwner: "m-2", "disabled": "yes"}, ""},
			{"ami-prod", map[string]string{}, "snap-prod"},
		}},
		snapshots: map[string]map[string]map[string]string{"us-east-2": {
			"snap-bake":     {tagOwner: "m-1"},
			"snap-orphan":   {tagOwner: "m-2"},
			"snap-prod":     {},
			"snap-untagged": {},
		}},
		sgs: map[string][]fakeSG{
			"us-east-2": {
				{"sg-new", "spotexec-ssh", "spotexec runners: SSH ingress", map[string]string{tagOwner: "m-1"}},
				{"sg-legacy", "spotexec-ssh-0a1b2c3d", "spotexec runners: SSH ingress", nil},
				{"sg-handmade", "spotexec-ssh-prod", "prod bastion", nil},
			},
			"us-west-2": {{"sg-west", "spotexec-ssh", "spotexec runners: SSH ingress", map[string]string{tagOwner: "m-2"}}},
		},
		repos: map[string][]string{"us-east-2": {"spotexec/app", "team/app"}},
		buckets: map[string]map[string]bool{
			"spotexec-cache-111111111111-us-east-2": {"repo/a.tar": true, "repo/b & c.tar": true},
			"spotexec-cache-111111111111-us-west-2": {"repo/w.tar": true},
			"spotexec-cache-111111111111-backup":    {"x": true}, // not a region: not spotexec's name
			"my-own-cache":                          {"x": true},
		},
		versions:  map[string][]fakeObjVersion{},
		versioned: map[string]bool{},
		failRepo:  map[string]bool{},
		busySG:    map[string]int{"sg-west": 1},
	}
	c.extra = c.serve
	c.install(t)
	return c
}

// teardown lists and removes only what spotexec created, in every region,
// and deletes security groups once the instances using them are gone.
func TestTeardown(t *testing.T) {
	c := newFakeCloud(t)
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Account != "111111111111" || strings.Join(p.Regions, ",") != "us-east-2,us-west-2" || p.Err() != nil {
		t.Fatalf("plan %+v", p)
	}
	var got []string
	for _, it := range p.Items {
		got = append(got, it.Kind+" "+it.ID)
	}
	sort.Strings(got)
	want := []string{
		"AMI ami-bake", "AMI ami-bake2", "AMI ami-disabled", "ECR repository spotexec/app", "IAM user spotexec-mac",
		"S3 bucket spotexec-cache-111111111111-us-east-2", "S3 bucket spotexec-cache-111111111111-us-west-2",
		"instance i-bake", "instance i-run", "security group sg-legacy", "security group sg-new", "security group sg-west",
		"snapshot snap-bake", "snapshot snap-orphan",
	}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if c.users["spotexec-mac"] != nil || c.keys["AKMAC"][0] != "" {
		t.Fatalf("IAM left: %v %v", c.users, c.keys)
	}
	if _, ok := c.buckets["spotexec-cache-111111111111-us-east-2"]; ok || c.buckets["my-own-cache"] == nil || c.buckets["spotexec-cache-111111111111-backup"] == nil {
		t.Fatalf("buckets %v", c.buckets)
	}
	for _, o := range c.s3owner {
		if o != "111111111111" {
			t.Fatalf("S3 call without the expected owner: %v", c.s3owner)
		}
	}
	if c.users["ops-bot"] == nil || c.keys["AKOPS"][1] != "Active" {
		t.Fatalf("a user outside /spotexec/ was touched: %v %v", c.users, c.keys)
	}
	if !strings.Contains(strings.Join(c.terminated, ","), "i-late") {
		t.Fatalf("a VM launched after the plan survived: %v", c.terminated)
	}
	if len(c.images["us-east-2"]) != 1 || len(c.snapshots["us-east-2"]) != 2 || c.snapshots["us-east-2"]["snap-prod"] == nil || c.snapshots["us-east-2"]["snap-untagged"] == nil {
		t.Fatalf("images %v, snapshots %v", c.images, c.snapshots)
	}
	if strings.Join(c.repos["us-east-2"], ",") != "team/app" {
		t.Fatalf("deleted %v", c.deleted)
	}
	d := strings.Join(c.deleted, ",")
	for _, sg := range []string{"sg-new", "sg-legacy", "sg-west"} {
		if !strings.Contains(d, "sg "+sg) {
			t.Fatalf("%s not deleted: %v", sg, c.deleted)
		}
	}
	if strings.Contains(d, "sg-handmade") || strings.Contains(strings.Join(c.terminated, ","), "i-prod") {
		t.Fatalf("touched something that isn't spotexec's: %v %v", c.deleted, c.terminated)
	}

	// Run again: nothing left of spotexec's.
	p, err = PlanTeardown(context.Background(), "admin", nil)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("second plan %+v %v", p.Items, err)
	}
}

// A GovCloud teardown signs in to that partition: the admin check, IAM and
// the bucket listing go to us-gov-west-1, never the commercial us-east-1.
func TestTeardownGovCloud(t *testing.T) {
	c := newFakeCloud(t)
	c.instances["AK-admin"] = map[string][]fakeAcctInstance{"us-gov-east-1": c.instances["AK-admin"]["us-east-2"]}
	c.late = nil
	c.buckets["spotexec-cache-111111111111-us-gov-east-1"] = map[string]bool{}
	t.Setenv("AWS_REGION", "us-gov-east-1") // the whole account, found from the session's region
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil || p.Err() != nil || p.Regional || strings.Join(p.Regions, ",") != "us-gov-east-1,us-gov-west-1" {
		t.Fatalf("%v %v %+v", err, p.Err(), p)
	}
	var got []string
	for _, it := range p.Items {
		if it.Kind == KindInstance || it.Kind == KindBucket {
			got = append(got, it.ID)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "i-bake,i-run,spotexec-cache-111111111111-us-gov-east-1" {
		t.Fatalf("plan %v", got)
	}
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if c.users["spotexec-mac"] != nil || !strings.Contains(strings.Join(c.signed, ","), "iam DeleteUser us-gov-west-1") {
		t.Fatalf("IAM user not removed through GovCloud's IAM: %v", c.signed)
	}
	// IAM is GovCloud's one endpoint; STS may also be asked regionally.
	for _, s := range c.signed {
		if !strings.Contains(s, " us-gov-") || strings.HasPrefix(s, "iam ") && !strings.HasSuffix(s, " us-gov-west-1") {
			t.Fatalf("call outside GovCloud's IAM home: %v", c.signed)
		}
	}
	if c.signed[0] != "sts GetCallerIdentity us-gov-west-1" {
		t.Fatalf("the admin session was checked in %v", c.signed[0])
	}
	if len(c.listedIn) == 0 || len(c.signed) == 0 {
		t.Fatalf("ListBuckets %v, IAM/STS %v", c.listedIn, c.signed)
	}
	for _, rg := range c.listedIn {
		if rg != "us-gov-west-1" {
			t.Fatalf("ListBuckets signed in %v", c.listedIn)
		}
	}
	if _, err := PlanTeardown(context.Background(), "admin", []string{"us-gov-east-1", "us-east-2"}); err == nil {
		t.Fatal("mixed partitions accepted")
	}
}

// What can't be deleted is reported, the rest still goes, and a re-run
// finishes the job.
func TestTeardownFailures(t *testing.T) {
	c := newFakeCloud(t)
	c.failRepo["spotexec/app"] = true
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "ECR repository spotexec/app in us-east-2") {
		t.Fatalf("err = %v", err)
	}
	if c.users["spotexec-mac"] != nil {
		t.Fatal("the other resources should still be removed")
	}
	c.failRepo["spotexec/app"] = false
	p, _ = PlanTeardown(context.Background(), "admin", nil)
	if len(p.Items) != 1 || p.Items[0].ID != "spotexec/app" {
		t.Fatalf("second plan %+v", p.Items)
	}
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
}

// A versioned cache bucket keeps every version and delete marker, so
// emptying it means deleting each by its version ID: one whose only
// entries are historical, and one with a live object too.
func TestTeardownVersionedBucket(t *testing.T) {
	c := newFakeCloud(t)
	east, west := "spotexec-cache-111111111111-us-east-2", "spotexec-cache-111111111111-us-west-2"
	c.versioned[east], c.versioned[west] = true, true
	c.buckets[east] = map[string]bool{}
	c.versions[east] = []fakeObjVersion{
		{key: "repo/a.tar", id: "3/L4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"},
		{key: "repo/a.tar", id: "dm-a", marker: true},
		{key: "repo/b & c.tar", id: "v<1>&"},
		{key: "repo/b & c.tar", id: "dm-b", marker: true},
		{key: "repo/c.tar", id: "v2"},
		{key: "repo/c.tar", id: "v3"},
		{key: "repo/c.tar", id: "dm-c", marker: true},
		{key: "repo/z.tar", id: "dm-z1", marker: true}, // only markers left: a marker-only last page
		{key: "repo/z.tar", id: "dm-z2", marker: true},
	}
	c.versions[west] = []fakeObjVersion{{key: "repo/w.tar", id: "v0"}}
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{east, west} {
		if _, ok := c.buckets[b]; ok || len(c.versions[b]) > 0 {
			t.Fatalf("%s left: objects %v, versions %v", b, c.buckets[b], c.versions[b])
		}
	}
	for _, o := range c.s3owner {
		if o != "111111111111" {
			t.Fatalf("S3 call without the expected owner: %v", c.s3owner)
		}
	}
	// Every listing starts from the beginning, and the bucket goes only
	// after one comes back empty.
	for i, op := range c.s3ops {
		if strings.HasPrefix(op, "list-from-marker ") || op == "drop "+east && (i == 0 || c.s3ops[i-1] != "list "+east+" 0") {
			t.Fatalf("S3 calls: %v", c.s3ops)
		}
	}
}

// A version Object Lock retains is reported by key, version and reason,
// the marker hiding it stays, the bucket is kept, and a re-run finishes
// once the lock lapses.
func TestTeardownLockedVersion(t *testing.T) {
	c := newFakeCloud(t)
	b := "spotexec-cache-111111111111-us-east-2"
	c.versioned[b] = true
	c.versions[b] = []fakeObjVersion{
		{key: "repo/held.tar", id: "v-held", locked: true},
		{key: "repo/held.tar", id: "dm-held", marker: true}, // hides v-held, and must keep hiding it
		{key: "repo/old.tar", id: "v-old"},
	}
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "S3 bucket "+b) || !strings.Contains(err.Error(), "repo/held.tar version v-held: Access Denied because object protected by object lock. (AccessDenied)") {
		t.Fatalf("err = %v", err)
	}
	for _, op := range c.s3ops {
		if op == "drop "+b {
			t.Fatalf("DeleteBucket tried on a bucket that isn't empty: %v", c.s3ops)
		}
	}
	var left []string
	for _, v := range c.versions[b] {
		left = append(left, v.id)
	}
	sort.Strings(left)
	if len(c.buckets[b]) > 0 || strings.Join(left, ",") != "dm-held,v-held,v-old" {
		t.Fatalf("bucket left with %v %v", c.buckets[b], c.versions[b])
	}
	for i := range c.versions[b] {
		c.versions[b][i].locked = false
	}
	p, _ = PlanTeardown(context.Background(), "admin", nil)
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.buckets[b]; ok || len(c.versions[b]) > 0 {
		t.Fatalf("bucket left: %v %v", c.buckets[b], c.versions[b])
	}
}

// A version still listed after S3 said it was deleted stops the drain
// with a diagnostic instead of deleting it over and over.
func TestTeardownStaleVersion(t *testing.T) {
	c := newFakeCloud(t)
	b := "spotexec-cache-111111111111-us-east-2"
	c.versioned[b] = true
	c.buckets[b] = map[string]bool{}
	c.versions[b] = []fakeObjVersion{{key: "repo/stuck.tar", id: "v1", sticky: true}}
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "repo/stuck.tar version v1 is still listed after it was deleted") {
		t.Fatalf("err = %v", err)
	}
	var ops []string
	for _, op := range c.s3ops {
		if strings.HasSuffix(op, b) || strings.Contains(op, b+" ") {
			ops = append(ops, op)
		}
	}
	if strings.Join(ops, ",") != "list "+b+" 1,delete "+b+" 1,list "+b+" 1" {
		t.Fatalf("S3 calls: %v", ops)
	}
}

// A machine's own spotexec key isn't an admin session.
func TestTeardownNeedsAdmin(t *testing.T) {
	c := newFakeCloud(t)
	c.accounts["AK-spotexec"] = "111111111111"
	_, err := PlanTeardown(context.Background(), "spotexec", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestS3URL(t *testing.T) {
	// RFC 3986, as SigV4 signs it: "~" bare, a space as %20.
	q := url.Values{"list-type": {"2"}, "continuation-token": {"a+b/c=~d e"}}
	for _, c := range []struct{ endpoint, region, bucket, want string }{
		{"", "us-east-1", "", "https://s3.us-east-1.amazonaws.com/"},
		{"", "us-gov-west-1", "", "https://s3.us-gov-west-1.amazonaws.com/"},
		{"", "eu-west-1", "spotexec-cache-1-eu-west-1", "https://spotexec-cache-1-eu-west-1.s3.eu-west-1.amazonaws.com/?continuation-token=a%2Bb%2Fc%3D~d%20e&list-type=2"},
		{"http://127.0.0.1:9/", "eu-west-1", "b", "http://127.0.0.1:9/b/?continuation-token=a%2Bb%2Fc%3D~d%20e&list-type=2"},
	} {
		qq := q
		if c.bucket == "" {
			qq = nil
		}
		if got := s3URL(c.endpoint, c.region, c.bucket, "/", qq); got != c.want {
			t.Errorf("s3URL(%q, %q) = %s, want %s", c.endpoint, c.bucket, got, c.want)
		}
	}
}

// While instances won't terminate, the security groups they use and the
// IAM users (whose owner IDs find them again) are kept for a re-run.
func TestTeardownStuckInstances(t *testing.T) {
	c := newFakeCloud(t)
	c.stuck = true
	p, err := PlanTeardown(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "still not terminated") {
		t.Fatalf("err = %v", err)
	}
	if c.users["spotexec-mac"] == nil || c.keys["AKMAC"][1] != "Inactive" || len(c.sgs["us-east-2"]) != 3 {
		t.Fatalf("users %v, keys %v, groups %v", c.users, c.keys, c.sgs)
	}
	c.mu.Lock()
	c.stuck = false
	c.mu.Unlock()
	p, _ = PlanTeardown(context.Background(), "admin", nil)
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if c.users["spotexec-mac"] != nil || len(c.sgs["us-east-2"]) != 1 {
		t.Fatalf("users %v, groups %v", c.users, c.sgs)
	}
}

// Apply re-checks the account and every user before touching anything, and
// reports what appeared while the plan was on screen.
func TestTeardownChangedUnderneath(t *testing.T) {
	t.Run("account switched", func(t *testing.T) {
		c := newFakeCloud(t)
		p, err := PlanTeardown(context.Background(), "admin", []string{"us-east-2"})
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.accounts["AK-admin"] = "999999999999"
		c.mu.Unlock()
		if err := p.Apply(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "nothing was deleted") {
			t.Fatalf("err = %v", err)
		}
		if len(c.log) != 0 || len(c.deleted) != 0 {
			t.Fatalf("mutations: %v %v", c.log, c.deleted)
		}
	})
	t.Run("user moved and resources added", func(t *testing.T) {
		c := newFakeCloud(t)
		c.users["spotexec-other"] = map[string]string{}
		c.keys["AKOTHER"] = [2]string{"spotexec-other", "Active"}
		p, err := PlanTeardown(context.Background(), "admin", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.userPaths["spotexec-other"] = "/elsewhere/"
		c.repos["us-east-2"] = append(c.repos["us-east-2"], "spotexec/new")
		c.mu.Unlock()
		err = p.Apply(context.Background(), t.Logf)
		if err == nil || !strings.Contains(err.Error(), "not under /spotexec/") {
			t.Fatalf("err = %v", err)
		}
		if c.keys["AKOTHER"][1] != "Active" || c.users["spotexec-other"] == nil {
			t.Fatalf("a moved user was touched: %v", c.keys)
		}
		// Not drained (a user's keys couldn't be checked): its dependencies stay.
		if c.users["spotexec-mac"] == nil {
			t.Fatal("IAM users must wait until every key is off")
		}
		c.mu.Lock()
		c.userPaths["spotexec-other"] = ""
		delete(c.users, "spotexec-other")
		delete(c.keys, "AKOTHER")
		c.mu.Unlock()
		p, _ = PlanTeardown(context.Background(), "admin", nil)
		c.mu.Lock()
		c.repos["us-east-2"] = append(c.repos["us-east-2"], "spotexec/newer")
		c.mu.Unlock()
		if err := p.Apply(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "appeared meanwhile") || !strings.Contains(err.Error(), "spotexec/newer") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("user added to the account", func(t *testing.T) {
		// The leftover check of an account-wide plan still looks at IAM,
		// though the plan's regions are now spelled out.
		c := newFakeCloud(t)
		p, err := PlanTeardown(context.Background(), "admin", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.users["spotexec-new"] = map[string]string{}
		c.keys["AKNEW"] = [2]string{"spotexec-new", "Active"}
		c.mu.Unlock()
		if err := p.Apply(context.Background(), t.Logf); err == nil || !strings.Contains(err.Error(), "appeared meanwhile") || !strings.Contains(err.Error(), "IAM user spotexec-new") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("launch into a region with no spotexec VMs", func(t *testing.T) {
		// A regional teardown leaves the keys on: a VM launched right after a
		// plan that listed none is still terminated before its group goes.
		c := newFakeCloud(t)
		c.late = nil
		c.instances["AK-admin"]["us-east-2"] = c.instances["AK-admin"]["us-east-2"][2:] // only i-prod
		p, err := PlanTeardown(context.Background(), "admin", []string{"us-east-2"})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			if it.Kind == KindInstance {
				t.Fatalf("plan lists %s", it.ID)
			}
		}
		c.mu.Lock()
		c.instances["AK-admin"]["us-east-2"] = append(c.instances["AK-admin"]["us-east-2"], fakeAcctInstance{"i-new", "pending", map[string]string{tagRunner: "aws", tagOwner: "m-1"}})
		c.mu.Unlock()
		if err := p.Apply(context.Background(), t.Logf); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(c.terminated, ","), "i-new") || len(c.sgs["us-east-2"]) != 1 {
			t.Fatalf("terminated %v, groups %v", c.terminated, c.sgs)
		}
	})
}

// iamCalls lists the IAM requests the fake has seen.
func iamCalls(c *fakeCloud) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var calls []string
	for _, s := range c.signed {
		if strings.HasPrefix(s, "iam ") {
			calls = append(calls, s)
		}
	}
	return calls
}

// A teardown limited to some regions leaves the account-wide IAM users and
// their keys alone: a machine still running in another region keeps its
// credentials, and nothing outside the regions is touched.
func TestTeardownRegionKeepsIAM(t *testing.T) {
	c := newFakeCloud(t)
	c.instances["AK-admin"]["us-west-2"] = []fakeAcctInstance{{"i-west", "running", map[string]string{tagRunner: "aws", tagOwner: "m-1", tagHost: "mac"}}}
	c.instances["AK-admin"]["eu-west-1"] = []fakeAcctInstance{{"i-eu", "running", map[string]string{tagRunner: "aws", tagOwner: "m-1", tagHost: "mac"}}}
	p, err := PlanTeardown(context.Background(), "admin", []string{"us-east-2", "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Err() != nil || !p.Regional || strings.Join(p.Regions, ",") != "eu-west-1,us-east-2" {
		t.Fatalf("plan %+v: %v", p, p.Err())
	}
	for _, it := range p.Items {
		if it.Kind == KindIAMUser || it.Region == "" {
			t.Fatalf("a regional plan lists account-wide %s %s", it.Kind, it.ID)
		}
	}
	// Apply leaves IAM alone even when a user is in the plan.
	p.Items = append(p.Items, TeardownItem{Kind: KindIAMUser, ID: "spotexec-mac"})
	if err := p.Apply(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	if c.users["spotexec-mac"] == nil || c.keys["AKMAC"] != [2]string{"spotexec-mac", "Active"} {
		t.Fatalf("the machine's IAM user or key was touched: %v %v", c.users, c.keys)
	}
	term := strings.Join(c.terminated, ",")
	if strings.Contains(term, "i-west") || !strings.Contains(term, "i-run") || !strings.Contains(term, "i-eu") {
		t.Fatalf("terminated %v", c.terminated)
	}
	if c.buckets["spotexec-cache-111111111111-us-west-2"] == nil || len(c.sgs["us-west-2"]) != 1 {
		t.Fatalf("us-west-2 touched: buckets %v, groups %v", c.buckets, c.sgs)
	}
	if calls := iamCalls(c); len(calls) != 0 {
		t.Fatalf("IAM called: %v", calls)
	}

	// The leftover check stays in the plan's regions, and off IAM.
	p, err = PlanTeardown(context.Background(), "admin", []string{"us-east-2", "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.repos["us-east-2"] = append(c.repos["us-east-2"], "spotexec/newer")
	c.repos["us-west-2"] = append(c.repos["us-west-2"], "spotexec/west")
	c.mu.Unlock()
	err = p.Apply(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "appeared meanwhile") || !strings.Contains(err.Error(), "spotexec/newer") || strings.Contains(err.Error(), "spotexec/west") || strings.Contains(err.Error(), "IAM") {
		t.Fatalf("err = %v", err)
	}
	if calls := iamCalls(c); len(calls) != 0 {
		t.Fatalf("IAM called: %v", calls)
	}
}

// A regional teardown whose VMs won't terminate keeps its security groups,
// and still never touches a key.
func TestTeardownRegionStuck(t *testing.T) {
	c := newFakeCloud(t)
	c.stuck = true
	p, err := PlanTeardown(context.Background(), "admin", []string{"us-east-2"})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "still not terminated") || !strings.Contains(err.Error(), "security group(s) kept") {
		t.Fatalf("err = %v", err)
	}
	if c.users["spotexec-mac"] == nil || c.keys["AKMAC"][1] != "Active" || len(c.sgs["us-east-2"]) != 3 {
		t.Fatalf("users %v, keys %v, groups %v", c.users, c.keys, c.sgs)
	}
}
