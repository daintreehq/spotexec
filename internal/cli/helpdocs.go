package cli

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/docs"
)

// A docPage is one page of docs/ that spotexec help prints.
type docPage struct {
	name, file, desc string
}

// docPages lists the shipped pages in reading order. Every docs/*.md is
// either here or in contributorDocs (a test checks), so a new page can't
// ship without a way to open it.
var docPages = []docPage{
	{"when-to-use", "when-to-use.md", "Where it helps and where it doesn't, agents, costs"},
	{"setup", "setup.md", "spotexec setup, runners, more machines, keys, revoking"},
	{"aws", "aws.md", "Every AWS step by hand: IAM, regions, quota, networking"},
	{"running", "running.md", "Run flags, environment, exit codes, write-back"},
	{"agents", "agents.md", "What agents see, --same-slot, NDJSON events, the hook"},
	{"environments", "environments.md", "Project containers, the dependency cache, baked images"},
	{"configuration", "configuration.md", "Every key in config.toml and .spotexec.toml"},
	{"tuning", "tuning.md", "Getting a larger project's checks to pass remotely"},
	{"scaling", "scaling.md", "A small warm pool, VM costs, slots, queueing, sharding"},
	{"pool", "POOL.md", "The Spot pool's design: scheduling, placement, draining"},
	{"troubleshooting", "troubleshooting.md", "Error codes and what to do about them"},
	{"reference", "reference.md", "Every command, and every file spotexec reads or writes"},
	{"architecture", "architecture.md", "How it works, the security model, known limits"},
	{"case-study-daintree", "case-study-daintree.md", "A 114,000-test Electron app on spotexec"},
}

// contributorDocs are the pages in docs/ about working on spotexec rather
// than using it; they don't ship.
var contributorDocs = map[string]bool{"development.md": true}

// docWidth is the column pages wrap at.
const docWidth = 80

// findDocPage looks a page up by name, as spotexec help lists it, or by its
// file name (POOL.md).
func findDocPage(name string) (docPage, bool) {
	n := strings.TrimSuffix(strings.ToLower(name), ".md")
	for _, p := range docPages {
		if p.name == n {
			return p, true
		}
	}
	return docPage{}, false
}

func docPageByFile(file string) (docPage, bool) {
	for _, p := range docPages {
		if p.file == file {
			return p, true
		}
	}
	return docPage{}, false
}

func docPageNames() []string {
	names := make([]string, len(docPages))
	for i, p := range docPages {
		names[i] = p.name
	}
	return names
}

func cmdHelp(root *cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   "help [page | command]",
		Args:  cobra.ArbitraryArgs,
		Short: "Read the documentation, or a command's help",
		Long: `spotexec help lists the pages of the documentation for this version.
spotexec help <page> prints one as plain text. spotexec help <command>
prints a command's help, unless a page has the same name; spotexec <command>
--help always does.`,
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			if len(args) == 0 {
				writeDocIndex(out, root)
				return nil
			}
			if len(args) == 1 {
				if p, ok := findDocPage(args[0]); ok {
					text, err := renderDocPage(p)
					if err != nil {
						return err
					}
					_, err = io.WriteString(out, text)
					return err
				}
			}
			if cmd, rest, err := root.Find(args); err == nil && cmd != root && len(rest) == 0 {
				cmd.InitDefaultHelpFlag()
				cmd.InitDefaultVersionFlag()
				return cmd.Help()
			}
			return fmt.Errorf("no documentation page or command %q\npages: %s\n(spotexec help describes them; spotexec --help lists the commands)",
				strings.Join(args, " "), strings.Join(docPageNames(), ", "))
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
			var out []cobra.Completion
			parent := root
			if len(args) > 0 {
				cmd, rest, err := root.Find(args)
				if err != nil || len(rest) > 0 || cmd == root {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
				parent = cmd
			} else {
				for _, p := range docPages {
					if strings.HasPrefix(p.name, toComplete) {
						out = append(out, cobra.CompletionWithDesc(p.name, "page: "+p.desc))
					}
				}
			}
			for _, sub := range parent.Commands() {
				if !sub.IsAvailableCommand() || sub.Name() == "help" || !strings.HasPrefix(sub.Name(), toComplete) {
					continue
				}
				if _, page := findDocPage(sub.Name()); page && parent == root {
					continue // the page wins
				}
				out = append(out, cobra.CompletionWithDesc(sub.Name(), sub.Short))
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
	}
	// spotexec help --help and spotexec help help: the index says how it works.
	c.SetHelpFunc(func(c *cobra.Command, _ []string) { writeDocIndex(c.OutOrStdout(), root) })
	return c
}

// writeDocIndex is spotexec help with no page: the pages, and how to open
// one or a command's help.
func writeDocIndex(w io.Writer, root *cobra.Command) {
	fmt.Fprintf(w, "The documentation for spotexec %s, offline. Pages:\n\n", Version)
	for _, p := range docPages {
		fmt.Fprintf(w, "  %-21s %s\n", p.name, p.desc)
	}
	fmt.Fprint(w, `
spotexec help <page> prints a page as plain text, with no pager, for example
spotexec help running. A long page starts with a list of its sections; pipe it
through less or grep to find one.

spotexec <command> --help, or spotexec help <command>, prints a command's
flags; spotexec --help lists the commands.
`)
	var both []string
	for _, p := range docPages {
		if c, _, err := root.Find([]string{p.name}); err == nil && c != root {
			both = append(both, p.name)
		}
	}
	for _, n := range both {
		fmt.Fprintf(w, "A page wins over a command of the same name: spotexec help %[1]s is the\n%[1]s page, and spotexec %[1]s --help is the command's help.\n", n)
	}
}

func renderDocPage(p docPage) (string, error) {
	src, err := docs.Files.ReadFile(p.file)
	if err != nil {
		return "", err
	}
	return renderMarkdown(p.file, string(src), docWidth), nil
}

// A docHeading is a heading of a page, with the anchor GitHub gives it.
type docHeading struct {
	level      int
	text, slug string
}

var (
	atxHeadingRe = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	listItemRe   = regexp.MustCompile(`^( *)([-*+]|\d+[.)])[ \t]+(.*)$`)
	tableSepRe   = regexp.MustCompile(`^:?-+:?$`)
	linkRe       = regexp.MustCompile(`^\[((?:[^\]\x60]|\x60[^\x60]*\x60)*)\]\(([^)\s]*)\)`)
	italicRe     = regexp.MustCompile(`(^|[\s(])\*([^*\s](?:[^*]*[^*\s])?)\*`)
	codeComment  = regexp.MustCompile(`(^|[ \t])(#|//) `)
	docsPathRe   = regexp.MustCompile(`(^|[^\w/.-])docs/([A-Za-z0-9_-]+\.md)`)
)

// fenceOf reports a code fence's marker and indent, or "" for a line that
// isn't one.
func fenceOf(line string) (marker string, indent int) {
	t := strings.TrimLeft(line, " ")
	indent = len(line) - len(t)
	for _, m := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, m) {
			n := len(t) - len(strings.TrimLeft(t, m[:1]))
			return t[:n], indent
		}
	}
	return "", 0
}

// docHeadings lists a page's headings outside code, with GitHub's anchors.
func docHeadings(src string) []docHeading {
	var hs []docHeading
	seen := map[string]int{}
	fence := ""
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if m, _ := fenceOf(line); m != "" {
			switch {
			case fence == "":
				fence = m
			case strings.HasPrefix(m, fence) && strings.TrimSpace(strings.TrimLeft(line, " ")[len(m):]) == "":
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		g := atxHeadingRe.FindStringSubmatch(line)
		if g == nil {
			continue
		}
		slug := headingSlug(g[2])
		if n := seen[slug]; n > 0 {
			seen[slug] = n + 1
			slug = fmt.Sprintf("%s-%d", slug, n)
		} else {
			seen[slug] = 1
		}
		hs = append(hs, docHeading{level: len(g[1]), text: strings.ReplaceAll(g[2], "**", ""), slug: slug})
	}
	return hs
}

// plainHeading is a heading's text without its Markdown.
func plainHeading(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	return strings.ReplaceAll(s, "`", "")
}

// headingSlug is the anchor GitHub gives a heading: lower case, with
// everything but letters, digits, spaces, hyphens and underscores dropped,
// and spaces turned into hyphens.
func headingSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(plainHeading(s)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// docAnchors maps each embedded page's file to its anchors' headings.
func docAnchors() map[string]map[string]string {
	all := map[string]map[string]string{}
	entries, _ := docs.Files.ReadDir(".")
	for _, e := range entries {
		src, err := docs.Files.ReadFile(e.Name())
		if err != nil {
			continue
		}
		m := map[string]string{}
		for _, h := range docHeadings(string(src)) {
			m[h.slug] = h.text
		}
		all[e.Name()] = m
	}
	return all
}

// mdRenderer turns one page's Markdown into plain text for a terminal.
type mdRenderer struct {
	file    string
	width   int
	anchors map[string]map[string]string
	out     []string
}

// renderMarkdown renders a page: headings underlined, code indented,
// paragraphs and lists wrapped, tables aligned or listed, and links named
// the way they are opened in a terminal (spotexec help <page>).
func renderMarkdown(file, src string, width int) string {
	hs := docHeadings(src)
	r := &mdRenderer{file: file, width: width, anchors: docAnchors()}
	own := map[string]string{}
	for _, h := range hs {
		own[h.slug] = h.text
	}
	r.anchors[file] = own
	r.render(strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n"), hs)
	for len(r.out) > 0 && r.out[len(r.out)-1] == "" {
		r.out = r.out[:len(r.out)-1]
	}
	return strings.Join(r.out, "\n") + "\n"
}

func (r *mdRenderer) emit(lines ...string) {
	for _, l := range lines {
		r.out = append(r.out, strings.TrimRight(l, " "))
	}
}

// blank separates blocks with one empty line.
func (r *mdRenderer) blank() {
	if len(r.out) > 0 && r.out[len(r.out)-1] != "" {
		r.out = append(r.out, "")
	}
}

// startsBlock reports a line that can't continue a paragraph or list item.
func startsBlock(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "|") || strings.HasPrefix(t, ">") || atxHeadingRe.MatchString(line) || listItemRe.MatchString(line) {
		return true
	}
	m, _ := fenceOf(line)
	return m != ""
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func (r *mdRenderer) render(lines []string, headings []docHeading) {
	titled := false
	for i := 0; i < len(lines); {
		line := lines[i]
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			r.blank()
			i++
		case func() bool { m, _ := fenceOf(line); return m != "" }():
			i = r.code(lines, i)
		case atxHeadingRe.MatchString(line):
			g := atxHeadingRe.FindStringSubmatch(line)
			text := r.inline(g[2])
			r.blank()
			switch len(g[1]) {
			case 1:
				r.emit(text, strings.Repeat("=", utf8.RuneCountInString(text)))
			case 2:
				r.emit(text, strings.Repeat("-", utf8.RuneCountInString(text)))
			default:
				r.emit(text, strings.Repeat("~", utf8.RuneCountInString(text)))
			}
			r.blank()
			if len(g[1]) == 1 && !titled {
				titled = true
				r.sections(headings)
			}
			i++
		case strings.HasPrefix(t, "|"):
			j := i
			var rows []string
			for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "|") {
				rows = append(rows, strings.TrimSpace(lines[j]))
				j++
			}
			r.table(rows, indentOf(line))
			i = j
		case strings.HasPrefix(t, ">"):
			var parts []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				parts = append(parts, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
				i++
			}
			ind := strings.Repeat(" ", indentOf(line))
			r.emit(wrapText(r.inline(strings.Join(parts, " ")), r.width, ind+"> ", ind+"> ")...)
		case listItemRe.MatchString(line):
			g := listItemRe.FindStringSubmatch(line)
			parts := []string{g[3]}
			i++
			for i < len(lines) && !startsBlock(lines[i]) {
				parts = append(parts, strings.TrimSpace(lines[i]))
				i++
			}
			first := g[1] + g[2] + " "
			r.emit(wrapText(r.inline(strings.Join(parts, " ")), r.width, first, strings.Repeat(" ", len(first)))...)
		default:
			parts := []string{t}
			i++
			for i < len(lines) && !startsBlock(lines[i]) {
				parts = append(parts, strings.TrimSpace(lines[i]))
				i++
			}
			ind := strings.Repeat(" ", indentOf(line))
			r.emit(wrapText(r.inline(strings.Join(parts, " ")), r.width, ind, ind)...)
		}
	}
}

// sections lists a page's sections under its title, when it has more than
// one, so a reader can find the part they need.
func (r *mdRenderer) sections(hs []docHeading) {
	var list []string
	for _, h := range hs {
		if h.level == 2 || h.level == 3 {
			list = append(list, strings.Repeat("  ", h.level-1)+h.text)
		}
	}
	if len(list) < 2 {
		return
	}
	r.emit("Sections:")
	r.emit(list...)
	r.blank()
}

// code copies a fenced block, indented, from its opening line; it returns
// the line after the closing fence.
func (r *mdRenderer) code(lines []string, i int) int {
	marker, indent := fenceOf(lines[i])
	r.blank()
	pad := strings.Repeat(" ", indent+4)
	for i++; i < len(lines); i++ {
		l := lines[i]
		if m, _ := fenceOf(l); m != "" && strings.HasPrefix(m, marker) && strings.TrimSpace(strings.TrimLeft(l, " ")[len(m):]) == "" {
			i++
			break
		}
		l = l[min(indent, indentOf(l)):]
		// Only a comment names a page; a command's paths stay as written.
		if c := codeComment.FindStringIndex(l); c != nil {
			l = l[:c[0]] + r.docPaths(l[c[0]:])
		}
		r.emit(pad + l)
	}
	r.blank()
	return i
}

// table prints a Markdown table aligned when it fits the width, and as one
// block per row when it doesn't.
func (r *mdRenderer) table(rows []string, indent int) {
	var cells [][]string
	for n, row := range rows {
		cs := splitTableRow(row)
		sep := n == 1 // the delimiter row, under the header
		for _, c := range cs {
			if !tableSepRe.MatchString(strings.TrimSpace(c)) {
				sep = false
			}
		}
		if sep {
			continue
		}
		for k := range cs {
			cs[k] = r.inline(strings.TrimSpace(cs[k]))
		}
		cells = append(cells, cs)
	}
	if len(cells) == 0 {
		return
	}
	cols := 0
	for _, cs := range cells {
		cols = max(cols, len(cs))
	}
	widths := make([]int, cols)
	for _, cs := range cells {
		for k, c := range cs {
			widths[k] = max(widths[k], utf8.RuneCountInString(c))
		}
	}
	total := indent + 2*(cols-1)
	for _, w := range widths {
		total += w
	}
	ind := strings.Repeat(" ", indent)
	r.blank()
	if total <= r.width {
		line := func(cs []string) string {
			var b strings.Builder
			b.WriteString(ind)
			for k := 0; k < cols; k++ {
				c := ""
				if k < len(cs) {
					c = cs[k]
				}
				b.WriteString(c)
				if k < cols-1 {
					b.WriteString(strings.Repeat(" ", widths[k]-utf8.RuneCountInString(c)+2))
				}
			}
			return b.String()
		}
		r.emit(line(cells[0]))
		rule := make([]string, cols)
		for k := range rule {
			rule[k] = strings.Repeat("-", widths[k])
		}
		r.emit(line(rule))
		for _, cs := range cells[1:] {
			r.emit(line(cs))
		}
		r.blank()
		return
	}
	header := cells[0]
	var named []string
	for _, h := range header {
		if h != "" {
			named = append(named, h)
		}
	}
	if len(named) > 0 {
		r.emit(wrapText(strings.Join(named, " · "), r.width, ind, ind)...)
		r.blank()
	}
	for _, cs := range cells[1:] {
		r.emit(wrapText(cs[0], r.width, ind, ind+"  ")...)
		for k := 1; k < len(cs); k++ {
			if cs[k] == "" {
				continue
			}
			v := cs[k]
			if cols > 2 && k < len(header) && header[k] != "" {
				v = header[k] + ": " + v
			}
			r.emit(wrapText(v, r.width, ind+"    ", ind+"    ")...)
		}
		r.blank()
	}
}

// splitTableRow splits a row on its unescaped pipes outside code spans.
func splitTableRow(row string) []string {
	row = strings.TrimPrefix(strings.TrimSpace(row), "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var cells []string
	var b strings.Builder
	code := false
	for k := 0; k < len(row); k++ {
		switch c := row[k]; {
		case c == '\\' && k+1 < len(row) && row[k+1] == '|':
			b.WriteByte('|')
			k++
		case c == '`':
			code = !code
			b.WriteByte(c)
		case c == '|' && !code:
			cells = append(cells, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(cells, b.String())
}

// docPaths names a repository path to a shipped page (docs/POOL.md) the
// way it's opened.
func (r *mdRenderer) docPaths(s string) string {
	return docsPathRe.ReplaceAllStringFunc(s, func(m string) string {
		g := docsPathRe.FindStringSubmatch(m)
		if p, ok := docPageByFile(g[2]); ok {
			return g[1] + "spotexec help " + p.name
		}
		return m
	})
}

// inline renders a block's inline Markdown: code spans stay as written,
// emphasis goes, and links become their text and where to read them.
func (r *mdRenderer) inline(s string) string {
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	var b, text strings.Builder
	flush := func() {
		b.WriteString(r.docPaths(italicRe.ReplaceAllString(text.String(), "$1$2")))
		text.Reset()
	}
	for k := 0; k < len(s); {
		switch {
		case s[k] == '`':
			n := len(s[k:]) - len(strings.TrimLeft(s[k:], "`"))
			end := strings.Index(s[k+n:], s[k:k+n])
			if end < 0 {
				text.WriteString(s[k : k+n])
				k += n
				continue
			}
			flush()
			b.WriteString(s[k : k+n+end+n])
			k += n + end + n
		case s[k] == '[':
			g := linkRe.FindStringSubmatch(s[k:])
			if g == nil {
				text.WriteByte('[')
				k++
				continue
			}
			flush()
			b.WriteString(r.link(r.inline(g[1]), g[2]))
			k += len(g[0])
		case s[k] == '\\' && k+1 < len(s) && strings.ContainsRune("\\`*_[]()#|<>", rune(s[k+1])):
			flush()
			b.WriteByte(s[k+1]) // literal: no emphasis, code or link
			k += 2
		case strings.HasPrefix(s[k:], "**") && (k+2 < len(s) && !isSpace(s[k+2]) || k > 0 && !isSpace(s[k-1])):
			// A bold delimiter, paired below across code spans and links
			// (**`max_instances = 8`, as a ceiling.**).
			text.WriteByte(boldMark)
			k += 2
		default:
			text.WriteByte(s[k])
			k++
		}
	}
	flush()
	out := b.String()
	if strings.Count(out, string(boldMark))%2 == 1 {
		// An unpaired one was literal (**/*.go).
		i := strings.LastIndexByte(out, boldMark)
		out = out[:i] + "**" + out[i+1:]
	}
	return strings.ReplaceAll(out, string(boldMark), "")
}

// boldMark stands in for a ** delimiter until inline pairs them.
const boldMark = '\x01'

func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// link renders a link's text and, for a reader with only a terminal, where
// it leads: another page as spotexec help <page> and its section, a section
// of this page by name, and a web page by its URL.
func (r *mdRenderer) link(text, target string) string {
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		if text == target {
			return target
		}
		return text + " (" + target + ")"
	}
	file, anchor, _ := strings.Cut(target, "#")
	if file == "" {
		if h, ok := r.anchors[r.file][anchor]; ok {
			if sameText(text, h) {
				return text + " (a section of this page)"
			}
			return fmt.Sprintf("%s (section %q)", text, h)
		}
		return text
	}
	file = path.Clean(file)
	p, ok := docPageByFile(file)
	if !ok {
		if contributorDocs[file] {
			return text + " (docs/" + file + " in the spotexec source repository)"
		}
		return text
	}
	if h, ok := r.anchors[p.file][anchor]; ok && anchor != "" {
		if sameText(text, h) {
			return fmt.Sprintf("%s (a section of spotexec help %s)", text, p.name)
		}
		return fmt.Sprintf("%s (spotexec help %s, section %q)", text, p.name, h)
	}
	return fmt.Sprintf("%s (spotexec help %s)", text, p.name)
}

// sameText reports link text that already names the heading it leads to.
func sameText(text, heading string) bool {
	return strings.EqualFold(strings.ReplaceAll(text, "`", ""), strings.ReplaceAll(heading, "`", ""))
}

// wrapText fills words into lines of at most width columns, the first
// starting with first and the rest with rest; a word longer than a line
// gets one to itself.
func wrapText(s string, width int, first, rest string) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{strings.TrimRight(first, " ")}
	}
	var lines []string
	cur := first + words[0]
	for _, w := range words[1:] {
		if utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) > width {
			lines = append(lines, cur)
			cur = rest + w
			continue
		}
		cur += " " + w
	}
	return append(lines, cur)
}
