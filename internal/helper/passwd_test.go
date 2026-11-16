package helper

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/spotexec/internal/protocol"
)

func TestWithRunUser(t *testing.T) {
	debianPasswd := "root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n"
	debianGroup := "root:x:0:\nnogroup:x:65534:\n"
	for _, c := range []struct {
		name                  string
		passwd, group         string
		uid, gid              int
		wantPasswd, wantGroup string
	}{
		{
			// python, golang, rust: no user for the runner.
			name: "unknown uid", passwd: debianPasswd, group: debianGroup, uid: 1001, gid: 1001,
			wantPasswd: debianPasswd + "spotexec:x:1001:1001:spotexec:/home/spotexec:/bin/sh\n",
			wantGroup:  debianGroup + "spotexec:x:1001:\n",
		},
		{
			// node with a runner at 1000: the image's user keeps its name.
			name: "known uid", passwd: debianPasswd + "node:x:1000:1000::/home/node:/bin/bash\n",
			group: debianGroup + "node:x:1000:\n", uid: 1000, gid: 1000,
			wantPasswd: debianPasswd + "node:x:1000:1000::/home/spotexec:/bin/bash\n",
			wantGroup:  debianGroup + "node:x:1000:\n",
		},
		{
			// ...and takes the run's gid as its group.
			name: "known uid, other gid", passwd: debianPasswd + "node:x:1000:1000::/home/node:/bin/bash\n",
			group: debianGroup + "node:x:1000:\n", uid: 1000, gid: 1001,
			wantPasswd: debianPasswd + "node:x:1000:1001::/home/spotexec:/bin/bash\n",
			wantGroup:  debianGroup + "node:x:1000:\nspotexec:x:1001:\n",
		},
		{
			name: "known gid only", passwd: debianPasswd, group: debianGroup + "users:x:100:\n", uid: 1000, gid: 100,
			wantPasswd: debianPasswd + "spotexec:x:1000:100:spotexec:/home/spotexec:/bin/sh\n",
			wantGroup:  debianGroup + "users:x:100:\n",
		},
		{
			// A spotexec entry under another id stays the image's.
			name: "name taken", passwd: debianPasswd + "spotexec:x:2000:2000::/home/spotexec:/bin/sh\n",
			group: debianGroup + "spotexec:x:2000:\n", uid: 1000, gid: 1000,
			wantPasswd: debianPasswd + "spotexec:x:2000:2000::/home/spotexec:/bin/sh\nspotexec-1000:x:1000:1000:spotexec:/home/spotexec:/bin/sh\n",
			wantGroup:  debianGroup + "spotexec:x:2000:\nspotexec-1000:x:1000:\n",
		},
		{
			// No files at all (an image built from scratch).
			name: "empty", uid: 1000, gid: 1000,
			wantPasswd: "root:x:0:0:root:/root:/bin/sh\nspotexec:x:1000:1000:spotexec:/home/spotexec:/bin/sh\n",
			wantGroup:  "root:x:0:\nspotexec:x:1000:\n",
		},
		{
			// No final newline, CRLF, and lines that aren't entries survive.
			name: "odd lines", passwd: "root:x:0:0:root:/root:/bin/sh\r\n+::::::", group: "root:x:0:", uid: 1000, gid: 1000,
			wantPasswd: "root:x:0:0:root:/root:/bin/sh\n+::::::\nspotexec:x:1000:1000:spotexec:/home/spotexec:/bin/sh\n",
			wantGroup:  "root:x:0:\nspotexec:x:1000:\n",
		},
	} {
		p, g := withRunUser([]byte(c.passwd), []byte(c.group), c.uid, c.gid)
		if string(p) != c.wantPasswd {
			t.Errorf("%s: passwd\n%s\nwant\n%s", c.name, p, c.wantPasswd)
		}
		if string(g) != c.wantGroup {
			t.Errorf("%s: group\n%s\nwant\n%s", c.name, g, c.wantGroup)
		}
	}
}

// writeTar writes an archive holding one file, as `docker cp -L ctr:path -`
// prints it.
func writeTar(t *testing.T, path, name, body string) {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A runner-user run mounts passwd and group files made from the image's,
// read once per image from a container that is removed again; a root run
// mounts none.
func TestContainerScriptRunUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as root: no run user to add")
	}
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	bin, calls := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	writeTar(t, filepath.Join(bin, "tar-passwd"), "passwd", "root:x:0:0:root:/root:/bin/bash\n")
	slot := t.TempDir()
	s := &Session{slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo"}
	if err := os.MkdirAll(s.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(r *protocol.ContainerRun) string {
		t.Helper()
		host, _, cleanup, err := s.containerScript(r, "true", s.workspace, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		return host
	}

	host := run(&protocol.ContainerRun{Image: "python:3"})
	mounts := regexp.MustCompile(`type=bind,source=([^,]+),target=/etc/(passwd|group),readonly`).FindAllStringSubmatch(host, -1)
	if len(mounts) != 2 {
		t.Fatalf("no passwd and group mounts: %s", host)
	}
	want := map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/bash\nspotexec:x:" + uid + ":" + gid + ":spotexec:/home/spotexec:/bin/sh\n",
		// The image has no /etc/group.
		"group": "root:x:0:\nspotexec:x:" + gid + ":\n",
	}
	for _, m := range mounts {
		b, err := os.ReadFile(m[1])
		if err != nil || string(b) != want[m[2]] {
			t.Fatalf("%s: %q (%v), want %q", m[2], b, err, want[m[2]])
		}
	}
	created := callsWith(t, calls, "create --pull never")
	if len(created) != 1 || !strings.Contains(created[0], "sha256:") {
		t.Fatalf("creates %q", created)
	}
	name := strings.Fields(created[0])[6]
	if !strings.HasPrefix(name, s.containerPrefix()) || len(callsWith(t, calls, "rm -fv "+name)) != 1 {
		t.Fatalf("reading container %s not removed", name)
	}
	if _, ok := s.containers.Load(name); ok {
		t.Fatal("reading container still tracked")
	}

	// The same image again: read once.
	if host2 := run(&protocol.ContainerRun{Image: "python:3"}); !strings.Contains(host2, mounts[0][0]) {
		t.Fatalf("other files for the same image: %s", host2)
	}
	if n := len(callsWith(t, calls, "create --pull never")); n != 1 {
		t.Fatalf("image read %d times", n)
	}

	if host := run(&protocol.ContainerRun{Image: "python:3", Root: true}); strings.Contains(host, "/etc/passwd") {
		t.Fatalf("root run mounts passwd: %s", host)
	}

	// Files that can't be read fail the run.
	if err := os.RemoveAll(filepath.Join(slot, "run", "users")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cp-fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := s.containerScript(&protocol.ContainerRun{Image: "python:3"}, "true", s.workspace, map[string]string{})
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeImageFailed || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestTarFile(t *testing.T) {
	dir := t.TempDir()
	writeTar(t, filepath.Join(dir, "a"), "passwd", "root:x:0:0::/root:/bin/sh\n")
	b, _ := os.ReadFile(filepath.Join(dir, "a"))
	if got, err := tarFile(b); err != nil || string(got) != "root:x:0:0::/root:/bin/sh\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := tarFile(nil); err == nil {
		t.Fatal("empty archive read")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "passwd", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.Close()
	if _, err := tarFile(buf.Bytes()); err == nil {
		t.Fatal("directory read as a file")
	}
}
