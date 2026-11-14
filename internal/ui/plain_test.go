package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestPlainReporterIsSparse(t *testing.T) {
	old := plainDelay
	plainDelay = 50 * time.Millisecond
	defer func() { plainDelay = old }()
	var w syncBuf
	p := &plainReporter{w: &w}
	// A quick phase is never mentioned.
	p.Status("aws#1 · checking dependency cache")
	p.Status("aws#1 · running pnpm test")
	p.Clear()
	time.Sleep(120 * time.Millisecond)
	// A slow phase whose counter ticks is mentioned once, with its latest text.
	for i := 1; i <= 5; i++ {
		p.Status("queued 2nd %d.0s · 4 slots busy", i)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	p.Warn("X", "careful")
	p.Done("4.1s on aws#3 · exit 0", true)
	got := w.String()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "sx: queued 2nd ") || !strings.HasSuffix(lines[0], "s · 4 slots busy") ||
		lines[1] != "sx: warning: careful" || lines[2] != "sx: 4.1s on aws#3 · exit 0" {
		t.Fatalf("got:\n%s", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("ANSI in plain output")
	}
}
