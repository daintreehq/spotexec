package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daintreehq/spotexec/internal/gitseed"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// seedGit gives the slot a minimal .git ([sync] git_seed), or removes the
// one an earlier run seeded when the project no longer asks for it. Only a
// broken connection is an error: anything else is a warning, and the
// command runs without a .git.
func seedGit(ctx context.Context, s *transport.Session, m *manifest.Manifest, enabled bool, o Options, label string) error {
	if !enabled {
		return s.Conn.Send(protocol.TypeGitDrop, nil)
	}
	t := time.Now()
	plan, err := gitseed.NewPlan(ctx, m)
	if err != nil {
		if errors.Is(err, gitseed.ErrNoGit) {
			o.Rep.Warn("GIT_SEED_UNAVAILABLE", "git_seed needs a Git worktree (this project syncs as a plain directory), so the runner gets no .git")
		} else {
			o.Rep.Warn("GIT_SEED_FAILED", "couldn't read HEAD for git_seed, so the runner gets no .git: %v", err)
		}
		return s.Conn.Send(protocol.TypeGitDrop, nil)
	}
	if err := s.Conn.Send(protocol.TypeGitSeed, plan.Seed); err != nil {
		return err
	}
	env, err := s.Expect(ctx, protocol.TypeGitNeed)
	if err != nil {
		// A refused request (the helper drops any earlier seed first) is
		// the seed's problem, not the run's.
		var re *transport.RemoteError
		if errors.As(err, &re) && re.Code == protocol.CodeBadRequest {
			o.Rep.Warn("GIT_SEED_FAILED", "the runner refused git_seed, so it has no .git: %v", err)
			return nil
		}
		return err
	}
	var need protocol.GitNeed
	if err := env.Unmarshal(&need); err != nil {
		return err
	}
	if len(need.IDs) > 0 {
		// The runner may only ask for the seed's own objects, never other
		// history or objects of files that aren't synced.
		asked := map[string]bool{}
		var bad error
		for _, id := range need.IDs {
			if !plan.Has(id) || asked[id] {
				bad = fmt.Errorf("runner asked for object %q, which is not part of the seed", id)
				break
			}
			asked[id] = true
		}
		bw := &protocol.BlobWriter{C: s.Conn}
		var werr error
		if bad == nil {
			_, werr = plan.WriteObjects(ctx, need.IDs, bw)
		} else {
			werr = bad
		}
		if err := bw.CloseWithError(werr); err != nil {
			return err
		}
		if bad != nil {
			_, _ = s.Expect(ctx, protocol.TypeGitSeeded)
			return bad
		}
	}
	env, err = s.Expect(ctx, protocol.TypeGitSeeded)
	if err != nil {
		return err
	}
	var res protocol.GitSeeded
	if err := env.Unmarshal(&res); err != nil {
		return err
	}
	if res.Warning != "" {
		o.Rep.Warn("GIT_SEED_FAILED", "%s", res.Warning)
		return nil
	}
	o.Rep.Event("git_seed", map[string]any{"head": plan.Seed.Head, "ref": plan.Seed.Ref, "reseeded": res.Reseeded, "objects": res.Objects, "bytes": res.Bytes, "duration_ms": time.Since(t).Milliseconds()})
	if res.Reseeded {
		o.Rep.Status("%s · seeded .git at %s", label, short(plan.Seed.Head))
	}
	return nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "an unborn branch"
	}
	return id
}
