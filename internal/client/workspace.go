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
	Root        string // canonical worktree root
	Git         bool
	GitDir      string // per-worktree git dir (conflicts live here)
	RepoID      string // shared across worktrees of the same repo
	RepoName    string
	WorktreeKey string // unique per host+worktree
	Cwd         string // cwd relative to Root ("." at root)
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
		if out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output(); err == nil {
			if u := normalizeRemote(strings.TrimSpace(string(out))); u != "" {
				ident = u
				name = filepath.Base(u)
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

// normalizeRemote strips scheme, credentials and .git so https and ssh
// remotes of the same repository map to one ID.
func normalizeRemote(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.LastIndex(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.Replace(u, ":", "/", 1)
	return strings.ToLower(u)
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
// root. A container's Dockerfile, its companion .dockerignore and its build
// context sync even inside built-in excludes such as .spotexec/, Git or
// not: the runner builds the image from the synced copy.
func ManifestOptions(root string, p *config.Project) manifest.Options {
	o := manifest.Options{Root: root, Exclude: p.Sync.Exclude, Source: p.Sync.Source, Protect: p.Deps.CacheDirs}
	if df := p.Container.Dockerfile; df != "" && p.Container.Image == "" {
		ctx := p.Container.Context
		if ctx == "" {
			ctx = path.Dir(df)
		}
		// The Dockerfile's own directory too: with context = "." its
		// support files (.spotexec/setup.sh) are still build inputs.
		o.Include = []string{df, df + ".dockerignore", path.Dir(df), ctx}
	}
	return o
}
