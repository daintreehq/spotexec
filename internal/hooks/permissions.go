package hooks

// Claude Code checks its permission rules against the command a hook hands
// back, so after a rewrite every rule sees `spotexec run -- '<cmd>'` and
// not the command the agent wrote: an allow rule for `pnpm test` no longer
// matches (the user is asked about a command they had approved), and an
// ask or deny rule for it no longer fires.
//
// So the hook reads the rules itself and checks the command as written, the
// way RTK's hook does:
//   - a deny rule matching any part of the line: the command is left alone,
//     for Claude Code to deny as it would have;
//   - an ask rule matching any part: offloaded, with the hook asking;
//   - an allow rule matching every heavy command, and every other part being
//     one Claude Code allows anyway (a cd inside the project, a filter that
//     only reads its input, an echo): offloaded and allowed;
//   - otherwise: offloaded, and Claude Code's own check decides on the
//     rewritten command.
//
// A deny or ask rule is matched loosely (past wrappers and assignments, and
// as a prefix) so it can only make the verdict stricter. An allow rule is
// matched as Claude Code matches it (checked against Claude Code 2.1.325):
// past timeout, time, nice, nohup and command, but not past an assignment or
// env, and never more widely. The hook allows nothing when it can't see every
// rule that might matter: a settings file it couldn't read, an MDM policy, or
// Read/Edit rules (which Claude Code applies to the files a Bash command
// reads). Rules from sources it can't see at all (--settings,
// --disallowedTools, --setting-sources, server-managed settings) apply only
// to the rewritten command.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/daintreehq/spotexec/internal/manifest"
)

// Verdict is the hook's reading of the user's permission rules.
type Verdict int

const (
	VerdictNone  Verdict = iota // no rule decides: Claude Code judges the rewrite
	VerdictAllow                // every part allowed
	VerdictAsk                  // an ask rule matches
	VerdictDeny                 // a deny rule matches
)

// Permission is the verdict and the rule that decided it.
type Permission struct {
	Verdict Verdict
	Rule    string // as written in the settings file, e.g. Bash(pnpm test:*)
	File    string
}

type bashRule struct {
	pattern string // the text inside Bash(...), "*" for every command
	raw     string
	file    string
}

type permRules struct {
	allow, ask, deny []bashRule
	// complete is false when a rule that matters may be out of sight: a
	// settings file that exists but couldn't be read, an MDM policy, or
	// Read/Edit rules. Nothing is auto-allowed then.
	complete bool
}

// managedSettings is Claude Code's file-based managed settings file (a var
// for tests); drop-ins in managed-settings.d beside it join it.
var managedSettings = func() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "windows":
		return `C:\Program Files\ClaudeCode\managed-settings.json`
	default:
		return "/etc/claude-code/managed-settings.json"
	}
}

// mdmPolicies are where an MDM-delivered Claude Code policy would be, which
// the hook doesn't read (a var for tests).
var mdmPolicies = func() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	p := []string{"/Library/Managed Preferences/com.anthropic.claudecode.plist"}
	if u := os.Getenv("USER"); u != "" {
		p = append(p, filepath.Join("/Library/Managed Preferences", u, "com.anthropic.claudecode.plist"))
	}
	return p
}

// permSettingsFiles are the settings files whose permission rules apply in
// dir: the managed ones, then the user's, the project's shared and local
// files (at its root: CLAUDE_PROJECT_DIR, set for hooks, or the git root),
// and, in a linked worktree, the main checkout's local file, which Claude
// Code uses for the worktree too.
func permSettingsFiles(proj string) (managed, others []string, ok bool) {
	m := managedSettings()
	managed = []string{m}
	ok = true
	ents, err := os.ReadDir(filepath.Join(filepath.Dir(m), "managed-settings.d"))
	if err != nil && !os.IsNotExist(err) {
		ok = false // drop-ins that can't be listed may deny
	}
	var ds []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			ds = append(ds, filepath.Join(filepath.Dir(m), "managed-settings.d", e.Name()))
		}
	}
	sort.Strings(ds)
	managed = append(managed, ds...)
	others = []string{GlobalSettings(), filepath.Join(proj, ".claude", "settings.json"), filepath.Join(proj, ".claude", "settings.local.json")}
	if main := mainCheckout(proj); main != "" && main != proj {
		others = append(others, filepath.Join(main, ".claude", "settings.local.json"))
	}
	return managed, others, ok
}

// projectOf is the Claude Code project a hook event in dir belongs to:
// CLAUDE_PROJECT_DIR, which Claude Code sets for hooks, or the git root.
func projectOf(dir string) string {
	if p := os.Getenv("CLAUDE_PROJECT_DIR"); p != "" {
		return p
	}
	return gitRoot(dir)
}

// mainCheckout is the main worktree of the repository at dir, or "".
func mainCheckout(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	common := manifest.GitPath(out)
	if filepath.Base(common) != ".git" {
		return "" // a bare repository
	}
	return filepath.Dir(common)
}

type settingsFile struct {
	Permissions struct {
		Allow []any `json:"allow"`
		Ask   []any `json:"ask"`
		Deny  []any `json:"deny"`
	} `json:"permissions"`
	ManagedOnly bool `json:"allowManagedPermissionRulesOnly"`
}

// loadPermRules reads the Bash permission rules of every Claude Code
// settings file that applies in the project proj.
func loadPermRules(proj string) permRules {
	r := permRules{complete: true}
	for _, p := range mdmPolicies() {
		if _, err := os.Stat(p); err == nil {
			r.complete = false
		}
	}
	managed, others, ok := permSettingsFiles(proj)
	if !ok {
		r.complete = false
	}
	managedOnly := false
	read := func(f string, isManaged bool) {
		b, err := os.ReadFile(f)
		if err != nil {
			if !os.IsNotExist(err) {
				r.complete = false
			}
			return
		}
		var s settingsFile
		if json.Unmarshal(b, &s) != nil {
			r.complete = false
			return
		}
		if isManaged && s.ManagedOnly {
			managedOnly = true
		}
		// A Read/Edit ask or deny rule restricts the files a Bash command
		// may read or write, which the hook doesn't model.
		if fileRules(s.Permissions.Ask) || fileRules(s.Permissions.Deny) {
			r.complete = false
		}
		if isManaged || !managedOnly {
			r.allow = append(r.allow, bashRules(s.Permissions.Allow, f)...)
		}
		// Ask and deny rules from every file still count: they can only
		// make the verdict stricter.
		r.ask = append(r.ask, bashRules(s.Permissions.Ask, f)...)
		r.deny = append(r.deny, bashRules(s.Permissions.Deny, f)...)
	}
	for _, f := range managed {
		read(f, true)
	}
	for _, f := range others {
		read(f, false)
	}
	return r
}

var (
	bashRuleRe = regexp.MustCompile(`^Bash(?:\((.*)\))?$`)
	fileRuleRe = regexp.MustCompile(`^(Read|Edit|Write)(\(|$)`)
)

func bashRules(list []any, file string) []bashRule {
	var out []bashRule
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			continue
		}
		m := bashRuleRe.FindStringSubmatch(strings.TrimSpace(s))
		if m == nil {
			continue
		}
		p := strings.TrimSpace(m[1])
		if p == "" {
			p = "*"
		}
		out = append(out, bashRule{pattern: p, raw: s, file: file})
	}
	return out
}

// fileRules reports Read/Edit/Write rules, which Claude Code also applies
// to the files a Bash command reads or writes (`cat secrets.txt`).
func fileRules(list []any) bool {
	for _, v := range list {
		if s, ok := v.(string); ok && fileRuleRe.MatchString(strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

// ruleMatches reports whether a Bash rule's pattern matches cmd, as Claude
// Code documents it: `*` matches anything (so `ls *` needs the space and
// `ls*` doesn't), `:*` at the end is the same as ` *`, and a pattern without
// a wildcard is exact. loose (for deny and ask) also reads an exact pattern
// as a prefix and ignores runs of blanks, so it can only match more.
func ruleMatches(pattern, cmd string, loose bool) bool {
	pattern = strings.TrimSpace(pattern)
	cmd = strings.TrimSpace(cmd)
	if loose {
		cmd = strings.Join(strings.Fields(cmd), " ")
		pattern = strings.Join(strings.Fields(pattern), " ")
	}
	if p, ok := strings.CutSuffix(pattern, ":*"); ok {
		pattern = p + " *"
	}
	if pattern == "*" || pattern == " *" {
		return true
	}
	if strings.Contains(pattern, "*") {
		var re strings.Builder
		re.WriteString("^")
		for i, part := range strings.Split(pattern, "*") {
			if i > 0 {
				re.WriteString(".*")
			}
			re.WriteString(regexp.QuoteMeta(part))
		}
		re.WriteString("$")
		if regexp.MustCompile("(?s)" + re.String()).MatchString(cmd) {
			return true
		}
		// `Bash(ls *)` covers a bare `ls` too.
		if p, ok := strings.CutSuffix(pattern, " *"); ok && !strings.Contains(p, "*") && cmd == p {
			return true
		}
		return false
	}
	return cmd == pattern || loose && strings.HasPrefix(cmd, pattern+" ")
}

// allowReadings are the ways Claude Code reads a command for an allow rule:
// as written, and past the process wrappers and builtins it strips
// (timeout, time without options, nice, nohup, command without options).
// Never past an assignment or env.
func allowReadings(cmd string) []string {
	out := []string{cmd}
	at := peelCtx{first: true, builtin: true}
	for i := 0; i < 16; i++ {
		if envPrefix(cmd) != "" {
			break
		}
		w := rawWords(cmd)
		if len(w) < 2 {
			break
		}
		switch w[0].text {
		case "timeout", "nice", "nohup", "command":
		case "time":
			if strings.HasPrefix(w[1].text, "-") {
				return out
			}
		default:
			return out
		}
		rest, external, ok := peelOne(cmd, at, nil)
		if !ok || strings.TrimSpace(rest) == "" {
			break
		}
		at.first, at.builtin = false, at.builtin && !external
		cmd = rest
		out = append(out, cmd)
	}
	return out
}

// autoAllowed reports a trailer Claude Code allows without a rule: true,
// echo or printf with nothing to expand or assign (it asks about `echo
// "exit=$?"` and `:`).
func autoAllowed(seg string) bool {
	return trailer(seg, true) && !strings.Contains(seg, "$") && !strings.HasPrefix(seg, ":")
}

// CheckPermissions reads command (as the agent wrote it, already through
// Normalize) against the Claude Code permission rules that apply in dir.
func CheckPermissions(command, dir string, prefixes []string) Permission {
	return checkPermissions(command, prefixes, loadPermRules(projectOf(dir)))
}

func checkPermissions(command string, prefixes []string, r permRules) Permission {
	// Every simple command of the line: each pipeline stage of each list
	// segment. filter says a stage reads a pipe.
	type part struct {
		text   string
		filter bool
	}
	var parts []part
	segs, _ := splitList(command)
	for _, s := range segs {
		stages, _, ok := splitPipes(s)
		if !ok {
			stages = []string{s}
		}
		for j, st := range stages {
			parts = append(parts, part{strings.TrimSpace(st), j > 0})
		}
	}
	// The ways a deny or ask rule may name each part: as written, without
	// trailing redirects, and past each wrapper or assignment.
	all := make([][]string, len(parts))
	for i, pt := range parts {
		p := pt.text
		bare := strings.TrimSpace(trailingRedirect.ReplaceAllString(p, ""))
		all[i] = readings(p, prefixes)
		if bare != p {
			all[i] = append(all[i], readings(bare, prefixes)...)
		}
	}
	for _, rule := range r.deny {
		for _, rs := range all {
			for _, c := range rs {
				if ruleMatches(rule.pattern, c, true) {
					return Permission{VerdictDeny, rule.raw, rule.file}
				}
			}
		}
	}
	for _, rule := range r.ask {
		for _, rs := range all {
			for _, c := range rs {
				if ruleMatches(rule.pattern, c, true) {
					return Permission{VerdictAsk, rule.raw, rule.file}
				}
			}
		}
	}
	if !r.complete || len(r.allow) == 0 {
		return Permission{}
	}
	var first bashRule
	matched := false
	for _, pt := range parts {
		p := pt.text
		bare := strings.TrimSpace(trailingRedirect.ReplaceAllString(p, ""))
		if isCd(p) || autoAllowed(bare) || pt.filter && portableFilter(p) {
			continue
		}
		found := false
	rules:
		for _, rule := range r.allow {
			for _, c := range append(allowReadings(p), allowReadings(bare)...) {
				if ruleMatches(rule.pattern, c, false) {
					if !matched {
						first, matched = rule, true
					}
					found = true
					break rules
				}
			}
		}
		if !found {
			return Permission{}
		}
	}
	if !matched {
		return Permission{}
	}
	return Permission{VerdictAllow, first.raw, first.file}
}

// shortFile is a settings file for a message: relative to home when under it.
func shortFile(f string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, f); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return f
}
