package cli

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A browserDep is a package.json dependency whose binary links system
// libraries the node image doesn't have: Electron, and the Chromium (and
// Firefox, WebKit) builds Playwright and Puppeteer download.
type browserDep struct {
	tool     string // Electron, Playwright, Puppeteer
	name     string // the dependency that declares it
	manifest string // repo-relative package.json that declares it
}

// browserTools maps each dependency name to the tool it brings, in the
// order the tools are reported.
var browserTools = []struct{ tool, name string }{
	{"Electron", "electron"},
	{"Playwright", "@playwright/test"},
	{"Playwright", "playwright"},
	{"Puppeteer", "puppeteer"},
}

// maxWorkspaceManifests bounds how many workspace package.json files
// browserDeps reads.
const maxWorkspaceManifests = 500

// browserDeps finds the tools in browserTools that the root package.json or
// a workspace's (pnpm-workspace.yaml packages for pnpm, package.json
// workspaces otherwise) declares in dependencies, devDependencies or
// optionalDependencies: the first declaration of each tool, in tool order.
// notes names workspace patterns it couldn't look in.
func browserDeps(root, pm string, pkg map[string]any) (deps []browserDep, notes []string) {
	found := map[string]browserDep{}
	look := func(rel string, pkg map[string]any) {
		for _, sec := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
			m, _ := pkg[sec].(map[string]any)
			for _, t := range browserTools {
				if _, ok := m[t.name]; ok {
					if _, seen := found[t.tool]; !seen {
						found[t.tool] = browserDep{t.tool, t.name, rel}
					}
				}
			}
		}
	}
	look("package.json", pkg)

	var pats []string
	if pm == "pnpm" {
		pats, _ = pnpmWorkspacePackages(readSmall(root, "pnpm-workspace.yaml"))
	} else {
		pats = packageJSONWorkspaces(pkg)
	}
	var excl []string
	for _, p := range pats {
		if p, ok := strings.CutPrefix(strings.TrimSpace(p), "!"); ok {
			excl = append(excl, strings.TrimRight(strings.TrimPrefix(p, "./"), "/"))
		}
	}
	nm, skipped := workspaceNodeModules(pats)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	read := 0
	seen := map[string]bool{}
scan:
	for _, d := range nm[1:] {
		matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(d, "/node_modules")), "package.json"))
		sort.Strings(matches)
		for _, m := range matches {
			rel, err := filepath.Rel(root, m)
			if err != nil || seen[rel] {
				continue
			}
			seen[rel] = true
			rel = filepath.ToSlash(rel)
			if excluded(path.Dir(rel), excl) || !inside(realRoot, m) {
				continue
			}
			if read == maxWorkspaceManifests {
				notes = append(notes, fmt.Sprintf("looked for Electron, Playwright and Puppeteer in the first %d workspace package.json files only", maxWorkspaceManifests))
				break scan
			}
			read++
			var wp map[string]any
			if s := readSmall(root, filepath.FromSlash(rel)); s != "" && json.Unmarshal([]byte(s), &wp) == nil {
				look(rel, wp)
			}
		}
	}
	if len(skipped) > 0 {
		notes = append(notes, fmt.Sprintf("didn't look for Electron, Playwright or Puppeteer in workspace pattern(s) %s (no ** or {a,b}); add the system libraries they need to the Dockerfile by hand", strings.Join(skipped, ", ")))
	}
	for _, t := range []string{"Electron", "Playwright", "Puppeteer"} {
		if d, ok := found[t]; ok {
			deps = append(deps, d)
		}
	}
	return deps, notes
}

// excluded reports whether the workspace directory dir matches a negated
// workspace pattern.
func excluded(dir string, excl []string) bool {
	for _, e := range excl {
		if ok, _ := path.Match(e, dir); ok {
			return true
		}
	}
	return false
}

// inside reports whether p, with symlinks resolved, is within root (already
// resolved): a workspace symlinked out of the repository isn't read.
func inside(root, p string) bool {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, r)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Debian 12 (bookworm) packages, for the node:<version>-bookworm image.
var (
	// Chromium's libraries, as Playwright lists them for debian12.
	chromiumPkgs = []string{
		"libasound2", "libatk-bridge2.0-0", "libatk1.0-0", "libatspi2.0-0", "libcairo2", "libcups2",
		"libdbus-1-3", "libdrm2", "libgbm1", "libglib2.0-0", "libnspr4", "libnss3", "libpango-1.0-0",
		"libx11-6", "libxcb1", "libxcomposite1", "libxdamage1", "libxext6", "libxfixes3",
		"libxkbcommon0", "libxrandr2",
	}
	// What Puppeteer's troubleshooting guide adds for Chrome, and Electron
	// links: GTK and the rest of X.
	chromePkgs = []string{
		"fonts-liberation", "libexpat1", "libfontconfig1", "libgtk-3-0", "libpangocairo-1.0-0",
		"libx11-xcb1", "libxcursor1", "libxi6", "libxrender1", "libxss1", "libxtst6", "xdg-utils",
	}
	// Electron also links libsecret and libxshmfence, and a window needs a
	// display: xvfb-run.
	electronPkgs = []string{"libsecret-1-0", "libxshmfence1", "xauth", "xvfb"}
	// The rest of `playwright install-deps` for debian12 (Playwright's
	// nativeDeps.ts): its fonts and tools, Firefox and WebKit.
	playwrightPkgs = []string{
		// tools
		"xvfb", "fonts-noto-color-emoji", "fonts-unifont", "libfontconfig1", "libfreetype6",
		"xfonts-scalable", "fonts-liberation", "fonts-ipafont-gothic", "fonts-wqy-zenhei",
		"fonts-tlwg-loma-otf", "fonts-freefont-ttf",
		// firefox
		"libavcodec59", "libcairo-gobject2", "libdbus-glib-1-2", "libgdk-pixbuf-2.0-0", "libgtk-3-0",
		"libharfbuzz0b", "libpangocairo-1.0-0", "libx11-xcb1", "libxcb-shm0", "libxcursor1", "libxi6",
		"libxrender1", "libxtst6",
		// webkit
		"libsoup-3.0-0", "gstreamer1.0-libav", "gstreamer1.0-plugins-bad", "gstreamer1.0-plugins-base",
		"gstreamer1.0-plugins-good", "libegl1", "libenchant-2-2", "libepoxy0", "libevdev2", "libgles2",
		"libglx0", "libgstreamer-gl1.0-0", "libgstreamer-plugins-base1.0-0", "libgstreamer1.0-0",
		"libgtk-4-1", "libgudev-1.0-0", "libharfbuzz-icu0", "libhyphen0", "libicu72", "libjpeg62-turbo",
		"liblcms2-2", "libmanette-0.2-0", "libnotify4", "libopengl0", "libopenjp2-7", "libopus0",
		"libpng16-16", "libproxy1v5", "libsecret-1-0", "libwayland-client0", "libwayland-egl1",
		"libwayland-server0", "libwebp7", "libwebpdemux2", "libwoff1", "libxml2", "libxslt1.1",
		"libatomic1", "libevent-2.1-7", "libavif15",
	}
)

// browserLibs returns the Dockerfile lines that install, on the bookworm
// node image, the system libraries deps need, and a line for the command's
// output saying what was added and why.
func browserLibs(deps []browserDep) (body, note string) {
	if len(deps) == 0 {
		return "", ""
	}
	set := map[string]bool{}
	add := func(pkgs []string) {
		for _, p := range pkgs {
			set[p] = true
		}
	}
	var tools, manifests []string
	declared := map[string][]string{}
	electron := false
	for _, d := range deps {
		tools = append(tools, d.tool)
		if declared[d.manifest] == nil {
			manifests = append(manifests, d.manifest)
		}
		declared[d.manifest] = append(declared[d.manifest], d.name)
		add(chromiumPkgs)
		switch d.tool {
		case "Electron":
			electron = true
			add(chromePkgs)
			add(electronPkgs)
		case "Playwright":
			add(playwrightPkgs)
		case "Puppeteer":
			add(chromePkgs)
		}
	}
	pkgs := make([]string, 0, len(set))
	for p := range set {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	var why []string
	for _, m := range manifests {
		why = append(why, m+" declares "+joinAnd(declared[m]))
	}
	need := joinAnd(tools) + " need"
	if len(tools) == 1 {
		need += "s"
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(commentWrap(fmt.Sprintf("System libraries %s (%s). The base image has none of them, so their binaries would fail with \"error while loading shared libraries\".",
		need, strings.Join(why, "; "))))
	for _, d := range deps {
		switch d.tool {
		case "Electron":
			b.WriteString("# Electron links them even as plain Node (ELECTRON_RUN_AS_NODE); tests that\n# open a window need a display, so run them under `xvfb-run -a`.\n")
		case "Playwright":
			b.WriteString("# Playwright's are its Debian 12 list (`playwright install-deps`) for Chromium,\n# Firefox and WebKit; the browsers themselves come from `playwright install`.\n")
		}
	}
	b.WriteString("RUN apt-get update \\\n && apt-get install -y --no-install-recommends \\\n")
	line := "     "
	for _, p := range pkgs {
		if len(line)+1+len(p) > 76 {
			b.WriteString(line + " \\\n")
			line = "     "
		}
		line += " " + p
	}
	b.WriteString(line + " \\\n && rm -rf /var/lib/apt/lists/*\n")

	note = "added the system libraries " + need + " (" + strings.Join(why, "; ") + ")"
	if electron {
		note += "; run Electron tests that open a window under `xvfb-run -a`"
	}
	return b.String(), note
}

// commentWrap wraps text as Dockerfile comment lines of at most 78 columns.
func commentWrap(text string) string {
	var b strings.Builder
	line := "#"
	for _, w := range strings.Fields(text) {
		if len(line) > 1 && len(line)+1+len(w) > 78 {
			b.WriteString(line + "\n")
			line = "#"
		}
		line += " " + w
	}
	return b.String() + line + "\n"
}

// joinAnd joins words as "a", "a and b" or "a, b and c".
func joinAnd(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}
