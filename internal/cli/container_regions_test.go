package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/provider"
)

func regionsTestView() regionsView {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	pushed, older := now.Add(-26*time.Hour), now.Add(-5*24*time.Hour)
	return regionsView{runner: "aws", project: "app", repoID: "app-1234abcd", platform: "linux/amd64", dockerfile: ".spotexec/Dockerfile",
		pj: &provider.Project{RepoID: "app-1234abcd", Image: true, ImageTag: "0123456789abcdef", Deps: true}, now: now,
		holdings: []provider.RegionHolding{
			{Region: "eu-north-1", Image: provider.Holding{State: provider.HoldMissing},
				Deps: provider.Holding{State: provider.HoldMissing, Why: "no image here, so no archive for it"},
				Kept: []provider.ECRImage{{Tags: []string{"fedcba9876543210"}, Digest: "sha256:m2", PushedAt: older}}},
			{Region: "us-east-1", Image: provider.Holding{State: provider.HoldPresent}, Pushed: pushed, ImageID: "sha256:cfg",
				Deps: provider.Holding{State: provider.HoldPresent}, DepsKey: "k1",
				Kept: []provider.ECRImage{{Tags: []string{"0123456789abcdef"}, Digest: "sha256:m1", PushedAt: pushed}}},
			{Region: "ap-south-1", Image: provider.Holding{State: provider.HoldUnknown, Why: "BatchGetImage: ThrottlingException"},
				Deps: provider.Holding{State: provider.HoldUnknown, Why: "keyed by this region's image, which couldn't be looked up"},
				KeptErr: "DescribeImages: AccessDeniedException"},
		}}
}

// The table gives each region's image (with its push time) and archive,
// then the other tags the repositories keep.
func TestContainerRegionsText(t *testing.T) {
	var b bytes.Buffer
	regionsTestView().writeText(&b)
	out := b.String()
	t.Log("\n" + out)
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "app: .spotexec/Dockerfile builds image tag 0123456789abcdef (linux/amd64), in 3 region(s) of aws") {
		t.Fatalf("heading %q", lines[0])
	}
	if f := strings.Join(strings.Fields(lines[2]), " "); f != "REGION IMAGE PUSHED DEPENDENCY ARCHIVE" {
		t.Fatalf("header %q", lines[2])
	}
	pushed := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	for _, want := range []string{
		"eu-north-1 no - no (no image here, so no archive for it)",
		"us-east-1 yes " + pushed + " (26h ago) yes",
		"ap-south-1 unknown (BatchGetImage: ThrottlingException) - unknown (keyed by this region's image, which couldn't be looked up)",
		"eu-north-1 fedcba9876543210",
		"ap-south-1: couldn't list images: DescribeImages: AccessDeniedException",
	} {
		found := false
		for _, l := range lines {
			found = found || strings.Contains(strings.Join(strings.Fields(l), " "), want)
		}
		if !found {
			t.Errorf("missing %q", want)
		}
	}
	// The current tag isn't listed again among the others.
	if strings.Count(out, "0123456789abcdef") != 1 || !strings.Contains(out, "(5d ago)") {
		t.Errorf("kept tags:\n%s", out)
	}

	// Without a Dockerfile only the archive is shown.
	v := regionsTestView()
	v.dockerfile, v.pj = "", &provider.Project{RepoID: "app-1234abcd", Deps: true, DepsKey: "k1"}
	v.regionsErr = errors.New("DescribeRegions: denied")
	b.Reset()
	v.writeText(&b)
	out = b.String()
	if !strings.Contains(out, "no image is built (no Dockerfile)") || strings.Contains(out, "PUSHED") || strings.Contains(out, "Other tags") ||
		!strings.Contains(out, "may be incomplete: DescribeRegions: denied") {
		t.Fatalf("deps only:\n%s", out)
	}
}

// --json carries the same, with explicit states for scripts.
func TestContainerRegionsJSON(t *testing.T) {
	var b bytes.Buffer
	if err := regionsTestView().writeJSON(&b); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Runner   string `json:"runner"`
		Tag      string `json:"image_tag"`
		Platform string `json:"platform"`
		Regions  []struct {
			Region string `json:"region"`
			Image  struct {
				State    string     `json:"state"`
				Why      string     `json:"why"`
				PushedAt *time.Time `json:"pushed_at"`
				ID       string     `json:"id"`
			} `json:"image"`
			Deps struct {
				State, Why, Key string
			} `json:"deps"`
			Kept      []provider.ECRImage `json:"kept"`
			KeptError string              `json:"kept_error"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Runner != "aws" || got.Tag != "0123456789abcdef" || got.Platform != "linux/amd64" || len(got.Regions) != 3 {
		t.Fatalf("json: %s", b.String())
	}
	eu, us, ap := got.Regions[0], got.Regions[1], got.Regions[2]
	if eu.Image.State != "missing" || eu.Image.PushedAt != nil || eu.Deps.State != "missing" || len(eu.Kept) != 1 {
		t.Errorf("eu-north-1: %+v", eu)
	}
	if us.Image.State != "present" || us.Image.PushedAt == nil || !us.Image.PushedAt.Equal(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)) ||
		us.Image.ID != "sha256:cfg" || us.Deps.State != "present" || us.Deps.Key != "k1" {
		t.Errorf("us-east-1: %+v", us)
	}
	if ap.Image.State != "unknown" || ap.Deps.State != "unknown" || ap.KeptError == "" || ap.Kept == nil {
		t.Errorf("ap-south-1: %+v", ap)
	}
	if !strings.Contains(b.String(), `"kept": []`) || !strings.Contains(b.String(), `"pushed_at": "2026-11-11T12:00:00Z"`) {
		t.Errorf("json keys: %s", b.String())
	}
}
