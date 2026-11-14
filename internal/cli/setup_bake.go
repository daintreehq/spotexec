package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
)

// bakeTime is roughly how long `image bake` takes, said before asking.
const bakeTime = "about 10 to 15 minutes, plus your bootstrap"

// bakeNeed says why runner r should be baked ("" = its current bake is
// fine): it has none, or the one it has is stale or no longer used.
func bakeNeed(b *provider.Bake) string {
	switch {
	case b == nil:
		return "no baked image"
	case b.Expired:
		return "its baked image is no longer used (" + b.Stale + ")"
	case b.Stale != "":
		return "its baked image is stale (" + b.Stale + ")"
	}
	return ""
}

// bakePlan is what setup does about baking: "bake", "ask", "hint" (say how
// to bake, without asking) or "" (nothing).
func bakePlan(force, yes, interactive bool) string {
	switch {
	case force:
		return "bake"
	case yes || !interactive:
		return "hint"
	}
	return "ask"
}

type bakeCandidate struct {
	r    *config.Runner
	a    *provider.AWSSpot
	need string
}

// Swapped in tests.
var (
	currentBake = func(ctx context.Context, a *provider.AWSSpot) (*provider.Bake, error) { return a.CurrentBake(ctx) }
	doBake      = func(r *config.Runner, a *provider.AWSSpot) error { return runBake(r, a, nil) }
)

// offerBake ends an AWS setup: for AWS runners without a current bake it
// offers (or with force, goes ahead) to bake one, since an unbaked VM spends
// a minute or more on its bootstrap before the first command. --yes skips
// the offer unless force (--bake) asked for it. Only a bake that was asked
// for up front (force) fails setup; otherwise problems are warnings.
func offerBake(ctx context.Context, w io.Writer, in *bufio.Reader, force, yes, interactive bool) error {
	if os.Getenv("SPOTEXEC_SETUP_NESTED") != "" {
		return nil // another machine's setup: bakes are shared, it offers them
	}
	g, err := config.LoadGlobal()
	if err != nil {
		return nil
	}
	var cands []bakeCandidate
	var errs []string
	for _, r := range awsRunners(g) {
		if r.AMI != "" && r.AMI != "auto" {
			continue // pinned: a bake would never be used
		}
		p, err := provider.New(r)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		a, ok := p.(*provider.AWSSpot)
		if !ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		b, err := currentBake(cctx, a)
		cancel()
		if err != nil {
			fmt.Fprintf(w, "! couldn't check %s for a baked image: %v\n", r.Name, err)
			errs = append(errs, fmt.Sprintf("%s: checking for a baked image: %v", r.Name, err))
			continue
		}
		if need := bakeNeed(b); need != "" {
			cands = append(cands, bakeCandidate{r, a, need})
		}
	}
	fail := func() error {
		if !force || len(errs) == 0 {
			return nil
		}
		return fmt.Errorf("--bake: %s", strings.Join(errs, "; "))
	}
	if len(cands) == 0 {
		return fail()
	}
	switch bakePlan(force, yes, interactive) {
	case "hint":
		for _, c := range cands {
			fmt.Fprintf(w, "  %s: %s; `spotexec image bake %s` (%s) makes cold starts much faster\n", c.r.Name, c.need, c.r.Name, bakeTime)
		}
		return nil
	case "ask":
		if !askBake(w, in, cands) {
			return nil
		}
	}
	for _, c := range cands {
		fmt.Fprintf(w, "Baking an image for %s (%s)…\n", c.r.Name, bakeTime)
		if err := doBake(c.r, c.a); err != nil {
			fmt.Fprintf(w, "! baking %s failed: %v\n  Retry with `spotexec image bake %s`.\n", c.r.Name, err, c.r.Name)
			errs = append(errs, fmt.Sprintf("baking %s: %v", c.r.Name, err))
			if errors.Is(err, context.Canceled) {
				break // Ctrl-C: don't start the next one
			}
		}
	}
	return fail()
}

// askBake explains what a bake is for and how long it takes, then asks.
func askBake(w io.Writer, in *bufio.Reader, cands []bakeCandidate) bool {
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.r.Name
		fmt.Fprintf(w, "\n%s: %s.", c.r.Name, c.need)
	}
	fmt.Fprintf(w, "\nWithout one, every new VM spends a minute or more installing its bootstrap (apt packages, Docker) before the first command runs.\n")
	fmt.Fprintf(w, "Bake an image for %s now? It runs an on-demand VM for %s each, and every machine sharing this runner config then boots from it. [y/N] ", strings.Join(names, ", "), bakeTime)
	line, _ := in.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	if a != "y" && a != "yes" {
		for _, n := range names {
			fmt.Fprintf(w, "  skipped; bake later with `spotexec image bake %s`\n", n)
		}
		return false
	}
	return true
}
