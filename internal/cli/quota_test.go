package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	b, err := json.Marshal(accountStatus{
		AccountSearch: &provider.AccountSearch{Instances: []provider.AccountInstance{{ID: "i-1", Region: "us-east-2"}}},
		SpotQuotas: []regionQuota{{Region: "us-east-2", Usage: provider.QuotaUsage{Region: "us-east-2", Quota: 16, Used: 8, Mine: 8}},
			{Region: "eu-west-1", Usage: provider.QuotaUsage{Quota: -1}, Error: "AccessDenied", Err: errors.New("AccessDenied")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"instances":[{"id":"i-1"`, `"spot_quotas":[{"region":"us-east-2","usage":{"region":"us-east-2","quota":16,"used":8,"mine":8,"other":0}}`, `"error":"AccessDenied"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("JSON lacks %s:\n%s", w, b)
		}
	}
}
