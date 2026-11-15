package transport

import "golang.org/x/sys/unix"

// sinceBoot reads a clock that keeps counting while the machine sleeps
// (CLOCK_MONOTONIC is mach_continuous_time on macOS; Go's monotonic clock
// is mach_absolute_time, which stops), in ns.
func sinceBoot() (int64, bool) { return clockNS(unix.CLOCK_MONOTONIC) }
