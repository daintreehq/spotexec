package client

import (
	"context"
	"sync"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/manifest"
)

// buildManifest scans a worktree (tests count the scans).
var buildManifest = manifest.Build

// SharedScan is one scan of a worktree that the parts of a fan-out (a
// sharded or parallel task) all start from, so N parts that sync the same
// files don't each list and hash them (#275). It is taken lazily, by the
// first part to need it, while that part already counts as seeking a slot;
// the others wait for it. Each part's first attempt gets its own Clone, so
// upload corrections and write-back stay its own; a replay or a move
// rescans, as a run on its own does.
type SharedScan struct {
	opts   manifest.Options
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
	m      *manifest.Manifest
	err    error
}

// NewSharedScan is a scan of ws with p's sync settings, taken within ctx
// (not a single part's: a part stopped by fail_fast mustn't end it for the
// rest). Close it once the parts are done.
func NewSharedScan(ctx context.Context, ws *Workspace, p *config.Project) *SharedScan {
	ctx, cancel := context.WithCancel(ctx)
	return &SharedScan{opts: ManifestOptions(ws.Root, p), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Close stops a scan still running: no part is waiting for it any more.
func (s *SharedScan) Close() { s.cancel() }

// get returns a copy of the scan for one attempt, starting the scan if no
// part has yet. A part whose ctx ends stops waiting; the scan goes on for
// the others. Every part gets the same error when the scan fails.
func (s *SharedScan) get(ctx context.Context) (*manifest.Manifest, error) {
	s.once.Do(func() {
		go func() {
			defer close(s.done)
			s.m, s.err = buildManifest(s.ctx, s.opts)
		}()
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.m.Clone(), nil
}
