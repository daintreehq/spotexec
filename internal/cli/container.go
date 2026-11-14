package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/daintreehq/spotexec/internal/client"
)

// defaultDockerfile is where `container init` writes the project image.
const defaultDockerfile = ".spotexec/Dockerfile"

func cmdContainer() *cobra.Command {
	c := &cobra.Command{Use: "container", Short: "Project container image: scaffold a Dockerfile and build it on a runner"}

	var force, yes bool
	var dfPath string
	initC := &cobra.Command{
		Use:   "init",
		Short: "Write a Dockerfile for this project and point .spotexec.toml at it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, _ := os.Getwd()
			ws, err := client.Resolve(wd)
			if err != nil {
				return err
			}
			if err := checkProjectDir(ws, yes); err != nil {
				return err
			}
			return containerInit(ws, dfPath, force)
		},
	}
	initC.Flags().BoolVar(&force, "force", false, "overwrite an existing Dockerfile")
	initC.Flags().BoolVar(&yes, "yes", false, "mark a directory that isn't a Git repository without asking")
	initC.Flags().StringVar(&dfPath, "path", defaultDockerfile, "repo-relative path of the Dockerfile to write")

	var f runFlags
	build := &cobra.Command{
		Use:   "build [flags]",
		Short: "Build (or pull) the project image on a runner now; on AWS it is pushed to ECR for every VM",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(f.dir)
			if err != nil {
				return err
			}
			if !l.cfg.Project.Container.Enabled() {
				return fmt.Errorf("no [container] in %s; run `spotexec container init` first", filepath.Join(l.ws.Root, ".spotexec.toml"))
			}
			f.noDeps = true
			return exitWith(f.execute(l, runSpec{display: "(container build)", syncOnly: true, sync: "in", waitPush: true}))
		},
	}
	f.register(build.Flags())
	c.AddCommand(initC, build)
	return c
}

// containerInit writes the Dockerfile and wires it into .spotexec.toml.
func containerInit(ws *client.Workspace, rel string, force bool) error {
	rel = strings.TrimPrefix(path.Clean(filepath.ToSlash(rel)), "./")
	if rel == "." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
		return fmt.Errorf("--path must be inside the repository")
	}
	abs := filepath.Join(ws.Root, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); err == nil {
		if !force {
			return fmt.Errorf("%s exists (use --force to overwrite)", rel)
		}
		// Replace, never write through, whatever is there (a symlink).
		if err := os.Remove(abs); err != nil {
			return err
		}
	}
	body, stack := scaffoldDockerfile(ws.Root)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s)\n", rel, stack)

	cfgPath := filepath.Join(ws.Root, ".spotexec.toml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		b, notes := detectProject(ws.Root)
		if err := os.WriteFile(cfgPath, []byte(b), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote .spotexec.toml")
		for _, n := range notes {
			fmt.Println("  " + n)
		}
	}
	msg, err := wireContainer(cfgPath, rel)
	if err != nil {
		return err
	}
	fmt.Println(msg)

	// The configured Dockerfile and its build context sync whether or not
	// Git tracks them; committing it shares the image with the team.
	if ws.Git {
		fmt.Printf("commit %s with the project so everyone runs in the same image\n", rel)
	}
	fmt.Println("next: edit the Dockerfile, then `spotexec container build -r <runner>` to build it (and, on AWS, push it to ECR)")
	return nil
}

// wireContainer points [container] at the Dockerfile. It appends the table
// when there is none and otherwise leaves the file alone (it may hold
// comments and choices we must not rewrite), saying what to change.
func wireContainer(cfgPath, rel string) (string, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", err
	}
	var probe struct {
		Container map[string]any `toml:"container"`
	}
	md, err := toml.Decode(string(data), &probe)
	if err != nil {
		return "", fmt.Errorf("%s: %w", cfgPath, err)
	}
	if !md.IsDefined("container") {
		add := "\n# Every command and the [deps] install run in this image (spotexec container build).\n[container]\ndockerfile = " + fmt.Sprintf("%q", rel) + "\n"
		if len(data) > 0 && data[len(data)-1] != '\n' {
			add = "\n" + add
		}
		f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString(add); err != nil {
			return "", err
		}
		return fmt.Sprintf("added [container] dockerfile = %q to .spotexec.toml", rel), nil
	}
	if d, _ := probe.Container["dockerfile"].(string); d == rel {
		if _, hasImage := probe.Container["image"]; !hasImage {
			return ".spotexec.toml already uses it", nil
		}
	}
	return fmt.Sprintf(".spotexec.toml already has a [container] table: set dockerfile = %q there (and remove image =) to use it", rel), nil
}

var (
	firstNum   = regexp.MustCompile(`\d+`)
	majorMinor = regexp.MustCompile(`^\s*(\d+\.\d+)`)
	goDirect   = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	rustChan   = regexp.MustCompile(`(?m)^\s*channel\s*=\s*"(\d+\.\d+(?:\.\d+)?)"`)
	pmVersion  = regexp.MustCompile(`^(pnpm|yarn|npm)@(\d+[0-9A-Za-z.\-]*)`)
	bareVer    = regexp.MustCompile(`^\s*(\d+\.\d+(?:\.\d+)?)\s*$`)
)

func readSmall(root, name string) string {
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || len(b) > 1<<20 {
		return ""
	}
	return string(b)
}

// nodeMajor reads the Node version from .nvmrc, .node-version or
// package.json engines.node (the first number), defaulting to 22.
func nodeMajor(root string, pkg map[string]any) string {
	for _, f := range []string{".nvmrc", ".node-version"} {
		if v := firstNum.FindString(strings.TrimPrefix(strings.TrimSpace(readSmall(root, f)), "v")); v != "" {
			return v
		}
	}
	if eng, ok := pkg["engines"].(map[string]any); ok {
		if n, ok := eng["node"].(string); ok {
			if v := firstNum.FindString(n); v != "" {
				return v
			}
		}
	}
	return "22"
}

// scaffoldDockerfile proposes a Dockerfile for the project in root and
// names the stack it detected.
func scaffoldDockerfile(root string) (string, string) {
	var pkg map[string]any
	if s := readSmall(root, "package.json"); s != "" {
		_ = json.Unmarshal([]byte(s), &pkg)
	}
	pmPin := ""
	if s, ok := pkg["packageManager"].(string); ok {
		if m := pmVersion.FindStringSubmatch(s); m != nil {
			pmPin = m[2]
		}
	}
	var stack, body string
	switch {
	case exists(root, "bun.lock") || exists(root, "bun.lockb"):
		stack = "Bun"
		body = `FROM oven/bun:1

# Tools many test suites expect (the base image is slim).
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
`
	case exists(root, "pnpm-lock.yaml") || exists(root, "yarn.lock") || exists(root, "package-lock.json") || pkg != nil:
		node := nodeMajor(root, pkg)
		stack = "Node.js " + node
		body = fmt.Sprintf(`FROM node:%s-bookworm
`, node)
		switch {
		case exists(root, "pnpm-lock.yaml"):
			stack += ", pnpm"
			v := "latest"
			if pmPin != "" && strings.HasPrefix(pkg["packageManager"].(string), "pnpm@") {
				v = pmPin
			}
			body += fmt.Sprintf(`
# pnpm, installed globally (pinned from package.json "packageManager" when set).
RUN npm install -g pnpm@%s && npm cache clean --force
`, v)
		case exists(root, "yarn.lock"):
			stack += ", Yarn"
			body += `
# Yarn 1 ships with this image. Yarn 2+ projects usually commit their release
# (.yarn/releases, yarnPath in .yarnrc.yml), which Yarn 1 hands off to.
`
		}
	case exists(root, "uv.lock"):
		py := pythonVersion(root)
		stack = "Python " + py + ", uv"
		body = fmt.Sprintf(`FROM ghcr.io/astral-sh/uv:python%s-bookworm

# Use the image's Python; copy (not hardlink) out of uv's cache.
ENV UV_PYTHON_DOWNLOADS=never \
    UV_LINK_MODE=copy
`, py)
	case exists(root, "pyproject.toml") || exists(root, "requirements.txt") || exists(root, "setup.py"):
		py := pythonVersion(root)
		stack = "Python " + py
		body = fmt.Sprintf(`FROM python:%s-bookworm

# Commands run as a non-root user, so install packages into a virtualenv in
# the workspace (and list it in [deps] cache_dirs), or bake them in here:
# RUN pip install --no-cache-dir pytest
`, py)
	case exists(root, "go.mod"):
		gv := "1"
		if m := goDirect.FindStringSubmatch(readSmall(root, "go.mod")); m != nil {
			gv = m[1]
		}
		stack = "Go " + gv
		body = fmt.Sprintf(`FROM golang:%s-bookworm

# Keep the module and build caches in the slot's persistent HOME, so they
# survive between runs (the container itself is thrown away each time).
ENV GOMODCACHE=/home/spotexec/go/pkg/mod \
    GOCACHE=/home/spotexec/.cache/go-build
`, gv)
	case exists(root, "Cargo.toml"):
		rv := "1"
		for _, f := range []string{"rust-toolchain.toml", "rust-toolchain"} {
			s := readSmall(root, f)
			if m := rustChan.FindStringSubmatch(s); m != nil {
				rv = m[1]
				break
			}
			if m := bareVer.FindStringSubmatch(s); m != nil {
				rv = m[1]
				break
			}
		}
		stack = "Rust " + rv
		body = fmt.Sprintf(`FROM rust:%s-bookworm

# Components are installed as root: keep the toolchain writable for the
# non-root user, as the base image does. Install extra tools system-wide:
# RUN cargo install --root /usr/local/cargo cargo-nextest
RUN rustup component add clippy rustfmt && chmod -R a+w "$RUSTUP_HOME" "$CARGO_HOME"

# At run time, keep the registry and git checkouts in the slot's persistent
# HOME, so they survive between runs (the container itself is thrown away).
ENV CARGO_HOME=/home/spotexec/.cargo \
    PATH=/home/spotexec/.cargo/bin:$PATH
`, rv)
	case exists(root, "pom.xml") || exists(root, "build.gradle") || exists(root, "build.gradle.kts"):
		stack = "Java 21"
		tools := "git"
		if exists(root, "pom.xml") && !exists(root, "mvnw") {
			tools += " maven"
			stack += ", Maven"
		} else if exists(root, "pom.xml") {
			stack += ", Maven wrapper"
		} else {
			stack += ", Gradle wrapper"
		}
		body = fmt.Sprintf(`FROM eclipse-temurin:21-jdk

# ./mvnw and ./gradlew download into HOME (persistent per slot).
RUN apt-get update \
 && apt-get install -y --no-install-recommends %s \
 && rm -rf /var/lib/apt/lists/*
`, tools)
	default:
		stack = "generic"
		body = `FROM ubuntu:24.04

RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential git curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
`
	}
	header := `# Project container for spotexec (generated by ` + "`spotexec container init`" + ` for a
# ` + stack + ` project). Every ` + "`sx`" + ` command, and the [deps] install, runs inside
# this image on the runner. Edit it freely, then build it with:
#
#   spotexec container build -r <runner>
#
# On AWS the built image is pushed to a private ECR repository, so every
# other VM pulls it instead of building it again.
#
# How the image is used:
# - Your source is NOT copied in: the worktree is mounted at the same path
#   at run time. Install tools here, not code; dependencies (node_modules,
#   .venv, ...) come from the [deps] cache.
# - Commands run as the runner's non-root user, with HOME=/home/spotexec (a
#   per-slot directory mounted at run time that persists between runs).
#   Install toolchains system-wide (e.g. /usr/local), never under /root or
#   /home/spotexec: anything built there is hidden by the mount.
# - The build context is this file's directory. The image is rebuilt only
#   when this file (or the context) changes, and needs /bin/sh (bash is used
#   when present).

`
	extra := `
# Extra system packages your commands need, for example:
# RUN apt-get update \
#  && apt-get install -y --no-install-recommends postgresql-client jq \
#  && rm -rf /var/lib/apt/lists/*
`
	return header + body + extra, stack
}

// pythonVersion reads major.minor from .python-version (default 3.12).
func pythonVersion(root string) string {
	if m := majorMinor.FindStringSubmatch(readSmall(root, ".python-version")); m != nil {
		return m[1]
	}
	return "3.12"
}
