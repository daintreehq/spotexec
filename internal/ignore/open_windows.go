package ignore

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// openIgnoreFile opens the ignore file at p for reading. With noFollow a
// symlink at p fails with ELOOP, as O_NOFOLLOW makes it elsewhere: Windows
// has no such flag, so p is looked at first, and the file opened must be
// the one looked at (a link swapped in meanwhile is an error, not
// followed).
func openIgnoreFile(p string, noFollow bool) (*os.File, error) {
	if !noFollow {
		return os.Open(p)
	}
	li, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.ELOOP}
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !os.SameFile(li, fi) {
		err = &fs.PathError{Op: "open", Path: p, Err: errors.New("replaced while being opened")}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
