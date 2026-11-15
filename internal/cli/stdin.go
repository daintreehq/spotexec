package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/daintreehq/spotexec/internal/ui"
)

// unreadInput reports whether f holds input nothing has read yet, without
// reading any of it or blocking: a regular file with bytes past its offset,
// or a pipe or socket with bytes waiting in its buffer. A terminal,
// /dev/null and an open but empty pipe (what an agent harness may hand a
// command) hold none.
func unreadInput(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	switch m := fi.Mode(); {
	case m.IsRegular():
		off, err := f.Seek(0, io.SeekCurrent)
		return err == nil && fi.Size() > off
	case m&(os.ModeNamedPipe|os.ModeSocket) != 0:
		rc, err := f.SyscallConn()
		if err != nil {
			return false
		}
		n := 0
		cerr := rc.Control(func(fd uintptr) { n, err = unix.IoctlGetInt(int(fd), fionread) })
		return cerr == nil && err == nil && n > 0
	}
	return false
}

// stdinNotice warns, at most once, that input piped or redirected into
// spotexec doesn't reach the command, which runs with stdin from /dev/null
// on the runner and in a local fallback alike. It looks before the run and
// again after it, since a writer such as `git diff | sx -- lint` may not
// have written anything yet when spotexec starts.
type stdinNotice struct {
	in     *os.File
	warned bool
}

// newStdinNotice watches spotexec's own stdin for a run of command, and
// returns nil (which never warns) when there is no command to feed: a sync
// or container build.
func newStdinNotice(command string) *stdinNotice {
	if command == "" {
		return nil
	}
	return &stdinNotice{in: os.Stdin}
}

func (s *stdinNotice) check(rep ui.Reporter) {
	if s == nil || s.warned || s.in == nil || !unreadInput(s.in) {
		return
	}
	s.warned = true
	rep.Warn("STDIN_IGNORED", "stdin has input, but the command doesn't get it: it runs with stdin from /dev/null. Save the input to a file in the project and pass its path instead")
}

// reporter returns rep with one more look at stdin just before the run's
// result event, so in JSON mode the warning comes before the result rather
// than after it.
func (s *stdinNotice) reporter(rep ui.Reporter) ui.Reporter {
	if s == nil {
		return rep
	}
	return stdinReporter{Reporter: rep, n: s}
}

type stdinReporter struct {
	ui.Reporter
	n *stdinNotice
}

func (r stdinReporter) Event(name string, fields map[string]any) {
	if name == "result" {
		r.n.check(r.Reporter)
	}
	r.Reporter.Event(name, fields)
}
