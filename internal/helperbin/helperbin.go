// Package helperbin carries the cross-compiled spotexec-helper binaries
// (gzip-compressed) embedded into release builds by `make build`.
package helperbin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Hidden entries are left out: `make helpers` stages each helper in a hidden
// directory under dist, so a build running meanwhile, or after an
// interrupted one, never embeds a partial file. `keep` holds the directory
// in a clean checkout.
//
//go:embed dist
var dist embed.FS

// Name is the embedded file name for an OS/arch pair.
func Name(goos, goarch string) string {
	return fmt.Sprintf("spotexec-helper-%s-%s.gz", goos, goarch)
}

// Get returns the gzip'd helper for goos/goarch and a short content hash used
// to version the remote install path. SPOTEXEC_HELPER_DIR overrides the
// embedded set (useful in development).
func Get(goos, goarch string) ([]byte, string, error) {
	name := Name(goos, goarch)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if c, ok := cache[name]; ok {
		return c.b, c.h, nil
	}
	var b []byte
	var err error
	if d := os.Getenv("SPOTEXEC_HELPER_DIR"); d != "" {
		b, err = os.ReadFile(filepath.Join(d, name))
	} else {
		b, err = fs.ReadFile(dist, "dist/"+name)
	}
	if err != nil {
		return nil, "", fmt.Errorf("no spotexec-helper for %s/%s in this build (build with `make build`, or set SPOTEXEC_HELPER_DIR): %w", goos, goarch, err)
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])[:12]
	cache[name] = cached{b, h}
	return b, h, nil
}

type cached struct {
	b []byte
	h string
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cached{}
)

// Available lists embedded helper names.
func Available() []string {
	var out []string
	ents, _ := fs.ReadDir(dist, "dist")
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".gz" {
			out = append(out, e.Name())
		}
	}
	return out
}
