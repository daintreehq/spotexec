package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Write-back keeps the local versions it replaced or deleted that Git had
// no copy of (backup/<run-id>/) and the remote versions that conflicted
// (conflicts/<run-id>/). Each write-back prunes old ones of either kind:
// a run's directory goes once it is keepAtMost old, or once it is
// keepAtLeast old and keepNewest newer runs of the same kind exist. Nothing
// younger than keepAtLeast is pruned, whatever the count. A directory's age
// counts from when it was last written (a long run's copies are saved when
// it ends), and at most keepPruneMax go per write-back, oldest first.
const (
	keepAtLeast  = 24 * time.Hour
	keepAtMost   = 14 * 24 * time.Hour
	keepNewest   = 50
	keepPruneMax = 20
)

// KeptKinds are the kinds of kept run directories.
var KeptKinds = []string{"backup", "conflicts"}

// KeptRun is one run's backups or conflict copies.
type KeptRun struct {
	Kind  string `json:"kind"` // "backup" or "conflicts"
	RunID string `json:"run_id"`
	Dir   string `json:"dir"`
	// Time is when the copies were saved: the directory's modification
	// time, or the run's start (from its ID) if that is later.
	Time  time.Time `json:"time"`
	Files int       `json:"files"`
	Bytes int64     `json:"bytes"`
	named bool      // named like a run ID: spotexec made it
}

// runIDName matches newRunID's names.
var runIDName = regexp.MustCompile(`^(\d{8}-\d{6})-[0-9a-f]{12}$`)

// keptBases are the directories kept runs live under: keepBase, and the
// worktree's own git dir, where earlier versions kept a linked worktree's.
func keptBases(ws *Workspace) []string {
	bases := []string{keepBase(ws)}
	if old := conflictBase(ws); old != bases[0] {
		bases = append(bases, old)
	}
	return bases
}

// KeptRuns lists the kept runs of this worktree's repository, newest first;
// with sizes, it counts their files and bytes too.
func KeptRuns(ws *Workspace, sizes bool) []KeptRun {
	var runs []KeptRun
	for _, base := range keptBases(ws) {
		for _, kind := range KeptKinds {
			ents, err := os.ReadDir(filepath.Join(base, kind))
			if err != nil {
				continue
			}
			for _, e := range ents {
				if !e.IsDir() { // never a symlink
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				r := KeptRun{Kind: kind, RunID: e.Name(), Dir: filepath.Join(base, kind, e.Name()), Time: info.ModTime()}
				if m := runIDName.FindStringSubmatch(e.Name()); m != nil {
					if t, err := time.Parse("20060102-150405", m[1]); err == nil {
						r.named = true
						if t.After(r.Time) {
							r.Time = t
						}
					}
				}
				if sizes {
					r.Files, r.Bytes = dirSize(r.Dir)
				}
				runs = append(runs, r)
			}
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Time.After(runs[j].Time) })
	return runs
}

func dirSize(dir string) (files int, bytes int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files++
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes
}

// RemoveKept deletes the given runs' directories.
func RemoveKept(runs []KeptRun) error {
	var errs []error
	for _, r := range runs {
		if err := os.RemoveAll(r.Dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pruneKept removes the kept runs the policy above no longer keeps. Only
// directories named like run IDs are judged (anything else there is left
// alone), and errors are ignored: pruning never fails a write-back.
func pruneKept(ws *Workspace, now time.Time) {
	rank := map[string]int{}
	var drop []KeptRun
	for _, r := range KeptRuns(ws, false) {
		if !r.named {
			continue
		}
		newer := rank[r.Kind]
		rank[r.Kind]++
		if age := now.Sub(r.Time); age >= keepAtMost || (age >= keepAtLeast && newer >= keepNewest) {
			drop = append(drop, r)
		}
	}
	if len(drop) > keepPruneMax {
		drop = drop[len(drop)-keepPruneMax:] // the oldest: runs are newest first
	}
	_ = RemoveKept(drop)
}
