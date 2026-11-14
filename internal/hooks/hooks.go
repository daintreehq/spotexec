// Package hooks implements deterministic agent command interception: a
// Claude Code PreToolUse hook that wraps heavy commands (tests, type checks,
// formatters) with `spotexec run --`, and an AGENTS.md snippet for other
// harnesses.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/daintreehq/spotexec/internal/config"
)

// DefaultPatterns match command segments worth offloading.
var DefaultPatterns = []string{
	`^(npx |pnpm (exec |dlx )?|yarn (run )?|bunx |bun x |npm exec )?(vitest|jest|mocha|ava|tsc|prettier|eslint|biome|playwright test|nx (run-many|affected|test))\b`,
	`^(pnpm|npm|yarn|bun) (run )?(test|tests|test:[\w:-]+|lint|lint:[\w:-]+|typecheck|type-check|tsc|check|format|fmt|build|verify|ci)\b`,
	`^(npm|pnpm|yarn|bun) t\b`,
	`^(python3? -m )?pytest\b`,
	`^(uv run|poetry run|pipenv run) (pytest|ruff|mypy|pyright|black)\b`,
	`^(ruff|mypy|pyright|black|tox|nox)\b`,
	`^cargo (test|nextest|clippy|build|check|fmt)\b`,
	`^go (test|vet|build)\b`,
	`^(make|just) (test|check|lint|build|ci)\b`,
	`^(bundle exec )?(rspec|rake test|rails test)\b`,
	`^(mvn|\./mvnw)( -[\w.=-]+)* (clean )?(test|verify|compile|test-compile|package|checkstyle:check|spotless:check)\b`,
	`^(gradle|\./gradlew)( -[\w.=-]+)* (clean )?(test|check|build|compileJava|compileKotlin|lint|spotlessCheck)\b`,
}

// DefaultExclude keeps watch modes, dev servers and anything already wrapped
// local.
//
// Words are matched as whole command words, not inside paths: a test file
// under ~/dev/ or src/watch.test.ts is not a dev server.
var DefaultExclude = []string{
	`--watch\b`, `(^|\s|:)(watch|serve)(\s|:|$)`, `(^|\s)(dev|start)(\s|:|$)`, `--ui\b`, `(^|\s)sx(\s|$)`, `(^|[\s/])spotexec(\s|$)`,
	// The agent's explicit opt-out for one command.
	`(^|\s)SPOTEXEC_LOCAL=1(\s|$)`,
}

var (
	trailingMerge = regexp.MustCompile(`\s+2>&1$`)
	cdSeg         = regexp.MustCompile(`^cd\s+\S+$`)
	envSeg        = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*=\S*\s+)+`)
)

// Decider decides which commands to offload.
type Decider struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// NewDecider compiles defaults plus user patterns.
func NewDecider(h config.Hook) (*Decider, error) {
	d := &Decider{}
	for _, p := range append(append([]string{}, DefaultPatterns...), h.Patterns...) {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("hook pattern %q: %w", p, err)
		}
		d.include = append(d.include, re)
	}
	for _, p := range append(append([]string{}, DefaultExclude...), h.Exclude...) {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("hook exclude %q: %w", p, err)
		}
		d.exclude = append(d.exclude, re)
	}
	return d, nil
}

// ShouldOffload reports whether a Bash command line should run remotely.
// Every `&&`/`;`-separated segment must be either a heavy command or a
// harmless prefix (cd, env assignment). The line may end in a pipeline of
// simple output filters (`| tail -50`, `| grep FAIL`), which keep running
// locally on the streamed output. Redirects into files, subshells,
// background jobs and anything else keep the command local.
func (d *Decider) ShouldOffload(command string) bool {
	head, _, ok := splitFilters(command)
	return ok && d.shouldOffload(head)
}

// outputFilter matches commands that only transform text on stdin.
var outputFilter = regexp.MustCompile(`^(tail|head|grep|egrep|fgrep|rg|sed|awk|wc|sort|uniq|cut|tr|cat|tee|column|jq|less|more|nl|fold|ts)(\s|$)`)

// splitFilters splits "heavy | filter | filter" at unquoted single pipes.
// ok is false when a part after the first isn't a plain output filter.
func splitFilters(command string) (head, filters string, ok bool) {
	parts, ok := splitPipes(strings.TrimSpace(command))
	if !ok {
		return "", "", false
	}
	if len(parts) == 1 {
		return parts[0], "", true
	}
	for _, f := range parts[1:] {
		f = strings.TrimSpace(f)
		if !outputFilter.MatchString(f) || hasUnquoted(f, "<>;&`$(){}\n") {
			return "", "", false
		}
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(strings.Join(parts[1:], "|")), true
}

// splitPipes splits at unquoted "|" (not "||"). ok is false for unbalanced
// quotes or an empty part.
func splitPipes(c string) ([]string, bool) {
	var parts []string
	var q rune
	start := 0
	for i := 0; i < len(c); i++ {
		ch := rune(c[i])
		switch {
		case q != 0:
			if ch == q {
				q = 0
			} else if ch == '\\' && q == '"' {
				i++
			}
		case ch == '\'' || ch == '"':
			q = ch
		case ch == '\\':
			i++
		case ch == '|':
			if i+1 < len(c) && c[i+1] == '|' {
				i++ // "||" is a control operator, not a pipe
				continue
			}
			parts = append(parts, c[start:i])
			start = i + 1
		}
	}
	if q != 0 {
		return nil, false
	}
	parts = append(parts, c[start:])
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return nil, false
		}
	}
	return parts, true
}

// hasUnquoted reports whether s has any of chars outside quotes.
func hasUnquoted(s, chars string) bool {
	var q rune
	for i := 0; i < len(s); i++ {
		ch := rune(s[i])
		switch {
		case q != 0:
			if ch == q {
				q = 0
			}
		case ch == '\'' || ch == '"':
			q = ch
		case strings.ContainsRune(chars, ch):
			return true
		}
	}
	return false
}

func (d *Decider) shouldOffload(command string) bool {
	// A trailing 2>&1 (agents add it to nearly everything) only merges the
	// streams: harmless, and it stays part of the offloaded command.
	c := strings.TrimSpace(trailingMerge.ReplaceAllString(strings.TrimSpace(command), ""))
	if c == "" || strings.ContainsAny(c, "|<>`$(){}\n&") && !onlyAndAnd(c) {
		return false
	}
	for _, re := range d.exclude {
		if re.MatchString(c) {
			return false
		}
	}
	heavy := false
	for _, seg := range splitSegments(c) {
		seg = strings.TrimSpace(envSeg.ReplaceAllString(strings.TrimSpace(seg), ""))
		if seg == "" {
			continue
		}
		if cdSeg.MatchString(seg) {
			continue
		}
		matched := false
		for _, re := range d.include {
			if re.MatchString(seg) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
		heavy = true
	}
	return heavy
}

// onlyAndAnd allows '&' only as part of "&&" (and no other metacharacters).
func onlyAndAnd(c string) bool {
	if strings.ContainsAny(c, "|<>`$(){}\n") {
		return false
	}
	return !strings.Contains(strings.ReplaceAll(c, "&&", ""), "&")
}

func splitSegments(c string) []string {
	c = strings.ReplaceAll(c, "&&", "\x00")
	c = strings.ReplaceAll(c, ";", "\x00")
	return strings.Split(c, "\x00")
}

// Wrap returns the offloaded form of command. Trailing output filters stay
// local, reading the remote command's streamed output; spotexec's own
// progress is silenced so it can't mix into what they read.
func Wrap(bin, command string) string {
	head, filters, ok := splitFilters(command)
	if ok && filters != "" {
		return bin + " run -q -- " + shellQuote(head) + " | " + filters
	}
	return bin + " run -- " + shellQuote(command)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// claudeInput is the PreToolUse stdin payload (fields we use).
type claudeInput struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	Cwd       string         `json:"cwd"`
	Extra     map[string]any `json:"-"`
}

// HandleClaude processes one PreToolUse event. It never fails the tool call:
// on any problem it prints nothing and exits 0, leaving the command as is.
func HandleClaude(in io.Reader, out io.Writer, bin string) {
	var ev claudeInput
	if err := json.NewDecoder(in).Decode(&ev); err != nil || ev.ToolName != "Bash" {
		return
	}
	cmd, _ := ev.ToolInput["command"].(string)
	if cmd == "" {
		return
	}
	if bg, _ := ev.ToolInput["run_in_background"].(bool); bg {
		return // long-lived background jobs stay local
	}
	dir := ev.Cwd
	if dir == "" {
		dir, _ = os.Getwd()
	}
	h, ok := projectHook(dir)
	if !ok || h.Disabled {
		return
	}
	d, err := NewDecider(h)
	if err != nil || !d.ShouldOffload(cmd) {
		return
	}
	// `cd` must stay inside this repository: sx syncs only this worktree.
	if !cdsInside(cmd, dir) {
		return
	}
	if h.WhenLoadAbove > 0 {
		if l, err := LoadPerCPU(); err != nil || l < h.WhenLoadAbove {
			return
		}
	}
	wrapped := Wrap(bin, cmd)
	if h.Mode == "block" {
		writeJSON(out, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": "This project offloads heavy commands to remote runners. Re-run it as: " + wrapped + " (or prefix SPOTEXEC_LOCAL=1 if it must run on this machine, e.g. it needs local services)",
		}})
		return
	}
	newInput := map[string]any{}
	for k, v := range ev.ToolInput {
		newInput[k] = v
	}
	newInput["command"] = wrapped
	writeJSON(out, map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  newInput,
	}})
}

// CdsInside reports whether every `cd` stays in the repository (see cdsInside).
func CdsInside(command, dir string) bool { return cdsInside(command, dir) }

// cdsInside reports whether every `cd` in the command stays within the git
// worktree (or project directory) containing dir, following the cds in order.
func cdsInside(command, dir string) bool {
	root := dir
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		root = strings.TrimSpace(string(out))
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	cur := dir
	if r, err := filepath.EvalSymlinks(cur); err == nil {
		cur = r
	}
	for _, seg := range splitSegments(command) {
		seg = strings.TrimSpace(envSeg.ReplaceAllString(strings.TrimSpace(seg), ""))
		if !cdSeg.MatchString(seg) {
			continue
		}
		target := strings.TrimSpace(strings.TrimPrefix(seg, "cd"))
		// Unwrap one level of matching quotes; anything still quoted or
		// escaped is too clever to reason about: keep it local.
		if len(target) >= 2 && (target[0] == '"' || target[0] == '\'') && target[len(target)-1] == target[0] {
			target = target[1 : len(target)-1]
		}
		if target == "" || strings.ContainsAny(target, "'\"\\$`") || strings.HasPrefix(target, "~") || strings.HasPrefix(target, "-") {
			return false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(cur, target)
		}
		target = filepath.Clean(target)
		if r, err := filepath.EvalSymlinks(target); err == nil {
			target = r
		}
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return false
		}
		cur = target
	}
	return true
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	w.Write(append(b, '\n'))
}

// projectHook loads [hook] for the project containing dir. Offloading only
// happens in projects that opted in with a .spotexec.toml.
func projectHook(dir string) (config.Hook, bool) {
	root := dir
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		root = strings.TrimSpace(string(out))
	}
	p := config.FindProject(dir, root)
	if p == "" {
		return config.Hook{}, false
	}
	proj, err := config.LoadProject(p)
	if err != nil {
		return config.Hook{}, false
	}
	return proj.Hook, true
}

// LoadPerCPU returns the 1-minute load average divided by CPU count.
func LoadPerCPU() (float64, error) {
	var l float64
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return 0, err
		}
		l, err = strconv.ParseFloat(strings.Fields(string(b))[0], 64)
		if err != nil {
			return 0, err
		}
	default:
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, err
		}
		f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
		if len(f) == 0 {
			return 0, errors.New("unexpected vm.loadavg")
		}
		l, err = strconv.ParseFloat(f[0], 64)
		if err != nil {
			return 0, err
		}
	}
	return l / float64(runtime.NumCPU()), nil
}

// InstallClaude adds the PreToolUse hook to a Claude Code settings file,
// preserving everything else. It is idempotent.
func InstallClaude(settingsPath, hookCmd string) (bool, error) {
	settings := map[string]any{}
	if b, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &settings); err != nil {
			return false, fmt.Errorf("%s is not valid JSON: %w", settingsPath, err)
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	pre, _ := hooks["PreToolUse"].([]any)
	for _, e := range pre {
		em, _ := e.(map[string]any)
		hs, _ := em["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); strings.Contains(c, "hook claude") && strings.Contains(c, "spotexec") || strings.HasSuffix(c, "sx hook claude") {
				return false, nil
			}
		}
	}
	pre = append(pre, map[string]any{
		"matcher": "Bash",
		"hooks":   []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}},
	})
	hooks["PreToolUse"] = pre
	settings["hooks"] = hooks
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(settingsPath, append(b, '\n'), 0o644)
}

// UninstallClaude removes spotexec hook entries.
func UninstallClaude(settingsPath string) (bool, error) {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	settings := map[string]any{}
	if err := json.Unmarshal(b, &settings); err != nil {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	var kept []any
	removed := false
	for _, e := range pre {
		em, _ := e.(map[string]any)
		hs, _ := em["hooks"].([]any)
		var keepHooks []any
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); strings.Contains(c, "hook claude") {
				removed = true
				continue
			}
			keepHooks = append(keepHooks, h)
		}
		if len(keepHooks) > 0 {
			em["hooks"] = keepHooks
			kept = append(kept, em)
		}
	}
	if !removed {
		return false, nil
	}
	if len(kept) == 0 {
		delete(hooks, "PreToolUse")
	} else {
		hooks["PreToolUse"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	out, _ := json.MarshalIndent(settings, "", "  ")
	return true, os.WriteFile(settingsPath, append(out, '\n'), 0o644)
}

// AgentsSnippet is guidance for harnesses without hooks.
const AgentsSnippet = `## Running tests, type checks and formatters

This repository offloads heavy commands to remote runners with spotexec.
Prefix test suites, type checkers, linters, formatters and builds with ` + "`sx`" + `
(or ` + "`spotexec run --`" + `), for example:

    sx pnpm test
    sx pnpm tsc --noEmit
    sx pnpm prettier --write .

Files changed remotely (formatting, snapshot updates, codegen) are written back
to this worktree automatically. Keep fast, interactive commands local (git,
ls, rg, editors, dev servers). ` + "`sx -h`" + ` lists the flags and this project's named tasks.

Each ` + "`sx`" + ` may land on a different machine. To come back to the one your last
command used (say, after installing a package there), add ` + "`--same-slot`" + `:

    sx --same-slot 'sudo apt-get install -y libvips-dev'
    sx --same-slot pnpm test
`
