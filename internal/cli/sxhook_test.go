package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A hook installed by v0.2.0 runs `sx hook claude`: it must behave exactly
// like `spotexec hook claude` (answer on stdout, exit 0), never run remotely.
func TestSxHookClaudeIsTheHookHandler(t *testing.T) {
	// No real config: were this ever run as a command, it could go nowhere.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SPOTEXEC_CONFIG_DIR", filepath.Join(home, "spotexec"))
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(home, "spotexec", "config.toml"))
	os.MkdirAll(filepath.Join(home, "spotexec"), 0o755)
	if err := os.WriteFile(filepath.Join(home, "spotexec", "config.toml"), []byte("version = 1\n[runners.box]\ntype = \"ssh\"\nhost = \"me@box\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTEXEC_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("SPOTEXEC_POOL", "")
	t.Setenv("SPOTEXEC_RUNNER", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "cwd": dir,
		"tool_input": map[string]any{"command": "pnpm test"},
	})
	run := func(name string, args ...string) (int, string) {
		inR, inW, _ := os.Pipe()
		outR, outW, _ := os.Pipe()
		oldIn, oldOut := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = inR, outW
		inW.Write(ev)
		inW.Close()
		code := MainAs(name, args)
		os.Stdin, os.Stdout = oldIn, oldOut
		outW.Close()
		b, _ := io.ReadAll(outR)
		inR.Close()
		outR.Close()
		return code, string(b)
	}
	wantCode, want := run("spotexec", "hook", "claude")
	code, got := run("sx", "hook", "claude")
	if code != 0 || wantCode != 0 || got != want {
		t.Fatalf("sx hook claude = %d %q, want %d %q", code, got, wantCode, want)
	}
	var res struct {
		H struct {
			Updated map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(got), &res); err != nil {
		t.Fatalf("bad hook output %q: %v", got, err)
	}
	if c, _ := res.H.Updated["command"].(string); !strings.Contains(c, "spotexec run -- ") {
		t.Fatalf("unexpected rewrite %q", c)
	}
}
