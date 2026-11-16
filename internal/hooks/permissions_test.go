package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuleMatches(t *testing.T) {
	type c struct {
		pattern, cmd string
		strict       bool // as an allow rule reads it
		loose        bool // as a deny or ask rule reads it
	}
	for _, tc := range []c{
		{"*", "anything", true, true},
		{"pnpm test:*", "pnpm test", true, true},
		{"pnpm test:*", "pnpm test --run", true, true},
		{"pnpm test:*", "pnpm testx", false, false},
		{"pnpm test:*", "pnpm  test   -x", false, true}, // an allow matches as written
		{"pnpm test", "pnpm test", true, true},
		{"pnpm test", "pnpm test --run", false, true},
		{"pnpm test", "pnpm tests", false, false},
		{"npm run *", "npm run test", true, true},
		{"npm run *", "npm run", true, true},
		{"npm run *", "npm runx", false, false},
		{"git push*", "git pushx", true, true},
		{"git * main", "git push origin main", true, true},
		{"git * main", "git push origin dev", false, false},
		{"* --help", "pnpm test --help", true, true},
		{"make build:*", "make build", true, true},
		{"cargo:*", "cargo test", true, true},
		{"cargo:*", "cargox", false, false},
		{"pytest -k 'a*'", "pytest -k 'abc'", true, true},
		{"a.b", "axb", false, false}, // not a regexp
		// :* is a wildcard only at the end; inside, the colon is literal.
		{"pnpm test --project=api:* --run", "pnpm test --project=api:prod --run", true, true},
		{"git:* push", "git push", false, false},
		{"pnpm:* test", "pnpm run test", false, false},
		// Quoted text keeps its blanks for an allow.
		{"make -f 'a b' test", "make -f 'a  b' test", false, true},
	} {
		if got := ruleMatches(tc.pattern, tc.cmd, false); got != tc.strict {
			t.Errorf("allow Bash(%s) on %q = %v, want %v", tc.pattern, tc.cmd, got, tc.strict)
		}
		if got := ruleMatches(tc.pattern, tc.cmd, true); got != tc.loose {
			t.Errorf("deny Bash(%s) on %q = %v, want %v", tc.pattern, tc.cmd, got, tc.loose)
		}
	}
}

func rules(allow, ask, deny []string) permRules {
	mk := func(l []string) []bashRule {
		var any []any
		for _, s := range l {
			any = append(any, s)
		}
		return bashRules(any, "settings.json")
	}
	return permRules{allow: mk(allow), ask: mk(ask), deny: mk(deny), complete: true}
}

func TestCheckPermissions(t *testing.T) {
	type c struct {
		cmd              string
		allow, ask, deny []string
		want             Verdict
		rule             string
		prefixes         []string
		incomplete       bool
	}
	for _, tc := range []c{
		{cmd: "pnpm test", want: VerdictNone},
		{cmd: "pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow, rule: "Bash(pnpm test:*)"},
		{cmd: "pnpm test", allow: []string{"Bash"}, want: VerdictAllow},
		{cmd: "pnpm test", allow: []string{"Bash(*)"}, want: VerdictAllow},
		{cmd: "pnpm test", allow: []string{"Read", "WebFetch(domain:x)"}, want: VerdictNone},
		{cmd: "pnpm test", allow: []string{"Bash(pnpm test:*)"}, incomplete: true, want: VerdictNone},
		// Every heavy part needs an allow; cds, read-only filters and
		// trailers are allowed anyway.
		{cmd: "cd sub && pnpm test 2>&1 | tail -50", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: "pnpm test | grep -E 'FAIL|ok' | head -5", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: "pnpm test || true", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: `pnpm test; echo "exit=$?"`, allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone}, // Claude Code asks about $?
		{cmd: `pnpm test; echo done`, allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: `pnpm test || :`, allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: `printf -v PATH /tmp/bin; pnpm test`, allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: `printf 'x%n' v; pnpm test`, allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test | tee out.log", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test | tee out.log", allow: []string{"Bash(pnpm test:*)", "Bash(tee:*)"}, want: VerdictAllow},
		{cmd: "pnpm test | sed -n 1p", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test && pnpm lint", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test && pnpm lint", allow: []string{"Bash(pnpm test:*)", "Bash(pnpm lint)"}, want: VerdictAllow},
		{cmd: "cd sub", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		// An allow rule reads the command as written only.
		// Past the wrappers Claude Code strips, and no others.
		{cmd: "timeout 60 pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: "timeout -s KILL 60 nice -n 5 pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: "time pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: "time -p pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "nohup command pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone}, // command after nohup is an executable
		{cmd: "command pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		{cmd: "env pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "timeout 60 CI=1 pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		// Read/Edit rules restrict what Bash commands read: never allow.
		{cmd: "pnpm test | cat secrets.txt", allow: []string{"Bash(pnpm test:*)", "Bash(cat:*)"}, incomplete: true, want: VerdictNone},
		// Filters with a file operand or non-portable options need a rule.
		{cmd: "pnpm test | cat secrets.txt", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test | grep x out.log", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test | tail -r", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test --run", allow: []string{"Bash(pnpm test)"}, want: VerdictNone},
		{cmd: "pnpm test 2>&1", allow: []string{"Bash(pnpm test)"}, want: VerdictAllow},
		{cmd: "CI=1 pnpm test", allow: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "CI=1 pnpm test", allow: []string{"Bash(CI=1 pnpm test:*)"}, want: VerdictAllow},
		// Ask and deny read past wrappers and assignments, and as prefixes.
		{cmd: "make build", ask: []string{"Bash(make build:*)"}, want: VerdictAsk, rule: "Bash(make build:*)"},
		{cmd: "timeout 600 make build", ask: []string{"Bash(make build)"}, want: VerdictAsk},
		{cmd: "pnpm test", allow: []string{"Bash(pnpm test:*)"}, ask: []string{"Bash(pnpm:*)"}, want: VerdictAsk},
		{cmd: "pnpm test", deny: []string{"Bash(pnpm test:*)"}, want: VerdictDeny},
		{cmd: "CI=1 env X=2 pnpm test", deny: []string{"Bash(pnpm test)"}, want: VerdictDeny},
		{cmd: "nice -n 5 time pnpm test", deny: []string{"Bash(pnpm test:*)"}, want: VerdictNone}, // time not first: not peeled
		{cmd: "time nice -n 5 pnpm test", deny: []string{"Bash(pnpm test:*)"}, want: VerdictDeny},
		{cmd: "pnpm test 2>&1 | tail", deny: []string{"Bash(tail:*)"}, want: VerdictDeny},
		{cmd: "cd sub && pnpm test", deny: []string{"Bash(cd:*)"}, want: VerdictDeny},
		{cmd: "pnpm test", deny: []string{"Bash"}, allow: []string{"Bash(pnpm test:*)"}, want: VerdictDeny},
		{cmd: "pnpm test", deny: []string{"Bash(pnpm test:*)"}, ask: []string{"Bash(pnpm:*)"}, want: VerdictDeny},
		{cmd: "direnv exec . pnpm test", deny: []string{"Bash(pnpm test:*)"}, prefixes: []string{"direnv exec ."}, want: VerdictDeny},
		{cmd: "direnv exec . pnpm test", deny: []string{"Bash(pnpm test:*)"}, want: VerdictNone},
		{cmd: "pnpm test |& tail", deny: []string{"Bash(tail:*)"}, want: VerdictDeny},
		{cmd: "pnpm test |& tail", allow: []string{"Bash(pnpm test:*)"}, want: VerdictAllow},
		// Pipes inside the list: each stage is a part.
		{cmd: "go vet | grep -c x; go test", allow: []string{"Bash(go vet:*)", "Bash(go test:*)"}, want: VerdictAllow},
		{cmd: "go vet | grep -c x; go test", allow: []string{"Bash(go test:*)"}, want: VerdictNone},
		{cmd: "go vet | grep -c x; go test", deny: []string{"Bash(grep:*)"}, want: VerdictDeny},
		{cmd: "gofmt -l . && go test", allow: []string{"Bash(go test:*)"}, want: VerdictNone},
		{cmd: "pnpm test >/dev/null 2>&1", allow: []string{"Bash(pnpm test)"}, want: VerdictAllow},
	} {
		r := rules(tc.allow, tc.ask, tc.deny)
		r.complete = !tc.incomplete
		got := checkPermissions(tc.cmd, tc.prefixes, r)
		if got.Verdict != tc.want || tc.rule != "" && got.Rule != tc.rule {
			t.Errorf("%q (allow %q ask %q deny %q) = %v %q, want %v %q", tc.cmd, tc.allow, tc.ask, tc.deny, got.Verdict, got.Rule, tc.want, tc.rule)
		}
	}
}

func writeSettings(t *testing.T, path string, perms map[string][]string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.Marshal(map[string]any{"permissions": perms})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPermRules(t *testing.T) {
	isolate(t, false)
	dir := projectDir(t, "")
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	writeSettings(t, GlobalSettings(), map[string][]string{"allow": {"Bash(pnpm test:*)", "Read"}})
	writeSettings(t, filepath.Join(dir, ".claude", "settings.json"), map[string][]string{"ask": {"Bash(make build)"}})
	writeSettings(t, filepath.Join(dir, ".claude", "settings.local.json"), map[string][]string{"deny": {"Bash"}})
	writeSettings(t, managedSettings(), map[string][]string{"deny": {"Bash(rm:*)"}})
	r := loadPermRules(dir)
	if !r.complete || len(r.allow) != 1 || len(r.ask) != 1 || len(r.deny) != 2 || r.deny[0].pattern != "rm:*" || r.deny[1].pattern != "*" {
		t.Fatalf("rules = %+v", r)
	}
	// An unreadable file may hold the rule that matters.
	os.WriteFile(GlobalSettings(), []byte("{not json"), 0o644)
	if r := loadPermRules(dir); r.complete {
		t.Fatal("a broken settings file counted as read")
	}
}

// The hook passes the user's rules about the command as written on to the
// rewritten one.
func TestHandleClaudePermissions(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "")
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	local := filepath.Join(dir, ".claude", "settings.local.json")
	ev := func(cmd, mode string) map[string]any {
		return map[string]any{"tool_name": "Bash", "cwd": dir, "permission_mode": mode, "tool_input": map[string]any{"command": cmd}}
	}

	// No rule: Claude Code judges the rewrite.
	res, raw := runHook(t, ev("pnpm test", "default"))
	if res.H.Updated["command"] != "spotexec run -- 'pnpm test'" || res.H.Decision != "" {
		t.Fatalf("no rules: %s", raw)
	}

	writeSettings(t, local, map[string][]string{"allow": {"Bash(pnpm test:*)"}, "ask": {"Bash(make build:*)"}, "deny": {"Bash(cargo test:*)"}})
	res, raw = runHook(t, ev("pnpm test 2>&1 | tail -50", "default"))
	if res.H.Decision != "allow" || res.H.Updated["command"] != "spotexec run -- 'pnpm test 2>&1' | tail -50" ||
		!strings.Contains(res.H.Reason, "Bash(pnpm test:*)") {
		t.Fatalf("allowed: %s", raw)
	}
	// Plan mode never gets an allow from the hook.
	if res, raw = runHook(t, ev("pnpm test", "plan")); res.H.Decision != "" || res.H.Updated == nil {
		t.Fatalf("plan mode: %s", raw)
	}
	// An ask rule asks, about the rewritten command.
	res, raw = runHook(t, ev("make build", "bypassPermissions"))
	if res.H.Decision != "ask" || res.H.Updated["command"] != "spotexec run -- 'make build'" || !strings.Contains(res.H.Reason, "Bash(make build:*)") {
		t.Fatalf("ask: %s", raw)
	}
	// A deny rule leaves the command alone, for Claude Code to deny.
	if _, raw = runHook(t, ev("timeout 600 cargo test", "default")); raw != "" {
		t.Fatalf("deny: %s", raw)
	}
	// Even in block mode, which would otherwise tell the agent to re-run
	// the command through spotexec.
	blockDir := projectDir(t, "[hook]\nmode = \"block\"\n")
	writeSettings(t, filepath.Join(blockDir, ".claude", "settings.local.json"), map[string][]string{"deny": {"Bash(cargo test:*)"}})
	t.Setenv("CLAUDE_PROJECT_DIR", blockDir)
	e := ev("cargo test", "default")
	e["cwd"] = blockDir
	if _, raw = runHook(t, e); raw != "" {
		t.Fatalf("deny in block mode: %s", raw)
	}
}

// The hook reads the command as bash does, and looks past wrappers, global
// options and paths, end to end.
func TestHandleClaudeNormalizesAndPeels(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "[hook]\ntransparent_prefixes = [\"direnv exec .\"]\n")
	for in, want := range map[string]string{
		"pnpm test  # run the suite":   "spotexec run -- 'pnpm test'",
		"pnpm lint\npnpm test":         "spotexec run -- 'pnpm lint; pnpm test'",
		"cargo test \\\n  --workspace": "spotexec run -- 'cargo test   --workspace'",
		"timeout 600 pnpm test":        "spotexec run -- 'timeout 600 pnpm test'",
		"direnv exec . pnpm test":      "spotexec run -- 'direnv exec . pnpm test'",
		"pnpm --filter web test":       "spotexec run -- 'pnpm --filter web test'",
		".venv/bin/pytest -x || true":  "spotexec run -- '.venv/bin/pytest -x || true'",
		dir + "/.venv/bin/pytest":      "spotexec run -- '" + dir + "/.venv/bin/pytest'",
		"cargo test |& tail -20":       "spotexec run -- 'cargo test 2>&1' | tail -20",
		"rtk cargo test # compact":     "spotexec run --rtk-filter cargo-test -- 'cargo test'",
	} {
		res, raw := runHook(t, map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": in}})
		if res.H.Updated["command"] != want {
			t.Errorf("%q: got %s, want %q", in, raw, want)
		}
	}
}
