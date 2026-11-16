package hooks

// Discover reads past Claude Code sessions in a repository and sorts every
// Bash command the agent ran by what the hook does with it today: offloads
// it, leaves it alone, or keeps a heavy command local (and why). It is how
// to see which commands slip past the hook, the way `rtk discover` does for
// RTK.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Discovery is what the agent ran in a repository's past sessions.
type Discovery struct {
	Roots    []string  `json:"roots"`
	Since    time.Time `json:"since"`
	Sessions int       `json:"sessions"`
	Commands int       `json:"commands"`
	// Offloaded are the commands the hook offloads, as it decides today.
	Offloaded int `json:"offloaded"`
	// Direct are the commands the agent ran through spotexec itself.
	Direct int `json:"direct"`
	// Missed are the heavy commands kept local, most frequent first, and
	// Reasons their reasons, counted.
	Missed  []Miss   `json:"missed"`
	Reasons []Reason `json:"reasons"`
}

// Miss is one heavy command the hook keeps local.
type Miss struct {
	Command string `json:"command"`
	Reason  string `json:"reason"`
	Count   int    `json:"count"`
}

// Reason is one reason heavy commands stay local, with how often.
type Reason struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// ClaudeProjectsDir is where Claude Code keeps its session transcripts.
func ClaudeProjectsDir() string {
	return filepath.Join(filepath.Dir(GlobalSettings()), "projects")
}

// projectDirName is Claude Code's directory name for sessions started in
// dir: every character but a letter or digit becomes '-'.
func projectDirName(dir string) string {
	return nonAlnum.ReplaceAllString(dir, "-")
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// Discover reads the transcripts under projects of sessions in any of
// roots (a repository's worktrees), from since on.
func Discover(projects string, roots []string, since time.Time, d *Decider) (Discovery, error) {
	res := Discovery{Roots: roots, Since: since}
	ents, err := os.ReadDir(projects)
	if err != nil {
		return res, err
	}
	var dirs, prefixes []string // roots as written and resolved
	for _, root := range roots {
		dirs = append(dirs, root)
		if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
			dirs = append(dirs, r)
		}
	}
	for _, r := range dirs {
		prefixes = append(prefixes, projectDirName(r))
	}
	sessions := map[string]bool{}
	perms := map[string]permRules{}
	seen := map[string]bool{}
	missed := map[string]*Miss{}
	reasons := map[string]int{}
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() || !matchesPrefix(name, prefixes) {
			continue
		}
		filepath.WalkDir(filepath.Join(projects, name), func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			if fi, err := de.Info(); err != nil || fi.ModTime().Before(since) {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			for _, c := range bashCalls(f, since) {
				root := insideAny(c.cwd, dirs)
				if seen[c.id] || root == "" {
					continue
				}
				d.root, d.dir = resolve(root), resolve(c.cwd)
				rules, ok := perms[root]
				if !ok {
					rules = loadPermRules(root)
					perms[root] = rules
				}
				seen[c.id] = true
				sessions[c.session] = true
				res.Commands++
				switch kind, why := classifyPast(d, c.command, c.cwd, rules); kind {
				case pastOffloaded:
					res.Offloaded++
				case pastDirect:
					res.Direct++
				case pastMissed:
					key := strings.Join(strings.Fields(c.command), " ")
					if m := missed[key]; m != nil {
						m.Count++
					} else {
						missed[key] = &Miss{Command: key, Reason: why, Count: 1}
					}
					reasons[reasonKind(why)]++
				}
			}
			return nil
		})
	}
	res.Sessions = len(sessions)
	for _, m := range missed {
		res.Missed = append(res.Missed, *m)
	}
	sort.Slice(res.Missed, func(i, j int) bool {
		a, b := res.Missed[i], res.Missed[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Command < b.Command
	})
	for r, n := range reasons {
		res.Reasons = append(res.Reasons, Reason{r, n})
	}
	sort.Slice(res.Reasons, func(i, j int) bool {
		a, b := res.Reasons[i], res.Reasons[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Reason < b.Reason
	})
	return res, nil
}

func matchesPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p+"-") {
			return true
		}
		// Claude Code cuts a name past 200 characters and adds a hash:
		// the cwd of each call decides then.
		if len(p) > maxDirName && strings.HasPrefix(name, p[:maxDirName]) {
			return true
		}
	}
	return false
}

const maxDirName = 200

// insideAny returns the root dir is in, or "".
func insideAny(dir string, roots []string) string {
	if dir == "" {
		return ""
	}
	for _, r := range roots {
		if dir == r || strings.HasPrefix(dir, r+string(filepath.Separator)) {
			return r
		}
	}
	return ""
}

// Worktrees are the roots of every worktree of the repository at dir
// (dir alone outside git).
func Worktrees(dir string) []string {
	out, err := exec.Command("git", "-C", dir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return []string{dir}
	}
	var roots []string
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			roots = append(roots, p)
		}
	}
	if len(roots) == 0 {
		return []string{dir}
	}
	// Nested worktrees: the deepest root first, so a session is counted
	// under the worktree it ran in.
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return roots
}

type bashCall struct {
	id, session, cwd, command string
}

// bashCalls reads the Bash tool calls of one transcript, from since on.
func bashCalls(r io.Reader, since time.Time) []bashCall {
	var out []bashCall
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := readLine(br, maxLine)
		if len(line) > 0 && bytes.Contains(line, []byte(`"Bash"`)) {
			var e struct {
				Type      string `json:"type"`
				Cwd       string `json:"cwd"`
				SessionID string `json:"sessionId"`
				Timestamp string `json:"timestamp"`
				Message   struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &e) == nil && e.Type == "assistant" {
				ts, terr := time.Parse(time.RFC3339Nano, e.Timestamp)
				var blocks []struct {
					Type  string `json:"type"`
					ID    string `json:"id"`
					Name  string `json:"name"`
					Input struct {
						Command    string `json:"command"`
						Background bool   `json:"run_in_background"`
					} `json:"input"`
				}
				if (terr != nil || !ts.Before(since)) && json.Unmarshal(e.Message.Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type == "tool_use" && b.Name == "Bash" && b.Input.Command != "" && !b.Input.Background {
							out = append(out, bashCall{b.ID, e.SessionID, e.Cwd, b.Input.Command})
						}
					}
				}
			}
		}
		if err != nil {
			return out
		}
	}
}

// maxLine caps a transcript line: longer ones (a huge tool result) are
// skipped, as a command never comes close.
const maxLine = 32 << 20

// readLine reads one line, or nil for one longer than max (read past).
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) <= limit {
			line = append(line, chunk...)
		} else {
			line = line[:0:0]
			limit = -1 // too long: drop the rest of it
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if limit < 0 {
			return nil, err
		}
		return line, err
	}
}

const (
	pastOther = iota
	pastOffloaded
	pastDirect
	pastMissed
)

// classifyPast sorts one past command; why is the reason a heavy one stays
// local.
func classifyPast(d *Decider, command, cwd string, rules permRules) (kind int, why string) {
	norm := Normalize(command)
	line, _, _ := UnwrapRTK(norm)
	ok, why := d.Why(line)
	if ok {
		if hasCd(line) && !cdsInside(line, cwd) {
			return pastMissed, "a cd leaves the repository"
		}
		if p := checkPermissions(norm, d.prefixes, rules); p.Verdict == VerdictDeny {
			return pastMissed, "your deny rule " + p.Rule + " (" + shortFile(p.File) + ")"
		}
		return pastOffloaded, ""
	}
	if runsSpotexec(norm) {
		return pastDirect, ""
	}
	if strings.Contains(norm, "SPOTEXEC_LOCAL=1") || !d.mentionsHeavy(norm) {
		return pastOther, ""
	}
	return pastMissed, why
}

func hasCd(line string) bool {
	head, _, _ := splitFilters(line)
	segs, _ := splitList(head)
	return slices.ContainsFunc(segs, isCd)
}

// runsSpotexec reports a line with a command that is spotexec (or sx)
// itself, not one that only names it (`go test ./cmd/spotexec`).
func runsSpotexec(line string) bool {
	segs, _ := splitList(line)
	for _, seg := range segs {
		parts, _, ok := splitPipes(seg)
		if !ok {
			parts = []string{seg}
		}
		for _, p := range parts {
			_, cmd, _ := splitPrefix(strings.TrimSpace(p), nil)
			if w := rawWords(cmd); len(w) > 0 {
				switch filepath.Base(w[0].text) {
				case "spotexec", "sx":
					return true
				}
			}
		}
	}
	return false
}

// commandStart are words after which a new command starts.
var commandStart = map[string]bool{
	"&&": true, "||": true, "|": true, "|&": true, ";": true, "then": true, "do": true, "else": true,
	"{": true, "(": true, "!": true, "time": true, "nohup": true, "exec": true, "xargs": true,
	"sudo": true, "command": true, "env": true, "nice": true, "timeout": true, "watch": true, "-c": true,
}

// mentionsHeavy reports whether a line runs a heavy command anywhere, at a
// place a command starts: not an argument (`git commit -m "fix pnpm
// test"`) and not a dev server or watch mode, which stay local on purpose.
func (d *Decider) mentionsHeavy(line string) bool {
	if i := strings.Index(line, "<<"); i >= 0 {
		// A here-document's body is data: only the line that starts it.
		if j := strings.IndexByte(line[i:], '\n'); j >= 0 {
			line = line[:i+j]
		}
	}
	f := strings.Fields(blankQuoted(line))
	for i := range f {
		if i > 0 {
			p := f[i-1]
			if !commandStart[p] && !strings.HasSuffix(p, ";") && !strings.HasSuffix(p, "&&") &&
				!assignName.MatchString(p) && !durationArg.MatchString(p) {
				continue
			}
		}
		// The patterns read a command's first words only.
		sub := strings.TrimLeft(strings.Join(f[i:min(i+12, len(f))], " "), "'\"(`$")
		at := place{root: d.root}
		form := uvForm(toolForm(sub, at), at)
		for _, re := range d.include {
			if re.MatchString(sub) || re.MatchString(form) {
				if localCommand(form) {
					return false
				}
				return true
			}
		}
	}
	return false
}

// blankQuoted is line with the contents of its quoted strings blanked, so
// prose in them (`git commit -m 'retry && pnpm test'`) reads as one word.
func blankQuoted(line string) string {
	b := []byte(line)
	var q byte
	for i := 0; i < len(b); i++ {
		ch := b[i]
		switch {
		case q == '"' && ch == '\\' && i+1 < len(b):
			b[i], b[i+1] = '_', '_'
			i++
		case q != 0 && ch == q:
			q = 0
		case q != 0:
			b[i] = '_'
		case ch == '\\' && i+1 < len(b):
			i++
		case ch == '\'' || ch == '"':
			q = ch
		}
	}
	return string(b)
}

var quoted = regexp.MustCompile("`[^`]*`")

// reasonKind is a reason without the command it quotes, for counting.
func reasonKind(why string) string {
	return quoted.ReplaceAllString(why, "a part")
}
