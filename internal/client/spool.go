package client

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/ui"
)

// spool keeps fetched files on disk, not in memory, until they are applied:
// a write-back or pull can be far larger than the memory this process has.
type spool struct {
	dir string
	n   int
}

// spoolMaxAge is how old a spool left behind by a killed process must be
// before a later one removes it (no transfer runs that long).
const spoolMaxAge = 24 * time.Hour

// newSpool makes a private spool in ws's spotexec state, next to the
// worktree rather than in a system temp directory that may be memory-backed.
// Close removes it.
func newSpool(ws *Workspace) (*spool, error) {
	base := filepath.Join(conflictBase(ws), "spool")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	pruneSpools(base, time.Now())
	dir, err := os.MkdirTemp(base, "fetch-")
	if err != nil {
		return nil, err
	}
	return &spool{dir: dir}, nil
}

// Close removes the spool and everything in it.
func (s *spool) Close() error { return os.RemoveAll(s.dir) }

// pruneSpools removes spools a killed process left behind.
func pruneSpools(base string, now time.Time) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "fetch-") {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > spoolMaxAge {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// add copies the regular file entry h, read from r, into the spool and
// returns it as a remote file, hashed as it was copied.
func (s *spool) add(h *tar.Header, r io.Reader) (remoteFile, error) {
	s.n++
	p := filepath.Join(s.dir, strconv.Itoa(s.n))
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return remoteFile{}, err
	}
	id, err := fsid.Reader(io.TeeReader(r, f), h.Size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return remoteFile{}, err
	}
	rf := remoteFile{spool: p, size: h.Size, id: id, mode: fsid.ModeFile}
	if h.Mode&0o111 != 0 {
		rf.mode = fsid.ModeExec
	}
	return rf, nil
}

// copyContent writes rf's spooled content to a new file at p, created with
// perm.
func copyContent(p string, rf remoteFile, perm os.FileMode) error {
	if rf.spool == "" {
		return errors.New("no content for a regular file")
	}
	src, err := os.Open(rf.spool)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != rf.size {
		err = fmt.Errorf("spooled copy is %d bytes, expected %d", n, rf.size)
	}
	return err
}

// errArchiveTooLarge: the decompressed archive went past what was asked for.
var errArchiveTooLarge = errors.New("archive is larger than the files it was asked for")

// capReader passes through at most max bytes of r, then fails if r has more.
type capReader struct {
	r    io.Reader
	left int64
	max  int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var b [1]byte
		if n, err := c.r.Read(b[:]); n > 0 {
			return 0, fmt.Errorf("%w (over %s decompressed)", errArchiveTooLarge, ui.Bytes(c.max))
		} else {
			return 0, err
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
