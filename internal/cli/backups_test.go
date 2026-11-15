package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/client"
)

func TestPickKept(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	runs := []client.KeptRun{
		{Kind: "backup", RunID: "new", Time: now.Add(-time.Hour)},
		{Kind: "conflicts", RunID: "mid", Time: now.Add(-3 * 24 * time.Hour)},
		{Kind: "backup", RunID: "old", Time: now.Add(-10 * 24 * time.Hour)},
	}
	ids := func(rs []client.KeptRun) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.RunID)
		}
		return strings.Join(s, ",")
	}
	for _, c := range []struct {
		ids   []string
		byAge bool
		age   time.Duration
		want  string
	}{
		{nil, false, 0, "new,mid,old"}, // --all
		{nil, true, 7 * 24 * time.Hour, "old"},
		{nil, true, 3 * 24 * time.Hour, "mid,old"},
		{[]string{"new", "old"}, false, 0, "new,old"},
		{[]string{"new", "old"}, true, 24 * time.Hour, "old"},
	} {
		got, err := pickKept(runs, c.ids, c.byAge, c.age, now)
		if err != nil || ids(got) != c.want {
			t.Errorf("pickKept(%v, %v, %v) = %q, %v; want %q", c.ids, c.byAge, c.age, ids(got), err, c.want)
		}
	}
	if _, err := pickKept(runs, []string{"new", "nope"}, false, 0, now); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("an unknown run ID: %v", err)
	}
}

func TestPrintKept(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	printKept(&b, nil, now)
	if !strings.Contains(b.String(), "no write-back backups") {
		t.Errorf("empty list: %q", b.String())
	}
	b.Reset()
	printKept(&b, []client.KeptRun{
		{Kind: "backup", RunID: "20261115-110000-aaaaaaaaaaaa", Dir: "/r/.git/spotexec/backup/20261115-110000-aaaaaaaaaaaa", Time: now.Add(-time.Hour), Files: 41, Bytes: 2048},
		{Kind: "conflicts", RunID: "20261112-120000-bbbbbbbbbbbb", Dir: "/r/.git/spotexec/conflicts/20261112-120000-bbbbbbbbbbbb", Time: now.Add(-72 * time.Hour), Files: 1, Bytes: 10},
	}, now)
	out := b.String()
	for _, want := range []string{"backup ", "1h ago", "41 files", "2 KB", "/r/.git/spotexec/backup/20261115-110000-aaaaaaaaaaaa", "3d ago", "1 file ", "2 runs, 42 files"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPickKeptTakesEveryDirectoryOfARun(t *testing.T) {
	now := time.Now()
	runs := []client.KeptRun{
		{Kind: "backup", RunID: "r1", Dir: "/a/backup/r1", Time: now},
		{Kind: "conflicts", RunID: "r1", Dir: "/a/conflicts/r1", Time: now},
		{Kind: "backup", RunID: "r1", Dir: "/old/backup/r1", Time: now},
		{Kind: "backup", RunID: "r2", Dir: "/a/backup/r2", Time: now},
	}
	got, err := pickKept(runs, []string{"r1", "r1"}, false, 0, now)
	if err != nil || len(got) != 3 {
		t.Errorf("picked %+v, %v", got, err)
	}
}

func TestBackupsClear(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".spotexec.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour).UTC()
	id := old.Format("20060102-150405") + "-aaaaaaaaaaaa"
	run := filepath.Join(dir, ".spotexec", "backup", id)
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(run, old, old); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	clear := func(args ...string) error {
		c := cmdBackupsClear()
		c.SetArgs(append([]string{}, args...))
		c.SetOut(&bytes.Buffer{})
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	for _, c := range []struct {
		args []string
		err  string
	}{
		{nil, "name the runs"},
		{[]string{"--older-than", ""}, "invalid age"},
		{[]string{id, "--older-than="}, "invalid age"},
		{[]string{"--all", "--older-than="}, "--all takes"},
		{[]string{"--all", id}, "--all takes"},
		{[]string{id, "nope"}, "no kept run nope"},
		{[]string{id, "--older-than", "11d"}, ""}, // too young: nothing to do
		{[]string{"--all", "--dry-run"}, ""},
	} {
		err := clear(c.args...)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("clear %q: %v, want %q", c.args, err, c.err)
		}
		if _, serr := os.Stat(filepath.Join(run, "a.txt")); serr != nil {
			t.Fatalf("clear %q removed the backup", c.args)
		}
	}
	if err := clear(id, "--older-than", "7d"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run); err == nil {
		t.Error("clear by ID and age left the run")
	}
}

func TestBackupsJSONWhenEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	c := cmdBackups()
	c.SetArgs([]string{"--json"})
	c.SetOut(&out)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"runs":[]}` {
		t.Errorf("got %s", got)
	}
}
