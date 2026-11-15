package cli

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/ui"
)

// A mistyped task name (`sx test-al`) isn't a task, so it runs as a command,
// and the shell's exit 127 reads like a broken toolchain on the runner. When
// the first word could have been meant as a task, the run's stderr is watched
// for the shell saying it found no command by that name, and a 127 then
// carries a warning that names the closest task.

// taskLikeWordRe is a plain command word: not a path, an assignment, a flag
// or a shell snippet.
var taskLikeWordRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:+@%,-]*$`)

// taskLikeWord returns the first word of a shorthand run when it could be a
// mistyped task name.
func taskLikeWord(rest []string, dashed bool) string {
	if dashed || len(rest) == 0 || !taskLikeWordRe.MatchString(rest[0]) {
		return ""
	}
	return rest[0]
}

// notFoundWatch passes writes through to w and notes whether a complete line
// is a shell saying it can't find the command word: bash's "WORD: command not
// found", dash's "WORD: not found" or zsh's "command not found: WORD".
type notFoundWatch struct {
	w    io.Writer
	word string

	mu   sync.Mutex
	line []byte // the current line so far
	long bool   // the current line outgrew maxWatchedLine: it can't match
	seen bool
}

// maxWatchedLine bounds the partial line kept between writes; a shell's
// not-found message is short, so a longer line can't be one.
const maxWatchedLine = 4096

func newNotFoundWatch(w io.Writer, word string) *notFoundWatch {
	return &notFoundWatch{w: w, word: word}
}

func (n *notFoundWatch) Write(p []byte) (int, error) {
	n.mu.Lock()
	rest := p
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		end := i
		if i < 0 {
			end = len(rest)
		}
		if !n.long && len(n.line)+end <= maxWatchedLine {
			n.line = append(n.line, rest[:end]...)
		} else {
			n.long, n.line = true, n.line[:0]
		}
		if i < 0 {
			break
		}
		if !n.long && !n.seen && notFoundLine(string(n.line), n.word) {
			n.seen = true
		}
		n.line, n.long = n.line[:0], false
		rest = rest[i+1:]
	}
	n.mu.Unlock()
	return n.w.Write(p)
}

// Seen reports whether the shell said the word wasn't found, on a complete
// line or on the last one, left without a newline.
func (n *notFoundWatch) Seen() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.seen || !n.long && notFoundLine(string(n.line), n.word)
}

func notFoundLine(line, word string) bool {
	line = strings.TrimRight(line, "\r ")
	for _, suffix := range []string{": command not found", ": not found"} {
		if s, ok := strings.CutSuffix(line, suffix); ok {
			if s, ok := strings.CutSuffix(s, word); ok && (s == "" || strings.HasSuffix(s, " ")) {
				return true
			}
		}
	}
	return strings.HasSuffix(line, "command not found: "+word)
}

// unknownTaskWarning returns the warning for a word that is neither a task
// nor, as far as the shell could tell, a command, naming the closest task
// when one is near enough.
func unknownTaskWarning(p *config.Project, word string, local bool) string {
	names := p.TaskNames()
	where := "on the runner"
	if local {
		where = "locally"
	}
	msg := fmt.Sprintf("no task is named %q, and the shell found no command by that name %s", word, where)
	if len(names) == 0 {
		return msg + " (this project defines no tasks)"
	}
	if c := closestName(word, names); c != "" {
		msg += fmt.Sprintf("; did you mean %q?", c)
	}
	return msg + " (spotexec tasks lists the tasks)"
}

// closestName returns the name nearest to word by edit distance (with
// adjacent transpositions): within 1 for words of 3 to 5 characters, 2 for
// longer ones, and never for shorter ones or when two names tie.
func closestName(word string, names []string) string {
	n := len([]rune(word))
	if n < 3 {
		return ""
	}
	limit := 1
	if n > 5 {
		limit = 2
	}
	best, bestD, tied := "", limit+1, false
	for _, name := range names {
		switch d := editDistance(strings.ToLower(word), strings.ToLower(name)); {
		case d < bestD:
			best, bestD, tied = name, d, false
		case d == bestD:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// editDistance is the optimal string alignment distance: insertions,
// deletions, substitutions and adjacent transpositions each cost 1.
func editDistance(a, b string) int {
	s, t := []rune(a), []rune(b)
	prev2 := make([]int, len(t)+1)
	prev := make([]int, len(t)+1)
	cur := make([]int, len(t)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(s); i++ {
		cur[0] = i
		for j := 1; j <= len(t); j++ {
			cost := 1
			if s[i-1] == t[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && s[i-1] == t[j-2] && s[i-2] == t[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(t)]
}

// warnUnknownTask warns, after a run that exited 127, when the watched word
// was reported as not found.
func warnUnknownTask(rep ui.Reporter, p *config.Project, watch *notFoundWatch, code int, local bool) {
	if watch == nil || code != 127 || !watch.Seen() {
		return
	}
	rep.Warn("UNKNOWN_TASK", "%s", unknownTaskWarning(p, watch.word, local))
}
