package hooks

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Normalize rewrites a command line into the same command as one line, the
// way bash reads it, so the decider can judge it:
//   - a line continuation (backslash-newline, outside single quotes) is
//     removed, as bash removes it;
//   - a comment (an unquoted '#' that begins a word) is dropped, up to the
//     end of its line;
//   - an unquoted newline that ends a command becomes "; ", and one after a
//     trailing && || or | (which continue the command) a space; blank lines
//     are dropped.
//
// Anything it can't read as plain lines (a here-document, a line starting
// with an operator, unbalanced quotes) comes back unchanged, where the
// newline keeps it local. Newlines inside quotes are data and stay.
func Normalize(c string) string {
	if !strings.ContainsAny(c, "\\#\n") {
		return c
	}
	if strings.ContainsRune(c, '\r') {
		return c // bash keeps a CR as part of a word: not a line break
	}
	var b strings.Builder
	var q byte // open quote
	line := 0  // where the current output line starts in b
	ok := true // false: give up and return c
	wordStart := func() bool {
		s := b.String()
		if len(s) == 0 {
			return true
		}
		return strings.IndexByte(" \t\n;&|()", s[len(s)-1]) >= 0
	}
	for i := 0; i < len(c) && ok; i++ {
		ch := c[i]
		switch {
		case q == '\'':
			b.WriteByte(ch)
			if ch == '\'' {
				q = 0
			}
		case ch == '\\' && i+1 < len(c) && c[i+1] == '\n':
			i++ // a line continuation, inside double quotes too
		case q == '"':
			b.WriteByte(ch)
			if ch == '\\' && i+1 < len(c) {
				i++
				b.WriteByte(c[i])
			} else if ch == '"' {
				q = 0
			}
		case ch == '\\':
			b.WriteByte(ch)
			if i+1 < len(c) {
				i++
				b.WriteByte(c[i])
			}
		case ch == '\'' || ch == '"':
			q = ch
			b.WriteByte(ch)
		case ch == '#' && wordStart():
			for i+1 < len(c) && c[i+1] != '\n' {
				i++
			}
		case ch == '<' && i+1 < len(c) && c[i+1] == '<':
			ok = false // a here-document's body is data, not lines
		case ch == '\n':
			s := strings.TrimRight(b.String(), " \t")
			cur := strings.TrimSpace(s[min(line, len(s)):])
			b.Reset()
			b.WriteString(s)
			switch {
			case cur == "":
				// A blank line, or the first: nothing to end.
			case strings.HasSuffix(cur, "&&") || strings.HasSuffix(cur, "||") || strings.HasSuffix(cur, "|"):
				b.WriteByte(' ')
			case strings.HasSuffix(cur, ";") && !strings.HasSuffix(cur, ";;"):
				b.WriteByte(' ')
			default:
				b.WriteString("; ")
			}
			line = b.Len()
			// A command line can't start with an operator.
			rest := strings.TrimLeft(c[i+1:], " \t")
			if rest != "" && strings.IndexByte("|&;)", rest[0]) >= 0 {
				ok = false
			}
		default:
			b.WriteByte(ch)
		}
	}
	if !ok || q != 0 {
		return c
	}
	out := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(b.String()), ";"))
	if strings.HasSuffix(out, ";") || strings.HasSuffix(out, "&") && !strings.HasSuffix(out, "&&") {
		return c // `a; ;` and `a;;`: bash refuses them; `a &`: a background job
	}
	return out
}

// Wrapper commands that run the command after them unchanged (`timeout 600
// pnpm test`, `env CI=1 cargo test`). The decider looks past them to judge
// the command they run, and the whole segment, wrapper included, runs on
// the runner. sudo never counts: the runner's root is not the agent's.
var (
	durationArg = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
	niceArg     = regexp.MustCompile(`^-?[0-9]+$`)
)

// peelCtx is where in a segment peelOne is reading.
type peelCtx struct {
	// first: s starts its segment, where bash reads `time` as a keyword.
	first bool
	// builtin: s runs in the shell (only assignments before it), where
	// `command` is the builtin; after env, nice or timeout it would be an
	// executable, which a runner needn't have.
	builtin bool
}

// peelOne strips one wrapper or run of leading assignments from s (a list
// segment without operators) and returns what it runs. external says the
// wrapper is an executable that runs the rest as a new process.
func peelOne(s string, at peelCtx, user []string) (rest string, external, ok bool) {
	if e := envPrefix(s); e != "" {
		return s[len(e):], false, true
	}
	words := rawWords(s)
	if len(words) < 2 {
		return "", false, false
	}
	after := func(n int) (string, bool, bool) {
		if n >= len(words) {
			return "", true, true // a wrapper with nothing to run
		}
		return s[words[n].start:], true, true
	}
	w := words[0].text
	if !strings.HasPrefix(s, w) {
		return "", false, false // quoted or escaped: an executable's name, not the keyword or builtin
	}
	switch w {
	case "env", "command", "nohup":
		// env and command take options that change what runs (env -i,
		// command -v); those aren't looked past.
		if strings.HasPrefix(words[1].text, "-") && w != "nohup" || w == "command" && !at.builtin {
			return "", false, false
		}
		rest, _, ok := after(1)
		return rest, w != "command", ok // the builtin runs the rest itself
	case "time":
		if !at.first {
			return "", false, false // not the keyword: /usr/bin/time, if installed
		}
		n := 1
		if words[n].text == "-p" {
			n++
		}
		rest, _, ok := after(n)
		return rest, false, ok // the keyword: the shell runs the rest
	case "timeout":
		n := 1
		for n < len(words) && strings.HasPrefix(words[n].text, "-") {
			switch o := words[n].text; {
			case o == "--preserve-status" || o == "--foreground" || o == "-v" || o == "--verbose":
			case o == "-s" || o == "-k" || o == "--signal" || o == "--kill-after":
				n++
			case strings.HasPrefix(o, "-s") || strings.HasPrefix(o, "-k") ||
				strings.HasPrefix(o, "--signal=") || strings.HasPrefix(o, "--kill-after="):
			default:
				return "", false, false
			}
			n++
		}
		if n >= len(words) || !durationArg.MatchString(words[n].text) {
			return "", false, false
		}
		return after(n + 1)
	case "nice":
		n := 1
		switch o := words[1].text; {
		case o == "-n" || o == "--adjustment":
			if len(words) < 3 || !niceArg.MatchString(words[2].text) {
				return "", false, false
			}
			n = 3
		case strings.HasPrefix(o, "--adjustment=") || strings.HasPrefix(o, "-n") && niceArg.MatchString(o[2:]):
			n = 2
		case niceArg.MatchString(o) && strings.HasPrefix(o, "-"):
			n = 2
		case strings.HasPrefix(o, "-"):
			return "", false, false
		}
		return after(n)
	}
	for _, p := range user {
		if rest, ok := strings.CutPrefix(s, p); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			return strings.TrimLeft(rest, " \t"), true, true
		}
	}
	return "", false, false
}

// splitPrefix splits a list segment into its wrappers and leading
// assignments, and the command they run: seg == prefix + cmd. ok is false
// when a wrapper runs nothing.
func splitPrefix(seg string, user []string) (prefix, cmd string, ok bool) {
	rs, ok := peelAll(seg, user)
	cmd = rs[len(rs)-1]
	return seg[:len(seg)-len(cmd)], cmd, ok
}

// peelAll is seg as written, then after each wrapper or assignment run in
// turn. ok is false when a wrapper runs nothing.
func peelAll(seg string, user []string) (rs []string, ok bool) {
	rs = []string{seg}
	at := peelCtx{first: true, builtin: true}
	cmd := seg
	for i := 0; i < 16; i++ {
		rest, external, peeled := peelOne(cmd, at, user)
		if !peeled {
			break
		}
		if strings.TrimSpace(rest) == "" {
			return []string{seg}, false
		}
		at.first = false
		at.builtin = at.builtin && !external
		cmd = rest
		rs = append(rs, cmd)
	}
	return rs, true
}

// readings are seg as written, then after each wrapper or assignment run in
// turn: every way a deny or ask rule might name the command.
func readings(seg string, user []string) []string {
	rs, _ := peelAll(seg, user)
	return rs
}

type rawWord struct {
	text  string // unquoted
	start int    // byte offset in the string
}

// rawWords splits a command without operators into words, with offsets.
func rawWords(s string) []rawWord {
	var out []rawWord
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return out
		}
		w, rest, _, ok := shellWord(s[i:])
		if !ok || len(rest) == len(s[i:]) {
			return out
		}
		out = append(out, rawWord{w, i})
		i = len(s) - len(rest)
	}
}

// toolForm is cmd as the default patterns read it: a tool called by a path
// inside the repository by its name (`.venv/bin/pytest`,
// `node_modules/.bin/vitest`), and the global options a package manager or
// build tool takes before its subcommand left out (`pnpm --filter web test`
// is `pnpm test`, `make -C sub test` is `make test`). It is for matching
// only: the command runs as written. A directory option must stay inside
// the repository, as a cd must.
func toolForm(cmd string, at place) string {
	form, _ := toolFormOK(cmd, at)
	return form
}

// toolFormOK is toolForm, with escapes true when a directory or file option
// of the tool leaves the repository (the command must stay local).
func toolFormOK(cmd string, at place) (form string, escapes bool) {
	words := rawWords(cmd)
	if len(words) == 0 {
		return cmd, false
	}
	tool := words[0].text
	if strings.Contains(tool, "/") {
		if !at.inside(tool) {
			return cmd, false
		}
		tool = filepath.Base(tool)
	}
	spec, ok := globalOpts[tool]
	n := 1
	if ok {
		// Directory options anywhere move the tool (make accepts
		// `make test -C ../x`).
		if !spec.pathsInside(words[1:], at) {
			return cmd, true
		}
		n = min(spec.skip(words, at), len(words))
		if n < 0 {
			return cmd, false
		}
	}
	if n >= len(words) {
		return tool, false
	}
	return tool + " " + cmd[words[n].start:], false
}

// place is where a command runs: the repository root and the directory
// (both with symlinks resolved; "" when unknown).
type place struct{ root, dir string }

// inside reports whether a path word names something inside the
// repository. Relative paths resolve from dir, through symlinks; with no
// dir, a relative path without .. counts (the agent's cds already stay
// inside). An absolute path must be under root.
func (at place) inside(p string) bool {
	if p == "" || strings.ContainsAny(p, "$`~*?[") {
		return false
	}
	base := "/"
	if !filepath.IsAbs(p) {
		if at.dir == "" || at.root == "" {
			for _, part := range strings.Split(p, "/") {
				if part == ".." {
					return false
				}
			}
			return true
		}
		base = at.dir
	}
	if at.root == "" {
		return false
	}
	c := physical(base, p)
	return c == at.root || strings.HasPrefix(c, at.root+string(filepath.Separator))
}

// physical resolves p from base as chdir and exec do: each .. goes up from
// where the symlinks before it lead, not from the path as written
// (`link/..` is link's target's parent).
func physical(base, p string) string {
	cur := base
	if filepath.IsAbs(p) {
		cur = "/"
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
		case "..":
			if r, err := filepath.EvalSymlinks(cur); err == nil {
				cur = r
			}
			cur = filepath.Dir(cur)
		default:
			cur = filepath.Join(cur, part)
		}
	}
	return resolve(cur)
}

// resolve resolves the symlinks of the longest existing part of an absolute
// path (a tool not installed yet still resolves through /var -> /private/var).
func resolve(p string) string {
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// optSpec is a tool's global options: flags, options taking a value, and
// options whose value is a directory or file (which must stay inside the
// repository). sub, when set, is a subcommand that takes one argument and
// leaves the rest to the tool (`yarn workspace web test`).
type optSpec struct {
	flags  []string
	values []string
	paths  []string
	sub    string
	// jobs: -j may take a value attached, separately (if numeric), or none.
	jobs bool
}

func (o optSpec) skip(words []rawWord, at place) int {
	n := 1
	for n < len(words) {
		w := words[n].text
		if o.sub != "" && w == o.sub {
			if n+2 >= len(words) {
				return -1
			}
			n += 2
			continue
		}
		if !strings.HasPrefix(w, "-") || w == "-" || w == "--" {
			return n
		}
		name, val, hasVal := strings.Cut(w, "=")
		switch {
		case o.jobs && (name == "-j" || name == "--jobs"):
			if !hasVal && n+1 < len(words) && niceArg.MatchString(words[n+1].text) {
				n++
			}
		case o.jobs && strings.HasPrefix(w, "-j") && niceArg.MatchString(w[2:]):
		case slices.Contains(o.flags, name) && !hasVal:
		case slices.Contains(o.values, name):
			if !hasVal {
				n++
			}
		case slices.Contains(o.paths, name):
			if !hasVal {
				n++
				if n >= len(words) {
					return -1
				}
				val = words[n].text
			}
			if !at.inside(val) {
				return -1
			}
		case len(name) > 2 && name[1] != '-' && slices.Contains(o.paths, name[:2]):
			// An attached short value: -Csub.
			if !at.inside(name[2:]) {
				return -1
			}
		default:
			return -1 // an option this doesn't know may change what runs
		}
		n++
	}
	return n
}

// pathsInside reports whether every directory or file option in words (the
// rest of the command, after the subcommand) stays inside the repository.
func (o optSpec) pathsInside(words []rawWord, at place) bool {
	seen := map[string]bool{}
	for i, w := range words {
		name, val, hasVal := strings.Cut(w.text, "=")
		if slices.Contains(o.paths, name) || len(name) > 2 && name[0] == '-' && name[1] != '-' && slices.Contains(o.paths, name[:2]) {
			// A second one applies after the first (make -C a -C b):
			// too clever to follow.
			key := name
			if len(name) > 2 && name[1] != '-' {
				key = name[:2]
			}
			if seen[key] {
				return false
			}
			seen[key] = true
		} else if len(name) > 2 && name[0] == '-' && name[1] != '-' {
			// A bundle of short options (make -sC dir): one of them may
			// take a directory.
			for _, p := range o.paths {
				if len(p) == 2 && strings.ContainsRune(name[2:], rune(p[1])) {
					return false
				}
			}
		}
		switch {
		case w.text == "--":
			return true
		case slices.Contains(o.paths, name):
			if !hasVal {
				if i+1 >= len(words) {
					return false
				}
				val = words[i+1].text
			}
			if !at.inside(val) {
				return false
			}
		case len(name) > 2 && name[0] == '-' && name[1] != '-' && slices.Contains(o.paths, name[:2]):
			if !at.inside(name[2:]) {
				return false
			}
		}
	}
	return true
}

var globalOpts = map[string]optSpec{
	"pnpm": {
		flags: []string{"-r", "--recursive", "-w", "--workspace-root", "-s", "--silent", "--parallel", "--stream",
			"--if-present", "--aggregate-output", "--no-bail", "--bail", "--sort", "--no-sort",
			"--fail-if-no-match", "--include-workspace-root", "--color", "--no-color", "--frozen-lockfile"},
		values: []string{"--filter", "-F", "--filter-prod", "--workspace-concurrency", "--reporter", "--loglevel",
			"--test-pattern", "--changed-files-ignore-pattern"},
		paths: []string{"-C", "--dir"},
	},
	"npm": {
		flags:  []string{"--workspaces", "-ws", "-s", "--silent", "--if-present", "--include-workspace-root", "-q", "--quiet", "--no-color"},
		values: []string{"-w", "--workspace", "--loglevel"},
		paths:  []string{"--prefix"},
	},
	"yarn": {
		flags: []string{"-s", "--silent"},
		paths: []string{"--cwd"},
		sub:   "workspace",
	},
	"bun": {
		flags:  []string{"--silent", "--bun"},
		values: []string{"--filter", "-F"},
		paths:  []string{"--cwd"},
	},
	"npx": {
		flags:  []string{"-y", "--yes", "--no", "--no-install", "-q", "--quiet"},
		values: []string{"-p", "--package"},
	},
	"bunx": {
		flags:  []string{"--bun"},
		values: []string{"-p", "--package"},
	},
	"make": {
		flags: []string{"-k", "--keep-going", "-s", "--silent", "--quiet", "-B", "--always-make",
			"--no-print-directory", "-w", "--print-directory", "-O", "--output-sync"},
		paths: []string{"-C", "--directory", "-f", "--file", "--makefile"},
		jobs:  true,
	},
	"just": {
		paths: []string{"-f", "--justfile", "-d", "--working-directory"},
	},
	"go": {
		paths: []string{"-C"},
	},
	"cargo": {
		flags:  []string{"-q", "--quiet", "-v", "-vv", "--verbose", "--locked", "--frozen", "--offline"},
		values: []string{"--color", "--config"},
	},
}

// uvRunOpts are the options of `uv run` itself, before the command it runs.
var uvRunOpts = optSpec{
	flags: []string{"--frozen", "--locked", "--no-sync", "--all-extras", "--all-packages", "--isolated",
		"--no-dev", "--active", "--exact", "-q", "--quiet"},
	values: []string{"--with", "--extra", "--group", "--package", "--python", "-p", "--only-group"},
}

// uvForm is `uv run <command>` without uv run's own options.
func uvForm(form string, at place) string {
	if !strings.HasPrefix(form, "uv run ") {
		return form
	}
	words := rawWords(form)
	// words[0] is uv, words[1] run: skip from there.
	n := uvRunOpts.skip(words[1:], at)
	if n < 0 || n+1 >= len(words) {
		return form
	}
	return "uv run " + form[words[n+1].start:]
}

// gitRootResolved is the repository root with symlinks resolved, for
// pathInside.
func gitRootResolved(dir string) string {
	root := gitRoot(dir)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	return root
}
