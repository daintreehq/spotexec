package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
)

func TestRewriteRoot(t *testing.T) {
	root := "/Users/me/repo"
	cases := []struct{ in, want string }{
		{"cd /Users/me/repo && npm test", `cd "${SPOTEXEC_WORKSPACE:?}" && npm test`},
		{"cd /Users/me/repo/pkg && vitest run /Users/me/repo/pkg/a.test.ts", `cd "${SPOTEXEC_WORKSPACE:?}"/pkg && vitest run "${SPOTEXEC_WORKSPACE:?}"/pkg/a.test.ts`},
		{"cat '/Users/me/repo/x'", `cat ''"${SPOTEXEC_WORKSPACE:?}"'/x'`},
		{`cat "/Users/me/repo/x"`, `cat "${SPOTEXEC_WORKSPACE:?}/x"`},
		{"ls /Users/me/repo-other/x", "ls /Users/me/repo-other/x"},
		{"--out=/Users/me/repo/dist", `--out="${SPOTEXEC_WORKSPACE:?}"/dist`},
		{`ls /Users/me/re\po/x`, `ls "${SPOTEXEC_WORKSPACE:?}"/x`},
		{`ls '/Users/me/re\po'`, `ls '/Users/me/re\po'`},
		{`ls x\ /Users/me/repo`, `ls x\ /Users/me/repo`},
		{`ls x\ y /Users/me/repo/x`, `ls x\ y "${SPOTEXEC_WORKSPACE:?}"/x`},
		{"ls \\\n/Users/me/repo/x", "ls \\\n\"${SPOTEXEC_WORKSPACE:?}\"/x"},
		{"ls x\\\n/Users/me/repo/x", "ls x\\\n/Users/me/repo/x"},
		{`ls prefix/Users/me/repo/x`, `ls prefix/Users/me/repo/x`},
		{"cat <<EOF\n/Users/me/repo/x\nEOF", "cat <<EOF\n${SPOTEXEC_WORKSPACE:?}/x\nEOF"},
		{"cat <<< /Users/me/repo/x", `cat <<< "${SPOTEXEC_WORKSPACE:?}"/x`},
	}
	for _, c := range cases {
		if got := rewriteRoot(c.in, root); got != c.want {
			t.Errorf("rewriteRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildCommandKeepsEnvAssignments(t *testing.T) {
	ws := &client.Workspace{Root: "/Users/me/repo", Cwd: "pkg"}
	values := []string{
		"--max-old-space-size=8192 --no-warnings",
		"a | b",
		"$HOME and ${PATH}",
		`it's "quoted"`,
		"",
		"plain",
	}
	for _, v := range values {
		line := buildCommand(ws, []string{"X=" + v, "Y_2=two words", "sh", "-c", `printf '%s\n' "$X" "$Y_2"`})
		cmd := exec.Command("bash", "-c", line)
		cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE=/remote")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if want := v + "\ntwo words\n"; string(out) != want {
			t.Errorf("value %q: line %q printed %q", v, line, out)
		}
	}
	// Worktree paths in a value become remote ones; later words are not
	// assignments, and a word that isn't NAME=value ends the prefix.
	line := buildCommand(ws, []string{"CFG=/Users/me/repo/a b.json", "1X=y", "printenv", "CFG"})
	if want := `CFG="${SPOTEXEC_WORKSPACE:?}"'/a b.json' 1X=y printenv CFG`; line != want {
		t.Errorf("got %q want %q", line, want)
	}
	if got := buildCommand(ws, []string{"echo", "A=b c"}); got != `echo 'A=b c'` {
		t.Errorf("argument after the command treated as an assignment: %q", got)
	}
	for _, c := range []struct{ root, value, want string }{
		{"/Users/me/repo", "/Users/me/repo/x", "/remote/x"},
		{"/Users/me/repo", "/Users/me/repo", "/remote"},
		{"/Users/me/it's repo", "--config=/Users/me/it's repo/a.json /Users/me/it's repo-other", "--config=/remote/a.json /Users/me/it's repo-other"},
	} {
		w := &client.Workspace{Root: c.root, Cwd: "."}
		cmd := exec.Command("bash", "-c", buildCommand(w, []string{"CFG=" + c.value, "printenv", "CFG"}))
		cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE=/remote")
		if out, err := cmd.Output(); err != nil || string(out) != c.want+"\n" {
			t.Errorf("root %q value %q: got %q %v", c.root, c.value, out, err)
		}
	}
}

// runWith runs a built command line under bash with SPOTEXEC_WORKSPACE set
// to a remote path containing a space.
func runWith(t *testing.T, line string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", line)
	cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE=/remote ws")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return string(out)
}

func TestBuildCommandArgsGetAbsoluteRemotePaths(t *testing.T) {
	// A tool that changes directory first (make -C) must still see the
	// workspace path, not one relative to the mirrored working directory.
	ws := &client.Workspace{Root: "/Users/me/repo", Cwd: "pkg"}
	line := buildCommand(ws, []string{"printf", "<%s>", "OUT=/Users/me/repo/build", "/Users/me/repo", "--cfg=/Users/me/repo/a b", "/Users/me/repo-other/x"})
	if got, want := runWith(t, line), "<OUT=/remote ws/build></remote ws><--cfg=/remote ws/a b></Users/me/repo-other/x>"; got != want {
		t.Errorf("line %q printed %q, want %q", line, got, want)
	}
	// A command word that is not NAME=value stays a command once spliced.
	line = buildCommand(ws, []string{"A+=/Users/me/repo/x", "y"})
	cmd := exec.Command("bash", "-c", line+" 2>&1; echo $?")
	cmd.Env = append(os.Environ(), "SPOTEXEC_WORKSPACE=/remote ws")
	if out, _ := cmd.Output(); !strings.Contains(string(out), "A+=/remote ws/x") || !strings.HasSuffix(string(out), "127\n") {
		t.Errorf("line %q ran as %q, want the command A+=/remote ws/x not found", line, out)
	}
}

func TestSnippetWorkspaceDoesNotWordSplit(t *testing.T) {
	ws := &client.Workspace{Root: "/Users/me/repo", Cwd: "."}
	line := buildCommand(ws, []string{`printf '<%s>' /Users/me/repo/a "/Users/me/repo/b" '/Users/me/repo/c' --out=/Users/me/repo/d /Users/me/repo`})
	if got, want := runWith(t, line), "</remote ws/a></remote ws/b></remote ws/c><--out=/remote ws/d></remote ws>"; got != want {
		t.Errorf("line %q printed %q, want %q", line, got, want)
	}
	// A task's arguments pass through the same rewriting.
	if got, want := runWith(t, buildTaskLine(&loaded{ws: ws}, `printf '<%s>' # each`, []string{"/Users/me/repo/x", "it's"})), "</remote ws/x><it's>"; got != want {
		t.Errorf("task line printed %q, want %q", got, want)
	}
	// A worktree whose path has a space, written with it escaped.
	ws = &client.Workspace{Root: "/Users/me/My Projects/repo", Cwd: "."}
	line = buildCommand(ws, []string{`printf '<%s>' /Users/me/My\ Projects/repo/x "/Users/me/My Projects/repo/y" /Users/me/My\ Projects/repo-other`})
	if got, want := runWith(t, line), "</remote ws/x></remote ws/y></Users/me/My Projects/repo-other>"; got != want {
		t.Errorf("line %q printed %q, want %q", line, got, want)
	}
	// Unescaped, a space, a quote or a glob is syntax, not part of the path.
	for _, c := range []struct{ root, in string }{
		{"/Users/me/it's repo", `ls /Users/me/it's repo'`},
		{"/Users/me/it's repo", `ls /Users/me/it\'s repo`},
		{"/Users/me/it's", `ls /Users/me/it's'`},
		{"/tmp/repo[1]", `ls /tmp/repo[1]/x`},
		{"/tmp/repo*", `ls /tmp/repo*/x`},
	} {
		if got := rewriteRoot(c.in, c.root); got != c.in {
			t.Errorf("rewriteRoot(%q) = %q, want it unchanged", c.in, got)
		}
	}
	root := "/Users/me/it's repo"
	if got, want := rewriteRoot(`ls /Users/me/it\'s\ repo/a`, root), `ls "${SPOTEXEC_WORKSPACE:?}"/a`; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
