//go:build !windows

package ignore

import (
	"os"
	"syscall"
)

// openIgnoreFile opens the ignore file at p for reading. Nonblocking, so a
// FIFO in its place fails the regular-file check instead of hanging the
// scan. With noFollow a symlink at p fails with ELOOP.
func openIgnoreFile(p string, noFollow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	return os.OpenFile(p, flags, 0)
}
