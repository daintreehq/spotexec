package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

func TestPrintBakesShowsBaseAgeAndStaleness(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	bakes := []provider.Bake{
		{ID: "ami-new", Created: now.Add(-50 * time.Hour), Hash: "abc123", Base: "ami-b1", Current: true, InUse: true, Host: "mac-a",
			Stale: "a newer Ubuntu base image (ami-b2) is out; used until Nov 20"},
		{ID: "ami-old", Created: now.Add(-40 * 24 * time.Hour), Hash: "abc123", Current: true, Expired: true, Mine: true, Stale: "40 days old (bakes are used for 30 days)"},
	}
	var b bytes.Buffer
	printBakes(&b, "aws", bakes, now)
	out := b.String()
	for _, want := range []string{"BASE IMAGE", "ami-b1", "2d", "40d", "in use, stale: a newer Ubuntu base image (ami-b2)", "expired: 40 days old", "spotexec image bake aws", "MADE BY", "mac-a", "this machine"} {
		if !strings.Contains(out, want) {
			t.Errorf("image list output lacks %q:\n%s", want, out)
		}
	}
}

func TestInterruptedKeepsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sshErr := errors.New("sanitize: exit status 255: Connection closed")
	if got := interrupted(ctx, sshErr); got != sshErr {
		t.Fatalf("not interrupted: error changed to %v", got)
	}
	cancel()
	got := interrupted(ctx, sshErr)
	if !errors.Is(got, context.Canceled) || !strings.Contains(got.Error(), "sanitize") {
		t.Fatalf("an interrupted bake must report cancellation and the failed step: %v", got)
	}
	if interrupted(ctx, nil) != nil {
		t.Fatal("success stays success")
	}
}
