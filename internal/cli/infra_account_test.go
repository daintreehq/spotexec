package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	scopes, failed := accountScopes(context.Background(), accountCfg())
	for _, sc := range scopes {
		got = append(got, sc.Region+"/"+sc.Profile)
	}
	if strings.Join(got, ",") != "us-east-2/a,us-east-2/b,us-west-2/a" || len(failed) != 0 {
		t.Fatalf("scopes %v, failed %v", got, failed)
	}
}

// A multi-region runner's scopes are every region it launches in; with
// "all", every region enabled in the account, and a region list that
// couldn't be fetched is a failed scope.
func TestAccountScopesMultiRegion(t *testing.T) {
	old := enabledRegions
	t.Cleanup(func() { enabledRegions = old })
	var fail error
	enabledRegions = func(_ context.Context, profile, home string, _ ...string) ([]string, error) {
		if fail != nil {
			return nil, fail
		}
		return []string{"eu-north-1", "us-east-1", "us-east-2"}, nil
	}
	cfg := accountCfg()
	cfg.Global.Runners["m"] = &config.Runner{Name: "m", Type: config.TypeAWSSpot, Region: "eu-north-1", Regions: []string{"eu-north-1", "us-west-2"}, Profile: "a"}
	cfg.Global.Runners["any"] = &config.Runner{Name: "any", Type: config.TypeAWSSpot, Region: "us-east-1", Regions: []string{config.AllRegions}, Profile: "b"}
	var got []string
	scopes, failed := accountScopes(context.Background(), cfg)
	for _, sc := range scopes {
		got = append(got, sc.Region+"/"+sc.Profile)
	}
	want := "eu-north-1/a,eu-north-1/b,us-east-1/b,us-east-2/a,us-east-2/b,us-west-2/a"
	if strings.Join(got, ",") != want || len(failed) != 0 {
		t.Fatalf("scopes %v, failed %v; want %s", got, failed, want)
	}
	fail = errors.New("UnauthorizedOperation")
	if _, failed := accountScopes(context.Background(), cfg); len(failed) != 1 || failed[0].Profile != "b" {
		t.Fatalf("failed %v", failed)
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
			Searched: []provider.AccountScope{{Region: "us-east-2", Profile: "a", Account: "111111111111"}, {Region: "us-east-2", Profile: "b", Account: "222222222222"}, {Region: "us-west-2", Profile: "a", Account: "111111111111"}, {Region: "us-west-2", Profile: "c", Account: "111111111111"}},
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
		if !slices.Contains(strings.Split(out, "\n"), "searched us-east-2, us-west-2 (account 111111111111, profile a); us-east-2 (account 222222222222, profile b); us-west-2 (account 111111111111, profile c) (the regions your AWS runners use)") {
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

// An explicit region list is searched only where the account has the
// region enabled (the rest can't hold VMs and some never answer), except
// a region this machine's placement state says may still hold VMs; when
// the enabled regions can't be read, the list is searched as configured.
func TestAccountScopesFiltersNotEnabled(t *testing.T) {
	t.Setenv("SPOTEXEC_STATE_DIR", t.TempDir())
	old := enabledRegions
	t.Cleanup(func() { enabledRegions = old })
	var fail error
	enabledRegions = func(_ context.Context, _, _ string, _ ...string) ([]string, error) {
		if fail != nil {
			return nil, fail
		}
		return []string{"eu-north-1", "us-east-1"}, nil
	}
	cfg := accountCfg()
	m := &config.Runner{Name: "m", Type: config.TypeAWSSpot, Region: "me-south-1", Regions: []string{"me-south-1", "eu-north-1", "ap-south-2"}, Profile: "a"}
	cfg.Global.Runners = map[string]*config.Runner{"m": m}
	scopes := func() string {
		sc, _ := accountScopes(context.Background(), cfg)
		var got []string
		for _, s := range sc {
			got = append(got, s.Region)
		}
		sort.Strings(got)
		return strings.Join(got, ",")
	}
	if got := scopes(); got != "eu-north-1" {
		t.Fatalf("scopes %s, want only the enabled eu-north-1", got)
	}
	// A region the placement state remembers is searched regardless.
	dir := provider.RunnerStateDir("m")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "placement.json"), []byte(`{"active":{"ap-south-2":"2099-01-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scopes(); got != "ap-south-2,eu-north-1" {
		t.Fatalf("scopes %s, want the remembered ap-south-2 kept", got)
	}
	fail = errors.New("throttled")
	if got := scopes(); got != "ap-south-2,eu-north-1,me-south-1" {
		t.Fatalf("scopes %s, want the list as configured", got)
	}
}
