package provider

import (
	"context"
	"strings"
	"testing"
	"time"
)

// #192: a multi-region runner finds the project's image in another of its
// regions, in the order it lists them, to copy rather than build it.
func TestImageSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		images   map[string]map[string]string
		imageErr map[string]string
		loginErr map[string]bool
		want     string // region of the source, "" none
		err      string
	}{
		{name: "the first listed region holding it",
			images: map[string]map[string]string{"r-one-1": {"t1": "sha256:a"}, "r-two-1": {"t1": "sha256:a"}, "r-three-1": {"t1": "sha256:a"}},
			want:   "r-two-1"},
		{name: "another tag only is no source",
			images: map[string]map[string]string{"r-two-1": {"t0": "sha256:a"}}},
		{name: "a region that can't be logged into is skipped",
			images:   map[string]map[string]string{"r-two-1": {"t1": "sha256:a"}, "r-three-1": {"t1": "sha256:a"}},
			loginErr: map[string]bool{"r-two-1": true},
			want:     "r-three-1"},
		{name: "no usable source is an error when one holds it",
			images:   map[string]map[string]string{"r-two-1": {"t1": "sha256:a"}},
			loginErr: map[string]bool{"r-two-1": true},
			err:      "AccessDeniedException"},
		{name: "a region that can't be asked is an error",
			imageErr: map[string]string{"r-three-1": "AccessDeniedException"},
			err:      "AccessDeniedException"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRegionsEC2{images: tc.images, imageErr: tc.imageErr, loginErr: tc.loginErr}
			g := newGlobalEnv(t, f, "r-two-1", "r-one-1", "r-three-1")
			discovered(t, g)
			if err := g.remember([]Instance{{ID: "i-1", Region: "r-one-1", LaunchedAt: time.Now()}}); err != nil {
				t.Fatal(err)
			}
			src, err := g.ImageSource(context.Background(), "i-1", "repo1", "t1")
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) || src != nil {
					t.Fatalf("src %+v, err %v", src, err)
				}
			case err != nil:
				t.Fatal(err)
			case tc.want == "":
				if src != nil {
					t.Fatalf("src %+v", src)
				}
			default:
				server := "123456789012.dkr.ecr." + tc.want + ".amazonaws.com"
				if src == nil || src.Server != server || src.Repo != server+"/spotexec/repo1" || src.User != "AWS" || src.Password != "pw-"+tc.want {
					t.Fatalf("src %+v", src)
				}
			}
			// The instance's own region is never asked, and nothing is created.
			for _, c := range f.callsOf("BatchGetImage") {
				if strings.HasPrefix(c, "r-one-1 ") {
					t.Fatalf("asked the instance's own region: %v", c)
				}
			}
		})
	}
}
