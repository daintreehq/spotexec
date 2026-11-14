// Package hooks implements deterministic agent command interception: a
// Claude Code PreToolUse hook that wraps heavy commands (tests, type checks,
// formatters) with `spotexec run --`, and an AGENTS.md snippet for other
// harnesses.
package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

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
//
// A pipe binds tighter than && and ;, so in `a && b | tail` only b is
// filtered. Splitting that across two runs could land them on different
// machines (b would miss a's build output), so a filtered line offloads only
// when a single heavy command (after any leading cds) feeds the filters.
func (d *Decider) ShouldOffload(command string) bool {
	if shellHazard(command) {
		return false
	}
	head, filters, ok := splitFilters(command)
	if !ok || !d.shouldOffload(head) {
		return false
	}
	segs, ops := splitList(head)
	k := 0 // leading cds, which Wrap runs locally
	for k < len(segs)-1 && isCd(segs[k]) {
		k++
	}
	for _, seg := range segs {
		if seg == "" {
			return false // `a;; b`, `a && && b`: bash refuses the line
		}
	}
	if k > 0 && ops[k-1] == "&&" && slices.Contains(ops[k:], ";") {
		// `cd x && a; b` runs b even when the cd fails; hoisted, the cd
		// would gate the whole rest of the line.
		return false
	}
	if filters != "" && len(segs)-k > 1 {
		return false
	}
	return true
}

// shellHazard reports shell syntax the splitters don't model: a comment
// (which could hide an operator) or a backslash escape outside quotes.
func shellHazard(c string) bool {
	var q byte
	for i := 0; i < len(c); i++ {
		ch := c[i]
		switch {
		case q == '"' && ch == '\\':
			i++
		case q != 0:
			if ch == q {
				q = 0
			}
		case ch == '\'' || ch == '"':
			q = ch
		case ch == '\\':
			return true
		case ch == '#' && (i == 0 || c[i-1] == ' ' || c[i-1] == '\t'):
			return true
		}
	}
	return false
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
			if heavy {
				// Offloaded, this cd would happen only remotely: the
				// agent's shell would stay where it was.
				return false
			}
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

// Wrap returns the offloaded form of command, meaning the same as the
// original line:
//   - leading `cd`s run locally, so the agent's shell (and any local filter
//     such as `tee out.log`) ends up where it asked; spotexec then runs the
//     rest from that directory, which maps to the same remote one;
//   - trailing output filters stay local, reading the remote command's
//     streamed output (ShouldOffload lets only one command feed them);
//   - spotexec's own warnings and summary go to stderr, outside the pipe.
//
// It expects a line ShouldOffload accepted.
func Wrap(bin, command string) string {
	head, filters, ok := splitFilters(command)
	if !ok {
		head, filters = strings.TrimSpace(command), ""
	}
	segs, ops := splitList(head)
	run := func(segs, ops []string) string { return bin + " run -- " + shellQuote(joinList(segs, ops)) }
	var b strings.Builder
	i := 0
	for ; i < len(segs)-1 && isCd(segs[i]); i++ {
		b.WriteString(segs[i] + listOp(ops[i]))
	}
	segs, ops = segs[i:], ops[i:]
	if filters == "" {
		b.WriteString(run(segs, ops))
		return b.String()
	}
	b.WriteString(run(segs, ops) + " | " + filters)
	return b.String()
}

func isCd(seg string) bool {
	return cdSeg.MatchString(strings.TrimSpace(envSeg.ReplaceAllString(seg, "")))
}

// splitList splits a command list at unquoted "&&" and ";" (the only list
// operators ShouldOffload lets through), returning the trimmed segments and
// the operators between them.
func splitList(c string) (segs, ops []string) {
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
		case ch == ';' || ch == '&' && i+1 < len(c) && c[i+1] == '&':
			segs = append(segs, strings.TrimSpace(c[start:i]))
			op := ";"
			if ch == '&' {
				op = "&&"
				i++
			}
			ops = append(ops, op)
			start = i + 1
		}
	}
	segs = append(segs, strings.TrimSpace(c[start:]))
	// A trailing ";" ends the list without starting another command.
	if len(segs) > 1 && segs[len(segs)-1] == "" && ops[len(ops)-1] == ";" {
		segs, ops = segs[:len(segs)-1], ops[:len(ops)-1]
	}
	return segs, ops
}

func listOp(op string) string {
	if op == ";" {
		return "; "
	}
	return " " + op + " "
}

func joinList(segs, ops []string) string {
	var b strings.Builder
	for i, s := range segs {
		if i > 0 {
			b.WriteString(listOp(ops[i-1]))
		}
		b.WriteString(s)
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// claudeInput is the PreToolUse stdin payload (fields we use).
type claudeInput struct {
	SessionID string         `json:"session_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	Cwd       string         `json:"cwd"`
}

// BashTimeoutMS is the Bash tool timeout an offloaded command gets when the
// agent set none: Claude Code's default is 2 minutes, too short for booting
// a Spot VM, installing dependencies and then running. It is Claude Code's
// default ceiling (BASH_MAX_TIMEOUT_MS raises it, and so does this).
const BashTimeoutMS = 600000

func bashTimeout() int {
	if n, err := strconv.Atoi(os.Getenv("BASH_MAX_TIMEOUT_MS")); err == nil && n > BashTimeoutMS {
		return n
	}
	return BashTimeoutMS
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
	proj, ok := project(dir)
	if !ok || proj.Hook.Disabled {
		return
	}
	h := proj.Hook
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
	if err := RunnersFor(cmd, dir, proj); err != nil {
		// Offloading would only fail: run it here, and say so once a
		// session (a hook installed before `spotexec setup` would otherwise
		// keep a first demo silently local).
		if firstNotice(ev.SessionID) {
			writeJSON(out, map[string]any{
				"systemMessage": "spotexec: heavy commands are running on this machine: " + err.Error(),
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "PreToolUse",
					"additionalContext": "spotexec kept this command on this machine instead of offloading it, because it has no usable runners (" + err.Error() + "). Heavy commands keep running locally until that is fixed.",
				},
			})
		}
		return
	}
	wrapped := Wrap(bin, cmd)
	if h.Mode == "block" {
		writeJSON(out, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": fmt.Sprintf("This project offloads heavy commands to remote runners. Re-run it as: %s with a Bash timeout of %d ms, since a cold start can take minutes (or prefix SPOTEXEC_LOCAL=1 if it must run on this machine, e.g. it needs local services)", wrapped, bashTimeout()),
		}})
		return
	}
	newInput := map[string]any{}
	for k, v := range ev.ToolInput {
		newInput[k] = v
	}
	newInput["command"] = wrapped
	if t, _ := newInput["timeout"].(float64); t <= 0 {
		newInput["timeout"] = bashTimeout()
	}
	writeJSON(out, map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  newInput,
	}})
}

// RunnersFor is UsableRunners for the config the wrapped command line will
// load: spotexec starts after the line's leading cds, which run locally.
func RunnersFor(command, dir string, proj *config.Project) error {
	if rd := wrapDir(command, dir); rd != dir {
		// As spotexec run there resolves it: defaults when that worktree
		// has no .spotexec.toml.
		p, err := config.LoadProject(config.FindProject(rd, gitRoot(rd)))
		if err != nil {
			return err
		}
		proj = p
	}
	return UsableRunners(proj)
}

// UsableRunners reports why a run in this project would have no runner to
// go to (none configured, an unknown pool entry, a broken config), or nil.
func UsableRunners(proj *config.Project) error {
	g, err := config.LoadGlobal()
	if err != nil {
		return err
	}
	cfg := &config.Config{Global: g, Project: proj}
	if err := cfg.MergeProjectRunners(); err != nil {
		return err
	}
	var override []string // as spotexec run resolves it
	if r := os.Getenv("SPOTEXEC_RUNNER"); r != "" {
		override = []string{r}
	}
	_, err = cfg.Pool(override)
	return err
}

// firstNotice reports whether this session hasn't had the "staying local"
// notice yet, and records that it has. Without a session id it is once a day.
func firstNotice(session string) bool {
	if session == "" {
		session = time.Now().Format("2006-01-02")
	}
	sum := sha256.Sum256([]byte(session))
	dir := filepath.Join(config.StateDir(), "hook-notices")
	p := filepath.Join(dir, hex.EncodeToString(sum[:8]))
	if os.MkdirAll(dir, 0o700) == nil {
		// Claimed atomically: parallel tool calls of one session race here.
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			return false
		}
		if err == nil {
			f.Close()
		}
		// Forget sessions older than a week.
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
					os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	return true
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
		target, ok := cdTarget(seg, cur)
		if !ok || target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return false
		}
		cur = target
	}
	return true
}

// cdTarget resolves the directory a `cd` segment changes to from cur. ok is
// false for targets too clever to reason about.
func cdTarget(seg, cur string) (string, bool) {
	target := strings.TrimSpace(strings.TrimPrefix(seg, "cd"))
	// Unwrap one level of matching quotes; anything still quoted or
	// escaped is too clever to reason about: keep it local.
	if len(target) >= 2 && (target[0] == '"' || target[0] == '\'') && target[len(target)-1] == target[0] {
		target = target[1 : len(target)-1]
	}
	if target == "" || strings.ContainsAny(target, "'\"\\$`") || strings.HasPrefix(target, "~") || strings.HasPrefix(target, "-") {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(cur, target)
	}
	target = filepath.Clean(target)
	if r, err := filepath.EvalSymlinks(target); err == nil {
		target = r
	}
	return target, true
}

// wrapDir is the directory Wrap starts spotexec in: dir after the line's
// leading cds.
func wrapDir(command, dir string) string {
	head, _, ok := splitFilters(command)
	if !ok {
		return dir
	}
	segs, _ := splitList(head)
	cur := dir
	for i := 0; i < len(segs)-1 && isCd(segs[i]); i++ {
		t, ok := cdTarget(strings.TrimSpace(envSeg.ReplaceAllString(segs[i], "")), cur)
		if !ok {
			return dir
		}
		cur = t
	}
	return cur
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	w.Write(append(b, '\n'))
}

// project loads the .spotexec.toml for the project containing dir.
// Offloading only happens in projects that opted in with one.
func project(dir string) (*config.Project, bool) {
	p := config.FindProject(dir, gitRoot(dir))
	if p == "" {
		return nil, false
	}
	proj, err := config.LoadProject(p)
	if err != nil {
		return nil, false
	}
	return proj, true
}

// gitRoot is the worktree containing dir, or dir outside git.
func gitRoot(dir string) string {
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return dir
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

// Hook command forms in a Claude Code settings file.
const (
	hookNone    = iota
	hookCurrent // calls spotexec
	hookLegacy  // calls sx, as v0.2.0 could install it
)

// hookForm classifies a PreToolUse hook command.
func hookForm(cmd string) int {
	cmd = strings.TrimSpace(cmd)
	if bin, ok := strings.CutSuffix(cmd, " hook claude"); ok {
		bin = strings.TrimSpace(bin)
		// Only the single executable the installer wrote: a compound line
		// (`audit && sx hook claude`) is the user's own and not ours to rewrite.
		quoted := len(bin) >= 2 && (bin[0] == '\'' || bin[0] == '"') && bin[len(bin)-1] == bin[0]
		if quoted {
			bin = strings.ReplaceAll(bin[1:len(bin)-1], `'\''`, "'")
		}
		if (quoted || !strings.ContainsAny(bin, " \t;&|()<>$`'\"\\")) && filepath.Base(bin) == "sx" {
			return hookLegacy
		}
	}
	if strings.Contains(cmd, "hook claude") && strings.Contains(cmd, "spotexec") {
		return hookCurrent
	}
	return hookNone
}

// claudeHooks returns the hook objects of every PreToolUse entry.
func claudeHooks(settings map[string]any) []map[string]any {
	hooks, _ := settings["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	var out []map[string]any
	for _, e := range pre {
		em, _ := e.(map[string]any)
		hs, _ := em["hooks"].([]any)
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok {
				out = append(out, hm)
			}
		}
	}
	return out
}

func readSettings(settingsPath string) (map[string]any, bool) {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, false
	}
	var settings map[string]any
	if json.Unmarshal(b, &settings) != nil {
		return nil, false
	}
	return settings, true
}

func hasForm(settingsPath string, form int) bool {
	settings, ok := readSettings(settingsPath)
	if !ok {
		return false
	}
	for _, hm := range claudeHooks(settings) {
		if c, _ := hm["command"].(string); hookForm(c) == form {
			return true
		}
	}
	return false
}

// HasClaude reports whether a Claude Code settings file has the hook, in
// either form (a legacy `sx hook claude` entry still works).
func HasClaude(settingsPath string) bool {
	return hasForm(settingsPath, hookCurrent) || hasForm(settingsPath, hookLegacy)
}

// HasLegacyClaude reports whether a Claude Code settings file has a hook
// that calls `sx hook claude`, which InstallClaude rewrites to spotexec.
func HasLegacyClaude(settingsPath string) bool { return hasForm(settingsPath, hookLegacy) }

// InstallClaude adds the PreToolUse hook to a Claude Code settings file,
// preserving everything else. It is idempotent. A legacy `sx hook claude`
// entry is rewritten in place to hookCmd (or dropped, when a spotexec entry
// is already there). It reports whether the file changed.
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
	current, changed := false, false
	for _, e := range pre {
		em, _ := e.(map[string]any)
		hs, _ := em["hooks"].([]any)
		// Each legacy command is rewritten in place, keeping its matcher and
		// options, unless its own entry already calls spotexec.
		has := false
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); hookForm(c) == hookCurrent {
				has, current = true, true
			}
		}
		var keepHooks []any
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); hookForm(c) == hookLegacy {
				changed, current = true, true
				if has {
					continue
				}
				hm["command"] = hookCmd
				has = true
			}
			keepHooks = append(keepHooks, h)
		}
		if len(keepHooks) != len(hs) {
			em["hooks"] = keepHooks
		}
	}
	if !current {
		pre = append(pre, map[string]any{
			"matcher": "Bash",
			"hooks":   []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}},
		})
		changed = true
	}
	if !changed {
		return false, nil
	}
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
