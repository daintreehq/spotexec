package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

func fakeAccountSearch(t *testing.T, res *provider.AccountSearch, failTerm map[string]bool) *[]string {
	t.Helper()
	var terminated []string
	oldFind, oldTerm := findAccountInstances, terminateAccountInstance
	findAccountInstances = func(context.Context, []provider.AccountScope) *provider.AccountSearch { return res }
	terminateAccountInstance = func(_ context.Context, in provider.AccountInstance) error {
		if failTerm[in.ID] {
			return errors.New("UnauthorizedOperation")
		}
		terminated = append(terminated, in.ID+"@"+in.Profile)
		return nil
	}
	t.Cleanup(func() { findAccountInstances, terminateAccountInstance = oldFind, oldTerm })
	return &terminated
}

func accountCfg() *config.Config {
	return &config.Config{Global: &config.Global{Runners: map[string]*config.Runner{
		"a1":  {Name: "a1", Type: config.TypeAWSSpot, Region: "us-east-2", Profile: "a"},
		"a2":  {Name: "a2", Type: config.TypeAWSSpot, Region: "us-east-2", Profile: "a"},
		"b":   {Name: "b", Type: config.TypeAWSSpot, Region: "us-east-2", Profile: "b"},
		"w":   {Name: "w", Type: config.TypeAWSSpot, Region: "us-west-2", Profile: "a"},
		"box": {Name: "box", Type: "ssh"},
	}}}
}

func TestAccountScopes(t *testing.T) {
	var got []string
	for _, sc := range accountScopes(accountCfg()) {
		got = append(got, sc.Region+"/"+sc.Profile)
	}
	if strings.Join(got, ",") != "us-east-2/a,us-east-2/b,us-west-2/a" {
		t.Fatalf("scopes %v", got)
	}
}

// down -A terminates builders and stopped VMs through the profile that
// found each, and fails when a region couldn't be listed or a VM couldn't
// be terminated.
func TestDownAccount(t *testing.T) {
	search := func() *provider.AccountSearch {
		return &provider.AccountSearch{
			Instances: []provider.AccountInstance{
				{ID: "i-mine", Profile: "a", Region: "us-east-2", State: "running", Mine: true},
				{ID: "i-builder", Profile: "a", Region: "us-east-2", State: "stopped", Builder: true, Host: "studio"},
				{ID: "i-otheracct", Profile: "b", Region: "us-east-2", State: "stopped", Host: "studio"},
				{ID: "i-going", Profile: "a", Region: "us-east-2", State: "shutting-down", Mine: true},
			},
			Searched: []provider.AccountScope{{Region: "us-east-2", Profile: "a", Account: "111111111111"}, {Region: "us-east-2", Profile: "b", Account: "222222222222"}, {Region: "us-west-2", Profile: "a", Account: "111111111111"}},
		}
	}
	t.Run("all clean", func(t *testing.T) {
		term := fakeAccountSearch(t, search(), nil)
		out := captureStdout(t, func() {
			if err := downAccount(context.Background(), accountCfg(), "", true); err != nil {
				t.Error(err)
			}
		})
		if strings.Join(*term, ",") != "i-mine@a,i-builder@a,i-otheracct@b" {
			t.Fatalf("terminated %v", *term)
		}
		if !strings.Contains(out, "searched us-east-2 (account 111111111111, profile a), us-east-2 (account 222222222222, profile b), us-west-2 (account 111111111111, profile a)") {
			t.Fatalf("output:\n%s", out)
		}
	})
	t.Run("failed region", func(t *testing.T) {
		res := search()
		res.Failed = []provider.AccountScopeError{{AccountScope: provider.AccountScope{Region: "us-west-2", Profile: "a"}, Err: "ExpiredToken"}}
		term := fakeAccountSearch(t, res, nil)
		err := downAccount(context.Background(), accountCfg(), "", true)
		if err == nil || !strings.Contains(err.Error(), "us-west-2") || len(*term) != 3 {
			t.Fatalf("err %v, terminated %v", err, *term)
		}
	})
	t.Run("failed region, nothing found", func(t *testing.T) {
		res := &provider.AccountSearch{Failed: []provider.AccountScopeError{{AccountScope: provider.AccountScope{Region: "us-west-2"}, Err: "ExpiredToken"}}}
		fakeAccountSearch(t, res, nil)
		if err := downAccount(context.Background(), accountCfg(), "", true); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("failed termination", func(t *testing.T) {
		term := fakeAccountSearch(t, search(), map[string]bool{"i-builder": true})
		err := downAccount(context.Background(), accountCfg(), "", true)
		if err == nil || !strings.Contains(err.Error(), "i-builder") || len(*term) != 2 {
			t.Fatalf("err %v, terminated %v", err, *term)
		}
	})
	t.Run("one machine", func(t *testing.T) {
		term := fakeAccountSearch(t, search(), nil)
		if err := downAccount(context.Background(), accountCfg(), "studio", true); err != nil {
			t.Fatal(err)
		}
		if strings.Join(*term, ",") != "i-builder@a,i-otheracct@b" {
			t.Fatalf("terminated %v", *term)
		}
	})
}
