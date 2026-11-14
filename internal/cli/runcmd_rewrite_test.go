package cli

import (
	"os"
	"os/exec"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
)

func TestRewriteRoot(t *testing.T) {
	root := "/Users/me/repo"
	cases := []struct {
		in      string
		snippet bool
		want    string
	}{
		{"cd /Users/me/repo && npm test", true, "cd ${SPOTEXEC_WORKSPACE:?} && npm test"},
		{"cd /Users/me/repo/pkg && vitest run /Users/me/repo/pkg/a.test.ts", true, "cd ${SPOTEXEC_WORKSPACE:?}/pkg && vitest run ${SPOTEXEC_WORKSPACE:?}/pkg/a.test.ts"},
		{"cat '/Users/me/repo/x'", true, `cat ''"${SPOTEXEC_WORKSPACE:?}"'/x'`},
		{"ls /Users/me/repo-other/x", true, "ls /Users/me/repo-other/x"},
		{"/Users/me/repo/a.txt", false, "../a.txt"},
		{"--out=/Users/me/repo/dist", true, "--out=${SPOTEXEC_WORKSPACE:?}/dist"},
	}
	for _, c := range cases {
		if got := rewriteRoot(c.in, root, "..", c.snippet); got != c.want {
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
