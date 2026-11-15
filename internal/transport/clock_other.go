//go:build !darwin && !linux

package transport

// sinceBoot: no clock that counts sleep here, so only a stopped process's
// late ticks show as a suspension.
func sinceBoot() (int64, bool) { return 0, false }
