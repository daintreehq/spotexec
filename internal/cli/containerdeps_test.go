package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for n, b := range files {
		p := filepath.Join(root, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrowserDeps(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []browserDep
		note  string
	}{
		{"none", map[string]string{"package.json": `{"dependencies":{"react":"^18"},"scripts":{"e2e":"playwright test"}}`}, nil, ""},
		{"every section", map[string]string{"package.json": `{"dependencies":{"puppeteer":"^24"},"devDependencies":{"electron":"^38"},"optionalDependencies":{"playwright":"1.56.0"}}`},
			[]browserDep{{"Electron", "electron", "package.json"}, {"Playwright", "playwright", "package.json"}, {"Puppeteer", "puppeteer", "package.json"}}, ""},
		{"lookalikes", map[string]string{"package.json": `{"devDependencies":{"puppeteer-core":"^24","playwright-core":"^1","electron-builder":"^25"}}`}, nil, ""},
		{"npm workspaces", map[string]string{
			"package.json":              `{"workspaces":["apps/*","!apps/legacy"],"devDependencies":{"typescript":"^5"}}`,
			"apps/desktop/package.json": `{"devDependencies":{"electron":"^38","@playwright/test":"^1.56.0"}}`,
			"apps/legacy/package.json":  `{"devDependencies":{"puppeteer":"^24"}}`,
		}, []browserDep{{"Electron", "electron", "apps/desktop/package.json"}, {"Playwright", "@playwright/test", "apps/desktop/package.json"}}, ""},
		{"yarn workspaces object", map[string]string{
			"package.json":              `{"workspaces":{"packages":["packages/*"]},"packageManager":"yarn@4.5.1"}`,
			"packages/e2e/package.json": `{"devDependencies":{"@playwright/test":"^1"}}`,
		}, []browserDep{{"Playwright", "@playwright/test", "packages/e2e/package.json"}}, ""},
		// The root's declaration wins over a workspace's.
		{"pnpm workspaces", map[string]string{
			"package.json":            `{"packageManager":"pnpm@9.15.0","devDependencies":{"playwright":"^1"}}`,
			"pnpm-workspace.yaml":     "packages:\n  - 'tools/*'\n  - 'web/**'\n",
			"tools/shot/package.json": `{"dependencies":{"puppeteer":"^24","playwright":"^1"}}`,
		}, []browserDep{{"Playwright", "playwright", "package.json"}, {"Puppeteer", "puppeteer", "tools/shot/package.json"}}, `pattern(s) "web/**"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, c.files)
			pkg := readPackageJSON(root)
			pm, _ := jsPackageManager(root, pkg)
			got, notes := browserDeps(root, pm, pkg)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("deps %v, want %v", got, c.want)
			}
			if c.note == "" && len(notes) > 0 || c.note != "" && !strings.Contains(strings.Join(notes, "\n"), c.note) {
				t.Errorf("notes %q, want %q", notes, c.note)
			}
		})
	}
}

// A workspace symlinked out of the repository isn't read.
func TestBrowserDepsSymlinkOutside(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeTree(t, root, map[string]string{"package.json": `{"workspaces":["packages/*"]}`})
	writeTree(t, outside, map[string]string{"package.json": `{"devDependencies":{"electron":"^38"}}`})
	os.MkdirAll(filepath.Join(root, "packages"), 0o755)
	if err := os.Symlink(outside, filepath.Join(root, "packages", "ext")); err != nil {
		t.Skip(err)
	}
	if got, _ := browserDeps(root, "npm", readPackageJSON(root)); len(got) != 0 {
		t.Fatalf("read a workspace outside the repository: %v", got)
	}
}

func TestBrowserLibs(t *testing.T) {
	pkgs := func(body string) map[string]bool {
		set := map[string]bool{}
		_, run, _ := strings.Cut(body, "apt-get install -y --no-install-recommends")
		run, _, _ = strings.Cut(run, "&& rm -rf")
		for _, f := range strings.Fields(strings.ReplaceAll(run, "\\", "")) {
			if set[f] {
				t.Errorf("%s listed twice", f)
			}
			set[f] = true
		}
		return set
	}
	if body, note := browserLibs(nil); body != "" || note != "" {
		t.Fatalf("libraries for no dependencies: %q %q", body, note)
	}

	body, note := browserLibs([]browserDep{{"Puppeteer", "puppeteer", "package.json"}})
	p := pkgs(body)
	for _, want := range []string{"libnss3", "libgbm1", "libgtk-3-0", "libasound2", "fonts-liberation"} {
		if !p[want] {
			t.Errorf("Puppeteer: no %s in\n%s", want, body)
		}
	}
	if p["xvfb"] || p["libgtk-4-1"] || strings.Contains(body, "ELECTRON_DISABLE_SANDBOX") || !strings.Contains(body, "--no-sandbox") ||
		note != "added the system libraries Puppeteer needs (package.json declares puppeteer); launch Puppeteer's Chrome with --no-sandbox" {
		t.Errorf("Puppeteer: %q\n%s", note, body)
	}

	body, note = browserLibs([]browserDep{{"Electron", "electron", "apps/desktop/package.json"}, {"Playwright", "@playwright/test", "apps/desktop/package.json"}})
	p = pkgs(body)
	for _, want := range []string{"libnss3", "libgtk-3-0", "libsecret-1-0", "xvfb", "xauth", "libgtk-4-1", "libavcodec59", "fonts-noto-color-emoji"} {
		if !p[want] {
			t.Errorf("Electron+Playwright: no %s in\n%s", want, body)
		}
	}
	if !strings.Contains(body, "\nENV ELECTRON_DISABLE_SANDBOX=1\n") || strings.Contains(body, "--no-sandbox") {
		t.Errorf("Electron+Playwright sandbox lines:\n%s", body)
	}
	if !strings.Contains(note, "Electron and Playwright need (apps/desktop/package.json declares electron and @playwright/test)") || !strings.Contains(note, "xvfb-run -a") {
		t.Errorf("note: %q", note)
	}
	for _, l := range strings.Split(body, "\n") {
		if len(l) > 80 {
			t.Errorf("line over 80 columns: %q", l)
		}
	}
}

// container init says what it added and why.
func TestContainerInitReportsBrowserLibs(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"package.json": `{"devDependencies":{"electron":"^38"}}`, "package-lock.json": "{}"})
	var err error
	out := captureStdout(t, func() { err = containerInit(&client.Workspace{Root: root}, defaultDockerfile, false) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote .spotexec/Dockerfile (Node.js 24)\n  added the system libraries Electron needs (package.json declares electron)") {
		t.Fatalf("output:\n%s", out)
	}
	df, _ := os.ReadFile(filepath.Join(root, defaultDockerfile))
	if !strings.Contains(string(df), "libnss3") {
		t.Fatalf("Dockerfile:\n%s", df)
	}
}
