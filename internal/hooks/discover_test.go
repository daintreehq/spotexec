package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/spotexec/internal/config"
)

type call struct {
	id, cmd, cwd string
	at           time.Time
	bg           bool
}

func transcript(t *testing.T, path string, calls ...call) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	var b strings.Builder
	for _, c := range calls {
		e := map[string]any{
			"type": "assistant", "cwd": c.cwd, "sessionId": filepath.Base(path), "timestamp": c.at.Format(time.RFC3339Nano),
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "running Bash"},
				map[string]any{"type": "tool_use", "id": c.id, "name": "Bash", "input": map[string]any{"command": c.cmd, "run_in_background": c.bg}},
			}},
		}
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
		// Other entries: a user turn and a tool result naming Bash.
		b.WriteString(`{"type":"user","message":{"content":"run \"Bash\" please"}}` + "\n")
	}
	b.WriteString("{not json\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	projects := t.TempDir()
	root := filepath.Join(t.TempDir(), "my.repo")
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	other := filepath.Join(t.TempDir(), "other")
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)
	dir := filepath.Join(projects, projectDirName(root))
	transcript(t, filepath.Join(dir, "s1.jsonl"),
		call{id: "1", cmd: "pnpm test", cwd: root, at: now},
		call{id: "2", cmd: "go test ./cmd/spotexec", cwd: filepath.Join(root, "sub"), at: now},
		call{id: "3", cmd: "sx run -- go test ./...", cwd: root, at: now},
		call{id: "4", cmd: "timeout 600 cargo test > log.txt", cwd: root, at: now},
		call{id: "5", cmd: "timeout 600 cargo test > log.txt", cwd: root, at: now},
		call{id: "6", cmd: `git commit -m "fix pnpm test"`, cwd: root, at: now},
		call{id: "7", cmd: "pnpm dev", cwd: root, at: now},
		call{id: "8", cmd: "pnpm test | xargs echo", cwd: root, at: now},
		call{id: "9", cmd: "pnpm test", cwd: root, at: old},   // too old
		call{id: "10", cmd: "pnpm test", cwd: other, at: now}, // another repository
		call{id: "11", cmd: "pnpm test", cwd: root, at: now, bg: true},
		call{id: "12", cmd: "cat > x.go <<'EOF'\nfunc main() { go test }\n&& pnpm test\nEOF", cwd: root, at: now},
		call{id: "13", cmd: "SPOTEXEC_LOCAL=1 pnpm test", cwd: root, at: now},
		call{id: "14", cmd: "cd .. && pnpm test", cwd: root, at: now},
		call{id: "15", cmd: "git status && pnpm test", cwd: root, at: now},
		call{id: "16", cmd: "bash -c 'pnpm test'", cwd: root, at: now},
		call{id: "17", cmd: `git commit -m "say \" && pnpm test"`, cwd: root, at: now},
	)
	// A subagent's transcript, and the same call recorded twice.
	transcript(t, filepath.Join(dir, "s1", "subagents", "agent-a.jsonl"),
		call{id: "1", cmd: "pnpm test", cwd: root, at: now},
		call{id: "20", cmd: "cargo test |& tail", cwd: root, at: now},
	)
	// A session of another repository whose name shares the prefix.
	transcript(t, filepath.Join(projects, projectDirName(root)+"x", "s.jsonl"), call{id: "30", cmd: "pnpm test", cwd: root + "x", at: now})
	// An old transcript file is skipped whole.
	transcript(t, filepath.Join(dir, "old.jsonl"), call{id: "40", cmd: "pnpm test", cwd: root, at: now})
	os.Chtimes(filepath.Join(dir, "old.jsonl"), old, old)

	d, _ := NewDecider(config.Hook{})
	res, err := Discover(projects, []string{root}, now.Add(-30*24*time.Hour), d)
	if err != nil {
		t.Fatal(err)
	}
	// 1, 2 and 20 offload; 3 ran spotexec; 4 (twice), 8, 14 and 15 are
	// heavy and kept local; 6, 7, 12, 13 and 16 (quoted) are something else.
	if res.Commands != 15 || res.Offloaded != 3 || res.Direct != 1 || res.Sessions != 2 {
		t.Fatalf("got %+v", res)
	}
	got := map[string]Miss{}
	n := 0
	for _, m := range res.Missed {
		got[m.Command] = m
		n += m.Count
	}
	if n != 5 || len(res.Missed) != 4 || res.Missed[0].Command != "timeout 600 cargo test > log.txt" || res.Missed[0].Count != 2 {
		t.Fatalf("missed: %+v", res.Missed)
	}
	for cmd, reason := range map[string]string{
		"timeout 600 cargo test > log.txt": "shell syntax",
		"pnpm test | xargs echo":           "a pipe into something",
		"cd .. && pnpm test":               "a cd leaves the repository",
		"git status && pnpm test":          "mixed with `git status`",
	} {
		if !strings.Contains(got[cmd].Reason, reason) {
			t.Errorf("%q: reason %q, want %q", cmd, got[cmd].Reason, reason)
		}
	}
	total := 0
	for _, r := range res.Reasons {
		total += r.Count
		if strings.Contains(r.Reason, "`") {
			t.Errorf("reason kind quotes a command: %q", r.Reason)
		}
	}
	if total != 5 {
		t.Errorf("reasons: %+v", res.Reasons)
	}

	// Every worktree of the repository counts.
	wt := filepath.Join(t.TempDir(), "wt")
	transcript(t, filepath.Join(projects, projectDirName(wt), "s.jsonl"), call{id: "50", cmd: "pnpm test", cwd: wt, at: now})
	res, _ = Discover(projects, []string{root, wt}, now.Add(-30*24*time.Hour), d)
	if res.Offloaded != 4 || res.Sessions != 3 {
		t.Fatalf("with a worktree: %+v", res)
	}
}

func TestRunsSpotexec(t *testing.T) {
	for c, want := range map[string]bool{
		"sx run -- pnpm test":               true,
		"cd x && spotexec run -- go test":   true,
		"./bin/spotexec status":             true,
		"timeout 60 sx pnpm test":           true,
		"go test ./cmd/spotexec":            false,
		"git log -- internal/sx":            false,
		"echo spotexec":                     false,
		"pnpm test | sx-filter":             false,
		"CI=1 /usr/local/bin/sx -- make ci": true,
	} {
		if got := runsSpotexec(c); got != want {
			t.Errorf("runsSpotexec(%q) = %v, want %v", c, got, want)
		}
	}
}

func TestProjectDirName(t *testing.T) {
	if got := projectDirName("/Users/me/Projects/my.repo_x"); got != "-Users-me-Projects-my-repo-x" {
		t.Fatal(got)
	}
}
