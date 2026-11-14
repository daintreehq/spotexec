package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/hooks"
	"github.com/daintreehq/spotexec/internal/provider"
	"github.com/daintreehq/spotexec/internal/ui"
)

const globalTemplate = `# spotexec global runner config (personal; never committed).
# Reference: https://github.com/daintreehq/spotexec/blob/main/docs/configuration.md
version = 1

# Runners are tried in priority order (lowest first). A fleet grows (up to
# max_instances) when every slot is busy, and each VM terminates itself
# after idle_timeout.
default_pool = ["aws"]

# Never silently fall back to running on this laptop.
local_fallback = false

[runners.aws]
type = "aws-spot"
region = "{{REGION}}"
# profile = "default"          # AWS CLI profile (SSO and aws login work)
instance_types = ["c7a.2xlarge", "m7a.2xlarge", "c6a.2xlarge"]  # 8 vCPUs; tried in order on capacity errors
max_slots = 4                  # concurrent jobs per VM (SPOTEXEC_CPUS = 2 each)
max_instances = 4              # fleet ceiling
priority = 20
idle_timeout = "10m"           # the VM terminates itself after this much idle time (15m suits long agent pauses)
max_lifetime = "12h"           # hard cap regardless of activity
hourly_cost = 0.12             # for 'spotexec monitor' burn estimates
# max_price = "0.40"           # USD/hour cap (default: the on-demand price)
# on_demand_fallback = true
# cache_bucket = "auto"        # shared dependency cache in S3: auto | <bucket> | off
bootstrap_script = """
apt-get update -o DPkg::Lock::Timeout=300
apt-get install -y -o DPkg::Lock::Timeout=300 git curl unzip build-essential zstd docker.io docker-buildx
usermod -aG docker ubuntu
"""
# user_bootstrap_script = """
# curl -fsSL https://mise.run | sh
# """
# remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'
#
# Then run "spotexec image bake aws" once: new VMs boot from the baked image
# and skip this bootstrap (much faster cold starts).

# A machine you already have (LAN / Tailscale), tried before the cloud:
# [runners.homelab]
# type = "ssh"
# hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]  # dialed in parallel; the first wins within 500ms
# max_slots = 4
# priority = 10
# (then: default_pool = ["homelab", "aws"])
`

func cmdInit() *cobra.Command {
	var global, force, yes, noContainer bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Generate .spotexec.toml for this repo, and a container for its toolchain (or --global runner config)",
		Long: `Generate .spotexec.toml for this repository: dependency caching and tasks
detected from its lockfiles and scripts.

When it detects a language toolchain (Node.js or Bun, Python, Go, Rust, Java),
init also writes .spotexec/Dockerfile, pinned to the versions the repository
declares (.nvmrc, packageManager, .python-version, go.mod, rust-toolchain, ...),
and points [container] at it: AWS VMs boot plain Ubuntu, and every command runs
in that image. --no-container keeps commands on the runner's own toolchain, as
does a setup whose runners are all SSH machines.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if global {
				p := config.GlobalPath()
				if _, err := os.Stat(p); err == nil && !force {
					return fmt.Errorf("%s exists (use --force to overwrite)", p)
				}
				region, why, err := writeGlobalTemplate(p, "")
				if err != nil {
					return err
				}
				fmt.Printf("wrote %s (region %s, %s)\nnext: `spotexec setup` (AWS access that doesn't expire), then `spotexec init` in a repo, then `sx <command>`\n", p, region, why)
				return nil
			}
			wd, _ := os.Getwd()
			ws, err := client.Resolve(wd)
			if err != nil {
				return err
			}
			if err := checkProjectDir(ws, yes); err != nil {
				return err
			}
			p := filepath.Join(ws.Root, ".spotexec.toml")
			if _, err := os.Stat(p); err == nil && !force {
				return fmt.Errorf("%s exists (use --force to overwrite)", p)
			}
			body, notes := detectProject(ws.Root)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
			fmt.Printf("wrote %s\n", p)
			for _, n := range notes {
				fmt.Println("  " + n)
			}
			if err := initContainer(ws, noContainer); err != nil {
				return err
			}
			if _, err := os.Stat(config.GlobalPath()); err != nil {
				fmt.Println("no runners configured yet: run `spotexec setup`")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&global, "global", false, "write an example ~/.config/spotexec/config.toml")
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	c.Flags().BoolVar(&yes, "yes", false, "mark a directory that isn't a Git repository without asking")
	c.Flags().BoolVar(&noContainer, "no-container", false, "don't set up a project container: commands use the runner's own toolchain")
	return c
}

// initContainer sets up the project container when init detects a
// toolchain: AWS VMs boot without one, and the image pins it exactly.
func initContainer(ws *client.Workspace, skip bool) error {
	_, stack, _ := scaffoldDockerfile(ws.Root)
	switch {
	case stack == "":
		fmt.Println("no language toolchain detected, so commands use the runner's own tools; `spotexec container init` sets up a container image")
		return nil
	case skip:
		fmt.Printf("%s project, no container (--no-container): commands use the runner's own toolchain, which AWS VMs only have if bootstrap_script installs it\n", stack)
		return nil
	case sshRunnersOnly():
		fmt.Printf("%s project; your runners are SSH machines, so commands use their own toolchain (`spotexec container init` runs them in a container instead)\n", stack)
		return nil
	}
	if _, err := os.Lstat(filepath.Join(ws.Root, filepath.FromSlash(defaultDockerfile))); err == nil {
		msg, err := wireContainer(filepath.Join(ws.Root, ".spotexec.toml"), defaultDockerfile)
		if err != nil {
			return err
		}
		fmt.Printf("kept the existing %s (`spotexec container init --force` rewrites it for %s)\n%s\n", defaultDockerfile, stack, msg)
		return nil
	}
	return containerInit(ws, defaultDockerfile, false)
}

// sshRunnersOnly reports whether every configured runner is an SSH
// machine, which runs commands with its own toolchain.
func sshRunnersOnly() bool {
	g, err := config.LoadGlobal()
	if err != nil || len(g.Runners) == 0 {
		return false
	}
	for _, r := range g.Runners {
		if r.Ephemeral() {
			return false
		}
	}
	return true
}

// checkProjectDir refuses to make the home directory or / a project, and
// asks before marking a directory that isn't a Git worktree: every file
// under it would sync to runners.
func checkProjectDir(ws *client.Workspace, yes bool) error {
	if config.NeverProject(ws.Root) {
		return fmt.Errorf("%s is your home directory, / or a credential directory, which spotexec never syncs (they hold ~/.aws, ~/.ssh and more): cd into the project's repository and run this again", ws.Root)
	}
	if ws.Git || ws.Marked || yes {
		return nil
	}
	if !ui.IsTerminal(os.Stdin) {
		return fmt.Errorf("%s is not in a Git repository, so every file under it would sync to runners: cd into the project's repository, or pass --yes if this directory is the project", ws.Root)
	}
	fmt.Printf("%s is not in a Git repository: every file under it (apart from [sync].exclude and built-in excludes such as node_modules/) will sync to runners.\nMake it a spotexec project? [y/N] ", ws.Root)
	var a string
	fmt.Scanln(&a)
	if a = strings.ToLower(strings.TrimSpace(a)); a != "y" && a != "yes" {
		return errors.New("nothing written: cd into the project's repository first")
	}
	return nil
}

// writeGlobalTemplate writes the example global config with the region
// detected for AWS profile (see detectRegion), and returns that region and
// where it came from.
func writeGlobalTemplate(p, profile string) (region, why string, err error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", "", err
	}
	region, why = detectRegion(profile)
	return region, why, os.WriteFile(p, []byte(strings.ReplaceAll(globalTemplate, "{{REGION}}", region)), 0o644)
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

// detectProject sniffs lockfiles and scripts to propose deps and tasks.
func detectProject(root string) (string, []string) {
	var b strings.Builder
	var notes []string
	b.WriteString("# spotexec project config (commit this). Runners live in ~/.config/spotexec/config.toml.\n")
	b.WriteString("version = 1\ntimeout = \"5m\"\nenv_allow = [\"NODE_OPTIONS\", \"TZ\"]\n\n")
	b.WriteString("[sync]\nmode = \"auto\"          # write back files the command changed (prettier --write, vitest -u, codegen)\nwriteback = \"always\"\nexclude = [\".env*\", \"*.pem\", \"*.key\"]\npull_artifacts = []     # e.g. [\"coverage/\"]\n\n")

	// The same package manager the project container installs.
	pkg := readPackageJSON(root)
	pm, pmVer := jsPackageManager(root, pkg)
	jsDeps := func() string {
		dirs, n := jsCacheDirs(root, pm, pkg)
		notes = append(notes, n...)
		return tomlStrings(dirs)
	}
	switch {
	case pm == "pnpm" && exists(root, "pnpm-lock.yaml"):
		fmt.Fprintf(&b, "[deps]\nlockfiles = [\"pnpm-lock.yaml\"]\ninstall = \"pnpm install --frozen-lockfile\"\ncache_dirs = %s\n\n", jsDeps())
	case pm == "bun" && (exists(root, "bun.lock") || exists(root, "bun.lockb")):
		lf := "bun.lock"
		if !exists(root, lf) {
			lf = "bun.lockb"
		}
		fmt.Fprintf(&b, "[deps]\nlockfiles = [%q]\ninstall = \"bun install --frozen-lockfile\"\ncache_dirs = %s\n\n", lf, jsDeps())
	case pm == "yarn" && exists(root, "yarn.lock"):
		inst := "yarn install --frozen-lockfile"
		if exists(root, ".yarnrc.yml") || (pmVer != "" && !strings.HasPrefix(pmVer, "1.")) {
			inst = "yarn install --immutable"
		}
		fmt.Fprintf(&b, "[deps]\nlockfiles = [\"yarn.lock\"]\ninstall = %q\ncache_dirs = %s\n\n", inst, jsDeps())
	case pm == "npm" && exists(root, "package-lock.json"):
		fmt.Fprintf(&b, "[deps]\nlockfiles = [\"package-lock.json\"]\ninstall = \"npm ci\"\ncache_dirs = %s\n\n", jsDeps())
	case exists(root, "uv.lock"):
		b.WriteString("[deps]\nlockfiles = [\"uv.lock\"]\ninstall = \"uv sync --frozen\"\ncache_dirs = [\".venv\"]\n\n")
	}

	tasks := map[string]string{}
	var order []string
	add := func(name, run string) {
		if _, ok := tasks[name]; !ok {
			tasks[name] = run
			order = append(order, name)
		}
	}
	if pm != "" {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
			_ = json.Unmarshal(data, &pkg)
		}
		run := pm + " run "
		for _, s := range []string{"test", "lint", "typecheck", "type-check", "format", "build"} {
			if _, ok := pkg.Scripts[s]; ok {
				name := s
				if s == "type-check" {
					name = "typecheck"
				}
				if s == "format" {
					name = "fmt"
				}
				if pm == "npm" {
					add(name, run+s+" -- {args}") // npm run needs -- before script flags
				} else {
					add(name, run+s+" {args}")
				}
			}
		}
		if _, ok := tasks["typecheck"]; !ok && exists(root, "tsconfig.json") {
			add("typecheck", map[string]string{
				"npm": "npx tsc --noEmit", "pnpm": "pnpm exec tsc --noEmit",
				"yarn": "yarn tsc --noEmit", "bun": "bunx tsc --noEmit",
			}[pm])
		}
	}
	switch {
	case exists(root, "uv.lock"):
		add("test", "uv run pytest {args}")
	case exists(root, "pyproject.toml") || exists(root, "pytest.ini"):
		add("test", "pytest {args}")
	case exists(root, "Cargo.toml"):
		add("test", "cargo test {args}")
		add("lint", "cargo clippy --all-targets {args}")
		notes = append(notes, "Rust: target/ is per-slot build output and is never synced back")
	case exists(root, "go.mod"):
		add("test", "go test ./... {args}")
		add("vet", "go vet ./...")
	}
	for _, n := range order {
		fmt.Fprintf(&b, "[tasks.%s]\nrun = %q\n\n", n, tasks[n])
	}
	var par []string
	for _, n := range []string{"typecheck", "lint", "test"} {
		if _, ok := tasks[n]; ok {
			par = append(par, fmt.Sprintf("%q", n))
		}
	}
	if len(par) >= 2 {
		fmt.Fprintf(&b, "[tasks.verify]\nparallel = [%s]\nfail_fast = false\n\n", strings.Join(par, ", "))
	}
	b.WriteString("# Agent hook tuning (spotexec hook install --claude)\n[hook]\n# when_load_above = 0.6   # only offload when local load/CPU exceeds this\n# patterns = []           # extra regexes to offload\n")
	if len(order) == 0 {
		notes = append(notes, "no test runner detected; add [tasks.<name>] entries as needed")
	} else {
		notes = append(notes, "tasks: "+strings.Join(order, ", ")+" (run with `sx <task>`)")
	}
	return b.String(), notes
}

// jsCacheDirs returns deps.cache_dirs for a JavaScript project: the root
// node_modules, then each workspace's, from pnpm-workspace.yaml "packages"
// (pnpm) or package.json "workspaces" (npm, Yarn, Bun). Without them a run
// restored from the cache would be missing every workspace's node_modules.
func jsCacheDirs(root, pm string, pkg map[string]any) ([]string, []string) {
	var pats []string
	var notes []string
	src := "package.json workspaces"
	if pm == "pnpm" {
		src = "pnpm-workspace.yaml packages"
		if exists(root, "pnpm-workspace.yaml") {
			var ok bool
			if pats, ok = pnpmWorkspacePackages(readSmall(root, "pnpm-workspace.yaml")); !ok {
				return []string{"node_modules", "packages/*/node_modules", "apps/*/node_modules"},
					[]string{"pnpm workspace detected, but its packages list wasn't readable: adjust deps.cache_dirs globs to your package folders"}
			}
		}
	} else {
		if pkg == nil && exists(root, "package.json") {
			notes = append(notes, "package.json didn't parse: deps.cache_dirs lists only the root node_modules; add each workspace's node_modules if it has workspaces")
		}
		pats = packageJSONWorkspaces(pkg)
	}
	dirs, skipped := workspaceNodeModules(pats)
	if len(dirs) > 1 {
		notes = append(notes, fmt.Sprintf("%s workspaces: deps.cache_dirs includes each workspace's node_modules, from %s (update it when those change)", pm, src))
	}
	if len(skipped) > 0 {
		notes = append(notes, fmt.Sprintf("deps.cache_dirs leaves out workspace pattern(s) %s: cache_dirs globs match one directory per * (no ** or {a,b}) inside the repository; add those workspaces' node_modules by hand", strings.Join(skipped, ", ")))
	}
	return dirs, notes
}

// packageJSONWorkspaces reads "workspaces" as an array (npm, Yarn, Bun) or
// an object with a "packages" array (Yarn classic, Bun).
func packageJSONWorkspaces(pkg map[string]any) []string {
	w := pkg["workspaces"]
	if o, ok := w.(map[string]any); ok {
		w = o["packages"]
	}
	list, _ := w.([]any)
	var pats []string
	for _, v := range list {
		if s, ok := v.(string); ok {
			pats = append(pats, s)
		}
	}
	return pats
}

// workspaceNodeModules maps each workspace glob X to X/node_modules after
// the root node_modules, deduplicated in declaration order. Negated
// patterns only exclude, so they add nothing. skipped holds the (quoted)
// patterns cache_dirs can't express: cache_dirs are expanded with
// filepath.Glob, which has no ** or brace alternatives, and must stay
// inside the repository.
func workspaceNodeModules(pats []string) (dirs, skipped []string) {
	dirs = []string{"node_modules"}
	seen := map[string]bool{"node_modules": true}
	for _, raw := range pats {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "!") {
			continue
		}
		for strings.HasPrefix(p, "./") {
			p = strings.TrimLeft(p[2:], "/")
		}
		p = strings.TrimRight(p, "/")
		if p == "" || p == "." {
			continue // the root itself
		}
		d := p + "/node_modules"
		_, badGlob := path.Match(d, "")
		_, badPath := fsid.CleanRel(strings.NewReplacer("*", "x", "?", "x", "[", "x", "]", "x").Replace(d))
		if strings.Contains(p, "**") || strings.ContainsAny(p, "{}") || badGlob != nil || badPath != nil {
			skipped = append(skipped, strconv.Quote(raw))
			continue
		}
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs, skipped
}

// pnpmWorkspacePackages reads the top-level "packages" sequence of a
// pnpm-workspace.yaml, block or flow style, without a YAML parser. ok is
// false for anything it can't read with confidence (anchors, multi-line
// scalars); a file without "packages" is ok and declares no workspaces.
func pnpmWorkspacePackages(data string) (pats []string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	for i, l := range lines {
		rest, found := strings.CutPrefix(l, "packages:")
		if !found {
			if r, f := strings.CutPrefix(l, `"packages":`); f {
				rest, found = r, true
			} else if r, f := strings.CutPrefix(l, `'packages':`); f {
				rest, found = r, true
			}
		}
		if !found {
			continue
		}
		rest = strings.TrimSpace(stripYAMLComment(rest))
		if strings.HasPrefix(rest, "[") {
			if !strings.HasSuffix(rest, "]") {
				return nil, false
			}
			for _, item := range strings.Split(rest[1:len(rest)-1], ",") {
				if item = strings.TrimSpace(item); item == "" {
					continue
				}
				v, ok := yamlScalar(item)
				if !ok {
					return nil, false
				}
				pats = append(pats, v)
			}
			return pats, true
		}
		if rest != "" {
			return nil, false
		}
		for _, l := range lines[i+1:] {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			item, isItem := strings.CutPrefix(t, "-")
			if !isItem || (item != "" && item[0] != ' ' && item[0] != '\t') {
				break // the next key
			}
			v, ok := yamlScalar(strings.TrimSpace(item))
			if !ok {
				return nil, false
			}
			pats = append(pats, v)
		}
		return pats, true
	}
	return nil, true
}

// yamlScalar reads a single-line quoted or plain YAML scalar.
func yamlScalar(s string) (string, bool) {
	switch {
	case s == "":
		return "", false
	case s[0] == '\'':
		var v strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '\'' {
				v.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' { // '' is an escaped quote
				v.WriteByte('\'')
				i++
				continue
			}
			if strings.TrimSpace(stripYAMLComment(s[i+1:])) != "" {
				return "", false
			}
			return v.String(), true
		}
		return "", false
	case s[0] == '"':
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				v, err := strconv.Unquote(s[:i+1])
				if err != nil || strings.TrimSpace(stripYAMLComment(s[i+1:])) != "" {
					return "", false
				}
				return v, true
			}
		}
		return "", false
	case strings.ContainsRune("&*|>{[@`%", rune(s[0])):
		return "", false
	}
	return strings.TrimSpace(stripYAMLComment(s)), true
}

// stripYAMLComment drops a trailing comment: # at the start or after
// whitespace, outside quotes.
func stripYAMLComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// tomlStrings renders a TOML array of basic strings.
func tomlStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func hookBinary() string {
	exe, err := os.Executable()
	if err == nil {
		if p, err := exec.LookPath("spotexec"); err == nil {
			a, _ := filepath.EvalSymlinks(p)
			b, _ := filepath.EvalSymlinks(exe)
			if a == b {
				return "spotexec"
			}
		}
		if !strings.Contains(exe, os.TempDir()) && !strings.Contains(exe, "go-build") {
			// Started as sx: the hook must call spotexec (sx only runs things).
			if filepath.Base(exe) == "sx" {
				if p := filepath.Join(filepath.Dir(exe), "spotexec"); fileExists(p) {
					return p
				}
				if real, err := filepath.EvalSymlinks(exe); err == nil {
					return real
				}
			}
			return exe
		}
	}
	return "spotexec"
}

func cmdHook() *cobra.Command {
	c := &cobra.Command{Use: "hook", Short: "Agent command-interception hooks"}
	var claude, codex, globalScope, shared, uninstall bool
	install := &cobra.Command{
		Use:   "install [--claude] [--codex]",
		Short: "Install the Claude Code PreToolUse hook and/or an AGENTS.md snippet",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !claude && !codex {
				claude = true
			}
			wd, _ := os.Getwd()
			ws, err := client.Resolve(wd)
			if err != nil {
				return err
			}
			if claude {
				var p string
				switch {
				case globalScope:
					home, _ := os.UserHomeDir()
					p = filepath.Join(home, ".claude", "settings.json")
				case shared:
					p = filepath.Join(ws.Root, ".claude", "settings.json")
				default:
					p = filepath.Join(ws.Root, ".claude", "settings.local.json")
				}
				if uninstall {
					ok, err := hooks.UninstallClaude(p)
					if err != nil {
						return err
					}
					fmt.Printf("%s: %s\n", p, map[bool]string{true: "removed spotexec hook", false: "no spotexec hook found"}[ok])
				} else {
					hookCmd := transportQuote(hookBinary()) + " hook claude"
					legacy := hooks.HasLegacyClaude(p)
					ok, err := hooks.InstallClaude(p, hookCmd)
					if err != nil {
						return err
					}
					if ok && legacy {
						fmt.Printf("updated the PreToolUse hook in %s to call spotexec (sx only runs commands)\n", p)
					} else if ok {
						fmt.Printf("installed PreToolUse hook in %s\n", p)
					} else {
						fmt.Printf("hook already present in %s\n", p)
					}
					if !exists(ws.Root, ".spotexec.toml") {
						fmt.Println("note: the hook only offloads in projects with a .spotexec.toml (run `spotexec init`)")
					}
					fmt.Println("note: permission rules now see `spotexec run -- <cmd>`; allow `Bash(spotexec run:*)` if you pre-approve test commands")
				}
			}
			if codex {
				p := filepath.Join(ws.Root, "AGENTS.md")
				b, _ := os.ReadFile(p)
				has := strings.Contains(string(b), "offloads heavy commands to remote runners with spotexec")
				switch {
				case uninstall && has:
					nb := strings.Replace(string(b), hooks.AgentsSnippet, "", 1)
					if nb == string(b) {
						fmt.Printf("%s: the spotexec section was edited; remove it by hand\n", p)
					} else if err := os.WriteFile(p, []byte(strings.TrimRight(nb, "\n")+"\n"), 0o644); err != nil {
						return err
					} else {
						fmt.Printf("removed the spotexec section from %s\n", p)
					}
				case uninstall:
				case has:
					fmt.Printf("%s already has the spotexec section\n", p)
				default:
					f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
					if err != nil {
						return err
					}
					if len(b) > 0 && !strings.HasSuffix(string(b), "\n\n") {
						f.WriteString("\n")
					}
					f.WriteString(hooks.AgentsSnippet)
					f.Close()
					fmt.Printf("appended spotexec section to %s\n", p)
				}
			}
			return nil
		},
	}
	install.Flags().BoolVar(&claude, "claude", false, "Claude Code PreToolUse hook (default)")
	install.Flags().BoolVar(&codex, "codex", false, "append an AGENTS.md section for Codex and other harnesses")
	install.Flags().BoolVar(&globalScope, "global", false, "install into ~/.claude/settings.json")
	install.Flags().BoolVar(&shared, "shared", false, "install into .claude/settings.json (committed) instead of settings.local.json")
	install.Flags().BoolVar(&uninstall, "uninstall", false, "remove the hook instead")
	handler := &cobra.Command{
		Use:    "claude",
		Short:  "PreToolUse hook handler (reads the event JSON on stdin)",
		Hidden: true,
		Run:    func(*cobra.Command, []string) { claudeHook() },
	}
	check := &cobra.Command{
		Use:                "check <command>",
		Short:              "Show whether the hook would offload a command line",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true, // the command's own flags (pytest -x) are not ours
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			wd, _ := os.Getwd()
			l, err := load(wd)
			if err != nil {
				return err
			}
			d, err := hooks.NewDecider(l.cfg.Project.Hook)
			if err != nil {
				return err
			}
			line := strings.Join(args, " ")
			runnersErr := hooks.RunnersFor(line, wd, l.cfg.Project)
			// The same decision the hook makes (project opt-in, disabled,
			// cd outside the repository).
			switch {
			case l.cfg.Project.Path == "":
				fmt.Println("local (no .spotexec.toml: the hook only offloads in projects that opt in)")
			case l.cfg.Project.Hook.Disabled:
				fmt.Println("local ([hook] disabled = true)")
			case !d.ShouldOffload(line):
				fmt.Println("local")
			case !hooks.CdsInside(line, wd):
				fmt.Println("local (cd leaves the repository)")
			case l.cfg.Project.Hook.WhenLoadAbove > 0 && belowLoad(l.cfg.Project.Hook.WhenLoadAbove):
				fmt.Printf("local (load per CPU is below when_load_above = %.2f)\n", l.cfg.Project.Hook.WhenLoadAbove)
			case runnersErr != nil:
				fmt.Printf("local (no usable runners: %v)\n", runnersErr)
			default:
				fmt.Println("offload:", hooks.Wrap("spotexec", line))
				return nil
			}
			return errors.New("not offloaded")
		},
	}
	c.AddCommand(install, handler, check)
	return c
}

// claudeHook handles one Claude Code PreToolUse event on stdin.
func claudeHook() {
	hooks.HandleClaude(os.Stdin, os.Stdout, transportQuote(hookBinary()))
}

func transportQuote(s string) string {
	if strings.ContainsAny(s, " '\"") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// detectRegion picks the AWS region for a new config the way the AWS CLI
// would for profile (empty: AWS_PROFILE, else default), reading the AWS
// config files directly: AWS_REGION, AWS_DEFAULT_REGION, the profile's
// region, else us-east-1. why says which, for the user.
func detectRegion(profile string) (region, why string) {
	r, from := provider.ProfileRegion(profile)
	if r != "" {
		return r, "from " + from
	}
	return "us-east-1", "the default: " + from
}

func belowLoad(threshold float64) bool {
	l, err := hooks.LoadPerCPU()
	return err != nil || l < threshold
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
