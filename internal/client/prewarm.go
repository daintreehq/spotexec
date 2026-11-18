package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
)

// ErrBootstrapFailed marks a VM whose bootstrap_script exited non-zero
// (WaitReady's error wraps it).
var ErrBootstrapFailed = errors.New("bootstrap_script failed")

type bootstrapError struct{ msg string }

func (e *bootstrapError) Error() string        { return e.msg }
func (e *bootstrapError) Is(target error) bool { return target == ErrBootstrapFailed }

// Prewarm launches the instance reserved under id (spotexec up) and waits
// until ready (normally WaitReady) says it finished first-boot setup.
//
// As on the run path, the launch itself is not cut short by ctx: an
// interrupted RunInstances could create an instance nobody records. A VM
// that was launched but never got ready (a failed bootstrap_script, an
// unreachable ssh_cidr, a boot timeout, or ctx canceled by Ctrl-C) is
// terminated, so it can't bill until its idle timer or max_lifetime, and
// recorded: a bootstrap failure pauses this fleet's automatic launches like
// one a run finds; any other failure backs off like a failed launch.
//
// Before ending a VM it did not see get ready, Prewarm asks finished (when
// given) once more: a VM that finished booting in the meantime is left
// running, since other runs may already be using it, and its idle watchdog
// is armed.
func (p *Pool) Prewarm(ctx context.Context, id string, log provider.Logger, ready func(context.Context, provider.Instance) error, finished func(context.Context, provider.Instance) bool) (provider.Instance, error) {
	if ctx.Err() != nil {
		p.Cancel(id) // interrupted before the launch request: don't send it
		return provider.Instance{}, ctx.Err()
	}
	// When the launch was planned, for timing it (its record may be pruned
	// before a slow setup finishes).
	var started time.Time
	_ = p.with(func(st *poolState) error {
		for _, l := range st.Launches {
			if l.ID == id {
				started = l.Started
			}
		}
		return nil
	})
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.R.BootTimeout.Duration+time.Minute)
	in, err := p.Provision(pctx, id, log)
	expired := pctx.Err() != nil
	cancel()
	if err != nil && (in.ID == "" || !expired) {
		// Never launched, or ProvisionHeld has already ended a VM that
		// failed to reach running.
		return in, err
	}
	if err == nil {
		err = ctx.Err() // interrupted while the launch was finishing
	}
	if err == nil {
		err = ready(ctx, in)
	}
	if err == nil {
		// Watched until it finished setup: an exact launch lead for Plan.
		if !started.IsZero() {
			_ = p.with(func(st *poolState) error {
				now := time.Now()
				st.sched().observeLaunch(in.ID, now.Sub(started), now)
				return nil
			})
		}
		return in, nil
	}
	bg := context.WithoutCancel(ctx)
	if !errors.Is(err, ErrBootstrapFailed) && finished != nil && in.Target.Host != "" {
		fctx, cancel := context.WithTimeout(bg, 15*time.Second)
		done := finished(fctx, in)
		cancel()
		if done {
			return in, fmt.Errorf("%w (%s finished booting meanwhile, so it was left running; it ends itself after idle_timeout)", err, in.ID)
		}
	}
	if !p.fenceBooting(in.ID) {
		if errors.Is(err, ErrBootstrapFailed) {
			p.failBoot(id, in.ID, err, false, ctx.Err() != nil) // the setup is broken all the same
		}
		return in, fmt.Errorf("%w (%s was left running: a run is using it)", err, in.ID)
	}
	tctx, cancel := context.WithTimeout(bg, 30*time.Second)
	terr := p.Fleet.Terminate(tctx, in.ID)
	cancel()
	p.failBoot(id, in.ID, err, terr == nil, ctx.Err() != nil)
	if terr != nil {
		return in, fmt.Errorf("%w; terminating %s also failed: %v (run `spotexec down %s`)", err, in.ID, terr, p.R.Name)
	}
	return in, fmt.Errorf("%w (terminated %s)", err, in.ID)
}

// failBoot records an instance launched under id that never got ready. Once
// the terminate request succeeded it no longer counts (as an instance or a
// pending launch); if that failed it stays visible, so health checks and
// the pool manager still see it. A bootstrap failure is remembered like one
// a run finds (the helper drains such a VM anyway); any other failure,
// unless the user interrupted, backs off further launches like a failed
// launch.
func (p *Pool) failBoot(id, instanceID string, cause error, terminated, interrupted bool) {
	_ = p.with(func(st *poolState) error {
		now := time.Now()
		n := st.note(instanceID)
		if terminated {
			n.Draining = true
			for i := range st.Launches {
				if st.Launches[i].ID == id {
					st.Launches[i].Failed = true
				}
			}
		}
		if errors.Is(cause, ErrBootstrapFailed) {
			n.Draining, n.BootFailed = true, true
			if n.BootFailedAt.IsZero() {
				n.BootFailedAt = now
			}
		}
		if !interrupted {
			st.BackoffUntil, st.LastLaunchErr = now.Add(launchBackoff), firstLine(cause.Error())
		}
		st.ListedAt = staleListing(st.ListedAt)
		return nil
	})
}

// BootFinished reports whether the VM at t finished its first-boot setup
// successfully (one probe, no waiting).
func BootFinished(ctx context.Context, r *config.Runner, t provider.Target) bool {
	out, err := transport.New(t, r.WorkRoot).Run(ctx, "cat /var/tmp/spotexec/ready 2>/dev/null", nil)
	return err == nil && strings.TrimSpace(string(out)) == "0"
}
