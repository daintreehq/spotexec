package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
)

func TestTaskRecap(t *testing.T) {
	shard := func(i int, status string, code int, runner string, slot int) partOutcome {
		pt := taskPart{name: "unit", label: fmt.Sprintf("unit %d/4", i), shard: i, shards: 4}
		return partOutcome{part: pt, status: status, code: code, took: 90 * time.Second, runner: runner, slot: slot}
	}
	outs := []partOutcome{
		shard(1, partPassed, 0, "aws", 4),
		shard(2, partFailed, 1, "aws", 2),
		shard(3, partStopped, 130, "aws", 0),
		shard(4, partFailed, 125, "", 0),
		shard(5, partInterrupted, 130, "aws", 5),
	}
	var got []string
	for _, o := range recapOrder(outs) {
		got = append(got, o.recapLine())
	}
	want := []string{
		"unit 2/4 · failed · 1m30s on aws#2 · exit 1",
		"unit 4/4 · failed · 1m30s · exit 125",
		"unit 5/4 · interrupted · 1m30s on aws#5 · exit 130",
		"unit 3/4 · stopped (fail_fast) · 1m30s on aws#0",
		"unit 1/4 · passed · 1m30s on aws#4",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("recap:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v := taskVerdict("unit", outs, 125, false, 12*time.Minute); v != "unit · 2 of 5 parts failed (unit 2/4, unit 4/4) · 1 stopped · 12m · exit 125" {
		t.Errorf("verdict: %q", v)
	}

	// Remote slot 0 survives, a part that never reached a runner claims
	// none, and a local fallback says so.
	local := shard(1, partPassed, 0, "", 0)
	local.local = true
	b, _ := json.Marshal(resultParts([]partOutcome{outs[2], outs[3], local}))
	if want := `[{"conflict_count":0,"duration_ms":90000,"exit_code":130,"label":"unit 3/4","runner":"aws","shard":3,"shards":4,"slot":0,"status":"stopped","task":"unit"},` +
		`{"conflict_count":0,"duration_ms":90000,"exit_code":125,"label":"unit 4/4","shard":4,"shards":4,"status":"failed","task":"unit"},` +
		`{"conflict_count":0,"duration_ms":90000,"exit_code":0,"label":"unit 1/4","local":true,"shard":1,"shards":4,"status":"passed","task":"unit"}]`; string(b) != want {
		t.Errorf("parts:\n%s\nwant:\n%s", b, want)
	}
}

func TestTaskVerdict(t *testing.T) {
	part := func(label, status string) partOutcome {
		return partOutcome{part: taskPart{name: label, label: label}, status: status}
	}
	many := []partOutcome{part("a", partFailed), part("b", partFailed), part("c", partFailed), part("d", partFailed), part("e", partFailed), part("f", partPassed)}
	for _, c := range []struct {
		outs        []partOutcome
		exit        int
		interrupted bool
		want        string
	}{
		{[]partOutcome{part("lint", partPassed), part("test", partPassed)}, 0, false, "all · 2 parts passed · 5.0s"},
		{[]partOutcome{part("lint", partPassed)}, 0, false, "all · 1 part passed · 5.0s"},
		{many, 1, false, "all · 5 of 6 parts failed (a, b, c, d, 1 more) · 5.0s · exit 1"},
		{[]partOutcome{part("lint", partPassed), part("test", partInterrupted)}, 130, true, "all · interrupted · 5.0s · exit 130"},
		{[]partOutcome{part("lint", partFailed), part("test", partInterrupted)}, 130, true, "all · interrupted · 1 of 2 parts failed (lint) · 5.0s · exit 130"},
	} {
		if got := taskVerdict("all", c.outs, c.exit, c.interrupted, 5*time.Second); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

// A part whose write-back conflicted says so on its own row, in the verdict
// and in the result event, whether or not the conflict failed it.
func TestTaskRecapConflicts(t *testing.T) {
	part := func(label, status string, code, conflicts int) partOutcome {
		return partOutcome{part: taskPart{name: label, label: label}, status: status, code: code, took: 2 * time.Second, runner: "aws", slot: 1, conflicts: conflicts}
	}
	outs := []partOutcome{
		part("fmt", partFailed, client.ExitWritebackConflict, 2),
		part("lint", partPassed, 0, 1),
		part("test", partPassed, 0, 0),
	}
	var got []string
	for _, o := range recapOrder(outs) {
		got = append(got, o.recapLine())
	}
	want := []string{
		"fmt · failed · 2.0s on aws#1 · 2 conflicts · exit 122",
		"lint · passed · 2.0s on aws#1 · 1 conflict",
		"test · passed · 2.0s on aws#1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("recap:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v := taskVerdict("all", outs, 122, false, 5*time.Second); v != "all · 1 of 3 parts failed (fmt) · 3 conflicts · 5.0s · exit 122" {
		t.Errorf("verdict: %q", v)
	}
	if n := totalConflicts(outs); n != 3 {
		t.Errorf("total conflicts %d, want 3", n)
	}
	b, _ := json.Marshal(resultParts(outs[1:2]))
	if !strings.Contains(string(b), `"conflict_count":1`) {
		t.Errorf("part result lacks its conflict count: %s", b)
	}
}

func TestTaskExit(t *testing.T) {
	part := func(status string, code int) partOutcome { return partOutcome{status: status, code: code} }
	for _, c := range []struct {
		name        string
		outs        []partOutcome
		interrupted bool
		want        int
	}{
		{"all passed", []partOutcome{part(partPassed, 0), part(partPassed, 0)}, false, 0},
		{"first failure in task order", []partOutcome{part(partFailed, 2), part(partFailed, 1)}, false, 2},
		{"a conflict-only failure counts", []partOutcome{part(partPassed, 0), part(partFailed, client.ExitWritebackConflict)}, false, client.ExitWritebackConflict},
		{"125 outranks an earlier failure", []partOutcome{part(partFailed, client.ExitWritebackConflict), part(partFailed, client.ExitInfra)}, false, client.ExitInfra},
		{"125 stays over a later failure", []partOutcome{part(partFailed, client.ExitInfra), part(partFailed, 1)}, false, client.ExitInfra},
		{"stopped parts don't count", []partOutcome{part(partStopped, 130), part(partPassed, 0)}, false, 0},
		{"interrupted wins", []partOutcome{part(partFailed, client.ExitInfra)}, true, client.ExitInterrupted},
	} {
		if got := taskExit(c.outs, c.interrupted); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
