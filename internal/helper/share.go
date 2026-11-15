package helper

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// transfer is a detached upload or push (cache-put, image-push): it
// outlives the session, but while the session lasts it can be waited for,
// so runs waiting on this one's dependency install or image build fetch
// the result instead of repeating it (#139).
type transfer struct {
	done chan struct{}
	err  error // set before done is closed
}

// watch reaps cmd, already started, and reports when it ends.
func watch(cmd *exec.Cmd) *transfer {
	t := &transfer{done: make(chan struct{})}
	go func() {
		t.err = cmd.Wait()
		close(t.done)
	}()
	return t
}

// shareTimeout bounds ShareWait when the client gives no limit: longer
// than any transfer may run (transferBudget, image-push's 20 minutes).
const shareTimeout = 25 * time.Minute

// shareTick is how often ShareWait checks that the client is still there.
var shareTick = 2 * time.Second

func (s *Session) track(kind string, t *transfer) {
	if s.transfers == nil {
		s.transfers = map[string]*transfer{}
	}
	s.transfers[kind] = t
}

// shareWait waits for the transfer of w.Kind this session last started.
func (s *Session) shareWait(w protocol.ShareWait) error {
	start := time.Now()
	reply := func(err error) error {
		res := protocol.ShareDone{OK: err == nil, MS: time.Since(start).Milliseconds()}
		if err != nil {
			res.Message = err.Error()
		}
		return s.c.Send(protocol.TypeShareDone, res)
	}
	t := s.transfers[w.Kind]
	if t == nil {
		return reply(fmt.Errorf("no %s transfer was started", w.Kind))
	}
	limit := time.Duration(w.TimeoutMS) * time.Millisecond
	if limit <= 0 || limit > shareTimeout {
		limit = shareTimeout
	}
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(shareTick)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			if t.err != nil {
				log := "cache-upload.log"
				if w.Kind == protocol.ShareImage {
					log = "image-push.log"
				}
				return reply(fmt.Errorf("%v (see %s in the runner's work root)", t.err, log))
			}
			return reply(nil)
		case <-deadline.C:
			return reply(fmt.Errorf("still running after %s", limit.Round(time.Second)))
		case <-tick.C:
			if s.clientGone() {
				return perr(protocol.CodeInternal, "client went away while waiting for the %s transfer", w.Kind)
			}
		}
	}
}
