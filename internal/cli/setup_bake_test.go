package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

func TestBakeNeed(t *testing.T) {
	if bakeNeed(nil) == "" {
		t.Error("no bake: setup should offer one")
	}
	if bakeNeed(&provider.Bake{ID: "ami-1", InUse: true}) != "" {
		t.Error("a current bake needs nothing")
	}
	if !strings.Contains(bakeNeed(&provider.Bake{ID: "ami-1", InUse: true, Stale: "a newer Ubuntu base image (ami-2) is out"}), "stale") {
		t.Error("a stale bake should be offered for re-baking")
	}
	if !strings.Contains(bakeNeed(&provider.Bake{ID: "ami-1", Expired: true, Stale: "40 days old"}), "no longer used") {
		t.Error("an expired bake should be offered for re-baking")
	}
}

func TestBakePlan(t *testing.T) {
	cases := []struct {
		force, yes, interactive bool
		want                    string
	}{
		{false, false, true, "ask"},
		{false, true, true, "hint"},   // --yes skips the offer...
		{true, true, true, "bake"},    // ...unless --bake asked for it
		{true, false, false, "bake"},  // --bake needs no terminal
		{false, false, false, "hint"}, // nobody to ask
	}
	for _, c := range cases {
		if got := bakePlan(c.force, c.yes, c.interactive); got != c.want {
			t.Errorf("bakePlan(force=%v, yes=%v, tty=%v) = %q, want %q", c.force, c.yes, c.interactive, got, c.want)
		}
	}
}

func TestAskBake(t *testing.T) {
	cands := []bakeCandidate{{r: &config.Runner{Name: "aws"}, need: "no baked image"}}
	var out bytes.Buffer
	if !askBake(&out, bufio.NewReader(strings.NewReader("y\n")), cands) {
		t.Fatal("yes not accepted")
	}
	for _, want := range []string{"aws: no baked image", "minute or more", "10 to 15 minutes", "[y/N]"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("offer lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if askBake(&out, bufio.NewReader(strings.NewReader("\n")), cands) || !strings.Contains(out.String(), "spotexec image bake aws") {
		t.Fatalf("the default is no, with a pointer to baking later:\n%s", out.String())
	}
}

func TestSetupNextRecommendsWarmUp(t *testing.T) {
	t.Setenv("SPOTEXEC_SETUP_NESTED", "")
	if out := captureStdout(t, func() { printSetupNext(true) }); !strings.Contains(out, "warm-up `sx` run") {
		t.Fatalf("AWS setup should recommend a warm-up run:\n%s", out)
	}
	if out := captureStdout(t, func() { printSetupNext(false) }); strings.Contains(out, "warm-up") {
		t.Fatalf("SSH-only setup has no cold boots to warm up:\n%s", out)
	}
}

// fakeBakes sets up three AWS runners (c pins an AMI) and fakes the bake
// lookup and the bake itself.
func fakeBakes(t *testing.T, bakes map[string]*provider.Bake, lookupErr, bakeErr map[string]error) *[]string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	cfg := `version = 1
default_pool = ["a", "b"]
[runners.a]
type = "aws-spot"
region = "us-east-1"
[runners.b]
type = "aws-spot"
region = "us-east-1"
[runners.c]
type = "aws-spot"
region = "us-east-1"
ami = "ami-pinned"
[runners.box]
type = "ssh"
hosts = ["me@box"]
`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", p)
	t.Setenv("SPOTEXEC_SETUP_NESTED", "")
	var mu sync.Mutex
	var baked []string
	oldLookup, oldBake := currentBake, doBake
	t.Cleanup(func() { currentBake, doBake = oldLookup, oldBake })
	currentBake = func(_ context.Context, a *provider.AWSSpot) (*provider.Bake, error) {
		n := a.Runner().Name
		if n == "c" {
			t.Error("looked up a bake for a runner that pins its AMI")
		}
		return bakes[n], lookupErr[n]
	}
	doBake = func(r *config.Runner, _ *provider.AWSSpot) error {
		mu.Lock()
		defer mu.Unlock()
		baked = append(baked, r.Name)
		return bakeErr[r.Name]
	}
	return &baked
}

func TestOfferBakeAsks(t *testing.T) {
	current := &provider.Bake{ID: "ami-ok", InUse: true}
	baked := fakeBakes(t, map[string]*provider.Bake{"b": current}, nil, nil)
	var out bytes.Buffer
	if err := offerBake(context.Background(), &out, bufio.NewReader(strings.NewReader("y\n")), false, false, true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*baked, ",") != "a" || !strings.Contains(out.String(), "a: no baked image") || strings.Contains(out.String(), "b:") {
		t.Fatalf("only a (no bake) should be offered and baked: baked %v\n%s", *baked, out.String())
	}
	*baked = nil
	out.Reset()
	if err := offerBake(context.Background(), &out, bufio.NewReader(strings.NewReader("n\n")), false, false, true); err != nil || len(*baked) != 0 {
		t.Fatalf("declined, yet baked %v (%v)", *baked, err)
	}
}

func TestOfferBakeYesOnlyHints(t *testing.T) {
	baked := fakeBakes(t, nil, nil, nil)
	var out bytes.Buffer
	if err := offerBake(context.Background(), &out, bufio.NewReader(strings.NewReader("")), false, true, true); err != nil {
		t.Fatal(err)
	}
	if len(*baked) != 0 || !strings.Contains(out.String(), "`spotexec image bake a`") || !strings.Contains(out.String(), "10 to 15 minutes") {
		t.Fatalf("--yes should only hint: baked %v\n%s", *baked, out.String())
	}
}

func TestOfferBakeNested(t *testing.T) {
	baked := fakeBakes(t, nil, nil, nil)
	t.Setenv("SPOTEXEC_SETUP_NESTED", "1")
	var out bytes.Buffer
	if err := offerBake(context.Background(), &out, nil, true, true, false); err != nil || len(*baked) != 0 || out.Len() != 0 {
		t.Fatalf("a nested setup must leave baking to the machine running it: %v %v %q", *baked, err, out.String())
	}
}

func TestOfferBakeForcedFailuresFailSetup(t *testing.T) {
	baked := fakeBakes(t, nil, nil, map[string]error{"a": errors.New("bootstrap_script exited 1")})
	var out bytes.Buffer
	err := offerBake(context.Background(), &out, nil, true, true, false)
	if err == nil || !strings.Contains(err.Error(), "baking a") || strings.Join(*baked, ",") != "a,b" {
		t.Fatalf("--bake: a failed bake must fail setup after trying the rest: %v, baked %v", err, *baked)
	}

	baked = fakeBakes(t, nil, map[string]error{"b": errors.New("ExpiredToken")}, nil)
	if err := offerBake(context.Background(), &out, nil, true, true, false); err == nil || !strings.Contains(err.Error(), "ExpiredToken") || strings.Join(*baked, ",") != "a" {
		t.Fatalf("--bake: a failed lookup must fail setup: %v, baked %v", err, *baked)
	}

	baked = fakeBakes(t, nil, nil, map[string]error{"a": context.Canceled})
	if err := offerBake(context.Background(), &out, nil, true, true, false); err == nil || strings.Join(*baked, ",") != "a" {
		t.Fatalf("Ctrl-C must stop the bakes and fail setup: %v, baked %v", err, *baked)
	}

	// Unasked-for (offered) bakes only warn.
	baked = fakeBakes(t, nil, nil, map[string]error{"a": errors.New("boom")})
	if err := offerBake(context.Background(), &out, bufio.NewReader(strings.NewReader("y\n")), false, false, true); err != nil {
		t.Fatalf("an offered bake that fails should only warn: %v", err)
	}
}
