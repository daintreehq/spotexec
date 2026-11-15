package ui

import (
	"bytes"
	"io"
	"sync"
)

// Rewriter replaces occurrences of Old with New in a byte stream, holding
// back only the shortest tail that could begin a match split across writes.
// It maps remote workspace paths in stack traces back to local paths so
// links in agent/IDE output resolve.
type Rewriter struct {
	W     io.Writer
	Old   []byte
	New   []byte
	mu    sync.Mutex
	carry []byte
}

func (r *Rewriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Old) == 0 {
		return r.W.Write(p)
	}
	buf := append(r.carry, p...)
	buf = bytes.ReplaceAll(buf, r.Old, r.New)
	keep := 0
	for k := min(len(r.Old)-1, len(buf)); k > 0; k-- {
		if bytes.HasPrefix(r.Old, buf[len(buf)-k:]) {
			keep = k
			break
		}
	}
	out := buf[:len(buf)-keep]
	r.carry = append([]byte(nil), buf[len(buf)-keep:]...)
	if len(out) > 0 {
		if _, err := r.W.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes any held-back bytes.
func (r *Rewriter) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.carry) == 0 {
		return nil
	}
	_, err := r.W.Write(r.carry)
	r.carry = nil
	return err
}

// PrefixWriter prefixes each line (used for parallel task fan-out).
type PrefixWriter struct {
	W      io.Writer
	Prefix []byte
	mu     *sync.Mutex
	mid    bool
}

// NewPrefixWriter builds a line-prefixing writer sharing lock mu.
func NewPrefixWriter(w io.Writer, prefix string, mu *sync.Mutex) *PrefixWriter {
	return &PrefixWriter{W: w, Prefix: []byte(prefix), mu: mu}
}

func (p *PrefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.W.Write(prefixLines(b, p.Prefix, &p.mid))
	return len(b), err
}

// prefixLines puts prefix at the start of every line in b; mid carries
// whether the previous write ended partway through a line.
func prefixLines(b, prefix []byte, mid *bool) []byte {
	var out bytes.Buffer
	for _, c := range b {
		if !*mid {
			out.Write(prefix)
			*mid = true
		}
		out.WriteByte(c)
		if c == '\n' {
			*mid = false
		}
	}
	return out.Bytes()
}
