package helper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/depskey"
	"github.com/daintreehq/spotexec/internal/fsid"
	"github.com/daintreehq/spotexec/internal/protocol"
)

// depsKey hashes lockfile contents + install command + platform (the same
// function the client uses to sign cache URLs).
func depsKey(ws string, d protocol.Deps) (string, error) {
	image := d.ImageID
	if image == "" && d.Container != nil {
		image = d.Container.Image
	}
	return depskey.Key(ws, depskey.Spec{Lockfiles: d.Lockfiles, Install: d.Install, CacheDirs: d.CacheDirs, Image: image, Toolchain: d.Toolchain, OS: runtime.GOOS, Arch: runtime.GOARCH})
}

// expandCacheDirs resolves cache_dirs globs (e.g. "packages/*/node_modules")
// to existing relative directories under base.
// clearCacheDirs removes whatever matches cache_dirs in the workspace
// (directories, files or symlinks, which are removed, never followed), but
// never a synced (source) file: a broad pattern like "vendor/*" may match
// tracked files, which are not dependency output.
func (s *Session) clearCacheDirs(base string, patterns []string) error {
	return clearCacheDirs(base, patterns, s.syncedIndex())
}

// holdsSynced reports whether rel is, contains, or lies inside a synced
// (source) file: dependency handling must never replace those. It answers
// from an index of synced files and their directories, built once per call
// site (see syncedIndex), in O(depth).
func (s *Session) holdsSynced(rel string) bool { return s.syncedIndex()(rel) }

func (s *Session) syncedIndex() func(string) bool {
	s.loadStored()
	dirs := map[string]bool{}
	for p := range s.stored {
		for d := path.Dir(p); d != "." && d != "/" && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	return func(rel string) bool {
		if _, ok := s.stored[rel]; ok || dirs[rel] {
			return true // a synced file, or a directory holding some
		}
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			if _, ok := s.stored[d]; ok {
				return true // inside a path that is a synced file
			}
		}
		return false
	}
}

func clearCacheDirs(base string, patterns []string, synced func(string) bool) error {
	for _, p := range patterns {
		p = strings.Trim(p, "/")
		matches, _ := filepath.Glob(filepath.Join(base, filepath.FromSlash(p)))
		for _, m := range matches {
			rel, err := filepath.Rel(base, m)
			if err != nil {
				continue
			}
			if _, err := fsid.SafeJoin(base, filepath.ToSlash(rel)); err != nil {
				continue // reached through a symlink: not ours to touch
			}
			if synced(filepath.ToSlash(rel)) {
				continue // holds source files: leave it
			}
			if err := os.RemoveAll(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func expandCacheDirs(base string, patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range patterns {
		p = strings.Trim(p, "/")
		matches, _ := filepath.Glob(filepath.Join(base, filepath.FromSlash(p)))
		for _, m := range matches {
			rel, err := filepath.Rel(base, m)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if _, err := fsid.SafeJoin(base, rel); err != nil {
				continue // reached through a symlink: not ours to touch
			}
			if fi, err := os.Lstat(m); err == nil && fi.IsDir() {
				if !seen[rel] {
					seen[rel] = true
					out = append(out, rel)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *Session) deps(d protocol.Deps) error {
	if s.slotLock == nil {
		return perr(protocol.CodeBadRequest, "acquire required before deps")
	}
	start := time.Now()
	for _, p := range d.CacheDirs {
		if err := fsid.CheckPattern(strings.Trim(p, "/")); err != nil {
			return perr(protocol.CodeBadRequest, "invalid cache_dirs entry %q", p)
		}
	}
	key, err := depsKey(s.workspace, d)
	if err != nil {
		return perr(protocol.CodeDepsFailed, "%v", err)
	}
	res := protocol.DepsResult{Key: key}
	cacheRoot := filepath.Join(s.layout.Deps(s.repoID), key)
	ready := filepath.Join(cacheRoot, ".ready")

	// A slot an older helper relocated (text files only) from a cache that
	// doesn't record its origin: its activate scripts now name this slot,
	// so nothing says where its symlinks point. It is hydrated afresh.
	dirs := expandCacheDirs(s.workspace, d.CacheDirs)
	unknownOrigin := s.state.VenvsRelocated && s.state.VenvRelocation < venvRelocation && len(cacheOrigins(cacheRoot)) == 0 &&
		slices.ContainsFunc(dirs, func(rel string) bool { return len(venvsAt(filepath.Join(s.workspace, filepath.FromSlash(rel)))) > 0 })
	if s.state.DepsKey == key && len(dirs) > 0 && !unknownOrigin {
		if s.state.VenvRelocation < venvRelocation {
			// Hydrated by an older helper, which copied venvs as they were
			// or left their symlinks pointing into another slot.
			// Until this succeeds the slot's copy is suspect: a failure
			// leaves the key unset, so the next run hydrates afresh.
			s.state.DepsKey = ""
			if err := saveSlotState(s.slotDir, s.state); err != nil {
				return err
			}
			origins, synced := cacheOrigins(cacheRoot), s.syncedIndex()
			for _, rel := range dirs {
				if synced(rel) {
					continue
				}
				if err := relocateVenvs(filepath.Join(s.workspace, filepath.FromSlash(rel)), rel, origins, s.workspace); err != nil {
					return perr(protocol.CodeDepsFailed, "relocating %s: %v", rel, err)
				}
			}
			s.state.DepsKey, s.state.VenvsRelocated, s.state.VenvRelocation = key, true, venvRelocation
			if err := saveSlotState(s.slotDir, s.state); err != nil {
				return err
			}
		}
		_ = os.Chtimes(ready, time.Now(), time.Now())
		res.MS = time.Since(start).Milliseconds()
		return s.c.Send(protocol.TypeDepsResult, res)
	}

	if _, err := os.Stat(ready); err != nil {
		// Build once per key under an exclusive lock; other slots wait.
		lockPath := cacheRoot + ".lock"
		var l *flock
		for waited := false; ; waited = true {
			var ok bool
			l, ok, err = tryLock(lockPath)
			if err != nil {
				return err
			}
			if ok {
				break
			}
			if !waited {
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_wait", Message: "waiting for another slot on this runner to install the same dependencies"})
			}
			if s.clientGone() {
				return perr(protocol.CodeDepsFailed, "client went away while waiting for dependency build")
			}
			time.Sleep(500 * time.Millisecond)
		}
		err := func() error {
			// Released before hydrating: the hydration below takes the same
			// lock shared, and flock would deadlock against our own EX lock.
			defer l.Unlock()
			if _, err := os.Stat(ready); err == nil {
				return nil // another slot built it while we waited
			}
			// A fresh VM: try the shared cache before installing.
			if d.CacheGet != "" && d.CacheKey == key {
				_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_cache", Message: "checking shared cache"})
				if ok, err := cacheRestore(context.Background(), s.layout.Root, d.CacheGet, cacheRoot); err == nil && ok {
					res.Restored = true
					return nil
				} else if err != nil {
					_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_cache_error", Message: err.Error()})
				}
			}
			if d.Probe {
				// The client decides who installs (one run for every
				// runner waiting on this key), and asks again.
				res.Missing = true
				return nil
			}
			// The install runs arbitrary package scripts in the workspace:
			// invalidate the fingerprint and dependency key first.
			s.state.Fingerprint, s.state.DepsKey = "", ""
			if err := saveSlotState(s.slotDir, s.state); err != nil {
				return err
			}
			if err := s.buildDeps(d, key, cacheRoot); err != nil {
				return err
			}
			res.Built = true
			if d.CachePut != "" && d.CacheKey == key {
				if t, err := startCacheUpload(s.layout.Root, cacheRoot, cacheRoot+".lock", d.CachePut); err == nil {
					s.track(protocol.ShareDeps, t)
					res.Uploaded = true
				} else {
					_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_cache_error", Message: err.Error()})
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
		if res.Missing {
			res.MS = time.Since(start).Milliseconds()
			return s.c.Send(protocol.TypeDepsResult, res)
		}
	}
	if !res.Built {
		// Hold the key lock shared while copying out of the cache so prune
		// (which needs it exclusively) cannot delete it mid-copy.
		lf, err := openLockFile(cacheRoot + ".lock")
		if err != nil {
			return err
		}
		defer lf.Close()
		for syscall.Flock(int(lf.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
			if s.clientGone() {
				return perr(protocol.CodeDepsFailed, "client went away while waiting for dependency cache")
			}
			time.Sleep(200 * time.Millisecond)
		}
		if _, err := os.Stat(ready); err != nil {
			return perr(protocol.CodeDepsFailed, "dependency cache %s disappeared; retry", key)
		}
		s.state.DepsKey = ""
		if err := saveSlotState(s.slotDir, s.state); err != nil {
			return err
		}
		method, err := s.hydrate(d, cacheRoot)
		if err != nil {
			return perr(protocol.CodeDepsFailed, "hydrate: %v", err)
		}
		res.Hydrated, res.Method = true, method
	}
	_ = os.Chtimes(ready, time.Now(), time.Now())
	s.state.DepsKey, s.state.VenvsRelocated, s.state.VenvRelocation = key, true, venvRelocation
	if err := saveSlotState(s.slotDir, s.state); err != nil {
		return err
	}
	res.MS = time.Since(start).Milliseconds()
	return s.c.Send(protocol.TypeDepsResult, res)
}

// buildDeps runs the install command in the slot workspace (which has the
// full repo context the package manager needs), then snapshots the resulting
// cache dirs into the shared, immutable cache.
func (s *Session) buildDeps(d protocol.Deps, key, cacheRoot string) error {
	script := d.Install
	if strings.TrimSpace(d.Setup) != "" {
		script = d.Setup + "\n" + script
	}
	env := map[string]string{"CI": "true"}
	if mb := s.memoryShareMB(); mb > 0 {
		env["SPOTEXEC_MEMORY"] = strconv.FormatInt(mb, 10)
	}
	for k, v := range d.Env {
		env[k] = v
	}
	// Start from nothing: leftovers from another branch's dependencies
	// (or a symlink planted there) must not end up in this key's cache.
	if err := s.clearCacheDirs(s.workspace, d.CacheDirs); err != nil {
		return perr(protocol.CodeDepsFailed, "clearing cache_dirs: %v", err)
	}
	_ = s.c.Send(protocol.TypeEvent, protocol.Event{Name: "deps_install", Message: d.Install})
	cleanup := func() error { return nil }
	if d.Container != nil {
		host, hostEnv, c, err := s.containerScript(d.Container, d.Install, s.workspace, env)
		if err != nil {
			return perr(protocol.CodeDepsFailed, "%v", err)
		}
		script, env, cleanup = host, hostEnv, c
	}
	pr := s.runProc(script, s.workspace, env, time.Duration(d.TimeoutMS)*time.Millisecond, 5*time.Second, false, nil)
	if err := cleanup(); err != nil {
		return perr(protocol.CodeDepsFailed, "%v", err)
	}
	if pr.startErr != nil || pr.code != 0 {
		msg := fmt.Sprintf("install command exited %d", pr.code)
		if pr.startErr != nil {
			msg = pr.startErr.Error()
		}
		if pr.timedOut {
			msg = "install command timed out"
		} else if d.Container != nil && pr.startErr == nil && s.containerOOMKilled() {
			msg += ": out of memory, the kernel killed a process in its container"
			if s.memLimit > 0 {
				msg += fmt.Sprintf(" when the jobs sharing the runner reached the memory set aside for them (%s); use --exclusive or fewer max_slots", gib(s.memLimit))
			}
		}
		return perr(protocol.CodeDepsFailed, "%s", msg)
	}
	var dirs []string
	synced := s.syncedIndex()
	all := expandCacheDirs(s.workspace, d.CacheDirs)
	for _, rel := range all {
		if !synced(rel) { // source files are never cached output
			dirs = append(dirs, rel)
		}
	}
	if len(all) > 0 && len(dirs) == 0 {
		return perr(protocol.CodeDepsFailed, "every cache_dirs match holds tracked source files (e.g. %s): point cache_dirs at the installed dependencies only", all[0])
	}
	tmp := fmt.Sprintf("%s.tmp-%d", cacheRoot, os.Getpid())
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	for _, rel := range dirs {
		dst := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := cloneTree(filepath.Join(s.workspace, filepath.FromSlash(rel)), dst, "auto"); err != nil {
			_ = os.RemoveAll(tmp)
			return perr(protocol.CodeDepsFailed, "snapshot %s: %v", rel, err)
		}
	}
	// Where the cache was built, for relocateVenvs in other slots.
	if err := os.WriteFile(filepath.Join(tmp, originFile), []byte(originOf(s.workspace)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".ready"), []byte(strings.Join(dirs, "\n")), 0o644); err != nil {
		return err
	}
	_ = os.RemoveAll(cacheRoot)
	return os.Rename(tmp, cacheRoot)
}

// hydrate replaces the slot's cache dirs with clones from the shared cache.
func (s *Session) hydrate(d protocol.Deps, cacheRoot string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cacheRoot, ".ready"))
	if err != nil {
		return "", err
	}
	if err := s.clearCacheDirs(s.workspace, d.CacheDirs); err != nil {
		return "", err
	}
	origins := cacheOrigins(cacheRoot)
	method := "none"
	synced := s.syncedIndex()
	for _, rel := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if rel == "" {
			continue
		}
		// The marker may come from a shared archive: only ever touch paths
		// that match the configured cache_dirs, and never through symlinks.
		if !matchesCacheDirs(rel, d.CacheDirs) {
			return "", fmt.Errorf("dependency cache lists %q, which is not one of cache_dirs", rel)
		}
		src, err := fsid.SafeJoin(cacheRoot, rel)
		if err != nil {
			return "", err
		}
		if synced(rel) {
			continue // source files live here: never overwrite them
		}
		if fi, err := os.Lstat(src); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("dependency cache entry %q is not a directory", rel)
		}
		dst, err := fsid.PrepareParent(s.workspace, rel)
		if err != nil {
			return "", err
		}
		// Whatever is there (a symlink, a file, an earlier copy for a nested
		// entry) must go: copying into it would follow it or nest.
		if err := os.RemoveAll(dst); err != nil {
			return "", err
		}
		m, err := cloneTree(src, dst, d.Hydrate)
		if err != nil {
			return "", err
		}
		// A .venv built in another slot (or VM) points at that workspace.
		if err := relocateVenvs(dst, rel, origins, s.workspace); err != nil {
			return "", fmt.Errorf("relocating %s: %v", rel, err)
		}
		method = m
	}
	return method, nil
}

func matchesCacheDirs(rel string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(strings.Trim(p, "/"), rel); ok {
			return true
		}
	}
	return false
}

// cloneTree copies src to dst preferring copy-on-write. Hardlinks are used
// only when explicitly requested: they share inodes, so a test writing into
// node_modules would corrupt the cache for every slot.
func cloneTree(src, dst, mode string) (string, error) {
	try := func(args ...string) error {
		out, err := exec.Command("cp", append(args, src, dst)...).CombinedOutput()
		if err != nil {
			_ = os.RemoveAll(dst)
			return fmt.Errorf("cp %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	reflink := []string{"-a", "--reflink=always"}
	if runtime.GOOS == "darwin" {
		reflink = []string{"-c", "-R", "-p"}
	}
	switch mode {
	case "hardlink":
		return "hardlink", try("-a", "-l")
	case "copy":
		return "copy", try("-a")
	case "reflink":
		return "reflink", try(reflink...)
	}
	if err := try(reflink...); err == nil {
		return "reflink", nil
	}
	if runtime.GOOS == "darwin" {
		return "copy", try("-R", "-p")
	}
	return "copy", try("-a")
}
