// Package ui renders lifecycle telemetry: an erasable spinner plus a dim
// footer for humans at a terminal; plain, sparse lines when stderr isn't a
// terminal (agents, CI, pipes); or NDJSON events with --json,
// SPOTEXEC_JSON=1 or SPOTEXEC_AGENT=1. Command stdout/stderr always pass
// through untouched.
package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Mode selects the reporter style.
type Mode int

const (
	ModeAuto Mode = iota
	ModeJSON
	ModePretty
	ModeQuiet
	ModePlain
)

// Reporter is the telemetry sink.
type Reporter interface {
	// Event records a lifecycle event (always emitted in JSON mode).
	Event(name string, fields map[string]any)
	// Status shows transient progress (pretty mode only).
	Status(format string, args ...any)
	// Clear erases transient progress before command output.
	Clear()
	// Warn prints a warning in either mode.
	Warn(code, format string, args ...any)
	// Done prints the final footer in pretty mode.
	Done(summary string, ok bool)
	// PartDone reports one finished part of a larger run (a task of a
	// parallel task) without ending the output; JSON and quiet modes leave
	// it to the events.
	PartDone(summary string, ok bool)
	// IsJSON reports agent mode.
	IsJSON() bool
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// New picks a reporter. eventsFile, when set, receives NDJSON in addition.
func New(mode Mode, w *os.File, eventsFile string) (Reporter, error) {
	var extra io.Writer
	if eventsFile != "" {
		f, err := os.OpenFile(eventsFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		extra = f
	}
	if mode == ModeAuto {
		switch {
		case os.Getenv("SPOTEXEC_AGENT") == "1":
			mode = ModeJSON
		case IsTerminal(w):
			mode = ModePretty
		default:
			mode = ModePlain
		}
	}
	switch mode {
	case ModePlain:
		return &plainReporter{w: w, extra: extra}, nil
	case ModeJSON:
		return &jsonReporter{w: w, extra: extra}, nil
	case ModeQuiet:
		return &quietReporter{&plainReporter{w: w, extra: extra}}, nil
	}
	p := &prettyReporter{w: w, extra: extra}
	return p, nil
}

type jsonReporter struct {
	mu    sync.Mutex
	w     io.Writer
	extra io.Writer
}

func (j *jsonReporter) Event(name string, fields map[string]any) {
	m := map[string]any{"event": name}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(orderedEvent(name, m))
	j.mu.Lock()
	defer j.mu.Unlock()
	j.w.Write(append(b, '\n'))
	if j.extra != nil {
		j.extra.Write(append(b, '\n'))
	}
}

func (j *jsonReporter) Status(string, ...any) {}
func (j *jsonReporter) Clear()                {}
func (j *jsonReporter) Done(string, bool)     {}
func (j *jsonReporter) PartDone(string, bool) {}
func (j *jsonReporter) IsJSON() bool          { return true }
func (j *jsonReporter) Warn(code, format string, args ...any) {
	j.Event("warning", map[string]any{"code": code, "message": fmt.Sprintf(format, args...)})
}

// orderedEvent puts "event" first for readability.
type ordered struct {
	name string
	m    map[string]any
}

func orderedEvent(name string, m map[string]any) ordered { return ordered{name, m} }

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"event":`)
	n, _ := json.Marshal(o.name)
	b.Write(n)
	keys := make([]string, 0, len(o.m))
	for k := range o.m {
		if k != "event" {
			keys = append(keys, k)
		}
	}
	sortStrings(keys)
	for _, k := range keys {
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(o.m[k])
		if err != nil {
			return nil, err
		}
		b.WriteByte(',')
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

type prettyReporter struct {
	mu      sync.Mutex
	w       io.Writer
	extra   io.Writer
	line    string
	shown   bool
	stopped bool
	frame   int
	ticker  *time.Ticker
	stop    chan struct{}
}

var spin = []string{"◐", "◓", "◑", "◒"}

const (
	dim   = "\x1b[2m"
	reset = "\x1b[0m"
	yel   = "\x1b[33m"
	red   = "\x1b[31m"
	grn   = "\x1b[32m"
)

func (p *prettyReporter) Event(name string, fields map[string]any) {
	if p.extra != nil {
		m := map[string]any{"event": name}
		for k, v := range fields {
			m[k] = v
		}
		b, _ := json.Marshal(orderedEvent(name, m))
		p.extra.Write(append(b, '\n'))
	}
}

func (p *prettyReporter) Status(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.line = fmt.Sprintf(format, args...)
	if p.ticker == nil {
		p.ticker = time.NewTicker(120 * time.Millisecond)
		p.stop = make(chan struct{})
		go func(t *time.Ticker, stop chan struct{}) {
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					p.mu.Lock()
					if !p.stopped && p.line != "" {
						p.frame++
						p.draw()
					}
					p.mu.Unlock()
				}
			}
		}(p.ticker, p.stop)
	}
	p.draw()
}

func (p *prettyReporter) draw() {
	fmt.Fprintf(p.w, "\r\x1b[2K%s %s%s%s", spin[p.frame%len(spin)], dim, truncate(p.line, termWidth()-4), reset)
	p.shown = true
}

func (p *prettyReporter) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
}

func (p *prettyReporter) clearLocked() {
	if p.shown {
		fmt.Fprint(p.w, "\r\x1b[2K")
		p.shown = false
	}
	if p.ticker != nil {
		p.ticker.Stop()
		close(p.stop)
		p.ticker = nil
	}
	p.line = ""
}

func (p *prettyReporter) Warn(code, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := p.line
	p.clearLocked()
	fmt.Fprintf(p.w, "%s! %s%s\n", yel, fmt.Sprintf(format, args...), reset)
	if line != "" && !p.stopped {
		p.mu.Unlock()
		p.Status("%s", line)
		p.mu.Lock()
	}
	if p.extra != nil {
		b, _ := json.Marshal(map[string]any{"event": "warning", "code": code, "message": fmt.Sprintf(format, args...)})
		p.extra.Write(append(b, '\n'))
	}
}

func (p *prettyReporter) Done(summary string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.stopped = true
	mark := grn + "●" + reset
	if !ok {
		mark = red + "●" + reset
	}
	fmt.Fprintf(p.w, "%s %s%s%s\n", mark, dim, summary, reset)
}

func (p *prettyReporter) PartDone(summary string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := p.line
	p.clearLocked()
	mark := grn + "✓" + reset
	if !ok {
		mark = red + "✗" + reset
	}
	fmt.Fprintf(p.w, "%s %s%s%s\n", mark, dim, summary, reset)
	if line != "" && !p.stopped {
		p.mu.Unlock()
		p.Status("%s", line)
		p.mu.Lock()
	}
}

func (p *prettyReporter) IsJSON() bool { return false }

func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 20 {
		return w
	}
	return 100
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ClearingWriter erases the spinner before the first byte of command output.
type ClearingWriter struct {
	W    io.Writer
	R    Reporter
	once sync.Once
}

func (c *ClearingWriter) Write(p []byte) (int, error) {
	c.once.Do(c.R.Clear)
	return c.W.Write(p)
}

// Bytes formats a byte count.
func Bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Dur formats a duration compactly.
func Dur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return strings.TrimSuffix(d.Round(time.Second).String(), "0s")
}

// plainReporter is the default when stderr isn't a terminal. An agent reads
// everything on stderr, so it says little: warnings, a progress line only
// when a phase takes a while (booting, queueing, installing dependencies)
// and every 30s after that, and one summary line at the end. No colour, no
// carriage returns.
type plainReporter struct {
	mu      sync.Mutex
	w       io.Writer
	extra   io.Writer
	line    string
	key     string
	gen     int
	stopped bool
}

// plainDelay is how long a phase must last before it's mentioned.
var plainDelay = 3 * time.Second

// phaseKey identifies a phase regardless of the counters in its text
// ("queued 2nd 4.1s" and "queued 2nd 5.2s" are the same phase).
var digits = regexp.MustCompile(`[0-9][0-9.:]*`)

func (p *plainReporter) Event(name string, fields map[string]any) {
	if p.extra != nil {
		m := map[string]any{"event": name}
		for k, v := range fields {
			m[k] = v
		}
		b, _ := json.Marshal(orderedEvent(name, m))
		p.mu.Lock()
		p.extra.Write(append(b, '\n'))
		p.mu.Unlock()
	}
}

func (p *plainReporter) Status(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if strings.Contains(line, " · running ") {
		return // the command's own output says it's running
	}
	key := digits.ReplaceAllString(line, "#")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.line = line
	if key == p.key {
		return // same phase: keep its timer
	}
	p.key = key
	p.gen++
	gen := p.gen
	var tick func(d time.Duration)
	tick = func(d time.Duration) {
		time.AfterFunc(d, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.gen != gen || p.stopped || p.line == "" {
				return
			}
			fmt.Fprintf(p.w, "sx: %s\n", p.line)
			tick(30 * time.Second)
		})
	}
	tick(plainDelay)
}

func (p *plainReporter) Clear() {
	p.mu.Lock()
	p.line, p.key = "", ""
	p.gen++
	p.mu.Unlock()
}

func (p *plainReporter) Warn(code, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, "sx: warning: %s\n", msg)
	if p.extra != nil {
		b, _ := json.Marshal(map[string]any{"event": "warning", "code": code, "message": msg})
		p.extra.Write(append(b, '\n'))
	}
}

func (p *plainReporter) Done(summary string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.gen++
	fmt.Fprintf(p.w, "sx: %s\n", summary)
}

func (p *plainReporter) PartDone(summary string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, "sx: %s\n", summary)
}

func (p *plainReporter) IsJSON() bool { return false }

// quietReporter (-q) drops progress and the summary but keeps warnings: a
// write-back conflict, a timeout or a local fallback is never silent.
type quietReporter struct{ *plainReporter }

func (quietReporter) Status(string, ...any) {}
func (quietReporter) Done(string, bool)     {}
func (quietReporter) PartDone(string, bool) {}
