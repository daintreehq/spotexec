package hooks

import (
	"bufio"
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

// Lines whose offloaded form would mean something else stay local.
func TestShouldOffloadKeepsMeaning(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	outside := t.TempDir()
	os.Mkdir(filepath.Join(root, "inside"), 0o755)
	os.Symlink(outside, filepath.Join(root, "linked"))
	os.Mkdir(filepath.Join(outside, "child"), 0o755)
	os.Symlink(filepath.Join(outside, "child"), filepath.Join(root, "deep"))
	d, _ := NewDecider(config.Hook{})
	d.root, d.dir = root, root
	yes := []string{
		"make test -C inside", "make -C inside test", "go test; echo \"$?\"", "time go test",
		"go build && go test | tail -n 5", "go build && go test | grep -c -E 'a|b'", "go build && go test | head -3",
		"go build && go test | wc -l", "go build && go test | cut -c1-20", "go build && go test | tr a-z A-Z",
		"go test 2>&1", "go test >&2", "command go test", "go build && time go test | tail -1",
		"go vet | grep -v -e ok -e PASS; go test", "go test >/dev/null 2>&1",
		"time command go test", "command command go test", "cd inside && make test -C ..",
		"go build && go test | grep 'a*b'", "make -j8 -k test", "go test -run TestCache ./...",
	}
	no := []string{
		"cd sub >/dev/null && go test", "cd ../other >/dev/null; go test",
		"go build && go test | grep FAIL || cd sub", `go build && go test | grep "$(uname -s)"`,
		"make -j8 test -C ../outside", "make test -C /tmp", "make -C linked test", "make test --directory=linked",
		"go build && go test | tail -r", "go build && go test | cat /etc/hosts", "go build && go test | grep -P x",
		"go build && go test | grep x out.log", "go build && go test | sort", "go build && go test | sed s/a/b/",
		"go build && go test | head -c", "go vet | cat secrets.txt; go test",
		`echo "$?"; go test`, "true; echo $?; go test",
		"printf -v X y; go test", "go test; printf '%n' v", "go test; printf -vX y",
		"env CI=1 command go test", "nohup command go test", "nice command go test",
		"time go test | tail -1", `"time" go test`, `\time go test`,
		"go test >&3", "go test 2>&3", "go test 3>/dev/null",
		"go test\r", "go test \r && go vet",
		// Second-round review: sequential -C, bundled options, physical
		// .., a cd that may fail, globs and ~ in remote filters, %n forms.
		"make test -C inside -C ..", "make test -sC ../outside", "make -kC linked test", "make test -C deep/..",
		"cd missing; make test -C linked", "cd inside; make test -C ..", "go build && go test | grep ~", "go build && go test | grep a*",
		"go build && go test | grep [ab]", "printf '%10n' r; go test", "go test; printf '%ln' r",
	}
	for _, c := range yes {
		if ok, why := d.Why(c); !ok {
			t.Errorf("expected offload: %q (%s)", c, why)
		}
	}
	for _, c := range no {
		if d.ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
	// A wrapped redirected cd stays on the runner, so it must not be read
	// as one that is hoisted.
	if got := Wrap("sx", "cd sub 2>&1 && go test"); got != "cd sub 2>&1 && sx run -- 'go test'" {
		t.Errorf("Wrap = %q", got)
	}
}

// Nothing in the reading and rewriting panics on odd input.
func TestNoPanicOnRandomInput(t *testing.T) {
	alphabet := []string{"go", " test", " ", "|", "||", "|&", "&&", "&", ";", ";;", "'", "\"", "\\", "\n", "\r", "#",
		"$", "$?", "$(", ")", "(", "`", "<", "<<", ">", ">&", "2>&1", "/dev/null", "cd", " sub", "-C", "make", "pnpm",
		"--filter", "=", "timeout", " 5", "time", "-p", "env", "X=1", "nice", "-n", "command", "nohup", "grep", "tail",
		"-r", "echo", "printf", "-v", "%n", "{", "}", "..", "/", "*", ":*", "rtk", "uv run", "--frozen", "npx", "-y"}
	d, _ := NewDecider(config.Hook{TransparentPrefixes: []string{"direnv exec ."}})
	d.root, d.dir = t.TempDir(), t.TempDir()
	r := rand.New(rand.NewSource(1))
	rules := permRules{complete: true, allow: bashRules([]any{"Bash(go test:*)", "Bash(git:* push)", "Bash(* --help)"}, "f"),
		ask: bashRules([]any{"Bash(make *)"}, "f"), deny: bashRules([]any{"Bash(rm:*)"}, "f")}
	for i := 0; i < 30000; i++ {
		var b strings.Builder
		for n := r.Intn(14); n >= 0; n-- {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		c := b.String()
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic on %q: %v", c, p)
				}
			}()
			n := Normalize(c)
			line, _, _ := UnwrapRTK(n)
			if d.ShouldOffload(line) {
				Wrap("sx", line)
				WrapRTK("sx", line, true)
			}
			checkPermissions(n, d.prefixes, rules)
			d.mentionsHeavy(n)
			runsSpotexec(n)
			portableFilter(c)
			toolForm(c, place{d.root, d.dir})
		}()
	}
}

func TestPermissionSources(t *testing.T) {
	isolate(t, false)
	dir := projectDir(t, "")
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	writeSettings(t, GlobalSettings(), map[string][]string{"allow": {"Bash(cargo test:*)"}})
	if r := loadPermRules(dir); !r.complete || len(r.allow) != 1 {
		t.Fatalf("user allow: %+v", r)
	}
	// A managed drop-in counts like the managed file.
	writeSettings(t, filepath.Join(filepath.Dir(managedSettings()), "managed-settings.d", "20-security.json"), map[string][]string{"deny": {"Bash(cargo test:*)"}})
	if p := checkPermissions("cargo test", nil, loadPermRules(dir)); p.Verdict != VerdictDeny || !strings.Contains(p.File, "20-security.json") {
		t.Fatalf("drop-in deny: %+v", p)
	}
	os.RemoveAll(filepath.Join(filepath.Dir(managedSettings()), "managed-settings.d"))
	// Managed rules only: the user's allow no longer counts.
	os.WriteFile(managedSettings(), []byte(`{"allowManagedPermissionRulesOnly": true, "permissions": {"allow": ["Bash(go test:*)"]}}`), 0o644)
	r := loadPermRules(dir)
	if p := checkPermissions("cargo test", nil, r); p.Verdict != VerdictNone {
		t.Fatalf("managed only, user allow: %+v", p)
	}
	if p := checkPermissions("go test", nil, r); p.Verdict != VerdictAllow {
		t.Fatalf("managed only, managed allow: %+v", p)
	}
	os.Remove(managedSettings())
	// An MDM policy the hook can't read: nothing is auto-allowed.
	os.WriteFile(mdmPolicies()[0], []byte("<plist/>"), 0o644)
	if r := loadPermRules(dir); r.complete {
		t.Fatal("an MDM policy counted as read")
	}
	os.Remove(mdmPolicies()[0])
	// Nor when a settings file can't be read.
	if os.Getuid() != 0 {
		os.Chmod(GlobalSettings(), 0)
		if r := loadPermRules(dir); r.complete {
			t.Fatal("an unreadable settings file counted as read")
		}
		os.Chmod(GlobalSettings(), 0o644)
	}
	// Read/Edit ask or deny rules: no allow (an allow Read rule is fine).
	writeSettings(t, GlobalSettings(), map[string][]string{"allow": {"Bash(cargo test:*)", "Read"}, "deny": {"Read(./secrets.txt)"}})
	if r := loadPermRules(dir); r.complete {
		t.Fatal("a Read deny rule counted as nothing")
	}
	writeSettings(t, GlobalSettings(), map[string][]string{"allow": {"Bash(cargo test:*)", "Read"}})
	if r := loadPermRules(dir); !r.complete {
		t.Fatal("an allow Read rule made the rules incomplete")
	}
}

// In a linked worktree, Claude Code keeps the local settings in the main
// checkout.
func TestPermissionSourcesWorktree(t *testing.T) {
	isolate(t, false)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	main := t.TempDir()
	git := func(dir string, args ...string) {
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(main, "init", "-q")
	git(main, "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(t.TempDir(), "wt")
	git(main, "worktree", "add", "-q", wt)
	writeSettings(t, filepath.Join(main, ".claude", "settings.local.json"), map[string][]string{"deny": {"Bash(cargo test:*)"}})
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	if p := checkPermissions("cargo test", nil, loadPermRules(wt)); p.Verdict != VerdictDeny {
		t.Fatalf("main checkout's local deny missed: %+v", p)
	}
	roots := Worktrees(wt)
	if len(roots) != 2 {
		t.Fatalf("Worktrees = %q", roots)
	}
}

// A denied command is left alone, and never handed to a chained hook that
// could rewrite it past the rule; an ask rule in block mode leaves it alone
// too (the retry through spotexec wouldn't match it).
func TestHandleClaudeDenyAndAskPaths(t *testing.T) {
	isolate(t, true)
	dir := projectDir(t, "")
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	writeSettings(t, filepath.Join(dir, ".claude", "settings.local.json"), map[string][]string{"deny": {"Bash(cargo test:*)"}, "ask": {"Bash(make build:*)"}})
	marker := filepath.Join(t.TempDir(), "chained")
	then := "touch " + marker + "; echo '{}'"
	run := func(cwd, cmd string) (int, string) {
		in := []byte(`{"tool_name":"Bash","cwd":"` + cwd + `","tool_input":{"command":"` + cmd + `"}}`)
		var out, errOut bytes.Buffer
		code := RunClaude(bytes.NewReader(in), &out, &errOut, ClaudeOptions{Bin: "spotexec", Then: then})
		return code, out.String()
	}
	if code, out := run(dir, "cargo test"); code != 0 || out != "" {
		t.Fatalf("deny: %d %q", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a denied command reached the chained hook")
	}
	// Not denied: the chained hook gets what spotexec leaves alone.
	run(dir, "git status")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the chained hook never ran")
	}
	block := projectDir(t, "[hook]\nmode = \"block\"\n")
	writeSettings(t, filepath.Join(block, ".claude", "settings.local.json"), map[string][]string{"ask": {"Bash(make build:*)"}})
	t.Setenv("CLAUDE_PROJECT_DIR", block)
	if res, raw := runHook(t, map[string]any{"tool_name": "Bash", "cwd": block, "tool_input": map[string]any{"command": "make build"}}); raw != "" || res.H.Decision != "" {
		t.Fatalf("ask in block mode: %s", raw)
	}
	// Without the ask rule, block mode denies with instructions.
	writeSettings(t, filepath.Join(block, ".claude", "settings.local.json"), map[string][]string{})
	if res, raw := runHook(t, map[string]any{"tool_name": "Bash", "cwd": block, "tool_input": map[string]any{"command": "make build"}}); !strings.Contains(res.H.Reason, "Re-run it as: spotexec run -- 'make build'") {
		t.Fatalf("block: %s", raw)
	}
}

func TestDiscoverEdgeCases(t *testing.T) {
	isolate(t, false)
	projects := t.TempDir()
	root := filepath.Join(t.TempDir(), strings.Repeat("long-directory-name-", 12))
	os.MkdirAll(root, 0o755)
	t.Setenv("CLAUDE_PROJECT_DIR", root)
	writeSettings(t, filepath.Join(root, ".claude", "settings.local.json"), map[string][]string{"deny": {"Bash(cargo test:*)"}})
	now := time.Now()
	// Claude Code cuts a long name at 200 characters and adds a hash.
	name := projectDirName(root)[:maxDirName] + "-1a2b3c"
	path := filepath.Join(projects, name, "s.jsonl")
	transcript(t, path,
		call{id: "1", cmd: "pnpm test", cwd: root, at: now},
		call{id: "2", cmd: "git commit -m 'retry && pnpm test'", cwd: root, at: now},
		call{id: "3", cmd: "cargo test", cwd: root, at: now},
	)
	// A malformed line naming Bash between valid ones, and a later call.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"` + "\n")
	f.Close()
	b, _ := os.ReadFile(path)
	transcript(t, path+".tmp", call{id: "4", cmd: "go test ./...", cwd: root, at: now})
	extra, _ := os.ReadFile(path + ".tmp")
	os.Remove(path + ".tmp")
	os.WriteFile(path, append(b, extra...), 0o644)

	d, _ := NewDecider(config.Hook{})
	res, err := Discover(projects, []string{root}, now.Add(-time.Hour), d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commands != 4 || res.Offloaded != 2 || len(res.Missed) != 1 || !strings.Contains(res.Missed[0].Reason, "deny rule Bash(cargo test:*)") {
		t.Fatalf("got %+v", res)
	}
}

func TestReadLineSkipsHugeLines(t *testing.T) {
	in := strings.Repeat("x", 5000) + "\nshort\n" + strings.Repeat("y", 70) + "\nlast"
	br := bufio.NewReaderSize(strings.NewReader(in), 16)
	var got []string
	for {
		line, err := readLine(br, 64)
		got = append(got, string(line))
		if err != nil {
			break
		}
	}
	if strings.Join(got, "|") != "|short\n||last" {
		t.Fatalf("got %q", got)
	}
}
