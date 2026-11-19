package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/docs"
	"github.com/daintreehq/spotexec/internal/client"
)

// helpOut runs spotexec with args and returns its exit code, stdout and
// stderr.
func helpOut(t *testing.T, name string, args ...string) (int, string, string) {
	t.Helper()
	var code int
	var stderr string
	out := captureStdout(t, func() { code, stderr = runMain(t, name, args...) })
	return code, out, stderr
}

// Every page in docs/ ships with the binary unless it is declared a
// contributor page, so a new page can't be left without a way to open it.
func TestEveryDocIsAPageOrAContributorDoc(t *testing.T) {
	entries, err := docs.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, e := range entries {
		files[e.Name()] = true
		_, page := docPageByFile(e.Name())
		if page == contributorDocs[e.Name()] {
			t.Errorf("docs/%s: list it in docPages (to ship it with spotexec help) or contributorDocs (if it is only for contributors), not both or neither", e.Name())
		}
	}
	names := map[string]bool{}
	for _, p := range docPages {
		if !files[p.file] {
			t.Errorf("page %s: docs/%s doesn't exist", p.name, p.file)
		}
		if names[p.name] {
			t.Errorf("page name %s is used twice", p.name)
		}
		names[p.name] = true
		if want := strings.TrimSuffix(strings.ToLower(p.file), ".md"); p.name != want {
			t.Errorf("page %s: name it %s, after its file", p.name, want)
		}
		// The index line, "  name  desc", fits the page width.
		if n := 2 + 21 + 1 + len(p.desc); n > docWidth {
			t.Errorf("page %s: description is %d columns too long for the index", p.name, n-docWidth)
		}
	}
	for f := range contributorDocs {
		if !files[f] {
			t.Errorf("contributor doc docs/%s doesn't exist", f)
		}
	}
}

// Every shipped page opens through spotexec help, with no Markdown link left
// for a terminal reader to follow.
func TestEveryPageOpensAsPlainText(t *testing.T) {
	mdLink := regexp.MustCompile(`\]\([^)]*\)|[A-Za-z_-]+\.md\b`)
	for _, p := range docPages {
		code, out, stderr := helpOut(t, "spotexec", "help", p.name)
		if code != 0 || stderr != "" {
			t.Errorf("spotexec help %s: exit %d, stderr %q", p.name, code, stderr)
			continue
		}
		src, _ := docs.Files.ReadFile(p.file)
		title := plainHeading(strings.TrimPrefix(strings.SplitN(string(src), "\n", 2)[0], "# "))
		if !strings.HasPrefix(out, title+"\n"+strings.Repeat("=", len([]rune(title)))+"\n") {
			t.Errorf("spotexec help %s doesn't start with its title %q: %.80q", p.name, title, out)
		}
		for i, line := range strings.Split(out, "\n") {
			if m := mdLink.FindString(line); m != "" && !strings.Contains(line, "docs/development.md in the spotexec source repository") {
				t.Errorf("spotexec help %s, line %d: %q is a repository link: %q", p.name, i+1, m, line)
			}
		}
		// A page name opens the same page however it is spelled.
		if _, again, _ := helpOut(t, "spotexec", "help", strings.ToUpper(p.file)); again != out {
			t.Errorf("spotexec help %s differs from spotexec help %s", strings.ToUpper(p.file), p.name)
		}
	}
}

// Every link in a shipped page leads to a page that ships (or a contributor
// doc) and to a heading that exists there.
func TestDocLinksResolve(t *testing.T) {
	anchors := docAnchors()
	inlineLink := regexp.MustCompile(`\[(?:[^\]\x60]|\x60[^\x60]*\x60)*\]\(([^)\s]*)\)`)
	for _, p := range docPages {
		src, _ := docs.Files.ReadFile(p.file)
		fence := false
		for i, line := range strings.Split(string(src), "\n") {
			if m, _ := fenceOf(line); m != "" {
				fence = !fence
				continue
			}
			if fence {
				continue
			}
			// Link text may hold code; a code span outside one is no link.
			for _, g := range inlineLink.FindAllStringSubmatch(line, -1) {
				target := g[1]
				before, _, _ := strings.Cut(line, g[0])
				if strings.Contains(target, "://") || strings.Count(before, "`")%2 == 1 {
					continue // a web page, or link-like text in code
				}
				file, anchor, _ := strings.Cut(target, "#")
				if file == "" {
					file = p.file
				}
				if _, ok := docPageByFile(file); !ok {
					if !contributorDocs[file] {
						t.Errorf("docs/%s:%d links to %s, which spotexec help can't open", p.file, i+1, target)
					}
					continue
				}
				if _, ok := anchors[file][anchor]; anchor != "" && !ok {
					t.Errorf("docs/%s:%d links to %s, a heading that doesn't exist", p.file, i+1, target)
				}
			}
		}
	}
}

func TestHelpListsThePages(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"help", "help"}, {"help", "--help"}} {
		code, out, stderr := helpOut(t, "spotexec", args...)
		if code != 0 || stderr != "" {
			t.Fatalf("spotexec %v: exit %d, stderr %q", args, code, stderr)
		}
		for _, p := range docPages {
			if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(p.name) + ` +` + regexp.QuoteMeta(p.desc) + `$`).MatchString(out) {
				t.Errorf("spotexec %v doesn't list %s with its description:\n%s", args, p.name, out)
			}
		}
		for _, want := range []string{
			"spotexec help <page> prints a page",
			"spotexec <command> --help",
			// setup is a page and a command: the page wins, and says so.
			"spotexec help setup is the\nsetup page, and spotexec setup --help is the command's help.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("spotexec %v lacks %q:\n%s", args, want, out)
			}
		}
		if strings.Contains(out, "development") {
			t.Errorf("spotexec %v lists the contributor docs", args)
		}
	}
}

// setup is both a page and a command: spotexec help setup is the page, and
// spotexec setup --help is still the command's help.
func TestHelpPageWinsOverCommand(t *testing.T) {
	_, page, _ := helpOut(t, "spotexec", "help", "setup")
	if !strings.HasPrefix(page, "Setting up machines\n") {
		t.Errorf("spotexec help setup isn't the setup page: %.80q", page)
	}
	code, cmd, _ := helpOut(t, "spotexec", "setup", "--help")
	if code != 0 || !strings.Contains(cmd, "Usage:\n  spotexec setup") || strings.Contains(cmd, "Setting up machines\n===") {
		t.Errorf("spotexec setup --help: exit %d, %.200q", code, cmd)
	}
}

// A command that isn't a page still gets its help from spotexec help, nested
// commands too.
func TestHelpForACommand(t *testing.T) {
	for _, path := range [][]string{{"status"}, {"container", "build"}} {
		code, out, stderr := helpOut(t, "spotexec", append([]string{"help"}, path...)...)
		if code != 0 || stderr != "" || !strings.Contains(out, "Usage:\n  spotexec "+strings.Join(path, " ")) {
			t.Errorf("spotexec help %v: exit %d, stdout %.200q, stderr %q", path, code, out, stderr)
		}
	}
}

// A name that is neither a page nor a command lists the pages and fails,
// with nothing on stdout for a pipe to take as the page.
func TestHelpUnknownPage(t *testing.T) {
	for _, args := range [][]string{{"help", "nope"}, {"help", "container", "nope"}, {"help", "status", "extra"}, {"help", "development"}} {
		code, out, stderr := helpOut(t, "spotexec", args...)
		if code != 1 || out != "" || !strings.Contains(stderr, "no documentation page or command") || !strings.Contains(stderr, strings.Join(docPageNames(), ", ")) {
			t.Errorf("spotexec %v: exit %d, stdout %.80q, stderr %q", args, code, out, stderr)
		}
	}
}

// spotexec --help, bare spotexec and sx -h point at spotexec help.
func TestHelpPointsAtTheDocumentation(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"spotexec", []string{"--help"}},
		{"spotexec", []string{}},
		{"sx", []string{"-h"}},
	} {
		_, out, _ := helpOut(t, c.name, c.args...)
		if !strings.Contains(out, "spotexec help") {
			t.Errorf("%s %v doesn't mention spotexec help:\n%s", c.name, c.args, out)
		}
	}
}

// sx help still runs a command called help: the unknown runner shows it got
// as far as choosing one.
func TestSxHelpIsStillACommand(t *testing.T) {
	dir := project(t, "")
	code, stderr := runMain(t, "sx", "-C", dir, "-r", "nope", "help")
	if code != client.ExitInfra || !strings.Contains(stderr, `unknown runner "nope"`) {
		t.Errorf("sx help: exit %d, stderr %q", code, stderr)
	}
}

func TestHelpCompletesPagesAndCommands(t *testing.T) {
	_, out, _ := helpOut(t, "spotexec", "__complete", "help", "")
	for _, want := range []string{"running\tpage: ", "setup\tpage: ", "status\t", "container\t"} {
		if !strings.Contains(out, "\n"+want) && !strings.HasPrefix(out, want) {
			t.Errorf("spotexec help completion lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\nsetup\t")+strings.Count("\n"+out[:min(len(out), 6)], "\nsetup\t") > 1 {
		t.Errorf("setup completes twice:\n%s", out)
	}
	_, out, _ = helpOut(t, "spotexec", "__complete", "help", "container", "")
	if !strings.Contains(out, "build\t") || strings.Contains(out, "running\t") {
		t.Errorf("spotexec help container completion:\n%s", out)
	}
}

func TestHeadingSlug(t *testing.T) {
	for in, want := range map[string]string{
		"3. Grant permissions":                            "3-grant-permissions",
		"Global config: `~/.config/spotexec/config.toml`": "global-config-configspotexecconfigtoml",
		"Size worker pools with `$SPOTEXEC_CPUS`":         "size-worker-pools-with-spotexec_cpus",
		"Simulation (`type = \"sim\"`)":                   "simulation-type--sim",
		"One VM per part: the lesson":                     "one-vm-per-part-the-lesson",
	} {
		if got := headingSlug(in); got != want {
			t.Errorf("headingSlug(%q) = %q, want %q", in, got, want)
		}
	}
	hs := docHeadings("# T\n## A\n```\n## not a heading\n```\n## A\n")
	if len(hs) != 3 || hs[1].slug != "a" || hs[2].slug != "a-1" {
		t.Errorf("duplicate headings: %+v", hs)
	}
}

func TestRenderMarkdown(t *testing.T) {
	src := "# Title\n\n## One\n\n## Two\n\n" +
		"See [setup](setup.md), [codes](running.md#exit-codes), [first](#one), [Two](#two), [web](https://example.com) and [dev](development.md).\n\n" +
		"Code `[x](y.md)` stays, **bold** and *italic* go, `a*b*c` and 2 * 3 * 4 stay.\n\n" +
		"| Key | Meaning |\n| --- | --- |\n| `a` | `x\\|y` |\n\n" +
		"- an item that\n  continues\n  - nested\n\n" +
		"```toml\n# see docs/POOL.md\n```\n"
	got := renderMarkdown("x.md", src, 400)
	for _, want := range []string{
		"Title\n=====\n\nSections:\n  One\n  Two\n",
		"See setup (spotexec help setup), codes (spotexec help running, section \"Exit codes\"), first (section \"One\"), Two (a section of this page), web (https://example.com) and dev (docs/development.md in the spotexec source repository).",
		"Code `[x](y.md)` stays, bold and italic go, `a*b*c` and 2 * 3 * 4 stay.",
		"Key  Meaning\n---  -------\n`a`  `x|y`\n",
		"- an item that continues\n  - nested\n",
		"    # see spotexec help pool\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered page lacks %q:\n%s", want, got)
		}
	}
}

// Prose wraps at the width, a word too long for a line gets its own, and a
// table too wide for the width becomes one block per row.
func TestRenderMarkdownWraps(t *testing.T) {
	long := strings.Repeat("word ", 30)
	got := renderMarkdown("x.md", "# T\n\n"+long+"\n\n- "+long+"\n\n| K | Meaning |\n| - | - |\n| `k` | "+long+"|\n", 40)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 40 {
			t.Errorf("line over 40 columns: %q", line)
		}
	}
	if !strings.Contains(got, "\n  word word") || !strings.Contains(got, "K · Meaning\n\n`k`\n    word word") {
		t.Errorf("unexpected layout:\n%s", got)
	}
	if w := wrapText("a "+strings.Repeat("x", 50)+" b", 20, "", ""); len(w) != 3 || w[1] != strings.Repeat("x", 50) {
		t.Errorf("wrapText with a long word: %q", w)
	}
}
