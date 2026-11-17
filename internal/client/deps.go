package client

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// depsLogDir is where install logs are kept on this machine.
func depsLogDir() string { return filepath.Join(config.StateDir(), "logs", "deps") }

// DepsError is a failed dependency install. Its message stays one line; the
// output that explains it is in Detail, for a terminal to print after it
// (JSON mode has it in the deps_failed event instead).
type DepsError struct {
	Err       error
	Tail      string // the install's last lines of output
	Truncated bool   // earlier output was left out of Tail
	Log       string // the full output, on this machine ("" if none)
}

func (e *DepsError) Error() string { return e.Err.Error() }
func (e *DepsError) Unwrap() error { return e.Err }

// Detail is the install's output to show under the error, sanitized for a
// terminal, and where the full log is.
func (e *DepsError) Detail() []string {
	var out []string
	if e.Tail != "" {
		lines := ui.CleanLines(e.Tail)
		if e.Truncated {
			out = append(out, fmt.Sprintf("last %d lines of the install's output:", len(lines)))
		} else {
			out = append(out, "the install's output:")
		}
		for _, l := range lines {
			out = append(out, "  "+l)
		}
	}
	if e.Log != "" {
		out = append(out, "full log: "+e.Log)
	}
	return out
}

// statusEvery is how often the install's latest line may update the status.
var statusEvery = 100 * time.Millisecond

// consumeDeps reads the helper's replies to a deps request until its result.
// The install's output goes to log only, never to the run's stdout or
// stderr: its latest line is shown as the status's detail instead.
func consumeDeps(ctx context.Context, next func(context.Context) (protocol.Frame, error), onEvent func(protocol.Event), rep ui.Reporter, label string, log *depsLog) (protocol.DepsResult, error) {
	st := &depsStatus{rep: rep}
	defer st.stop()
	for {
		f, err := next(ctx)
		if err != nil {
			return protocol.DepsResult{}, err
		}
		switch f.Kind {
		case protocol.KindStdout, protocol.KindStderr:
			log.Write(f.Data)
			st.detail(ui.LogLine(lastLine(f.Data)))
			continue
		}
		env, err := protocol.Decode(f)
		if err != nil {
			return protocol.DepsResult{}, err
		}
		switch env.Type {
		case protocol.TypeStarted:
			continue
		case protocol.TypeEvent:
			var ev protocol.Event
			_ = env.Unmarshal(&ev)
			switch ev.Name {
			case "deps_install":
				st.phase(fmt.Sprintf("%s · installing dependencies (%s)", label, ev.Message))
			case "deps_flush":
				// Message: how much is waiting, e.g. "8.1 GB".
				st.phase(fmt.Sprintf("%s · writing %s to disk before the container starts", label, ev.Message))
			}
			onEvent(ev)
			continue
		case protocol.TypeError:
			var em protocol.ErrorMsg
			_ = env.Unmarshal(&em)
			return protocol.DepsResult{}, fmt.Errorf("dependencies: %w", &transport.RemoteError{ErrorMsg: em})
		case protocol.TypeDepsResult:
			var dr protocol.DepsResult
			_ = env.Unmarshal(&dr)
			return dr, nil
		default:
			return protocol.DepsResult{}, fmt.Errorf("protocol: unexpected %q during deps", env.Type)
		}
	}
}

// depsStatus shows the install's latest line as the status's detail, at
// most every statusEvery (a chatty install sends many small frames): a line
// held back is shown when the interval is up, even if nothing follows it.
type depsStatus struct {
	rep ui.Reporter

	mu      sync.Mutex
	status  string // the phase, "" until the install starts
	line    string // the latest line
	shown   string // the line on screen
	shownAt time.Time
	timer   *time.Timer
	done    bool
}

func (d *depsStatus) phase(status string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status, d.line, d.shown = status, "", ""
	d.rep.Status("%s", status)
}

func (d *depsStatus) detail(line string) {
	if line == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.status == "" {
		return
	}
	d.line = line
	if wait := statusEvery - time.Since(d.shownAt); wait > 0 {
		if d.timer == nil {
			d.timer = time.AfterFunc(wait, d.flush)
		}
		return
	}
	d.showLocked()
}

func (d *depsStatus) flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.timer = nil
	// A line shown since this was armed restarts the interval.
	if wait := statusEvery - time.Since(d.shownAt); wait > 0 && !d.done {
		d.timer = time.AfterFunc(wait, d.flush)
		return
	}
	d.showLocked()
}

func (d *depsStatus) showLocked() {
	if d.done || d.line == d.shown {
		return
	}
	d.shown, d.shownAt = d.line, time.Now()
	d.rep.Status("%s%s%s", d.status, ui.DetailSep, d.line)
}

// stop ends updates: the phase is over.
func (d *depsStatus) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// lastLine is the last non-empty line in b.
func lastLine(b []byte) string {
	b = bytes.TrimRight(b, "\r\n \t")
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[i+1:]
	}
	return strings.TrimSpace(string(b))
}
