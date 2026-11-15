//go:build darwin || linux

package transport

import "golang.org/x/sys/unix"

// clockNS reads clock id in ns (plus bootSkew); false if it can't be read.
func clockNS(id int32) (int64, bool) {
	var ts unix.Timespec
	if unix.ClockGettime(id, &ts) != nil {
		return 0, false
	}
	return ts.Nano() + bootSkew.Load(), true
}
