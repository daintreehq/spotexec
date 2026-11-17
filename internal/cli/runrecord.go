package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/ui"
)

// Run records (#217). Every run leaves a small JSON file on this machine
// saying where it ran, where its time went, the warnings it raised, how it
// ended and the VMs it launched, so a busy period can be looked into after
// the terminals that showed it are gone. Command output never goes in a
// record: only spotexec's own warnings, errors and launch messages, each
// cut to a bounded length. Recording never fails, slows or prints anything
// for a run: a record that can't be written is lost.
const (
	runRecordVersion = 1
	// The newest records kept, and at most this many bytes of them.
	runRecordsKept  = 2000
	runRecordsBytes = 32 << 20
	recordTextMax   = 1024 // bytes of a warning, error or command line
	recordWarnMax   = 100  // warnings kept per part (and for the whole run)
	recordLaunchMax = 20   // launches kept per part
	recordLogMax    = 40   // provider messages kept per launch
	recordPartsMax  = 256  // parts of a task kept
	// What the whole record keeps, however many parts share it, so it
	// stays well under a megabyte or two.
	recordWarnTotal   = 400
	recordLaunchTotal = 100
	recordLogTotal    = 400
	recordTmpPrefix   = ".tmp-"
)

// outputText is a warning argument that may hold another program's output
// (what RTK printed): reporters show it, records leave it out.
type outputText string

func (t outputText) String() string { return string(t) }

// runRecordDir is where records go (a variable for tests).
var runRecordDir = func() string { return filepath.Join(config.StateDir(), "logs", "runs") }

// runRecord is one invocation's record: a single run, or a parallel or
// sharded task with a part for each of its runs.
type runRecord struct {
	Version    int       `json:"version"`
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMS int64     `json:"duration_ms"`
	PID        int       `json:"pid"`
	Worktree   string    `json:"worktree,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Task       string    `json:"task,omitempty"` // a parallel or sharded task
	ExitCode   int       `json:"exit_code"`
	// Warnings raised for the whole task rather than one of its parts.
	Warnings        []recordWarning `json:"warnings"`
	WarningsDropped int             `json:"warnings_dropped,omitempty"`
	Parts           []*recordPart   `json:"parts"`
	PartsDropped    int             `json:"parts_dropped,omitempty"`
}

// recordPart is one run: the only part of a single run, or one of a task's.
type recordPart struct {
	Name       string    `json:"name,omitempty"` // the task part's label ("unit 2/4")
	Command    string    `json:"command,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMS int64     `json:"duration_ms"`
	// Status: passed, failed, interrupted, or stopped (by a sibling's
	// failure under fail_fast).
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Local    bool   `json:"local,omitempty"` // ran on this machine (--fallback-local)
	// Where it ran (its last attempt).
	Runner       string `json:"runner,omitempty"`
	Slot         int    `json:"slot,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	Region       string `json:"region,omitempty"`
	Zone         string `json:"zone,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Attempts     int    `json:"attempts"`
	// PhasesMS is where its time went (client.Phase*), summed over
	// its attempts.
	PhasesMS    map[string]int64 `json:"phases_ms"`
	TimedOut    bool             `json:"timed_out,omitempty"`
	OOMKilled   bool             `json:"oom_killed,omitempty"`
	SetupFailed bool             `json:"setup_failed,omitempty"`
	// Error: spotexec failed the run (exit 125, or 123 for a project
	// image that failed to build).
	Error           *recordError    `json:"error,omitempty"`
	Warnings        []recordWarning `json:"warnings"`
	WarningsDropped int             `json:"warnings_dropped,omitempty"`
	// Launches: the VMs this run's demand launched, or tried to.
	Launches        []*recordLaunch `json:"launches,omitempty"`
	LaunchesDropped int             `json:"launches_dropped,omitempty"`

	launchIdx map[string]*recordLaunch // nil for a launch left out
}

type recordError struct {
	Code string `json:"code"` // QUEUE_TIMEOUT, DEPS_FAILED, … or SPOTEXEC_ERROR
	// Reason, when the run lost its runner and wasn't replayed:
	// RUNNER_UNRESPONSIVE, MEMORY_PRESSURE, CLIENT_SUSPENDED or
	// REGION_UNREACHABLE.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`
}

type recordWarning struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}

// recordLaunch is one VM launch: requested on scale_out, then launched (the
// request returned an instance), running, or failed. Log has the provider's
// messages, which name the options it tried and skipped before the one it
// launched.
type recordLaunch struct {
	LaunchID     string     `json:"launch_id"`
	Runner       string     `json:"runner"`
	Status       string     `json:"status"` // requested | launched | running | failed
	RequestedAt  time.Time  `json:"requested_at"`
	Demand       *int       `json:"demand,omitempty"`    // runs waiting for a slot when it was asked for
	Instances    *int       `json:"instances,omitempty"` // live VMs then
	LaunchedAt   *time.Time `json:"launched_at,omitempty"`
	RunningAt    *time.Time `json:"running_at,omitempty"`
	InstanceID   string     `json:"instance_id,omitempty"`
	Region       string     `json:"region,omitempty"`
	Zone         string     `json:"zone,omitempty"`
	InstanceType string     `json:"instance_type,omitempty"`
	Placement    any        `json:"placement,omitempty"`
	Error        string     `json:"error,omitempty"`
	Log          []string   `json:"log,omitempty"`
	LogDropped   int        `json:"log_dropped,omitempty"`
}

// recorder builds an invocation's record from its reporters' events and
// warnings and its outcome, and saves it.
type recorder struct {
	mu     sync.Mutex
	rec    runRecord
	start  time.Time // monotonic, for durations
	frozen bool      // saved for good: later events are dropped
	dirty  bool      // changed since last saved
	path   string    // where it was saved
	now    func() time.Time
	// What the whole record holds so far, against the record-wide caps.
	warnings, launches, logs int
}

func newRecorder(ws *client.Workspace, task string) *recorder {
	r := &recorder{now: time.Now}
	r.start = r.now()
	r.rec = runRecord{Version: runRecordVersion, StartedAt: r.start.UTC(), PID: os.Getpid(), Task: task, Warnings: []recordWarning{}, Parts: []*recordPart{}}
	if ws != nil {
		r.rec.Worktree, r.rec.Cwd = ws.Root, ws.Cwd
		if r.rec.Cwd == "." {
			r.rec.Cwd = ""
		}
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	r.rec.ID = r.start.UTC().Format("20060102T150405.000Z") + "-" + hex.EncodeToString(b[:])
	return r
}

// reporter wraps rep so the warnings it is given are recorded as the whole
// run's.
func (r *recorder) reporter(rep ui.Reporter) ui.Reporter {
	return &recordingReporter{Reporter: rep, r: r}
}

// part starts recording one run and returns its reporter: rep, recording
// its warnings and launches.
func (r *recorder) part(rep ui.Reporter, name, command string) (*recordPart, ui.Reporter) {
	p := &recordPart{Name: name, Command: clip(command), StartedAt: r.now().UTC(), PhasesMS: map[string]int64{}, Warnings: []recordWarning{}}
	r.mu.Lock()
	if len(r.rec.Parts) < recordPartsMax {
		r.rec.Parts = append(r.rec.Parts, p)
	} else {
		r.rec.PartsDropped++
	}
	r.dirty = true
	r.mu.Unlock()
	return p, &recordingReporter{Reporter: rep, r: r, p: p}
}

// end records how part p ended: the client's result (its zero value when
// it never ran remotely), the error spotexec failed it with, its exit code
// and status.
func (r *recorder) end(p *recordPart, res client.Result, err error, code int, status string, local bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	r.dirty = true
	p.EndedAt = r.now().UTC()
	p.DurationMS = p.EndedAt.Sub(p.StartedAt).Milliseconds()
	p.Status, p.ExitCode, p.Local = status, code, local
	p.Runner, p.Slot, p.Attempts = res.Runner, res.Slot, res.Attempts
	if local {
		p.Runner, p.Slot = "", 0
	}
	p.InstanceID, p.Region, p.Zone, p.InstanceType = res.InstanceID, res.Region, res.Zone, res.InstanceType
	for k, d := range res.Phases {
		p.PhasesMS[k] = d.Milliseconds()
	}
	p.TimedOut, p.OOMKilled, p.SetupFailed = res.TimedOut, res.OOMKilled, res.SetupFailed
	if err != nil {
		p.Error = &recordError{Code: errorCode(err), Reason: res.LostCode, Message: clip(err.Error())}
	}
}

// localPhase records a part that ran locally as all command.
func (r *recorder) localPhase(p *recordPart, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.PhasesMS[client.PhaseCommand] += d.Milliseconds()
}

// finish records the invocation's exit code and saves the record. Launches
// may still report after it (settle).
func (r *recorder) finish(code int) {
	r.mu.Lock()
	end := r.now()
	r.rec.EndedAt, r.rec.ExitCode = end.UTC(), code
	r.rec.DurationMS = end.Sub(r.start).Milliseconds()
	r.dirty = true
	r.mu.Unlock()
	r.save()
}

// settle saves the record again if a launch reported since finish, and
// stops recording. Call it after the run's launches are recorded
// (waitLaunches).
func (r *recorder) settle() {
	r.mu.Lock()
	r.frozen = true
	r.mu.Unlock()
	r.save()
}

func (r *recorder) save() {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	b, err := json.MarshalIndent(&r.rec, "", "  ")
	r.dirty = false
	r.mu.Unlock()
	if err != nil {
		return
	}
	dir := runRecordDir()
	if p, err := writeRecord(dir, r.rec.ID+".json", append(b, '\n')); err == nil {
		r.mu.Lock()
		r.path = p
		r.mu.Unlock()
		pruneRunRecords(dir, runRecordsKept, runRecordsBytes)
	}
}

// Path is where the record was saved ("" when it wasn't).
func (r *recorder) Path() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path
}

func (r *recorder) warn(p *recordPart, code, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	r.dirty = true
	w := recordWarning{At: r.now().UTC(), Code: clip(code), Message: clip(msg)}
	list, dropped := &r.rec.Warnings, &r.rec.WarningsDropped
	if p != nil {
		list, dropped = &p.Warnings, &p.WarningsDropped
	}
	if len(*list) >= recordWarnMax || r.warnings >= recordWarnTotal {
		*dropped++
		return
	}
	*list = append(*list, w)
	r.warnings++
}

// event records the launch events of part p's run; every other event is
// left to the reporter.
func (r *recorder) event(p *recordPart, name string, f map[string]any) {
	switch name {
	case "scale_out", "provision", "instance_launched", "instance_running", "launch_failed":
	default:
		return
	}
	id, _ := f["launch_id"].(string)
	if p == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	now := r.now().UTC()
	l, seen := p.launchIdx[id]
	if !seen {
		if p.launchIdx == nil {
			p.launchIdx = map[string]*recordLaunch{}
		}
		if len(p.Launches) >= recordLaunchMax || r.launches >= recordLaunchTotal {
			p.launchIdx[id] = nil
			p.LaunchesDropped++
			r.dirty = true
			return
		}
		r.launches++
		runner, _ := f["runner"].(string)
		l = &recordLaunch{LaunchID: clip(id), Runner: clip(runner), Status: "requested", RequestedAt: now}
		p.launchIdx[id] = l
		p.Launches = append(p.Launches, l)
	}
	if l == nil {
		return // left out
	}
	r.dirty = true
	switch name {
	case "scale_out":
		if n, ok := f["demand"].(int); ok {
			l.Demand = &n
		}
		if n, ok := f["instances"].(int); ok {
			l.Instances = &n
		}
	case "provision":
		msg, _ := f["message"].(string)
		if len(l.Log) < recordLogMax && r.logs < recordLogTotal {
			l.Log = append(l.Log, clip(msg))
			r.logs++
		} else {
			l.LogDropped++
		}
	case "instance_launched", "instance_running":
		if name == "instance_launched" {
			l.LaunchedAt = &now
			if l.Status != "running" {
				l.Status = "launched"
			}
		} else {
			l.RunningAt, l.Status = &now, "running"
		}
		if s, _ := f["instance_id"].(string); s != "" {
			l.InstanceID = clip(s)
		}
		if s, _ := f["region"].(string); s != "" {
			l.Region = clip(s)
		}
		if s, _ := f["zone"].(string); s != "" {
			l.Zone = clip(s)
		}
		if s, _ := f["type"].(string); s != "" {
			l.InstanceType = clip(s)
		}
		if pl, ok := f["placement"]; ok && pl != nil {
			l.Placement = pl
		}
	case "launch_failed":
		msg, _ := f["message"].(string)
		l.Status, l.Error = "failed", clip(msg)
	}
}

// recordingReporter passes everything on to its Reporter, recording the
// warnings and launch events of its part (the whole run's warnings when p
// is nil).
type recordingReporter struct {
	ui.Reporter
	r *recorder
	p *recordPart
}

func (rr *recordingReporter) Event(name string, fields map[string]any) {
	rr.r.event(rr.p, name, fields)
	rr.Reporter.Event(name, fields)
}

func (rr *recordingReporter) Warn(code, format string, args ...any) {
	kept := make([]any, len(args))
	for i, a := range args {
		if _, ok := a.(outputText); ok {
			a = ""
		}
		kept[i] = a
	}
	rr.r.warn(rr.p, code, fmt.Sprintf(format, kept...))
	rr.Reporter.Warn(code, format, args...)
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)

// clip makes s safe and short for a record: no bootstrap log lines, escape
// sequences or other control characters (a newline becomes a space), valid
// UTF-8, at most recordTextMax bytes.
func clip(s string) string {
	// A failed bootstrap_script's last lines (the project's output) follow
	// this in its error, wherever that is quoted: a fallback's warning, the
	// errors of several runners.
	if i := strings.Index(s, protocol.BootTailMark); i >= 0 {
		s = s[:i] + "its last lines aren't recorded"
	}
	s = ansiSeq.ReplaceAllString(s, "")
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(c rune) rune {
		switch {
		case c == '\n' || c == '\t':
			return ' '
		case c < 0x20 || c == 0x7f:
			return -1
		}
		return c
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= recordTextMax {
		return s
	}
	cut := recordTextMax - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// writeRecord writes b to dir/name in one step (a temporary file renamed
// into place), so a reader never sees half a record.
func writeRecord(dir, name string, b []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, recordTmpPrefix+"*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	path := filepath.Join(dir, name)
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return "", werr
	}
	return path, nil
}

// pruneRunRecords keeps the newest keep records in dir, and no more of them
// than fit in maxBytes (records are named by their UTC start, so they sort
// by age). It also removes temporary files a killed process left over an
// hour ago.
func pruneRunRecords(dir string, keep int, maxBytes int64) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var recs []os.DirEntry
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		switch n := e.Name(); {
		case strings.HasPrefix(n, recordTmpPrefix):
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(dir, n))
			}
		case strings.HasSuffix(n, ".json"):
			recs = append(recs, e)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name() > recs[j].Name() }) // newest first
	var total int64
	for i, e := range recs {
		fi, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		total += fi.Size()
		if i >= keep || (total > maxBytes && i > 0) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
	}
}
