package hooks

// RTK (github.com/rtk-ai/rtk) compacts command output for agents through a
// PreToolUse hook of its own (`rtk hook claude`) that rewrites the same heavy
// commands spotexec offloads: `cargo test` becomes `rtk cargo test`. Claude
// Code runs every matching hook on the original command at once and doesn't
// say which of two rewrites wins, so with both hooks a test run would either
// stay local (RTK's) or come back unfiltered (spotexec's), at random.
//
// So exactly one hook decides. `spotexec hook install` chains RTK's hook
// behind spotexec's (`spotexec hook claude --then 'rtk hook claude'`), and a
// command spotexec doesn't offload goes to RTK unchanged. An RTK hook that
// isn't chained still races, so a command it would rewrite is denied with the
// offloaded form to re-run instead (RTK leaves `spotexec run` lines alone).
//
// Where RTK is in use, an offloaded command's output is compacted on this
// machine by `rtk pipe`, through `spotexec run --rtk-filter`, but only for
// commands whose raw output RTK's pipe filters are known to read correctly:
// on others (pytest, plain `go test`) they report "no tests" and hide the
// failures.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// isRTKHook reports whether a hook command is RTK's own Claude Code hook,
// called by any path: `rtk hook claude`.
func isRTKHook(cmd string) bool {
	_, ok := rtkBin(cmd)
	return ok
}

// rtkBin returns the executable of an `rtk hook claude` command.
func rtkBin(cmd string) (string, bool) {
	exe, args, ok := hookExe(cmd)
	if !ok || filepath.Base(exe) != "rtk" || args != "hook claude" {
		return "", false
	}
	return exe, true
}

// hookExe splits a hook command into its executable, unquoted (a leading ~
// or $HOME expanded, as the shell would), and its arguments. ok is false
// when the command doesn't start with a plain executable.
func hookExe(cmd string) (exe, args string, ok bool) {
	exe, rest, vars, ok := shellWord(strings.TrimSpace(cmd))
	if !ok || exe == "" {
		return "", "", false
	}
	if home, _ := os.UserHomeDir(); home != "" {
		if strings.HasPrefix(exe, "~/") {
			exe = filepath.Join(home, exe[2:])
		} else if vars {
			for _, p := range []string{"$HOME/", "${HOME}/"} {
				if strings.HasPrefix(exe, p) {
					exe = filepath.Join(home, exe[len(p):])
					vars = strings.ContainsAny(exe, "$`")
					break
				}
			}
		}
	}
	if vars {
		return "", "", false // an expansion this can't follow
	}
	return exe, strings.TrimSpace(rest), true
}

// shellWord reads the first shell word of s, joining its bare, single- and
// double-quoted parts, up to unquoted whitespace or an operator. Expansions
// are kept as written, and vars says there was one ($ or ` outside single
// quotes).
func shellWord(s string) (word, rest string, vars, ok bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || strings.IndexByte(";&|()<>", c) >= 0:
			return b.String(), s[i:], vars, true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", "", false, false
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				} else if s[i] == '$' || s[i] == '`' {
					vars = true
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return "", "", false, false
			}
			i++
		case c == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		default:
			vars = vars || c == '$' || c == '`'
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), "", vars, true
}

// chainCmd is the hook command that runs spotexec's hook first and hands
// every command it leaves alone to then.
func chainCmd(hookCmd, then string) string {
	return hookCmd + " --then " + shellQuote(then)
}

// thenOf returns the hook a chained spotexec hook command hands over to.
func thenOf(cmd string) (string, bool) {
	exe, args, ok := hookExe(cmd)
	if !ok || filepath.Base(exe) != "spotexec" {
		return "", false
	}
	arg, ok := strings.CutPrefix(args, "hook claude --then ")
	if !ok {
		return "", false
	}
	then, rest, _, ok := shellWord(strings.TrimSpace(arg))
	if !ok || strings.TrimSpace(rest) != "" || then == "" {
		return "", false
	}
	return then, true
}

// HasRTK reports whether a Claude Code settings file has an RTK hook that
// isn't chained behind spotexec's, which InstallClaude would chain.
func HasRTK(settingsPath string) bool {
	settings, ok := readSettings(settingsPath)
	if !ok {
		return false
	}
	for _, e := range preEntries(settings) {
		for _, hm := range e.hooks {
			if c, _ := hm["command"].(string); isRTKHook(c) && matchesBash(e.matcher) {
				return true
			}
		}
	}
	return false
}

type preEntry struct {
	matcher string
	hooks   []map[string]any
}

func preEntries(settings map[string]any) []preEntry {
	hooks, _ := settings["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	var out []preEntry
	for _, e := range pre {
		em, _ := e.(map[string]any)
		m, _ := em["matcher"].(string)
		var hs []map[string]any
		list, _ := em["hooks"].([]any)
		for _, h := range list {
			if hm, ok := h.(map[string]any); ok {
				hs = append(hs, hm)
			}
		}
		out = append(out, preEntry{m, hs})
	}
	return out
}

// matchesBash reports whether a PreToolUse matcher covers the Bash tool, as
// Claude Code evaluates it: everything for "", "*"; a list of exact names
// separated by | or , when it has only name characters; otherwise an
// unanchored (JavaScript) regular expression. One Go can't compile counts as
// matching, so a racing hook is never missed.
func matchesBash(m string) bool {
	if m == "" || m == "*" {
		return true
	}
	if plainMatcher.MatchString(m) {
		for _, name := range strings.FieldsFunc(m, func(r rune) bool { return r == '|' || r == ',' }) {
			if strings.TrimSpace(name) == "Bash" {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(m)
	return err != nil || re.MatchString("Bash")
}

var plainMatcher = regexp.MustCompile(`^[A-Za-z0-9_\- ,|]+$`)

// rtkHook is an RTK hook found in a Claude Code settings file.
type rtkHook struct {
	bin  string // rtk's executable
	file string // the settings file
}

// rtkState is what the Claude Code settings in effect for dir say about RTK.
type rtkState struct {
	inUse     bool      // some RTK hook, chained or not
	unchained []rtkHook // RTK hooks that race spotexec's
}

// ClaudeSettingsFiles are the Claude Code settings files whose hooks apply
// in dir: the user's, then the project's shared and local ones.
func ClaudeSettingsFiles(dir string) []string {
	proj := os.Getenv("CLAUDE_PROJECT_DIR")
	if proj == "" {
		proj = gitRoot(dir)
	}
	files := []string{GlobalSettings()}
	for _, f := range []string{filepath.Join(proj, ".claude", "settings.json"), filepath.Join(proj, ".claude", "settings.local.json")} {
		if f != files[0] {
			files = append(files, f)
		}
	}
	return files
}

func scanRTK(dir string) rtkState {
	var st rtkState
	for _, f := range ClaudeSettingsFiles(dir) {
		settings, ok := readSettings(f)
		if !ok {
			continue
		}
		for _, e := range preEntries(settings) {
			if !matchesBash(e.matcher) {
				continue
			}
			for _, hm := range e.hooks {
				c, _ := hm["command"].(string)
				if bin, ok := rtkBin(c); ok {
					st.inUse = true
					st.unchained = append(st.unchained, rtkHook{bin, f})
				} else if then, ok := thenOf(c); ok && isRTKHook(then) {
					st.inUse = true
				}
			}
		}
	}
	return st
}

// racer returns the unchained RTK hook that would rewrite command, if any.
func (st rtkState) racer(command string) (rtkHook, bool) {
	for _, h := range st.unchained {
		if rtkRewrites(h.bin, command) {
			return h, true
		}
	}
	return rtkHook{}, false
}

// rtkRewriteTimeout caps the `rtk rewrite` probe (a var for tests: the first
// exec of a freshly written script can take seconds on a loaded Mac).
var rtkRewriteTimeout = 3 * time.Second

// rtkRewrites asks RTK whether its hook would rewrite command (`rtk rewrite`
// prints the rewrite, or exits 1 with nothing to say).
func rtkRewrites(bin, command string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), rtkRewriteTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "rewrite", command).Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() == 1) {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}

// rtkTool matches a segment an agent prefixed with rtk, for the tools whose
// rtk form runs the same command line (`rtk vitest` and `rtk tsc` don't:
// RTK finds the binary and adds `run`); rtkWrapper matches RTK's generic
// wrappers.
var (
	rtkTool    = regexp.MustCompile(`^rtk\s+((cargo|go|pytest|mypy|ruff|pnpm|npx)(\s|$))`)
	rtkWrapper = regexp.MustCompile(`^rtk\s+(test|err|proxy)\s+(\S.*)$`)
)

// UnwrapRTK returns command with each `rtk <tool> …` segment as the plain
// `<tool> …` RTK runs, so it can be offloaded. found says there was one, and
// raw that one was `rtk proxy …`, the agent's request for unfiltered output.
// A line it can't take apart comes back unchanged.
func UnwrapRTK(command string) (line string, found, raw bool) {
	if !strings.Contains(command, "rtk") || shellHazard(command) {
		return command, false, false
	}
	head, filters, ok := splitFilters(command)
	if !ok {
		return command, false, false
	}
	segs, ops := splitList(head)
	for i, seg := range segs {
		if seg == "" {
			return command, false, false // bash refuses `a;; b`
		}
		env := envPrefix(seg)
		rest := seg[len(env):]
		if m := rtkTool.FindStringSubmatchIndex(rest); m != nil {
			rest, found = rest[m[2]:], true
		} else if m := rtkWrapper.FindStringSubmatch(rest); m != nil {
			if isCd(m[2]) {
				// RTK's child process changes directory, not the shell.
				return command, false, false
			}
			rest, found = m[2], true
			raw = raw || m[1] == "proxy"
		}
		segs[i] = env + rest
	}
	if !found {
		return command, false, false
	}
	line = joinList(segs, ops)
	if filters != "" {
		line += " | " + filters
	}
	return line, true, raw
}

// rtkFilters are the offloaded commands whose raw output RTK's pipe filter
// reads correctly, with any flag RTK itself adds to get that output.
var rtkFilters = []struct {
	match  *regexp.Regexp
	filter string
	flag   string // inserted after the match unless already there
	has    *regexp.Regexp
	// The filter reads stdout as JSON: a trailing 2>&1 would mix compiler
	// errors into it, so it is dropped and stderr reaches the agent as is.
	stdoutOnly bool
}{
	{regexp.MustCompile(`^cargo test(\s|$)`), "cargo-test", "", nil, false},
	{regexp.MustCompile(`^go test(\s|$)`), "go-test", "-json", regexp.MustCompile(`(^|\s)--?json(=true)?(\s|$)`), true},
}

// rtkNoFilter is a line that asks for other output than the filter reads.
var rtkNoFilter = regexp.MustCompile(`(^|\s)--?json=(false|0|f|F|FALSE|False)(\s|$)`)

// rtkFilterFor returns the RTK pipe filter for a single command segment and
// the segment as it must run to feed it.
func rtkFilterFor(seg string) (filter, run string, ok bool) {
	env := envPrefix(seg)
	rest := seg[len(env):]
	if rtkNoFilter.MatchString(rest) {
		return "", "", false
	}
	for _, f := range rtkFilters {
		loc := f.match.FindStringSubmatchIndex(rest)
		if loc == nil {
			continue
		}
		if f.stdoutOnly {
			rest = trailingMerge.ReplaceAllString(rest, "")
		}
		if f.flag != "" && !f.has.MatchString(rest) {
			at := loc[2] // right after the subcommand, before its separator
			rest = rest[:at] + " " + f.flag + rest[at:]
		}
		return f.filter, env + rest, true
	}
	return "", "", false
}

// mergeNotice adds spotexec's one-time notice to a chained hook's answer,
// beside any messages of its own.
func mergeNotice(out []byte, notice map[string]any) []byte {
	var m map[string]any
	if len(bytes.TrimSpace(out)) == 0 {
		m = map[string]any{}
	} else if json.Unmarshal(out, &m) != nil || m == nil {
		return out // not an answer spotexec can add to
	}
	join := func(theirs any, ours string) string {
		if t, _ := theirs.(string); t != "" {
			return t + "\n" + ours
		}
		return ours
	}
	m["systemMessage"] = join(m["systemMessage"], notice["systemMessage"].(string))
	ours, _ := notice["hookSpecificOutput"].(map[string]any)
	theirs, ok := m["hookSpecificOutput"].(map[string]any)
	if !ok {
		theirs = map[string]any{"hookEventName": "PreToolUse"}
		m["hookSpecificOutput"] = theirs
	}
	theirs["additionalContext"] = join(theirs["additionalContext"], ours["additionalContext"].(string))
	b, _ := json.Marshal(m)
	return append(b, '\n')
}

// SettingsScope is the `spotexec hook install --claude` flag that installs
// into a Claude Code settings file: --global, --shared or none (local).
func SettingsScope(file string) string {
	if file == GlobalSettings() {
		return " --global"
	}
	if filepath.Base(file) == "settings.json" {
		return " --shared"
	}
	return ""
}

// GlobalSettings is the user's Claude Code settings file.
func GlobalSettings() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// RTKInfo is what the Claude Code settings that apply in a directory say
// about RTK, for `spotexec hook check`.
type RTKInfo struct{ st rtkState }

// RTKFor reads the Claude Code settings that apply in dir.
func RTKFor(dir string) RTKInfo { return RTKInfo{scanRTK(dir)} }

// InUse reports whether an RTK hook is installed, chained or not.
func (r RTKInfo) InUse() bool { return r.st.inUse }

// Races returns the settings file of an unchained RTK hook that would
// rewrite command too.
func (r RTKInfo) Races(command string) (string, bool) {
	h, ok := r.st.racer(command)
	return h.file, ok
}
