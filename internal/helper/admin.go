package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/spotexec/internal/protocol"
)

// SlotInfo describes one repo workspace slot.
type SlotInfo struct {
	Repo     string `json:"repo"`
	Slot     int    `json:"slot"`
	Held     bool   `json:"held"`
	LastUsed int64  `json:"last_used"`
	Worktree string `json:"worktree_key"`
	DepsKey  string `json:"deps_key"`
}

// Status is the JSON printed by `spotexec-helper status`.
type Status struct {
	Version     string            `json:"version"`
	Hostname    string            `json:"hostname"`
	OS          string            `json:"os"`
	Arch        string            `json:"arch"`
	CPUs        int               `json:"cpus"`
	Load1       float64           `json:"load1"`
	MemTotalMB  int64             `json:"mem_total_mb"`
	MemAvailMB  int64             `json:"mem_avail_mb"`
	UptimeS     int64             `json:"uptime_s"`
	Draining    bool              `json:"draining"`
	SpotNotice  string            `json:"spot_notice,omitempty"`
	Booting     bool              `json:"booting,omitempty"`     // first-boot setup not finished
	BootFailed  string            `json:"boot_failed,omitempty"` // bootstrap exit status, if it failed
	IdleS       int64             `json:"idle_s"`                // seconds since last activity (-1 unknown)
	Ephemeral   bool              `json:"ephemeral"`
	IdleTimeout string            `json:"idle_timeout,omitempty"`
	Holders     []protocol.Holder `json:"holders"`
	Waiters     []protocol.Holder `json:"waiters,omitempty"` // runs waiting for their exact workspace slot (no token yet)
	Slots       []SlotInfo        `json:"slots"`
	DiskFreeMB  int64             `json:"disk_free_mb"`
}

// GetStatus inspects the work root without taking any locks.
func GetStatus(root string) Status {
	l := Layout{Root: root}
	st := Status{Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), IdleS: -1}
	st.Hostname, _ = os.Hostname()
	st.Draining = draining(root)
	st.Holders = readHolders(l)
	if st.Holders == nil {
		st.Holders = []protocol.Holder{}
	}
	st.Waiters = readWaiters(l)
	vm := onSpotexecVM()
	for _, p := range []string{ActivityFile, filepath.Join(root, "activity")} {
		if p == ActivityFile && !vm {
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			st.IdleS = max(0, int64(time.Since(fi.ModTime()).Seconds()))
			break
		}
	}
	if b, err := lifecycleFile("idle_timeout"); err == nil {
		st.Ephemeral = true
		st.IdleTimeout = strings.TrimSpace(string(b))
		if b, err := os.ReadFile(filepath.Join(LifecycleDir, "ready")); err != nil {
			st.Booting = true
		} else if v := strings.TrimSpace(string(b)); v != "" && v != "0" {
			st.BootFailed = v
		}
		// A pending interruption drains the VM even with no session open to
		// see it, so no new job is placed on it.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if st.Draining {
			// already draining: nothing more a notice could change
		} else if n := SpotNotice(ctx); n != "" {
			st.SpotNotice = n
			if !st.Draining {
				markDraining(root, n)
				st.Draining = true
			}
		}
		cancel()
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			st.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 2 {
				continue
			}
			v, _ := strconv.ParseInt(fs[1], 10, 64)
			switch fs[0] {
			case "MemTotal:":
				st.MemTotalMB = v / 1024
			case "MemAvailable:":
				st.MemAvailMB = v / 1024
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			st.UptimeS = int64(v)
		}
	}
	var sfs syscall.Statfs_t
	if syscall.Statfs(root, &sfs) == nil {
		st.DiskFreeMB = int64(sfs.Bavail) * int64(sfs.Bsize) / (1 << 20)
	}
	repos, _ := os.ReadDir(filepath.Join(root, "repos"))
	for _, r := range repos {
		slots, _ := os.ReadDir(filepath.Join(l.Repo(r.Name()), "slots"))
		for _, sd := range slots {
			n, err := strconv.Atoi(strings.TrimPrefix(sd.Name(), "slot-"))
			if err != nil {
				continue
			}
			dir := l.Slot(r.Name(), n)
			ss := loadSlotState(dir)
			st.Slots = append(st.Slots, SlotInfo{
				Repo: r.Name(), Slot: n, Held: isHeld(filepath.Join(dir, ".lock")),
				LastUsed: ss.LastUsed, Worktree: ss.WorktreeKey, DepsKey: ss.DepsKey,
			})
		}
	}
	return st
}

// PrintStatus writes status JSON.
func PrintStatus(w io.Writer, root string) error {
	return json.NewEncoder(w).Encode(GetStatus(root))
}

// Unlock forcibly reclaims held tokens: it terminates the recorded command
// process group and the helper process that owns the lock.
func Unlock(w io.Writer, root string, token int, all bool) error {
	l := Layout{Root: root}
	n := 0
	for _, h := range readHolders(l) {
		if !all && h.Token != token {
			continue
		}
		if h.ChildPGID > 0 && groupAlive(h.ChildPGID) {
			killGroup(h.ChildPGID, 3*time.Second)
		}
		// Only signal a PID that is still a spotexec helper (a stale record's
		// PID may have been reused by something else), and give it time to
		// clean up (its container) before forcing it.
		if h.HelperPID > 1 && isHelperProcess(h.HelperPID) {
			_ = syscall.Kill(h.HelperPID, syscall.SIGTERM)
			for i := 0; i < 200 && syscall.Kill(h.HelperPID, 0) == nil; i++ {
				time.Sleep(50 * time.Millisecond)
			}
			if syscall.Kill(h.HelperPID, 0) == nil && isHelperProcess(h.HelperPID) {
				_ = syscall.Kill(h.HelperPID, syscall.SIGKILL)
			}
		}
		fmt.Fprintf(w, "released token %d (held by %s pid %d: %s)\n", h.Token, h.Client.Host, h.Client.PID, h.Client.Command)
		n++
	}
	if n == 0 {
		fmt.Fprintln(w, "no matching held slots")
	}
	// Runs waiting for their exact workspace hold nothing to release; they
	// carry on waiting (and may now get it).
	for _, h := range readWaiters(l) {
		fmt.Fprintf(w, "still waiting for workspace slot %d: %s pid %d: %s\n", h.Slot, h.Client.Host, h.Client.PID, h.Client.Command)
	}
	return nil
}

// Prune removes dependency caches and idle slot workspaces not used within
// maxAge. Held slots and caches referenced by any slot are kept.
func Prune(w io.Writer, root string, maxAge time.Duration, dryRun bool) error {
	l := Layout{Root: root}
	cutoff := time.Now().Add(-maxAge)
	var freed int
	repos, _ := os.ReadDir(filepath.Join(root, "repos"))
	for _, r := range repos {
		used := map[string]bool{}
		slots, _ := os.ReadDir(filepath.Join(l.Repo(r.Name()), "slots"))
		live := 0
		for _, sd := range slots {
			dir := filepath.Join(l.Repo(r.Name()), "slots", sd.Name())
			ss := loadSlotState(dir)
			lk, ok, _ := tryLock(filepath.Join(dir, ".lock"))
			if !ok {
				used[ss.DepsKey] = true
				live++
				continue
			}
			if !dryRun {
				_ = os.RemoveAll(filepath.Join(dir, "run")) // stale run scripts (may hold secrets)
			}
			if time.Unix(ss.LastUsed, 0).Before(cutoff) {
				fmt.Fprintf(w, "remove slot %s/%s\n", r.Name(), sd.Name())
				if !dryRun {
					_ = os.RemoveAll(filepath.Join(dir, "workspace"))
					_ = os.Remove(filepath.Join(dir, "manifest.json"))
					_ = os.Remove(filepath.Join(dir, "state.json"))
				}
				freed++
			} else {
				used[ss.DepsKey] = true
				live++
			}
			lk.Unlock()
		}
		deps, _ := os.ReadDir(l.Deps(r.Name()))
		for _, d := range deps {
			name := d.Name()
			key, _, isTmp := strings.Cut(name, ".tmp-")
			if !d.IsDir() || used[key] {
				continue
			}
			p := filepath.Join(l.Deps(r.Name()), name)
			stale := isTmp
			if !isTmp {
				fi, err := os.Stat(filepath.Join(p, ".ready"))
				stale = err != nil || fi.ModTime().Before(cutoff)
			}
			if !stale {
				continue
			}
			// Hold the key's build lock while deleting so an in-progress
			// build (which writes KEY.tmp-PID under KEY.lock) is never removed.
			lk, ok, _ := tryLock(filepath.Join(l.Deps(r.Name()), key+".lock"))
			if !ok {
				continue
			}
			fmt.Fprintf(w, "remove deps cache %s/%s\n", r.Name(), name)
			if !dryRun {
				_ = os.RemoveAll(p)
			}
			lk.Unlock()
			freed++
		}
	}
	// Env files of runs whose helper was killed (they hold job secrets).
	if ents, err := os.ReadDir(filepath.Join(root, "tmp")); err == nil && !dryRun {
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), "env-") && time.Since(fi.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(root, "tmp", e.Name()))
			}
		}
	}
	if d, err := probeDocker(context.Background()); err == nil && !dryRun {
		if len(readHolders(l)) > 0 {
			// A session may be between preparing its image and running it.
			fmt.Fprintln(w, "docker: skipped image prune while slots are in use")
		} else {
			mins := strconv.Itoa(max(1, int(maxAge.Minutes())))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			out, _ := runShell(ctx, d.cmd("image prune -af --filter until="+mins+"m")+" ; "+d.cmd("builder prune -f --filter until="+mins+"m"))
			cancel()
			if t := strings.TrimSpace(string(out)); t != "" {
				fmt.Fprintf(w, "docker: %s\n", firstLines(t, 2))
			}
		}
	}
	fmt.Fprintf(w, "%d item(s) %s\n", freed, map[bool]string{true: "would be removed", false: "removed"}[dryRun])
	return nil
}

// isHelperProcess reports whether pid is alive and (where /proc exists) is a
// spotexec helper.
func isHelperProcess(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return !os.IsNotExist(err) || runtime.GOOS != "linux"
	}
	return strings.Contains(string(b), "spotexec-helper")
}
