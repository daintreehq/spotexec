package client

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/manifest"
)

// Mass-deletion guard: a write-back that would delete at least
// massDeleteMin files and at least half of the synced files holds all its
// deletions back. Deleting the project's own root .spotexec.toml or
// .gitignore is always held back.
const massDeleteMin = 5

// protectedPath reports whether deleting p is always held back.
func protectedPath(p string) bool {
	return strings.EqualFold(p, ".spotexec.toml") || strings.EqualFold(p, ".gitignore")
}

// massDeletion reports whether deleting n of total synced files is enough
// to hold the whole batch back.
func massDeletion(n, total int) bool {
	return n >= massDeleteMin && 2*n >= total
}

// committedFiles answers whether a local version has a committed copy: the
// manifest's HEAD holds the same path with the same blob and mode, so git
// can give it back. Outside git, before the first commit, or when HEAD's
// tree can't be read, nothing counts as committed.
type committedFiles struct {
	ctx    context.Context
	m      *manifest.Manifest
	loaded bool
	tree   map[string]version
}

func (c *committedFiles) has(p string, v version) bool {
	if !v.exists || !c.m.Git || c.m.Head == "" {
		return false
	}
	if !c.loaded {
		c.loaded = true
		c.tree, _ = headTree(c.ctx, c.m.Root, c.m.Head)
	}
	t, ok := c.tree[p]
	return ok && t.eq(v)
}

// headTree lists the files of commit head: path -> (blob, mode).
func headTree(ctx context.Context, root, head string) (map[string]version, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-tree", "-r", "-z", "--full-tree", head)
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	tree := map[string]version{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		// "<mode> <type> <id>\t<path>"
		meta, p, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 3 || f[1] != "blob" {
			continue
		}
		mode, err := strconv.ParseUint(f[0], 8, 32)
		if err != nil {
			continue
		}
		tree[string(p)] = version{exists: true, id: f[2], mode: uint32(mode)}
	}
	return tree, nil
}

// backupLocal saves the local file root/p, expected to be at version want,
// to dir/p (a symlink is saved as a symlink). It returns where the copy
// went, or errChangedUnderUs when the file no longer matches want: the copy
// is checked against want as it is made, so what is saved is exactly what
// write-back is about to delete or replace.
func backupLocal(root, dir, p string, want version) (string, error) {
	src, err := fsid.SafeJoin(root, p)
	if err != nil {
		return "", errChangedUnderUs
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return "", errChangedUnderUs
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := stageBackup(src, fi, dir, want)
	if err != nil {
		return "", err
	}
	dst, err := fsid.SafeJoin(dir, p)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(dst), 0o700); err == nil {
			err = renameNoReplace(tmp, dst)
		}
	}
	if err != nil && (stashCollision(err) || errors.Is(err, fs.ErrExist)) {
		// Something in the backup is in the way: a flattened name nothing
		// else uses, at the top of the backup.
		base := strings.ReplaceAll(p, "/", "__")
		for i := 2; ; i++ {
			dst = filepath.Join(dir, fmt.Sprintf("%s.%d", base, i))
			if err = renameNoReplace(tmp, dst); !errors.Is(err, fs.ErrExist) {
				break
			}
		}
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// renameNoReplace renames oldp to newp unless newp exists.
func renameNoReplace(oldp, newp string) error {
	if _, err := os.Lstat(newp); err == nil {
		return fs.ErrExist
	}
	return os.Rename(oldp, newp)
}

// stageBackup copies src into a temporary file in dir and returns its path.
func stageBackup(src string, fi fs.FileInfo, dir string, want version) (string, error) {
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return "", errChangedUnderUs
		}
		if !want.eq(version{true, fsid.Bytes([]byte(target)), fsid.ModeSymlink}) {
			return "", errChangedUnderUs
		}
		f, err := os.CreateTemp(dir, ".spotexec-tmp-*")
		if err != nil {
			return "", err
		}
		tmp := f.Name()
		f.Close()
		os.Remove(tmp)
		return tmp, os.Symlink(target, tmp)
	}
	if !fi.Mode().IsRegular() {
		return "", errChangedUnderUs
	}
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", errChangedUnderUs
	}
	defer in.Close()
	ofi, err := in.Stat()
	if err != nil || !os.SameFile(fi, ofi) {
		return "", errChangedUnderUs
	}
	if mode, _ := fsid.Mode(ofi.Mode()); mode != want.mode {
		return "", errChangedUnderUs
	}
	out, err := os.CreateTemp(dir, ".spotexec-tmp-*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", ofi.Size())
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != ofi.Size() || hex.EncodeToString(h.Sum(nil)) != want.id) {
		err = errChangedUnderUs
	}
	if err == nil {
		err = os.Chmod(tmp, ofi.Mode().Perm())
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}
