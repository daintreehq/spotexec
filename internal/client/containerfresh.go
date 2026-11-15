package client

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/daintreehq/spotexec/internal/config"
)

// CONTAINER_FRESH tells the user that a project container starts fresh each
// run. Shown on every --same-slot run and exec it would be ignored, so it
// appears when it is likely to matter: on a command that installs system
// packages (they won't be there next run), and on this worktree's first
// --same-slot run or exec (which might expect them still to be there).

// containerFreshMarker exists once this worktree has seen the warning.
func containerFreshMarker(w *Workspace) string {
	return filepath.Join(config.StateDir(), "worktrees", w.WorktreeKey+".container-fresh")
}

// claimContainerFresh reports whether this is the first claim for the
// worktree. Exclusive creation gives concurrent runs a single winner. When
// the marker can't be written it claims anyway: an extra warning beats none.
func claimContainerFresh(w *Workspace) bool {
	p := containerFreshMarker(w)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return true
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return !os.IsExist(err)
	}
	f.Close()
	return true
}

// warnContainerFresh shows CONTAINER_FRESH for a run in the project
// container, when it is relevant to this run.
func warnContainerFresh(w *Workspace, o Options, dockerfile string) {
	where := orDefault(dockerfile, "the image")
	if installsSystemPackages(o.Command) {
		claimContainerFresh(w)
		o.Rep.Warn("CONTAINER_FRESH", "this command installs system packages, but this project runs in a container, which starts fresh each run: they won't be there for the next run, even with --same-slot (add them to %s)", where)
		return
	}
	if (o.SameSlot || o.PreferLast) && claimContainerFresh(w) {
		o.Rep.Warn("CONTAINER_FRESH", "this project runs in a container, which starts fresh each run: files in the workspace and $HOME carry over, but system packages installed by an earlier run don't (add them to %s)", where)
	}
}

// pkgInstall matches a system package manager installing something: the
// changes a fresh container drops (unlike files in the workspace or $HOME).
// Best effort: it reads the command line, not the scripts it runs.
var pkgInstall = regexp.MustCompile(`(?:^|[\s;&|(` + "`" + `'"])(?:` +
	`(?:apt-get|apt|aptitude|dnf|yum|microdnf|tdnf|zypper)(?:\s+(?:-[otc]\s+\S+|--option\s+\S+|-\S+))*\s+` +
	`(?:install|reinstall|in|localinstall|groupinstall|group\s+install)\b` +
	`|apk(?:\s+-\S+)*\s+add\b` +
	`|pacman(?:\s+-\S+)*\s+-S[yu]*\b` +
	`|dpkg(?:\s+-\S+)*\s+(?:-i|--install)\b` +
	`|rpm(?:\s+-\S+)*\s+(?:-[iU]\w*|--install|--upgrade)\b` +
	`)`)

// installsSystemPackages reports whether the shell command installs system
// packages.
func installsSystemPackages(cmd string) bool { return pkgInstall.MatchString(cmd) }
