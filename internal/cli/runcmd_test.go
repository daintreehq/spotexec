package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
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

// A --json error event names the reason when there is one, so a caller
// can branch on it without parsing the message (#134).
func TestErrorCode(t *testing.T) {
	remote := func(code string) error {
		return &transport.RemoteError{ErrorMsg: protocol.ErrorMsg{Code: code, Message: "details"}}
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("aws: %w (fix bootstrap_script, then `spotexec down aws`)", remote(protocol.CodeBootFailed)), "BOOTSTRAP_FAILED"},
		{&client.InfraError{Err: &client.DepsError{Err: fmt.Errorf("dependencies: %w", remote(protocol.CodeDepsFailed))}}, "DEPS_FAILED"},
		{remote(protocol.CodeInternal), "INTERNAL"},
		{fmt.Errorf("%s: no free slot within 10m0s", protocol.CodeQueueTimeout), "QUEUE_TIMEOUT"},
		{&client.InfraError{Err: errors.New("QUEUE_TIMEOUT: no free slot within 10m0s (2 busy)")}, "QUEUE_TIMEOUT"},
		{remote(""), "SPOTEXEC_ERROR"},
		{errors.New("ssh: connect to host 192.0.2.1 port 22: timed out"), "SPOTEXEC_ERROR"},
		{errors.New("GPU: no such runner"), "SPOTEXEC_ERROR"}, // a runner name, not a code
		{errors.New("BUILD_POOL: bootstrap_script failed on i-0abc"), "SPOTEXEC_ERROR"},
		{errors.New("queue_timeout: lower case"), "SPOTEXEC_ERROR"},
		// The helper's reason beats whatever wraps it.
		{fmt.Errorf("QUEUE_TIMEOUT: %w", remote(protocol.CodeDraining)), "RUNNER_DRAINING"},
		{errors.New("unknown runner \"gone\""), "SPOTEXEC_ERROR"},
		{&client.ImageBuildError{Err: fmt.Errorf("container image: %w", remote(protocol.CodeImageBuildFailed))}, "IMAGE_BUILD_FAILED"},
		{&client.InfraError{Err: fmt.Errorf("container image: %w", remote(protocol.CodeImageFailed))}, "IMAGE_FAILED"},
	} {
		if got := errorCode(c.err); got != c.want {
			t.Errorf("errorCode(%q) = %s, want %s", c.err, got, c.want)
		}
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

func TestTaskCommandArgsSurviveTrailingComment(t *testing.T) {
	// Under bash, as tasks run ($'...' and <<< are not POSIX sh).
	cases := []struct{ tmpl, want string }{
		{`printf "<%s>" # print each`, "<a><two words>"},
		{"printf \"<%s>\"  # print each\n", "<a><two words>"},
		{"printf \"<%s>\"\n# trailing note\n  # another\n", "<a><two words>"},
		{`printf "<%s>" "#not" a#b '# nor'`, "<#not><a#b><# nor><a><two words>"},
		{"printf \"<%s>\" \"multi\n# line\"", "<multi\n# line><a><two words>"},
		{`printf "<%s>" \# # escaped`, "<#><a><two words>"},
		{`printf "<%s>" x\ #y # after an escaped space '#' is data`, "<x #y><a><two words>"},
		{`printf "<%s>";# no space`, "<a><two words>"},
		{`printf "<%s>" 2>&1 ;`, "<a><two words>"},
		{"printf \"<%s>\" &", "<a><two words>"},
		{"printf \"<%s>\" # it's \"unbalanced\n# more", "<a><two words>"},
		{"printf \"<%s>\"\n\\\n # continued", "<a><two words>"},
		{`printf "<%s>" $(printf x)#y # c`, "<x#y><a><two words>"},
		{`printf "<%s>" $'it\'s #x' # c`, "<it's #x><a><two words>"},
		{"printf \"<%s>\" x\r#y", "<x\r#y><a><two words>"},
		{"cat <<'EOF'\n<%s>\nEOF\n# note", "<%s>\n"},
		{`printf "<%s>" \&;`, "<&><a><two words>"},
		{`printf "<%s>" "$(printf '%s' " #")"`, "< #><a><two words>"},
		{`printf "<%s>" "$(date +"%%")" # c`, "<%><a><two words>"},
		{`printf "<%s>" \$'x\' ' #y'`, `<$x\>< #y><a><two words>`},
		{"(true)# '\nprintf \"<%s>\" ' #x'", "< #x><a><two words>"},
		{`printf "<%s>" x # << note`, "<x><a><two words>"},
		{"printf \"<%s>\" `echo '#'` # c", "<#><a><two words>"},
		{`printf "<%s>" $((1+2)) <<< x # c`, "<3><a><two words>"},
		{`n=d#1; printf "<%s>" ${n//[ #]/_}`, "<d_1><a><two words>"},
		{`printf "<%s>" "${n:-"x #"}" # c`, "<x #><a><two words>"},
		{`t='{v'; printf "<%s>" "${t//\{/}" # strip braces`, "<v><a><two words>"},
		{`printf "<%s>" "$(printf '%s' case)" # label`, "<case><a><two words>"},
	}
	for _, c := range cases {
		line := taskCommand(c.tmpl, []string{"a", "two words"})
		out, err := exec.Command("bash", "-c", line).Output()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if string(out) != c.want {
			t.Errorf("template %q: line %q printed %q, want %q", c.tmpl, line, out, c.want)
		}
	}
	// A case pattern's ')' doesn't end $(...). (Checked as text: bash 3.2,
	// macOS's sh, misparses this case inside $(...).)
	if got := taskCommand(`printf "<%s>" "$(case r in r) printf "r #1";; *) :;; esac)" # c`, []string{"a"}); !strings.HasSuffix(got, `esac)" "$@" # c`) {
		t.Errorf("case in $(...): got %q", got)
	}
	// A run that is only a comment must not run its arguments, and an
	// operator at the end still takes them as its right-hand side.
	for _, c := range []struct{ tmpl, want string }{
		{"# nothing yet", "done\n"},
		{"true &&", "UNEXPECTED\ndone\n"},
		{"echo x 2>&", "x\ndone\n"},
	} {
		line := taskCommand(c.tmpl, []string{"printf", `UNEXPECTED\n`})
		if c.tmpl == "echo x 2>&" {
			line = taskCommand(c.tmpl, []string{"1"})
		}
		if out, err := exec.Command("bash", "-c", line+"\necho done").Output(); err != nil || string(out) != c.want {
			t.Errorf("template %q: line %q printed %q %v, want %q", c.tmpl, line, out, err, c.want)
		}
	}
}
