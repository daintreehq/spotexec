package client

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Install output is the runner's business, not the command's: it goes to a
// local log (and a short in-memory tail for diagnosing a failure) instead of
// the run's stdout or stderr.
const (
	depsLogsKept  = 20       // newest install logs kept in the logs directory
	depsTailBytes = 16 << 10 // bytes of output kept in memory for the tail
	depsTailLines = 40       // lines of the tail shown when an install fails
	// A log written to this recently may belong to an install still running
	// (another part of a parallel task, another sx): pruning leaves it.
	depsLogsActive = time.Hour
)

// depsLog captures one install's output. The file is created on the first
// byte, so a cache hit (which prints nothing) leaves no log behind. A log
// that can't be created only loses the file: the tail is still kept.
type depsLog struct {
	dir, name string

	mu      sync.Mutex
	f       *os.File
	path    string
	failed  bool // the file couldn't be created or written: stop trying
	tail    []byte
	written int64
}

func newDepsLog(dir, label string) *depsLog {
	return &depsLog{dir: dir, name: logName(label)}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func logName(label string) string {
	n := strings.Trim(unsafeName.ReplaceAllString(label, "-"), "-.")
	if n == "" {
		n = "deps"
	}
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

func (l *depsLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.written += int64(len(p))
	l.tail = append(l.tail, p...)
	if over := len(l.tail) - depsTailBytes; over > 0 {
		l.tail = append(l.tail[:0], l.tail[over:]...)
	}
	if l.f == nil && !l.failed {
		l.open()
	}
	if l.f != nil {
		if _, err := l.f.Write(p); err != nil {
			l.failed = true
			l.f.Close()
			l.f = nil
		}
	}
	return len(p), nil // never fail the install over its log
}

func (l *depsLog) open() {
	if l.dir == "" {
		l.failed = true
		return
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		l.failed = true
		return
	}
	f, err := os.CreateTemp(l.dir, time.Now().UTC().Format("20060102-150405")+"-"+l.name+"-*.log")
	if err != nil {
		l.failed = true
		return
	}
	l.f, l.path = f, f.Name()
	pruneDepsLogs(l.dir, depsLogsKept, time.Now().Add(-depsLogsActive))
}

// Path is the log's file, or "" when there is none (nothing was printed, or
// it couldn't be created). A write that failed part way leaves the file with
// what came before it.
func (l *depsLog) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// Bytes is how much output the install printed.
func (l *depsLog) Bytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written
}

// Tail returns up to n of the last lines printed, and whether earlier output
// was left out.
func (l *depsLog) Tail(n int) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := bytes.TrimRight(l.tail, "\r\n")
	cut := int64(len(l.tail)) < l.written
	if cut {
		// The buffer starts mid-line: drop the partial first line.
		if i := bytes.IndexByte(t, '\n'); i >= 0 {
			t = t[i+1:]
		}
	}
	lines := strings.Split(string(t), "\n")
	if len(lines) > n {
		return strings.Join(lines[len(lines)-n:], "\n"), true
	}
	return string(t), cut
}

func (l *depsLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		if err := l.f.Close(); err != nil {
			l.failed = true
		}
		l.f = nil
	}
}

// pruneDepsLogs keeps the newest keep logs in dir (names start with a UTC
// timestamp, so they sort by age), and any written to since active.
func pruneDepsLogs(dir string, keep int, active time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var logs []os.DirEntry
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e)
		}
	}
	if len(logs) <= keep {
		return
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].Name() < logs[j].Name() })
	for _, e := range logs[:len(logs)-keep] {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(active) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
