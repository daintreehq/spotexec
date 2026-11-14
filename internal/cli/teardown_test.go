package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/provider"
)

func fakeTeardown(t *testing.T, plan *provider.TeardownPlan, applyErr error) *int {
	t.Helper()
	applied := 0
	oldPlan, oldApply := planTeardown, applyTeardown
	planTeardown = func(context.Context, string, []string) (*provider.TeardownPlan, error) { return plan, nil }
	applyTeardown = func(context.Context, *provider.TeardownPlan, func(string, ...any)) error {
		applied++
		return applyErr
	}
	t.Cleanup(func() { planTeardown, applyTeardown = oldPlan, oldApply })
	return &applied
}

func TestTeardownCommand(t *testing.T) {
	plan := func() *provider.TeardownPlan {
		return &provider.TeardownPlan{Account: "111111111111", AdminARN: "arn:aws:iam::111111111111:user/admin", Regions: []string{"us-east-2"},
			Items: []provider.TeardownItem{{Kind: provider.KindInstance, ID: "i-1", Region: "us-east-2"}, {Kind: provider.KindIAMUser, ID: "spotexec-mac"}}}
	}
	t.Run("dry run", func(t *testing.T) {
		applied := fakeTeardown(t, plan(), nil)
		out := captureStdout(t, func() {
			if err := teardownAccount(context.Background(), "", nil, false, true); err != nil {
				t.Error(err)
			}
		})
		if *applied != 0 || !strings.Contains(out, "i-1") || !strings.Contains(out, "spotexec-mac") || !strings.Contains(out, "dry run") {
			t.Fatalf("applied %d, output:\n%s", *applied, out)
		}
	})
	t.Run("asks first", func(t *testing.T) {
		// Tests have no terminal on stdin: without --yes nothing is deleted.
		applied := fakeTeardown(t, plan(), nil)
		var err error
		captureStdout(t, func() { err = teardownAccount(context.Background(), "", nil, false, false) })
		if err == nil || !strings.Contains(err.Error(), "--yes") || *applied != 0 {
			t.Fatalf("err %v, applied %d", err, *applied)
		}
	})
	t.Run("yes", func(t *testing.T) {
		applied := fakeTeardown(t, plan(), nil)
		var err error
		captureStdout(t, func() { err = teardownAccount(context.Background(), "", nil, true, false) })
		if err != nil || *applied != 1 {
			t.Fatalf("err %v, applied %d", err, *applied)
		}
	})
	t.Run("failures exit non-zero", func(t *testing.T) {
		p := plan()
		p.Failed = []string{"S3 buckets: AccessDenied"}
		applied := fakeTeardown(t, p, errors.New("couldn't remove 1 resource(s): IAM user spotexec-mac: AccessDenied"))
		var err error
		captureStdout(t, func() { err = teardownAccount(context.Background(), "", nil, true, false) })
		if err == nil || !strings.Contains(err.Error(), "IAM user spotexec-mac") || !strings.Contains(err.Error(), "S3 buckets") || *applied != 1 {
			t.Fatalf("err %v", err)
		}
	})
}
