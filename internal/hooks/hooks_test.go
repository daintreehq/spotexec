package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spotexec/spotexec/internal/config"
)

func TestShouldOffload(t *testing.T) {
	d, err := NewDecider(config.Hook{})
	if err != nil {
		t.Fatal(err)
	}
	yes := []string{
		"pnpm test", "pnpm vitest run src/a.test.ts", "npx prettier --write .", "pytest -x tests/",
		"cargo test", "go test ./...", "cd packages/api && pnpm test", "CI=1 pnpm run lint",
		"pnpm tsc --noEmit", "uv run pytest", "npm run test:unit",
		"pytest /Users/me/dev/app/tests", "pnpm test 2>&1", "cd pkg && go test ./... 2>&1", "pnpm vitest run src/watch.test.ts", "./gradlew test", "mvn -q verify",
	}
	no := []string{
		"git status", "ls -la", "rg foo", "pnpm dev", "pnpm vitest --watch", "pnpm test | tee out.txt",
		"pnpm test > out.txt", "pnpm test && git commit -m x", "sx pnpm test", "echo $(pnpm test)",
		"pnpm install", "pnpm test &", "",
		"SPOTEXEC_LOCAL=1 pytest", "mvn deploy", "./gradlew bootRun", "pnpm run dev", "npm run serve",
		"pnpm test:watch", "npm run lint:watch", "/usr/local/bin/spotexec run -- pnpm test",
	}
	for _, c := range yes {
		if !d.ShouldOffload(c) {
			t.Errorf("expected offload: %q", c)
		}
	}
	for _, c := range no {
		if d.ShouldOffload(c) {
			t.Errorf("expected local: %q", c)
		}
	}
}

func TestHandleClaudeRewrites(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte("version = 1\n"), 0o644)
	in, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "cwd": dir,
		"tool_input": map[string]any{"command": "pnpm test", "description": "run tests", "timeout": 120000},
	})
	var out bytes.Buffer
	HandleClaude(bytes.NewReader(in), &out, "spotexec")
	var res struct {
		H struct {
			Event   string         `json:"hookEventName"`
			Updated map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("bad output %q: %v", out.String(), err)
	}
	if res.H.Updated["command"] != "spotexec run -- 'pnpm test'" || res.H.Updated["description"] != "run tests" {
		t.Fatalf("unexpected rewrite: %+v", res.H.Updated)
	}
}

func TestHandleClaudeNoProjectNoOp(t *testing.T) {
	dir := t.TempDir()
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "pnpm test"}})
	var out bytes.Buffer
	HandleClaude(bytes.NewReader(in), &out, "spotexec")
	if out.Len() != 0 {
		t.Fatalf("expected no output outside opted-in projects, got %q", out.String())
	}
}

func TestInstallIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"model":"x","hooks":{"Stop":[]}}`), 0o644)
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || ok {
		t.Fatal("second install should be a no-op", ok, err)
	}
	b, _ := os.ReadFile(p)
	var s map[string]any
	json.Unmarshal(b, &s)
	if s["model"] != "x" {
		t.Fatal("lost existing settings")
	}
	if ok, _ := UninstallClaude(p); !ok {
		t.Fatal("uninstall failed")
	}
}

func TestCdsInside(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]bool{
		"cd pkg && pnpm test":          true,
		"cd " + root + " && pnpm test": true,
		"cd " + sub + " && pnpm test":  true,
		"cd .. && pnpm test":           false,
		"cd /tmp && pytest":            false,
		"cd ~/x && pytest":             false,
		"pnpm test":                    true,
	} {
		if got := cdsInside(cmd, root); got != want {
			t.Errorf("cdsInside(%q) = %v, want %v", cmd, got, want)
		}
	}
}
