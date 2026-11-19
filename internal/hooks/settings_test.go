package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sentinelSettings = `{"sentinel":"keep me","hooks":{"Stop":[]}}`

// unchanged fails unless p still holds want, byte for byte.
func unchanged(t *testing.T, p, want string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("settings changed:\n%s", b)
	}
}

// noTemps fails if a replacement left a temporary file beside p.
func noTemps(t *testing.T, p string) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != filepath.Base(p) {
			t.Fatalf("left %s beside the settings", e.Name())
		}
	}
}

func TestInstallRefusesUnreadableSettings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a write-only file")
	}
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(sentinelSettings), 0o200); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
		t.Fatalf("install over an unreadable file: changed %v, err %v", ok, err)
	}
	if ok, err := UninstallClaude(p); err == nil || ok {
		t.Fatalf("uninstall over an unreadable file: changed %v, err %v", ok, err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged(t, p, sentinelSettings)
}

func TestInstallRefusesNonObjectSettings(t *testing.T) {
	for _, doc := range []string{
		`null`,
		`[]`,
		`"x"`,
		`{"hooks":[]}`,
		`{"hooks":{"PreToolUse":{}}}`,
	} {
		p := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
			t.Errorf("install over %s: changed %v, err %v", doc, ok, err)
		}
		unchanged(t, p, doc)
	}
}

func TestInstallKeepsSettingsWhenReplaceFails(t *testing.T) {
	old := renameFile
	t.Cleanup(func() { renameFile = old })
	renameFile = func(string, string) error { return errors.New("injected") }

	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(sentinelSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallClaude(p, "spotexec hook claude"); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("install: %v", err)
	}
	unchanged(t, p, sentinelSettings)
	noTemps(t, p)

	renameFile = old
	if _, err := InstallClaude(p, "spotexec hook claude"); err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(p)
	renameFile = func(string, string) error { return errors.New("injected") }
	if _, err := UninstallClaude(p); err == nil {
		t.Fatal("uninstall: no error from a failed replacement")
	}
	unchanged(t, p, string(installed))
	noTemps(t, p)
}

func TestInstallPreservesSettingsMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(sentinelSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after install: %v %v", fi.Mode(), err)
	}
	if ok, err := UninstallClaude(p); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after uninstall: %v %v", fi.Mode(), err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"sentinel": "keep me"`) {
		t.Fatalf("lost the sentinel:\n%s", b)
	}
}

func TestInstallFollowsSettingsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(sentinelSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../dotfiles/settings.json", link); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(link, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the settings symlink was replaced: %v %v", fi.Mode(), err)
	}
	if !HasClaude(target) {
		t.Fatal("the symlink's target has no hook")
	}
}

func TestUninstallBlankSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := UninstallClaude(p); err != nil || ok {
		t.Fatalf("uninstall from a blank file: changed %v, err %v", ok, err)
	}
	unchanged(t, p, "  \n")
}
