// Package depskey computes the dependency-cache key shared by the CLI and
// the remote helper: both must derive the same key from the same lockfiles,
// so the CLI can sign cache URLs before the helper looks for a cache.
package depskey

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spotexec/spotexec/internal/fsid"
)

// Spec identifies a dependency set.
type Spec struct {
	Lockfiles []string // repo-relative
	Install   string
	CacheDirs []string
	Image     string // container image the install runs in ("" = host)
	Toolchain string // fingerprint of the runner's environment (AMI, bootstrap, PATH setup)
	OS, Arch  string // runner platform (Go names)
}

// Toolchain fingerprints what a runner's dependency install depends on
// besides the lockfile: native modules built against one Node/Python ABI
// must never be restored onto a runner with another.
func Toolchain(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d\x00%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Key hashes lockfile contents (read under root) with the install command,
// cache dirs, image and platform.
func Key(root string, s Spec) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s/%s\x00%s\x00%s\x00", s.OS, s.Arch, s.Install, strings.Join(s.CacheDirs, ","))
	if s.Image != "" {
		fmt.Fprintf(h, "image\x00%s\x00", s.Image)
	}
	if s.Toolchain != "" {
		fmt.Fprintf(h, "toolchain\x00%s\x00", s.Toolchain)
	}
	for _, lf := range s.Lockfiles {
		rel, err := fsid.CleanRel(lf)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", fmt.Errorf("lockfile %s: %w", lf, err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", lf, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}
