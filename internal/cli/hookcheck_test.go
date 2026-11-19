package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookEnv points spotexec and Claude Code at temp dirs, with one SSH runner
// configured (never contacted: these commands only decide), and returns a
// project directory to run in.
func hookEnv(t *testing.T, project string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SPOTEXEC_CONFIG_DIR", filepath.Join(home, "spotexec"))
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(home, "spotexec", "config.toml"))
	os.MkdirAll(filepath.Join(home, "spotexec"), 0o755)
	os.WriteFile(filepath.Join(home, "spotexec", "config.toml"), []byte("version = 1\n[runners.box]\ntype = \"ssh\"\nhost = \"me@box\"\n"), 0o644)
	t.Setenv("SPOTEXEC_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("SPOTEXEC_POOL", "")
	t.Setenv("SPOTEXEC_RUNNER", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte("version = 1\n"+project), 0o644)
	t.Chdir(dir)
	return dir
}

func TestHookCheckExplains(t *testing.T) {
	dir := hookEnv(t, "[hook]\ntransparent_prefixes = [\"direnv exec .\"]\n")
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"),
		[]byte(`{"permissions":{"allow":["Bash(pnpm test:*)"],"ask":["Bash(make build:*)"],"deny":["Bash(cargo test:*)"]}}`), 0o644)
	check := func(args ...string) (int, string) {
		var code int
		out := captureStdout(t, func() { code = MainAs("spotexec", append([]string{"hook", "check"}, args...)) })
		return code, out
	}
	for _, tc := range []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"pnpm test # all"}, 0, []string{"offload: spotexec run -- 'pnpm test'", "allowed by your rule Bash(pnpm test:*)"}},
		{[]string{"timeout 600 make build"}, 0, []string{"offload: spotexec run -- 'timeout 600 make build'", "asks first: your rule Bash(make build:*)"}},
		{[]string{"direnv exec . pnpm lint"}, 0, []string{"offload: spotexec run -- 'direnv exec . pnpm lint'"}},
		{[]string{"cargo test"}, 1, []string{"local (your rule Bash(cargo test:*)", "denies it"}},
		{[]string{"pnpm test > out.log"}, 1, []string{"local (shell syntax in `pnpm test > out.log`"}},
		{[]string{"git status && pnpm test"}, 1, []string{"local (mixed with `git status`, which isn't a heavy command)"}},
		{[]string{"pnpm", "--filter", "web", "test"}, 0, []string{"offload: spotexec run -- 'pnpm --filter web test'"}},
	} {
		code, out := check(tc.args...)
		if code != tc.code {
			t.Errorf("%q: exit %d, want %d\n%s", tc.args, code, tc.code, out)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%q: output %q lacks %q", tc.args, out, w)
			}
		}
	}
}

func TestHookDiscoverReports(t *testing.T) {
	dir := hookEnv(t, "")
	projects := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(dir))
	os.MkdirAll(projects, 0o755)
	var lines []string
	for i, c := range []string{"pnpm test", "pnpm test 2>&1 | tail", "pnpm test > log", "pnpm test > log", "git status", "sx run -- make ci"} {
		e, _ := json.Marshal(map[string]any{
			"type": "assistant", "cwd": dir, "sessionId": "s1", "timestamp": time.Now().Format(time.RFC3339),
			"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": string(rune('a' + i)), "name": "Bash", "input": map[string]any{"command": c}}}},
		})
		lines = append(lines, string(e))
	}
	os.WriteFile(filepath.Join(projects, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	var code int
	out := captureStdout(t, func() { code = MainAs("spotexec", []string{"hook", "discover"}) })
	for _, w := range []string{
		"1 sessions, 6 Bash commands", "2 heavy commands the hook offloads", "1 ran through spotexec directly",
		"2 heavy commands kept local", "2  shell syntax in a part", "2  pnpm test > log",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	out = captureStdout(t, func() { code = MainAs("spotexec", []string{"hook", "discover", "--json"}) })
	var res struct {
		FormatVersion                         int `json:"format_version"`
		Sessions, Commands, Offloaded, Direct int
		Missed                                []struct {
			Command, Reason string
			Count           int
		}
		Reasons []struct {
			Reason string
			Count  int
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 {
		t.Fatalf("--json: %v %d %s", err, code, out)
	}
	if res.FormatVersion != 1 || res.Sessions != 1 || res.Commands != 6 || res.Offloaded != 2 || res.Direct != 1 || len(res.Missed) != 1 ||
		res.Missed[0].Command != "pnpm test > log" || res.Missed[0].Count != 2 || !strings.Contains(res.Missed[0].Reason, "shell syntax in `pnpm test > log`") ||
		len(res.Reasons) != 1 || res.Reasons[0].Count != 2 {
		t.Fatalf("--json: %+v", res)
	}
	// --limit caps the list; --days 0 reads nothing as old as these.
	out = captureStdout(t, func() { MainAs("spotexec", []string{"hook", "discover", "--limit", "0"}) })
	if !strings.Contains(out, "... and 1 more (--limit)") {
		t.Errorf("--limit 0:\n%s", out)
	}
	os.Chtimes(filepath.Join(projects, "s1.jsonl"), time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	out = captureStdout(t, func() { MainAs("spotexec", []string{"hook", "discover", "--days", "1"}) })
	if !strings.Contains(out, "0 sessions, 0 Bash commands") {
		t.Errorf("--days 1:\n%s", out)
	}
}

// In block mode the hook denies with instructions; check says so, and an
// ask rule keeps the command local there.
func TestHookCheckBlockMode(t *testing.T) {
	dir := hookEnv(t, "[hook]\nmode = \"block\"\n")
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(`{"permissions":{"ask":["Bash(make build:*)"]}}`), 0o644)
	var code int
	out := captureStdout(t, func() { code = MainAs("spotexec", []string{"hook", "check", "pnpm test"}) })
	if code != 0 || !strings.Contains(out, "offload, by denying it with instructions to re-run as: spotexec run -- 'pnpm test'") || !strings.Contains(out, `mode = "block"`) {
		t.Fatalf("block: %d %q", code, out)
	}
	out = captureStdout(t, func() { code = MainAs("spotexec", []string{"hook", "check", "make build"}) })
	if code == 0 || !strings.Contains(out, "local (your rule Bash(make build:*)") {
		t.Fatalf("block with ask: %d %q", code, out)
	}
}
