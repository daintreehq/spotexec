//go:build windows

package manifest

// device is unknown on Windows, where Git's discovery doesn't stop at
// filesystem boundaries either.
func device(string) (uint64, bool) { return 0, false }
