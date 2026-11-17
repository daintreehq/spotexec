package helper

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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
			name: "both names taken", passwd: debianPasswd + "spotexec:x:2000:2000::/h:/bin/sh\nspotexec-1000:x:2001:2001::/h:/bin/sh\n",
			group: debianGroup, uid: 1000, gid: 1000,
			wantPasswd: debianPasswd + "spotexec:x:2000:2000::/h:/bin/sh\nspotexec-1000:x:2001:2001::/h:/bin/sh\nspotexec-1000-2:x:1000:1000:spotexec:/home/spotexec:/bin/sh\n",
			wantGroup:  debianGroup + "spotexec:x:1000:\n",
		},
		{
			// libc reads the id as a number.
			name: "zero-padded uid", passwd: debianPasswd + "node:x:01000:1000::/home/node:/bin/sh\n",
			group: debianGroup + "node:x:01000:\n", uid: 1000, gid: 1000,
			wantPasswd: debianPasswd + "node:x:01000:1000::/home/spotexec:/bin/sh\n",
			wantGroup:  debianGroup + "node:x:01000:\n",
		},
		{
			// Comments and NIS directives aren't entries for the uid.
			name: "comment and directive", passwd: debianPasswd + "#node:x:1000:1000::/home/node:/bin/sh\n+node:x:1000:1000::/home/node:/bin/sh\n",
			group: debianGroup + "#node:x:1000:\n-node:x:1000:\n", uid: 1000, gid: 1000,
			// ...and the new one goes first, since musl reads them as entries.
			wantPasswd: debianPasswd + "spotexec:x:1000:1000:spotexec:/home/spotexec:/bin/sh\n#node:x:1000:1000::/home/node:/bin/sh\n+node:x:1000:1000::/home/node:/bin/sh\n",
			wantGroup:  debianGroup + "spotexec:x:1000:\n#node:x:1000:\n-node:x:1000:\n",
		},
		{
			// ...indented too.
			name: "indented comment", passwd: debianPasswd + "  #node:x:1000:1000::/home/node:/bin/sh\n",
			group: debianGroup + "\t#node:x:1000:\n", uid: 1000, gid: 1000,
			wantPasswd: debianPasswd + "spotexec:x:1000:1000:spotexec:/home/spotexec:/bin/sh\n  #node:x:1000:1000::/home/node:/bin/sh\n",
			wantGroup:  debianGroup + "spotexec:x:1000:\n\t#node:x:1000:\n",
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
	s := &Session{layout: Layout{Root: t.TempDir()}, slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo"}
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
	if len(mounts) != 2 || mounts[0][2] != "passwd" {
		t.Fatalf("no passwd and group mounts, in that order: %s", host)
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

	// A passwd the project mounts itself is left to it.
	host = run(&protocol.ContainerRun{Image: "python:3", Options: []string{"-v", "/x/passwd:/etc/passwd:ro"}})
	if strings.Contains(host, "target=/etc/passwd") || !strings.Contains(host, "target=/etc/group") {
		t.Fatalf("mounts with the project's passwd: %s", host)
	}

	// Files that can't be read fail the run.
	if err := os.RemoveAll(s.layout.Users()); err != nil {
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
	big := strings.Repeat("x", maxAccountFile+1)
	writeTar(t, filepath.Join(dir, "big"), "passwd", big)
	b, _ = os.ReadFile(filepath.Join(dir, "big"))
	if _, err := tarFile(b); err == nil {
		t.Fatal("oversized file read")
	}
}

// The bound holds where it matters: as a command's stdout, which os/exec
// fills with io.Copy.
func TestLimitedBufferAsStdout(t *testing.T) {
	var lb limitedBuffer
	cmd := exec.Command("head", "-c", strconv.Itoa(maxAccountFile+128<<10), "/dev/zero")
	cmd.Stdout = &lb
	if err := cmd.Run(); err == nil || !lb.full || lb.buf.Len() > maxAccountFile+64<<10 {
		t.Fatalf("err %v, full %v, kept %d", err, lb.full, lb.buf.Len())
	}
}

func TestMountsAt(t *testing.T) {
	for _, c := range []struct {
		opts []string
		want bool
	}{
		{nil, false},
		{[]string{"-v", "/x/passwd:/etc/passwd:ro"}, true},
		{[]string{"--volume=/x/passwd:/etc/passwd"}, true},
		{[]string{"-v/x/passwd:/etc/passwd"}, true},
		{[]string{"--mount", "type=bind,src=/x,target=/etc/passwd,readonly"}, true},
		{[]string{"--mount=type=bind,source=/x,dst=/etc/passwd"}, true},
		{[]string{"--mount", "type=bind,destination=/etc/passwd,source=/x"}, true},
		// The host's file mounted elsewhere, or another file.
		{[]string{"-v", "/etc/passwd:/host/passwd:ro"}, false},
		{[]string{"--mount", "type=bind,source=/etc/passwd,target=/x"}, false},
		{[]string{"-v", "/x:/etc/passwd.d"}, false},
		{[]string{"-v", "/x:/etc/./passwd:ro"}, true},
		{[]string{"--mount", "Type=bind,Source=/x,Target=/etc/passwd/"}, true},
		{[]string{"-itv", "/x:/etc/passwd"}, true},
		{[]string{"--mount", `type=bind,src=/x,"target=/etc/passwd",readonly`}, true},
		{[]string{"-v=/x:/etc/passwd"}, true},
		{[]string{"-v", "/etc/passwd"}, true}, // an anonymous volume there
		{[]string{"-e", "A=/etc/passwd"}, false},
		// Only the mount flags' values count.
		{[]string{"-e", "LOOKUP=/tmp:/etc/passwd"}, false},
		{[]string{"--label", "target=/etc/passwd"}, false},
		{[]string{"--label", "-v", "-e", "x:/etc/passwd"}, false},
	} {
		if got := mountsAt(c.opts, "/etc/passwd"); got != c.want {
			t.Errorf("%q: got %v", c.opts, got)
		}
	}
}

// Slots of one runner user share the users files of an image: runs that
// start together read the image once, and every later run reuses what the
// first one made, though acquiring a slot clears its run directory.
func TestRunUserFilesSharedAcrossSlots(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as root: no run user to add")
	}
	bin, calls := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	writeTar(t, filepath.Join(bin, "tar-passwd"), "passwd", "root:x:0:0:root:/root:/bin/bash\n")
	// Slow enough that every first run asks before the read finishes.
	if err := os.WriteFile(filepath.Join(bin, "create-sleep"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := Layout{Root: t.TempDir()}
	session := func(n int) *Session {
		s := &Session{layout: l, slotDir: l.Slot("repo", n), repoID: "repo"}
		s.workspace = filepath.Join(s.slotDir, "workspace")
		if err := os.MkdirAll(s.workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		return s
	}
	mountRe := regexp.MustCompile(`type=bind,source=([^,]+),target=/etc/passwd,readonly`)
	run := func(s *Session) (string, error) {
		host, _, cleanup, err := s.containerScript(&protocol.ContainerRun{Image: "python:3"}, "true", s.workspace, map[string]string{})
		if err != nil {
			return "", err
		}
		if err := cleanup(); err != nil {
			return "", err
		}
		m := mountRe.FindStringSubmatch(host)
		if m == nil {
			return "", errors.New("no passwd mount: " + host)
		}
		return m[1], nil
	}

	sessions := []*Session{session(0), session(1), session(2), session(3)}
	sources := make([]string, len(sessions))
	errs := make(chan error, len(sessions))
	for i, s := range sessions {
		go func() {
			var err error
			sources[i], err = run(s)
			errs <- err
		}()
	}
	for range sessions {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := len(callsWith(t, calls, "create --pull never")); n != 1 {
		t.Fatalf("image read %d times by %d first runs", n, len(sessions))
	}
	for i, src := range sources {
		if src != sources[0] {
			t.Fatalf("slot %d mounts %s, slot 0 %s", i, src, sources[0])
		}
		if strings.HasPrefix(src, sessions[i].slotDir+string(filepath.Separator)) {
			t.Fatalf("users files kept in the slot: %s", src)
		}
	}

	// Later runs, on a slot acquired again (which clears its run directory)
	// and on a slot new to the image, while the VM is busy: no read.
	if err := os.RemoveAll(filepath.Join(sessions[0].slotDir, "run")); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{sessions[0], session(4)} {
		src, err := run(s)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(src); err != nil || !strings.HasPrefix(string(b), "root:x:0:0:") {
			t.Fatalf("passwd %q (%v)", b, err)
		}
	}
	if n := len(callsWith(t, calls, "create --pull never")); n != 1 {
		t.Fatalf("image read %d times", n)
	}
}

// Getting the users files has one budget, from the image's inspection to
// the end of the read, past which the run fails; teardown ends a read in
// progress instead of waiting for it.
func TestRunUserFilesBudget(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as root: no run user to add")
	}
	if usersBudget < 2*time.Minute {
		t.Fatalf("budget %v: a fresh VM's first runs need minutes", usersBudget)
	}
	bin, calls := statefulDocker(t, false, "", "", "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	writeTar(t, filepath.Join(bin, "tar-passwd"), "passwd", "root:x:0:0:root:/root:/bin/bash\n")
	setSleep := func(d string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, "create-sleep"), []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	slot := t.TempDir()
	s := &Session{layout: Layout{Root: t.TempDir()}, slotDir: slot, workspace: filepath.Join(slot, "workspace"), repoID: "repo"}
	if err := os.MkdirAll(s.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	start := func() (time.Duration, error) {
		t.Helper()
		t0 := time.Now()
		_, _, cleanup, err := s.containerScript(&protocol.ContainerRun{Image: "python:3"}, "true", s.workspace, map[string]string{})
		if err == nil {
			err = cleanup()
		}
		return time.Since(t0), err
	}
	old := usersBudget
	t.Cleanup(func() { usersBudget = old })

	// Over budget: the read is stopped and the run fails, promptly.
	usersBudget = 500 * time.Millisecond
	setSleep("10")
	took, err := start()
	var pe *protoError
	if !errors.As(err, &pe) || pe.code != protocol.CodeImageFailed || took > 8*time.Second {
		t.Fatalf("over budget: %v after %v", err, took)
	}
	if _, err := os.Stat(s.layout.Users()); err == nil {
		if left, _ := os.ReadDir(s.layout.Users()); len(left) != 1 { // the lock file
			t.Fatalf("left behind: %v", left)
		}
	}

	// A slow read inside the budget passes.
	usersBudget = 10 * time.Second
	setSleep("1")
	if _, err := start(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.layout.Users()); err != nil {
		t.Fatal(err)
	}

	// Teardown cancels a read in progress.
	setSleep("30")
	n := len(callsWith(t, calls, "create --pull never"))
	errs := make(chan error, 1)
	t0 := time.Now()
	go func() { _, err := start(); errs <- err }()
	for len(callsWith(t, calls, "create --pull never")) == n {
		if time.Since(t0) > 5*time.Second {
			t.Fatal("no read started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.stopping.Store(true)
	s.cancelPrep()
	select {
	case err := <-errs:
		if !errors.As(err, &pe) || pe.code != protocol.CodeImageFailed {
			t.Fatalf("cancelled read: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("teardown waited for the read")
	}
	// Once stopping, a new preparation starts cancelled.
	ctx, done := s.startPrep(time.Minute)
	defer done()
	if ctx.Err() == nil {
		t.Fatal("preparation after teardown not cancelled")
	}
}
