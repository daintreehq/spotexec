package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
)

// defaultDockerfile is where `container init` writes the project image.
const defaultDockerfile = ".spotexec/Dockerfile"

func cmdContainer() *cobra.Command {
	c := &cobra.Command{Use: "container", Short: "Project container image: scaffold a Dockerfile, build it on a runner, and see which regions hold it"}

	var force, yes bool
	var dfPath string
	initC := &cobra.Command{
		Use:   "init",
		Short: "Write a Dockerfile for this project and point .spotexec.toml at it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, _ := os.Getwd()
			ws, err := client.Resolve(wd)
			if err != nil {
				return err
			}
			if err := checkProjectDir(ws, yes); err != nil {
				return err
			}
			return containerInit(ws, dfPath, force)
		},
	}
	initC.Flags().BoolVar(&force, "force", false, "overwrite an existing Dockerfile")
	initC.Flags().BoolVar(&yes, "yes", false, "mark a directory that isn't a Git repository without asking")
	initC.Flags().StringVar(&dfPath, "path", defaultDockerfile, "repo-relative path of the Dockerfile to write")

	var f runFlags
	build := &cobra.Command{
		Use:   "build [flags]",
		Short: "Build (or pull) the project image on a runner now, even if its last build failed; on AWS it is pushed to ECR for every VM",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(f.dir)
			if err != nil {
				return err
			}
			if !l.cfg.Project.Container.Enabled() {
				return fmt.Errorf("no [container] in %s; run `spotexec container init` first", filepath.Join(l.ws.Root, ".spotexec.toml"))
			}
			f.noDeps = true
			return exitWith(f.execute(l, runSpec{display: "(container build)", syncOnly: true, sync: "in", waitPush: true}))
		},
	}
	f.register(build.Flags())
	c.AddCommand(initC, build, cmdContainerRegions())
	return c
}

// containerInit writes the Dockerfile and wires it into .spotexec.toml.
func containerInit(ws *client.Workspace, rel string, force bool) error {
	rel = strings.TrimPrefix(path.Clean(filepath.ToSlash(rel)), "./")
	if rel == "." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
		return fmt.Errorf("--path must be inside the repository")
	}
	cfgPath := filepath.Join(ws.Root, ".spotexec.toml")
	// Don't add a version-1 table to a config in another format, or write a
	// Dockerfile for it.
	if _, err := os.Stat(cfgPath); err == nil {
		if err := config.CheckVersion(cfgPath); err != nil {
			return err
		}
	}
	abs := filepath.Join(ws.Root, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); err == nil {
		if !force {
			return fmt.Errorf("%s exists (use --force to overwrite)", rel)
		}
		// Replace, never write through, whatever is there (a symlink).
		if err := os.Remove(abs); err != nil {
			return err
		}
	}
	body, stack, missing := scaffoldDockerfile(ws.Root)
	if stack == "" {
		stack = "no language toolchain detected: a generic Ubuntu image"
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s)\n", rel, stack)
	if len(missing) > 0 {
		fmt.Printf("note: this repository also uses %s; add it to %s if your commands need it\n", strings.Join(missing, " and "), rel)
	}

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		b, notes := detectProject(ws.Root)
		if err := os.WriteFile(cfgPath, []byte(b), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote .spotexec.toml")
		for _, n := range notes {
			fmt.Println("  " + n)
		}
	}
	msg, err := wireContainer(cfgPath, rel)
	if err != nil {
		return err
	}
	fmt.Println(msg)

	// The configured Dockerfile and its build context sync whether or not
	// Git tracks them; committing it shares the image with the team.
	if ws.Git {
		fmt.Printf("commit %s with the project so everyone runs in the same image\n", rel)
	}
	fmt.Println("every command now runs in this image: the first `sx` builds it (on AWS, once, then pushed to ECR for every VM); `spotexec container build` builds it now. Edit the Dockerfile to add system packages")
	return nil
}

// wireContainer points [container] at the Dockerfile. It appends the table
// when there is none and otherwise leaves the file alone (it may hold
// comments and choices we must not rewrite), saying what to change.
func wireContainer(cfgPath, rel string) (string, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", err
	}
	var probe struct {
		Container map[string]any `toml:"container"`
	}
	md, err := toml.Decode(string(data), &probe)
	if err != nil {
		return "", fmt.Errorf("%s: %w", cfgPath, err)
	}
	if !md.IsDefined("container") {
		add := "\n# Every command and the [deps] install run in this image (spotexec container build).\n[container]\ndockerfile = " + fmt.Sprintf("%q", rel) + "\n"
		if len(data) > 0 && data[len(data)-1] != '\n' {
			add = "\n" + add
		}
		f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString(add); err != nil {
			return "", err
		}
		return fmt.Sprintf("added [container] dockerfile = %q to .spotexec.toml", rel), nil
	}
	if d, _ := probe.Container["dockerfile"].(string); d == rel {
		if _, hasImage := probe.Container["image"]; !hasImage {
			return ".spotexec.toml already uses it", nil
		}
	}
	return fmt.Sprintf(".spotexec.toml already has a [container] table: set dockerfile = %q there (and remove image =) to use it", rel), nil
}

var (
	firstNum    = regexp.MustCompile(`\d+`)
	legacyJava  = regexp.MustCompile(`^1\.(\d+)`)
	exactVer    = regexp.MustCompile(`^\d+(\.\d+){0,2}$`)
	goDirective = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*(?://.*)?$`)
	goToolchain = regexp.MustCompile(`(?m)^toolchain\s+go(\d+\.\d+(?:\.\d+)?)\s*(?://.*)?$`)
	rustChan    = regexp.MustCompile(`(?m)^\s*channel\s*=\s*"(\d+\.\d+(?:\.\d+)?)"`)
	pmSpec      = regexp.MustCompile(`^(pnpm|yarn|npm|bun)@(\d+\.\d+\.\d+[0-9A-Za-z.\-]*)`)
	bareVer     = regexp.MustCompile(`^\s*(\d+\.\d+(?:\.\d+)?)\s*$`)
	pyMinimum   = regexp.MustCompile(`(?:>=|==|~=)\s*(\d+\.\d+)`)
	pyExact     = regexp.MustCompile(`==\s*(\d+\.\d+\.\d+)(?:[^.*\d]|$)`)
	uvRequired  = regexp.MustCompile(`(?m)^\s*required-version\s*=\s*"==\s*(\d+\.\d+\.\d+)"`)
	pnpmLock    = regexp.MustCompile(`(?m)^lockfileVersion:\s*'?(\d+)`)
	javaProp    = regexp.MustCompile(`<(?:maven\.compiler\.release|maven\.compiler\.source|java\.version|release)>\s*(?:1\.)?(\d+)\s*<`)
	gradleJava  = regexp.MustCompile(`(?:JavaLanguageVersion\.of\(\s*|JavaVersion\.VERSION_(?:1_)?|sourceCompatibility\s*=\s*['"]?(?:1\.)?)(\d+)`)
)

// Defaults when the repository pins nothing: current LTS lines.
const (
	defaultNode   = "24"
	defaultPython = "3.12"
	defaultJava   = "21"
)

// ltsNodes maps nvm's lts/<codename> to its major version.
var ltsNodes = map[string]string{
	"argon": "4", "boron": "6", "carbon": "8", "dubnium": "10", "erbium": "12", "fermium": "14",
	"gallium": "16", "hydrogen": "18", "iron": "20", "jod": "22", "krypton": "24",
}

// trixieFrom says where each official image starts publishing Debian 13
// (trixie) variants: every release of line all and later, plus, on older
// lines, every release from the one given. A line is the major for node and
// major.minor for the others. Lines older than all and not listed only ever
// got bookworm (e.g. node 18, 21 and 23, python 3.8, golang 1.23, rust 1.87).
var trixieFrom = map[string]struct {
	all   string
	lines map[string]string
}{
	"node":   {"25", map[string]string{"20": "20.19.4", "22": "22.18.0", "24": "24.5.0"}},
	"python": {"3.14", map[string]string{"3.9": "3.9.23", "3.10": "3.10.18", "3.11": "3.11.13", "3.12": "3.12.11", "3.13": "3.13.6"}},
	"golang": {"1.25", map[string]string{"1.24": "1.24.6"}},
	"rust":   {"1.88", nil},
}

// debianTag is image's tag for version on Debian trixie when the official
// image publishes one for it, else on bookworm. A partial version (20, 3.12)
// is the newest release with that prefix, and a name (lts, current) or a
// version shorter than a line (golang's and rust's "1") follows the newest
// line, which has trixie. version comes from the scaffold's own parsers, so
// the only names it can be are node's lts and current.
func debianTag(image, version string) string {
	if hasTrixie(image, version) {
		return version + "-trixie"
	}
	return version + "-bookworm"
}

func hasTrixie(image, version string) bool {
	t, ok := trixieFrom[image]
	if !ok {
		return false
	}
	if !exactVer.MatchString(version) {
		return true
	}
	n := 2
	if image == "node" {
		n = 1
	}
	parts := strings.Split(version, ".")
	if len(parts) < n {
		return true
	}
	line := strings.Join(parts[:n], ".")
	if versionAtLeast(line, t.all) {
		return true
	}
	first, ok := t.lines[line]
	if !ok {
		return false
	}
	// Compare only as many parts as version names: 22.18 floats to 22.18.x,
	// whose newest release is on or past 22.18.0.
	fp := strings.Split(first, ".")
	if len(fp) > len(parts) {
		fp = fp[:len(parts)]
	}
	return versionAtLeast(version, strings.Join(fp, "."))
}

// versionAtLeast compares dotted numeric versions part by part (a missing
// part counts as 0).
func versionAtLeast(v, min string) bool {
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			y, _ = strconv.Atoi(b[i])
		}
		if x != y {
			return x > y
		}
	}
	return true
}

func readSmall(root, name string) string {
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || len(b) > 1<<20 {
		return ""
	}
	return string(b)
}

// versionLine is the first non-blank, non-comment line of s.
func versionLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

// toolVersion reads tool's version from an asdf/mise .tool-versions file.
func toolVersion(root string, names ...string) string {
	for _, l := range strings.Split(readSmall(root, ".tool-versions"), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		for _, n := range names {
			if f[0] == n {
				return f[1]
			}
		}
	}
	return ""
}

// nodeSpec turns a version file's contents into a node image tag: an exact
// version (20.11.1), a major (20), or lts/current.
func nodeSpec(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	l := strings.ToLower(s)
	switch {
	case exactVer.MatchString(s):
		return s
	case l == "lts/*" || l == "lts":
		return "lts"
	case strings.HasPrefix(l, "lts/"):
		if v, ok := ltsNodes[l[4:]]; ok {
			return v
		}
		return "lts"
	case l == "node" || l == "current" || l == "latest" || l == "stable":
		return "current"
	}
	return ""
}

// nodeVersion picks the Node image version as precisely as the repository
// pins it: .node-version, .nvmrc, .tool-versions, package.json volta.node
// (exact versions where given), then the major of engines.node.
func nodeVersion(root string, pkg map[string]any) string {
	for _, s := range []string{
		versionLine(readSmall(root, ".node-version")),
		versionLine(readSmall(root, ".nvmrc")),
		toolVersion(root, "nodejs", "node"),
	} {
		if v := nodeSpec(s); v != "" {
			return v
		}
	}
	if volta, ok := pkg["volta"].(map[string]any); ok {
		if n, ok := volta["node"].(string); ok && exactVer.MatchString(n) {
			return n
		}
	}
	if eng, ok := pkg["engines"].(map[string]any); ok {
		if n, ok := eng["node"].(string); ok {
			n = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n), "="))
			if exactVer.MatchString(strings.TrimPrefix(n, "v")) {
				return strings.TrimPrefix(n, "v")
			}
			if v := firstNum.FindString(n); v != "" {
				return v
			}
		}
	}
	return defaultNode
}

// packageManager returns the name and exact version from package.json's
// "packageManager" (dropping any +sha suffix).
func packageManager(pkg map[string]any) (string, string) {
	s, _ := pkg["packageManager"].(string)
	m := pmSpec.FindStringSubmatch(s)
	if m == nil {
		return "", ""
	}
	v, _, _ := strings.Cut(m[2], "+")
	return m[1], v
}

// jsPackageManager returns the project's JavaScript package manager and its
// pinned version: package.json "packageManager" when it names one, else the
// lockfile (pnpm, Bun, Yarn, npm, in that order). detectProject and the
// Dockerfile use the same answer.
func jsPackageManager(root string, pkg map[string]any) (string, string) {
	if n, v := packageManager(pkg); n != "" {
		return n, v
	}
	switch {
	case exists(root, "pnpm-lock.yaml"):
		return "pnpm", ""
	case exists(root, "bun.lock") || exists(root, "bun.lockb"):
		return "bun", ""
	case exists(root, "yarn.lock"):
		return "yarn", ""
	case exists(root, "package-lock.json"):
		return "npm", ""
	}
	return "", ""
}

func readPackageJSON(root string) map[string]any {
	var pkg map[string]any
	if s := readSmall(root, "package.json"); s != "" {
		_ = json.Unmarshal([]byte(s), &pkg)
	}
	return pkg
}

// goVersion reads go.mod's toolchain line, else its go line.
func goVersion(root string) string {
	gomod := readSmall(root, "go.mod")
	if m := goToolchain.FindStringSubmatch(gomod); m != nil {
		return m[1]
	}
	if m := goDirective.FindStringSubmatch(gomod); m != nil {
		return m[1]
	}
	return "1"
}

// uvVersion is uv's version from [tool.uv] required-version when it names
// exactly one, else latest.
func uvVersion(root string) string {
	if m := uvRequired.FindStringSubmatch(readSmall(root, "pyproject.toml")); m != nil {
		return m[1]
	}
	return "latest"
}

// pythonVersion picks the Python version: .python-version or
// .tool-versions (exact where given), else the lower bound of
// requires-python, else 3.12.
func pythonVersion(root string) string {
	for _, s := range []string{versionLine(readSmall(root, ".python-version")), toolVersion(root, "python")} {
		if exactVer.MatchString(s) && strings.Contains(s, ".") {
			return s
		}
	}
	for _, l := range strings.Split(readSmall(root, "pyproject.toml"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "requires-python") {
			if m := pyExact.FindStringSubmatch(l); m != nil {
				return m[1]
			}
			if m := pyMinimum.FindStringSubmatch(l); m != nil {
				return m[1]
			}
		}
	}
	return defaultPython
}

// javaVersion picks the JDK major from .java-version, .sdkmanrc,
// .tool-versions, or the build file's release/toolchain setting.
func javaVersion(root string) string {
	for _, s := range []string{
		versionLine(readSmall(root, ".java-version")),
		strings.TrimPrefix(versionLine(readSmall(root, ".sdkmanrc")), "java="),
		toolVersion(root, "java"),
	} {
		s = legacyJava.ReplaceAllString(strings.TrimPrefix(strings.TrimSpace(s), "temurin-"), "$1") // 1.8 is Java 8
		if v := firstNum.FindString(s); v != "" && strings.HasPrefix(s, v) {
			return v
		}
	}
	if m := javaProp.FindStringSubmatch(readSmall(root, "pom.xml")); m != nil {
		return m[1]
	}
	for _, f := range []string{"build.gradle.kts", "build.gradle"} {
		if m := gradleJava.FindStringSubmatch(readSmall(root, f)); m != nil {
			return m[1]
		}
	}
	return defaultJava
}

// corepackInstall installs a pinned package manager system-wide through
// Corepack (the image's HOME is replaced at run time, so it can't live there).
func corepackInstall(name, version string) string {
	return fmt.Sprintf(`
# %[1]s %[2]s, pinned from package.json "packageManager", installed system-wide
# with Corepack so no run has to download it.
ENV COREPACK_HOME=/usr/local/share/corepack \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN (command -v corepack >/dev/null || npm install -g corepack) \
 && rm -f /usr/local/bin/yarn /usr/local/bin/yarnpkg \
 && corepack enable %[1]s \
 && corepack install -g %[1]s@%[2]s \
 && chmod -R a+rwX "$COREPACK_HOME"
`, name, version)
}

// utf8Locale follows the FROM line of every Debian or Ubuntu scaffold.
const utf8Locale = `
# A UTF-8 locale, as GitHub's Linux runners use (without it, commands run
# under POSIX with an ASCII charset).
ENV LANG=C.UTF-8
`

// scaffoldDockerfile proposes a Dockerfile for the project in root, pinned
// as precisely as the repository allows, and names the stack it detected
// ("" when it found no language toolchain). The image is built on the
// first toolchain found (JavaScript, Python, Go, Rust, Java); Go and uv
// are added to it when the repository uses them too, and any other
// toolchain it also uses is listed in missing, to be added by hand.
func scaffoldDockerfile(root string) (body, stack string, missing []string) {
	pkg := readPackageJSON(root)
	pm, pmVer := jsPackageManager(root, pkg)
	// The image is from, then one apt layer of runnerTools plus the stack's
	// own system packages, then body.
	primary, from := "", ""
	var pkgs []string
	switch {
	case pm == "bun":
		v := "1"
		if pm == "bun" && pmVer != "" {
			v = pmVer
		} else if b := strings.TrimPrefix(versionLine(readSmall(root, ".bun-version")), "v"); exactVer.MatchString(b) {
			v = b
		} else if b := toolVersion(root, "bun"); exactVer.MatchString(b) {
			v = b
		}
		stack, primary = "Bun "+v, "js"
		from = "oven/bun:" + v
	case pm != "" || pkg != nil:
		node := nodeVersion(root, pkg)
		stack, primary = "Node.js "+node, "js"
		from = "node:" + debianTag("node", node)
		switch {
		case pm == "pnpm":
			v := pmVer
			if v == "" {
				// The lockfile format says which pnpm major wrote it.
				v = map[string]string{"9": "10", "6": "8", "5": "7"}[firstNum.FindString(firstMatch(pnpmLock, readSmall(root, "pnpm-lock.yaml")))]
				if v == "" {
					v = "latest"
				}
			}
			stack += ", pnpm " + v
			body += fmt.Sprintf(`
# pnpm, pinned from package.json "packageManager" (or the lockfile's format).
RUN npm install -g pnpm@%s && npm cache clean --force
`, v)
		case pm == "yarn" && pmVer != "":
			stack += ", Yarn " + pmVer
			body += corepackInstall("yarn", pmVer)
		case pm == "yarn" && pmVer == "":
			stack += ", Yarn"
			body += `
# Yarn 1. Yarn 2+ projects usually commit their release (.yarn/releases,
# yarnPath in .yarnrc.yml), which Yarn 1 hands off to; or set
# "packageManager" in package.json and run ` + "`spotexec container init --force`" + `.
RUN npm install -g --force yarn@1 && npm cache clean --force
`
		case pm == "npm" && pmVer != "":
			stack += ", npm " + pmVer
			body += fmt.Sprintf(`
# npm, pinned from package.json "packageManager".
RUN npm install -g npm@%s && npm cache clean --force
`, pmVer)
		}
	case exists(root, "uv.lock"):
		py, uv := pythonVersion(root), uvVersion(root)
		stack, primary = "Python "+py+", uv", "python"
		from = "python:" + debianTag("python", py)
		body = fmt.Sprintf(`
# uv (pinned from [tool.uv] required-version when it names one exact version).
COPY --from=ghcr.io/astral-sh/uv:%s /uv /uvx /usr/local/bin/

# Use the image's Python; copy (not hardlink) out of uv's cache.
ENV UV_PYTHON_DOWNLOADS=never \
    UV_LINK_MODE=copy
`, uv)
	case exists(root, "pyproject.toml") || exists(root, "requirements.txt") || exists(root, "setup.py"):
		py := pythonVersion(root)
		stack, primary = "Python "+py, "python"
		from = "python:" + debianTag("python", py)
		body = `
# Commands run as a non-root user, so install packages into a virtualenv in
# the workspace (and list it in [deps] cache_dirs), or bake them in here:
# RUN pip install --no-cache-dir pytest
`
	case exists(root, "go.mod"):
		gv := goVersion(root)
		stack, primary = "Go "+gv, "go"
		from = "golang:" + debianTag("golang", gv)
		body = `
# Keep the module and build caches in the slot's persistent HOME, so they
# survive between runs (the container itself is thrown away each time).
ENV GOMODCACHE=/home/spotexec/go/pkg/mod \
    GOCACHE=/home/spotexec/.cache/go-build
`
	case exists(root, "Cargo.toml"):
		rv := "1"
		for _, f := range []string{"rust-toolchain.toml", "rust-toolchain"} {
			s := readSmall(root, f)
			if m := rustChan.FindStringSubmatch(s); m != nil {
				rv = m[1]
				break
			}
			if m := bareVer.FindStringSubmatch(s); m != nil {
				rv = m[1]
				break
			}
		}
		stack, primary = "Rust "+rv, "rust"
		from = "rust:" + debianTag("rust", rv)
		body = `
# Components are installed as root: keep the toolchain writable for the
# non-root user, as the base image does. Install extra tools system-wide:
# RUN cargo install --root /usr/local/cargo cargo-nextest
RUN rustup component add clippy rustfmt && chmod -R a+w "$RUSTUP_HOME" "$CARGO_HOME"

# At run time, keep the registry and git checkouts in the slot's persistent
# HOME, so they survive between runs (the container itself is thrown away).
ENV CARGO_HOME=/home/spotexec/.cargo \
    PATH=/home/spotexec/.cargo/bin:$PATH
`
	case exists(root, "pom.xml") || exists(root, "build.gradle") || exists(root, "build.gradle.kts"):
		jv := javaVersion(root)
		stack, primary = "Java "+jv, "java"
		from = "eclipse-temurin:" + jv + "-jdk"
		if exists(root, "pom.xml") && !exists(root, "mvnw") {
			pkgs = append(pkgs, "maven")
			stack += ", Maven"
		} else if exists(root, "pom.xml") {
			stack += ", Maven wrapper"
		} else {
			stack += ", Gradle wrapper"
		}
		body = `
# ./mvnw and ./gradlew download into HOME (persistent per slot).
`
	default:
		from, pkgs = "ubuntu:24.04", []string{"build-essential"}
	}
	// A UTF-8 locale, as on GitHub's Linux runners, straight after FROM. Most
	// of these base images leave LANG unset, so commands would run under
	// POSIX with an ASCII charset; C.UTF-8 ships with glibc, so no locales
	// package is needed. eclipse-temurin already sets LANG and LC_ALL to
	// en_US.UTF-8.
	locale := ""
	if primary != "java" {
		locale = utf8Locale
	}
	body = "FROM " + from + "\n" + locale + aptLayer(pkgs) + body
	// Other toolchains the repository uses (its tasks may run them).
	if primary != "" {
		if primary != "go" && exists(root, "go.mod") {
			gv := goVersion(root)
			stack += " + Go " + gv
			body += fmt.Sprintf(`
# Go %[1]s too (go.mod), with its caches in the slot's persistent HOME.
COPY --from=golang:%[2]s /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH \
    GOMODCACHE=/home/spotexec/go/pkg/mod \
    GOCACHE=/home/spotexec/.cache/go-build
`, gv, debianTag("golang", gv))
		}
		if primary != "python" && exists(root, "uv.lock") {
			py := pythonVersion(root)
			stack += " + uv, Python " + py
			body += fmt.Sprintf(`
# uv too (uv.lock), and its Python installed in the image rather than in
# HOME (which is per slot), so a cached .venv works in every slot.
COPY --from=ghcr.io/astral-sh/uv:%s /uv /uvx /usr/local/bin/
ENV UV_PYTHON_INSTALL_DIR=/usr/local/share/uv/python \
    UV_LINK_MODE=copy
RUN uv python install %s && chmod -R a+rX "$UV_PYTHON_INSTALL_DIR"
ENV UV_PYTHON_DOWNLOADS=never
`, uvVersion(root), py)
		}
		for _, o := range []struct {
			name, kind string
			present    bool
		}{
			{"Node.js", "js", pkg != nil || pm != ""},
			{"Python", "python", !exists(root, "uv.lock") && (exists(root, "pyproject.toml") || exists(root, "requirements.txt"))},
			{"Rust", "rust", exists(root, "Cargo.toml")},
			{"Java", "java", exists(root, "pom.xml") || exists(root, "build.gradle") || exists(root, "build.gradle.kts")},
		} {
			if o.present && o.kind != primary {
				missing = append(missing, o.name)
			}
		}
		if len(missing) > 0 {
			body += "\n# This repository also uses " + strings.Join(missing, " and ") + ": install it here if your commands need it.\n"
		}
	}
	label := stack
	if label == "" {
		label = "generic"
	}
	header := `# Project container for spotexec (generated by ` + "`spotexec container init`" + ` for a
# ` + label + ` project). Every ` + "`sx`" + ` command, and the [deps] install, runs inside
# this image on the runner. Edit it freely, then build it with:
#
#   spotexec container build -r <runner>
#
# On AWS the built image is pushed to a private ECR repository, so every
# other VM pulls it instead of building it again.
#
# How the image is used:
# - Your source is NOT copied in: the worktree is mounted at the same path
#   at run time. Install tools here, not code; dependencies (node_modules,
#   .venv, ...) come from the [deps] cache.
# - Commands run as the runner's non-root user, with HOME=/home/spotexec (a
#   per-slot directory mounted at run time that persists between runs).
#   Install toolchains system-wide (e.g. /usr/local), never under /root or
#   /home/spotexec: anything built there is hidden by the mount.
# - The build context is this file's directory. The image is rebuilt only
#   when this file (or the context) changes, and needs /bin/sh (bash is used
#   when present).

`
	extra := `
# Extra system packages your commands need, for example:
# RUN apt-get update \
#  && apt-get install -y --no-install-recommends postgresql-client \
#  && rm -rf /var/lib/apt/lists/*
`
	return header + body + extra, stack, missing
}

// runnerTools are command-line tools GitHub's hosted Ubuntu runners ship that
// test suites shell out to or probe for. Every scaffold installs them, so a
// suite that passes in GitHub Actions runs the same tests under sx, rather
// than skipping the ones that look for a tool or failing with "command not
// found". The package names are the same on Debian and Ubuntu.
var runnerTools = []string{
	"bzip2", "ca-certificates", "curl", "file", "git", "jq", "lsof", "make",
	"openssh-client", "procps", "rsync", "sqlite3", "unzip", "wget", "xz-utils",
	"zip", "zsh",
}

// aptLayer installs runnerTools and the stack's own packages in one layer.
func aptLayer(extra []string) string {
	pkgs := append(append([]string{}, extra...), runnerTools...)
	sort.Strings(pkgs)
	var b strings.Builder
	b.WriteString(`
# Command-line tools GitHub's Ubuntu runners provide, which test suites often
# call or probe for (a test that checks for a missing tool skips silently).
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
`)
	line := "     "
	for _, p := range pkgs {
		if len(line)+1+len(p) > 76 {
			b.WriteString(line + " \\\n")
			line = "     "
		}
		line += " " + p
	}
	b.WriteString(line + " \\\n && rm -rf /var/lib/apt/lists/*\n")
	return b.String()
}

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
