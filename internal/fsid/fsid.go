// Package fsid computes content identities shared by the CLI and helper.
//
// The identity of a file is its Git blob SHA-1 ("blob <len>\0<bytes>"), and of
// a symlink the blob SHA-1 of its target. Using Git's scheme lets the CLI reuse
// index object IDs for clean tracked files instead of reading them from disk.
package fsid

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// File modes, as in Git.
const (
	ModeFile    uint32 = 0o100644
	ModeExec    uint32 = 0o100755
	ModeSymlink uint32 = 0o120000
)

// Mode maps an os.FileMode to a Git mode; ok is false for unsupported types.
func Mode(m fs.FileMode) (uint32, bool) {
	switch {
	case m&fs.ModeSymlink != 0:
		return ModeSymlink, true
	case m.IsRegular():
		if m&0o111 != 0 {
			return ModeExec, true
		}
		return ModeFile, true
	}
	return 0, false
}

// Perm returns the permission bits to create a regular file with.
func Perm(mode uint32) fs.FileMode {
	if mode == ModeExec {
		return 0o755
	}
	return 0o644
}

// Bytes returns the blob ID of b.
func Bytes(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Reader hashes size bytes from r.
func Reader(r io.Reader, size int64) (string, error) {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	n, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("file changed size while hashing (%d != %d)", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Path computes the ID and Git mode of the file at p (not following symlinks).
func Path(p string) (id string, mode uint32, size int64, err error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", 0, 0, err
	}
	mode, ok := Mode(fi.Mode())
	if !ok {
		return "", 0, 0, fmt.Errorf("%s: unsupported file type %s", p, fi.Mode().Type())
	}
	if mode == ModeSymlink {
		t, err := os.Readlink(p)
		if err != nil {
			return "", 0, 0, err
		}
		return Bytes([]byte(t)), mode, int64(len(t)), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	id, err = Reader(f, fi.Size())
	return id, mode, fi.Size(), err
}

// CleanRel validates a slash-separated relative path from the wire and
// returns it in OS form. It rejects absolute paths, "..", and empty segments.
func CleanRel(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid path %q", p)
		}
		// Never a git directory, in any case (macOS folds .GIT into .git):
		// files there are code that runs on whoever uses the repository.
		if strings.EqualFold(seg, ".git") {
			return "", fmt.Errorf("invalid path %q (inside .git)", p)
		}
	}
	return filepath.FromSlash(p), nil
}

// SafeJoin joins root and a wire path, and verifies that no existing ancestor
// directory between root and the target is a symlink (which could redirect a
// write outside root).
// ErrSymlinkParent: a parent directory of the path is a symlink.
var ErrSymlinkParent = errors.New("parent directory is a symlink")

func SafeJoin(root, rel string) (string, error) {
	r, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, r)
	dir := root
	parts := strings.Split(filepath.Dir(r), string(filepath.Separator))
	if filepath.Dir(r) == "." {
		parts = nil
	}
	for _, seg := range parts {
		dir = filepath.Join(dir, seg)
		fi, err := os.Lstat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to use %q through symlinked directory %q: %w", rel, dir, ErrSymlinkParent)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("refusing to use %q: %q is not a directory: %w", rel, dir, syscall.ENOTDIR)
		}
	}
	return full, nil
}

// PrepareParent ensures every ancestor of rel under root is a real directory,
// replacing symlinks or files that occupy an ancestor position (as git
// checkout does). It never follows a symlink. Used only inside spotexec-owned
// remote workspaces; local write-back uses SafeJoin, which refuses instead.
func PrepareParent(root, rel string) (string, error) {
	r, err := CleanRel(rel)
	if err != nil {
		return "", err
	}
	dir := root
	if d := filepath.Dir(r); d != "." {
		for _, seg := range strings.Split(d, string(filepath.Separator)) {
			dir = filepath.Join(dir, seg)
			fi, err := os.Lstat(dir)
			switch {
			case err == nil && fi.IsDir():
				continue
			case err == nil:
				if err := os.Remove(dir); err != nil {
					return "", err
				}
			case !os.IsNotExist(err):
				return "", err
			}
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(root, r), nil
}
