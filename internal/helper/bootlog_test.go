package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLatestBootLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "boot.log")
	if latestBootLine(p) != "" {
		t.Fatal("missing log")
	}
	os.WriteFile(p, []byte("spotexec: running bootstrap_script\nGet:1 10%\rGet:1 100%\r\n\n  \n"), 0o640)
	if got := latestBootLine(p); got != "Get:1 100%" {
		t.Fatalf("got %q", got)
	}
	os.WriteFile(p, []byte(strings.Repeat("filler line\n", 1000)+"spotexec: running user_bootstrap_script\n"), 0o640)
	if got := latestBootLine(p); got != "spotexec: running user_bootstrap_script" {
		t.Fatalf("got %q", got)
	}
}
