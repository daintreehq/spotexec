package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/client"
)

func TestScaffoldDockerfileStacks(t *testing.T) {
	// The CLI tools GitHub's Ubuntu runners provide that every image gets,
	// listed here rather than read from runnerTools so a removal is caught.
	wantTools := []string{"bzip2", "ca-certificates", "curl", "file", "git", "jq", "lsof", "make",
		"openssh-client", "procps", "rsync", "sqlite3", "unzip", "wget", "xz-utils", "zip", "zsh"}
	cases := []struct {
		files map[string]string
		from  string
		has   string
		stack string
	}{
		{map[string]string{"package.json": `{"packageManager":"pnpm@9.15.0+sha512.abc","engines":{"node":">=20"}}`, "pnpm-lock.yaml": ""}, "FROM node:20-trixie", "pnpm@9.15.0 ", "Node.js 20, pnpm 9.15.0"},
		{map[string]string{"package.json": `{}`, "package-lock.json": "", ".nvmrc": "v18.19.0\n"}, "FROM node:18.19.0-bookworm", "", "Node.js 18.19.0"},
		{map[string]string{"package.json": `{}`, "package-lock.json": "", ".nvmrc": "lts/iron\n"}, "FROM node:20-trixie", "", "Node.js 20"},
		{map[string]string{"package.json": `{}`, "package-lock.json": "", ".nvmrc": "lts/*"}, "FROM node:lts-trixie", "", "Node.js lts"},
		{map[string]string{"package.json": `{"volta":{"node":"22.11.0"}}`, "package-lock.json": ""}, "FROM node:22.11.0-bookworm", "", "Node.js 22.11.0"},
		{map[string]string{"package.json": `{"engines":{"node":"20.17.0"}}`, "package-lock.json": ""}, "FROM node:20.17.0-bookworm", "", "Node.js 20.17.0"},
		{map[string]string{"package.json": `{}`, "package-lock.json": "", ".tool-versions": "nodejs 21.7.3\npython 3.12.1\n"}, "FROM node:21.7.3-bookworm", "", "Node.js 21.7.3"},
		{map[string]string{"package.json": `{"packageManager":"npm@10.8.2"}`, "package-lock.json": ""}, "FROM node:24-trixie", "npm@10.8.2", "Node.js 24, npm 10.8.2"},
		{map[string]string{"package.json": `{"packageManager":"yarn@4.5.1"}`, "yarn.lock": "", ".node-version": "20.11.1"}, "FROM node:20.11.1-bookworm", "corepack install -g yarn@4.5.1", "Node.js 20.11.1, Yarn 4.5.1"},
		{map[string]string{"package.json": `{}`, "yarn.lock": ""}, "FROM node:24-trixie", "yarn@1 ", "Node.js 24, Yarn"},
		{map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\n"}, "FROM node:24-trixie", "pnpm@10 ", "Node.js 24, pnpm 10"},
		{map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: '6.0'\n"}, "FROM node:24-trixie", "pnpm@8 ", "Node.js 24, pnpm 8"},
		{map[string]string{"package.json": `{}`, "bun.lock": ""}, "FROM oven/bun:1", "git", "Bun 1"},
		{map[string]string{"package.json": `{"packageManager":"bun@1.1.38"}`, "bun.lock": ""}, "FROM oven/bun:1.1.38", "", "Bun 1.1.38"},
		{map[string]string{"uv.lock": "", ".python-version": "3.11.4\n"}, "FROM python:3.11.4-bookworm", "COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx", "Python 3.11.4, uv"},
		{map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nrequires-python = \">=3.10\"\n[tool.uv]\nrequired-version = \"==0.5.4\"\n"}, "FROM python:3.10-trixie", "ghcr.io/astral-sh/uv:0.5.4 ", "Python 3.10, uv"},
		{map[string]string{"requirements.txt": ""}, "FROM python:3.12-trixie", "", "Python 3.12"},
		{map[string]string{"go.mod": "module x\n\ngo 1.25.3\n"}, "FROM golang:1.25.3-trixie", "GOMODCACHE=/home/spotexec", "Go 1.25.3"},
		{map[string]string{"go.mod": "module x\n\ngo 1.22\n\ntoolchain go1.23.4\n"}, "FROM golang:1.23.4-bookworm", "", "Go 1.23.4"},
		{map[string]string{"Cargo.toml": "", "rust-toolchain.toml": "[toolchain]\nchannel = \"1.83.0\"\n"}, "FROM rust:1.83.0-bookworm", "CARGO_HOME=/home/spotexec", "Rust 1.83.0"},
		// Exact versions take trixie once the official image publishes it.
		{map[string]string{"package.json": `{}`, "package-lock.json": "", ".nvmrc": "22.18.0\n"}, "FROM node:22.18.0-trixie", "", "Node.js 22.18.0"},
		{map[string]string{"requirements.txt": "", ".python-version": "3.12.11\n"}, "FROM python:3.12.11-trixie", "", "Python 3.12.11"},
		{map[string]string{"requirements.txt": "", ".python-version": "3.12.10\n"}, "FROM python:3.12.10-bookworm", "", "Python 3.12.10"},
		{map[string]string{"Cargo.toml": "", "rust-toolchain.toml": "[toolchain]\nchannel = \"1.88.0\"\n"}, "FROM rust:1.88.0-trixie", "", "Rust 1.88.0"},
		{map[string]string{"Cargo.toml": ""}, "FROM rust:1-trixie", "", "Rust 1"},
		{map[string]string{"go.mod": "module x\n\ngo 1.25.3\n", "package.json": `{}`, "package-lock.json": "", ".nvmrc": "18.19.0"}, "FROM node:18.19.0-bookworm", "COPY --from=golang:1.25.3-trixie /usr/local/go", "Node.js 18.19.0 + Go 1.25.3"},
		{map[string]string{"go.mod": "module x\n\ngo 1.24.2\n", "package.json": `{}`, "package-lock.json": ""}, "FROM node:24-trixie", "COPY --from=golang:1.24.2-bookworm /usr/local/go", "Node.js 24 + Go 1.24.2"},
		{map[string]string{"pom.xml": "<properties><maven.compiler.release>17</maven.compiler.release></properties>"}, "FROM eclipse-temurin:17-jdk", " maven ", "Java 17, Maven"},
		{map[string]string{"build.gradle.kts": "java { toolchain { languageVersion = JavaLanguageVersion.of(23) } }"}, "FROM eclipse-temurin:23-jdk", "", "Java 23, Gradle wrapper"},
		{map[string]string{"pom.xml": "", "mvnw": "", ".sdkmanrc": "java=21.0.2-tem\n"}, "FROM eclipse-temurin:21-jdk", "", "Java 21, Maven wrapper"},
		{map[string]string{".java-version": "1.8.0_402\n", "build.gradle": ""}, "FROM eclipse-temurin:8-jdk", "", "Java 8, Gradle wrapper"},
		{map[string]string{"go.mod": "module x\n\ngo 1.24.2 // minimum\n\ntoolchain go1.25.4 // pinned\n"}, "FROM golang:1.25.4-trixie", "", "Go 1.25.4"},
		{map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nrequires-python = \"==3.11.*\"\n"}, "FROM python:3.11-trixie", "", "Python 3.11, uv"},
		{map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nrequires-python = \"~=3.10.2\"\n"}, "FROM python:3.10-trixie", "", "Python 3.10, uv"},
		// packageManager wins over a leftover lockfile, here and in detectProject.
		{map[string]string{"package.json": `{"packageManager":"yarn@4.5.1"}`, "pnpm-lock.yaml": "", "yarn.lock": ""}, "FROM node:24-trixie", "corepack install -g yarn@4.5.1", "Node.js 24, Yarn 4.5.1"},
		// A Go module with package.json tooling gets Go too; uv likewise.
		{map[string]string{"go.mod": "module x\n\ngo 1.25.3\n", "package.json": `{}`, "package-lock.json": ""}, "FROM node:24-trixie", "COPY --from=golang:1.25.3-trixie /usr/local/go /usr/local/go", "Node.js 24 + Go 1.25.3"},
		{map[string]string{"package.json": `{}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\n", "uv.lock": "", ".python-version": "3.12.7"}, "FROM node:24-trixie", "RUN uv python install 3.12.7", "Node.js 24, pnpm 10 + uv, Python 3.12.7"},
		{map[string]string{"package.json": `{"packageManager":"pnpm@9.15.0"}`, "pnpm-lock.yaml": "", "bun.lock": ""}, "FROM node:24-trixie", "pnpm@9.15.0", "Node.js 24, pnpm 9.15.0"},
		{map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nrequires-python = \"==3.12.7\"\n"}, "FROM python:3.12.7-bookworm", "", "Python 3.12.7, uv"},
		{map[string]string{"Cargo.toml": "", "package.json": `{}`}, "FROM node:24-trixie", "also uses Rust", "Node.js 24"},
		{map[string]string{}, "FROM ubuntu:24.04", "build-essential", ""},
		// Electron, Playwright and Puppeteer get their system libraries; the
		// stack is unchanged.
		{map[string]string{"package.json": `{"devDependencies":{"electron":"^38"}}`, "package-lock.json": ""}, "FROM node:24-bookworm", " libnss3 ", "Node.js 24"},
		{map[string]string{"package.json": `{"devDependencies":{"@playwright/test":"^1"}}`, "package-lock.json": ""}, "FROM node:24-bookworm", " libgtk-4-1 ", "Node.js 24"},
		{map[string]string{"package.json": `{"dependencies":{"puppeteer":"^24"}}`, "yarn.lock": ""}, "FROM node:24-bookworm", "--no-sandbox", "Node.js 24, Yarn"},
		{map[string]string{"package.json": `{"devDependencies":{"@playwright/test":"^1"}}`, "bun.lock": ""}, "FROM oven/bun:1", "# Playwright needs system libraries the Bun image doesn't have", "Bun 1"},
	}
	for _, c := range cases {
		root := t.TempDir()
		for n, b := range c.files {
			if err := os.WriteFile(filepath.Join(root, n), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		body, stack, _, _ := scaffoldDockerfile(root)
		if !strings.Contains(body, "\n"+c.from+"\n") || !strings.Contains(body, c.has) || stack != c.stack {
			t.Errorf("%v: stack %q (want %q); want %q and %q in:\n%s", c.files, stack, c.stack, c.from, c.has, body)
		}
		if !strings.Contains(c.files["package.json"], "electron") && !strings.Contains(c.files["package.json"], "playwright") &&
			!strings.Contains(c.files["package.json"], "puppeteer") && strings.Contains(body, "libnss3") {
			t.Errorf("%v: browser libraries without a browser dependency", c.files)
		}
		if strings.Count(body, "\nFROM ") != 1 {
			t.Errorf("%s: want exactly one FROM", stack)
		}
		// Every Debian or Ubuntu base gets a UTF-8 locale straight after FROM;
		// eclipse-temurin sets its own.
		want := 1
		if strings.HasPrefix(c.from, "FROM eclipse-temurin:") {
			want = 0
		}
		if n := strings.Count(body, "\n"+c.from+"\n"+utf8Locale); n != want || strings.Count(body, "ENV LANG=") != want {
			t.Errorf("%s: want %d UTF-8 locale(s), right after FROM, in:\n%s", stack, want, body)
		}
		// Every stack installs the tools GitHub's runners provide, in one
		// apt RUN that is the first instruction after FROM (and its UTF-8
		// locale), each of its lines continued until the cleanup that ends it.
		var fields []string
		closed := false
		after := body[strings.Index(body, "\n"+c.from+"\n")+len(c.from)+2:]
		lines := strings.Split(strings.TrimPrefix(after, utf8Locale), "\n")
		for len(lines) > 0 && (lines[0] == "" || strings.HasPrefix(lines[0], "#")) {
			lines = lines[1:]
		}
		for i, l := range lines {
			if i == 0 && l != "RUN apt-get update \\" {
				t.Errorf("%s: first instruction after FROM is %q, want the apt layer", stack, l)
				break
			}
			if l == " && rm -rf /var/lib/apt/lists/*" {
				closed = true
				break
			}
			if !strings.HasSuffix(l, " \\") {
				t.Errorf("%s: apt layer line %q isn't continued", stack, l)
				break
			}
			fields = append(fields, strings.Fields(l)...)
		}
		if !closed {
			t.Errorf("%s: apt layer doesn't end in its cleanup in:\n%s", stack, body)
		}
		for _, tool := range wantTools {
			if !slices.Contains(fields, tool) {
				t.Errorf("%s: %s not installed in:\n%s", stack, tool, body)
			}
		}
		// Maven is installed only for a pom.xml project without ./mvnw.
		if slices.Contains(fields, "maven") != strings.HasSuffix(c.stack, ", Maven") {
			t.Errorf("%s: maven installed = %v", stack, slices.Contains(fields, "maven"))
		}
		if strings.Count(body, "\nRUN apt-get update") != 1 {
			t.Errorf("%s: want exactly one apt layer in:\n%s", stack, body)
		}
	}
}

// debianTag picks trixie exactly where the official image publishes it.
func TestDebianTag(t *testing.T) {
	for _, c := range []struct{ image, version, want string }{
		{"node", "lts", "lts-trixie"},
		{"node", "current", "current-trixie"},
		{"node", "18", "18-bookworm"},
		{"node", "20", "20-trixie"},
		{"node", "20.19.3", "20.19.3-bookworm"},
		{"node", "20.19.4", "20.19.4-trixie"},
		{"node", "20.19", "20.19-trixie"},
		{"node", "21.7.3", "21.7.3-bookworm"},
		{"node", "22.17", "22.17-bookworm"},
		{"node", "22.18", "22.18-trixie"},
		{"node", "23", "23-bookworm"},
		{"node", "24.4.1", "24.4.1-bookworm"},
		{"node", "24.5.0", "24.5.0-trixie"},
		{"node", "25.0.0", "25.0.0-trixie"},
		{"node", "30", "30-trixie"},
		{"python", "3.8", "3.8-bookworm"},
		{"python", "3.9", "3.9-trixie"},
		{"python", "3.9.22", "3.9.22-bookworm"},
		{"python", "3.9.23", "3.9.23-trixie"},
		{"python", "3.10.17", "3.10.17-bookworm"},
		{"python", "3.10.18", "3.10.18-trixie"},
		{"python", "3.11.12", "3.11.12-bookworm"},
		{"python", "3.11.13", "3.11.13-trixie"},
		{"python", "3.12.10", "3.12.10-bookworm"},
		{"python", "3.12.11", "3.12.11-trixie"},
		{"python", "3.13.5", "3.13.5-bookworm"},
		{"python", "3.13.6", "3.13.6-trixie"},
		{"python", "3.14.0", "3.14.0-trixie"},
		{"python", "3", "3-trixie"},
		{"golang", "1", "1-trixie"},
		{"golang", "1.23", "1.23-bookworm"},
		{"golang", "1.24", "1.24-trixie"},
		{"golang", "1.24.5", "1.24.5-bookworm"},
		{"golang", "1.24.6", "1.24.6-trixie"},
		{"golang", "1.25.0", "1.25.0-trixie"},
		{"rust", "1", "1-trixie"},
		{"rust", "1.87.0", "1.87.0-bookworm"},
		{"rust", "1.88", "1.88-trixie"},
		{"rust", "1.100.0", "1.100.0-trixie"},
		{"oven/bun", "1", "1-bookworm"},
	} {
		if got := debianTag(c.image, c.version); got != c.want {
			t.Errorf("debianTag(%q, %q) = %q, want %q", c.image, c.version, got, c.want)
		}
	}
}

func TestWireContainer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".spotexec.toml")
	os.WriteFile(p, []byte("version = 1\n# keep me\n[tasks.test]\nrun = \"x\""), 0o644)
	if _, err := wireContainer(p, ".spotexec/Dockerfile"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# keep me") || !strings.Contains(string(b), "run = \"x\"\n") ||
		!strings.Contains(string(b), "[container]\ndockerfile = \".spotexec/Dockerfile\"\n") {
		t.Fatalf("bad append:\n%s", b)
	}
	// Already wired: unchanged.
	msg, _ := wireContainer(p, ".spotexec/Dockerfile")
	if b2, _ := os.ReadFile(p); string(b2) != string(b) || !strings.Contains(msg, "already uses") {
		t.Fatalf("rewired: %s", msg)
	}
	// An existing [container] with an image is left alone, with advice.
	os.WriteFile(p, []byte("[container]\nimage = \"node:22\"\n"), 0o644)
	msg, _ = wireContainer(p, ".spotexec/Dockerfile")
	if b3, _ := os.ReadFile(p); string(b3) != "[container]\nimage = \"node:22\"\n" || !strings.Contains(msg, "remove image") {
		t.Fatalf("edited an existing table: %s", msg)
	}
}

// container init leaves a config in an unsupported format alone, and writes
// no Dockerfile for it.
func TestContainerInitUnsupportedVersion(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, ".spotexec.toml")
	const body = "version = 2\n[tasks.test]\nrun = \"x\"\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	err := containerInit(&client.Workspace{Root: root}, defaultDockerfile, false)
	if err == nil || !strings.Contains(err.Error(), "version = 2 isn't supported") {
		t.Fatalf("want an unsupported-version error, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != body {
		t.Fatalf(".spotexec.toml changed:\n%s", b)
	}
	if _, err := os.Lstat(filepath.Join(root, defaultDockerfile)); !os.IsNotExist(err) {
		t.Fatalf("wrote a Dockerfile: %v", err)
	}
}

// The [deps] install uses the package manager the container installs.
func TestDetectProjectFollowsPackageManager(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"packageManager":"yarn@4.5.1","scripts":{"test":"vitest"}}`), 0o644)
	os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "yarn.lock"), nil, 0o644)
	body, _ := detectProject(root)
	if !strings.Contains(body, `install = "yarn install --immutable"`) || !strings.Contains(body, `run = "yarn run test {args}"`) || strings.Contains(body, "pnpm") {
		t.Fatalf("detectProject ignored packageManager:\n%s", body)
	}
}
