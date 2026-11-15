package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
)

func resolveIn(t *testing.T, dir string) *client.Workspace {
	t.Helper()
	ws, err := client.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// init and container init never mark the home directory or /, and need a
// confirmation (--yes when not interactive) for a non-Git directory.
func TestCheckProjectDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{home, "/"} {
		err := checkProjectDir(resolveIn(t, d), true)
		if err == nil || !strings.Contains(err.Error(), "home directory") {
			t.Errorf("%s: want a refusal even with --yes, got %v", d, err)
		}
	}
	ssh := filepath.Join(home, ".ssh")
	os.MkdirAll(ssh, 0o700)
	if err := checkProjectDir(resolveIn(t, ssh), true); err == nil {
		t.Error("~/.ssh accepted as a project")
	}
	keys := filepath.Join(home, "keys")
	os.MkdirAll(keys, 0o700)
	os.Symlink(keys, filepath.Join(home, ".gnupg"))
	if ws := resolveIn(t, filepath.Join(home, ".gnupg")); ws.Marked || checkProjectDir(ws, true) == nil {
		t.Error("a symlinked ~/.gnupg accepted as a project")
	}
	// A leftover ~/.spotexec.toml doesn't change that.
	os.WriteFile(filepath.Join(home, ".spotexec.toml"), nil, 0o644)
	if err := checkProjectDir(resolveIn(t, home), true); err == nil {
		t.Error("home with a .spotexec.toml accepted")
	}

	plain := filepath.Join(home, "notes")
	os.MkdirAll(plain, 0o755)
	ws := resolveIn(t, plain)
	if ws.Marked {
		t.Fatalf("plain dir %s resolved as marked", ws.Root)
	}
	// Tests run without a terminal on stdin: refuse, pointing at --yes.
	if err := checkProjectDir(ws, false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-git without --yes: %v", err)
	}
	if err := checkProjectDir(ws, true); err != nil {
		t.Errorf("non-git with --yes: %v", err)
	}

	repo := filepath.Join(home, "repo")
	os.MkdirAll(repo, 0o755)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := checkProjectDir(resolveIn(t, repo), false); err != nil {
		t.Errorf("git repo: %v", err)
	}
}

// The written region follows the profile setup signs in with, read from
// the AWS config file without the AWS CLI.
func TestWriteGlobalTemplateRegion(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "aws-config")
	os.WriteFile(cfg, []byte("[default]\nregion = us-west-2\n[profile work]\nsso_session = corp\nregion = eu-west-1\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("PATH", dir) // no aws CLI
	for _, c := range []struct{ profile, env, want string }{
		{"work", "", "eu-west-1"},
		{"", "work", "eu-west-1"},
		{"", "", "us-west-2"},
		{"nosuch", "", "us-east-1"},
	} {
		t.Setenv("AWS_PROFILE", c.env)
		p := filepath.Join(dir, "config.toml")
		os.Remove(p)
		region, why, err := writeGlobalTemplate(p, c.profile)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		if region != c.want || !strings.Contains(string(b), `region = "`+c.want+`"`) || why == "" {
			t.Errorf("profile %q (AWS_PROFILE %q): region %q (%s), want %q", c.profile, c.env, region, why, c.want)
		}
	}
}

// runInit runs `spotexec init` with args in dir.
func runInit(t *testing.T, dir string, args ...string) {
	t.Helper()
	t.Chdir(dir)
	c := cmdInit()
	c.SetArgs(args)
	if err := c.Execute(); err != nil {
		t.Fatalf("init %v: %v", args, err)
	}
}

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for n, b := range files {
		os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644)
	}
	return dir
}

// init sets up the project's container when it detects a toolchain, unless
// told not to, the runners are all SSH machines, or there is no toolchain.
func TestInitSetsUpContainer(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(cfgDir, "config.toml")) // no runners yet
	node := map[string]string{"package.json": `{"packageManager":"pnpm@9.15.0","scripts":{"test":"vitest"}}`, "pnpm-lock.yaml": "", ".nvmrc": "22.11.0\n"}

	dir := gitRepo(t, node)
	runInit(t, dir)
	df, err := os.ReadFile(filepath.Join(dir, ".spotexec", "Dockerfile"))
	if err != nil || !strings.Contains(string(df), "FROM node:22.11.0-bookworm") || !strings.Contains(string(df), "pnpm@9.15.0") {
		t.Fatalf("Dockerfile: %v\n%s", err, df)
	}
	toml, _ := os.ReadFile(filepath.Join(dir, ".spotexec.toml"))
	if !strings.Contains(string(toml), "[container]\ndockerfile = \".spotexec/Dockerfile\"") {
		t.Fatalf(".spotexec.toml not wired:\n%s", toml)
	}

	// --force keeps an edited Dockerfile and wires it again.
	os.WriteFile(filepath.Join(dir, ".spotexec", "Dockerfile"), []byte("FROM node:22\n"), 0o644)
	runInit(t, dir, "--force")
	if b, _ := os.ReadFile(filepath.Join(dir, ".spotexec", "Dockerfile")); string(b) != "FROM node:22\n" {
		t.Fatalf("init --force rewrote the Dockerfile: %s", b)
	}
	if toml, _ := os.ReadFile(filepath.Join(dir, ".spotexec.toml")); !strings.Contains(string(toml), "[container]") {
		t.Fatal("init --force dropped [container]")
	}

	for name, setup := range map[string]func() (string, []string){
		"--no-container": func() (string, []string) { return gitRepo(t, node), []string{"--no-container"} },
		"no toolchain":   func() (string, []string) { return gitRepo(t, map[string]string{"README.md": "hi"}), nil },
		"ssh runners only": func() (string, []string) {
			os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("version = 1\n[runners.box]\ntype = \"ssh\"\nhost = \"me@box\"\n"), 0o644)
			return gitRepo(t, node), nil
		},
	} {
		dir, args := setup()
		runInit(t, dir, args...)
		if _, err := os.Stat(filepath.Join(dir, ".spotexec", "Dockerfile")); err == nil {
			t.Errorf("%s: wrote a Dockerfile", name)
		}
		if toml, _ := os.ReadFile(filepath.Join(dir, ".spotexec.toml")); strings.Contains(string(toml), "[container]") {
			t.Errorf("%s: wired [container]", name)
		}
	}
}

// doctor flags a toolchain project with no container on an AWS pool.
func TestDoctorContainer(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n\ngo 1.25.3\n"})
	g := &config.Global{Runners: map[string]*config.Runner{"aws": {Name: "aws", Type: config.TypeAWSSpot}}}
	l := &loaded{cfg: &config.Config{Global: g, Project: &config.Project{}}, ws: &client.Workspace{Root: dir}}
	var got []string
	check := func(n string, ok bool, m string) { got = append(got, fmt.Sprintf("%v %s", ok, m)) }
	warn := func(n, m string) { got = append(got, "warn "+m) }

	doctorContainer(l, nil, check, warn)
	l.cfg.Global.Runners["box"] = &config.Runner{Name: "box", Type: config.TypeSSH}
	l.cfg.Global.DefaultPool = []string{"box"}
	doctorContainer(l, nil, check, warn)
	doctorContainer(l, []string{"aws"}, check, warn) // `spotexec doctor aws`
	l.cfg.Project.Container.Dockerfile = ".spotexec/Dockerfile"
	doctorContainer(l, nil, check, warn)
	os.MkdirAll(filepath.Join(dir, ".spotexec"), 0o755)
	os.WriteFile(filepath.Join(dir, ".spotexec", "Dockerfile"), []byte("FROM golang:1.25.3-bookworm\n"), 0o644)
	doctorContainer(l, nil, check, warn)

	want := []string{"warn none, but this is a Go 1.25.3 project", "true none", "warn none", "false .spotexec/Dockerfile is missing", "true .spotexec/Dockerfile"}
	for i, w := range want {
		if i >= len(got) || !strings.HasPrefix(got[i], w) {
			t.Fatalf("doctor container checks = %q, want prefixes %q", got, want)
		}
	}
}

// Workspace node_modules are cache_dirs too, from each package manager's
// own workspace declaration; otherwise a run restored from the cache is
// missing them.
func TestDetectProjectWorkspaceCacheDirs(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		dirs    string
		noteHas string
	}{
		{
			name:  "npm array",
			files: map[string]string{"package.json": `{"workspaces": ["packages/*", "./tools/cli/", "packages/*", "!packages/legacy", "."]}`, "package-lock.json": ""},
			dirs:  `cache_dirs = ["node_modules", "packages/*/node_modules", "tools/cli/node_modules"]`, noteHas: "npm workspaces",
		},
		{
			name:  "npm without workspaces",
			files: map[string]string{"package.json": `{"name": "app"}`, "package-lock.json": ""},
			dirs:  `cache_dirs = ["node_modules"]`,
		},
		{
			name:  "npm malformed package.json",
			files: map[string]string{"package.json": `{"workspaces": [`, "package-lock.json": ""},
			dirs:  `cache_dirs = ["node_modules"]`, noteHas: "package.json didn't parse",
		},
		{
			name:  "npm workspaces of the wrong type",
			files: map[string]string{"package.json": `{"workspaces": "packages/*"}`, "package-lock.json": ""},
			dirs:  `cache_dirs = ["node_modules"]`,
		},
		{
			name:  "yarn classic object form",
			files: map[string]string{"package.json": `{"workspaces": {"packages": ["packages/*", "apps/web"], "nohoist": ["**/react-native"]}}`, "yarn.lock": ""},
			dirs:  `cache_dirs = ["node_modules", "packages/*/node_modules", "apps/web/node_modules"]`, noteHas: "yarn workspaces",
		},
		{
			name:  "yarn berry array",
			files: map[string]string{"package.json": `{"packageManager": "yarn@4.5.1", "workspaces": ["packages/*"]}`, "yarn.lock": "", ".yarnrc.yml": "nodeLinker: node-modules\n"},
			dirs:  `cache_dirs = ["node_modules", "packages/*/node_modules"]`, noteHas: "yarn workspaces",
		},
		{
			name:  "bun object form",
			files: map[string]string{"package.json": `{"workspaces": {"packages": ["packages/*"], "catalog": {"react": "^19"}}}`, "bun.lock": ""},
			dirs:  `cache_dirs = ["node_modules", "packages/*/node_modules"]`, noteHas: "bun workspaces",
		},
		{
			name:  "unsupported globs become a note",
			files: map[string]string{"package.json": `{"workspaces": ["libs/**", "{a,b}/*", "../shared", "packages/*"]}`, "package-lock.json": ""},
			dirs:  `cache_dirs = ["node_modules", "packages/*/node_modules"]`, noteHas: `"libs/**", "{a,b}/*", "../shared"`,
		},
		{
			name: "pnpm block list",
			files: map[string]string{"package.json": `{"workspaces": ["ignored/*"]}`, "pnpm-lock.yaml": "", "pnpm-workspace.yaml": `# workspace
packages:
  - 'packages/*'   # libraries
  - "apps/*"

  - tools/cli
  - '!**/test/**'
onlyBuiltDependencies:
  - esbuild
`},
			dirs: `cache_dirs = ["node_modules", "packages/*/node_modules", "apps/*/node_modules", "tools/cli/node_modules"]`, noteHas: "pnpm-workspace.yaml packages",
		},
		{
			name:  "pnpm flow list",
			files: map[string]string{"pnpm-lock.yaml": "", "pnpm-workspace.yaml": "packages: ['libs/*', \"web\"] # all\n"},
			dirs:  `cache_dirs = ["node_modules", "libs/*/node_modules", "web/node_modules"]`, noteHas: "pnpm workspaces",
		},
		{
			name:  "pnpm workspace file without packages",
			files: map[string]string{"pnpm-lock.yaml": "", "pnpm-workspace.yaml": "onlyBuiltDependencies:\n  - esbuild\n"},
			dirs:  `cache_dirs = ["node_modules"]`,
		},
		{
			name:  "pnpm unreadable packages keeps the guess",
			files: map[string]string{"pnpm-lock.yaml": "", "pnpm-workspace.yaml": "packages: &pkgs\n  - packages/*\n"},
			dirs:  `cache_dirs = ["node_modules", "packages/*/node_modules", "apps/*/node_modules"]`, noteHas: "adjust deps.cache_dirs",
		},
		{
			name:  "pnpm without a workspace file",
			files: map[string]string{"package.json": `{"workspaces": ["packages/*"]}`, "pnpm-lock.yaml": ""},
			dirs:  `cache_dirs = ["node_modules"]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range c.files {
				os.WriteFile(filepath.Join(root, name), []byte(body), 0o644)
			}
			body, notes := detectProject(root)
			if !strings.Contains(body, c.dirs+"\n") {
				t.Fatalf("want %s in:\n%s", c.dirs, body)
			}
			joined := strings.Join(notes, "\n")
			if c.noteHas != "" && !strings.Contains(joined, c.noteHas) {
				t.Errorf("want a note containing %s, got:\n%s", c.noteHas, joined)
			}
			if c.noteHas == "" && strings.Contains(joined, "cache_dirs") {
				t.Errorf("unexpected cache_dirs note:\n%s", joined)
			}
			p := filepath.Join(root, ".spotexec.toml")
			os.WriteFile(p, []byte(body), 0o644)
			if _, err := config.LoadProject(p); err != nil {
				t.Errorf("written config doesn't load: %v\n%s", err, body)
			}
		})
	}
}

// What init emits must expand the way the helper expands cache_dirs: one
// directory per *.
func TestWorkspaceNodeModulesExpand(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"node_modules", "packages/a/node_modules", "packages/b/node_modules", "packages/c"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	dirs, skipped := workspaceNodeModules([]string{"packages/*"})
	if len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	var got []string
	for _, p := range dirs {
		m, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(p)))
		for _, x := range m {
			rel, _ := filepath.Rel(root, x)
			got = append(got, filepath.ToSlash(rel))
		}
	}
	if want := "node_modules packages/a/node_modules packages/b/node_modules"; strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestPnpmWorkspacePackages(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"packages:\n- a/*\n- 'it''s/*'\n- \"q\\\"d\"\nother: 1\n", `a/*|it's/*|q"d`, true},
		{"packages:\r\n  - x # c\r\n", "x", true},
		{"packages: []\n", "", true},
		{"packages: [a, 'b' c]\n", "", false},
		{"packages:\n  - |\n    a\n", "", false},
		{"packages: a\n", "", false},
		{"catalog:\n  packages: 1\n", "", true},
	}
	for _, c := range cases {
		got, ok := pnpmWorkspacePackages(c.in)
		if ok != c.ok || strings.Join(got, "|") != c.want {
			t.Errorf("%q: got %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// A backslash in a workspace pattern is a glob escape, not a name: it is
// skipped with the others init can't express (#92).
func TestWorkspaceNodeModulesSkipsBackslash(t *testing.T) {
	dirs, skipped := workspaceNodeModules([]string{`packages/\*`, "apps/*"})
	if strings.Join(dirs, " ") != "node_modules apps/*/node_modules" || len(skipped) != 1 || !strings.Contains(skipped[0], "packages") {
		t.Errorf("dirs %v, skipped %v", dirs, skipped)
	}
}
