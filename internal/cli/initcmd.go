package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/spotexec/spotexec/internal/client"
	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/hooks"
)

const globalTemplate = `# spotexec global runner config. Docs: spotexec --help
version = 1

# Runners are tried in priority order (lowest first). Ephemeral runners boot
# on demand when every static runner is busy or unreachable.
default_pool = ["homelab", "aws"]

# Never silently fall back to running on this laptop.
local_fallback = false

[runners.homelab]
type = "ssh"
# Tried in parallel; the first address wins if it answers within 500ms.
hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]
max_slots = 4          # concurrent jobs (default: CPUs / 4)
priority = 10
# remote_path_setup = 'export PATH="$HOME/.local/share/mise/shims:$PATH"'

[runners.aws]
type = "aws-spot"
region = "us-east-1"
# profile = "default"                     # AWS CLI profile (SSO works)
instance_types = ["c7a.4xlarge", "c6a.4xlarge", "m7a.4xlarge"]  # tried in order
# max_price = "0.40"                      # USD/hour cap (default: on-demand price)
max_slots = 8
priority = 20
idle_timeout = "10m"   # the VM powers itself off (= terminates) after this much idle time (15m suits long agent pauses)
max_instances = 4      # fleet ceiling
max_lifetime = "12h"   # hard cap regardless of activity
hourly_cost = 0.25     # for 'spotexec monitor' burn estimates
bootstrap_script = """
apt-get update -o DPkg::Lock::Timeout=300
apt-get install -y -o DPkg::Lock::Timeout=300 git curl unzip build-essential
"""
user_bootstrap_script = """
curl -fsSL https://mise.run | sh
"""
remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'
`

func cmdInit() *cobra.Command {
	var global, force bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Generate .spotexec.toml for this repo (or --global runner config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if global {
				p := config.GlobalPath()
				if _, err := os.Stat(p); err == nil && !force {
					return fmt.Errorf("%s exists (use --force to overwrite)", p)
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte(globalTemplate), 0o644); err != nil {
					return err
				}
				fmt.Printf("wrote %s; edit the runners, then run `spotexec doctor`\n", p)
				return nil
			}
			wd, _ := os.Getwd()
			ws, err := client.Resolve(wd)
			if err != nil {
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
			if _, err := os.Stat(config.GlobalPath()); err != nil {
				fmt.Println("no runners configured yet: run `spotexec init --global`")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&global, "global", false, "write an example ~/.config/spotexec/config.toml")
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return c
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

	pm := ""
	switch {
	case exists(root, "pnpm-lock.yaml"):
		pm = "pnpm"
		dirs := `["node_modules"]`
		if exists(root, "pnpm-workspace.yaml") {
			dirs = `["node_modules", "packages/*/node_modules", "apps/*/node_modules"]`
			notes = append(notes, "pnpm workspace detected: adjust deps.cache_dirs globs to your package folders")
		}
		fmt.Fprintf(&b, "[deps]\nlockfiles = [\"pnpm-lock.yaml\"]\ninstall = \"pnpm install --frozen-lockfile\"\ncache_dirs = %s\n\n", dirs)
	case exists(root, "bun.lock") || exists(root, "bun.lockb"):
		pm = "bun"
		lf := "bun.lock"
		if !exists(root, lf) {
			lf = "bun.lockb"
		}
		fmt.Fprintf(&b, "[deps]\nlockfiles = [%q]\ninstall = \"bun install --frozen-lockfile\"\ncache_dirs = [\"node_modules\"]\n\n", lf)
	case exists(root, "yarn.lock"):
		pm = "yarn"
		inst := "yarn install --frozen-lockfile"
		if exists(root, ".yarnrc.yml") {
			inst = "yarn install --immutable"
		}
		fmt.Fprintf(&b, "[deps]\nlockfiles = [\"yarn.lock\"]\ninstall = %q\ncache_dirs = [\"node_modules\"]\n\n", inst)
	case exists(root, "package-lock.json"):
		pm = "npm"
		b.WriteString("[deps]\nlockfiles = [\"package-lock.json\"]\ninstall = \"npm ci\"\ncache_dirs = [\"node_modules\"]\n\n")
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
				add(name, run+s+" {args}")
			}
		}
		if _, ok := tasks["typecheck"]; !ok && exists(root, "tsconfig.json") {
			add("typecheck", pm+" exec tsc --noEmit")
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
					ok, err := hooks.InstallClaude(p, hookCmd)
					if err != nil {
						return err
					}
					if ok {
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
				if strings.Contains(string(b), "offloads heavy commands to remote runners with spotexec") {
					fmt.Printf("%s already has the spotexec section\n", p)
				} else if !uninstall {
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
		Run: func(cmd *cobra.Command, args []string) {
			hooks.HandleClaude(os.Stdin, os.Stdout, transportQuote(hookBinary()))
		},
	}
	check := &cobra.Command{
		Use:   "check <command>",
		Short: "Show whether the hook would offload a command line",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if d.ShouldOffload(line) {
				fmt.Println("offload:", hooks.Wrap("spotexec", line))
				return nil
			}
			fmt.Println("local")
			return errors.New("not offloaded")
		},
	}
	c.AddCommand(install, handler, check)
	return c
}

func transportQuote(s string) string {
	if strings.ContainsAny(s, " '\"") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}
