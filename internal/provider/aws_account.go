package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AccountScope is one region searched with one AWS profile.
type AccountScope struct {
	Region  string `json:"region"`
	Profile string `json:"profile,omitempty"` // "" = the default credential chain
	Account string `json:"account,omitempty"` // the account the profile signs in to, once known
}

func (s AccountScope) String() string {
	out := s.Region
	var via []string
	if s.Account != "" {
		via = append(via, "account "+s.Account)
	}
	if s.Profile != "" {
		via = append(via, "profile "+s.Profile)
	}
	if len(via) > 0 {
		out += " (" + strings.Join(via, ", ") + ")"
	}
	return out
}

// AccountScopeError is a scope that couldn't be (fully) searched.
type AccountScopeError struct {
	AccountScope
	Err string `json:"error"`
}

// AccountSearch is every spotexec instance found in a set of scopes.
type AccountSearch struct {
	Instances []AccountInstance   `json:"instances"`
	Searched  []AccountScope      `json:"searched"`
	Failed    []AccountScopeError `json:"failed,omitempty"`
}

// Err reports the scopes that couldn't be searched, if any.
func (s *AccountSearch) Err() error {
	if len(s.Failed) == 0 {
		return nil
	}
	var msgs []string
	for _, f := range s.Failed {
		msgs = append(msgs, fmt.Sprintf("couldn't list %s: %s", f.AccountScope, f.Err))
	}
	return errors.New(strings.Join(msgs, "; "))
}

// accountInstanceStates are the states in which an instance still exists
// (and may bill, a stopped one for its volumes).
var accountInstanceStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// accountResolver returns the credentials for a profile (tests replace it).
var accountResolver = func(profile string) *awsCredResolver { return &awsCredResolver{profile: profile} }

// accountEndpoints overrides service endpoints (tests).
var accountEndpoints map[string]string

func accountClient(region string, creds *awsCredResolver) *awsClient {
	return &awsClient{region: region, creds: creds, http: &http.Client{Timeout: 60 * time.Second}, endpoints: accountEndpoints}
}

// FindAccountInstances lists every spotexec instance, whichever machine
// launched it: fleet VMs and image-bake builders, in every state but
// terminated, in each scope. Each instance records the account, region and
// profile it was found through; scopes that couldn't be listed are reported
// in Failed (their partial results are kept).
func FindAccountInstances(ctx context.Context, scopes []AccountScope) *AccountSearch {
	me, _ := ownerID()
	res := &AccountSearch{}
	resolvers := map[string]*awsCredResolver{}
	accounts := map[string]string{}
	seenScope := map[AccountScope]bool{}
	type key struct{ account, id string }
	seen := map[key]bool{}
	for _, sc := range scopes {
		sc.Account = ""
		if seenScope[sc] {
			continue
		}
		seenScope[sc] = true
		r := resolvers[sc.Profile]
		if r == nil {
			r = accountResolver(sc.Profile)
			resolvers[sc.Profile] = r
		}
		c := accountClient(sc.Region, r)
		if _, ok := accounts[sc.Profile]; !ok {
			// Only successes are cached: STS is regional, so a failure in
			// one region says nothing about the next.
			acct, _, err := callerIdentity(ctx, c)
			if err != nil {
				res.Failed = append(res.Failed, AccountScopeError{sc, err.Error()})
				continue
			}
			accounts[sc.Profile] = acct
		}
		sc.Account = accounts[sc.Profile]
		ins, err := listSpotexecInstances(ctx, c)
		for _, in := range ins {
			k := key{sc.Account, in.InstanceID}
			if seen[k] {
				continue
			}
			seen[k] = true
			res.Instances = append(res.Instances, accountInstance(in, sc, me))
		}
		if err != nil {
			res.Failed = append(res.Failed, AccountScopeError{sc, err.Error()})
		} else {
			res.Searched = append(res.Searched, sc)
		}
	}
	sort.SliceStable(res.Instances, func(i, j int) bool {
		a, b := res.Instances[i], res.Instances[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Launched.Before(b.Launched)
	})
	return res
}

// listSpotexecInstances pages through one region's spotexec instances.
func listSpotexecInstances(ctx context.Context, c *awsClient) ([]ec2Instance, error) {
	p := url.Values{"Filter.1.Name": {"tag-key"}, "Filter.1.Value.1": {tagRunner}, "Filter.1.Value.2": {tagBake},
		"Filter.2.Name": {"instance-state-name"}}
	for i, st := range accountInstanceStates {
		p.Set(fmt.Sprintf("Filter.2.Value.%d", i+1), st)
	}
	var out []ec2Instance
	for {
		b, err := c.ec2(ctx, "DescribeInstances", p)
		if err != nil {
			return out, err
		}
		ins, next, err := parseDescribeInstances(b)
		if err != nil {
			return out, err
		}
		out = append(out, ins...)
		if next == "" {
			return out, nil
		}
		p.Set("NextToken", next)
	}
}

func accountInstance(in ec2Instance, sc AccountScope, me string) AccountInstance {
	market := "on-demand"
	if in.Lifecycle == "spot" {
		market = "spot"
	}
	ai := AccountInstance{ID: in.InstanceID, Region: sc.Region, Account: sc.Account, Profile: sc.Profile,
		Runner: in.tag(tagRunner), Host: in.tag(tagHost), Owner: in.tag(tagOwner), Type: in.InstanceType,
		Market: market, State: in.State, IP: in.IPAddress, Launched: in.launched()}
	if ai.Runner == "" {
		ai.Runner, ai.Builder = in.tag(tagBake), true
	}
	ai.Mine = ai.Owner != "" && ai.Owner == me
	return ai
}

// TerminateAccountInstance terminates an instance FindAccountInstances
// found, through the profile that found it, after checking it still
// carries spotexec's tags.
func TerminateAccountInstance(ctx context.Context, in AccountInstance) error {
	c := accountClient(in.Region, accountResolver(in.Profile))
	b, err := c.ec2(ctx, "DescribeInstances", url.Values{"InstanceId.1": {in.ID}})
	switch {
	case awsCode(err) == "InvalidInstanceID.NotFound":
		// EC2 is eventually consistent: NotFound doesn't prove it's gone.
		// The tag-filtered search just found it, so terminate it anyway (the
		// call reports it if it really doesn't exist).
	case err != nil:
		return err
	default:
		ins, _, err := parseDescribeInstances(b)
		if err != nil {
			return err
		}
		if len(ins) > 0 && ins[0].State == "terminated" {
			return nil
		}
		if len(ins) > 0 && ins[0].tag(tagRunner) == "" && ins[0].tag(tagBake) == "" {
			return fmt.Errorf("%s is not a spotexec instance", in.ID)
		}
	}
	_, err = c.ec2(ctx, "TerminateInstances", url.Values{"InstanceId.1": {in.ID}})
	return err
}
