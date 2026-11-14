package helper

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplacePaths(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"#!/a/ws/.venv/bin/python\n", "#!/n/.venv/bin/python\n"},
		{"VIRTUAL_ENV='/a/ws/.venv'", "VIRTUAL_ENV='/n/.venv'"},
		{"/a/ws", "/n"},
		{"/a/ws2/x /a/ws/x", "/a/ws2/x /n/x"}, // not a whole path prefix
		{`{"url": "file:///p/a/ws/src"}`, `{"url": "file:///n/src"}`}, // the longer spelling wins
		{"nothing here", "nothing here"},
	} {
		got, _ := replacePaths([]byte(c.in), []string{"/a/ws", "/p/a/ws"}, "/n")
		if string(got) != c.want {
			t.Errorf("replacePaths(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A venv built in one workspace and copied into another is rewritten to
// point only at the new one, and keeps working once the first is gone.
func TestRelocateVenv(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	wsA, wsB := filepath.Join(root, "slot-01", "workspace"), filepath.Join(root, "slot-02", "workspace")
	venvA := filepath.Join(wsA, ".venv")
	write := func(p, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	sp := filepath.Join(venvA, "lib", "python3.12", "site-packages")
	write(filepath.Join(venvA, "pyvenv.cfg"), "home = /usr/bin\n", 0o644)
	write(filepath.Join(venvA, "bin", "tool"), "#!/bin/sh\necho "+venvA+"/bin/tool\n", 0o755)
	write(filepath.Join(venvA, "bin", "activate"), "VIRTUAL_ENV='"+venvA+"'\nexport VIRTUAL_ENV\n", 0o644)
	write(filepath.Join(sp, "demo.pth"), wsA+"/src\n", 0o644)
	write(filepath.Join(sp, "__editable___demo_finder.py"), "MAPPING = {'demo': '"+wsA+"/src/demo'}\n", 0o644)
	write(filepath.Join(sp, "demo-0.1.dist-info", "direct_url.json"), `{"url":"file://`+wsA+`","dir_info":{"editable":true}}`, 0o644)
	write(filepath.Join(sp, "_editable_impl_demo.py"), "PATHS = {'demo': '"+wsA+"/src/demo'}\n", 0o644)
	write(filepath.Join(sp, "__pycache__", "_editable_impl_demo.cpython-312.pyc"), "stale", 0o644)
	write(filepath.Join(sp, "__pycache__", "six.cpython-312.pyc"), "keep", 0o644)
	write(filepath.Join(sp, "pkg", "data.txt"), wsA+"\n", 0o644) // not ours to touch

	for _, origins := range [][]string{{wsA}, nil} { // recorded, or read from activate
		dst := filepath.Join(wsB, ".venv")
		os.RemoveAll(wsB)
		if err := os.MkdirAll(wsB, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-R", "-p", venvA, dst).CombinedOutput(); err != nil {
			t.Fatalf("cp: %v %s", err, out)
		}
		if err := relocateVenvs(dst, ".venv", origins, wsB); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"bin/tool", "bin/activate", "lib/python3.12/site-packages/demo.pth",
			"lib/python3.12/site-packages/__editable___demo_finder.py", "lib/python3.12/site-packages/_editable_impl_demo.py",
			"lib/python3.12/site-packages/demo-0.1.dist-info/direct_url.json"} {
			b, err := os.ReadFile(filepath.Join(dst, f))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), wsA) || !strings.Contains(string(b), wsB) {
				t.Errorf("origins %v: %s not relocated:\n%s", origins, f, b)
			}
		}
		if fi, _ := os.Stat(filepath.Join(dst, "bin", "tool")); fi.Mode().Perm() != 0o755 {
			t.Errorf("bin/tool mode %v", fi.Mode())
		}
		pyc := filepath.Join(dst, "lib/python3.12/site-packages/__pycache__")
		if _, err := os.Stat(filepath.Join(pyc, "_editable_impl_demo.cpython-312.pyc")); !os.IsNotExist(err) {
			t.Error("bytecode of a rewritten module kept")
		}
		if _, err := os.Stat(filepath.Join(pyc, "six.cpython-312.pyc")); err != nil {
			t.Error("unrelated bytecode removed")
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "lib/python3.12/site-packages/pkg/data.txt")); !strings.Contains(string(b), wsA) {
			t.Error("rewrote a package data file")
		}
		out, err := exec.Command(filepath.Join(dst, "bin", "tool")).Output()
		if err != nil || strings.TrimSpace(string(out)) != filepath.Join(dst, "bin", "tool") {
			t.Errorf("relocated script ran as %q (%v)", out, err)
		}
	}
	// A hardlinked hydration must not write through to the cache.
	dst := filepath.Join(root, "slot-03", "workspace", ".venv")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if out, err := exec.Command("cp", "-R", "-p", venvA, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	pth := filepath.Join(dst, "lib/python3.12/site-packages/demo.pth")
	os.Remove(pth)
	if err := os.Link(filepath.Join(sp, "demo.pth"), pth); err != nil {
		t.Fatal(err)
	}
	if err := relocateVenvs(dst, ".venv", []string{wsA}, filepath.Dir(dst)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(sp, "demo.pth")); string(b) != wsA+"/src\n" {
		t.Fatalf("the source copy changed: %q", b)
	}
}

// A real virtualenv (python3 -m venv) relocated by the helper runs from the
// new slot after the original is deleted.
func TestRelocateRealVenv(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	wsA, wsB := filepath.Join(root, "a", "workspace"), filepath.Join(root, "b", "workspace")
	os.MkdirAll(filepath.Join(wsA, "src", "demo"), 0o755)
	os.MkdirAll(wsB, 0o755)
	os.WriteFile(filepath.Join(wsA, "src", "demo", "__init__.py"), []byte("WHERE = __file__\n"), 0o644)
	if out, err := exec.Command(py, "-m", "venv", "--without-pip", filepath.Join(wsA, ".venv")).CombinedOutput(); err != nil {
		t.Skipf("python3 -m venv: %v %s", err, out)
	}
	sites, _ := filepath.Glob(filepath.Join(wsA, ".venv", "lib", "python*", "site-packages"))
	if len(sites) != 1 {
		t.Skipf("unexpected venv layout: %v", sites)
	}
	os.WriteFile(filepath.Join(sites[0], "demo.pth"), []byte(filepath.Join(wsA, "src")+"\n"), 0o644)
	// The source tree is synced into every slot; the venv comes from the cache.
	os.MkdirAll(filepath.Join(wsB, "src", "demo"), 0o755)
	os.WriteFile(filepath.Join(wsB, "src", "demo", "__init__.py"), []byte("WHERE = __file__\n"), 0o644)
	if out, err := exec.Command("cp", "-R", "-p", filepath.Join(wsA, ".venv"), filepath.Join(wsB, ".venv")).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	if err := relocateVenvs(filepath.Join(wsB, ".venv"), ".venv", []string{wsA}, wsB); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wsA); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(wsB, ".venv", "bin", "python"), "-c", "import demo, sys; print(demo.WHERE); print(sys.prefix)").CombinedOutput()
	if err != nil {
		t.Fatalf("relocated venv: %v %s", err, out)
	}
	if strings.Contains(string(out), wsA) || !strings.Contains(string(out), filepath.Join(wsB, "src", "demo")) {
		t.Fatalf("relocated venv still points elsewhere:\n%s", out)
	}
}

// Linked files are materialised, never written through; a site-packages
// that is a symlink out of the venv is left alone.
func TestRelocateVenvLinks(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	wsA, wsB := filepath.Join(root, "a"), filepath.Join(root, "b")
	ext := filepath.Join(root, "uv-cache")
	write := func(p, body string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(ext, "demo.pth"), wsA+"/src\n")
	write(filepath.Join(ext, "other-site", "x.pth"), wsA+"/src\n")

	v := filepath.Join(wsB, ".venv")
	write(filepath.Join(v, "pyvenv.cfg"), "home = /usr/bin\n")
	sp := filepath.Join(v, "lib", "python3.12", "site-packages")
	os.MkdirAll(sp, 0o755)
	if err := os.Symlink(filepath.Join(ext, "demo.pth"), filepath.Join(sp, "demo.pth")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(v, "lib", "python3.13"), 0o755)
	if err := os.Symlink(filepath.Join(ext, "other-site"), filepath.Join(v, "lib", "python3.13", "site-packages")); err != nil {
		t.Fatal(err)
	}
	if err := relocateVenvs(v, ".venv", []string{wsA}, wsB); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(sp, "demo.pth"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("linked .pth not materialised: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(sp, "demo.pth")); string(b) != wsB+"/src\n" {
		t.Errorf("materialised .pth = %q", b)
	}
	for _, f := range []string{"demo.pth", "other-site/x.pth"} {
		if b, _ := os.ReadFile(filepath.Join(ext, f)); string(b) != wsA+"/src\n" {
			t.Errorf("wrote through a link into %s: %q", f, b)
		}
	}
}
