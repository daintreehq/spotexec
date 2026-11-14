package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldDockerfileStacks(t *testing.T) {
	cases := []struct {
		files map[string]string
		from  string
		has   string
	}{
		{map[string]string{"package.json": `{"packageManager":"pnpm@9.15.0+sha512.abc","engines":{"node":">=20"}}`, "pnpm-lock.yaml": ""}, "FROM node:20-bookworm", "pnpm@9.15.0 "},
		{map[string]string{"package.json": `{}`, "package-lock.json": "", ".nvmrc": "v18.19.0\n"}, "FROM node:18-bookworm", ""},
		{map[string]string{"package.json": `{}`, "bun.lock": ""}, "FROM oven/bun:1", "git"},
		{map[string]string{"uv.lock": "", ".python-version": "3.11.4\n"}, "FROM ghcr.io/astral-sh/uv:python3.11-bookworm", "UV_PYTHON_DOWNLOADS"},
		{map[string]string{"go.mod": "module x\n\ngo 1.25.3\n"}, "FROM golang:1.25-bookworm", "GOMODCACHE=/home/spotexec"},
		{map[string]string{"Cargo.toml": "", "rust-toolchain.toml": "[toolchain]\nchannel = \"1.83.0\"\n"}, "FROM rust:1.83.0-bookworm", "CARGO_HOME=/home/spotexec"},
		{map[string]string{"pom.xml": ""}, "FROM eclipse-temurin:21-jdk", "git maven"},
		{map[string]string{"build.gradle.kts": ""}, "FROM eclipse-temurin:21-jdk", ""},
		{map[string]string{}, "FROM ubuntu:24.04", "build-essential"},
	}
	for _, c := range cases {
		root := t.TempDir()
		for n, b := range c.files {
			if err := os.WriteFile(filepath.Join(root, n), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		body, stack := scaffoldDockerfile(root)
		if !strings.Contains(body, "\n"+c.from+"\n") || !strings.Contains(body, c.has) {
			t.Errorf("%v (%s): want %q and %q in:\n%s", c.files, stack, c.from, c.has, body)
		}
		if strings.Count(body, "\nFROM ") != 1 {
			t.Errorf("%s: want exactly one FROM", stack)
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
