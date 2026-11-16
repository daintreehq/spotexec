package helper

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The name and home a run's uid gets inside the container when the image
// has no entry for it.
const (
	runUserName = "spotexec"
	runUserHome = "/home/spotexec"
)

// withRunUser returns the image's passwd and group files with entries that
// make uid and gid resolve: an existing entry for uid keeps its name (an
// image's sudoers or tooling may name it) but gets the run's gid and
// runUserHome as its home, which OpenSSH reads instead of $HOME; otherwise
// a runUserName entry is added (runUserName-uid if the image uses that name
// for another id). A gid the image already knows is left alone; otherwise
// it gets a group named the same way.
func withRunUser(passwd, group []byte, uid, gid int) ([]byte, []byte) {
	u, g := strconv.Itoa(uid), strconv.Itoa(gid)

	lines, found := splitLines(passwd), false
	for i, l := range lines {
		f := strings.Split(l, ":")
		if len(f) == 7 && f[2] == u {
			f[3], f[5] = g, runUserHome
			lines[i] = strings.Join(f, ":")
			found = true
			break
		}
	}
	if !found {
		if !hasID(lines, 7, "0") && uid != 0 {
			lines = append([]string{"root:x:0:0:root:/root:/bin/sh"}, lines...)
		}
		name := freeName(lines, u)
		lines = append(lines, name+":x:"+u+":"+g+":"+runUserName+":"+runUserHome+":/bin/sh")
	}
	newPasswd := joinLines(lines)

	glines := splitLines(group)
	if !hasID(glines, 4, g) {
		if !hasID(glines, 4, "0") && gid != 0 {
			glines = append([]string{"root:x:0:"}, glines...)
		}
		glines = append(glines, freeName(glines, g)+":x:"+g+":")
	}
	return newPasswd, joinLines(glines)
}

// splitLines returns the file's lines without their newlines.
func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// hasID reports whether an entry with fields fields has id as its third.
func hasID(lines []string, fields int, id string) bool {
	for _, l := range lines {
		if f := strings.Split(l, ":"); len(f) == fields && f[2] == id {
			return true
		}
	}
	return false
}

// freeName returns runUserName, or runUserName-id if an entry has that
// name already.
func freeName(lines []string, id string) string {
	for _, l := range lines {
		if n, _, _ := strings.Cut(l, ":"); n == runUserName {
			return runUserName + "-" + id
		}
	}
	return runUserName
}

// imageIDRe matches what `docker image inspect --format '{{.Id}}'` prints.
var imageIDRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// maxAccountFile bounds the passwd or group file read from an image.
const maxAccountFile = 4 << 20

// runUserFiles returns passwd and group files, for bind mounts over the
// image's own, in which uid and gid resolve (withRunUser). They are made
// from the image's files once per image, uid and gid, under the slot's run
// directory (cleared when the slot is next acquired), and read from a
// container that is created but never started, so the image needs no tools.
func (s *Session) runUserFiles(ctx context.Context, d dockerRunner, image string, uid, gid int) (passwd, group string, err error) {
	out, err := runShell(ctx, d.cmd("image inspect --format '{{.Id}}' "+shq(image)))
	id := strings.TrimSpace(string(out))
	if err != nil || !imageIDRe.MatchString(id) {
		return "", "", fmt.Errorf("can't inspect image %s: %s", image, firstLines(id, 3))
	}
	sum := sha256.Sum256([]byte("v1 " + id + " " + strconv.Itoa(uid) + ":" + strconv.Itoa(gid)))
	dir := filepath.Join(s.slotDir, "run", "users", hex.EncodeToString(sum[:12]))
	passwd, group = filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	if _, err := os.Stat(dir); err == nil {
		return passwd, group, nil // published whole (below)
	}
	files, err := s.readImageFiles(ctx, d, id, "/etc/passwd", "/etc/group")
	if err != nil {
		return "", "", fmt.Errorf("can't read the users of image %s: %v", image, err)
	}
	p, g := withRunUser(files[0], files[1], uid, gid)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp) // gone once renamed
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", "", err
	}
	for name, b := range map[string][]byte{"passwd": p, "group": g} {
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o644); err != nil {
			return "", "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another run of this slot published the same files first.
		if _, serr := os.Stat(group); serr != nil {
			return "", "", err
		}
	}
	return passwd, group, nil
}

// readImageFiles returns the contents of the image's files at paths (nil
// for one the image doesn't have), copied out of a container created from
// it and never started. The container is named like the slot's, so if the
// helper dies midway the next holder removes it.
func (s *Session) readImageFiles(ctx context.Context, d dockerRunner, image string, paths ...string) ([][]byte, error) {
	name := s.containerPrefix() + "users-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	s.containers.Store(name, d) // teardown removes it if it runs meanwhile
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// -v: volumes the image declares are made at create.
		_, _ = runShell(rctx, d.cmd("rm -fv "+shq(name))+" >/dev/null 2>&1")
		if removeContainer(rctx, d, name) == nil {
			s.containers.Delete(name)
		}
	}()
	if out, err := runShell(ctx, d.cmd("create --pull never --network none --name "+shq(name)+" --entrypoint /bin/sh "+shq(image))); err != nil {
		return nil, fmt.Errorf("docker create: %v: %s", err, firstLines(strings.TrimSpace(string(out)), 3))
	}
	files := make([][]byte, len(paths))
	for i, p := range paths {
		var stdout limitedBuffer
		var stderr bytes.Buffer
		cmd := shellCmd(ctx, d.cmd("cp -L "+shq(name+":"+p)+" -"))
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := stderr.String()
			// Older daemons say "Could not find the file".
			if strings.Contains(msg, "No such container:path") || strings.Contains(msg, "Could not find the file") {
				continue
			}
			if stdout.full {
				return nil, fmt.Errorf("%s is larger than %d bytes", p, maxAccountFile)
			}
			return nil, fmt.Errorf("docker cp %s: %v: %s", p, err, firstLines(strings.TrimSpace(msg), 3))
		}
		b, err := tarFile(stdout.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		files[i] = b
	}
	return files, nil
}

// tarFile returns the contents of the single regular file in a tar archive
// (what `docker cp -L container:file -` prints).
func tarFile(b []byte) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(b))
	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("unreadable archive: %v", err)
	}
	if h.Typeflag != tar.TypeReg {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(tr, maxAccountFile))
}

// limitedBuffer keeps at most maxAccountFile bytes and fails the write
// past that, so a huge file can't use up memory.
type limitedBuffer struct {
	bytes.Buffer
	full bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.Len()+len(p) > maxAccountFile+64<<10 { // the archive's headers and padding
		l.full = true
		return 0, errors.New("too large")
	}
	return l.Buffer.Write(p)
}
