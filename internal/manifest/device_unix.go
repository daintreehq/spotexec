//go:build !windows

package manifest

import (
	"os"
	"syscall"
)

// device is the ID of the filesystem holding path.
func device(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true //nolint:unconvert // int32 on darwin
}
