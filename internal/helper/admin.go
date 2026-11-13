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

	"github.com/spotexec/spotexec/internal/protocol"
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
	IdleS       int64             `json:"idle_s"` // seconds since last activity (-1 unknown)
	Ephemeral   bool              `json:"ephemeral"`
	IdleTimeout string            `json:"idle_timeout,omitempty"`
	Holders     []protocol.Holder `json:"holders"`
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
	for _, p := range []string{ActivityFile, filepath.Join(root, "activity")} {
		if fi, err := os.Stat(p); err == nil {
			st.IdleS = int64(time.Since(fi.ModTime()).Seconds())
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(LifecycleDir, "idle_timeout")); err == nil {
		st.Ephemeral = true
		st.IdleTimeout = strings.TrimSpace(string(b))
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
		if h.HelperPID > 1 {
			_ = syscall.Kill(h.HelperPID, syscall.SIGTERM)
			for i := 0; i < 40 && syscall.Kill(h.HelperPID, 0) == nil; i++ {
				time.Sleep(50 * time.Millisecond)
			}
			_ = syscall.Kill(h.HelperPID, syscall.SIGKILL)
		}
		fmt.Fprintf(w, "released token %d (held by %s pid %d: %s)\n", h.Token, h.Client.Host, h.Client.PID, h.Client.Command)
		n++
	}
	if n == 0 {
		fmt.Fprintln(w, "no matching held slots")
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
	if d, err := probeDocker(context.Background()); err == nil && !dryRun {
		hours := strconv.Itoa(int(maxAge.Hours()))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		out, _ := runShell(ctx, d.cmd("image prune -af --filter until="+hours+"h")+" ; "+d.cmd("builder prune -f --filter until="+hours+"h"))
		cancel()
		if t := strings.TrimSpace(string(out)); t != "" {
			fmt.Fprintf(w, "docker: %s\n", firstLines(t, 2))
		}
	}
	fmt.Fprintf(w, "%d item(s) %s\n", freed, map[bool]string{true: "would be removed", false: "removed"}[dryRun])
	return nil
}
