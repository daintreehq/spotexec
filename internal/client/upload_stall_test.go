package client

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
)

// A sync whose upload crawls while the helper keeps answering (#265) ends
// within the stall window as a stalled upload, not a lost runner: the run
// retries on a fresh connection without blaming the VM.
func TestSyncEndsOnCrawlingUpload(t *testing.T) {
	r, tgt := liveRunner(t, "rpc-crawl")
	shorten(t, &transport.UploadStallWindow, time.Second)
	_, s, err := Connect(context.Background(), r, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	// A manifest of a few MiB: the upload stalls inside its one frame.
	m := &manifest.Manifest{ByPath: map[string]protocol.Entry{}}
	for i := range 40000 {
		e := protocol.Entry{Path: fmt.Sprintf("src/pkg%05d/file.go", i), Size: 1, ID: strings.Repeat("a", 40)}
		m.Entries = append(m.Entries, e)
		m.ByPath[e.Path] = e
	}
	done := make(chan error, 1)
	go func() {
		_, err := syncIn(context.Background(), s, m, nil, nil, Options{Rep: quietRep(t)})
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("sync still waiting on a crawling upload")
	}
	l := &Lease{Sess: s}
	if err := uploadStall(l); err == nil || !strings.Contains(err.Error(), "upload to the runner stalled") {
		t.Fatalf("want a stalled upload, got %v", err)
	}
}

// A silent helper is a lost runner, not a stalled upload.
func TestSilentHelperIsNotUploadStall(t *testing.T) {
	r, tgt := liveRunner(t, "rpc-silent")
	_, s, err := Connect(context.Background(), r, tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort()
	_, _ = s.Expect(context.Background(), protocol.TypeAcquired)
	if err := uploadStall(&Lease{Sess: s}); err != nil {
		t.Fatalf("a silent helper read as a stalled upload: %v", err)
	}
}

func TestStallRetry(t *testing.T) {
	base := Options{RetryPreempt: true}
	for _, tc := range []struct {
		name     string
		o        Options
		canceled bool
		n        int
		want     bool
	}{
		{"first attempt", base, false, 1, true},
		{"already retried", base, false, 2, false},
		{"--no-retry", Options{}, false, 1, false},
		{"canceled", base, true, 1, false},
		{"--same-slot", Options{RetryPreempt: true, SameSlot: true}, false, 1, false},
		{"prefer last", Options{RetryPreempt: true, PreferLast: true}, false, 1, false},
	} {
		if got := stallRetry(tc.o, tc.canceled, tc.n); got != tc.want {
			t.Errorf("%s: retry %v, want %v", tc.name, got, tc.want)
		}
	}
}
