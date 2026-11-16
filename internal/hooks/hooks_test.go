package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
)

func TestShouldOffload(t *testing.T) {
	d, err := NewDecider(config.Hook{})
	if err != nil {
		t.Fatal(err)
	}
	yes := []string{
		"pnpm test", "pnpm vitest run src/a.test.ts", "npx prettier --write .", "pytest -x tests/",
		"cargo test", "go test ./...", "cd packages/api && pnpm test", "CI=1 pnpm run lint",
		"pnpm tsc --noEmit", "uv run pytest", "npm run test:unit",
		"pytest /Users/me/dev/app/tests", "pnpm test 2>&1", "cd pkg && go test ./... 2>&1", "pnpm vitest run src/watch.test.ts", "./gradlew test", "mvn -q verify",
		"pnpm test | tee out.txt", "pnpm test 2>&1 | tail -50", "cargo test 2>&1 | grep -E 'FAIL|panicked' | head -20",
		"go test ./... | grep -v '^ok'", "pytest -q 2>&1 | tail -n 30",
		"cd packages/api && pnpm test | tee test.log", "pytest -k 'a#b' | tail", "pytest issue#3",
	}
	no := []string{
		"git status", "ls -la", "rg foo", "pnpm dev", "pnpm vitest --watch", "pnpm test | xargs echo",
		"git log | head", "pnpm test | tail -5 > out.txt", "pnpm test || echo failed", "pnpm test | sh",
		"pnpm test | grep 'unterminated", "pnpm test |", "pnpm test | tail -$N",
		"pnpm test > out.txt", "pnpm test && git commit -m x", "sx pnpm test", "echo $(pnpm test)",
		"pnpm install", "pnpm test &", "",
		"SPOTEXEC_LOCAL=1 pytest", "mvn deploy", "./gradlew bootRun", "pnpm run dev", "npm run serve",
		"pnpm test:watch", "npm run lint:watch", "/usr/local/bin/spotexec run -- pnpm test",
	}
	for _, c := range yes {
		if !d.ShouldOffload(c) {
			t.Errorf("expected offload: %q", c)
		}
	}
	for _, c := range no {
		if d.ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
}

func TestWrapKeepsFiltersLocal(t *testing.T) {
	for in, want := range map[string]string{
		"pnpm test":                         "sx run -- 'pnpm test'",
		"pnpm test && pnpm lint":            "sx run -- 'pnpm test && pnpm lint'",
		"pnpm test 2>&1 | tail -50":         "sx run -- 'pnpm test 2>&1' | tail -50",
		"cargo test | grep -E 'FAIL|panic'": "sx run -- 'cargo test' | grep -E 'FAIL|panic'",
		"pytest -k 'a;b' | tail":            `sx run -- 'pytest -k '\''a;b'\''' | tail`,
		"pnpm test;":                        "sx run -- 'pnpm test'",
		"cd pkg; pnpm test && pnpm lint;":   "cd pkg; sx run -- 'pnpm test && pnpm lint'",
	} {
		if got := Wrap("sx", in); got != want {
			t.Errorf("Wrap(%q) = %q, want %q", in, got, want)
		}
	}
}

// A rewritten line means what the original meant: cds stay local (the
// agent's shell and local filters land where it asked), and a pipe filters
// only the command right before it, as in bash.
func TestWrapPreservesShellSemantics(t *testing.T) {
	for in, want := range map[string]string{
		"cd packages/api && pnpm test | tee test.log": "cd packages/api && sx run -- 'pnpm test' | tee test.log",
		"cd packages/api && pnpm test":                "cd packages/api && sx run -- 'pnpm test'",
		"cd pkg; go test ./... 2>&1":                  "cd pkg; sx run -- 'go test ./... 2>&1'",
		"cd a && cd b && pnpm test":                   "cd a && cd b && sx run -- 'pnpm test'",
		"cd api && CI=1 pnpm lint 2>&1 | grep error":  "cd api && sx run -- 'CI=1 pnpm lint 2>&1' | grep error",
	} {
		if got := Wrap("sx", in); got != want {
			t.Errorf("Wrap(%q) = %q, want %q", in, got, want)
		}
	}
	d, _ := NewDecider(config.Hook{})
	for _, c := range []string{
		// Offloaded, the second cd would only happen remotely.
		"pnpm build && cd pkg && pnpm test", "go vet ./... && cd sub && go test | tail",
		// Only lint is filtered; as two runs, lint could miss build's output.
		"pnpm test && pnpm lint | tail -50", "pnpm build; pnpm test | tail",
		// A comment can hide an operator; escapes aren't modelled.
		// Hoisted, the cd would also gate lint.
		"cd missing && pnpm test; pnpm lint",
		// bash refuses these without running anything.
		"prettier --write .;;", "pnpm test && && pnpm lint", "pnpm test &&",
		"pnpm test # off: ; pnpm format | tail -50", "pnpm test foo\\ ; pnpm lint", "pnpm test \\| tail",
	} {
		if d.ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
}

// The rewritten line must sequence, filter and change directory like the
// original. Run both under sh, with shell functions standing in for the
// heavy commands and for spotexec (which runs its snippet in a subshell from
// the current directory, as the remote side does).
func TestWrapRunsLikeOriginal(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	const fns = `oktest() { echo test-ran; }; failtest() { echo test-failed; return 3; }; oklint() { echo lint-ran; }; `
	for _, line := range []string{
		"cd sub && oktest | tee out.log",
		"cd sub; failtest 2>&1 | grep test",
		"cd sub && oktest && oklint",
		"cd missing && failtest && oklint",
		"cd missing; oktest; oklint",
		"cd sub && cd missing; oktest && oklint",
		"cd missing && oktest | tee out.log",
	} {
		run := func(cmdline string) string {
			dir := t.TempDir()
			os.Mkdir(filepath.Join(dir, "sub"), 0o755)
			script := fns + `fakesx() { shift 2; (eval "$1"); }
` + cmdline + `
echo "status=$? pwd=${PWD##*/}"; ls -R`
			c := exec.Command("sh", "-c", script)
			c.Dir = dir
			out, _ := c.CombinedOutput()
			return strings.ReplaceAll(string(out), filepath.Base(dir), "ROOT")
		}
		wrapped := Wrap("fakesx", line)
		if want, got := run(line), run(wrapped); got != want {
			t.Errorf("%q -> %q\ngot:  %q\nwant: %q", line, wrapped, got, want)
		}
	}
}

// isolate points spotexec's config and state at a temp dir, optionally with
// one SSH runner configured.
func isolate(t *testing.T, runner bool) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(d, "config.toml"))
	t.Setenv("SPOTEXEC_STATE_DIR", filepath.Join(d, "state"))
	t.Setenv("SPOTEXEC_POOL", "")
	t.Setenv("SPOTEXEC_RUNNER", "")
	t.Setenv("BASH_MAX_TIMEOUT_MS", "")
	// Not this machine's Claude Code settings (RTK's hook, say).
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(d, "claude"))
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if runner {
		if err := os.WriteFile(filepath.Join(d, "config.toml"), []byte("version = 1\n[runners.box]\ntype = \"ssh\"\nhost = \"me@box\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type hookOut struct {
	System string `json:"systemMessage"`
	H      struct {
		Event   string         `json:"hookEventName"`
		Updated map[string]any `json:"updatedInput"`
		Context string         `json:"additionalContext"`
		Reason  string         `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func runHook(t *testing.T, ev map[string]any) (hookOut, string) {
	t.Helper()
	in, _ := json.Marshal(ev)
	var out bytes.Buffer
	HandleClaude(bytes.NewReader(in), &out, "spotexec")
	var res hookOut
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("bad output %q: %v", out.String(), err)
		}
	}
	return res, out.String()
}

func projectDir(t *testing.T, toml string) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte("version = 1\n"+toml), 0o644)
	return dir
}

func TestHandleClaudeRewrites(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "")
	res, raw := runHook(t, map[string]any{
		"tool_name": "Bash", "cwd": dir,
		"tool_input": map[string]any{"command": "pnpm test", "description": "run tests", "timeout": 120000},
	})
	if res.H.Event != "PreToolUse" || res.H.Updated["command"] != "spotexec run -- 'pnpm test'" || res.H.Updated["description"] != "run tests" {
		t.Fatalf("unexpected rewrite: %s", raw)
	}
	// The agent's own timeout stands.
	if res.H.Updated["timeout"] != float64(120000) {
		t.Fatalf("agent's timeout overridden: %s", raw)
	}
}

// Without a timeout, a cold start (boot, bootstrap, dependencies) would hit
// Claude Code's 2-minute default and kill the run.
func TestHandleClaudeSetsTimeout(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "")
	ev := map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "pnpm test 2>&1 | tail -50"}}
	res, raw := runHook(t, ev)
	if res.H.Updated["timeout"] != float64(BashTimeoutMS) || res.H.Updated["command"] != "spotexec run -- 'pnpm test 2>&1' | tail -50" {
		t.Fatalf("got %s", raw)
	}
	// A raised ceiling is used in full.
	t.Setenv("BASH_MAX_TIMEOUT_MS", "1800000")
	if res, raw = runHook(t, ev); res.H.Updated["timeout"] != float64(1800000) {
		t.Fatalf("got %s", raw)
	}
	// Block mode tells the agent to set it.
	dir = projectDir(t, "[hook]\nmode = \"block\"\n")
	ev["cwd"] = dir
	if res, raw = runHook(t, ev); !strings.Contains(res.H.Reason, "timeout of 1800000 ms") || res.H.Updated != nil {
		t.Fatalf("got %s", raw)
	}
}

// A hook installed before `spotexec setup` keeps commands local, and says so
// once a session to both the user (systemMessage) and the agent
// (additionalContext), without failing or changing the tool call.
func TestHandleClaudeNoRunnersStaysLocalAndSaysSoOnce(t *testing.T) {
	isolate(t, false)
	dir := projectDir(t, "")
	ev := map[string]any{"session_id": "s1", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "pnpm test"}}
	res, raw := runHook(t, ev)
	if res.H.Updated != nil || res.H.Event != "PreToolUse" ||
		!strings.Contains(res.System, "running on this machine") || !strings.Contains(res.System, "spotexec setup") ||
		!strings.Contains(res.H.Context, "no usable runners") || strings.Contains(raw, "permissionDecision") {
		t.Fatalf("got %s", raw)
	}
	if _, raw = runHook(t, ev); raw != "" {
		t.Fatalf("notice repeated in the same session: %s", raw)
	}
	ev["session_id"] = "s2"
	if res, _ = runHook(t, ev); res.System == "" {
		t.Fatal("a new session should hear it once too")
	}
	// Non-heavy commands never trigger it.
	ev["session_id"], ev["tool_input"] = "s3", map[string]any{"command": "git status"}
	if _, raw = runHook(t, ev); raw != "" {
		t.Fatalf("got %s", raw)
	}
}

func TestHandleClaudeNoProjectNoOp(t *testing.T) {
	isolate(t, false)
	dir := t.TempDir()
	if _, raw := runHook(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "pnpm test"}}); raw != "" {
		t.Fatalf("expected no output outside opted-in projects, got %q", raw)
	}
}

func TestInstallIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"model":"x","hooks":{"Stop":[]}}`), 0o644)
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || ok {
		t.Fatal("second install should be a no-op", ok, err)
	}
	b, _ := os.ReadFile(p)
	var s map[string]any
	json.Unmarshal(b, &s)
	if s["model"] != "x" {
		t.Fatal("lost existing settings")
	}
	if ok, _ := UninstallClaude(p); !ok {
		t.Fatal("uninstall failed")
	}
}

func TestCdsInside(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]bool{
		"cd pkg && pnpm test":          true,
		"cd " + root + " && pnpm test": true,
		"cd " + sub + " && pnpm test":  true,
		"cd .. && pnpm test":           false,
		"cd /tmp && pytest":            false,
		"cd ~/x && pytest":             false,
		"pnpm test":                    true,
	} {
		if got := cdsInside(cmd, root); got != want {
			t.Errorf("cdsInside(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestHookForm(t *testing.T) {
	for cmd, want := range map[string]int{
		"spotexec hook claude":                      hookCurrent,
		"/opt/homebrew/bin/spotexec hook claude":    hookCurrent,
		"'/Users/me/my tools/spotexec' hook claude": hookCurrent,
		"sx hook claude":                            hookLegacy,
		"/opt/homebrew/bin/sx hook claude":          hookLegacy,
		"'/Users/me/my tools/sx' hook claude":       hookLegacy,
		"/Users/me/spotexec/bin/sx hook claude":     hookLegacy,
		"my-sx hook claude":                         hookNone,
		"other-tool check":                          hookNone,
	} {
		if got := hookForm(cmd); got != want {
			t.Errorf("hookForm(%q) = %d, want %d", cmd, got, want)
		}
	}
}

func TestInstallRewritesLegacySxHook(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{
  "model": "x",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "lint-on-edit"}]},
      {"matcher": "Bash", "hooks": [
        {"type": "command", "command": "audit-bash"},
        {"type": "command", "command": "/opt/homebrew/bin/sx hook claude", "timeout": 10}
      ]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  }
}`), 0o644)
	if !HasClaude(p) || !HasLegacyClaude(p) {
		t.Fatal("legacy hook not recognised")
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal("legacy hook not rewritten", ok, err)
	}
	if HasLegacyClaude(p) || !HasClaude(p) {
		t.Fatal("legacy hook still present after install")
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || ok {
		t.Fatal("second install should be a no-op", ok, err)
	}
	b, _ := os.ReadFile(p)
	var s struct {
		Model string `json:"model"`
		Hooks map[string][]struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	pre := s.Hooks["PreToolUse"]
	if s.Model != "x" || len(s.Hooks["Stop"]) != 1 || len(pre) != 2 || pre[0].Hooks[0]["command"] != "lint-on-edit" {
		t.Fatalf("lost existing settings:\n%s", b)
	}
	bash := pre[1].Hooks
	if pre[1].Matcher != "Bash" || len(bash) != 2 || bash[0]["command"] != "audit-bash" ||
		bash[1]["command"] != "spotexec hook claude" || bash[1]["timeout"] != float64(10) {
		t.Fatalf("legacy entry not rewritten in place:\n%s", b)
	}
}

func TestInstallDropsLegacyBesideCurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Bash","hooks":[{"type":"command","command":"spotexec hook claude"},{"type":"command","command":"sx hook claude"}]}
	]}}`), 0o644)
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "hook claude") != 1 || strings.Contains(string(b), "sx hook") {
		t.Fatalf("want one spotexec entry, got:\n%s", b)
	}
}

// Legacy entries are rewritten where they are: a spotexec hook under another
// matcher must not cost the Bash entry its hook.
func TestInstallRewritesEachLegacyEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Edit","hooks":[{"type":"command","command":"spotexec hook claude"}]},
	  {"matcher":"Bash","hooks":[{"type":"command","command":"sx hook claude"}]},
	  {"matcher":"Bash","hooks":[{"type":"command","command":"audit && sx hook claude"}]}
	]}}`), 0o644)
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(p)
	var s struct {
		Hooks struct {
			Pre []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	json.Unmarshal(b, &s)
	pre := s.Hooks.Pre
	if len(pre) != 3 || pre[1].Matcher != "Bash" || len(pre[1].Hooks) != 1 || pre[1].Hooks[0].Command != "spotexec hook claude" ||
		pre[2].Hooks[0].Command != "audit && sx hook claude" {
		t.Fatalf("got:\n%s", b)
	}
}

// The runner check follows spotexec run's own resolution: SPOTEXEC_RUNNER
// wins over the pool, and the config is the one spotexec will load after the
// line's leading cds.
func TestUsableRunnersMatchesRun(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "")
	ev := map[string]any{"session_id": "r", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "pnpm test"}}
	t.Setenv("SPOTEXEC_POOL", "missing")
	t.Setenv("SPOTEXEC_RUNNER", "box")
	if res, raw := runHook(t, ev); res.H.Updated == nil {
		t.Fatalf("SPOTEXEC_RUNNER=box should offload: %s", raw)
	}
	t.Setenv("SPOTEXEC_POOL", "")
	t.Setenv("SPOTEXEC_RUNNER", "missing")
	if res, raw := runHook(t, ev); res.H.Updated != nil || res.System == "" {
		t.Fatalf("unknown SPOTEXEC_RUNNER should stay local: %s", raw)
	}
	t.Setenv("SPOTEXEC_RUNNER", "")

	// The repository root's config names a runner this machine lacks; api's
	// own config doesn't, and `cd api && …` runs spotexec from api.
	root := projectDir(t, "pool = [\"gone\"]\n")
	os.MkdirAll(filepath.Join(root, "api"), 0o755)
	os.WriteFile(filepath.Join(root, "api", ".spotexec.toml"), []byte("version = 1\n"), 0o644)
	ev["cwd"], ev["session_id"] = root, "n1"
	ev["tool_input"] = map[string]any{"command": "cd api && pnpm test"}
	if res, raw := runHook(t, ev); res.H.Updated["command"] != "cd api && spotexec run -- 'pnpm test'" {
		t.Fatalf("got %s", raw)
	}
	ev["tool_input"] = map[string]any{"command": "pnpm test"}
	if res, raw := runHook(t, ev); res.H.Updated != nil || res.System == "" {
		t.Fatalf("root config has no usable runner: %s", raw)
	}
	// A committed fleet built on a runner this machine lacks doesn't stop
	// commands that don't use it; a pool that names it stays local (#66).
	fleet := projectDir(t, "[runners.aws-big]\nextends = \"aws\"\nmax_slots = 1\n")
	ev["cwd"], ev["session_id"] = fleet, "f1"
	if res, raw := runHook(t, ev); res.H.Updated == nil {
		t.Fatalf("an unused unavailable fleet should still offload: %s", raw)
	}
	fleet = projectDir(t, "pool = [\"aws-big\"]\n[runners.aws-big]\nextends = \"aws\"\n")
	ev["cwd"], ev["session_id"] = fleet, "f2"
	if res, raw := runHook(t, ev); res.H.Updated != nil || !strings.Contains(res.System+raw, "aws-big") {
		t.Fatalf("a pool of an unavailable fleet should stay local and say why: %s", raw)
	}
	ev["cwd"], ev["session_id"] = root, "n1"

	// A nested repository without a .spotexec.toml runs with the defaults.
	lib := filepath.Join(root, "vendor", "lib")
	os.MkdirAll(lib, 0o755)
	if out, err := exec.Command("git", "-C", lib, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	ev["tool_input"] = map[string]any{"command": "cd vendor/lib && pnpm test"}
	if res, raw := runHook(t, ev); res.H.Updated == nil {
		t.Fatalf("nested repo uses default config: %s", raw)
	}
	// A broken config there is reported, not replaced by the parent's.
	os.WriteFile(filepath.Join(lib, ".spotexec.toml"), []byte("version = \n"), 0o644)
	if err := RunnersFor("cd vendor/lib && pnpm test", root, nil); err == nil || !strings.Contains(err.Error(), "vendor/lib") {
		t.Fatalf("got %v", err)
	}
}

// Parallel tool calls of one session hear the notice once between them.
func TestFirstNoticeOncePerSessionConcurrently(t *testing.T) {
	isolate(t, false)
	var wg sync.WaitGroup
	var n atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if firstNotice("", "same") {
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("notice given %d times", n.Load())
	}
}

// Operators and exclude words inside quotes or arguments are data, not
// shell syntax or commands (#160).
func TestShouldOffloadQuotedAndArguments(t *testing.T) {
	d, err := NewDecider(config.Hook{})
	if err != nil {
		t.Fatal(err)
	}
	yes := []string{
		"go test -run 'TestFoo|TestBar' ./...", "vitest run -t 'login|signup'",
		"pytest -k 'a or (b and c)'", `pytest -k "a or (b and c)" | tail`, "pytest -k 'a;b' | tail",
		"pytest -k 'x && y'", "go test ./cmd/spotexec", "cargo test -p sx", "pytest -k watch",
		"cargo test --features dev", "pytest tests/start", "go test ./internal/serve/...",
		"pnpm test 2>&1 && pnpm lint 2>&1", "pytest -k '$FILTER'", "vitest run --reporter=dot src/a.test.ts",
	}
	no := []string{
		"X='1 pytest; pytest' git commit -m wip", `X="pytest && pytest" git push`,
		"X='a;b' pytest; git push", "pytest 'unterminated",
		"sx pnpm test", "spotexec run -- pnpm test", "/usr/local/bin/spotexec run -- pnpm test",
		"CI=1 sx pnpm test", "cd pkg && sx go test", "npm start", "pnpm start", "yarn dev",
		"npx vitest watch", "pnpm vitest watch", "vitest dev", "pnpm run test:watch",
		"pnpm test 2>&1 > out.txt", "pnpm test 2>&1 &",
		// Expansions run with this machine's values or the runner's.
		`pytest -k "$FILTER"`, `pytest -k "$(cat f)"`, "pytest -k \"`cat f`\"", "X=\"$(id)\" pytest",
		// bash refuses these as written; joined, they would run.
		"go test\n&& go test", "go test\n| tail", "pytest -k 'a\nb'",
		// The subcommand may follow vitest's options.
		"vitest --no-color watch", "npx vitest --reporter=dot dev",
	}
	for _, c := range yes {
		if !d.ShouldOffload(c) {
			t.Errorf("expected offload: %q", c)
		}
	}
	for _, c := range no {
		if d.ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
}

func TestWrapQuotedAndMerged(t *testing.T) {
	for in, want := range map[string]string{
		"go test -run 'TestA|TestB' ./... 2>&1 | tail": `sx run -- 'go test -run '\''TestA|TestB'\'' ./... 2>&1' | tail`,
		"pnpm test 2>&1 && pnpm lint 2>&1":             "sx run -- 'pnpm test 2>&1 && pnpm lint 2>&1'",
		"cd pkg 2>&1 && go test ./...":                 "cd pkg 2>&1 && sx run -- 'go test ./...'",
		"X='a; b' pytest -x":                           `sx run -- 'X='\''a; b'\'' pytest -x'`,
	} {
		if got := Wrap("sx", in); got != want {
			t.Errorf("Wrap(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"pytest":                        "",
		"CI=1 pytest":                   "CI=1 ",
		"A=1 B=2  pytest":               "A=1 B=2  ",
		"X='1 pytest; pytest' git push": "X='1 pytest; pytest' ",
		`X="a && b" Y= go test`:         `X="a && b" Y= `,
		"X=1":                           "",
		"X=1;pytest":                    "",
		"'X'=1 pytest":                  "",
		"X='open pytest":                "",
	} {
		if got := envPrefix(in); got != want {
			t.Errorf("envPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCdsInsideQuotedAndMerged(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "pkg"), 0o755)
	for cmd, want := range map[string]bool{
		"cd pkg 2>&1 && go test ./...":      true,
		"cd .. 2>&1 && go test ./...":       false,
		"X='a; cd /tmp' pytest":             true,
		"X='a; b' cd .. && pytest":          false,
		"cd pkg && pytest -k 'x; cd /tmp'":  true,
		"cd pkg && pytest | grep 'a; cd /'": true,
	} {
		if got := cdsInside(cmd, root); got != want {
			t.Errorf("cdsInside(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// Only an entry that sees Bash commands counts as installed (#160).
func TestInstallAddsBashBesideOtherMatcher(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Edit|Write","hooks":[{"type":"command","command":"spotexec hook claude"}]}
	]}}`), 0o644)
	if HasClaude(p) {
		t.Fatal("an Edit|Write entry never sees Bash")
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(p)
	if !HasClaude(p) || !strings.Contains(string(b), `"Edit|Write"`) || strings.Count(string(b), "spotexec hook claude") != 2 {
		t.Fatalf("want a Bash entry beside the Edit|Write one:\n%s", b)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || ok {
		t.Fatal("second install should be a no-op", ok, err)
	}
	// A wildcard matcher covers Bash.
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"spotexec hook claude"}]}]}}`), 0o644)
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || ok || !HasClaude(p) {
		t.Fatal("a * entry is installed already", ok, err)
	}
}

// After a switch of install method, re-installing points the hook at the
// spotexec now in use, keeping a chain and the entry's options (#160).
func TestInstallRefreshesStalePath(t *testing.T) {
	want := "/opt/homebrew/bin/spotexec hook claude"
	for name, c := range map[string]struct{ have, keep string }{
		"plain": {
			`{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/local/bin/spotexec hook claude","timeout":7}]}`,
			`"command": "` + want + `",` + "\n" + `            "timeout": 7`,
		},
		"chained": {
			`{"matcher":"Bash","hooks":[{"type":"command","command":"'/old dir/spotexec' hook claude --then 'rtk hook claude'"}]}`,
			`"command": "` + want + ` --then 'rtk hook claude'"`,
		},
	} {
		p := filepath.Join(t.TempDir(), "settings.json")
		// The user's own compound command is never ours to rewrite.
		os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[`+c.have+`,
		  {"matcher":"Bash","hooks":[{"type":"command","command":"audit; /usr/local/bin/spotexec hook claude"}]}
		]}}`), 0o644)
		if ok, err := InstallClaude(p, want); err != nil || !ok {
			t.Fatal(name, ok, err)
		}
		b, _ := os.ReadFile(p)
		got := string(b)
		if !strings.Contains(got, c.keep) || strings.Contains(got, "old dir") ||
			!strings.Contains(got, `"audit; /usr/local/bin/spotexec hook claude"`) || strings.Count(got, `"matcher"`) != 2 {
			t.Errorf("%s: want %s, the user's own hook kept:\n%s", name, c.keep, got)
		}
		if ok, err := InstallClaude(p, want); err != nil || ok {
			t.Fatal(name, "second install should be a no-op", ok, err)
		}
	}
	// The same executable, quoted differently, is left as it is.
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"'/opt/homebrew/bin/spotexec' hook claude"}]}]}}`), 0o644)
	if ok, err := InstallClaude(p, want); err != nil || ok {
		t.Fatal("same executable rewritten", ok, err)
	}
}

// Through a package runner, spotexec is still spotexec.
func TestShouldOffloadRunnerWrappedSpotexec(t *testing.T) {
	d, err := NewDecider(config.Hook{Patterns: []string{`^pnpm exec `, `^npx `}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"pnpm exec sx pytest", "pnpm exec '/opt/tools/spotexec' run -- pytest", "npx spotexec run -- pytest"} {
		if d.ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
	if !d.ShouldOffload("pnpm exec vitest run") {
		t.Error("expected offload: pnpm exec vitest run")
	}
}

func TestWrapDirMerged(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "pkg"), 0o755)
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "pkg"))
	for _, c := range []string{"cd pkg 2>&1 && pnpm test", "X='a; b' cd pkg && pnpm test"} {
		if got := wrapDir(c, root); got != want {
			t.Errorf("wrapDir(%q) = %q, want %q", c, got, want)
		}
	}
}

// A matcher Go can't compile may exclude Bash: it doesn't count as
// installed, nor does a legacy hook under another matcher.
func TestInstallIgnoresUncertainMatchers(t *testing.T) {
	for name, entry := range map[string]string{
		"lookahead": `{"matcher":"^(?!Bash$).*$","hooks":[{"type":"command","command":"spotexec hook claude"}]}`,
		"legacy":    `{"matcher":"Edit","hooks":[{"type":"command","command":"sx hook claude"}]}`,
	} {
		p := filepath.Join(t.TempDir(), "settings.json")
		os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[`+entry+`]}}`), 0o644)
		if HasClaude(p) {
			t.Errorf("%s: counted as installed", name)
		}
		if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
			t.Fatal(name, ok, err)
		}
		b, _ := os.ReadFile(p)
		if !HasClaude(p) || !strings.Contains(string(b), `"matcher": "Bash"`) {
			t.Errorf("%s: no Bash entry added:\n%s", name, b)
		}
	}
}

// Refreshing the executable leaves a chain's argument exactly as written,
// expansions included.
func TestInstallRefreshKeepsChainVerbatim(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/old/spotexec hook claude --then \"$NEXT\""}]}]}}`), 0o644)
	if ok, err := InstallClaude(p, "/new/spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"command": "/new/spotexec hook claude --then \"$NEXT\""`) {
		t.Fatalf("chain argument changed:\n%s", b)
	}
}
