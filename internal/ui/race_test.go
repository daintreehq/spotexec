package ui

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

// prettyReporter's --events-file writes come from several goroutines (e.g.
// EnsureSpare beside pumpExec); extra is a plain file, so each line must be
// written whole under the reporter's lock. bytes.Buffer isn't goroutine-safe,
// so -race flags any unlocked write.
func TestPrettyReporterEventsFileConcurrent(t *testing.T) {
	var extra bytes.Buffer
	p := &prettyReporter{w: io.Discard, extra: &extra}
	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func() { defer wg.Done(); p.Event("tick", map[string]any{"i": i}) }()
		go func() { defer wg.Done(); p.Warn("w", "warning %d", i) }()
	}
	wg.Wait()
	p.Done("ok", true)
	lines := strings.Split(strings.TrimSuffix(extra.String(), "\n"), "\n")
	if len(lines) != 2*n {
		t.Fatalf("got %d event lines, want %d", len(lines), 2*n)
	}
	count := map[string]int{}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad event line %q: %v", l, err)
		}
		count[m["event"].(string)]++
	}
	if count["tick"] != n || count["warning"] != n {
		t.Fatalf("event counts = %v, want %d of each", count, n)
	}
}
