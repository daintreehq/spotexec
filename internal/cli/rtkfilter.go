package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/ui"
)

// rtkMaxSpool is how much output is held for RTK; past it, the output is
// printed unfiltered.
var rtkMaxSpool int64 = 256 << 20

// rtkOutput holds a command's stdout for RTK to compact (`rtk pipe -f NAME`)
// once the command is done: RTK's filters summarize a whole run, so nothing
// is lost by not streaming it. The raw stdout is kept in a file, shown in
// full if RTK fails, and named after the compacted output when the command
// failed, as `rtk <cmd>` itself does; stderr is never held, and reaches the
// agent as it comes. Output that can't be held (too large,
// or the disk is full) is printed as it comes instead, unfiltered.
type rtkOutput struct {
	filter string
	out    io.Writer
	rep    ui.Reporter
	mu     sync.Mutex
	f      *os.File
	n      int64
	direct bool // printing as it comes
}

const (
	rtkKeep    = 20              // failed runs' raw outputs kept
	rtkTimeout = 2 * time.Minute // for rtk pipe
	rtkActive  = 6 * time.Hour   // a newer file may be a run still writing it
	rtkDirName = "rtk"
)

func newRTKOutput(filter string, out io.Writer, rep ui.Reporter) (*rtkOutput, error) {
	if _, err := exec.LookPath("rtk"); err != nil {
		return nil, fmt.Errorf("rtk isn't on PATH")
	}
	dir := filepath.Join(config.StateDir(), rtkDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, time.Now().Format("20060102-150405")+"-*.log")
	if err != nil {
		return nil, err
	}
	return &rtkOutput{filter: filter, out: out, rep: rep, f: f}, nil
}

func (r *rtkOutput) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.direct {
		return r.out.Write(p)
	}
	var err error
	held := 0
	if r.n+int64(len(p)) > rtkMaxSpool {
		err = fmt.Errorf("more than %s of output", ui.Bytes(rtkMaxSpool))
	} else {
		held, err = r.f.Write(p)
		r.n += int64(held)
		if err == nil {
			return held, nil
		}
	}
	r.rep.Warn("RTK_FILTER", "can't hold the output for RTK (%v); it is shown unfiltered", err)
	r.direct = true
	// What the file holds (including any part of p) goes first, then the
	// rest of p.
	r.replay()
	n, err := r.out.Write(p[held:])
	return held + n, err
}

// replay prints what the file holds so far.
func (r *rtkOutput) replay() {
	if _, err := r.f.Seek(0, io.SeekStart); err == nil {
		io.CopyN(r.out, r.f, r.n)
	}
}

// finish prints the compacted output (or, if RTK fails, the raw output) and
// keeps the raw output only when the command failed.
func (r *rtkOutput) finish(failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := r.f.Name()
	keep := false
	defer func() {
		r.f.Close()
		if !keep {
			os.Remove(path)
		} else {
			pruneRTK(filepath.Dir(path))
		}
	}()
	if r.direct || r.n == 0 {
		return
	}
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		r.rep.Warn("RTK_FILTER", "can't read the output back for RTK (%v)", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rtkTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	c := exec.CommandContext(ctx, "rtk", "pipe", "-f", r.filter)
	c.Stdin, c.Stdout, c.Stderr = io.LimitReader(r.f, r.n), &out, &errOut
	err := c.Run()
	if err == nil && out.Len() > 0 {
		if !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteByte('\n') // spotexec's summary starts a line of its own
		}
		r.out.Write(out.Bytes())
		if failed {
			keep = true
			// Only stdout went through RTK: stderr was printed as it came.
			fmt.Fprintf(r.out, "\n[full stdout: %s]\n", path)
		}
		return
	}
	switch {
	case ctx.Err() != nil:
		err = fmt.Errorf("no answer in %s", rtkTimeout)
	case err == nil:
		err = fmt.Errorf("no output")
	}
	if msg := bytes.TrimSpace(errOut.Bytes()); len(msg) > 0 {
		err = fmt.Errorf("%v: %s", err, msg)
	}
	r.rep.Warn("RTK_FILTER", "rtk pipe -f %s failed (%v); the output below is unfiltered", r.filter, err)
	r.replay()
}

// pruneRTK keeps the newest rtkKeep raw outputs, never touching one a run
// may still be writing.
func pruneRTK(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= rtkKeep {
		return
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			files = append(files, file{filepath.Join(dir, e.Name()), fi.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[min(rtkKeep, len(files)):] {
		if time.Since(f.mod) > rtkActive {
			os.Remove(f.path)
		}
	}
}
