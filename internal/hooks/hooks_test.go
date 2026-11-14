package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestHandleClaudeRewrites(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte("version = 1\n"), 0o644)
	in, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "cwd": dir,
		"tool_input": map[string]any{"command": "pnpm test", "description": "run tests", "timeout": 120000},
	})
	var out bytes.Buffer
	HandleClaude(bytes.NewReader(in), &out, "spotexec")
	var res struct {
		H struct {
			Event   string         `json:"hookEventName"`
			Updated map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("bad output %q: %v", out.String(), err)
	}
	if res.H.Updated["command"] != "spotexec run -- 'pnpm test'" || res.H.Updated["description"] != "run tests" {
		t.Fatalf("unexpected rewrite: %+v", res.H.Updated)
	}
}

func TestHandleClaudeNoProjectNoOp(t *testing.T) {
	dir := t.TempDir()
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "pnpm test"}})
	var out bytes.Buffer
	HandleClaude(bytes.NewReader(in), &out, "spotexec")
	if out.Len() != 0 {
		t.Fatalf("expected no output outside opted-in projects, got %q", out.String())
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
