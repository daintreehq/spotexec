package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// TestMain keeps the runs tests make from leaving records in this
// machine's own state directory: a test that doesn't give the CLI a state
// directory of its own records into a temporary one.
func TestMain(m *testing.M) {
	real := config.StateDir()
	tmp, err := os.MkdirTemp("", "spotexec-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	def := runRecordDir
	runRecordDir = func() string {
		if config.StateDir() == real {
			return filepath.Join(tmp, "logs", "runs")
		}
		return def()
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// records reads every run record under state.
func records(t *testing.T, state string) ([]runRecord, []string) {
	t.Helper()
	dir := filepath.Join(state, "logs", "runs")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("no run records: %v", err)
	}
	var recs []runRecord
	var raw []string
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("stray file in the records: %s", e.Name())
		}
		var r runRecord
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if fi, _ := e.Info(); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", e.Name(), fi.Mode().Perm())
		}
		recs = append(recs, r)
		raw = append(raw, string(b))
	}
	return recs, raw
}

func hasWarning(ws []recordWarning, code string) bool {
	for _, w := range ws {
		if w.Code == code && w.Message != "" && !w.At.IsZero() {
			return true
		}
	}
	return false
}

func TestRunLeavesARecord(t *testing.T) {
	dir := project(t, "")
	// A runner that refuses connections at once.
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())

	// Run locally after the runner refuses: the command's output (made at
	// run time, so it isn't in the command line) never reaches the record.
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	before := time.Now().UTC()
	code, stderr := runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "printf 'OUT%sPUT\\n' SECRET; echo ERR$((6*7)) >&2; exit 7")
	if code != 7 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	recs, raw := records(t, state)
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Version != runRecordVersion || r.ID == "" || r.ExitCode != 7 || r.PID != os.Getpid() || r.Task != "" {
		t.Errorf("record: %+v", r)
	}
	if r.StartedAt.Before(before.Add(-time.Second)) || r.EndedAt.Before(r.StartedAt) || r.Worktree == "" {
		t.Errorf("times or worktree: %+v", r)
	}
	if len(r.Parts) != 1 {
		t.Fatalf("parts: %+v", r.Parts)
	}
	p := r.Parts[0]
	if p.Status != "failed" || p.ExitCode != 7 || !p.Local || p.Runner != "" || p.Error != nil || !strings.Contains(p.Command, "exit 7") {
		t.Errorf("part: %+v", p)
	}
	if !hasWarning(p.Warnings, "LOCAL_FALLBACK") {
		t.Errorf("warnings: %+v", p.Warnings)
	}
	if _, ok := p.PhasesMS[client.PhaseCommand]; !ok {
		t.Errorf("phases: %v", p.PhasesMS)
	}
	for _, out := range []string{"OUTSECRETPUT", "ERR42"} {
		if strings.Contains(raw[0], out) {
			t.Errorf("the record holds the command's output %q:\n%s", out, raw[0])
		}
	}

	// A command that exits 130 itself failed: nobody interrupted it.
	state = t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	if code, stderr = runMain(t, "sx", "-C", dir, "--fallback-local", "--queue-timeout", "5s", "exit 130"); code != 130 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if recs, _ = records(t, state); len(recs) != 1 || recs[0].Parts[0].Status != "failed" || recs[0].Parts[0].ExitCode != 130 {
		t.Errorf("exit 130: %+v", recs[0].Parts[0])
	}

	// Without the fallback spotexec fails the run (125), and says why.
	state = t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	code, stderr = runMain(t, "sx", "-C", dir, "--queue-timeout", "5s", "true")
	if code != client.ExitInfra {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	recs, _ = records(t, state)
	if len(recs) != 1 || len(recs[0].Parts) != 1 {
		t.Fatalf("records: %+v", recs)
	}
	p = recs[0].Parts[0]
	if recs[0].ExitCode != client.ExitInfra || p.Status != "failed" || p.ExitCode != client.ExitInfra || p.Local {
		t.Errorf("failed run: %+v / %+v", recs[0], p)
	}
	if p.Error == nil || p.Error.Code != "SPOTEXEC_ERROR" || !strings.Contains(p.Error.Message, "no runner available") {
		t.Errorf("error: %+v", p.Error)
	}
	if _, ok := p.PhasesMS[client.PhaseQueued]; !ok || p.Attempts != 1 {
		t.Errorf("phases %v, attempts %d", p.PhasesMS, p.Attempts)
	}
}

func TestParallelTaskLeavesOneRecord(t *testing.T) {
	dir := project(t, "[tasks.l]\nrun = \"true\"\n[tasks.t]\nrun = \"true\"\n[tasks.v]\nparallel = [\"l\", \"t\"]\n")
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("version = 1\n[runners.a]\ntype = \"ssh\"\nhosts = [\"nobody@127.0.0.1\"]\nssh_options = [\"Port=1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_CONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	code, stderr := runMain(t, "sx", "-C", dir, "--queue-timeout", "5s", "v")
	if code != client.ExitInfra {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	recs, _ := records(t, state)
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Task != "v" || r.ExitCode != client.ExitInfra || len(r.Parts) != 2 {
		t.Fatalf("record: %+v", r)
	}
	names := map[string]bool{}
	for _, p := range r.Parts {
		names[p.Name] = true
		if p.Status != "failed" || p.ExitCode != client.ExitInfra || p.Error == nil || p.EndedAt.IsZero() {
			t.Errorf("part: %+v", p)
		}
	}
	if !names["l"] || !names["t"] {
		t.Errorf("parts: %v", names)
	}
}

// fixedClock steps a second each call.
func fixedClock() func() time.Time {
	t := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func TestRecorderLaunchesAndOutcome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	r := newRecorder(&client.Workspace{Root: "/w", Cwd: "sub"}, "")
	r.now = fixedClock()
	base, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	p, rep := r.part(base, "", "go test ./x")
	rep.Event("scale_out", map[string]any{"runner": "aws", "launch_id": "L1", "demand": 3, "instances": 2})
	rep.Event("provision", map[string]any{"runner": "aws", "launch_id": "L1", "message": "skipping c7i.large in ap-south-1a: InsufficientInstanceCapacity"})
	rep.Event("instance_launched", map[string]any{"runner": "aws", "launch_id": "L1", "instance_id": "i-1", "region": "ap-south-1", "zone": "ap-south-1b", "type": "c7i.large",
		"placement": &provider.Placement{Order: "run_cost", Rank: 2, Of: 5, Why: "first choice had no capacity"}})
	rep.Event("scale_out", map[string]any{"runner": "aws", "launch_id": "L2", "demand": 4, "instances": 3})
	rep.Event("launch_failed", map[string]any{"runner": "aws", "launch_id": "L2", "message": "quota"})
	// Events that aren't about launches, or carry output, are left out.
	rep.Event("deps_failed", map[string]any{"output_tail": "SECRET-TAIL"})
	rep.Event("provision", map[string]any{"runner": "aws", "message": "no launch id"})
	rep.Warn("IMAGE_REGISTRY", "can't reach %s; building locally", "ecr")
	res := client.Result{Runner: "aws", Slot: 3, InstanceID: "i-0", Region: "ap-south-1", Zone: "ap-south-1a", InstanceType: "c7i.xlarge", Attempts: 1, LostCode: "RUNNER_UNRESPONSIVE",
		Phases: map[string]time.Duration{client.PhaseQueued: 2 * time.Second, client.PhaseCommand: 1500 * time.Millisecond}}
	r.end(p, res, errors.New("lost runner aws during execution: EOF"), client.ExitInfra, partFailed, false)
	r.finish(client.ExitInfra)
	// A launch that reports while the run waits for it lands in the record.
	rep.Event("instance_running", map[string]any{"runner": "aws", "launch_id": "L1", "instance_id": "i-1"})
	r.settle()
	rep.Warn("LATE", "after the record was settled")

	recs, raw := records(t, state)
	if len(recs) != 1 || r.Path() == "" {
		t.Fatalf("records: %d, path %q", len(recs), r.Path())
	}
	if strings.Contains(raw[0], "SECRET-TAIL") || strings.Contains(raw[0], "LATE") {
		t.Errorf("record holds what it shouldn't:\n%s", raw[0])
	}
	got := recs[0]
	if got.Worktree != "/w" || got.Cwd != "sub" || got.ExitCode != client.ExitInfra {
		t.Errorf("record: %+v", got)
	}
	pt := got.Parts[0]
	if pt.Runner != "aws" || pt.Slot != 3 || pt.InstanceID != "i-0" || pt.Region != "ap-south-1" || pt.Zone != "ap-south-1a" || pt.InstanceType != "c7i.xlarge" {
		t.Errorf("where: %+v", pt)
	}
	if pt.PhasesMS[client.PhaseQueued] != 2000 || pt.PhasesMS[client.PhaseCommand] != 1500 || pt.Attempts != 1 {
		t.Errorf("phases %v attempts %d", pt.PhasesMS, pt.Attempts)
	}
	if pt.Error == nil || pt.Error.Code != "SPOTEXEC_ERROR" || pt.Error.Reason != "RUNNER_UNRESPONSIVE" {
		t.Errorf("error: %+v", pt.Error)
	}
	if !hasWarning(pt.Warnings, "IMAGE_REGISTRY") || len(pt.Warnings) != 1 {
		t.Errorf("warnings: %+v", pt.Warnings)
	}
	if len(pt.Launches) != 2 {
		t.Fatalf("launches: %+v", pt.Launches)
	}
	l1, l2 := pt.Launches[0], pt.Launches[1]
	if l1.LaunchID != "L1" || l1.Status != "running" || l1.InstanceID != "i-1" || l1.Region != "ap-south-1" || l1.Zone != "ap-south-1b" || l1.InstanceType != "c7i.large" ||
		l1.Demand == nil || *l1.Demand != 3 || l1.Instances == nil || *l1.Instances != 2 || l1.LaunchedAt == nil || l1.RunningAt == nil || l1.Placement == nil ||
		len(l1.Log) != 1 || !strings.Contains(l1.Log[0], "InsufficientInstanceCapacity") {
		t.Errorf("launch 1: %+v", l1)
	}
	if l2.LaunchID != "L2" || l2.Status != "failed" || l2.Error != "quota" || l2.InstanceID != "" {
		t.Errorf("launch 2: %+v", l2)
	}
}

func TestRecorderBounds(t *testing.T) {
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	r := newRecorder(nil, "")
	base, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	p, rep := r.part(base, "", strings.Repeat("x", 5000))
	for i := 0; i < recordWarnMax+5; i++ {
		rep.Warn("W", "%s", strings.Repeat("é", 2000))
	}
	for i := 0; i < recordLaunchMax+3; i++ {
		rep.Event("scale_out", map[string]any{"runner": "aws", "launch_id": fmt.Sprint("L", i)})
	}
	for i := 0; i < recordLogMax+2; i++ {
		rep.Event("provision", map[string]any{"runner": "aws", "launch_id": "L0", "message": "m"})
	}
	r.end(p, client.Result{}, nil, 0, partPassed, false)
	r.finish(0)
	r.settle()
	recs, raw := records(t, state)
	pt := recs[0].Parts[0]
	if len(pt.Warnings) != recordWarnMax || pt.WarningsDropped != 5 || len(pt.Launches) != recordLaunchMax || pt.LaunchesDropped != 3 ||
		len(pt.Launches[0].Log) != recordLogMax || pt.Launches[0].LogDropped != 2 {
		t.Errorf("caps: %d warnings (+%d), %d launches (+%d), %d log (+%d)", len(pt.Warnings), pt.WarningsDropped, len(pt.Launches), pt.LaunchesDropped, len(pt.Launches[0].Log), pt.Launches[0].LogDropped)
	}
	if len(pt.Command) > recordTextMax || len(pt.Warnings[0].Message) > recordTextMax || !strings.HasSuffix(pt.Warnings[0].Message, "…") {
		t.Errorf("text not cut: command %d bytes, warning %d bytes", len(pt.Command), len(pt.Warnings[0].Message))
	}
	if len(raw[0]) > 256<<10 {
		t.Errorf("record is %d bytes", len(raw[0]))
	}
}

func TestClip(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[31mred\x1b[0m":     "red",
		"a\nb\tc\r":              "a b c",
		"bell\x07 \x1b]0;t\x07x": "bell x",
		"bad \xff utf8":          "bad \uFFFD utf8",
	} {
		if got := clip(in); got != want {
			t.Errorf("clip(%q) = %q, want %q", in, got, want)
		}
	}
	long := clip(strings.Repeat("日", 1000))
	if len(long) > recordTextMax || !strings.HasSuffix(long, "…") || !strings.HasPrefix(long, "日") {
		t.Errorf("long: %d bytes", len(long))
	}
	if s := strings.TrimSuffix(long, "…"); !json.Valid([]byte(fmt.Sprintf("%q", s))) {
		t.Error("cut mid-rune")
	}
}

func TestPruneRunRecords(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("20261116T1000%02d.000Z-x.json", i)), make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTmp := filepath.Join(dir, recordTmpPrefix+"old")
	newTmp := filepath.Join(dir, recordTmpPrefix+"new")
	for _, f := range []string{oldTmp, newTmp} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldTmp, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	left := func() []string {
		ents, _ := os.ReadDir(dir)
		var n []string
		for _, e := range ents {
			n = append(n, e.Name())
		}
		return n
	}
	pruneRunRecords(dir, 6, 1<<20)
	got := left()
	if len(got) != 7 || got[0] != recordTmpPrefix+"new" || got[1] != "20261116T100004.000Z-x.json" {
		t.Fatalf("by count: %v", got)
	}
	// The byte cap keeps the newest that fit, and always the newest one.
	pruneRunRecords(dir, 6, 250)
	if got := left(); len(got) != 3 || got[1] != "20261116T100008.000Z-x.json" {
		t.Fatalf("by bytes: %v", got)
	}
	pruneRunRecords(dir, 6, 10)
	if got := left(); len(got) != 2 || got[1] != "20261116T100009.000Z-x.json" {
		t.Fatalf("newest kept: %v", got)
	}
}

func TestUnwritableRecordDirIsSilent(t *testing.T) {
	state := t.TempDir()
	// A file where the records' directory should be.
	if err := os.MkdirAll(filepath.Join(state, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "logs", "runs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	r := newRecorder(nil, "")
	base, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	p, _ := r.part(base, "", "true")
	r.end(p, client.Result{}, nil, 0, partPassed, false)
	r.finish(0)
	r.settle()
	if r.Path() != "" {
		t.Errorf("saved to %s", r.Path())
	}
}

// captureWarn keeps the warnings a reporter is given, as it would show them.
type captureWarn struct {
	ui.Reporter
	shown []string
}

func (c *captureWarn) Warn(code, format string, args ...any) {
	c.shown = append(c.shown, code+": "+fmt.Sprintf(format, args...))
}

func TestRecordLeavesOutOtherProgramsOutput(t *testing.T) {
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	r := newRecorder(nil, "")
	quiet, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	base := &captureWarn{Reporter: quiet}
	p, rep := r.part(base, "", "go test -json ./...")
	// What RTK printed is shown, never recorded.
	rep.Warn("RTK_FILTER", "rtk pipe -f %s failed (%v%s); the output below is unfiltered", "go-test", errors.New("exit status 1"), outputText(": --- FAIL: TestSecret"))
	// A failed bootstrap's log lines are the project's output.
	boot := &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: protocol.CodeBootFailed, Message: "bootstrap_script failed (exit 2) on this VM; last lines of /var/log/spotexec-bootstrap.log:\nSECRET-BOOT-LINE"}}
	// Quoted by a fallback's warning too, among other runners' errors.
	rep.Warn("LOCAL_FALLBACK", "no runner available (%v); running locally", fmt.Errorf("no runner available:\n  aws: %w\n  box: refused", boot))
	r.end(p, client.Result{}, boot, client.ExitInfra, partFailed, false)
	r.finish(client.ExitInfra)
	r.settle()
	if len(base.shown) != 2 || !strings.Contains(base.shown[0], "--- FAIL: TestSecret") || !strings.Contains(base.shown[1], "SECRET-BOOT-LINE") {
		t.Errorf("shown: %q", base.shown)
	}
	recs, raw := records(t, state)
	if strings.Contains(raw[0], "TestSecret") || strings.Contains(raw[0], "SECRET-BOOT-LINE") {
		t.Errorf("record holds another program's output:\n%s", raw[0])
	}
	pt := recs[0].Parts[0]
	if len(pt.Warnings) != 2 || pt.Warnings[0].Message != "rtk pipe -f go-test failed (exit status 1); the output below is unfiltered" ||
		!strings.HasSuffix(pt.Warnings[1].Message, "its last lines aren't recorded") {
		t.Errorf("warning: %+v", pt.Warnings)
	}
	if pt.Error == nil || pt.Error.Code != protocol.CodeBootFailed || pt.Error.Message != "BOOTSTRAP_FAILED: bootstrap_script failed (exit 2) on this VM; its last lines aren't recorded" {
		t.Errorf("error: %+v", pt.Error)
	}
}

func TestRecordWideCaps(t *testing.T) {
	state := t.TempDir()
	t.Setenv("SPOTEXEC_STATE_DIR", state)
	r := newRecorder(nil, "t")
	base, _ := ui.New(ui.ModeQuiet, os.Stderr, "")
	var parts []*recordPart
	for i := 0; i < 10; i++ {
		p, rep := r.part(base, fmt.Sprint(i), "true")
		parts = append(parts, p)
		for j := 0; j < recordWarnMax; j++ {
			rep.Warn("W", "w")
		}
		for j := 0; j < recordLaunchMax; j++ {
			id := fmt.Sprint("L", i, "-", j)
			rep.Event("scale_out", map[string]any{"runner": "aws", "launch_id": id})
			for k := 0; k < 5; k++ {
				rep.Event("provision", map[string]any{"runner": "aws", "launch_id": id, "message": "m"})
			}
		}
	}
	for _, p := range parts {
		r.end(p, client.Result{}, nil, 0, partPassed, false)
	}
	r.finish(0)
	r.settle()
	recs, _ := records(t, state)
	var warns, dropped, launches, ldropped, logs, logDropped int
	for _, p := range recs[0].Parts {
		warns, dropped = warns+len(p.Warnings), dropped+p.WarningsDropped
		launches, ldropped = launches+len(p.Launches), ldropped+p.LaunchesDropped
		for _, l := range p.Launches {
			logs, logDropped = logs+len(l.Log), logDropped+l.LogDropped
		}
	}
	if warns != recordWarnTotal || warns+dropped != 10*recordWarnMax {
		t.Errorf("warnings: %d kept, %d dropped", warns, dropped)
	}
	if launches != recordLaunchTotal || launches+ldropped != 10*recordLaunchMax {
		t.Errorf("launches: %d kept, %d dropped", launches, ldropped)
	}
	if logs != recordLogTotal || logs+logDropped != 5*recordLaunchTotal {
		t.Errorf("launch logs: %d kept, %d dropped", logs, logDropped)
	}
}
