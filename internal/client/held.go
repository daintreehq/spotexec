package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// HeldFiles are new files a write-back left in a slot because they were over
// the write-back limit (writeback_over = "hold"), by path. spotexec pull may
// bring these back although Git doesn't ignore them.
type HeldFiles map[string]protocol.Entry

// heldRecord is what a write-back held in one slot. The slot removes held
// files at the next run that syncs into it, so a sync clears the record.
type heldRecord struct {
	Runner     string           `json:"runner"`
	InstanceID string           `json:"instance_id,omitempty"`
	Slot       int              `json:"slot"`
	RunID      string           `json:"run_id"`
	Entries    []protocol.Entry `json:"entries"`
}

// heldPath is where the record for lr's slot lives: one file per slot, so
// runs from this worktree in different slots never write the same file.
func heldPath(w *Workspace, lr LastRun) string {
	// Hashed: runner names may differ only in case, on a filesystem that
	// folds it.
	sum := sha256.Sum256(fmt.Appendf(nil, "%q %q %d", lr.Runner, lr.InstanceID, lr.Slot))
	return filepath.Join(config.StateDir(), "worktrees", w.WorktreeKey+".held", hex.EncodeToString(sum[:12])+".json")
}

// saveHeld records the files a write-back left in lr's slot.
func saveHeld(w *Workspace, lr LastRun, runID string, entries []protocol.Entry) error {
	p := heldPath(w, lr)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(heldRecord{Runner: lr.Runner, InstanceID: lr.InstanceID, Slot: lr.Slot, RunID: runID, Entries: entries})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".held-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), p)
	}
	if werr != nil {
		os.Remove(f.Name())
	}
	return werr
}

// clearHeld forgets what was held in lr's slot: a sync into it removes them.
func clearHeld(w *Workspace, lr LastRun) {
	if err := os.Remove(heldPath(w, lr)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.WriteFile(heldPath(w, lr), []byte("{}"), 0o600) // an empty record holds nothing
	}
}

// LoadHeld returns the files a write-back left in lr's slot (none when
// nothing was held there, or the record is unreadable).
func LoadHeld(w *Workspace, lr LastRun) HeldFiles {
	b, err := os.ReadFile(heldPath(w, lr))
	if err != nil {
		return nil
	}
	var rec heldRecord
	if json.Unmarshal(b, &rec) != nil || rec.Runner != lr.Runner || rec.InstanceID != lr.InstanceID || rec.Slot != lr.Slot {
		return nil
	}
	held := HeldFiles{}
	for _, e := range rec.Entries {
		held[e.Path] = e
	}
	return held
}

// HeldPatterns selects the held files matching globs (all of them when there
// are none), and the pull patterns that ask for them. The patterns may reach
// other files too: pull them with heldOnly and only the selection.
func HeldPatterns(held HeldFiles, globs []string) ([]string, HeldFiles, error) {
	if len(held) == 0 {
		return nil, nil, errors.New("the last run from this worktree held no files in its slot (or a later run synced into it)")
	}
	var out []string
	sel := HeldFiles{}
	for p, e := range held {
		if len(globs) > 0 && !pullMatches(globs, p) {
			continue
		}
		sel[p] = e
		// A pattern is a glob: a name with glob characters asks for itself
		// with each one as "?", and only held paths are written anyway.
		out = append(out, strings.Map(func(r rune) rune {
			if r == '*' || r == '?' || r == '[' {
				return '?'
			}
			return r
		}, p))
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no held file matches %s", strings.Join(globs, " "))
	}
	sort.Strings(out)
	return out, sel, nil
}
