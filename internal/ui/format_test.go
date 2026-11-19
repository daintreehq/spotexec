package ui

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Every NDJSON event says which format it is in, whichever reporter wrote
// it: the JSON stream and every mode's events file, warnings included.
func TestEveryEventCarriesFormatVersion(t *testing.T) {
	var jsonOnly, jsonOut, jsonExtra, prettyExtra, plainExtra, quietExtra bytes.Buffer
	reporters := map[string]Reporter{
		"json alone": &jsonReporter{w: &jsonOnly},
		"json":       &jsonReporter{w: &jsonOut, extra: &jsonExtra},
		"pretty":     &prettyReporter{w: io.Discard, extra: &prettyExtra},
		"plain":      &plainReporter{w: io.Discard, extra: &plainExtra},
		"quiet":      &quietReporter{&plainReporter{w: io.Discard, extra: &quietExtra}},
	}
	for _, r := range reporters {
		// The reserved names a caller passes are dropped, not written.
		r.Event("slot_acquired", map[string]any{"runner": "aws", "slot": 3, "event": "other", "format_version": 99})
		r.Warn("SYNC_CONFLICT", "kept %s", "a.go")
	}
	for name, buf := range map[string]*bytes.Buffer{
		"json stream alone": &jsonOnly, "json stream": &jsonOut, "json events file": &jsonExtra,
		"pretty events file": &prettyExtra, "plain events file": &plainExtra, "quiet events file": &quietExtra,
	} {
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("%s: got %d lines, want 2: %q", name, len(lines), buf.String())
		}
		for _, l := range lines {
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("%s: bad line %q: %v", name, l, err)
			}
			if m["format_version"] != float64(EventsFormat) {
				t.Errorf("%s: format_version = %v in %s, want %d", name, m["format_version"], l, EventsFormat)
			}
		}
		if want := `{"event":"slot_acquired","format_version":1,"runner":"aws","slot":3}`; lines[0] != want {
			t.Errorf("%s: event = %s, want %s", name, lines[0], want)
		}
		if want := `{"event":"warning","format_version":1,"code":"SYNC_CONFLICT","message":"kept a.go"}`; lines[1] != want {
			t.Errorf("%s: warning = %s, want %s", name, lines[1], want)
		}
	}
}

// A caller can't misstate the event's name or its format.
func TestEventFieldsCantOverrideEventOrFormat(t *testing.T) {
	got := strings.TrimSuffix(string(eventLine("result", map[string]any{"event": "other", "format_version": 99, "exit_code": 0})), "\n")
	if want := `{"event":"result","format_version":1,"exit_code":0}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
