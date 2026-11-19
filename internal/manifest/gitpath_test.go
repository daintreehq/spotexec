package manifest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGitPathKeepsSpaces(t *testing.T) {
	for in, want := range map[string]string{
		"/w/checkout \n": "/w/checkout ",
		" /w/lead\n":     " /w/lead",
		"/w/plain\n":     "/w/plain",
		"/w/no-newline":  "/w/no-newline",
		"/w/two\n\n":     "/w/two\n",
		"/w/tab\t\n":     "/w/tab\t",
		"":               "",
	} {
		if got := GitPath([]byte(in)); got != want {
			t.Errorf("GitPath(%q) = %q, want %q", in, got, want)
		}
	}
	// A carriage return is part of a POSIX name; Windows names never hold one.
	want := "/w/checkout\r"
	if runtime.GOOS == "windows" {
		want = "/w/checkout"
	}
	if got := GitPath([]byte("/w/checkout\r\n")); got != want {
		t.Errorf("GitPath(CRLF) = %q, want %q", got, want)
	}
}

// Root and GitDir name the checkout asked for when a sibling's name differs
// only by a trailing space.
func TestRootKeepsTrailingSpace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot end in a space")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	parent := t.TempDir()
	if r, err := filepath.EvalSymlinks(parent); err == nil {
		parent = r
	}
	for _, name := range []string{"checkout", "checkout "} {
		if err := os.MkdirAll(filepath.Join(parent, name, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, filepath.Join(parent, name), "init", "-q")
	}
	spaced := filepath.Join(parent, "checkout ")
	for _, dir := range []string{spaced, filepath.Join(spaced, "sub")} {
		if r, git := Root(dir); r != spaced || !git {
			t.Errorf("Root(%q) = %q, %v; want %q", dir, r, git, spaced)
		}
	}
	gd, err := GitDir(spaced)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(spaced, ".git"); gd != want {
		t.Errorf("GitDir = %q, want %q", gd, want)
	}
}
