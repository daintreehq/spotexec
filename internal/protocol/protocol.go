// Package protocol defines the framed stdio RPC spoken between the spotexec
// CLI and spotexec-helper over a single SSH channel.
//
// Every frame is: uint32 big-endian length (of kind byte + payload), one kind
// byte, then the payload. Control messages are JSON objects; stream data
// (command stdout/stderr, tar archives) travels as raw bytes so large outputs
// are not base64-inflated.
package protocol

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Version is bumped whenever the wire format or message semantics change. The
// helper binary path embeds the build hash, so mismatches mean a stale helper.
const Version = 1

// MaxFrame bounds a single frame so a corrupt length prefix cannot make either
// side allocate unbounded memory. Manifests are single frames, and a large
// monorepo (100k+ files with long paths) needs tens of MiB.
const MaxFrame = 128 << 20

// Chunk is the preferred payload size for stream data frames.
const Chunk = 64 << 10

// Frame kinds.
const (
	KindMsg     byte = 'M' // JSON control message
	KindStdout  byte = 'O' // command stdout bytes
	KindStderr  byte = 'E' // command stderr bytes
	KindBlob    byte = 'B' // a chunk of a tar stream
	KindBlobEnd byte = 'Z' // end of tar stream (payload: optional error text)
)

// Frame is one decoded frame.
type Frame struct {
	Kind byte
	Data []byte
}

// Envelope wraps every JSON control message.
type Envelope struct {
	Type string          `json:"t"`
	Body json.RawMessage `json:"b,omitempty"`
}

// Conn is a framed, concurrency-safe (for writers) connection.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	bw *bufio.Writer
	mu sync.Mutex
}

// NewConn wraps a reader and writer.
// ProgressReader calls OnRead whenever bytes arrive, so liveness can count
// a large frame still streaming in (not only finished frames).
type ProgressReader struct {
	R      io.Reader
	OnRead func()
}

func (p ProgressReader) Read(b []byte) (int, error) {
	n, err := p.R.Read(b)
	if n > 0 && p.OnRead != nil {
		p.OnRead()
	}
	return n, err
}

func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReaderSize(r, 128<<10), w: w, bw: bufio.NewWriterSize(w, 128<<10)}
}

// WriteFrame writes and flushes a single frame.
func (c *Conn) WriteFrame(kind byte, data []byte) error {
	if len(data)+1 > MaxFrame {
		return fmt.Errorf("protocol: frame too large (%d bytes)", len(data))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(data)+1))
	hdr[4] = kind
	if _, err := c.bw.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.bw.Write(data); err != nil {
		return err
	}
	return c.bw.Flush()
}

// Send writes a JSON control message.
func (c *Conn) Send(typ string, body any) error {
	env := Envelope{Type: typ}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		env.Body = b
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.WriteFrame(KindMsg, b)
}

// ReadFrame reads the next frame.
func (c *Conn) ReadFrame() (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrame {
		return Frame{}, fmt.Errorf("protocol: bad frame length %d (stray output on the SSH channel? check ~/.bashrc echoes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Kind: buf[0], Data: buf[1:]}, nil
}

// Decode parses a KindMsg frame into an envelope.
func Decode(f Frame) (Envelope, error) {
	var env Envelope
	if f.Kind != KindMsg {
		return env, fmt.Errorf("protocol: expected message frame, got %q", f.Kind)
	}
	err := json.Unmarshal(f.Data, &env)
	return env, err
}

// Unmarshal decodes an envelope body into v.
func (e Envelope) Unmarshal(v any) error {
	if len(e.Body) == 0 {
		return nil
	}
	return json.Unmarshal(e.Body, v)
}

// BlobWriter streams bytes as KindBlob frames. Close sends KindBlobEnd.
type BlobWriter struct {
	C   *Conn
	buf []byte
}

func (b *BlobWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		room := Chunk - len(b.buf)
		take := min(room, len(p))
		b.buf = append(b.buf, p[:take]...)
		p = p[take:]
		if len(b.buf) >= Chunk {
			if err := b.flush(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (b *BlobWriter) flush() error {
	if len(b.buf) == 0 {
		return nil
	}
	err := b.C.WriteFrame(KindBlob, b.buf)
	b.buf = b.buf[:0]
	return err
}

// CloseWithError flushes and terminates the blob, optionally carrying an error.
func (b *BlobWriter) CloseWithError(e error) error {
	if err := b.flush(); err != nil {
		return err
	}
	var msg []byte
	if e != nil {
		msg = []byte(e.Error())
		if len(msg) == 0 {
			msg = []byte("unknown error")
		}
	}
	return b.C.WriteFrame(KindBlobEnd, msg)
}

// Close terminates the blob successfully.
func (b *BlobWriter) Close() error { return b.CloseWithError(nil) }

// BlobReader reads a blob stream from frames delivered by next. Frames of other
// kinds are passed to other (e.g. heartbeats) and skipped.
type BlobReader struct {
	Next  func() (Frame, error)
	Other func(Frame) error
	cur   []byte
	done  bool
	err   error
}

func (b *BlobReader) Read(p []byte) (int, error) {
	for len(b.cur) == 0 {
		if b.done {
			if b.err != nil {
				return 0, b.err
			}
			return 0, io.EOF
		}
		f, err := b.Next()
		if err != nil {
			b.done, b.err = true, err
			return 0, err
		}
		switch f.Kind {
		case KindBlob:
			b.cur = f.Data
		case KindBlobEnd:
			b.done = true
			if len(f.Data) > 0 {
				b.err = errors.New(string(f.Data))
			}
		default:
			if b.Other != nil {
				if err := b.Other(f); err != nil {
					b.done, b.err = true, err
					return 0, err
				}
			}
		}
	}
	n := copy(p, b.cur)
	b.cur = b.cur[n:]
	return n, nil
}

// Drain consumes the rest of the blob stream.
func (b *BlobReader) Drain() error {
	_, err := io.Copy(io.Discard, b)
	return err
}
