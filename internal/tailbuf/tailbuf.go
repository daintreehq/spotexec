// Package tailbuf keeps the last bytes written to it: the output of a step
// whose output is shown only when it fails.
package tailbuf

import (
	"fmt"
	"sync"
)

// Buffer is an io.Writer that keeps the last Max bytes written to it. It
// never fails a write, so a process writing to it is never blocked or cut
// off. It is safe for concurrent use (stdout and stderr share one).
type Buffer struct {
	Max int

	mu    sync.Mutex
	buf   []byte
	total int64
}

// New returns a Buffer keeping the last max bytes.
func New(max int) *Buffer { return &Buffer{Max: max} }

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	if len(p) >= b.Max {
		b.buf = append(b.buf[:0], p[len(p)-b.Max:]...)
		return len(p), nil
	}
	if over := len(b.buf) + len(p) - b.Max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// Bytes returns what was kept, preceded by a note when earlier output was
// dropped.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if dropped := b.total - int64(len(b.buf)); dropped > 0 {
		return append([]byte(fmt.Sprintf("[... %d earlier bytes of output omitted ...]\n", dropped)), b.buf...)
	}
	return append([]byte(nil), b.buf...)
}
