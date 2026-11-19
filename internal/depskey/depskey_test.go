package depskey

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

var synced = []string{
	"package-lock.json",
	"package.json",
	"packages/b/package.json",
	"packages/a/package.json",
	"packages/a/src/index.ts",
	"scripts/patchNodePty.cjs",
	"scripts/patchNodePtyWin.cjs",
	"scripts/build.mjs",
	"electron/native/foo/binding.gyp",
	"electron/native/foo/package.json",
	"electron/native/foo/src/foo.cc",
	"electron/native/foo/src/deep/util.h",
	"electron/native/bar/src/bar.cc",
	"docs/a1.md",
	"docs/a2.md",
	"docs/b1.md",
}

func TestExpandLiteralOnlyIsUnchanged(t *testing.T) {
	in := []string{"pnpm-lock.yaml", "a/package-lock.json", "pnpm-lock.yaml", "not/synced.lock"}
	out, unmatched, err := Expand(in, synced)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) || unmatched != nil {
		t.Fatalf("literal-only entries must pass through verbatim, got %v %v", out, unmatched)
	}
}

func TestExpand(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        []string
		want      []string
		unmatched []string
	}{
		{"star one level", []string{"packages/*/package.json"}, []string{"packages/a/package.json", "packages/b/package.json"}, nil},
		{"star prefix", []string{"scripts/patchNodePty*.cjs"}, []string{"scripts/patchNodePty.cjs", "scripts/patchNodePtyWin.cjs"}, nil},
		{"question", []string{"docs/a?.md"}, []string{"docs/a1.md", "docs/a2.md"}, nil},
		{"class", []string{"docs/[b-z]1.md"}, []string{"docs/b1.md"}, nil},
		{"star is single level", []string{"electron/native/*/src/*"}, []string{"electron/native/bar/src/bar.cc", "electron/native/foo/src/foo.cc"}, nil},
		{"doublestar recursive", []string{"electron/native/*/src/**"}, []string{"electron/native/bar/src/bar.cc", "electron/native/foo/src/deep/util.h", "electron/native/foo/src/foo.cc"}, nil},
		{"doublestar zero dirs", []string{"**/package.json"}, []string{"package.json", "packages/a/package.json", "packages/b/package.json", "electron/native/foo/package.json"}, nil},
		{"doublestar middle", []string{"electron/**/*.h"}, []string{"electron/native/foo/src/deep/util.h"}, nil},
		{"sorted union with literals, deduplicated", []string{"packages/a/package.json", "package-lock.json", "packages/*/package.json", "packages/a/*.json"}, []string{"package-lock.json", "packages/a/package.json", "packages/b/package.json"}, nil},
		{"unmatched pattern", []string{"package-lock.json", "addons/*/binding.gyp"}, []string{"package-lock.json"}, []string{"addons/*/binding.gyp"}},
		{"literal kept even if not synced", []string{"missing.lock", "docs/b*.md"}, []string{"docs/b1.md", "missing.lock"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, unmatched, err := Expand(tc.in, synced)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{}, tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(out, want) {
				t.Fatalf("got %v, want %v", out, want)
			}
			if !reflect.DeepEqual(unmatched, tc.unmatched) {
				t.Fatalf("unmatched %v, want %v", unmatched, tc.unmatched)
			}
		})
	}
}

func TestExpandRejectsEscapes(t *testing.T) {
	for _, in := range [][]string{
		{"../*.lock"},
		{"/etc/*"},
		{"a/../../*"},
		{"*/../x"},
		{".git/*"},
		{".GIT/**"},
		{"a//*"},
		{"./*"},
		{"docs/[a"},
		{"docs/\\*"},
		{"docs/*.md", "../package-lock.json"}, // literals next to a pattern are checked too
	} {
		if _, _, err := Expand(in, synced); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestMatchIsLinear(t *testing.T) {
	// Many "**" against a deep path must not blow up.
	pat := strings.Split(strings.Repeat("**/a/", 20)+"b", "/")
	name := strings.Split(strings.Repeat("a/", 60)+"c", "/")
	if match(pat, name) {
		t.Fatal("unexpected match")
	}
}

func spec(lockfiles ...string) Spec {
	return Spec{Lockfiles: lockfiles, Install: "npm ci", CacheDirs: []string{"node_modules"}, Toolchain: "tc", OS: "linux", Arch: "amd64"}
}

// The key for a literal-only config must never change: it would throw away
// every existing dependency cache.
func TestKeyLiteralGolden(t *testing.T) {
	root := writeTree(t, map[string]string{"package-lock.json": "{\"lockfileVersion\":3}\n", "packages/a/package.json": "{}\n"})
	in := []string{"package-lock.json", "packages/a/package.json"}
	lockfiles, _, err := Expand(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := Key(root, spec(lockfiles...))
	if err != nil {
		t.Fatal(err)
	}
	if want := "b059f7e99e985bf0cb4367e0"; key != want {
		t.Fatalf("literal key changed: %s, want %s", key, want)
	}
}

func TestKeyPatternStable(t *testing.T) {
	files := map[string]string{
		"package-lock.json":       "lock",
		"packages/a/package.json": "a",
		"packages/b/package.json": "b",
	}
	root := writeTree(t, files)
	var list []string
	for p := range files {
		list = append(list, p)
	}
	key := func(entries ...string) string {
		t.Helper()
		lf, _, err := Expand(entries, list)
		if err != nil {
			t.Fatal(err)
		}
		k, err := Key(root, spec(lf...))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k1 := key("package-lock.json", "packages/*/package.json")
	if k2 := key("packages/*/package.json", "package-lock.json"); k2 != k1 {
		t.Fatal("entry order changed the key")
	}
	if k3 := key("packages/b/package.json", "package-lock.json", "packages/*/package.json"); k3 != k1 {
		t.Fatal("a duplicate literal changed the key")
	}
	if k4 := key("package-lock.json", "packages/a/package.json", "packages/b/package.json", "packages/*/package.json"); k4 != k1 {
		t.Fatal("spelling the matches out alongside the pattern changed the key")
	}
	// A new workspace joins the key.
	if err := os.MkdirAll(filepath.Join(root, "packages", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packages", "c", "package.json"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	list = append(list, "packages/c/package.json")
	if key("package-lock.json", "packages/*/package.json") == k1 {
		t.Fatal("a newly matched file must change the key")
	}
}

// #315: the install's environment keys the cache, by name and value, in
// any map order; an empty one leaves the key as it was.
func TestKeyEnv(t *testing.T) {
	root := writeTree(t, map[string]string{"package-lock.json": "{\"lockfileVersion\":3}\n", "packages/a/package.json": "{}\n"})
	key := func(env map[string]string) string {
		t.Helper()
		s := spec("package-lock.json", "packages/a/package.json")
		s.Env = env
		k, err := Key(root, s)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if k := key(map[string]string{}); k != "b059f7e99e985bf0cb4367e0" {
		t.Fatalf("an empty env changed the key: %s", k)
	}
	prod := key(map[string]string{"NODE_ENV": "production"})
	if prod == key(nil) {
		t.Fatal("an env didn't change the key")
	}
	if key(map[string]string{"NODE_ENV": "development"}) == prod {
		t.Fatal("a changed value kept the key")
	}
	if key(map[string]string{"NODE_ENVX": "production"}) == prod {
		t.Fatal("a changed name kept the key")
	}
	if key(map[string]string{"A": "BC"}) == key(map[string]string{"AB": "C"}) {
		t.Fatal("name and value boundaries collide")
	}
	if key(map[string]string{"A": "", "B": ""}) == key(map[string]string{"A": "B"}) {
		t.Fatal("entry boundaries collide")
	}
	if key(map[string]string{"A": "1\x00B", "C": ""}) == key(map[string]string{"A": "", "B": "1\x00C"}) {
		t.Fatal("value boundaries collide")
	}
	// Go randomizes map iteration: many maps built in different orders
	// must all give one key.
	names := []string{"NODE_ENV", "TZ", "NODE_OPTIONS", "A", "Z", "M", "npm_config_production"}
	want := ""
	for i := range 50 {
		env := map[string]string{}
		for j := range names {
			n := names[(i+j)%len(names)]
			env[n] = "v-" + n
		}
		if k := key(env); want == "" {
			want = k
		} else if k != want {
			t.Fatalf("map order changed the key: %s, want %s", k, want)
		}
	}
	// Only a hash of the values reaches the key.
	if k := key(map[string]string{"TOKEN": "s3cr3t"}); strings.Contains(k, "s3cr3t") || len(k) != 24 {
		t.Fatalf("key %q", k)
	}
}

// The env section can't be spelled by the lockfile sections after it: an
// env of A and B (empty) against no env and lockfiles that would frame the
// same bytes under a length-prefixed encoding written inline.
func TestKeyEnvSectionIsFramed(t *testing.T) {
	root := writeTree(t, map[string]string{"2": "", "env": "1\x00", "A0": "B", "0": "0\x00"})
	base := Spec{Install: "i", OS: "linux", Arch: "amd64"}
	withEnv := base
	withEnv.Env, withEnv.Lockfiles = map[string]string{"A": "", "B": ""}, []string{"2"}
	noEnv := base
	noEnv.Lockfiles = []string{"env", "A0", "0"}
	a, err := Key(root, withEnv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key(root, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("an env collides with lockfiles")
	}
}
