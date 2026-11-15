package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

var keepNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// keptRun makes a run directory of kind under base, named like a run ID
// that started age before keepNow and last written then, holding one file.
func keptRun(t *testing.T, base, kind string, age time.Duration, n int) string {
	t.Helper()
	at := keepNow.Add(-age)
	id := at.Format("20060102-150405") + fmt.Sprintf("-%012x", n)
	dir := filepath.Join(base, kind, id)
	writeFile(t, dir, "src/x.go", "x\n")
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return id
}

// keptFixture makes a git worktree and a run directory of kind for each age.
func keptFixture(t *testing.T, kind string, ages ...time.Duration) (*Workspace, []string) {
	t.Helper()
	f := newWBFixture(t, true, map[string]string{"a.txt": "a\n"}, nil)
	var ids []string
	for i, a := range ages {
		ids = append(ids, keptRun(t, keepBase(f.ws), kind, a, i))
	}
	return f.ws, ids
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// checkKept fails unless exactly the runs marked in want survive.
func checkKept(t *testing.T, ws *Workspace, kind string, ids []string, want func(i int) bool) {
	t.Helper()
	for i, id := range ids {
		if got := exists(filepath.Join(keepBase(ws), kind, id)); got != want(i) {
			t.Errorf("run %d (%s) kept = %v, want %v", i, id, got, want(i))
		}
	}
}

func TestPruneKeptByAge(t *testing.T) {
	ws, ids := keptFixture(t, "backup", time.Hour, 25*time.Hour, 13*day, 14*day-time.Second, 14*day, 30*day)
	pruneKept(ws, keepNow)
	checkKept(t, ws, "backup", ids, func(i int) bool { return i < 4 }) // 14 days old goes
}

func TestPruneKeptByCount(t *testing.T) {
	ages := make([]time.Duration, 0, keepNewest+1)
	for range keepNewest - 1 {
		ages = append(ages, time.Hour)
	}
	ages = append(ages, 30*time.Hour, 31*time.Hour) // 49 newer runs, then 50
	ws, ids := keptFixture(t, "conflicts", ages...)
	pruneKept(ws, keepNow)
	checkKept(t, ws, "conflicts", ids, func(i int) bool { return i != len(ids)-1 })
}

func TestPruneKeptNeverTakesARecentRun(t *testing.T) {
	ages := make([]time.Duration, 0, keepNewest+2)
	for range keepNewest {
		ages = append(ages, time.Hour)
	}
	ages = append(ages, day-time.Second, day) // both behind 50 newer runs
	ws, ids := keptFixture(t, "backup", ages...)
	pruneKept(ws, keepNow)
	checkKept(t, ws, "backup", ids, func(i int) bool { return i != len(ids)-1 })
}

// A long run's copies are saved when it ends: its age counts from then.
func TestPruneKeptCountsFromWhenTheCopiesWereSaved(t *testing.T) {
	ws, ids := keptFixture(t, "backup", 20*day)
	dir := filepath.Join(keepBase(ws), "backup", ids[0])
	saved := keepNow.Add(-time.Hour)
	if err := os.Chtimes(dir, saved, saved); err != nil {
		t.Fatal(err)
	}
	pruneKept(ws, keepNow)
	if !exists(dir) {
		t.Error("copies saved an hour ago were pruned because the run started 20 days ago")
	}
}

func TestPruneKeptCountsEachKindSeparately(t *testing.T) {
	ages := make([]time.Duration, keepNewest)
	for i := range ages {
		ages[i] = 2 * day
	}
	ws, _ := keptFixture(t, "backup", ages...)
	c := keptRun(t, keepBase(ws), "conflicts", 3*day, 99)
	pruneKept(ws, keepNow)
	if runs := KeptRuns(ws, false); len(runs) != keepNewest+1 || !exists(filepath.Join(keepBase(ws), "conflicts", c)) {
		t.Errorf("%d runs left, want %d", len(runs), keepNewest+1)
	}
}

func TestPruneKeptRemovesAtMostAFewPerWriteback(t *testing.T) {
	ages := make([]time.Duration, keepPruneMax+3)
	for i := range ages {
		ages[i] = 20*day + time.Duration(i)*time.Hour
	}
	ws, ids := keptFixture(t, "backup", ages...)
	pruneKept(ws, keepNow)
	checkKept(t, ws, "backup", ids, func(i int) bool { return i < 3 }) // the oldest go first
}

func TestPruneKeptLeavesOtherNames(t *testing.T) {
	ws, _ := keptFixture(t, "backup")
	dir := filepath.Join(keepBase(ws), "backup", "mine")
	writeFile(t, dir, "a", "a\n")
	old := keepNow.Add(-60 * day)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	pruneKept(ws, keepNow)
	if !exists(dir) {
		t.Error("a directory spotexec didn't name was pruned")
	}
}

func TestKeptRunsListsBothKindsAndTheLegacyLinkedWorktreeDir(t *testing.T) {
	ws, ids := keptFixture(t, "backup", time.Hour, 5*time.Hour)
	c := keptRun(t, keepBase(ws), "conflicts", 2*time.Hour, 7)
	cdir := filepath.Join(keepBase(ws), "conflicts", c)
	writeFile(t, cdir, "dir/f.txt", "12345\n")
	if at := keepNow.Add(-2 * time.Hour); os.Chtimes(cdir, at, at) != nil {
		t.Fatal("chtimes")
	}
	// Before #119, a linked worktree kept them in its own git dir.
	legacy := &Workspace{Root: ws.Root, Git: true, GitDir: t.TempDir(), CommonGitDir: ws.CommonGitDir}
	old := filepath.Join(legacy.GitDir, "spotexec")
	l := keptRun(t, old, "backup", 30*time.Minute, 8)
	gone := keptRun(t, old, "conflicts", 20*day, 9)

	pruneKept(legacy, keepNow)
	if exists(filepath.Join(old, "conflicts", gone)) {
		t.Error("an expired run in the old location was not pruned")
	}
	runs := KeptRuns(legacy, true)
	var order []string
	for _, r := range runs {
		order = append(order, r.RunID)
	}
	if got, want := strings.Join(order, " "), strings.Join([]string{l, ids[0], c, ids[1]}, " "); got != want {
		t.Fatalf("runs newest first:\n got %s\nwant %s", got, want)
	}
	if runs[2].Kind != "conflicts" || runs[2].Files != 2 || runs[2].Bytes != 8 {
		t.Errorf("conflicts run %+v", runs[2])
	}
	if !runs[1].Time.Equal(keepNow.Add(-time.Hour)) {
		t.Errorf("time %v", runs[1].Time)
	}
	if err := RemoveKept(runs[1:]); err != nil {
		t.Fatal(err)
	}
	if left := KeptRuns(legacy, false); len(left) != 1 || left[0].RunID != l {
		t.Errorf("left %+v", left)
	}
}

func TestWritebackPrunesOldRuns(t *testing.T) {
	f := newWBFixture(t, true, map[string]string{"a.txt": "a\n"}, map[string]string{"new.txt": "n\n"})
	now := time.Now()
	old := filepath.Join(keepBase(f.ws), "backup", now.Add(-keepAtMost-time.Hour).UTC().Format("20060102-150405")+"-aaaaaaaaaaaa")
	recent := filepath.Join(keepBase(f.ws), "conflicts", now.Add(-time.Hour).UTC().Format("20060102-150405")+"-bbbbbbbbbbbb")
	writeFile(t, old, "x", "x\n")
	writeFile(t, recent, "y", "y\n")
	for _, d := range []string{old, recent} {
		at := now.Add(-keepAtMost - time.Hour)
		if d == recent {
			at = now.Add(-time.Hour)
		}
		if err := os.Chtimes(d, at, at); err != nil {
			t.Fatal(err)
		}
	}
	wb := f.apply(t, []string{"new.txt"}, nil)
	if exists(old) {
		t.Error("a run past the age limit was not pruned")
	}
	if !exists(recent) || readOr(filepath.Join(wb.BackupDir, "new.txt")) != "n\n" {
		t.Error("a recent run or this run's backup was pruned")
	}
}
