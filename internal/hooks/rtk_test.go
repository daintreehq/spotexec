package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnwrapRTK(t *testing.T) {
	for in, want := range map[string]struct {
		line       string
		found, raw bool
	}{
		"rtk cargo test":                  {"cargo test", true, false},
		"CI=1 rtk go test ./...":          {"CI=1 go test ./...", true, false},
		"cd pkg && rtk pytest -x":         {"cd pkg && pytest -x", true, false},
		"rtk test cargo test":             {"cargo test", true, false},
		"rtk err pnpm test":               {"pnpm test", true, false},
		"rtk proxy cargo test":            {"cargo test", true, true},
		"rtk cargo test 2>&1 | tail -20":  {"cargo test 2>&1 | tail -20", true, false},
		"rtk pytest -x && rtk cargo test": {"pytest -x && cargo test", true, false},
		"rtk git status":                  {"rtk git status", false, false},
		"cargo test":                      {"cargo test", false, false},
		"rtk -v cargo test":               {"rtk -v cargo test", false, false},
		"echo rtk cargo test":             {"echo rtk cargo test", false, false},
		"rtk cargo test | xargs echo":     {"rtk cargo test | xargs echo", false, false},
		"rtk cargo test # a \\ hazard":    {"rtk cargo test # a \\ hazard", false, false},
		"rtk cargotest":                   {"rtk cargotest", false, false},
	} {
		line, found, raw := UnwrapRTK(in)
		if line != want.line || found != want.found || raw != want.raw {
			t.Errorf("UnwrapRTK(%q) = %q %v %v, want %q %v %v", in, line, found, raw, want.line, want.found, want.raw)
		}
	}
}

func TestWrapRTK(t *testing.T) {
	for in, want := range map[string]string{
		"cargo test":      "sx run --rtk-filter cargo-test -- 'cargo test'",
		"cargo test 2>&1": "sx run --rtk-filter cargo-test -- 'cargo test 2>&1'",
		// go-test reads stdout as JSON: compiler errors stay on stderr.
		"go test ./... 2>&1":                      "sx run --rtk-filter go-test -- 'go test -json ./...'",
		"cd pkg && CI=1 go test -json ./... 2>&1": "cd pkg && sx run --rtk-filter go-test -- 'CI=1 go test -json ./...'",
		"go test -run 'TestA|TestB' ./...":        `sx run --rtk-filter go-test -- 'go test -json -run '\''TestA|TestB'\'' ./...'`,
		"go test ./... 2>&1 | tail":               "sx run -- 'go test ./... 2>&1' | tail",
		"go test ./...":                           "sx run --rtk-filter go-test -- 'go test -json ./...'",
		"CI=1 go test -json ./...":                "sx run --rtk-filter go-test -- 'CI=1 go test -json ./...'",
		"go test":                                 "sx run --rtk-filter go-test -- 'go test -json'",
		"cd pkg && cargo test":                    "cd pkg && sx run --rtk-filter cargo-test -- 'cargo test'",
		"cargo test | tail -50":                   "sx run -- 'cargo test' | tail -50",
		"cargo build && cargo test":               "sx run -- 'cargo build && cargo test'",
		"pnpm test":                               "sx run -- 'pnpm test'",
		"pytest -x":                               "sx run -- 'pytest -x'",
		"cargo testing":                           "sx run -- 'cargo testing'",
	} {
		if got := WrapRTK("sx", in, true); got != want {
			t.Errorf("WrapRTK(%q) = %q, want %q", in, got, want)
		}
	}
	if got := WrapRTK("sx", "cargo test", false); got != "sx run -- 'cargo test'" {
		t.Errorf("without RTK: %q", got)
	}
}

func TestThenOf(t *testing.T) {
	for cmd, want := range map[string]string{
		"spotexec hook claude --then 'rtk hook claude'":                      "rtk hook claude",
		"/opt/homebrew/bin/spotexec hook claude --then '/x/rtk hook claude'": "/x/rtk hook claude",
		"spotexec hook claude --then 'it'\\''s hook'":                        "it's hook",
		"spotexec hook claude":                                               "",
		"rtk hook claude --then x":                                           "",
	} {
		got, ok := thenOf(cmd)
		if got != want || ok != (want != "") {
			t.Errorf("thenOf(%q) = %q %v, want %q", cmd, got, ok, want)
		}
	}
	for cmd, want := range map[string]bool{
		"rtk hook claude": true, "/opt/homebrew/bin/rtk hook claude": true, "'/my tools/rtk' hook claude": true,
		"rtk hook gemini": false, "audit && rtk hook claude": false, "myrtk hook claude": false,
	} {
		if got := isRTKHook(cmd); got != want {
			t.Errorf("isRTKHook(%q) = %v", cmd, got)
		}
	}
	for m, want := range map[string]bool{
		"": true, "*": true, "Bash": true, "Bash|Edit": true, "Edit, Bash": true, ".*": true, "ash$": true, "^Bash$": true, "Bash(": true,
		"Edit": false, "Edit|Write": false, "Bashful": false, "^Edit$": false, "Notebook.*": false,
	} {
		if got := matchesBash(m); got != want {
			t.Errorf("matchesBash(%q) = %v", m, got)
		}
	}
}

// fakeRTK writes an rtk that rewrites only `cargo test` (as `rtk rewrite`
// does, exiting 3), and whose hook answers with a marker.
func fakeRTK(t *testing.T) string {
	t.Helper()
	// A slow first exec of the script mustn't read as "RTK wouldn't rewrite it".
	old := rtkRewriteTimeout
	rtkRewriteTimeout = time.Minute
	t.Cleanup(func() { rtkRewriteTimeout = old })
	bin := filepath.Join(t.TempDir(), "rtk")
	script := `#!/bin/sh
case "$1" in
rewrite) [ "$2" = "cargo test" ] && { echo "rtk cargo test"; exit 3; }; exit 1 ;;
hook) cat >/dev/null; echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"from-rtk"}}}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func writeClaudeSettings(t *testing.T, dir, command string) string {
	t.Helper()
	p := filepath.Join(dir, "settings.json")
	b, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{
		map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": command}}},
	}}})
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runChained(t *testing.T, ev map[string]any, then string) (hookOut, string, int) {
	t.Helper()
	in, _ := json.Marshal(ev)
	var out, errOut bytes.Buffer
	code := RunClaude(bytes.NewReader(in), &out, &errOut, ClaudeOptions{Bin: "spotexec", Then: then})
	var res hookOut
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("bad output %q: %v", out.String(), err)
		}
	}
	return res, out.String(), code
}

// An RTK hook that isn't chained races spotexec's: a command it would
// rewrite is denied with the offloaded form, which RTK leaves alone.
func TestHandleClaudeDeniesWhenRTKRaces(t *testing.T) {
	isolate(t, true)
	rtk := fakeRTK(t)
	writeClaudeSettings(t, os.Getenv("CLAUDE_CONFIG_DIR"), rtk+" hook claude")
	dir := projectDir(t, "")
	ev := map[string]any{"session_id": "race", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "cargo test"}}
	res, raw := runHook(t, ev)
	if !strings.Contains(raw, `"deny"`) || !strings.Contains(res.H.Reason, "spotexec run --rtk-filter cargo-test -- 'cargo test'") {
		t.Fatalf("want a deny with the filtered offload, got %s", raw)
	}
	if !strings.Contains(res.System, "spotexec hook install --claude --global") {
		t.Fatalf("want the chaining advice once, got %q", res.System)
	}
	if res, _ := runHook(t, ev); res.System != "" {
		t.Fatalf("advice repeated: %q", res.System)
	}
	// RTK wouldn't rewrite it: no race, so the usual rewrite.
	ev["tool_input"] = map[string]any{"command": "pnpm test"}
	if res, raw := runHook(t, ev); res.H.Updated["command"] != "spotexec run -- 'pnpm test'" {
		t.Fatalf("want a plain rewrite, got %s", raw)
	}
}

// Chained, spotexec decides first and RTK gets everything else.
func TestHandleClaudeChainsRTK(t *testing.T) {
	isolate(t, true)
	rtk := fakeRTK(t)
	dir := projectDir(t, "")
	then := rtk + " hook claude"
	res, raw, code := runChained(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "go test ./..."}}, then)
	if code != 0 || res.H.Updated["command"] != "spotexec run --rtk-filter go-test -- 'go test -json ./...'" {
		t.Fatalf("want spotexec's filtered rewrite, got %d %s", code, raw)
	}
	_, raw, code = runChained(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "git status"}}, then)
	if code != 0 || !strings.Contains(raw, "from-rtk") {
		t.Fatalf("want RTK's answer, got %d %s", code, raw)
	}
	// Outside a spotexec project, it is RTK's hook alone.
	_, raw, _ = runChained(t, map[string]any{"tool_name": "Bash", "cwd": t.TempDir(), "tool_input": map[string]any{"command": "cargo test"}}, then)
	if !strings.Contains(raw, "from-rtk") {
		t.Fatalf("want RTK's answer outside projects, got %s", raw)
	}
	// The chained hook's exit code is the hook's.
	if _, _, code := runChained(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "ls"}}, "cat >/dev/null; echo no >&2; exit 2"); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
}

// With no runners, the command stays local (with RTK's rewrite) and the
// once-a-session notice rides along with RTK's answer.
func TestHandleClaudeChainKeepsNotice(t *testing.T) {
	isolate(t, false)
	rtk := fakeRTK(t)
	dir := projectDir(t, "")
	res, raw, _ := runChained(t, map[string]any{"session_id": "n", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "cargo test"}}, rtk+" hook claude")
	if res.H.Updated["command"] != "from-rtk" || !strings.Contains(res.System, "running on this machine") || res.H.Context == "" {
		t.Fatalf("want RTK's rewrite plus the notice, got %s", raw)
	}
}

// An agent that writes `rtk cargo test` gets it offloaded, compacted.
func TestHandleClaudeOffloadsRTKCommands(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "")
	for in, want := range map[string]string{
		"rtk cargo test":       "spotexec run --rtk-filter cargo-test -- 'cargo test'",
		"rtk proxy cargo test": "spotexec run -- 'cargo test'",
		"rtk pnpm test":        "spotexec run -- 'pnpm test'",
	} {
		res, raw := runHook(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": in}})
		if res.H.Updated["command"] != want {
			t.Errorf("%q: got %s, want %q", in, raw, want)
		}
	}
	if _, raw := runHook(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "rtk git status"}}); raw != "" {
		t.Errorf("rtk git status touched: %s", raw)
	}
}

func TestInstallChainsRTK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Bash","hooks":[{"type":"command","command":"rtk hook claude","timeout":30}]},
	  {"matcher":"Bash","hooks":[{"type":"command","command":"spotexec hook claude","timeout":10}]},
	  {"matcher":"Edit","hooks":[{"type":"command","command":"lint"}]}
	]}}`), 0o644)
	if !HasRTK(p) {
		t.Fatal("RTK hook not found")
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if HasRTK(p) || !HasClaude(p) {
		t.Fatal("RTK hook not chained")
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || ok {
		t.Fatal("second install should be a no-op", ok, err)
	}
	var s struct {
		Hooks struct {
			Pre []struct {
				Matcher string           `json:"matcher"`
				Hooks   []map[string]any `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &s)
	pre := s.Hooks.Pre
	if len(pre) != 2 || pre[0].Hooks[0]["command"] != "spotexec hook claude --then 'rtk hook claude'" || pre[0].Hooks[0]["timeout"] != float64(30) || pre[1].Matcher != "Edit" {
		t.Fatalf("want RTK's entry chained in place and the separate spotexec entry gone:\n%s", b)
	}
	// Uninstalling gives RTK its hook back.
	if ok, err := UninstallClaude(p); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ = os.ReadFile(p)
	if !HasRTK(p) || HasClaude(p) || !strings.Contains(string(b), `"lint"`) {
		t.Fatalf("want RTK's own hook back:\n%s", b)
	}
}

// Uninstalling spotexec's hook leaves other tools' `hook claude` alone.
func TestUninstallKeepsRTK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Bash","hooks":[{"type":"command","command":"rtk hook claude"},{"type":"command","command":"/usr/local/bin/spotexec hook claude"}]}
	]}}`), 0o644)
	if ok, err := UninstallClaude(p); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !HasRTK(p) || HasClaude(p) {
		b, _ := os.ReadFile(p)
		t.Fatalf("got:\n%s", b)
	}
}

func TestRTKEdgeCases(t *testing.T) {
	for in, want := range map[string]string{
		"rtk cargo test;;":                    "rtk cargo test;;",             // bash refuses it
		"rtk err cd pkg && cargo test":        "rtk err cd pkg && cargo test", // RTK's child changes directory
		"rtk vitest run":                      "rtk vitest run",               // RTK adds a package runner
		"rtk proxy cargo test && rtk go test": "cargo test && go test",
	} {
		if got, _, _ := UnwrapRTK(in); got != want {
			t.Errorf("UnwrapRTK(%q) = %q, want %q", in, got, want)
		}
	}
	if _, _, raw := UnwrapRTK("rtk proxy cargo test && rtk go test"); !raw {
		t.Error("an rtk proxy segment must keep the line unfiltered")
	}
	for in, want := range map[string]string{
		"cargo test-foo":             "sx run -- 'cargo test-foo'",
		"go test-foo":                "sx run -- 'go test-foo'",
		"go test -json=false ./...":  "sx run -- 'go test -json=false ./...'",
		"go test -json=true ./...":   "sx run --rtk-filter go-test -- 'go test -json=true ./...'",
		"go test -run X -v":          "sx run --rtk-filter go-test -- 'go test -json -run X -v'",
		"cargo test && cargo test":   "sx run -- 'cargo test && cargo test'",
		"go test ./a && go test ./b": "sx run -- 'go test ./a && go test ./b'",
	} {
		if got := WrapRTK("sx", in, true); got != want {
			t.Errorf("WrapRTK(%q) = %q, want %q", in, got, want)
		}
	}
	home, _ := os.UserHomeDir()
	for cmd, want := range map[string]string{
		`spotexec hook claude --then "rtk hook claude"`:                              "rtk hook claude",
		`spotexec hook claude --then rtk`:                                            "rtk",
		chainCmd("spotexec hook claude", "it's hook"):                                "it's hook",
		chainCmd(`'/my tools/spotexec' hook claude`, "~/.cargo/bin/rtk hook claude"): "~/.cargo/bin/rtk hook claude",
	} {
		if got, ok := thenOf(cmd); !ok || got != want {
			t.Errorf("thenOf(%q) = %q %v, want %q", cmd, got, ok, want)
		}
	}
	for cmd, want := range map[string]string{
		"~/.cargo/bin/rtk hook claude":        filepath.Join(home, ".cargo/bin/rtk"),
		`"$HOME/.cargo/bin/rtk" hook claude`:  filepath.Join(home, ".cargo/bin/rtk"),
		"/opt/spotexec-tools/rtk hook claude": "/opt/spotexec-tools/rtk",
	} {
		if got, ok := rtkBin(cmd); !ok || got != want {
			t.Errorf("rtkBin(%q) = %q %v, want %q", cmd, got, ok, want)
		}
	}
	for cmd, want := range map[string]int{
		`'/opt/$cache tools/spotexec' hook claude`: hookCurrent, // a literal $, single-quoted
		`/opt/$cache/spotexec hook claude`:         hookNone,    // an expansion
		`"$HOME/bin/spotexec" hook claude`:         hookCurrent,
	} {
		if got := hookForm(cmd); got != want {
			t.Errorf("hookForm(%q) = %d, want %d", cmd, got, want)
		}
	}
	if f := hookForm("/opt/spotexec-tools/rtk hook claude"); f != hookNone {
		t.Errorf("an RTK hook under a spotexec-named directory classified %d", f)
	}
}

func TestMergeNotice(t *testing.T) {
	notice := map[string]any{"systemMessage": "ours", "hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "additionalContext": "ctx"}}
	for in, want := range map[string]string{
		"":     `{"hookSpecificOutput":{"additionalContext":"ctx","hookEventName":"PreToolUse"},"systemMessage":"ours"}`,
		"null": "null",
		"oops": "oops",
		`{"systemMessage":"theirs","hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"tc","updatedInput":{"command":"x"}}}`: `{"hookSpecificOutput":{"additionalContext":"tc\nctx","hookEventName":"PreToolUse","updatedInput":{"command":"x"}},"systemMessage":"theirs\nours"}`,
	} {
		if got := strings.TrimSpace(string(mergeNotice([]byte(in), notice))); got != want {
			t.Errorf("mergeNotice(%q) = %s, want %s", in, got, want)
		}
	}
}

// The chained hook gets the event exactly as Claude Code sent it, and its
// stderr reaches Claude Code.
func TestChainForwardsEvent(t *testing.T) {
	isolate(t, true)
	got := filepath.Join(t.TempDir(), "event")
	in := []byte(`{"tool_name":"Bash","cwd":"/nowhere","tool_input":{"command":"git status"}}`)
	var out, errOut bytes.Buffer
	code := RunClaude(bytes.NewReader(in), &out, &errOut, ClaudeOptions{Bin: "spotexec", Then: "cat >" + shellQuote(got) + "; echo blocked >&2; exit 2"})
	b, _ := os.ReadFile(got)
	if code != 2 || !bytes.Equal(b, in) || errOut.String() != "blocked\n" {
		t.Fatalf("code %d, event %q, stderr %q", code, b, errOut.String())
	}
}

// A chain under another matcher doesn't see Bash, so it can't stand in for
// spotexec's Bash entry.
func TestInstallKeepsBashBesideOtherChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Edit","hooks":[{"type":"command","command":"spotexec hook claude --then 'rtk hook claude'"}]},
	  {"matcher":"Bash","hooks":[{"type":"command","command":"spotexec hook claude"}]}
	]}}`), 0o644)
	if _, err := InstallClaude(p, "spotexec hook claude"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"Bash"`) || strings.Count(string(b), "spotexec hook claude") != 2 {
		t.Fatalf("lost the Bash entry:\n%s", b)
	}
}

// `rtk init -g` adds RTK's hook again beside the chain: installing again
// folds it in instead of chaining it twice.
func TestInstallFoldsReaddedRTK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[
	  {"matcher":"Bash","hooks":[{"type":"command","command":"spotexec hook claude --then 'rtk hook claude'"}]},
	  {"matcher":"Bash","hooks":[{"type":"command","command":"rtk hook claude"}]}
	]}}`), 0o644)
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(p)
	if HasRTK(p) || strings.Count(string(b), "hook claude") != 2 || strings.Count(string(b), `"matcher"`) != 1 {
		t.Fatalf("want the one chain:\n%s", b)
	}
}
