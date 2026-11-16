package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/provider"
)

// regionsTimeout bounds the lookups of `container regions` across every
// region.
const regionsTimeout = time.Minute

// cmdContainerRegions shows, for this checkout, the tag its Dockerfile
// builds and which of an AWS runner's regions hold that image and the
// dependency archive for it.
func cmdContainerRegions() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "regions [runner]",
		Short: "Show the image tag this checkout builds and which of an AWS runner's regions hold it (pushed when) and its dependency archive",
		Long: fmt.Sprintf(`Show the tag this checkout's Dockerfile resolves to and, in every region
the runner launches in, whether ECR holds the image with that tag (and when
it was pushed), whether the region's dependency cache holds the archive for
it, and the other tags the region's repository keeps (ECR keeps a region's
newest %d builds). Each region builds and keeps its own image, so a run that
lands in a region without them builds the image and installs dependencies
first. Nothing is created or changed.`, provider.ECRKeepBuilds),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load("")
			if err != nil {
				return err
			}
			p := l.cfg.Project
			if p == nil || p.Path == "" || !l.ws.Marked {
				return fmt.Errorf("not in a spotexec project: run this where a .spotexec.toml is")
			}
			var r *config.Runner
			if len(args) > 0 {
				r, err = l.cfg.Runner(args[0])
			} else {
				r, err = firstEphemeral(l.cfg)
			}
			if err != nil {
				return err
			}
			if r.Type != config.TypeAWSSpot {
				return fmt.Errorf("runner %s is not an aws-spot runner: only AWS keeps images and dependency archives per region", r.Name)
			}
			ctr := p.Container
			switch {
			case ctr.Image != "":
				return fmt.Errorf("the project's container is the prebuilt image %s, which runs pull: no image of it is built or kept in ECR", ctr.Image)
			case !ctr.Enabled() && p.Deps.Install == "":
				return fmt.Errorf("the project has no Dockerfile ([container]) and no [deps]: nothing is kept per region")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), regionsTimeout)
			defer cancel()
			var m *manifest.Manifest
			if client.PlacementNeedsManifest(p) {
				if m, err = manifest.Build(ctx, client.ManifestOptions(l.ws.Root, p)); err != nil {
					return err
				}
			}
			pj, err := client.ProjectKeys(p, l.ws, m, false, r)
			if err != nil {
				return err
			}
			g, err := provider.NewAWSPricer(r)
			if err != nil {
				return err
			}
			hs, err := g.ProjectHoldings(ctx, pj)
			if len(hs) == 0 {
				if err == nil {
					err = fmt.Errorf("runner %s has no regions to look in (none of its regions is enabled in this AWS account)", r.Name)
				}
				return err
			}
			v := regionsView{runner: r.Name, project: l.ws.RepoName, repoID: l.ws.RepoID, platform: "linux/" + r.Arch,
				dockerfile: ctr.Dockerfile, pj: pj, holdings: hs, regionsErr: err, now: time.Now()}
			if asJSON {
				if err := v.writeJSON(os.Stdout); err != nil {
					return err
				}
			} else {
				v.writeText(os.Stdout)
			}
			return v.failed()
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

// regionsView is what `spotexec container regions` shows.
type regionsView struct {
	runner, project, repoID, platform string
	dockerfile                        string // "": no image is built
	pj                                *provider.Project
	holdings                          []provider.RegionHolding
	regionsErr                        error // the region list may be incomplete
	now                               time.Time
}

func (v regionsView) writeJSON(w io.Writer) error {
	type image struct {
		provider.Holding
		PushedAt *time.Time `json:"pushed_at,omitempty"`
		ID       string     `json:"id,omitempty"`
	}
	type deps struct {
		provider.Holding
		Key string `json:"key,omitempty"`
	}
	type region struct {
		Region    string              `json:"region"`
		Image     image               `json:"image"`
		Deps      deps                `json:"deps"`
		Kept      []provider.ECRImage `json:"kept"`
		KeptError string              `json:"kept_error,omitempty"`
	}
	rows := make([]region, 0, len(v.holdings))
	for _, h := range v.holdings {
		rw := region{Region: h.Region, Image: image{Holding: h.Image, ID: h.ImageID}, Deps: deps{Holding: h.Deps, Key: h.DepsKey},
			Kept: h.Kept, KeptError: h.KeptErr}
		if !h.Pushed.IsZero() {
			at := h.Pushed
			rw.Image.PushedAt = &at
		}
		if rw.Kept == nil {
			rw.Kept = []provider.ECRImage{}
		}
		rows = append(rows, rw)
	}
	out := map[string]any{"runner": v.runner, "project": v.project, "repo_id": v.repoID, "platform": v.platform, "regions": rows}
	if v.dockerfile != "" {
		out["dockerfile"], out["image_tag"] = v.dockerfile, v.pj.ImageTag
	}
	if v.regionsErr != nil {
		out["regions_error"] = v.regionsErr.Error()
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// failed is the command's error when no region could be looked up at
// all (expired credentials, the deadline), so scripts can tell that from
// regions that hold nothing; partial failures show in the rows.
func (v regionsView) failed() error {
	for _, h := range v.holdings {
		known := func(x provider.Holding) bool {
			return x.State == provider.HoldPresent || x.State == provider.HoldMissing
		}
		if known(h.Image) || known(h.Deps) {
			return nil
		}
	}
	return fmt.Errorf("no region of %s could be looked up", v.runner)
}

// holdingText is a holding as the table shows it.
func holdingText(h provider.Holding) string {
	s := map[string]string{provider.HoldPresent: "yes", provider.HoldMissing: "no", provider.HoldNotUsed: "-", provider.HoldUnknown: "unknown"}[h.State]
	if h.Why != "" {
		s += " (" + h.Why + ")"
	}
	return s
}

// when is a time as the tables show it: local, with its age.
func (v regionsView) when(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04") + " (" + provider.AgeText(v.now.Sub(t)) + " ago)"
}

func (v regionsView) writeText(w io.Writer) {
	image := v.dockerfile != ""
	n := fmt.Sprintf("%d region(s) of %s", len(v.holdings), v.runner)
	if image {
		fmt.Fprintf(w, "%s: %s builds image tag %s (%s), in %s:\n\n", v.project, v.dockerfile, v.pj.ImageTag, v.platform, n)
	} else {
		fmt.Fprintf(w, "%s: no image is built (no Dockerfile); dependency archive in %s:\n\n", v.project, n)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if image {
		fmt.Fprintln(tw, "REGION\tIMAGE\tPUSHED\tDEPENDENCY ARCHIVE")
	} else {
		fmt.Fprintln(tw, "REGION\tDEPENDENCY ARCHIVE")
	}
	for _, h := range v.holdings {
		if !image {
			fmt.Fprintf(tw, "%s\t%s\n", h.Region, holdingText(h.Deps))
			continue
		}
		pushed := "-"
		if !h.Pushed.IsZero() {
			pushed = v.when(h.Pushed)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.Region, holdingText(h.Image), pushed, holdingText(h.Deps))
	}
	tw.Flush()
	if image {
		v.writeKept(w)
	}
	if v.regionsErr != nil {
		fmt.Fprintf(w, "\nThe runner's regions may be incomplete: %v\n", v.regionsErr)
	}
	if image {
		fmt.Fprintln(w, "\nEach region builds and keeps its own image, and the dependency archive is keyed by it: a run that lands where either is missing builds the image or installs dependencies first.")
	} else {
		fmt.Fprintln(w, "\nEach region keeps its own dependency cache: a run that lands where the archive is missing installs dependencies first.")
	}
}

// writeKept lists the other tags each region's repository keeps.
func (v regionsView) writeKept(w io.Writer) {
	fmt.Fprintf(w, "\nOther tags kept (ECR keeps a region's newest %d builds):\n", provider.ECRKeepBuilds)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows, listed := 0, 0
	var failed []string
	for _, h := range v.holdings {
		if h.KeptErr != "" {
			failed = append(failed, fmt.Sprintf("  %s: couldn't list images: %s", h.Region, h.KeptErr))
			continue
		}
		listed++
		for _, img := range h.Kept {
			var other []string
			for _, t := range img.Tags {
				if t != v.pj.ImageTag {
					other = append(other, t)
				}
			}
			if len(other) == 0 {
				continue
			}
			pushed := "-"
			if !img.PushedAt.IsZero() {
				pushed = v.when(img.PushedAt)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", h.Region, strings.Join(other, ", "), pushed)
			rows++
		}
	}
	switch {
	case rows == 0 && listed == 0:
		fmt.Fprintln(tw, "  unknown: no region's images could be listed")
	case rows == 0 && len(failed) > 0:
		fmt.Fprintln(tw, "  none in the regions listed")
	case rows == 0:
		fmt.Fprintln(tw, "  none")
	}
	tw.Flush()
	for _, f := range failed {
		fmt.Fprintln(w, f)
	}
}
