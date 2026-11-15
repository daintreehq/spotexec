package cli

import (
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
)

func TestTaskCommandArgsAreData(t *testing.T) {
	cases := []struct{ tmpl, want string }{
		{`printf "<%s>" "{args}"`, "<$(echo INJECTED)><two words>"},
		{`printf "<%s>" {args}`, "<$(echo INJECTED)><two words>"},
		{`printf "<%s>" '{args}'`, "<$(echo INJECTED)><two words>"},
		{`printf "<%s>"`, "<$(echo INJECTED)><two words>"},
	}
	for _, c := range cases {
		line := taskCommand(c.tmpl, []string{"$(echo INJECTED)", "two words"})
		out, err := exec.Command("sh", "-c", line).Output()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if string(out) != c.want {
			t.Errorf("template %q: got %q want %q", c.tmpl, out, c.want)
		}
	}
	if got := taskCommand("pnpm test {args}", nil); got != "pnpm test" {
		t.Errorf("no args: got %q", got)
	}
}

func TestShardedTaskParts(t *testing.T) {
	l := testLoaded()
	l.cfg.Project.Tasks = map[string]*config.Task{
		"unit":  {Run: `vitest run --shard={shard}/{shards} {args}`, Shards: 3},
		"lint":  {Run: "eslint ."},
		"all":   {Parallel: []string{"lint", "unit"}},
		"plain": {Run: "make {args}"},
	}
	parts := taskParts(l.cfg.Project, "unit", []string{"-t", "two words"})
	var labels []string
	for i, pt := range parts {
		labels = append(labels, pt.label)
		s := pt.spec(l)
		out, err := exec.Command("sh", "-c", strings.Replace(s.command, "vitest run", "printf '<%s>'", 1)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s.command, err)
		}
		// {shard} is 1-based and every shard gets the extra arguments.
		if want := "<--shard=" + strconv.Itoa(i+1) + "/3><-t><two words>"; string(out) != want {
			t.Errorf("shard %d ran %q, want %q", i+1, out, want)
		}
		if want := map[string]string{"SPOTEXEC_SHARD": strconv.Itoa(i + 1), "SPOTEXEC_SHARDS": "3"}; !reflect.DeepEqual(s.env, want) {
			t.Errorf("shard %d env %v", i+1, s.env)
		}
	}
	if want := []string{"unit 1/3", "unit 2/3", "unit 3/3"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("labels %v", labels)
	}
	// In a parallel task, a sharded task fans out in place, in list order.
	labels = nil
	for _, pt := range taskParts(l.cfg.Project, "all", nil) {
		labels = append(labels, pt.label)
		if pt.args != nil {
			t.Errorf("%s got arguments", pt.label)
		}
	}
	if want := []string{"lint", "unit 1/3", "unit 2/3", "unit 3/3"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("parallel labels %v", labels)
	}
	// An unsharded task is one part with no shard environment.
	parts = taskParts(l.cfg.Project, "plain", []string{"x"})
	if len(parts) != 1 || parts[0].spec(l).env != nil || parts[0].label != "plain" {
		t.Errorf("plain task: %+v", parts)
	}
}

// setup = false on a task skips [setup] for it; on a parallel task, for
// every part; --no-setup for anything.
func TestTaskSetupOptOut(t *testing.T) {
	l := testLoaded()
	no := false
	l.cfg.Project.Tasks = map[string]*config.Task{
		"unit":   {Run: "vitest", Shards: 2},
		"status": {Run: "git status", Setup: &no},
		"some":   {Parallel: []string{"unit", "status"}},
		"quiet":  {Parallel: []string{"unit"}, Setup: &no},
		"e2e":    {Run: "playwright", Shards: 2, Setup: &no},
		"mixed":  {Parallel: []string{"e2e", "unit"}},
	}
	skips := func(name string) []bool {
		var got []bool
		for _, pt := range taskParts(l.cfg.Project, name, nil) {
			got = append(got, pt.spec(l).noSetup)
		}
		return got
	}
	for name, want := range map[string][]bool{
		"unit":   {false, false},
		"status": {true},
		"some":   {false, false, true},
		"quiet":  {true, true},
		"e2e":    {true, true},
		"mixed":  {true, true, false, false},
	} {
		if got := skips(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: noSetup %v, want %v", name, got, want)
		}
	}
	if o := parsed(t).options(l, runSpec{command: "x", noSetup: true}, nil, nil, nil, nil); !o.NoSetup {
		t.Error("a task's opt-out didn't reach the run")
	}
	if o := parsed(t, "--no-setup").options(l, runSpec{command: "x"}, nil, nil, nil, nil); !o.NoSetup {
		t.Error("--no-setup didn't reach the run")
	}
	if o := parsed(t).options(l, runSpec{command: "x"}, nil, nil, nil, nil); o.NoSetup {
		t.Error("setup skipped by default")
	}
}

// The summary says what came back as the sync_out event does: files
// written (new or changed) and files deleted, not all of them "modified"
// (#123).
func TestWritebackNote(t *testing.T) {
	for _, c := range []struct {
		written, deleted int
		want             string
	}{
		{0, 0, ""},
		{1, 0, " · pulled 1 file"},
		{1201, 0, " · pulled 1201 files"},
		{0, 1, " · deleted 1 file"},
		{3, 2, " · pulled 3 files · deleted 2 files"},
	} {
		if got := writebackNote(c.written, c.deleted); got != c.want {
			t.Errorf("writebackNote(%d, %d) = %q, want %q", c.written, c.deleted, got, c.want)
		}
	}
}
