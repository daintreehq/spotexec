package transport

import "golang.org/x/sys/unix"

// sinceBoot reads a clock that keeps counting while the machine is
// suspended (CLOCK_BOOTTIME; Go's monotonic clock is CLOCK_MONOTONIC, which
// stops), in ns.
func sinceBoot() (int64, bool) { return clockNS(unix.CLOCK_BOOTTIME) }
