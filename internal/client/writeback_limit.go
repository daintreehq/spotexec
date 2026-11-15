package client

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/ui"
)

// WritebackLimits bound what one write-back brings home. A zero limit is no
// limit, so the zero value never warns or holds.
type WritebackLimits struct {
	MaxFiles int   // more files than this is over
	MaxBytes int64 // more bytes than this is over
	// Hold: when the new files alone are over a limit, none of them comes
	// back; they stay in the slot (pending) for spotexec pull --held.
	Hold bool
}

// Over reports whether files and bytes exceed a limit.
func (l WritebackLimits) Over(files int, bytes int64) bool {
	return l.MaxFiles > 0 && files > l.MaxFiles || l.MaxBytes > 0 && bytes > l.MaxBytes
}

// String describes the limits ("1000 files or 100.0 MB").
func (l WritebackLimits) String() string {
	var parts []string
	if l.MaxFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d files", l.MaxFiles))
	}
	if l.MaxBytes > 0 {
		parts = append(parts, ui.Bytes(l.MaxBytes))
	}
	return strings.Join(parts, " or ")
}

// entryTotals counts entries and their bytes.
func entryTotals(entries []protocol.Entry) (files int, bytes int64) {
	for _, e := range entries {
		files++
		bytes += max(e.Size, 0)
	}
	return files, bytes
}

// pathTotal is a group of written-back files: a single file, or a directory
// ("gen/") with how many files under it came back and their size.
type pathTotal struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

func (t pathTotal) String() string {
	if t.Files == 1 && !strings.HasSuffix(t.Path, "/") {
		return fmt.Sprintf("%s (%s)", t.Path, ui.Bytes(t.Bytes))
	}
	return fmt.Sprintf("%s (%s, %s)", t.Path, files(t.Files), ui.Bytes(t.Bytes))
}

// largestPaths names what makes a write-back big: the entries grouped by
// their first path segment, each group named by the deepest directory all
// its files share (or the file itself when it is alone), and at most n
// groups by size plus at most n more by file count, largest first. One
// 200 MB file and 20,000 tiny files in gen/ both show up.
func largestPaths(entries []protocol.Entry, n int) []pathTotal {
	type group struct {
		pathTotal
		common string // the deepest directory (or file) shared so far
	}
	groups := map[string]*group{}
	for _, e := range entries {
		key := e.Path
		if i := strings.IndexByte(key, '/'); i >= 0 {
			key = key[:i]
		}
		g := groups[key]
		if g == nil {
			g = &group{common: e.Path}
			groups[key] = g
		} else {
			g.common = commonDir(g.common, e.Path)
		}
		g.Files++
		g.Bytes += max(e.Size, 0)
	}
	all := make([]pathTotal, 0, len(groups))
	for _, g := range groups {
		t := g.pathTotal
		t.Path = g.common
		if g.Files > 1 {
			t.Path += "/"
		}
		all = append(all, t)
	}
	bySize := func(a, b pathTotal) bool {
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		if a.Files != b.Files {
			return a.Files > b.Files
		}
		return a.Path < b.Path
	}
	sort.Slice(all, func(i, j int) bool { return bySize(all[i], all[j]) })
	picked := map[string]bool{}
	var out []pathTotal
	for _, t := range all[:min(n, len(all))] {
		picked[t.Path] = true
		out = append(out, t)
	}
	byCount := append([]pathTotal(nil), all...)
	sort.SliceStable(byCount, func(i, j int) bool {
		if byCount[i].Files != byCount[j].Files {
			return byCount[i].Files > byCount[j].Files
		}
		return bySize(byCount[i], byCount[j])
	})
	for _, t := range byCount[:min(n, len(byCount))] {
		if !picked[t.Path] {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return bySize(out[i], out[j]) })
	return out
}

// commonDir is the deepest directory holding both a and b, which share their
// first segment (a may already be a directory from an earlier call).
func commonDir(a, b string) string {
	if a == b {
		return a
	}
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	// A path is a directory only if something is under it, so a file's own
	// name never counts: compare parent directories.
	if !strings.HasPrefix(b, a+"/") {
		as = as[:len(as)-1]
	}
	bs = bs[:len(bs)-1]
	i := 0
	for i < len(as) && i < len(bs) && as[i] == bs[i] {
		i++
	}
	if i == 0 { // never: both share their first segment
		return strings.SplitN(b, "/", 2)[0]
	}
	return path.Join(as[:i]...)
}

func describeLargest(ts []pathTotal) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.String()
	}
	return strings.Join(parts, ", ")
}

// files is "1 file" or "N files".
func files(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}
