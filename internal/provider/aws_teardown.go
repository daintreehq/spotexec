package provider

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The kinds of resource a teardown removes, in the order it removes them.
const (
	KindInstance      = "instance"
	KindImage         = "AMI"
	KindSnapshot      = "snapshot"
	KindRepository    = "ECR repository"
	KindBucket        = "S3 bucket"
	KindSecurityGroup = "security group"
	KindIAMUser       = "IAM user"
)

// TeardownItem is one resource spotexec created.
type TeardownItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Region string `json:"region,omitempty"` // "" for global resources (IAM)
	Note   string `json:"note,omitempty"`
}

// TeardownPlan is every spotexec resource found in an account.
type TeardownPlan struct {
	Account  string         `json:"account"`
	AdminARN string         `json:"admin_arn"`
	Regions  []string       `json:"regions"`
	Items    []TeardownItem `json:"items"`
	Failed   []string       `json:"failed,omitempty"` // what couldn't be listed

	adminProfile string
}

// Teardown timings (tests shorten them).
var (
	teardownWait = 10 * time.Minute
	teardownPoll = 10 * time.Second
)

// PlanTeardown lists everything spotexec created in the admin session's
// account: instances (fleet VMs and bake builders), baked AMIs and their
// snapshots, spotexec's security groups, spotexec/* ECR repositories,
// spotexec-cache-<account>-* S3 buckets and the IAM users under /spotexec/.
// Only resources carrying spotexec's tags, or its exact names, are
// included. regions defaults to every region enabled in the account.
func PlanTeardown(ctx context.Context, adminProfile string, regions []string) (*TeardownPlan, error) {
	admin := accountClient("us-east-1", accountResolver(adminProfile))
	acct, arn, err := callerIdentity(ctx, admin)
	if err != nil {
		return nil, fmt.Errorf("removing spotexec from an AWS account needs an admin session: %w", err)
	}
	if strings.Contains(arn, ":user"+iamUserPath) {
		return nil, fmt.Errorf("signed in as %s, a spotexec machine user, which can't remove spotexec: sign in as an administrator (or pass --admin-profile)", arn)
	}
	p := &TeardownPlan{Account: acct, AdminARN: arn, adminProfile: adminProfile}
	if len(regions) == 0 {
		b, err := admin.ec2(ctx, "DescribeRegions", nil)
		if err != nil {
			return nil, fmt.Errorf("listing the account's regions: %w", err)
		}
		var v struct {
			Regions []string `xml:"regionInfo>item>regionName"`
		}
		if err := xml.Unmarshal(b, &v); err != nil || len(v.Regions) == 0 {
			return nil, fmt.Errorf("listing the account's regions: %v", err)
		}
		regions = v.Regions
	}
	sort.Strings(regions)
	p.Regions = regions
	failed := func(what string, err error) { p.Failed = append(p.Failed, fmt.Sprintf("%s: %v", what, err)) }

	search := FindAccountInstances(ctx, adminScopes(adminProfile, regions))
	for i := range search.Instances {
		in := search.Instances[i]
		note := in.Runner + ", " + in.State
		if in.Builder {
			note = "bake builder " + note
		}
		if in.Host != "" {
			note += ", from " + in.Host
		}
		p.Items = append(p.Items, TeardownItem{Kind: KindInstance, ID: in.ID, Region: in.Region, Note: note})
	}
	for _, f := range search.Failed {
		p.Failed = append(p.Failed, fmt.Sprintf("instances in %s: %s", f.Region, f.Err))
	}
	for _, region := range regions {
		c := accountClient(region, admin.creds)
		if err := p.listImages(ctx, c, region); err != nil {
			failed("AMIs and snapshots in "+region, err)
		}
		if err := p.listSecurityGroups(ctx, c, region); err != nil {
			failed("security groups in "+region, err)
		}
		if err := p.listRepositories(ctx, c, region); err != nil {
			failed("ECR repositories in "+region, err)
		}
	}
	if err := p.listBuckets(ctx, admin); err != nil {
		failed("S3 buckets", err)
	}
	if err := p.listUsers(ctx, admin); err != nil {
		failed("IAM users", err)
	}
	return p, nil
}

// Err reports what couldn't be listed.
func (p *TeardownPlan) Err() error {
	if len(p.Failed) == 0 {
		return nil
	}
	return errors.New("couldn't list " + strings.Join(p.Failed, "; "))
}

// paginate calls page with "" and then each token it returns until there is
// none, failing on a token that repeats rather than looping forever.
func paginate(page func(token string) (next string, err error)) error {
	seen := map[string]bool{}
	token := ""
	for {
		next, err := page(token)
		if err != nil || next == "" {
			return err
		}
		if seen[next] {
			return fmt.Errorf("paging didn't advance (token %q repeated)", next)
		}
		seen[next] = true
		token = next
	}
}

func (p *TeardownPlan) listImages(ctx context.Context, c *awsClient, region string) error {
	err := paginate(func(token string) (string, error) {
		q := url.Values{"Owner.1": {"self"}, "Filter.1.Name": {"tag-key"}, "Filter.1.Value.1": {tagOwner}, "MaxResults": {"1000"}, "IncludeDisabled": {"true"}}
		if token != "" {
			q.Set("NextToken", token)
		}
		b, err := c.ec2(ctx, "DescribeImages", q)
		if err != nil {
			return "", err
		}
		var imgs struct {
			Items []struct {
				ID   string `xml:"imageId"`
				Name string `xml:"name"`
			} `xml:"imagesSet>item"`
			Next string `xml:"nextToken"`
		}
		if err := xml.Unmarshal(b, &imgs); err != nil {
			return "", err
		}
		for _, im := range imgs.Items {
			p.Items = append(p.Items, TeardownItem{Kind: KindImage, ID: im.ID, Region: region, Note: im.Name})
		}
		return imgs.Next, nil
	})
	if err != nil {
		return err
	}
	return paginate(func(token string) (string, error) {
		q := url.Values{"Owner.1": {"self"}, "Filter.1.Name": {"tag-key"}, "Filter.1.Value.1": {tagOwner}, "MaxResults": {"1000"}}
		if token != "" {
			q.Set("NextToken", token)
		}
		b, err := c.ec2(ctx, "DescribeSnapshots", q)
		if err != nil {
			return "", err
		}
		var snaps struct {
			IDs  []string `xml:"snapshotSet>item>snapshotId"`
			Next string   `xml:"nextToken"`
		}
		if err := xml.Unmarshal(b, &snaps); err != nil {
			return "", err
		}
		for _, id := range snaps.IDs {
			p.Items = append(p.Items, TeardownItem{Kind: KindSnapshot, ID: id, Region: region})
		}
		return snaps.Next, nil
	})
}

func (p *TeardownPlan) listSecurityGroups(ctx context.Context, c *awsClient, region string) error {
	return paginate(func(token string) (string, error) {
		q := spotexecSGFilter()
		if token != "" {
			q.Set("NextToken", token)
		}
		b, err := c.ec2(ctx, "DescribeSecurityGroups", q)
		if err != nil {
			return "", err
		}
		groups, next, err := parseSecurityGroups(b)
		if err != nil {
			return "", err
		}
		for _, g := range groups {
			// Only groups spotexec made: its tag, or exactly its name and
			// description. A hand-made "spotexec-ssh-prod" is left alone.
			if g.tag(tagOwner) != "" || g.madeBySpotexec() {
				p.Items = append(p.Items, TeardownItem{Kind: KindSecurityGroup, ID: g.ID, Region: region, Note: g.Name + " in " + g.VPC})
			}
		}
		return next, nil
	})
}

func (p *TeardownPlan) listRepositories(ctx context.Context, c *awsClient, region string) error {
	return paginate(func(token string) (string, error) {
		in := map[string]any{"maxResults": 1000}
		if token != "" {
			in["nextToken"] = token
		}
		var out struct {
			Repos []struct {
				Name string `json:"repositoryName"`
			} `json:"repositories"`
			Next string `json:"nextToken"`
		}
		if err := ecrCall(ctx, c, "DescribeRepositories", in, &out); err != nil {
			return "", err
		}
		for _, r := range out.Repos {
			if strings.HasPrefix(r.Name, "spotexec/") {
				p.Items = append(p.Items, TeardownItem{Kind: KindRepository, ID: r.Name, Region: region})
			}
		}
		return out.Next, nil
	})
}

// listBuckets finds the dependency-cache buckets spotexec names
// spotexec-cache-<account>-<region>. Buckets set by hand with cache_bucket
// aren't spotexec's to delete.
func (p *TeardownPlan) listBuckets(ctx context.Context, admin *awsClient) error {
	want := map[string]string{} // exactly the names spotexec gives, in the regions searched
	for _, region := range p.Regions {
		want["spotexec-cache-"+p.Account+"-"+region] = region
	}
	return paginate(func(token string) (string, error) {
		q := url.Values{"max-buckets": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		st, b, err := s3Call(ctx, admin, "us-east-1", http.MethodGet, "", "/", q, nil, nil)
		if err != nil {
			return "", err
		}
		if st != 200 {
			return "", fmt.Errorf("ListBuckets: HTTP %d: %s", st, firstLineOf(b))
		}
		var v struct {
			Names []string `xml:"Buckets>Bucket>Name"`
			Next  string   `xml:"ContinuationToken"`
		}
		if err := xml.Unmarshal(b, &v); err != nil {
			return "", err
		}
		for _, n := range v.Names {
			if region, ok := want[n]; ok {
				p.Items = append(p.Items, TeardownItem{Kind: KindBucket, ID: n, Region: region})
			}
		}
		return v.Next, nil
	})
}

// listUsers finds the IAM users setup creates: exactly under /spotexec/
// (PathPrefix would also match /spotexec/anything/).
func (p *TeardownPlan) listUsers(ctx context.Context, admin *awsClient) error {
	return paginate(func(marker string) (string, error) {
		q := url.Values{"PathPrefix": {iamUserPath}, "MaxItems": {"1000"}}
		if marker != "" {
			q.Set("Marker", marker)
		}
		b, err := admin.query(ctx, "iam", iamAPIVersion, "ListUsers", q)
		if err != nil {
			return "", iamHint(err)
		}
		var v struct {
			Users []struct {
				Name string `xml:"UserName"`
				Path string `xml:"Path"`
			} `xml:"ListUsersResult>Users>member"`
			Truncated bool   `xml:"ListUsersResult>IsTruncated"`
			Marker    string `xml:"ListUsersResult>Marker"`
		}
		if err := xml.Unmarshal(b, &v); err != nil {
			return "", err
		}
		for _, u := range v.Users {
			if u.Path == iamUserPath {
				p.Items = append(p.Items, TeardownItem{Kind: KindIAMUser, ID: u.Name})
			}
		}
		if !v.Truncated {
			return "", nil
		}
		return v.Marker, nil
	})
}

// Apply deletes everything in the plan. In order: the machines' access keys
// are deactivated, so nothing new gets launched; every spotexec instance is
// terminated (including any launched since the plan) and confirmed gone;
// then AMIs, tagged snapshots, repositories, buckets and security groups
// are deleted, and the IAM users last. It carries on past failures and
// returns an error naming everything it couldn't delete. While instances
// remain, the security groups and IAM users are kept, so a re-run can
// still find and finish them.
func (p *TeardownPlan) Apply(ctx context.Context, log func(string, ...any)) error {
	admin := accountClient("us-east-1", accountResolver(p.adminProfile))
	// The session may have changed accounts while the plan was on screen.
	if acct, _, err := callerIdentity(ctx, admin); err != nil {
		return fmt.Errorf("checking the admin session again: %w", err)
	} else if acct != p.Account {
		return fmt.Errorf("the admin session is now for account %s, not %s as listed; nothing was deleted", acct, p.Account)
	}
	var errs []string
	fail := func(it TeardownItem, err error) {
		where := it.ID
		if it.Region != "" {
			where += " in " + it.Region
		}
		errs = append(errs, fmt.Sprintf("%s %s: %v", it.Kind, where, err))
	}
	byKind := map[string][]TeardownItem{}
	for _, it := range p.Items {
		byKind[it.Kind] = append(byKind[it.Kind], it)
	}
	client := func(region string) *awsClient { return accountClient(region, admin.creds) }

	// First stop every machine from creating more: deactivate their keys.
	// Until that has worked for every user (and all were listed), a machine
	// could still launch things, so nothing they depend on is deleted.
	keysOff := !strings.Contains(strings.Join(p.Failed, "\n"), "IAM users")
	for _, it := range byKind[KindIAMUser] {
		if err := checkSpotexecUser(ctx, admin, it.ID); err != nil {
			fail(it, err)
			keysOff = false
			continue
		}
		if err := deactivateKeys(ctx, admin, it.ID, log); err != nil {
			fail(it, err)
			keysOff = false
		}
	}
	// Then terminate every spotexec instance, including any launched since
	// the plan was made, and wait until they're confirmed gone (security
	// groups can't go before them).
	instancesGone := true
	if len(byKind[KindInstance]) > 0 || len(byKind[KindIAMUser]) > 0 {
		if err := stopMachineVMs(ctx, p.adminProfile, "", p.Regions, log); err != nil {
			errs = append(errs, "instances: "+err.Error())
			instancesGone = false
		}
	}
	for _, it := range byKind[KindImage] {
		// Snapshots are deleted below, only those with spotexec's tag.
		if _, err := client(it.Region).ec2(ctx, "DeregisterImage", url.Values{"ImageId": {it.ID}}); err != nil && awsCode(err) != "InvalidAMIID.NotFound" && awsCode(err) != "InvalidAMIID.Unavailable" {
			fail(it, err)
			continue
		}
		log("deregistered AMI %s in %s", it.ID, it.Region)
	}
	for _, it := range byKind[KindSnapshot] {
		// Most went with their AMI just now.
		if _, err := client(it.Region).ec2(ctx, "DeleteSnapshot", url.Values{"SnapshotId": {it.ID}}); err != nil && awsCode(err) != "InvalidSnapshot.NotFound" {
			fail(it, err)
			continue
		}
		log("deleted snapshot %s in %s", it.ID, it.Region)
	}
	for _, it := range byKind[KindRepository] {
		if err := ecrCall(ctx, client(it.Region), "DeleteRepository", map[string]any{"repositoryName": it.ID, "force": true}, nil); err != nil && !strings.Contains(err.Error(), "RepositoryNotFoundException") {
			fail(it, err)
			continue
		}
		log("deleted ECR repository %s in %s", it.ID, it.Region)
	}
	for _, it := range byKind[KindBucket] {
		if err := deleteBucket(ctx, admin, it.ID, it.Region, p.Account); err != nil {
			fail(it, err)
			continue
		}
		log("deleted S3 bucket %s", it.ID)
	}
	drained := instancesGone && keysOff
	if groups := byKind[KindSecurityGroup]; len(groups) > 0 && drained {
		for _, it := range groups {
			if err := deleteSecurityGroup(ctx, client(it.Region), it.ID); err != nil {
				fail(it, err)
				continue
			}
			log("deleted security group %s (%s) in %s", it.ID, it.Note, it.Region)
		}
	} else if len(groups) > 0 {
		errs = append(errs, fmt.Sprintf("%d security group(s) kept until every key is off and every instance gone", len(groups)))
	}
	for _, it := range byKind[KindIAMUser] {
		if !drained {
			// Its owner ID is how a re-run finds the VMs it left.
			fail(it, errors.New("kept until every key is off and every instance gone"))
			continue
		}
		if err := checkSpotexecUser(ctx, admin, it.ID); err != nil {
			fail(it, err)
			continue
		}
		if err := deleteIAMUser(ctx, admin, it.ID, log); err != nil {
			fail(it, err)
		}
	}
	// Anything a machine created while the plan was on screen is still
	// there: say so rather than claim the account is clean.
	if len(errs) == 0 {
		if again, err := PlanTeardown(ctx, p.adminProfile, p.Regions); err != nil {
			errs = append(errs, "checking for leftovers: "+err.Error())
		} else if err := again.Err(); err != nil {
			errs = append(errs, "checking for leftovers: "+err.Error())
		} else if len(again.Items) > 0 {
			var left []string
			for _, it := range again.Items {
				left = append(left, it.Kind+" "+it.ID)
			}
			errs = append(errs, fmt.Sprintf("%d resource(s) appeared meanwhile and are still there: %s", len(left), strings.Join(left, ", ")))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("couldn't remove %d resource(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// deleteSecurityGroup retries while EC2 still counts a terminated
// instance's network interface against the group.
func deleteSecurityGroup(ctx context.Context, c *awsClient, id string) error {
	deadline := time.Now().Add(teardownWait)
	for {
		_, err := c.ec2(ctx, "DeleteSecurityGroup", url.Values{"GroupId": {id}})
		if err == nil || awsCode(err) == "InvalidGroup.NotFound" {
			return nil
		}
		if awsCode(err) != "DependencyViolation" || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(teardownPoll):
		}
	}
}

// checkSpotexecUser makes sure name is still a user setup made, directly
// under /spotexec/ (it may have been moved or replaced since the plan).
func checkSpotexecUser(ctx context.Context, admin *awsClient, name string) error {
	u, err := getUser(ctx, admin, name)
	if err != nil {
		return iamHint(err)
	}
	if !strings.HasSuffix(u.ARN, ":user"+iamUserPath+name) {
		return fmt.Errorf("is now %s, not under %s; left alone", u.ARN, iamUserPath)
	}
	return nil
}

// deactivateKeys turns off a spotexec IAM user's access keys.
func deactivateKeys(ctx context.Context, admin *awsClient, user string, log func(string, ...any)) error {
	keys, err := listAccessKeys(ctx, admin, user)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.Status == "Inactive" {
			continue
		}
		if _, err := admin.query(ctx, "iam", iamAPIVersion, "UpdateAccessKey", url.Values{"UserName": {user}, "AccessKeyId": {k.ID}, "Status": {"Inactive"}}); err != nil {
			return iamHint(fmt.Errorf("deactivating access key %s: %w", k.ID, err))
		}
		log("deactivated access key %s (%s)", k.ID, user)
	}
	return nil
}

// deleteIAMUser deletes a spotexec IAM user: its access keys, its inline
// policy, then the user.
func deleteIAMUser(ctx context.Context, admin *awsClient, user string, log func(string, ...any)) error {
	keys, err := listAccessKeys(ctx, admin, user)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := deleteAccessKey(ctx, admin, user, k.ID); err != nil {
			return err
		}
		log("deleted access key %s", k.ID)
	}
	if _, err := admin.query(ctx, "iam", iamAPIVersion, "DeleteUserPolicy", url.Values{"UserName": {user}, "PolicyName": {"spotexec"}}); err != nil && awsCode(err) != "NoSuchEntity" {
		return iamHint(fmt.Errorf("deleting %s's policy: %w", user, err))
	}
	if _, err := admin.query(ctx, "iam", iamAPIVersion, "DeleteUser", url.Values{"UserName": {user}}); err != nil {
		return iamHint(fmt.Errorf("deleting IAM user %s: %w", user, err))
	}
	log("deleted IAM user %s", user)
	return nil
}

// deleteBucket empties a cache bucket and deletes it, asserting at every
// step that the account owns it.
func deleteBucket(ctx context.Context, c *awsClient, bucket, region, account string) error {
	owner := map[string]string{"x-amz-expected-bucket-owner": account}
	q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
	for page := 0; page < 100000; page++ {
		st, b, err := s3Call(ctx, c, region, http.MethodGet, bucket, "/", q, nil, owner)
		if err != nil {
			return err
		}
		if st == 404 {
			return nil
		}
		if st != 200 {
			return fmt.Errorf("listing objects: HTTP %d: %s", st, firstLineOf(b))
		}
		var v struct {
			Keys []string `xml:"Contents>Key"`
			Next string   `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(b, &v); err != nil {
			return err
		}
		if len(v.Keys) > 0 {
			var body bytes.Buffer
			body.WriteString(`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Quiet>true</Quiet>`)
			for _, k := range v.Keys {
				body.WriteString("<Object><Key>")
				_ = xml.EscapeText(&body, []byte(k))
				body.WriteString("</Key></Object>")
			}
			body.WriteString("</Delete>")
			sum := md5.Sum(body.Bytes())
			hdr := map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:]), "Content-Type": "application/xml"}
			for k, val := range owner {
				hdr[k] = val
			}
			st, b, err := s3Call(ctx, c, region, http.MethodPost, bucket, "/", url.Values{"delete": {""}}, body.Bytes(), hdr)
			if err != nil {
				return err
			}
			if st != 200 {
				return fmt.Errorf("deleting objects: HTTP %d: %s", st, firstLineOf(b))
			}
			var res struct {
				Errors []struct {
					Key  string `xml:"Key"`
					Code string `xml:"Code"`
				} `xml:"Error"`
			}
			if xml.Unmarshal(b, &res) == nil && len(res.Errors) > 0 {
				return fmt.Errorf("couldn't delete %d object(s), e.g. %s (%s)", len(res.Errors), res.Errors[0].Key, res.Errors[0].Code)
			}
		}
		if v.Next == "" {
			break
		}
		q.Set("continuation-token", v.Next)
	}
	st, b, err := s3Call(ctx, c, region, http.MethodDelete, bucket, "/", nil, nil, owner)
	if err != nil {
		return err
	}
	if st/100 != 2 && st != 404 {
		return fmt.Errorf("HTTP %d: %s", st, firstLineOf(b))
	}
	return nil
}

// s3Call makes a signed S3 request (bucket "" = the service, for
// ListBuckets). Tests point the "s3" endpoint at a path-style server.
func s3Call(ctx context.Context, c *awsClient, region, method, bucket, path string, q url.Values, body []byte, hdr map[string]string) (int, []byte, error) {
	creds, err := c.creds.get(ctx)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, s3URL(c.endpoints["s3"], region, bucket, path, q), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
	signV4(req, body, creds, region, "s3", time.Now())
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp.StatusCode, b, nil
}

// s3URL is a bucket's virtual-hosted URL in its region (or the service's,
// for bucket ""); a test endpoint is addressed path-style.
func s3URL(endpoint, region, bucket, path string, q url.Values) string {
	var u string
	switch ep := strings.TrimSuffix(endpoint, "/"); {
	case ep != "" && bucket == "":
		u = ep + path
	case ep != "":
		u = ep + "/" + bucket + path
	case bucket == "":
		u = "https://s3." + region + ".amazonaws.com" + path
	default:
		u = "https://" + bucket + ".s3." + region + ".amazonaws.com" + path
	}
	if len(q) > 0 {
		u += "?" + s3CanonicalQuery(q)
	}
	return u
}

// ecrCall makes an ECR JSON API call.
func ecrCall(ctx context.Context, c *awsClient, action string, in, out any) error {
	body, _ := json.Marshal(in)
	status, b, err := c.do(ctx, "ecr", "application/x-amz-json-1.1",
		map[string]string{"X-Amz-Target": "AmazonEC2ContainerRegistry_V20150921." + action}, body)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("%s: %s %s", action, e.Type, e.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
