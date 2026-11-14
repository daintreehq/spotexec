package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	os.Stdout = old
	w.Close()
	return <-done
}

func TestSxCompletesFlagsAndTasks(t *testing.T) {
	t.Setenv("SPOTEXEC_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".spotexec.toml"), []byte("version = 1\n[tasks.unit]\nrun = \"pnpm vitest run\"\n[tasks.lint]\nrun = \"pnpm lint\"\n"), 0o644)
	complete := func(words ...string) string {
		var code int
		out := captureStdout(t, func() { code = MainAs("sx", append([]string{"__complete", "-C", dir}, words...)) })
		if code != 0 {
			t.Fatalf("sx __complete %v exited %d", words, code)
		}
		return out
	}
	if out := complete(""); !strings.Contains(out, "lint\ttask: pnpm lint\n") || !strings.Contains(out, "unit\ttask: pnpm vitest run\n") {
		t.Fatalf("tasks not completed:\n%s", out)
	}
	if out := complete("u"); strings.Contains(out, "lint") || !strings.Contains(out, "unit") {
		t.Fatalf("prefix ignored:\n%s", out)
	}
	if out := complete("--same"); !strings.Contains(out, "--same-slot") {
		t.Fatalf("flags not completed:\n%s", out)
	}
	if out := complete("--sync", ""); !strings.Contains(out, "auto\nin\nnone\n") {
		t.Fatalf("--sync values not completed:\n%s", out)
	}
	// After the command, words are the command's own: no sx flags.
	if out := complete("pnpm", "--same"); strings.Contains(out, "--same-slot") {
		t.Fatalf("sx flags offered to the command:\n%s", out)
	}
}

func TestCompletionScripts(t *testing.T) {
	for shell, want := range map[string]string{
		"bash": "__start_sx", "zsh": "#compdef sx", "fish": "complete -c sx", "powershell": "Register-ArgumentCompleter",
	} {
		out := captureStdout(t, func() {
			if code := MainAs("spotexec", []string{"completion", "sx", shell}); code != 0 {
				t.Errorf("completion sx %s exited %d", shell, code)
			}
		})
		if !strings.Contains(out, want) {
			t.Errorf("completion sx %s: no %q in output", shell, want)
		}
	}
	out := captureStdout(t, func() { MainAs("spotexec", []string{"completion", "zsh"}) })
	if !strings.Contains(out, "#compdef spotexec") {
		t.Errorf("spotexec's own completion broke:\n%.200s", out)
	}
	var b bytes.Buffer
	root := newRoot()
	root.SetOut(&b)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil || b.String() != "spotexec "+Version+"\n" {
		t.Errorf("spotexec --version = %q, %v", b.String(), err)
	}
}
