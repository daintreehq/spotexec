// Package client orchestrates a spotexec run: pick a runner, acquire a slot,
// sync the worktree, hydrate dependencies, execute, and write back mutations.
package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/manifest"
	"github.com/daintreehq/spotexec/internal/provider"
)

// Workspace identifies the local worktree being run from.
type Workspace struct {
	Root   string // canonical worktree root
	Git    bool
	GitDir string // per-worktree git dir (the write-back lock lives here)
	// CommonGitDir is the git dir every worktree of the repository shares
	// (GitDir itself outside linked worktrees). Backups and conflict copies
	// live here, so removing a linked worktree doesn't take them with it.
	CommonGitDir string
	RepoID       string // shared across worktrees of the same repo
	RepoName     string
	WorktreeKey  string // unique per host+worktree
	Cwd          string // cwd relative to Root ("." at root)
	// Marked: Root is a Git worktree or holds a .spotexec.toml (the home
	// directory, / and credential directories never count, Git or not). Otherwise it is just the directory
	// spotexec was started in, which must not be synced wholesale (it may
	// be $HOME, holding ~/.aws and ~/.ssh).
	Marked bool
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Resolve finds the workspace for dir.
func Resolve(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	root, isGit := manifest.Root(abs)
	w := &Workspace{Root: root, Git: isGit}
	if _, err := os.Stat(filepath.Join(root, ".spotexec.toml")); (err == nil || isGit) && !config.NeverProject(root) {
		w.Marked = true
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = "."
	}
	w.Cwd = filepath.ToSlash(rel)
	ident := root
	name := filepath.Base(root)
	if isGit {
		w.GitDir, _ = manifest.GitDir(root)
		w.CommonGitDir = commonGitDir(root, w.GitDir)
		if out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output(); err == nil {
			if u := normalizeRemote(strings.TrimSpace(string(out))); u != "" {
				ident = u
				// Lowercase as before: only the hash tells case-distinct
				// paths apart, so a lowercase remote keeps its RepoID.
				name = strings.ToLower(filepath.Base(u))
			}
		} else if out, err := exec.Command("git", "-C", root, "rev-parse", "--git-common-dir").Output(); err == nil {
			// No origin: worktrees of one repo share the common git dir.
			cd := strings.TrimSpace(string(out))
			if !filepath.IsAbs(cd) {
				cd = filepath.Join(root, cd)
			}
			ident = filepath.Clean(cd)
			name = filepath.Base(filepath.Dir(ident))
		}
	}
	name = strings.Trim(nonAlnum.ReplaceAllString(name, "-"), "-.")
	if name == "" {
		name = "repo"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	w.RepoName = name
	h := sha256.Sum256([]byte(ident))
	w.RepoID = name + "-" + hex.EncodeToString(h[:])[:8]
	host, _ := os.Hostname()
	k := sha256.Sum256([]byte(host + "\x00" + root))
	w.WorktreeKey = hex.EncodeToString(k[:])[:8]
	return w, nil
}

// commonGitDir is the repository's common git dir for the worktree at root,
// or gitDir when git can't say.
func commonGitDir(root, gitDir string) string {
	// --path-format=absolute (git 2.31+) gives the canonical path; older git
	// echoes the unknown option back, and prints the dir relative to root.
	out, err := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil || strings.Contains(strings.TrimSpace(string(out)), "\n") {
		out, err = exec.Command("git", "-C", root, "rev-parse", "--git-common-dir").Output()
	}
	if err != nil {
		return gitDir
	}
	d := strings.TrimSpace(string(out))
	if d == "" {
		return gitDir
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(root, d)
	}
	return d
}

// normalizeRemote strips scheme, credentials and .git so https and ssh
// remotes of the same repository map to one ID ("host/path"). Only the
// host is case-folded: hosting can be case-sensitive, so the path keeps
// its case, and any "@" in it. Local paths come back unchanged.
func normalizeRemote(u string) string {
	// HTTP never sends a fragment, so it names nothing; elsewhere "#" is
	// part of the path.
	if l := strings.ToLower(u); strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		u, _, _ = strings.Cut(u, "#")
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	var host, p string
	if i := strings.Index(u, "://"); i >= 0 {
		// scheme://[userinfo@]host[:port]/path
		host = u[i+3:]
		if j := strings.IndexByte(host, '/'); j >= 0 {
			host, p = host[:j], host[j:]
		}
	} else if i := scpColon(u); i >= 0 {
		// [user@]host:path
		host, p = u[:i], "/"+u[i+1:]
	} else {
		return u
	}
	if j := strings.LastIndexByte(host, '@'); j >= 0 {
		host = host[j+1:]
	}
	return strings.ToLower(host) + p
}

// scpColon is the index of the colon ending the host of an scp-like
// remote ([user@]host:path, host possibly a bracketed IPv6 address), or -1
// when u is a local path: no colon before its first slash.
func scpColon(u string) int {
	head := u
	if j := strings.IndexByte(u, '/'); j >= 0 {
		head = u[:j]
	}
	from := 0
	if k := strings.IndexByte(head, '['); k >= 0 && (k == 0 || head[k-1] == '@') {
		if e := strings.IndexByte(head[k:], ']'); e >= 0 {
			from = k + e
		}
	}
	if i := strings.IndexByte(head[from:], ':'); i >= 0 {
		return from + i
	}
	return -1
}

// LastRun records where this worktree last ran (for exec/pull).
type LastRun struct {
	Runner     string          `json:"runner"`
	InstanceID string          `json:"instance_id,omitempty"`
	Image      string          `json:"image,omitempty"` // last container image prepared for this worktree
	Slot       int             `json:"slot"`
	Target     provider.Target `json:"target"`
	WorkRoot   string          `json:"work_root"`
	RemoteWS   string          `json:"remote_workspace"`
}

func lastRunPath(w *Workspace) string {
	return filepath.Join(config.StateDir(), "worktrees", w.WorktreeKey+".json")
}

// LoadLastRun reads the last-run record.
func LoadLastRun(w *Workspace) (*LastRun, error) {
	b, err := os.ReadFile(lastRunPath(w))
	if err != nil {
		return nil, err
	}
	var lr LastRun
	return &lr, json.Unmarshal(b, &lr)
}

// SaveLastRun saves the last-run record.
func SaveLastRun(w *Workspace, lr LastRun) { saveLastRun(w, lr) }

func saveLastRun(w *Workspace, lr LastRun) {
	p := lastRunPath(w)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(lr)
	// Atomic: a concurrent reader never sees a half-written record.
	f, err := os.CreateTemp(filepath.Dir(p), ".lastrun-*")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr != nil || cerr != nil {
		os.Remove(f.Name())
		return
	}
	if os.Rename(f.Name(), p) != nil {
		os.Remove(f.Name())
	}
}

// ManifestOptions are the manifest.Build options for project p rooted at
// root. A container's Dockerfile and the .dockerignore files that can apply
// to it sync even inside built-in excludes or Git-ignored paths, Git or
// not: the runner builds the image from the synced copy. Its directory and
// build context sync whole under .spotexec/, and by the usual rules
// (.gitignore included) anywhere else.
func ManifestOptions(root string, p *config.Project) manifest.Options {
	o := manifest.Options{Root: root, Exclude: p.Sync.Exclude, Source: p.Sync.Source, Protect: p.Deps.CacheDirs}
	if df := p.Container.Dockerfile; df != "" && p.Container.Image == "" {
		ctx := p.Container.Context
		if ctx == "" {
			ctx = path.Dir(df)
		}
		// The Dockerfile's own directory too: with context = "." its
		// support files (.spotexec/setup.sh) are still build inputs.
		o.Include = []string{df, df + ".dockerignore", path.Join(ctx, ".dockerignore"), path.Dir(df), ctx}
	}
	return o
}
