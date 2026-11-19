// Package hooks implements deterministic agent command interception: a
// Claude Code PreToolUse hook that wraps heavy commands (tests, type checks,
// formatters) with `spotexec run --`, and an AGENTS.md snippet for other
// harnesses.
package hooks

import (
	"bytes"
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
	`^(mvn|(\./)?mvnw)( -[\w.=-]+)* (clean )?(test|verify|compile|test-compile|package|checkstyle:check|spotless:check)\b`,
	`^(gradle|(\./)?gradlew)( -[\w.=-]+)* (clean )?((:[\w.-]+)*:)?(test|check|build|compileJava|compileKotlin|lint|spotlessCheck)\b`,
}

// DefaultExclude keeps watch flags local, and honours the agent's explicit
// opt-out for one command. Like user excludes, they match the whole line.
//
// Watch modes, dev servers and spotexec itself are recognised by the
// command's own words instead (see localCommand), not by its arguments:
// `pytest -k watch`, `cargo test --features dev` and `go test ./cmd/spotexec`
// are ordinary test runs.
var DefaultExclude = []string{
	`--watch\b`, `--ui\b`,
	`(^|\s)SPOTEXEC_LOCAL=1(\s|$)`,
}

var (
	trailingMerge = regexp.MustCompile(`\s+2>&1$`)
	// trailingRedirect is a segment's trailing redirects that send nothing
	// to a file: 2>&1, >&2, and stdout or stderr into /dev/null. They mean
	// the same on the runner (other descriptors aren't passed there).
	trailingRedirect = regexp.MustCompile(`(\s+([12]?>&[12]|[12&]?>>?\s*/dev/null))+$`)
	cdSeg            = regexp.MustCompile(`^cd\s+\S+$`)
	assignName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	commandWord      = regexp.MustCompile(`^[A-Za-z0-9_][\w.:@+-]*$`)
)

// Decider decides which commands to offload.
type Decider struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
	// prefixes are the user's transparent_prefixes, words collapsed.
	prefixes []string
	// root is the repository and dir the directory the command runs in
	// (symlinks resolved), for paths and directory options; "" accepts only
	// relative paths without "..".
	root, dir string
}

// NewDecider compiles defaults plus user patterns.
func NewDecider(h config.Hook) (*Decider, error) {
	d := &Decider{}
	for _, p := range h.TransparentPrefixes {
		w := strings.Join(strings.Fields(p), " ")
		if w == "" || shellSyntax(w) || strings.Contains(w, "#") {
			return nil, fmt.Errorf("hook transparent_prefixes entry %q: must be plain words, such as \"direnv exec .\"", p)
		}
		d.prefixes = append(d.prefixes, w)
	}
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

// In sets the directory the command runs in, so paths and directory options
// are checked against its repository.
func (d *Decider) In(dir string) *Decider {
	d.root = gitRootResolved(dir)
	d.dir = ""
	if d.root != "" {
		d.dir = resolve(dir)
	}
	return d
}

// Prefixes are the user's transparent prefixes.
func (d *Decider) Prefixes() []string { return d.prefixes }

// ShouldOffload reports whether a Bash command line (through Normalize)
// should run remotely. Every `&&`/`||`/`;`-separated segment must be a heavy
// command (possibly behind wrappers such as `timeout 600` or `env CI=1`), a
// leading cd, or a harmless trailer (`true`, `echo "exit $?"`), with at
// least one heavy command. The line may end in a pipeline of simple output
// filters (`| tail -50`, `|& grep FAIL`), which keep running locally on the
// streamed output. Redirects into files, subshells, background jobs and
// anything else keep the command local.
//
// A pipe binds tighter than && and ;, so in `a && b | tail` only b is
// filtered. Splitting that across two runs could land them on different
// machines (b would miss a's build output), so a filtered line offloads only
// when a single heavy command (after any leading cds) feeds the filters.
func (d *Decider) ShouldOffload(command string) bool {
	ok, _ := d.Why(command)
	return ok
}

// Why is ShouldOffload with the reason a line stays local.
func (d *Decider) Why(command string) (bool, string) {
	if shellHazard(command) {
		return false, "a line break, carriage return, comment or backslash escape it can't read"
	}
	head, filters, ok := splitFilters(command)
	if !ok {
		// Pipes inside the list (`go vet | grep -c x; go test`): the whole
		// line runs on the runner, if every filter only reads its input.
		if head, ok = innerPipes(command); !ok {
			return false, "a pipe into something other than an output filter"
		}
	}
	segs, ops := splitList(head)
	k := 0 // leading cds, which Wrap runs locally
	for k < len(segs)-1 && isCd(segs[k]) {
		k++
	}
	for _, seg := range segs {
		if seg == "" {
			return false, "an empty command, which bash refuses" // `a;; b`, `a && && b`
		}
	}
	// Paths and directory options resolve from where the leading cds end,
	// and a cd before `;` may fail and leave the directory as it was: the
	// command must be fine from each place it may run in.
	dirs := []string{d.dir}
	for i := 0; i < k && d.dir != ""; i++ {
		t, ok := cdTarget(bareSeg(segs[i]), dirs[len(dirs)-1])
		if !ok {
			dirs = []string{""}
			break
		}
		if ops[i] == ";" {
			dirs = append(dirs, t)
		} else {
			dirs = append(dirs[:len(dirs)-1], t)
		}
	}
	for _, dir := range dirs {
		if ok, why := d.why(head, place{root: d.root, dir: dir}); !ok {
			return false, why
		}
	}
	if slices.Contains(ops[:k], "||") {
		return false, "a cd decides what runs (cd x || ...)"
	}
	if k > 0 && ops[k-1] == "&&" && (slices.Contains(ops[k:], ";") || slices.Contains(ops[k:], "||")) {
		// `cd x && a; b` runs b even when the cd fails, and so does
		// `cd x && a || b`; hoisted, the cd would gate the whole rest.
		return false, "a cd && list followed by ; or ||"
	}
	if filters != "" && len(segs)-k > 1 && !portableFilters(filters) {
		// The filters would have to run on the runner with the commands
		// (see WrapRTK): only ones that read nothing but their input and
		// mean the same there.
		return false, "a pipe after several commands into a filter that can't run on the runner"
	}
	if filters != "" && len(segs)-k == 1 && timed(segs[k]) {
		// `time` times the whole pipeline; offloaded, it would time only
		// the remote part.
		return false, "`time` before a pipeline"
	}
	return true, ""
}

// timed reports a segment that starts with the time keyword.
func timed(seg string) bool {
	w := rawWords(seg)
	return len(w) > 0 && w[0].text == "time" && strings.HasPrefix(strings.TrimSpace(seg), "time")
}

// innerPipes is a list with pipelines inside it (`a | grep x; b`), each cut
// to its first command for judging. ok is false unless every later stage is
// a read-only filter, which can run on the runner with the command before
// it, and no cd is piped (it would change nothing).
func innerPipes(command string) (string, bool) {
	segs, ops := splitList(command)
	for i, seg := range segs {
		parts, amps, ok := splitPipes(seg)
		if !ok {
			return "", false
		}
		if len(parts) == 1 {
			continue
		}
		for _, f := range parts[1:] {
			if !portableFilter(f) {
				return "", false
			}
		}
		head := strings.TrimSpace(parts[0])
		if isCd(head) {
			return "", false
		}
		if amps[0] && !trailingMerge.MatchString(head) {
			head += " 2>&1"
		}
		segs[i] = head
	}
	return joinList(segs, ops), true
}

// portableFilters reports filters that may run on the runner (see
// portableFilter).
func portableFilters(filters string) bool {
	parts, _, ok := splitPipes(filters)
	if !ok {
		return false
	}
	for _, f := range parts {
		if !portableFilter(f) {
			return false
		}
	}
	return true
}

// portableFilter reports a filter stage that reads nothing but its input and
// means the same with the runner's GNU tools as with this machine's: cat,
// head, tail, wc, grep, cut or tr, with no file operand (which would be the
// runner's file), no expansion or redirect, and only options both share
// (not BSD tail -r or GNU grep -P).
func portableFilter(stage string) bool {
	t := strings.TrimSpace(stage)
	if t == "" || shellSyntax(t) || hasUnquoted(t, "*?[~") {
		return false // also globs and ~, which the runner would expand
	}
	ws := rawWords(t)
	args := make([]string, 0, len(ws))
	for _, w := range ws[1:] {
		args = append(args, w.text)
	}
	num := func(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }
	switch ws[0].text {
	case "cat":
		return len(args) == 0
	case "head", "tail":
		for i := 0; i < len(args); i++ {
			switch a := args[i]; {
			case a == "-n" || a == "-c":
				i++
				if i >= len(args) || !num(strings.TrimPrefix(args[i], "+")) {
					return false
				}
			case (strings.HasPrefix(a, "-n") || strings.HasPrefix(a, "-c")) && num(strings.TrimPrefix(a[2:], "+")):
			case strings.HasPrefix(a, "-") && num(a[1:]):
			default:
				return false
			}
		}
		return true
	case "wc":
		for _, a := range args {
			if a != "-l" && a != "-w" && a != "-c" && a != "-m" {
				return false
			}
		}
		return true
	case "grep", "egrep", "fgrep":
		pattern := false
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "-e":
				i++
				if i >= len(args) {
					return false
				}
				pattern = true
			case a == "-m" || a == "-A" || a == "-B" || a == "-C":
				i++
				if i >= len(args) || !num(args[i]) {
					return false
				}
			case grepFlags.MatchString(a) || grepCount.MatchString(a):
			case strings.HasPrefix(a, "-") && a != "-":
				return false
			case pattern:
				return false // a file operand
			default:
				pattern = true
			}
		}
		return pattern
	case "cut":
		for i := 0; i < len(args); i++ {
			switch a := args[i]; {
			case a == "-c" || a == "-f" || a == "-d" || a == "-b":
				i++
				if i >= len(args) {
					return false
				}
			case a == "-s" || len(a) > 2 && (a[:2] == "-c" || a[:2] == "-f" || a[:2] == "-d" || a[:2] == "-b"):
			default:
				return false
			}
		}
		return len(args) > 0
	case "tr":
		sets := 0
		for _, a := range args {
			switch {
			case sets == 0 && (a == "-d" || a == "-s" || a == "-c" || a == "-ds" || a == "-cd" || a == "-cs"):
			case strings.HasPrefix(a, "-") && len(a) > 1:
				return false
			default:
				sets++
			}
		}
		return sets == 1 || sets == 2
	}
	return false
}

var (
	grepFlags = regexp.MustCompile(`^-[cviEFnowxqsHh]+$`)
	grepCount = regexp.MustCompile(`^-[mABC][0-9]+$`)
)

// shellHazard reports shell syntax the splitters don't model: a newline
// (which separates commands, and the splitters trim), a comment (which could
// hide an operator) or a backslash escape outside quotes.
func shellHazard(c string) bool {
	if strings.ContainsAny(c, "\n\r") {
		return true // a CR is part of a word to bash, and trimming would drop it
	}
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
// ok is false when a part after the first isn't a plain output filter. A
// `|&` after the heavy part (its stderr into the pipe too) becomes a 2>&1
// on it, so spotexec's own messages stay outside the pipe.
func splitFilters(command string) (head, filters string, ok bool) {
	parts, amps, ok := splitPipes(strings.TrimSpace(command))
	if !ok {
		return "", "", false
	}
	if len(parts) == 1 {
		return parts[0], "", true
	}
	var b strings.Builder
	for i, f := range parts[1:] {
		if t := strings.TrimSpace(f); !outputFilter.MatchString(t) || hasUnquoted(t, "<>;&`$(){}\n") {
			return "", "", false
		}
		if i > 0 {
			b.WriteString(map[bool]string{false: "|", true: "|&"}[amps[i]])
		}
		b.WriteString(f)
	}
	head = strings.TrimSpace(parts[0])
	if amps[0] && !trailingMerge.MatchString(head) {
		head += " 2>&1"
	}
	return head, strings.TrimSpace(b.String()), true
}

// splitPipes splits at unquoted "|" and "|&" (not "||"); amps[i] says the
// pipe after parts[i] was "|&". ok is false for unbalanced quotes or an
// empty part.
func splitPipes(c string) (parts []string, amps []bool, ok bool) {
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
			amp := i+1 < len(c) && c[i+1] == '&'
			if amp {
				i++
			}
			amps = append(amps, amp)
			start = i + 1
		}
	}
	if q != 0 {
		return nil, nil, false
	}
	parts = append(parts, c[start:])
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return nil, nil, false
		}
	}
	return parts, amps, true
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

func (d *Decider) why(command string, at place) (bool, string) {
	c := strings.TrimSpace(command)
	// A trailing 2>&1 (agents add it to nearly everything) only merges the
	// streams: harmless, and it stays part of the offloaded command. User
	// excludes see the line without it, as they always have.
	line := strings.TrimSpace(trailingMerge.ReplaceAllString(c, ""))
	if line == "" {
		return false, "an empty command"
	}
	for _, re := range d.exclude {
		if re.MatchString(line) {
			return false, "excluded by " + re.String()
		}
	}
	segs, _ := splitList(c)
	heavy, leading := false, true
	for _, seg := range segs {
		// A cd is read exactly as Wrap hoists it (isCd): `cd x >/dev/null`
		// is not one, and stays on the runner like any other command.
		if isCd(seg) && !shellSyntax(bareSeg(seg)) {
			if !leading {
				// Only leading cds run locally (see Wrap): offloaded,
				// this one would happen only remotely, and the agent's
				// shell would stay where it was.
				return false, "a cd after another command"
			}
			continue
		}
		leading = false
		seg = strings.TrimSpace(trailingRedirect.ReplaceAllString(seg, ""))
		if trailer(seg, heavy) || rideAlong.MatchString(seg) && !shellSyntax(seg) {
			continue
		}
		if shellSyntax(seg) {
			return false, "shell syntax in " + quoteSeg(seg) + " (a redirect, expansion, subshell or background job)"
		}
		_, cmd, ok := splitPrefix(seg, d.prefixes)
		if !ok {
			return false, "a wrapper with nothing to run"
		}
		cmd = strings.TrimSpace(cmd)
		form, escapes := toolFormOK(cmd, at)
		if escapes {
			return false, quoteSeg(seg) + " works in a directory outside the repository"
		}
		form = uvForm(form, at)
		if localCommand(form) {
			return false, quoteSeg(seg) + " stays local (spotexec itself, a dev server or a watch mode)"
		}
		matched := false
		for _, re := range d.include {
			if re.MatchString(cmd) || re.MatchString(form) {
				matched = true
				break
			}
		}
		if !matched {
			return false, "mixed with " + quoteSeg(seg) + ", which isn't a heavy command"
		}
		heavy = true
	}
	if !heavy {
		return false, "no heavy command"
	}
	return true, ""
}

// quoteSeg is a segment for a message, shortened.
func quoteSeg(seg string) string {
	seg = strings.ReplaceAll(seg, "`", "'")
	if len(seg) > 60 {
		seg = seg[:57] + "..."
	}
	return "`" + seg + "`"
}

// trailerCmd is a command that only prints or sets a status, harmless to run
// on the runner after (or before) a heavy one: `pnpm test || true`,
// `cargo test; echo "exit=$?"`.
var trailerCmd = regexp.MustCompile(`^(true|:|echo|printf)(\s|$)`)

// printfAssign is a printf conversion that assigns: %n, with any flags,
// width or length (%10n, %ln).
var printfAssign = regexp.MustCompile(`%[-+ #0-9.*]*[hlLqjzt]*n`)

// rideAlong are quick commands that aren't worth offloading alone but run
// the same on the runner, in a list with heavy ones (`gofmt -l . && go
// test ./...`).
var rideAlong = regexp.MustCompile(`^(gofmt|goimports|go fmt|go mod verify)(\s|$)`)

// trailer reports a harmless trailer segment. Its $? is fine after a heavy
// command (on the runner it is that command's status, as here), not before
// one, where it would be the runner's own. printf -v and %n assign shell
// variables, so those aren't trailers.
func trailer(seg string, afterHeavy bool) bool {
	if !trailerCmd.MatchString(seg) || !afterHeavy && strings.Contains(seg, "$?") {
		return false
	}
	if strings.HasPrefix(seg, "printf") {
		for _, w := range rawWords(seg)[1:] {
			if strings.HasPrefix(w.text, "-v") || printfAssign.MatchString(w.text) {
				return false
			}
		}
	}
	return !shellSyntax(strings.ReplaceAll(seg, "$?", ""))
}

// shellSyntax reports syntax in one list segment that the decider doesn't
// model: a pipe, redirect, background job, subshell, brace group or
// expansion outside quotes, or an expansion inside double quotes (which
// would run locally, or with remote values). Single-quoted text is data, so
// `go test -run 'TestA|TestB'` and `pytest -k 'a or (b and c)'` are fine.
func shellSyntax(seg string) bool {
	var q byte
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		switch {
		case ch == '\n':
			return true
		case q == '\'':
			if ch == '\'' {
				q = 0
			}
		case q == '"':
			switch ch {
			case '\\':
				i++
			case '"':
				q = 0
			case '$', '`':
				return true
			}
		case ch == '\'' || ch == '"':
			q = ch
		case strings.IndexByte("\\|<>`$(){}&;", ch) >= 0:
			return true
		}
	}
	return q != 0
}

// envPrefix is the leading `NAME=value` assignments of a list segment, with
// the whitespace after them. A value may be quoted and hold spaces or
// operators: `X='a; pytest' git commit` assigns X and runs git.
func envPrefix(seg string) string {
	i := 0
	for {
		m := assignName.FindStringIndex(seg[i:])
		if m == nil {
			return seg[:i]
		}
		_, rest, _, ok := shellWord(seg[i+m[1]:])
		if !ok || rest == "" || rest[0] != ' ' && rest[0] != '\t' {
			// No command follows, or the value ends at an operator.
			return seg[:i]
		}
		i = len(seg) - len(strings.TrimLeft(rest, " \t"))
	}
}

// bareSeg is a list segment without its leading assignments and a trailing
// 2>&1, for recognising a `cd`.
func bareSeg(seg string) string {
	seg = strings.TrimSpace(trailingMerge.ReplaceAllString(strings.TrimSpace(seg), ""))
	return strings.TrimSpace(seg[len(envPrefix(seg)):])
}

// localCommand reports a command that stays local whatever the patterns
// say: spotexec itself, or a watch mode or dev server named by the command's
// own words (`vitest watch`, `pnpm dev`, `npm run test:watch`), never by an
// argument (`pytest -k watch`, `go test ./cmd/spotexec`, `cargo test -p sx`).
func localCommand(seg string) bool {
	toks := shellWords(seg)
	if len(toks) == 0 || toks[0] == "" {
		return true
	}
	toks[0] = filepath.Base(toks[0])
	// Past a package runner, the script or tool it runs.
	switch t := toks; {
	case t[0] == "npx" || t[0] == "bunx":
		toks = t[1:]
	case len(t) > 1 && (t[0] == "bun" && t[1] == "x" || t[0] == "npm" && t[1] == "exec" ||
		t[0] == "pnpm" && (t[1] == "exec" || t[1] == "dlx")):
		toks = t[2:]
	case t[0] == "npm" || t[0] == "pnpm" || t[0] == "yarn" || t[0] == "bun":
		toks = t[1:]
		if len(toks) > 0 && toks[0] == "run" {
			toks = toks[1:]
		}
	}
	if len(toks) == 0 {
		return false
	}
	tool := filepath.Base(toks[0])
	if tool == "spotexec" || tool == "sx" {
		return true
	}
	// The script or tool's name and its subcommand, the plain word after it
	// (`test:watch`, `ng serve`, `dev:server`): anything after an option or
	// path is an argument. vitest's subcommand may follow its options and
	// their values (`vitest --reporter dot watch`), so for vitest it is the
	// first of its own subcommands.
	words := []string{tool}
	for _, w := range toks[1:] {
		if tool == "vitest" {
			if vitestCommands[w] {
				words = append(words, w)
				break
			}
			continue
		}
		if commandWord.MatchString(w) {
			words = append(words, w)
		}
		break
	}
	for _, w := range words {
		parts := strings.Split(w, ":")
		if parts[0] == "dev" || parts[0] == "start" {
			return true
		}
		for _, p := range parts {
			if p == "watch" || p == "serve" {
				return true
			}
		}
	}
	return false
}

// vitestCommands are vitest's subcommands.
var vitestCommands = map[string]bool{"run": true, "watch": true, "dev": true, "related": true, "bench": true, "list": true, "init": true, "typecheck": true}

// shellWords splits a command without operators into its words, unquoted.
func shellWords(s string) []string {
	var out []string
	for s = strings.TrimLeft(s, " \t"); s != ""; s = strings.TrimLeft(s, " \t") {
		w, rest, _, ok := shellWord(s)
		if !ok || len(rest) == len(s) {
			break
		}
		out = append(out, w)
		s = rest
	}
	return out
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
func Wrap(bin, command string) string { return WrapRTK(bin, command, false) }

// WrapRTK is Wrap for a machine where RTK compacts agents' output (rtk true):
// a single command whose raw output RTK's pipe filter reads (see rtkFilters)
// runs with --rtk-filter, so it comes back compacted as `rtk <cmd>` would.
// A line the agent already filters itself (`| tail -50`) is left as it is.
func WrapRTK(bin, command string, rtk bool) string {
	head, filters, ok := splitFilters(command)
	if !ok {
		head, filters = strings.TrimSpace(command), ""
	}
	segs, ops := splitList(head)
	var b strings.Builder
	i := 0
	for ; i < len(segs)-1 && isCd(segs[i]); i++ {
		b.WriteString(segs[i] + listOp(ops[i]))
	}
	segs, ops = segs[i:], ops[i:]
	flags := ""
	if rtk && filters == "" && len(segs) == 1 {
		if name, run, ok := rtkFilterFor(segs[0]); ok {
			flags, segs[0] = "--rtk-filter "+name+" ", run
		}
	}
	run := joinList(segs, ops)
	if filters != "" && len(segs) > 1 {
		// A pipe binds tighter than && and ;, so the filters read the last
		// command only: they go with the commands, in one run, rather than
		// split across two machines (ShouldOffload lets only read-only
		// filters through here).
		run, filters = run+" | "+filters, ""
	}
	b.WriteString(bin + " run " + flags + "-- " + shellQuote(run))
	if filters != "" {
		b.WriteString(" | " + filters)
	}
	return b.String()
}

func isCd(seg string) bool {
	return cdSeg.MatchString(bareSeg(seg))
}

// splitList splits a command list at unquoted "&&", "||" and ";" (the only
// list operators ShouldOffload lets through), returning the trimmed segments
// and the operators between them.
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
		case ch == ';' || (ch == '&' || ch == '|') && i+1 < len(c) && rune(c[i+1]) == ch:
			segs = append(segs, strings.TrimSpace(c[start:i]))
			op := ";"
			if ch != ';' {
				op = string([]rune{ch, ch})
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
	// PermissionMode is Claude Code's mode: default, acceptEdits, plan,
	// bypassPermissions, dontAsk.
	PermissionMode string `json:"permission_mode"`
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
	RunClaude(in, out, io.Discard, ClaudeOptions{Bin: bin})
}

// ClaudeOptions configure the PreToolUse hook.
type ClaudeOptions struct {
	Bin string // how the rewritten command calls spotexec
	// Then is another PreToolUse hook command (RTK's) that gets every event
	// spotexec leaves alone, so the two never rewrite the same command.
	Then string
}

// RunClaude handles one PreToolUse event and returns the hook's exit code:
// spotexec's own answer always exits 0; a chained hook's exit code is its own.
func RunClaude(in io.Reader, out, errOut io.Writer, o ClaudeOptions) int {
	raw, err := io.ReadAll(in)
	if err != nil {
		return 0
	}
	resp, took := decide(raw, o)
	if took || o.Then == "" {
		if resp != nil {
			writeJSON(out, resp)
		}
		return 0
	}
	return chain(raw, resp, out, errOut, o.Then)
}

// chain hands the event to the next hook, adding spotexec's one-time notice
// (if any) to its answer.
func chain(raw []byte, notice map[string]any, out, errOut io.Writer, then string) int {
	c := exec.Command("sh", "-c", then)
	c.Stdin = bytes.NewReader(raw)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, errOut
	code := 0
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			// It never ran: leave the command as it is.
			fmt.Fprintf(errOut, "spotexec: couldn't run the chained hook %q: %v\n", then, err)
		} else if code = ee.ExitCode(); code < 0 {
			code = 1
		}
	}
	b := buf.Bytes()
	if notice != nil && code == 0 {
		b = mergeNotice(b, notice)
	}
	out.Write(b)
	return code
}

// decide is spotexec's answer to one event: nil to leave the command alone.
// took is true when spotexec rewrote or denied the command, so no chained
// hook may touch it.
func decide(raw []byte, o ClaudeOptions) (resp map[string]any, took bool) {
	var ev claudeInput
	if err := json.Unmarshal(raw, &ev); err != nil || ev.ToolName != "Bash" {
		return nil, false
	}
	cmd, _ := ev.ToolInput["command"].(string)
	if cmd == "" {
		return nil, false
	}
	if bg, _ := ev.ToolInput["run_in_background"].(bool); bg {
		return nil, false // long-lived background jobs stay local
	}
	dir := ev.Cwd
	if dir == "" {
		dir, _ = os.Getwd()
	}
	proj, ok := project(dir)
	if !ok || proj.Hook.Disabled {
		return nil, false
	}
	h := proj.Hook
	d, err := NewDecider(h)
	if err != nil {
		return nil, false
	}
	d.In(dir)
	// Comments, line continuations and line breaks, as bash reads them.
	norm := Normalize(cmd)
	// `rtk cargo test` is `cargo test` with its output compacted.
	line, explicit, raw2 := UnwrapRTK(norm)
	if !d.ShouldOffload(line) {
		return nil, false
	}
	// `cd` must stay inside this repository: sx syncs only this worktree.
	if !cdsInside(line, dir) {
		return nil, false
	}
	// The user's rules about the command as the agent wrote it.
	perm := CheckPermissions(norm, dir, d.prefixes)
	if perm.Verdict == VerdictDeny {
		// For Claude Code to deny, as it would have. Not handed on to a
		// chained hook either: its rewrite (`rtk cargo test`) could slip
		// past the rule.
		return nil, true
	}
	if h.WhenLoadAbove > 0 {
		if l, err := LoadPerCPU(); err != nil || l < h.WhenLoadAbove {
			return nil, false
		}
	}
	if err := RunnersFor(line, dir, proj); err != nil {
		// Offloading would only fail: run it here, and say so once a
		// session (a hook installed before `spotexec setup` would otherwise
		// keep a first demo silently local).
		if firstNotice("", ev.SessionID) {
			return map[string]any{
				"systemMessage": "spotexec: heavy commands are running on this machine: " + err.Error(),
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "PreToolUse",
					"additionalContext": "spotexec kept this command on this machine instead of offloading it, because it has no usable runners (" + err.Error() + "). Heavy commands keep running locally until that is fixed.",
				},
			}, false
		}
		return nil, false
	}
	rtk := scanRTK(dir)
	filtered := !raw2 && (explicit || rtk.inUse || isRTKHook(o.Then))
	wrapped := WrapRTK(o.Bin, line, filtered)
	if perm.Verdict == VerdictAsk && (h.Mode == "block" || rtk.unchainedRewrites(cmd)) {
		// Both paths deny with instructions to re-run through spotexec,
		// and an ask rule doesn't match that retry: leave the command as
		// it is, for Claude Code to ask about (and not to a chained hook,
		// whose rewrite might not match the rule either).
		return nil, true
	}
	if racer, ok := rtk.racer(cmd); ok && h.Mode != "block" {
		// RTK's hook will rewrite this command too, and either rewrite
		// could win: have the agent run the offloaded form, which RTK leaves
		// alone, and tell the user once how to chain the hooks instead.
		resp := map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": fmt.Sprintf("This project offloads heavy commands to remote runners, but RTK's hook would also rewrite this command, so it can't be rewritten automatically. Re-run it as: %s with a Bash timeout of %d ms, since a cold start can take minutes (or prefix SPOTEXEC_LOCAL=1 if it must run on this machine)", wrapped, bashTimeout()),
		}}
		if firstNotice("rtk", ev.SessionID) {
			resp["systemMessage"] = fmt.Sprintf("spotexec: RTK's hook in %s rewrites the same commands as spotexec's, so offloaded commands are denied with instructions to re-run. Run `%s` to chain RTK's hook behind spotexec's.", racer.file, "spotexec hook install --claude"+SettingsScope(racer.file))
		}
		return resp, true
	}
	if h.Mode == "block" {
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": fmt.Sprintf("This project offloads heavy commands to remote runners. Re-run it as: %s with a Bash timeout of %d ms, since a cold start can take minutes (or prefix SPOTEXEC_LOCAL=1 if it must run on this machine, e.g. it needs local services)", wrapped, bashTimeout()),
		}}, true
	}
	newInput := map[string]any{}
	for k, v := range ev.ToolInput {
		newInput[k] = v
	}
	newInput["command"] = wrapped
	if t, _ := newInput["timeout"].(float64); t <= 0 {
		newInput["timeout"] = bashTimeout()
	}
	out := map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  newInput,
	}
	switch {
	case perm.Verdict == VerdictAsk:
		out["permissionDecision"] = "ask"
		out["permissionDecisionReason"] = fmt.Sprintf("spotexec offloads this to a remote runner; your rule %s (%s) asks before it runs", perm.Rule, shortFile(perm.File))
	case perm.Verdict == VerdictAllow && ev.PermissionMode != "plan":
		out["permissionDecision"] = "allow"
		out["permissionDecisionReason"] = fmt.Sprintf("spotexec offloads this to a remote runner; allowed by your rule %s (%s)", perm.Rule, shortFile(perm.File))
	}
	return map[string]any{"hookSpecificOutput": out}, true
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

// firstNotice reports whether this session hasn't had the notice of this
// kind yet ("" is the "staying local" one), and records that it has. Without
// a session id it is once a day.
func firstNotice(kind, session string) bool {
	if session == "" {
		session = time.Now().Format("2006-01-02")
	}
	sum := sha256.Sum256([]byte(kind + session))
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
	segs, _ := splitList(command)
	for _, seg := range segs {
		seg = bareSeg(seg)
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
		head = command // pipes inside the list: none on a leading cd
	}
	segs, _ := splitList(head)
	cur := dir
	for i := 0; i < len(segs)-1 && isCd(segs[i]); i++ {
		t, ok := cdTarget(bareSeg(segs[i]), cur)
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

// hookForm classifies a PreToolUse hook command by its executable: only
// the single command the installer wrote counts (`spotexec hook claude`,
// chained or not, or v0.2.0's `sx hook claude`). A compound line such as
// `audit && sx hook claude` is the user's own and not ours to rewrite.
func hookForm(cmd string) int {
	exe, args, ok := hookExe(cmd)
	if !ok {
		return hookNone
	}
	switch filepath.Base(exe) {
	case "spotexec":
		if args == "hook claude" {
			return hookCurrent
		}
		if _, ok := thenOf(cmd); ok {
			return hookCurrent
		}
	case "sx":
		if args == "hook claude" {
			return hookLegacy
		}
	}
	return hookNone
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
	for _, e := range preEntries(settings) {
		if !coversBash(e.matcher) {
			continue // it may never see a Bash command
		}
		for _, hm := range e.hooks {
			if c, _ := hm["command"].(string); hookForm(c) == form {
				return true
			}
		}
	}
	return false
}

// refreshHook is an installed spotexec hook command calling spotexec the
// way hookCmd does: after a switch of install method (a release binary for
// Homebrew, say), the old path may be gone. Only the executable changes: a
// chain's --then argument stays exactly as written.
func refreshHook(cmd, hookCmd string) string {
	if hookForm(cmd) != hookCurrent {
		return cmd
	}
	have, _, ok := hookExe(cmd)
	want, _, ok2 := hookExe(hookCmd)
	if !ok || !ok2 || have == want {
		return cmd
	}
	cmd, hookCmd = strings.TrimSpace(cmd), strings.TrimSpace(hookCmd)
	_, args, _, _ := shellWord(cmd)
	_, wantArgs, _, _ := shellWord(hookCmd)
	return hookCmd[:len(hookCmd)-len(wantArgs)] + args
}

// HasClaude reports whether a Claude Code settings file has the hook for
// Bash commands, in either form (a legacy `sx hook claude` entry still
// works).
func HasClaude(settingsPath string) bool {
	return hasForm(settingsPath, hookCurrent) || hasForm(settingsPath, hookLegacy)
}

// HasLegacyClaude reports whether a Claude Code settings file has a Bash
// hook that calls `sx hook claude`, which InstallClaude rewrites to spotexec.
func HasLegacyClaude(settingsPath string) bool { return hasForm(settingsPath, hookLegacy) }

// InstallClaude adds the PreToolUse hook to a Claude Code settings file,
// preserving everything else. It is idempotent. A legacy `sx hook claude`
// entry is rewritten in place to hookCmd (or dropped, when a spotexec entry
// is already there). RTK's hook (`rtk hook claude`) is chained in place
// behind spotexec's, which then needs no entry of its own: one hook decides
// what happens to each command. Only an entry whose matcher covers Bash
// counts as installed, and an installed hook that calls spotexec by another
// path than hookCmd is updated to hookCmd's. It reports whether the file
// changed.
func InstallClaude(settingsPath, hookCmd string) (bool, error) {
	target, snapshot, settings, err := loadClaudeSettings(settingsPath)
	if err != nil {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	pre, _ := hooks["PreToolUse"].([]any)
	current, changed, chained := false, false, false
	// A chain already here (RTK's `rtk init` can add its hook again beside
	// it) takes in any further RTK hook rather than gaining a twin.
	hadChain := false
	for _, e := range pre {
		em, _ := e.(map[string]any)
		m, _ := em["matcher"].(string)
		hs, _ := em["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			c, _ := hm["command"].(string)
			if then, ok := thenOf(c); ok && isRTKHook(then) && coversBash(m) {
				hadChain = true
			}
		}
	}
	for _, e := range pre {
		em, _ := e.(map[string]any)
		m, _ := em["matcher"].(string)
		hs, _ := em["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			c, _ := hm["command"].(string)
			// Only under a matcher that surely covers Bash: a chain spotexec
			// can't count on would leave its own Bash entry running beside
			// it. An RTK hook left alone still counts as racing.
			if isRTKHook(c) && coversBash(m) && hadChain {
				hm["command"], changed = "", true // dropped below
				continue
			}
			if isRTKHook(c) && coversBash(m) {
				hm["command"] = chainCmd(hookCmd, c)
				c, changed = hm["command"].(string), true
			}
			// Only a chain that sees Bash commands stands in for spotexec's
			// own entry.
			if _, ok := thenOf(c); ok && coversBash(m) {
				chained = true
			}
		}
	}
	var keep []any
	for _, e := range pre {
		em, _ := e.(map[string]any)
		m, _ := em["matcher"].(string)
		// A hook under another matcher (Edit, say) never sees a Bash
		// command, so it doesn't stand in for spotexec's Bash entry.
		bash := coversBash(m)
		hs, _ := em["hooks"].([]any)
		// Each legacy command is rewritten in place, keeping its matcher and
		// options, unless its own entry already calls spotexec.
		has := false
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); hookForm(c) == hookCurrent {
				has = true
				current = current || bash
			}
		}
		var keepHooks []any
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			c, _ := hm["command"].(string)
			if c == "" && hm["type"] == "command" {
				continue // an RTK hook the chain already runs
			}
			if _, isChain := thenOf(c); chained && bash && !isChain && hookForm(c) != hookNone {
				// The chained entry runs spotexec's hook: another one would
				// rewrite the same commands without RTK in mind.
				changed = true
				continue
			}
			if hookForm(c) == hookLegacy {
				changed = true
				current = current || bash
				if has {
					continue
				}
				hm["command"] = hookCmd
				has = true
			} else if r := refreshHook(c, hookCmd); r != c {
				hm["command"], changed = r, true
			}
			keepHooks = append(keepHooks, h)
		}
		if len(keepHooks) != len(hs) {
			if len(keepHooks) == 0 {
				continue
			}
			em["hooks"] = keepHooks
		}
		keep = append(keep, e)
	}
	pre = keep
	if !current && !chained {
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
	if err := saveClaudeSettings(target, snapshot, settings); err != nil {
		return false, err
	}
	return true, nil
}

// UninstallClaude removes spotexec hook entries. A chained hook goes back to
// the hook it chained (RTK's), which stays installed.
func UninstallClaude(settingsPath string) (bool, error) {
	target, snapshot, settings, err := loadClaudeSettings(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // a dangling link: nothing to remove
	}
	if err != nil {
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
			c, _ := hm["command"].(string)
			if then, ok := thenOf(c); ok {
				hm["command"] = then
				removed = true
			} else if hookForm(c) != hookNone {
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
	if err := saveClaudeSettings(target, snapshot, settings); err != nil {
		return false, err
	}
	return true, nil
}

// loadClaudeSettings reads a Claude Code settings file for a change, and
// returns the file to replace: settingsPath, or the file it links to (a
// dotfiles symlink, say), resolved once so the change lands where it was
// read. A missing or blank file is an empty document. Any other read error,
// a dangling link (wrapping os.ErrNotExist), or a document whose top level,
// hooks, or hooks.PreToolUse isn't the shape Claude Code reads is an error
// before anything changes: rewriting what couldn't be read would lose the
// user's settings. Numbers are kept as written. The returned snapshot is the
// target as read (nil when it didn't exist), for saveClaudeSettings to check
// it hasn't changed since.
func loadClaudeSettings(settingsPath string) (target string, snapshot os.FileInfo, settings map[string]any, err error) {
	target, err = filepath.EvalSymlinks(settingsPath)
	if err != nil {
		if _, lerr := os.Lstat(settingsPath); !errors.Is(lerr, os.ErrNotExist) {
			return "", nil, nil, fmt.Errorf("%s: %w", settingsPath, err)
		}
		target = settingsPath // a new file
	}
	snapshot, err = os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return target, nil, map[string]any{}, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	if !snapshot.Mode().IsRegular() {
		return "", nil, nil, fmt.Errorf("%s is not a regular file", settingsPath)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return "", nil, nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return target, snapshot, map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", nil, nil, fmt.Errorf("%s is not valid JSON: %w", settingsPath, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", nil, nil, fmt.Errorf("%s is not valid JSON: data after the top-level value", settingsPath)
	}
	settings, ok := doc.(map[string]any)
	if !ok {
		return "", nil, nil, fmt.Errorf("%s is not a JSON object", settingsPath)
	}
	if h, has := settings["hooks"]; has {
		hm, ok := h.(map[string]any)
		if !ok {
			return "", nil, nil, fmt.Errorf("%s: \"hooks\" is not a JSON object", settingsPath)
		}
		if pre, has := hm["PreToolUse"]; has {
			if _, ok := pre.([]any); !ok {
				return "", nil, nil, fmt.Errorf("%s: \"hooks.PreToolUse\" is not a JSON array", settingsPath)
			}
		}
	}
	return target, snapshot, settings, nil
}

// renameFile replaces a settings file; a test makes it fail. beforeReplace
// runs just before the check that the file hasn't changed; a test changes it.
var (
	renameFile    = os.Rename
	beforeReplace = func() {}
)

// saveClaudeSettings replaces target, the file loadClaudeSettings returned,
// whole: the new document goes to a temporary file beside it, which is
// renamed over it only once fully written, so an interrupted write never
// leaves partial JSON. An existing file must be writable, as it had to be
// when it was rewritten in place, and keeps its permissions; a new one is
// created 0644 less the umask. If the target changed after it was read (an
// editor saved it, say), nothing is replaced: the save would undo that
// change.
func saveClaudeSettings(target string, snapshot os.FileInfo, settings map[string]any) error {
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	mode, existed := os.FileMode(0o644), false
	if fi, err := os.Stat(target); err == nil {
		mode, existed = fi.Mode().Perm(), true
		// Renaming over a read-only file would get around its permissions.
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		f.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := createSettingsTemp(dir, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if existed {
		if err := tmp.Chmod(mode); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	beforeReplace()
	if now, err := os.Stat(target); !sameSettings(snapshot, now, err) {
		return fmt.Errorf("%s changed while it was being updated; run the command again", target)
	}
	return renameFile(tmp.Name(), target)
}

// sameSettings reports whether a settings file is still the one read: both
// absent, or the same file with the same size and modification time.
func sameSettings(was, now os.FileInfo, err error) bool {
	if was == nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && os.SameFile(was, now) && was.Size() == now.Size() && was.ModTime().Equal(now.ModTime())
}

// createSettingsTemp creates a new hidden file in dir with mode less the
// umask (os.CreateTemp would always make it 0600).
func createSettingsTemp(dir string, mode os.FileMode) (*os.File, error) {
	for i := 0; ; i++ {
		name := filepath.Join(dir, fmt.Sprintf(".spotexec-%d-%d.tmp", os.Getpid(), time.Now().UnixNano()))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err == nil || !errors.Is(err, os.ErrExist) || i == 9 {
			return f, err
		}
	}
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
