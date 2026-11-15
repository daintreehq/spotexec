package ui

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"sync"
)

// groupMemCap is how much of a part's output a Group keeps in memory before
// moving it to a temporary file.
var groupMemCap = 8 << 20

// Group holds one part's output (stdout and stderr, prefixed per line as
// PrefixWriter does) and prints it as one contiguous block on Flush, so the
// parts of a parallel task don't interleave. The two streams keep the order
// their writes arrived in and still reach their own destinations. Writes
// after Flush pass straight through, prefixed, so nothing is lost. Every
// Group of a run shares emit, the lock guarding the destinations.
type Group struct {
	mu     sync.Mutex // this part's held output
	emit   *sync.Mutex
	out    [2]io.Writer
	prefix []byte
	mid    [2]bool
	mem    bytes.Buffer // frames, until it outgrows groupMemCap
	spill  *os.File     // frames, after that
	size   int64        // bytes of whole frames held
	err    error        // the first failure to hold output
	live   bool         // held output printed: writes pass straight through
	ended  bool         // Flush has run
}

// A frame is one write (or a piece of one): the stream (0 stdout, 1
// stderr), its length as a big-endian uint32, then the bytes.
const frameHeader = 5

// maxFrame bounds a frame's length well inside its uint32.
var maxFrame = 1 << 30

// NewGroup builds a Group printing to stdout and stderr under lock emit.
func NewGroup(stdout, stderr io.Writer, prefix string, emit *sync.Mutex) *Group {
	return &Group{emit: emit, out: [2]io.Writer{stdout, stderr}, prefix: []byte(prefix)}
}

// Stdout is the writer for the part's standard output.
func (g *Group) Stdout() io.Writer { return groupStream{g, 0} }

// Stderr is the writer for the part's standard error.
func (g *Group) Stderr() io.Writer { return groupStream{g, 1} }

type groupStream struct {
	g      *Group
	stream byte
}

func (s groupStream) Write(b []byte) (int, error) { return s.g.write(s.stream, b) }

func (g *Group) write(stream byte, b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	data := prefixLines(b, g.prefix, &g.mid[stream])
	if !g.live {
		n, err := g.hold(stream, data)
		if err == nil {
			return len(b), nil
		}
		// Output that can't be held is printed rather than lost: what is
		// held goes out now, then the rest of this write, and the part is
		// live from here on, its unfinished lines left as they are.
		g.err = err
		g.emit.Lock()
		defer g.emit.Unlock()
		g.drain()
		data = data[n:]
	} else {
		g.emit.Lock()
		defer g.emit.Unlock()
	}
	_, err := g.out[stream].Write(data)
	return len(b), err
}

// hold appends data as frames, moving the held output to a temporary file
// once it outgrows memory. It returns how much of data it held.
func (g *Group) hold(stream byte, data []byte) (int, error) {
	held := 0
	for held < len(data) {
		n := min(len(data)-held, maxFrame)
		if err := g.holdFrame(stream, data[held:held+n]); err != nil {
			return held, err
		}
		held += n
	}
	return held, nil
}

func (g *Group) holdFrame(stream byte, data []byte) error {
	frame := make([]byte, frameHeader, frameHeader+len(data))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[1:], uint32(len(data)))
	frame = append(frame, data...)
	if g.spill == nil && g.mem.Len()+len(frame) > groupMemCap {
		f, err := os.CreateTemp("", "spotexec-output-*")
		if err != nil {
			return err
		}
		// Unlinked at once, so nothing is left behind however the process
		// ends; the open file stays readable until it's closed.
		if err := os.Remove(f.Name()); err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(g.mem.Bytes()); err != nil {
			f.Close()
			return err
		}
		g.spill, g.mem = f, bytes.Buffer{}
	}
	if g.spill != nil {
		// A short write leaves a partial frame past size, never replayed.
		if _, err := g.spill.Write(frame); err != nil {
			return err
		}
	} else {
		g.mem.Write(frame)
	}
	g.size += int64(len(frame))
	return nil
}

// Flush prints everything held as one block, ending any unfinished line, then
// calls after (when not nil) still holding the output lock, so a summary can
// follow its block with nothing in between. Later writes pass straight
// through. It returns the first failure to hold the output (which was then
// printed as it came); like the live output, a destination that can't be
// written to is not an error. after runs on every call; the rest only once.
func (g *Group) Flush(after func()) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.emit.Lock()
	defer g.emit.Unlock()
	if !g.ended {
		g.ended = true
		g.drain()
		for i, mid := range g.mid {
			if mid {
				g.mid[i] = false
				g.out[i].Write([]byte{'\n'})
			}
		}
	}
	if after != nil {
		after()
	}
	return g.err
}

// drain prints the held output, frees it and makes the Group live. Both
// locks are held.
func (g *Group) drain() {
	if g.live {
		return
	}
	g.live = true
	g.replay()
	if g.spill != nil {
		g.spill.Close()
	}
	g.mem, g.spill, g.size = bytes.Buffer{}, nil, 0
}

// replay prints the held frames. A destination that fails stops receiving,
// but the other stream still gets its share.
func (g *Group) replay() {
	var r io.Reader = &g.mem
	if g.spill != nil {
		if _, err := g.spill.Seek(0, io.SeekStart); err != nil {
			return
		}
		r = bufio.NewReader(g.spill)
	}
	r = io.LimitReader(r, g.size)
	// Consecutive frames for one stream go out together; switching streams
	// first sends what the other has, keeping their order.
	var w [2]*bufio.Writer
	for i := range w {
		w[i] = bufio.NewWriterSize(&stickyWriter{w: g.out[i]}, 64<<10)
	}
	var h [frameHeader]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			break
		}
		s := h[0] & 1
		w[1-s].Flush()
		if _, err := io.CopyN(w[s], r, int64(binary.BigEndian.Uint32(h[1:]))); err != nil {
			break
		}
	}
	w[0].Flush()
	w[1].Flush()
}

// stickyWriter drops everything after its first failed write, so one broken
// destination doesn't stop the other stream's replay.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) Write(b []byte) (int, error) {
	if s.err == nil {
		_, s.err = s.w.Write(b)
	}
	return len(b), nil
}
