package helper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestNetworkArgs(t *testing.T) {
	for _, c := range []struct {
		network string
		options []string
		want    []string
	}{
		{"", nil, []string{"--network", "host"}},
		{"host", []string{"--shm-size=1g"}, []string{"--network", "host"}},
		{"private", nil, []string{"--network", "bridge"}},
		{"private", []string{"--label", "x"}, []string{"--network", "bridge"}},
		// The user's own network flag wins, in any spelling.
		{"", []string{"--network", "none"}, nil},
		{"", []string{"--network=mynet"}, nil},
		{"", []string{"--net=bridge"}, nil},
		{"private", []string{"--net", "host"}, nil},
		// Boolean flags take no value, so the network flag after one counts.
		{"", []string{"--use-api-socket", "--network", "mynet"}, nil},
		{"private", []string{"--init", "--net", "none"}, nil},
		// A value that looks like the flag isn't the flag.
		{"", []string{"--label", "--network"}, []string{"--network", "host"}},
	} {
		got := networkArgs(&protocol.ContainerRun{Network: c.network, Options: c.options})
		if !slices.Equal(got, c.want) {
			t.Errorf("network %q, options %q: got %q, want %q", c.network, c.options, got, c.want)
		}
	}
}

// Only the flags container.options sets count, not the values they take.
func TestForbiddenOption(t *testing.T) {
	for _, c := range []struct {
		options []string
		want    string
	}{
		{nil, ""},
		{[]string{"-e", "NODE_OPTIONS=-d"}, ""},
		{[]string{"--label", "-u"}, ""},
		{[]string{"-e", "--"}, ""},
		{[]string{"--label", "--name"}, ""},
		{[]string{"-e", "--user=0", "--shm-size", "1g"}, ""},
		{[]string{"--init", "-it", "--cap-add", "SYS_PTRACE"}, ""},
		{[]string{"--"}, "--"},
		{[]string{"--init", "--", "x"}, "--"},
		{[]string{"--name", "x"}, "--name"},
		{[]string{"--name=x"}, "--name"},
		{[]string{"--detach"}, "--detach"},
		{[]string{"--entrypoint", "sh"}, "--entrypoint"},
		{[]string{"-e", "A=1", "--user", "0"}, "--user"},
		{[]string{"--rm"}, "--rm"},
		{[]string{"--workdir=/x"}, "--workdir"},
		{[]string{"-d"}, "-d"},
		{[]string{"-itd"}, "-d"},
		{[]string{"-iu", "0"}, "-u"},
		{[]string{"-w/src"}, "-w"},
		{[]string{"--label", "x", "-Pqu0"}, "-u"},
		// A shorthand that takes a value ends the cluster: the rest is its value.
		{[]string{"-ed"}, ""},
	} {
		if got := forbiddenOption(c.options); got != c.want {
			t.Errorf("forbiddenOption(%q) = %q, want %q", c.options, got, c.want)
		}
	}
}

// containerScript refuses a forbidden option before writing anything, and
// takes a value that only looks like one.
func TestContainerScriptChecksOptions(t *testing.T) {
	bin, _ := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	slot := t.TempDir()
	s := &Session{layout: Layout{Root: t.TempDir()}, slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo"}
	if err := os.MkdirAll(s.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := s.containerScript(context.Background(), &protocol.ContainerRun{Image: "node:22", Options: []string{"-e", "A=1", "-iu", "0"}}, "true", s.workspace, map[string]string{})
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeImageFailed || !strings.Contains(err.Error(), "container.options may not set -u") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(slot, "run")); !os.IsNotExist(err) {
		t.Fatal("files written for a refused run")
	}
	_, _, cleanup, err := s.containerScript(context.Background(), &protocol.ContainerRun{Image: "node:22", Options: []string{"--label", "--name", "-e", "NODE_OPTIONS=-d"}}, "true", s.workspace, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

// A project container's docker run sizes /dev/shm once, from the memory
// docker info reports, before the user's options and the image; a
// --shm-size or --ipc of the user's own replaces it.
func TestContainerScriptSizesShm(t *testing.T) {
	bin, _ := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("DOCKER_HOST", "")
	if err := os.WriteFile(filepath.Join(bin, "info"), []byte(" Context:    default\n Total Memory: 16GiB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	slot := t.TempDir()
	// One slot: no cpuset or memory pool, and half of all but the reserve.
	s := &Session{layout: Layout{Root: t.TempDir()}, slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo", maxSlots: 1}
	if err := os.MkdirAll(s.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		options []string
		want    string
	}{
		{nil, "'--shm-size' '8053063680'"},
		{[]string{"--label", "x"}, "'--shm-size' '8053063680'"},
		{[]string{"--shm-size=2g"}, "'--shm-size=2g'"},
		{[]string{"--ipc=host"}, ""},
	} {
		host, _, cleanup, err := s.containerScript(context.Background(), &protocol.ContainerRun{Image: "node:22", Options: c.options}, "true", s.workspace, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(host, "--shm-size"); c.want == "" && n != 0 || c.want != "" && n != 1 {
			t.Errorf("%v: %d --shm-size in %s", c.options, n, host)
		}
		if c.want != "" && !(strings.Contains(host, c.want) && strings.Index(host, c.want) < strings.Index(host, "'node:22'")) {
			t.Errorf("%v: no %s before the image in %s", c.options, c.want, host)
		}
		if len(c.options) > 0 && c.options[0] == "--label" && strings.Index(host, c.want) > strings.Index(host, "'--label'") {
			t.Errorf("%v: %s after the user's options in %s", c.options, c.want, host)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
	}
}
