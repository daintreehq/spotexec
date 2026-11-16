package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/config"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"pnpm test":                          "pnpm test",
		"pnpm test  # quick run":             "pnpm test",
		"# only a comment":                   "",
		"pnpm test # it's fine":              "pnpm test",
		"pnpm test #; rm -rf / | tail":       "pnpm test",
		"echo ${#x} # length":                "echo ${#x}",
		"echo $#":                            "echo $#",
		"pytest -k 'a#b' # c":                "pytest -k 'a#b'",
		"pytest issue#3 # c":                 "pytest issue#3",
		"pytest \"a # b\" # c":               `pytest "a # b"`,
		"pnpm test;# c":                      "pnpm test",
		"cargo test \\\n  --workspace":       "cargo test   --workspace",
		"pnpm te\\\nst":                      "pnpm test",
		"echo \"a\\\nb\"":                    `echo "ab"`,
		"pnpm lint\npnpm test":               "pnpm lint; pnpm test",
		"pnpm lint;\npnpm test":              "pnpm lint; pnpm test",
		"pnpm build &&\n  pnpm test":         "pnpm build &&   pnpm test",
		"pnpm test ||\ntrue":                 "pnpm test || true",
		"pnpm test 2>&1 |\n  tail -50":       "pnpm test 2>&1 |   tail -50",
		"pnpm test\n\n\n":                    "pnpm test",
		"\n# setup\npnpm test # go\n":        "pnpm test",
		"pnpm test # note \\\npnpm lint":     "pnpm test; pnpm lint",
		"pnpm test \\# not a comment":        "pnpm test \\# not a comment",
		"pnpm test \\\\\npnpm lint":          "pnpm test \\\\; pnpm lint",
		"a\\#b # c":                          "a\\#b",
		"echo 'it''s' # c":                   "echo 'it''s'",
		"go test -run 'TestA|TestB' # x":     "go test -run 'TestA|TestB'",
		"pnpm lint # a\npnpm test # b":       "pnpm lint; pnpm test",
		"echo \"#\" # c":                     `echo "#"`,
		"pnpm test\n# trailing comment line": "pnpm test",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	// Left alone: a newline that is data, a here-document, a line that
	// starts with an operator, unbalanced quotes, a single-quoted
	// continuation.
	for _, in := range []string{
		"git commit -m 'a\nb'",
		"git commit -m \"a\nb\"",
		"cat <<EOF\nx\nEOF",
		"pnpm test <<< x # c",
		"pnpm test\n| tail",
		"pnpm test\n&& pnpm lint",
		"pnpm test\n;pnpm lint",
		"echo 'a\\\nb'",
		"echo 'unterminated # c",
		"prettier --write .;; # c",
		"pnpm test & # bg",
		"pnpm test; ; # c",
		"x=$(pnpm test # c\n)",
		"pnpm lint\r\npnpm test",
		"cargo test \\\r\n --release # c",
	} {
		if got := Normalize(in); got != in {
			t.Errorf("Normalize(%q) = %q, want it unchanged", in, got)
		}
	}
}

// A normalized line runs exactly like the original under bash.
func TestNormalizeRunsLikeOriginal(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	const fns = `oktest() { echo "test-ran $*"; }; failtest() { echo "test-failed $*"; return 3; }; oklint() { echo "lint-ran $*"; }
`
	for _, line := range []string{
		"oktest  # quick run",
		"oktest # note \\\noklint",
		"oktest a \\\n  b",
		"okte\\\nst",
		"oktest \"a\\\nb\"",
		"oklint\noktest",
		"failtest\noktest",
		"failtest ||\noktest",
		"oktest &&\n  oklint x",
		"oktest 2>&1 |\n  tr a-z A-Z",
		"\n# setup\noktest # go\n",
		"oktest;\n oklint",
		"oktest \\# x",
		"oktest 'a # b' # c",
		"oktest ${#HOME} # length",
		"failtest\necho \"exit=$?\"",
	} {
		norm := Normalize(line)
		if strings.Contains(norm, "\n") {
			t.Errorf("Normalize(%q) = %q still has a newline", line, norm)
		}
		run := func(script string) string {
			out, _ := exec.Command(bash, "-c", fns+script+"\necho \"status=$?\"").CombinedOutput()
			return string(out)
		}
		if want, got := run(line), run(norm); got != want {
			t.Errorf("%q -> %q\ngot:  %q\nwant: %q", line, norm, got, want)
		}
	}
}

func TestShouldOffloadNormalized(t *testing.T) {
	d, _ := NewDecider(config.Hook{})
	for _, c := range []string{
		"pnpm test # quick", "cargo test \\\n  --workspace", "pnpm lint\npnpm test",
		"pnpm test # off: ; pnpm format | tail -50", "pnpm test 2>&1 |\n  tail -50",
		"pytest tests/ -x  # quick run",
	} {
		if n := Normalize(c); !d.ShouldOffload(n) {
			t.Errorf("expected offload: %q (normalized %q)", c, n)
		}
	}
	for _, c := range []string{
		"pnpm test # c\ngit push", "for f in a b\ndo\npnpm test\ndone", "if pnpm test\nthen echo ok\nfi",
		"cat <<EOF | pnpm test\nx\nEOF", "pnpm test\n| tail", "# pnpm test", "pnpm test &\n",
	} {
		if n := Normalize(c); d.ShouldOffload(n) {
			t.Errorf("expected local: %q (normalized %q)", c, n)
		}
	}
}

func TestShouldOffloadWrappers(t *testing.T) {
	d, err := NewDecider(config.Hook{TransparentPrefixes: []string{"direnv  exec .", "op run --env-file=.env --"}})
	if err != nil {
		t.Fatal(err)
	}
	yes := []string{
		"timeout 600 cargo test", "timeout 10m pnpm test", "timeout 1.5h pytest",
		"timeout -s KILL 10m pnpm test", "timeout -sKILL 10 pnpm test", "timeout --signal=TERM -k 5 60 go test ./...",
		"timeout --preserve-status 5 go test ./...", "time go test ./...", "time -p pytest",
		"env CI=1 pnpm test", "env pnpm test", "env CI=1 FOO='a b' pnpm test", "nice -n 10 cargo build",
		"nice -5 cargo build", "nice -n5 cargo build", "nice --adjustment=3 cargo build", "nice cargo build",
		"nohup pytest", "command cargo test", "CI=1 timeout 60 pnpm test",
		"timeout 600 env CI=1 pnpm test 2>&1 | tail -50", "time timeout 60 nice cargo test",
		"direnv exec . pnpm test", "op run --env-file=.env -- pytest -x",
		"cd sub && timeout 300 pnpm test", "timeout 600 cargo test; timeout 600 cargo clippy",
	}
	no := []string{
		"timeout cargo test", "timeout 600", "timeout 600 git status", "timeout -x 5 cargo test",
		"env -i pnpm test", "env", "command -v pytest", "CI=1 time cargo test", "sudo cargo test",
		"nice --foo cargo test", "nice -n x cargo test", "timeout 600 pnpm dev", "nohup", "xargs pytest",
		"direnv exec .", "direnv exec ./x pnpm test", "timeout 600 spotexec run -- pnpm test",
		"env SPOTEXEC_LOCAL=1 pnpm test", "timeout $T cargo test", "timeout 600 cargo test > out.log",
		"stdbuf -oL pytest",
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
	plain, _ := NewDecider(config.Hook{})
	if plain.ShouldOffload("direnv exec . pnpm test") {
		t.Error("an unconfigured prefix must stay local")
	}
	for _, bad := range []string{"", "  ", "a; b", "a | b", "x $(y)", "a # b"} {
		if _, err := NewDecider(config.Hook{TransparentPrefixes: []string{bad}}); err == nil {
			t.Errorf("transparent_prefixes %q accepted", bad)
		}
	}
}

func TestShouldOffloadToolForms(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	d, _ := NewDecider(config.Hook{})
	d.root = root
	yes := []string{
		"pnpm --filter web test", "pnpm -F web test", "pnpm -r test", "pnpm --filter=web --parallel run lint",
		"pnpm -C packages/api test", "pnpm --dir=packages/api test", "pnpm -Cpackages/api test",
		"pnpm --filter web exec vitest run", "pnpm --filter './packages/**' test", "pnpm -r --if-present run build",
		"npm --prefix packages/api test", "npm -w web run test", "npm --workspaces test",
		"yarn workspace web test", "yarn --cwd web test", "bun --filter web test", "bun --cwd web run test",
		"npx --yes vitest run", "npx -y prettier --check .", "npx -p typescript tsc --noEmit",
		"make -C sub test", "make -j8 test", "make -j 8 -k test", "make -j test", "make --no-print-directory -s lint",
		"make -f Makefile.ci test", "just -f ci.just test",
		"go -C sub test ./...", "cargo --locked test", "cargo -q clippy", "cargo --color never build",
		"uv run --frozen pytest", "uv run --with pytest-xdist pytest -n auto", "uv run -p 3.12 mypy src",
		"./node_modules/.bin/vitest run", "node_modules/.bin/tsc --noEmit", ".venv/bin/pytest -x",
		".venv/bin/python -m pytest", "venv/bin/ruff check .", "bin/pytest",
		"gradle :app:test", "./gradlew :core:check", "./gradlew :a:b:test --tests Foo", "gradlew build",
		"mvnw verify", root + "/.venv/bin/pytest -x", root + "/node_modules/.bin/vitest run",
		"make -C " + root + "/sub test",
	}
	no := []string{
		"pnpm --filter web dev", "pnpm --filter web start", "pnpm -C ../other test", "pnpm -C /tmp test",
		"pnpm --dir=../x test", "make -C ../x test", "make -C /tmp test", "go -C /tmp test", "go -C ../x test",
		"pnpm --unknown test", "pnpm --filter", "/usr/local/bin/pytest -x", "../venv/bin/pytest",
		"~/bin/pytest", "$HOME/bin/pytest", "npx --yes vitest watch", "yarn workspace web dev",
		"yarn workspace web", "npm --prefix /tmp test", "pnpm --color=always test", "make -C $D test",
		"./bin/spotexec run -- pnpm test", "node_modules/.bin/vitest watch", "/opt/homebrew/bin/cargo test",
		"pnpm -C sub/../../x test", "uv run --unknown pytest", "uv run --frozen",
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
	// Without a root, only relative paths count.
	d.root = ""
	if d.ShouldOffload(root + "/.venv/bin/pytest") {
		t.Error("an absolute path counted without a root")
	}
}

func TestShouldOffloadListsAndPipes(t *testing.T) {
	d, _ := NewDecider(config.Hook{})
	yes := []string{
		"pnpm test || true", "pnpm test || :", "pnpm test || echo failed", `cargo test; echo "exit=$?"`,
		"cargo test; echo exit=$?", "pnpm test && echo ok", "echo start && pnpm test", "cargo test |& tail -50",
		"cd sub; pnpm test || true", "pnpm test || pnpm test", "pnpm test 2>&1 || true",
		"cargo test 2>&1 |& tail", "pnpm test | grep x |& tail", `printf 'done\n'; pnpm test`,
		"pnpm lint && pnpm test || echo 'tests failed'",
		"pnpm test && pnpm lint |& tail", "pnpm test && pnpm lint | tail -50", "pnpm build; pnpm test | tail",
		"go build ./... && go vet ./... && go test ./... 2>&1 | tail -6", "cd sub && go vet ./... && go test | grep FAIL | head -3",
		"make build >/dev/null 2>&1 && pnpm test", "pnpm test 2>/dev/null", "cargo test &>/dev/null; echo $?",
		"pnpm test > /dev/null 2>&1 | tail", "go test ./... 2>&1 >/dev/null", "pnpm test >>/dev/null",
		// Pipes inside the list, into read-only filters.
		`go vet ./... 2>&1 | grep -c "run.go"; go build ./... && echo ok`, "pnpm lint | tail -3 && pnpm test",
		"cd sub && go vet |& grep x && go test", "pnpm test | head -1 || true",
		// Quick commands riding along with heavy ones.
		"gofmt -l internal && go build ./... && go vet ./... && go test ./... 2>&1 | tail -6",
		"gofmt -l . ; go test ./...", "go fmt ./... && go test ./...", "goimports -l . && go vet ./...",
	}
	no := []string{
		"cd x || pnpm test", "cd x && pnpm test || true", "cd x && pnpm test; true", "pnpm test || git push",
		"echo $HOME && pnpm test", "pnpm test; echo $(date)", "true", "echo hi", "pnpm test && echo x > f",
		"pnpm test || || true", "pnpm test ||", "pnpm test |& xargs echo",
		"pnpm test |&", `pnpm test; echo "$HOME"`, "pnpm test; echo `date`", "true && cd x && pnpm test",
		"pnpm test > out.txt", "pnpm test 2>err.log", "pnpm test >/dev/nullx", "pnpm test > /dev/null.log",
		"pnpm build && pnpm test | tee log", "pnpm build && pnpm test | jq .", "pnpm build && pnpm test | sort -o f",
		"pnpm test </dev/null",
		"pnpm lint | tee log && pnpm test", "pnpm lint | xargs echo; pnpm test", "cd sub | tail && pnpm test",
		"cd sub && pnpm test | tail; pnpm lint", "pnpm lint | grep x > f; pnpm test", "pnpm lint | ; pnpm test",
		"gofmt -l .", "gofmt -l $DIR && go test", "go fmt ./...",
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

func TestWrapListsPipesAndWrappers(t *testing.T) {
	for in, want := range map[string]string{
		"cargo test |& tail -50":                             "sx run -- 'cargo test 2>&1' | tail -50",
		"cargo test 2>&1 |& tail":                            "sx run -- 'cargo test 2>&1' | tail",
		"pnpm test | grep x |& tail":                         "sx run -- 'pnpm test' | grep x |& tail",
		"cd x; pnpm test || true":                            "cd x; sx run -- 'pnpm test || true'",
		`cargo test; echo "exit=$?"`:                         `sx run -- 'cargo test; echo "exit=$?"'`,
		"timeout 600 pnpm test":                              "sx run -- 'timeout 600 pnpm test'",
		"cd x && cargo test |& tail":                         "cd x && sx run -- 'cargo test 2>&1' | tail",
		"pnpm --filter web test || :":                        "sx run -- 'pnpm --filter web test || :'",
		"go vet ./... && go test ./... 2>&1 | tail -6":       "sx run -- 'go vet ./... && go test ./... 2>&1 | tail -6'",
		"cd sub && go vet && go test |& grep FAIL | head -3": "cd sub && sx run -- 'go vet && go test 2>&1 | grep FAIL | head -3'",
		"make build >/dev/null 2>&1 && pnpm test":            "sx run -- 'make build >/dev/null 2>&1 && pnpm test'",
		"go vet ./... | grep -c x; go test ./...":            "sx run -- 'go vet ./... | grep -c x; go test ./...'",
		"cd sub && go vet |& grep x && go test":              "cd sub && sx run -- 'go vet |& grep x && go test'",
	} {
		if got := Wrap("sx", in); got != want {
			t.Errorf("Wrap(%q) = %q, want %q", in, got, want)
		}
	}
	// RTK's filters see past wrappers.
	for in, want := range map[string]string{
		"timeout 600 go test ./...":   "sx run --rtk-filter go-test -- 'timeout 600 go test -json ./...'",
		"time cargo test":             "sx run --rtk-filter cargo-test -- 'time cargo test'",
		"env CI=1 go test ./... 2>&1": "sx run --rtk-filter go-test -- 'env CI=1 go test -json ./...'",
	} {
		if got := WrapRTK("sx", in, true); got != want {
			t.Errorf("WrapRTK(%q) = %q, want %q", in, got, want)
		}
	}
}

// Wrapped lines with || and trailers run like the originals, under bash
// (for |&), with functions standing in for the heavy commands and spotexec.
func TestWrapListsRunLikeOriginal(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	// |& needs bash 4 (macOS's /bin/bash is 3.2); zsh has it too.
	pipeAmp := ""
	for _, sh := range []string{bash, "/opt/homebrew/bin/bash", "/usr/local/bin/bash", "zsh"} {
		if exec.Command(sh, "-c", "true |& true").Run() == nil {
			pipeAmp = sh
			break
		}
	}
	const fns = `oktest() { echo test-ran; }; failtest() { echo test-failed; return 3; }; errtest() { echo to-err >&2; return 2; }; `
	for _, line := range []string{
		"failtest || echo recovered",
		"oktest || echo recovered",
		"cd sub; failtest || true",
		`oktest; echo "exit=$?"`,
		`failtest; echo "exit=$?"`,
		"errtest |& grep err",
		"cd sub && errtest |& tr a-z A-Z",
		"failtest || failtest || oktest",
		"oktest && failtest || echo either",
		"oktest && failtest | tr a-z A-Z",
		"failtest; oktest | tail -1",
		"cd sub && oktest && errtest 2>&1 | grep err | head -1",
		"oktest >/dev/null 2>&1 && failtest 2>/dev/null",
		"failtest | grep -c test; oktest && echo ok",
		"cd sub && errtest |& grep -c err && oktest",
		"oktest | head -1 || true",
	} {
		sh := bash
		if strings.Contains(line, "|&") {
			if pipeAmp == "" {
				t.Logf("no shell with |&: skipping %q", line)
				continue
			}
			sh = pipeAmp
		}
		run := func(cmdline string) string {
			dir := t.TempDir()
			os.Mkdir(filepath.Join(dir, "sub"), 0o755)
			script := fns + `fakesx() { shift 2; (eval "$1"); }
` + cmdline + `
echo "status=$? pwd=${PWD##*/}"`
			c := exec.Command(sh, "-c", script)
			c.Dir = dir
			out, _ := c.CombinedOutput()
			return strings.ReplaceAll(string(out), filepath.Base(dir), "ROOT")
		}
		wrapped := Wrap("fakesx", line)
		if !strings.Contains(wrapped, "fakesx run -- ") {
			t.Errorf("%q wasn't offloaded: %q", line, wrapped)
		}
		if want, got := run(line), run(wrapped); got != want {
			t.Errorf("%q -> %q\ngot:  %q\nwant: %q", line, wrapped, got, want)
		}
	}
}

func TestSplitPrefix(t *testing.T) {
	for in, want := range map[string][2]string{
		"timeout 600 cargo test":          {"timeout 600 ", "cargo test"},
		"CI=1 env FOO=2 nice -n 5 pytest": {"CI=1 env FOO=2 nice -n 5 ", "pytest"},
		"cargo test":                      {"", "cargo test"},
		"time -p go test":                 {"time -p ", "go test"},
		"env -i x":                        {"", "env -i x"},
	} {
		p, c, ok := splitPrefix(in, nil)
		if !ok || p != want[0] || c != want[1] || p+c != in {
			t.Errorf("splitPrefix(%q) = %q, %q, %v; want %q, %q", in, p, c, ok, want[0], want[1])
		}
	}
	if _, _, ok := splitPrefix("timeout 5 ", nil); ok {
		t.Error("a wrapper with nothing to run")
	}
	got := readings("CI=1 timeout 5 pnpm test", nil)
	want := []string{"CI=1 timeout 5 pnpm test", "timeout 5 pnpm test", "pnpm test"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("readings = %q, want %q", got, want)
	}
}
