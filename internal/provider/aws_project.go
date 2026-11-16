package provider

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// What a region holds of a project, for `spotexec container regions`: its
// ECR repository's images and the project's dependency archive in the
// region's cache bucket. Every lookup is read-only and live: unlike run-cost
// placement (aws_warmth.go) nothing is cached, and nothing is created.

// Holding states.
const (
	HoldPresent = "present"  // the region holds it
	HoldMissing = "missing"  // the region doesn't hold it
	HoldNotUsed = "not_used" // the project doesn't use it
	HoldUnknown = "unknown"  // it couldn't be found out (Why says why)
)

// Holding is whether a region holds one thing a run of the project uses.
type Holding struct {
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
}

// ECRImage is one tagged image a region's repository for a project keeps.
type ECRImage struct {
	Tags     []string  `json:"tags"`
	Digest   string    `json:"digest"` // the manifest's digest
	PushedAt time.Time `json:"pushed_at"`
	Bytes    int64     `json:"size_bytes,omitempty"`
}

// RegionHolding is what one region holds of a project.
type RegionHolding struct {
	Region string
	// Image is the image tagged Project.ImageTag, pushed at Pushed (zero
	// when ECR's listing couldn't be read), with Docker ID ImageID.
	Image   Holding
	Pushed  time.Time
	ImageID string
	// Deps is the dependency archive for key DepsKey (for an image built
	// here, the key follows from this region's image).
	Deps    Holding
	DepsKey string
	// Kept is every tagged image the repository keeps, newest first; KeptErr
	// says why it couldn't be listed.
	Kept    []ECRImage
	KeptErr string
}

// ProjectHoldings looks up, in every region new instances of the runner may
// go to, what the region holds of project pj: its image (pj.ImageTag), the
// images its repository keeps, and its dependency archive. Regions come
// back sorted; one whose lookups fail says so in its row, and the error
// is about the regions themselves.
func (g *AWSGlobal) ProjectHoldings(ctx context.Context, pj *Project) ([]RegionHolding, error) {
	regions, err := g.PlacementRegions(ctx)
	regions = append([]string(nil), regions...)
	sort.Strings(regions)
	out := make([]RegionHolding, len(regions))
	for i, rg := range regions {
		out[i] = RegionHolding{Region: rg, Image: Holding{HoldUnknown, "not looked up"}, Deps: Holding{HoldUnknown, "not looked up"}}
	}
	index := map[string]int{}
	for i, rg := range regions {
		index[rg] = i
	}
	// Each region writes only its own row.
	_ = eachRegion(ctx, regions, func(ctx context.Context, rg string) error {
		out[index[rg]] = g.Regional(rg).projectHolding(ctx, pj)
		return nil
	})
	return out, err
}

// projectHolding is what this region holds of pj.
func (a *AWSSpot) projectHolding(ctx context.Context, pj *Project) RegionHolding {
	h := RegionHolding{Region: a.r.Region, Image: Holding{State: HoldNotUsed}, Deps: Holding{State: HoldNotUsed}}
	if pj.Image {
		if kept, err := a.projectImages(ctx, pj.RepoID); err != nil {
			h.KeptErr = ecrHint(err)
		} else {
			h.Kept = kept
		}
		if pj.ImageTag == "" {
			h.Image = Holding{HoldUnknown, "the image's tag couldn't be worked out"}
		} else {
			id, err := a.projectImage(ctx, pj.RepoID, pj.ImageTag)
			switch {
			case err != nil:
				h.Image = Holding{HoldUnknown, ecrHint(err)}
			case id == "":
				h.Image = Holding{State: HoldMissing}
			default:
				h.Image, h.ImageID = Holding{State: HoldPresent}, id
				for _, img := range h.Kept {
					if contains(img.Tags, pj.ImageTag) {
						h.Pushed = img.PushedAt
					}
				}
			}
		}
	}
	if !pj.Deps {
		return h
	}
	switch a.r.CacheBucket {
	case "off", "none", "false":
		h.Deps = Holding{HoldMissing, "the dependency cache is off (cache_bucket)"}
		return h
	}
	switch {
	case pj.DepsKey != "":
		h.DepsKey = pj.DepsKey
	case pj.DepsKeyFor == nil:
		h.Deps = Holding{HoldUnknown, "the dependency key couldn't be worked out"}
		return h
	case h.Image.State == HoldMissing:
		// The image built here will be new, and so will the archive keyed
		// by it.
		h.Deps = Holding{HoldMissing, "no image here, so no archive for it"}
		return h
	case h.Image.State != HoldPresent:
		h.Deps = Holding{HoldUnknown, "keyed by this region's image, which couldn't be looked up"}
		return h
	default:
		key, err := pj.DepsKeyFor(h.ImageID)
		if err != nil {
			h.Deps = Holding{HoldUnknown, "dependency key: " + err.Error()}
			return h
		}
		h.DepsKey = key
	}
	has, err := a.hasCacheEntry(ctx, pj.RepoID, h.DepsKey)
	switch {
	case err != nil:
		h.Deps = Holding{HoldUnknown, firstLineOf([]byte(err.Error()))}
	case has:
		h.Deps = Holding{State: HoldPresent}
	default:
		h.Deps = Holding{State: HoldMissing}
	}
	return h
}

// ecrHint explains an ECR failure, pointing an IAM policy from before
// ecr:Describe* at `spotexec setup`.
func ecrHint(err error) string {
	msg := firstLineOf([]byte(err.Error()))
	if strings.Contains(msg, "AccessDenied") {
		msg += " (re-run `spotexec setup` to update the IAM policy)"
	}
	return msg
}

// ecrPagesMax bounds a repository listing: a lifecycle policy keeps 10
// images, so more pages than this means the answers don't end.
const ecrPagesMax = 20

// projectImages lists the tagged images the region's repository for
// repoID keeps, newest first, creating nothing: none without a repository.
func (a *AWSSpot) projectImages(ctx context.Context, repoID string) ([]ECRImage, error) {
	var out []ECRImage
	token := ""
	for page := 0; ; page++ {
		if page == ecrPagesMax {
			return nil, fmt.Errorf("ECR: the listing of spotexec/%s didn't end after %d pages", ecrName(repoID), page)
		}
		in := map[string]any{
			"repositoryName": "spotexec/" + ecrName(repoID),
			"filter":         map[string]string{"tagStatus": "TAGGED"},
			"maxResults":     1000,
		}
		if token != "" {
			in["nextToken"] = token
		}
		var resp struct {
			Details []struct {
				Digest   string   `json:"imageDigest"`
				Tags     []string `json:"imageTags"`
				PushedAt float64  `json:"imagePushedAt"` // seconds since the epoch
				Bytes    int64    `json:"imageSizeInBytes"`
			} `json:"imageDetails"`
			Next string `json:"nextToken"`
		}
		err := a.ecr(ctx, "DescribeImages", in, &resp)
		switch {
		case err != nil && strings.Contains(err.Error(), "RepositoryNotFoundException"):
			return nil, nil
		case err != nil:
			return nil, err
		}
		for _, d := range resp.Details {
			if len(d.Tags) == 0 {
				continue
			}
			tags := append([]string(nil), d.Tags...)
			sort.Strings(tags)
			var pushed time.Time
			if d.PushedAt > 0 {
				pushed = time.UnixMilli(int64(math.Round(d.PushedAt * 1000))).UTC()
			}
			out = append(out, ECRImage{Tags: tags, Digest: d.Digest, PushedAt: pushed, Bytes: d.Bytes})
		}
		if resp.Next == "" || resp.Next == token {
			break
		}
		token = resp.Next
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].PushedAt.Equal(out[j].PushedAt) {
			return out[i].PushedAt.After(out[j].PushedAt)
		}
		return out[i].Digest < out[j].Digest
	})
	return out, nil
}
