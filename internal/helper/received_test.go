package helper

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// Heartbeats say how much of what the client sent has arrived, and how long
// the helper left it unread with its frame queue full (#265): the client
// must not take that time for its connection stalling.
func TestHeartbeatReportsReceivedAndHeld(t *testing.T) {
	s := newSession()
	pr, pw := io.Pipe()
	defer pw.Close()
	s.connect(pr, io.Discard)
	go s.readLoop()
	client := protocol.NewConn(strings.NewReader(""), pw)
	frame := make([]byte, 1000)
	written := int64(0)
	for range cap(s.frames) + 1 { // one more than the queue holds
		if err := client.WriteFrame(protocol.KindBlob, frame); err != nil {
			t.Fatal(err)
		}
		written += 5 + int64(len(frame))
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.holding.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	var hb protocol.Heartbeat
	s.beat(&hb)
	if hb.Received != written {
		t.Fatalf("received %d, want %d", hb.Received, written)
	}
	if hb.HeldMS < 20 {
		t.Fatalf("held %dms while the queue was full, want the wait so far", hb.HeldMS)
	}
	<-s.frames // dispatch takes one: the read loop goes on
	for s.holding.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var after protocol.Heartbeat
	s.beat(&after)
	if after.HeldMS < hb.HeldMS {
		t.Fatalf("held %dms after the wait, less than the %dms during it", after.HeldMS, hb.HeldMS)
	}
}
