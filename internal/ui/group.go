package ui

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"sync"
)

// groupMemCap is how much of a part's output a Group keeps in memory before
// moving it to a temporary file.
var groupMemCap = 8 << 20

// Group holds one part's output (stdout and stderr, prefixed per line as
// PrefixWriter does) and prints it as one contiguous block on Flush, so the
// parts of a parallel task don't interleave. The two streams keep their
// relative order and still reach their own destinations. Writes after Flush
// pass straight through, prefixed. Every Group of a run shares mu, which
// also guards the destinations.
type Group struct {
	mu      *sync.Mutex
	out     [2]io.Writer
	prefix  []byte
	mid     [2]bool
	segs    []groupSeg
	mem     bytes.Buffer
	spill   *os.File
	flushed bool
}

// groupSeg is a run of consecutive held bytes bound for one stream.
type groupSeg struct {
	stream int
	n      int64
}

// NewGroup builds a Group printing to stdout and stderr under lock mu.
func NewGroup(stdout, stderr io.Writer, prefix string, mu *sync.Mutex) *Group {
	return &Group{mu: mu, out: [2]io.Writer{stdout, stderr}, prefix: []byte(prefix)}
}

// Stdout is the writer for the part's standard output.
func (g *Group) Stdout() io.Writer { return groupStream{g, 0} }

// Stderr is the writer for the part's standard error.
func (g *Group) Stderr() io.Writer { return groupStream{g, 1} }

type groupStream struct {
	g      *Group
	stream int
}

func (s groupStream) Write(b []byte) (int, error) { return s.g.write(s.stream, b) }

func (g *Group) write(stream int, b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	data := prefixLines(b, g.prefix, &g.mid[stream])
	if g.flushed {
		_, err := g.out[stream].Write(data)
		return len(b), err
	}
	if err := g.hold(stream, data); err != nil {
		// Output that can't be held is printed rather than lost: this part
		// goes live from here on.
		g.flushLocked()
		_, err := g.out[stream].Write(data)
		return len(b), err
	}
	return len(b), nil
}

// hold appends data to the held output, moving it to a temporary file once
// it outgrows memory.
func (g *Group) hold(stream int, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if g.spill == nil && g.mem.Len()+len(data) > groupMemCap {
		f, err := os.CreateTemp("", "spotexec-output-*")
		if err != nil {
			return err
		}
		// Unlinked at once, so nothing is left behind however the process
		// ends; the open file stays readable until it's closed.
		os.Remove(f.Name())
		if _, err := f.Write(g.mem.Bytes()); err != nil {
			f.Close()
			return err
		}
		g.spill = f
		g.mem = bytes.Buffer{}
	}
	if g.spill != nil {
		if _, err := g.spill.Write(data); err != nil {
			return err
		}
	} else {
		g.mem.Write(data)
	}
	if n := len(g.segs); n > 0 && g.segs[n-1].stream == stream {
		g.segs[n-1].n += int64(len(data))
	} else {
		g.segs = append(g.segs, groupSeg{stream, int64(len(data))})
	}
	return nil
}

// Flush prints everything held as one block, ending any unfinished line, and
// makes later writes pass straight through. It is safe to call more than
// once.
func (g *Group) Flush() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.flushLocked()
}

func (g *Group) flushLocked() error {
	if g.flushed {
		return nil
	}
	g.flushed = true
	var err error
	var r io.Reader = &g.mem
	if g.spill != nil {
		defer g.spill.Close()
		if _, err = g.spill.Seek(0, io.SeekStart); err != nil {
			return err
		}
		r = bufio.NewReader(g.spill)
	}
	for _, s := range g.segs {
		if _, err = io.CopyN(g.out[s.stream], r, s.n); err != nil {
			break
		}
	}
	for i, mid := range g.mid {
		if mid {
			g.mid[i] = false
			if _, werr := g.out[i].Write([]byte{'\n'}); err == nil {
				err = werr
			}
		}
	}
	g.segs, g.mem, g.spill = nil, bytes.Buffer{}, nil
	return err
}
