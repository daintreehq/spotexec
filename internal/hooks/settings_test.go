package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if err := os.WriteFile(p, []byte(sentinelSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o200); err != nil { // write-only, whatever the umask
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
		t.Errorf("install over an unreadable file: changed %v, err %v", ok, err)
	}
	if ok, err := UninstallClaude(p); err == nil || ok {
		t.Errorf("uninstall over an unreadable file: changed %v, err %v", ok, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o200 {
		t.Fatalf("mode changed: %v %v", fi.Mode(), err)
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
		`{"a":1} {"b":2}`,
		`{"hooks":`,
		`{"hooks":null}`,
		`{"hooks":{"PreToolUse":null}}`,
	} {
		p := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
			t.Errorf("install over %s: changed %v, err %v", doc, ok, err)
		}
		if ok, err := UninstallClaude(p); err == nil || ok {
			t.Errorf("uninstall over %s: changed %v, err %v", doc, ok, err)
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
	if ok, err := InstallClaude(p, "spotexec hook claude"); ok || err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("install: changed %v, err %v", ok, err)
	}
	unchanged(t, p, sentinelSettings)
	noTemps(t, p)

	renameFile = old
	if _, err := InstallClaude(p, "spotexec hook claude"); err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(p)
	renameFile = func(string, string) error { return errors.New("injected") }
	if ok, err := UninstallClaude(p); ok || err == nil {
		t.Fatalf("uninstall over a failed replacement: changed %v, err %v", ok, err)
	}
	unchanged(t, p, string(installed))
	noTemps(t, p)
}

func TestInstallPreservesSettingsMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	if err := os.WriteFile(p, []byte(sentinelSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	// Bits the umask would strip from a new file.
	if err := os.Chmod(p, 0o664); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o664 {
		t.Fatalf("mode after install: %v %v", fi.Mode(), err)
	}
	if ok, err := UninstallClaude(p); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o664 {
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
	if ok, err := UninstallClaude(link); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("uninstall replaced the settings symlink: %v", err)
	}
	if HasClaude(target) {
		t.Fatal("uninstall left the hook in the symlink's target")
	}
	noTemps(t, target)
}

func TestUninstallBlankSettings(t *testing.T) {
	for _, blank := range []string{"", "  \n"} {
		p := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(p, []byte(blank), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, err := UninstallClaude(p); err != nil || ok {
			t.Errorf("uninstall from blank %q: changed %v, err %v", blank, ok, err)
		}
		unchanged(t, p, blank)
	}
}

func TestInstallRefusesReadOnlySettings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(sentinelSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
		t.Errorf("install over a read-only file: changed %v, err %v", ok, err)
	}
	// Uninstall needs a hook to remove before it tries to save.
	withHook := `{"sentinel":"keep me","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"spotexec hook claude"}]}]}}`
	q := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(q, []byte(withHook), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(q, 0o400); err != nil {
		t.Fatal(err)
	}
	if ok, err := UninstallClaude(q); err == nil || ok {
		t.Errorf("uninstall over a read-only file: changed %v, err %v", ok, err)
	}
	unchanged(t, q, withHook)
	unchanged(t, p, sentinelSettings)
	noTemps(t, p)
}

func TestInstallRefusesDanglingSettingsLink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Symlink("missing.json", p); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
		t.Errorf("install through a dangling link: changed %v, err %v", ok, err)
	}
	if ok, err := UninstallClaude(p); err != nil || ok {
		t.Errorf("uninstall through a dangling link: changed %v, err %v", ok, err)
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the dangling link was replaced: %v", err)
	}
	noTemps(t, p)
}

func TestUninstallRefusesSettingsLinkLoop(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.Symlink("other.json", p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("settings.json", filepath.Join(dir, "other.json")); err != nil {
		t.Fatal(err)
	}
	if ok, err := UninstallClaude(p); err == nil || ok {
		t.Errorf("uninstall through a link loop: changed %v, err %v", ok, err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
		t.Errorf("install through a link loop: changed %v, err %v", ok, err)
	}
}

func TestInstallRefusesSettingsChangedMeanwhile(t *testing.T) {
	t.Cleanup(func() { beforeReplace = func() {} })
	for _, start := range []string{sentinelSettings, ""} {
		p := filepath.Join(t.TempDir(), "settings.json")
		if start != "" {
			if err := os.WriteFile(p, []byte(start), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		const edited = `{"edited":true,"hooks":{"Stop":[]}}`
		beforeReplace = func() {
			// An editor's atomic save: a new file renamed over the old.
			tmp := p + ".editor"
			if err := os.WriteFile(tmp, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, p); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := InstallClaude(p, "spotexec hook claude"); err == nil || ok {
			t.Errorf("install over a file saved meanwhile (from %q): changed %v, err %v", start, ok, err)
		}
		unchanged(t, p, edited)
		noTemps(t, p)
	}
}

func TestInstallKeepsNumbersAsWritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(`{"big":9007199254740993,"f":1.50}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := UninstallClaude(p); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"big": 9007199254740993,`) || !strings.Contains(string(b), `"f": 1.50`) {
		t.Fatalf("numbers changed:\n%s", b)
	}
}

func TestInstallNewSettingsHonoursUmask(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	for umask, want := range map[int]os.FileMode{0o027: 0o640, 0o022: 0o644} {
		syscall.Umask(umask)
		p := filepath.Join(t.TempDir(), ".claude", "settings.json")
		if ok, err := InstallClaude(p, "spotexec hook claude"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("new settings under umask %o: %v %v, want %v", umask, fi.Mode(), err, want)
		}
		noTemps(t, p)
	}
}
