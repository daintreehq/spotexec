package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/ui"
)

// How a part of a parallel or sharded task ended.
const (
	partPassed      = "passed"
	partFailed      = "failed"
	partStopped     = "stopped" // cancelled by fail_fast after another part failed
	partInterrupted = "interrupted"
)

// partOutcome is how one part of a parallel or sharded task ended.
type partOutcome struct {
	part   taskPart
	status string
	code   int
	took   time.Duration
	runner string // empty when it never reached a runner
	slot   int
	local  bool // ran on this machine (--fallback-local)
}

// where says where the part ran, as its summary line puts it.
func (o partOutcome) where() string {
	switch {
	case o.local:
		return " locally"
	case o.runner != "":
		return fmt.Sprintf(" on %s#%d", o.runner, o.slot)
	}
	return ""
}

// recapLine is the part's row in the run's closing summary.
func (o partOutcome) recapLine() string {
	status := o.status
	if status == partStopped {
		status += " (fail_fast)"
	}
	line := fmt.Sprintf("%s · %s · %s%s", o.part.label, status, ui.Dur(o.took), o.where())
	if o.code != 0 && o.status != partStopped {
		line += fmt.Sprintf(" · exit %d", o.code)
	}
	return line
}

// recapOrder lists the parts failed first, then interrupted, stopped and
// passed, each group in the task's own order.
func recapOrder(outs []partOutcome) []partOutcome {
	var sorted []partOutcome
	for _, status := range []string{partFailed, partInterrupted, partStopped, partPassed} {
		for _, o := range outs {
			if o.status == status {
				sorted = append(sorted, o)
			}
		}
	}
	return sorted
}

// maxNamed caps how many failed parts the verdict names; each has its own
// row just above it.
const maxNamed = 4

// taskVerdict is the last line of a parallel or sharded run: the overall
// result, which parts failed, the wall time and the combined exit code.
func taskVerdict(name string, outs []partOutcome, exit int, interrupted bool, took time.Duration) string {
	var failed []string
	stopped := 0
	for _, o := range outs {
		switch o.status {
		case partFailed:
			failed = append(failed, o.part.label)
		case partStopped:
			stopped++
		}
	}
	n := len(outs)
	var parts []string
	if interrupted {
		parts = append(parts, "interrupted")
	}
	switch {
	case len(failed) > 0:
		named := failed
		if len(named) > maxNamed {
			named = append(named[:maxNamed:maxNamed], fmt.Sprintf("%d more", len(failed)-maxNamed))
		}
		parts = append(parts, fmt.Sprintf("%d of %d part%s failed (%s)", len(failed), n, plural(n), strings.Join(named, ", ")))
	case !interrupted && exit == 0:
		if n == 1 {
			parts = append(parts, "1 part passed")
		} else {
			parts = append(parts, fmt.Sprintf("%d parts passed", n))
		}
	}
	if stopped > 0 {
		parts = append(parts, fmt.Sprintf("%d stopped", stopped))
	}
	parts = append(parts, ui.Dur(took))
	if exit != 0 {
		parts = append(parts, fmt.Sprintf("exit %d", exit))
	}
	return name + " · " + strings.Join(parts, " · ")
}

// resultParts is each part's outcome for the NDJSON result event, in the
// task's own order.
func resultParts(outs []partOutcome) []map[string]any {
	parts := make([]map[string]any, len(outs))
	for i, o := range outs {
		m := map[string]any{"task": o.part.name, "label": o.part.label, "status": o.status, "exit_code": o.code, "duration_ms": o.took.Milliseconds()}
		if o.part.shards > 0 {
			m["shard"], m["shards"] = o.part.shard, o.part.shards
		}
		switch {
		case o.local:
			m["local"] = true
		case o.runner != "":
			m["runner"], m["slot"] = o.runner, o.slot
		}
		parts[i] = m
	}
	return parts
}
