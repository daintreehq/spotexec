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
	"slices"
	"strings"

	"github.com/daintreehq/spotexec/internal/fsid"
)

// Spec identifies a dependency set.
type Spec struct {
	Lockfiles []string // repo-relative
	Install   string
	CacheDirs []string
	Image     string // container image the install runs in ("" = host)
	Toolchain string // fingerprint of the runner's environment (AMI, bootstrap, PATH setup)
	OS, Arch  string // runner platform (Go names)
	// Env is what the install sees from [env] and env_allow: an install
	// that reads NODE_ENV (say) builds another dependency set for each
	// value. Only its hash reaches the key.
	Env map[string]string
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
// cache dirs, image, platform and install environment.
func Key(root string, s Spec) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s/%s\x00%s\x00%s\x00", s.OS, s.Arch, s.Install, strings.Join(s.CacheDirs, ","))
	if s.Image != "" {
		fmt.Fprintf(h, "image\x00%s\x00", s.Image)
	}
	if s.Toolchain != "" {
		fmt.Fprintf(h, "toolchain\x00%s\x00", s.Toolchain)
	}
	if len(s.Env) > 0 {
		// Sorted and length-prefixed: map order never changes the key, and
		// no two environments hash alike. Left out when empty, so a project
		// without one keeps its existing caches.
		names := make([]string, 0, len(s.Env))
		for k := range s.Env {
			names = append(names, k)
		}
		slices.Sort(names)
		fmt.Fprintf(h, "env\x00%d\x00", len(names))
		for _, k := range names {
			v := s.Env[k]
			fmt.Fprintf(h, "%d\x00%s%d\x00%s", len(k), k, len(v), v)
		}
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
