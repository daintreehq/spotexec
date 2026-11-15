package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/daintreehq/spotexec/internal/client"
	"github.com/daintreehq/spotexec/internal/config"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// runFlags are shared by the shorthand form, run, exec, and tasks.
type runFlags struct {
	timeout       time.Duration
	queueTimeout  time.Duration
	sync          string
	writeback     string
	pool          []string
	runner        string
	exclusive     bool
	pull          []string
	json, pretty  bool
	quiet         bool
	eventsFile    string
	noDeps        bool
	noSetup       bool
	forceLarge    bool
	fallbackLocal bool
	noBoot        bool
	dir           string
	noRetry       bool
	priority      int
	prioritySet   bool // --priority or SPOTEXEC_PRIORITY given (0 included)
	sameSlot      bool
	group         bool
	rtkFilter     string
	fs            *pflag.FlagSet // to tell explicit flags from defaults
}

func (f *runFlags) register(fs *pflag.FlagSet) {
	f.fs = fs
	fs.DurationVar(&f.timeout, "timeout", 0, "wall-clock limit for the command (default 5m or .spotexec.toml timeout)")
	fs.DurationVar(&f.queueTimeout, "queue-timeout", 0, "how long to wait for a free slot (default 10m or .spotexec.toml queue_timeout)")
	fs.StringVar(&f.sync, "sync", "", "sync mode: auto (bidirectional), in (no write-back), none")
	fs.StringVar(&f.writeback, "writeback", "", "write-back policy: always, on-success, never")
	fs.StringSliceVar(&f.pool, "pool", nil, "runner names to use (comma-separated)")
	fs.StringVarP(&f.runner, "runner", "r", "", "use only this runner")
	fs.BoolVar(&f.exclusive, "exclusive", false, "take every slot on the runner")
	fs.StringArrayVar(&f.pull, "pull", nil, "also pull these gitignored paths/globs back after the run (repeatable)")
	fs.BoolVar(&f.json, "json", false, "NDJSON lifecycle events on stderr (default with SPOTEXEC_AGENT=1)")
	fs.BoolVar(&f.pretty, "pretty", false, "human spinner output even when not a TTY")
	fs.BoolVarP(&f.quiet, "quiet", "q", false, "warnings only: no progress or summary")
	fs.StringVar(&f.eventsFile, "events-file", "", "also append NDJSON events to this file")
	fs.BoolVar(&f.noDeps, "no-deps", false, "skip dependency hydration")
	fs.BoolVar(&f.noSetup, "no-setup", false, "skip the project's [setup] step")
	fs.BoolVar(&f.forceLarge, "force-large-sync", false, "allow syncing more than 100k files / 5 GiB")
	fs.BoolVar(&f.fallbackLocal, "fallback-local", false, "run locally if no runner is available")
	fs.BoolVar(&f.noBoot, "no-boot", false, "never provision ephemeral runners for this command")
	fs.StringVarP(&f.dir, "dir", "C", "", "run as if started in this directory")
	fs.BoolVar(&f.noRetry, "no-retry", false, "do not replay the run if its runner is lost (spot reclaim)")
	fs.IntVar(&f.priority, "priority", 0, "queue priority when every slot is busy (higher runs first)")
	fs.BoolVarP(&f.sameSlot, "same-slot", "s", false, "run in the slot (same VM) this worktree last used, waiting if it's busy; with --sync none, skip the sync")
	fs.StringVar(&f.rtkFilter, "rtk-filter", "", "compact stdout with RTK once the command ends (rtk pipe -f `NAME`): cargo-test for cargo test, go-test for go test -json; the full output is kept when it fails")
	fs.BoolVar(&f.group, "group", false, "parallel and sharded tasks: print each part's output as one block when it finishes (default with SPOTEXEC_AGENT=1 when stdout isn't a terminal)")
}

func (f *runFlags) reporter() (ui.Reporter, error) {
	mode := ui.ModeAuto
	switch {
	case f.quiet:
		mode = ui.ModeQuiet
	case f.json:
		mode = ui.ModeJSON
	case f.pretty:
		mode = ui.ModePretty
	case f.changed("json") && os.Getenv("SPOTEXEC_AGENT") == "1":
		// --json=false beats SPOTEXEC_AGENT=1's NDJSON default.
		mode = ui.ModePlain
		if ui.IsTerminal(os.Stderr) {
			mode = ui.ModePretty
		}
	}
	return ui.New(mode, os.Stderr, f.eventsFile)
}

// applyEnv fills in SPOTEXEC_* defaults for every flag not given on the
// command line: an explicit flag always wins, whatever its value (so
// --json=false, --priority 0 and --pool beat the environment).
func (f *runFlags) applyEnv() {
	if !f.changed("timeout") {
		if d, err := time.ParseDuration(os.Getenv("SPOTEXEC_TIMEOUT")); err == nil {
			f.timeout = d
		}
	}
	if !f.changed("sync") {
		if v := os.Getenv("SPOTEXEC_SYNC"); v != "" {
			f.sync = v
		}
	}
	// A runner named in the environment narrows the default pool, never one
	// given with --pool.
	if !f.changed("runner") && !f.changed("pool") {
		if v := os.Getenv("SPOTEXEC_RUNNER"); v != "" {
			f.runner = v
		}
	}
	if os.Getenv("SPOTEXEC_JSON") == "1" && !f.changed("json") && !f.changed("pretty") && !f.changed("quiet") {
		f.json = true
	}
	if os.Getenv("SPOTEXEC_SAME_SLOT") == "1" && !f.changed("same-slot") {
		f.sameSlot = true
	}
	if f.changed("priority") {
		f.prioritySet = true
	} else if n, err := strconv.Atoi(os.Getenv("SPOTEXEC_PRIORITY")); err == nil {
		f.priority, f.prioritySet = n, true
	}
}

// validate rejects run settings no run could honour, before any runner is
// acquired: an unknown --writeback must never fall through to writing back.
func (f *runFlags) validate() error {
	// An explicitly empty value would silently mean "the default".
	for name, empty := range map[string]bool{"sync": f.sync == "", "writeback": f.writeback == "", "runner": f.runner == "", "pool": len(f.pool) == 0} {
		if empty && f.changed(name) {
			return fmt.Errorf("--%s needs a value", name)
		}
	}
	if f.sync != "" && f.sync != "auto" && f.sync != "in" && f.sync != "none" {
		return fmt.Errorf("--sync must be auto, in or none (got %q)", f.sync)
	}
	if f.writeback != "" && f.writeback != "always" && f.writeback != "on-success" && f.writeback != "never" {
		return fmt.Errorf("--writeback must be always, on-success or never (got %q)", f.writeback)
	}
	return nil
}

// grouped reports whether the parts of fan-out task t print their output as
// one block each: --group or --group=false, else the task's group setting,
// else agent (SPOTEXEC_AGENT=1 with stdout not a terminal).
func (f *runFlags) grouped(t *config.Task, agent bool) bool {
	switch {
	case f.changed("group"):
		return f.group
	case t.Group != nil:
		return *t.Group
	}
	return agent
}

// changed reports whether a flag was given on the command line (explicit
// flags beat SPOTEXEC_* variables).
func (f *runFlags) changed(name string) bool {
	return f.fs != nil && f.fs.Changed(name)
}

// loaded is everything a run needs.
type loaded struct {
	cfg *config.Config
	ws  *client.Workspace
}

func load(dir string) (*loaded, error) {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	ws, err := client.Resolve(dir)
	if err != nil {
		return nil, err
	}
	g, err := config.LoadGlobal()
	if err != nil {
		return nil, err
	}
	pp := config.FindProject(filepath.Join(ws.Root, ws.Cwd), ws.Root)
	p, err := config.LoadProject(pp)
	if err != nil {
		return nil, err
	}
	cfg := &config.Config{Global: g, Project: p}
	if err := cfg.MergeProjectRunners(); err != nil {
		return nil, err
	}
	return &loaded{cfg: cfg, ws: ws}, nil
}

// buildCommand turns argv into a remote shell line. A single argument is
// taken as a shell snippet (so `sx 'pnpm build && pnpm test'` works); several
// are quoted individually. Absolute local paths inside the worktree are
// mapped to the remote workspace: in a snippet to "$SPOTEXEC_WORKSPACE" (so
// they stay right after a `cd`), or, where the shell won't expand that
// (single quotes, separate arguments), to a path relative to the mirrored
// working directory.
func buildCommand(ws *client.Workspace, args []string) string {
	toRoot := "."
	if ws.Cwd != "." {
		toRoot = strings.TrimSuffix(strings.Repeat("../", strings.Count(ws.Cwd, "/")+1), "/")
	}
	if len(args) == 1 {
		return rewriteRoot(args[0], ws.Root, toRoot, true)
	}
	q := make([]string, len(args))
	assigning := true
	for i, a := range args {
		// Leading NAME=value words stay environment assignments: quoting
		// them whole would make bash run "NAME=value" as a command. The
		// value is quoted on its own, and since the shell expands it, a
		// worktree path in it can become the absolute remote one.
		if name, value, ok := envAssignment(a); ok && assigning {
			q[i] = name + "=" + quoteValue(value, ws.Root)
			continue
		}
		assigning = false
		q[i] = transport.ShellQuote(rewriteRoot(a, ws.Root, toRoot, false))
	}
	return strings.Join(q, " ")
}

// quoteValue shell-quotes an assignment's value, with each worktree path in
// it (matched before quoting) turned into the remote workspace's.
func quoteValue(v, root string) string {
	var b strings.Builder
	last := 0
	for i := 0; i+len(root) <= len(v); i++ {
		if strings.HasPrefix(v[i:], root) && boundaryBefore(v, i) && boundaryAfter(v, i+len(root)) {
			if i > last {
				b.WriteString(transport.ShellQuote(v[last:i]))
			}
			b.WriteString(`"${SPOTEXEC_WORKSPACE:?}"`)
			last = i + len(root)
			i = last - 1
		}
	}
	if last < len(v) || last == 0 {
		b.WriteString(transport.ShellQuote(v[last:]))
	}
	return b.String()
}

// envAssignment splits a NAME=value word whose name is a valid shell
// variable name.
func envAssignment(s string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(s, "=")
	if !ok || name == "" {
		return "", "", false
	}
	for i, c := range name {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return "", "", false
		}
	}
	return name, value, true
}

// rewriteRoot replaces root where it appears as a whole path (followed by
// '/', the end, or a shell delimiter, and not as part of a longer name such
// as /repo-other).
func rewriteRoot(s, root, toRoot string, snippet bool) string {
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(s); {
		c := s[i]
		if strings.HasPrefix(s[i:], root) && boundaryBefore(s, i) && boundaryAfter(s, i+len(root)) {
			switch {
			case snippet && !inSingle:
				b.WriteString("${SPOTEXEC_WORKSPACE:?}") // aborts rather than expanding to "" if unset
			case snippet:
				// Inside single quotes: close them, splice the variable in
				// double quotes, reopen ('a/x' -> ''"$W"'/x').
				b.WriteString(`'"${SPOTEXEC_WORKSPACE:?}"'`)
			default:
				b.WriteString(toRoot)
			}
			i += len(root)
			continue
		}
		switch {
		case c == '\\' && !inSingle && i+1 < len(s):
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i += 2
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func boundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	return strings.ContainsRune(" \t\n'\"=:(;&|", rune(s[i-1]))
}

func boundaryAfter(s string, i int) bool {
	if i == len(s) {
		return true
	}
	return strings.ContainsRune("/ \t\n'\";&|):", rune(s[i]))
}

// signals turns SIGINT/SIGTERM into a cancellable context plus an interrupt
// stream (each ^C forwards a cancel to the remote command; the third exits).
func signals() (context.Context, <-chan struct{}, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	intr := make(chan struct{}, 8)
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		n := 0
		for range sig {
			n++
			cancel()
			select {
			case intr <- struct{}{}:
			default:
			}
			if n >= 3 {
				fmt.Fprintln(os.Stderr, "spotexec: forced exit (remote command may still be cleaning up)")
				os.Exit(130)
			}
		}
	}()
	return ctx, intr, func() { signal.Stop(sig); cancel() }
}

// runSpec is one command to execute.
type runSpec struct {
	label      string
	command    string
	display    string
	timeout    time.Duration
	sync       string
	exclusive  bool
	pool       []string
	priority   int
	preferLast bool
	syncOnly   bool
	noSetup    bool              // the task opts out of [setup]
	env        map[string]string // set for the command (a shard's number)
	waitPush   bool              // container build: push a fresh image before returning
}

// pinnedSync: spotexec sync and container build always sync, and exec
// never does, whatever --sync or SPOTEXEC_SYNC says.
func (s runSpec) pinnedSync() bool { return s.syncOnly || s.preferLast }

// check rejects, before any runner is acquired, a run that couldn't do what
// was asked.
func (f *runFlags) check(s runSpec) error {
	if err := f.validate(); err != nil {
		return err
	}
	if s.pinnedSync() && f.changed("sync") && f.sync != s.sync {
		if s.syncOnly {
			return fmt.Errorf("--sync %s doesn't apply here: this command always syncs", f.sync)
		}
		return fmt.Errorf("--sync %s doesn't apply here: exec never syncs (use sx --same-slot to sync first)", f.sync)
	}
	return nil
}

func (f *runFlags) options(l *loaded, s runSpec, rep ui.Reporter, intr <-chan struct{}, stdout, stderr io.Writer) client.Options {
	p := l.cfg.Project
	o := client.Options{
		Command: s.command, Display: s.display, Timeout: p.Timeout.Duration, Grace: p.Grace.Duration,
		QueueTimeout: p.QueueTimeout.Duration, Sync: p.Sync.Mode, Writeback: p.Sync.Writeback,
		Pool: f.pool, Exclusive: f.exclusive || s.exclusive, Pull: f.pull, NoDeps: f.noDeps,
		NoSetup:        f.noSetup || s.noSetup,
		ForceLargeSync: f.forceLarge, Stdout: stdout, Stderr: stderr, Rep: rep, Interrupt: intr,
		Label: s.label, RetryPreempt: !f.noRetry, NoBoot: f.noBoot,
		PreferLast: s.preferLast, SyncOnly: s.syncOnly, WaitPush: s.waitPush, Priority: s.priority,
		Env: s.env,
	}
	if f.prioritySet {
		o.Priority = f.priority
	}
	if s.timeout > 0 {
		o.Timeout = s.timeout
	}
	if s.sync != "" {
		o.Sync = s.sync
	}
	if len(s.pool) > 0 && len(f.pool) == 0 {
		o.Pool = s.pool
	}
	if f.timeout > 0 {
		o.Timeout = f.timeout
	}
	if f.queueTimeout > 0 {
		o.QueueTimeout = f.queueTimeout
	}
	if f.sync != "" && !s.pinnedSync() {
		o.Sync = f.sync
	}
	if f.writeback != "" {
		o.Writeback = f.writeback
	}
	if f.runner != "" {
		o.Pool = []string{f.runner}
	}
	// --same-slot and exec go back to the runner this worktree last used; an
	// explicit --runner or --pool must include it.
	if f.changed("runner") || f.changed("pool") {
		o.LastWithin = o.Pool
	}
	if f.sameSlot {
		// Without a sync, --same-slot is exec: the slot must still hold
		// this worktree's files, and nothing comes back.
		if o.Sync == "none" {
			o.PreferLast = true
		} else {
			o.SameSlot = true
		}
	}
	return o
}

// execute runs one spec end to end and returns the process exit code.
func (f *runFlags) execute(l *loaded, s runSpec) int {
	// A scale-out started by this process must record its instance ID before
	// we exit, or the pool could lose track of it for a while.
	defer waitLaunches()
	f.applyEnv()
	if err := f.check(s); err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	rep, err := f.reporter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	ctx, intr, stop := signals()
	defer stop()
	var stdout io.Writer = os.Stdout
	finish := func(code int) int { return code }
	if f.rtkFilter != "" {
		if ro, err := newRTKOutput(f.rtkFilter, os.Stdout, rep); err != nil {
			rep.Warn("RTK_FILTER", "can't compact the output with RTK (%v); it is shown unfiltered", err)
		} else {
			stdout = ro
			// Before the summary, so it stays the last line.
			finish = func(code int) int { ro.finish(code != 0); return code }
		}
	}
	o := f.options(l, s, rep, intr, stdout, os.Stderr)
	res, err := client.Run(ctx, l.cfg, l.ws, o)
	if err != nil {
		if ctx.Err() != nil {
			rep.Clear()
			return finish(client.ExitInterrupted)
		}
		if f.fallsBack(l, o, res, err) {
			rep.Warn("LOCAL_FALLBACK", "no runner available (%v); running locally", err)
			return finish(runLocal(ctx, intr, l, o, rep, 0, 1))
		}
		finish(client.ExitInfra)
		rep.Clear()
		rep.Event("error", map[string]any{"code": "SPOTEXEC_ERROR", "message": err.Error()})
		if !rep.IsJSON() {
			fmt.Fprintf(os.Stderr, "spotexec: %v\n", err)
			depsDetail(os.Stderr, "", err)
		}
		return client.ExitInfra
	}
	if ctx.Err() != nil && !res.Canceled {
		// Interrupted after the command finished (while the session closed).
		res.Canceled, res.ExitCode = true, client.ExitInterrupted
	}
	summary := fmt.Sprintf("%s on %s#%d", ui.Dur(res.Duration), res.Runner, res.Slot)
	if res.Synced > 0 {
		summary += fmt.Sprintf(" · synced %d files (%s)", res.Synced, ui.Bytes(res.SyncBytes))
	}
	if res.DepsInstalled > 0 {
		summary += fmt.Sprintf(" · installed dependencies (%s)", ui.Dur(res.DepsInstalled))
	}
	if n := len(res.Mutated) + len(res.Deleted); n > 0 {
		summary += fmt.Sprintf(" · pulled %d modified file%s", n, plural(n))
	}
	if len(res.Conflicts) > 0 {
		summary += fmt.Sprintf(" · %d conflict%s", len(res.Conflicts), plural(len(res.Conflicts)))
	}
	if s.syncOnly && res.Image != "" {
		summary += " · image " + res.Image
	}
	switch {
	case res.Canceled:
		// Even if the command handled the signal and exited 0: it was cut
		// short, so `sx a && sx b` must not go on to b.
		summary += " · interrupted"
	case res.ExitCode != 0:
		summary += fmt.Sprintf(" · exit %d", res.ExitCode)
	}
	finish(res.ExitCode)
	rep.Done(summary, res.ExitCode == 0)
	return res.ExitCode
}

// executeLast runs in the last-used runner and slot (exec).
func (f *runFlags) executeLast(l *loaded, s runSpec) int {
	s.preferLast = true
	return f.execute(l, s)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// taskCommand expands a task's run template with extra args. Arguments are
// never spliced into the shell text: they become positional parameters and
// {args} expands to "$@", so quoting in the template cannot be broken out of.
func taskCommand(run string, args []string) string {
	if len(args) == 0 {
		// A quoted placeholder must vanish with its quotes, or the command
		// would get an empty argument ("").
		return strings.TrimSpace(strings.NewReplacer(`"{args}"`, "", `'{args}'`, "", "{args}", "").Replace(run))
	}
	// A multi-line TOML string ends in a newline: "$@" must stay on the
	// command's own line.
	run = strings.TrimRight(run, " \t\r\n")
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = transport.ShellQuote(a)
	}
	body := run
	if strings.Contains(run, "{args}") {
		// A quoted placeholder ("{args}" or '{args}') must become a bare
		// "$@" too, or the arguments would be word-split or not expanded.
		body = strings.NewReplacer(`"{args}"`, `"$@"`, `'{args}'`, `"$@"`, "{args}", `"$@"`).Replace(run)
	} else {
		body = run + ` "$@"`
	}
	return "set -- " + strings.Join(q, " ") + "\n" + body
}

// taskPart is one run of a task's fan-out: a task of a parallel task, or
// one shard of a sharded task.
type taskPart struct {
	name          string // the task
	label         string // output prefix and summaries: "test" or "test 3/8"
	shard, shards int    // 1-based; 0 when not sharded
	t             *config.Task
	args          []string
	noSetup       bool // the task, or the parallel task it is part of, sets setup = false
}

// taskParts expands a task into the runs it fans out to: each task of a
// parallel task (a sharded one into its shards), or each shard. Only a
// sharded task's own arguments reach its shards.
func taskParts(p *config.Project, name string, args []string) []taskPart {
	t := p.Tasks[name]
	if len(t.Parallel) == 0 {
		return shardParts(name, t, args)
	}
	var parts []taskPart
	for _, sub := range t.Parallel {
		for _, pt := range shardParts(sub, p.Tasks[sub], nil) {
			pt.noSetup = pt.noSetup || t.SkipsSetup()
			parts = append(parts, pt)
		}
	}
	return parts
}

func shardParts(name string, t *config.Task, args []string) []taskPart {
	if t.Shards == 0 {
		return []taskPart{{name: name, label: name, t: t, args: args, noSetup: t.SkipsSetup()}}
	}
	parts := make([]taskPart, t.Shards)
	for i := range parts {
		parts[i] = taskPart{name: name, label: fmt.Sprintf("%s %d/%d", name, i+1, t.Shards), shard: i + 1, shards: t.Shards, t: t, args: args, noSetup: t.SkipsSetup()}
	}
	return parts
}

// spec builds the part's run: {shard} and {shards} substituted, and
// SPOTEXEC_SHARD / SPOTEXEC_SHARDS in its environment.
func (pt taskPart) spec(l *loaded) runSpec {
	run := pt.t.Run
	var env map[string]string
	if pt.shards > 0 {
		run = strings.NewReplacer("{shard}", strconv.Itoa(pt.shard), "{shards}", strconv.Itoa(pt.shards)).Replace(run)
		env = map[string]string{"SPOTEXEC_SHARD": strconv.Itoa(pt.shard), "SPOTEXEC_SHARDS": strconv.Itoa(pt.shards)}
	}
	cmd := buildTaskLine(l, run, pt.args)
	t := pt.t
	return runSpec{label: pt.label, command: cmd, display: pt.label + ": " + cmd, timeout: t.Timeout.Duration, sync: t.Sync, exclusive: t.Exclusive, pool: t.Pool, priority: t.Priority, env: env, noSetup: pt.noSetup}
}

// runTask executes a named task (possibly a parallel or sharded fan-out).
func (f *runFlags) runTask(l *loaded, name string, args []string) int {
	t := l.cfg.Project.Tasks[name]
	defer waitLaunches()
	parts := taskParts(l.cfg.Project, name, args)
	if len(t.Parallel) == 0 && len(parts) == 1 {
		// A plain task, or one with a single shard: an ordinary run.
		return f.execute(l, parts[0].spec(l))
	}
	f.applyEnv()
	if err := f.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	if f.sameSlot {
		if len(t.Parallel) > 0 {
			fmt.Fprintf(os.Stderr, "spotexec: --same-slot can't apply to %s: it's a parallel task, whose parts run on several slots at once\n", name)
		} else {
			fmt.Fprintf(os.Stderr, "spotexec: --same-slot can't apply to %s: it's a sharded task, whose %d shards run on several slots at once\n", name, t.Shards)
		}
		return client.ExitInfra
	}
	if f.rtkFilter != "" {
		fmt.Fprintf(os.Stderr, "spotexec: --rtk-filter can't apply to %s: its parts' outputs are printed separately\n", name)
		return client.ExitInfra
	}
	if len(args) > 0 && len(t.Parallel) > 0 {
		fmt.Fprintf(os.Stderr, "spotexec: %s is a parallel task (%s), so it can't take extra arguments; run a part on its own: sx %s %s\n", name, strings.Join(t.Parallel, ", "), t.Parallel[0], strings.Join(args, " "))
		return client.ExitInfra
	}
	rep, err := f.reporter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	group := f.grouped(t, os.Getenv("SPOTEXEC_AGENT") == "1" && !ui.IsTerminal(os.Stdout))
	ctx, intr, stop := signals()
	defer stop()
	// mu guards the parts' bookkeeping and everything they print, so a
	// grouped part's block, error and summary go out with nothing between.
	var mu sync.Mutex
	outs := make([]partOutcome, len(parts))
	// Each part gets its own context, so fail_fast can stop siblings that
	// are still queued, syncing or installing (not only running ones).
	subCtx := make([]context.Context, len(parts))
	subCancel := make([]context.CancelFunc, len(parts))
	for i := range parts {
		subCtx[i], subCancel[i] = context.WithCancel(ctx)
		defer subCancel[i]()
	}
	stopped := make([]bool, len(parts)) // cancelled by fail_fast
	records := make([]*client.LastRun, len(parts))
	done := make([]bool, len(parts))
	intrs := make([]chan struct{}, len(parts))
	for i := range intrs {
		intrs[i] = make(chan struct{}, 4)
	}
	go func() {
		for range intr {
			for _, c := range intrs {
				select {
				case c <- struct{}{}:
				default:
				}
			}
		}
	}()
	failOnce := sync.Once{}
	var wg sync.WaitGroup
	start := time.Now()
	for i, part := range parts {
		wg.Add(1)
		go func(i int, part taskPart) {
			defer wg.Done()
			sr := &taskReporter{Reporter: rep, task: part.name, shard: part.shard, shards: part.shards, mu: &mu}
			prefix := fmt.Sprintf("[%s] ", part.label)
			var grp *ui.Group
			var stdout, stderr io.Writer
			if group {
				grp = ui.NewGroup(os.Stdout, os.Stderr, prefix, &mu)
				stdout, stderr = grp.Stdout(), grp.Stderr()
			} else {
				stdout, stderr = ui.NewPrefixWriter(os.Stdout, prefix, &mu), ui.NewPrefixWriter(os.Stderr, prefix, &mu)
			}
			o := f.options(l, part.spec(l), sr, intrs[i], stdout, stderr)
			// Parts race to sync: which one saves the last-run record is
			// decided below, once they are all done.
			o.DeferLastRun = true
			subStart := time.Now()
			res, err := client.Run(subCtx[i], l.cfg, l.ws, o)
			interrupted := ctx.Err() != nil
			out := partOutcome{part: part, code: res.ExitCode, runner: res.Runner, slot: res.Slot}
			if err != nil && subCtx[i].Err() == nil && f.fallsBack(l, o, res, err) {
				sr.Warn("LOCAL_FALLBACK", "no runner available (%v); running %s locally", err, part.label)
				out.code, err = runLocal(subCtx[i], intrs[i], l, o, sr, i, len(parts)), nil
				out.runner, out.slot, out.local = "", 0, true
				interrupted = ctx.Err() != nil
			}
			if err != nil {
				out.code = client.ExitInfra
			}
			code := out.code
			out.took = res.Duration
			if out.took == 0 {
				out.took = time.Since(subStart)
			}
			mu.Lock()
			switch {
			case code == 0 && !res.Canceled:
				// Finished, even if fail_fast reached for it just after.
				out.status = partPassed
			case stopped[i]:
				out.status = partStopped
			case res.Canceled || interrupted:
				// Cut short by an interrupt: even a command that handled
				// the signal and exited 0 didn't finish.
				out.status = partInterrupted
			default:
				out.status = partFailed
			}
			outs[i], done[i], records[i] = out, true, res.LastRun
			// Cancelled by fail_fast: not a failure of its own, so its
			// output is printed but not its error or summary.
			wasStopped := stopped[i]
			mu.Unlock()
			// report runs with mu held.
			report := func() {
				if wasStopped {
					return
				}
				if err != nil {
					sr.event("error", map[string]any{"code": "SPOTEXEC_ERROR", "message": err.Error()})
					if !rep.IsJSON() {
						fmt.Fprintf(os.Stderr, "%sspotexec: %v\n", prefix, err)
						depsDetail(os.Stderr, prefix, err)
					}
				}
				summary := fmt.Sprintf("%s · %s%s", part.label, ui.Dur(out.took), out.where())
				if res.DepsInstalled > 0 {
					summary += fmt.Sprintf(" · installed dependencies (%s)", ui.Dur(res.DepsInstalled))
				}
				if code != 0 {
					summary += fmt.Sprintf(" · exit %d", code)
				}
				sr.Reporter.PartDone(summary, code == 0)
			}
			// The siblings stop before this part's (possibly long) block
			// is printed.
			defer func() {
				if grp == nil {
					mu.Lock()
					report()
					mu.Unlock()
				} else if ferr := grp.Flush(report); ferr != nil {
					sr.Warn("OUTPUT_GROUP", "%s: couldn't hold its output to print as one block (%v), so some of it was printed as it came", part.label, ferr)
				}
			}()
			// After an interrupt every part is already stopping, and is
			// interrupted rather than stopped by fail_fast.
			if code != 0 && t.FailFast && !wasStopped && ctx.Err() == nil {
				failOnce.Do(func() {
					for j, c := range intrs {
						mu.Lock()
						skip := j == i || done[j]
						if !skip {
							stopped[j] = true
						}
						mu.Unlock()
						if skip {
							continue
						}
						select { // never block on a full buffer
						case c <- struct{}{}:
						default:
						}
						subCancel[j]()
					}
				})
			}
		}(i, part)
	}
	wg.Wait()
	saveFirstLastRun(l.ws, records)
	// Interrupted: 130. A spotexec failure (125) outranks a command's own
	// failure, so agents can tell "tests failed" from "couldn't run tests".
	exit := 0
	for _, o := range outs {
		if o.status == partStopped {
			continue // the failure that stopped it is what counts
		}
		if o.code != 0 && (exit == 0 || o.code == client.ExitInfra) {
			exit = o.code
		}
	}
	interrupted := ctx.Err() != nil
	if interrupted {
		exit = client.ExitInterrupted
	}
	took := time.Since(start)
	result := map[string]any{"task": name, "exit_code": exit, "duration_ms": took.Milliseconds(), "parts": resultParts(outs)}
	if len(t.Parallel) > 0 {
		result["parallel"] = t.Parallel
	} else {
		result["shards"] = t.Shards
	}
	rep.Event("result", result)
	// The end of the output says how the whole run went, however far up
	// each part's own result scrolled: every part, failed first, then the
	// verdict as the last line.
	for _, o := range recapOrder(outs) {
		rep.PartDone(o.recapLine(), o.status == partPassed)
	}
	rep.Done(taskVerdict(name, outs, exit, interrupted, took), exit == 0)
	return exit
}

// saveFirstLastRun makes --same-slot after a parallel or sharded task
// predictable: it returns to the first part, in `parallel` order (shard 1
// first), that recorded where it ran
// (as a single run does once synced, with its container image ready).
func saveFirstLastRun(ws *client.Workspace, records []*client.LastRun) {
	for _, r := range records {
		if r != nil {
			client.SaveLastRun(ws, *r)
			return
		}
	}
}

func buildTaskLine(l *loaded, run string, args []string) string {
	return buildCommand(l.ws, []string{taskCommand(run, args)})
}

// taskReporter tags events with the task name (and a shard's number) and
// suppresses the spinner (several tasks share one terminal). It takes the
// fan-out's output lock, so no event or warning lands inside a part's block.
type taskReporter struct {
	ui.Reporter
	task          string
	shard, shards int
	mu            *sync.Mutex
}

func (t *taskReporter) Event(name string, fields map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.event(name, fields)
}

func (t *taskReporter) Warn(code, format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Reporter.Warn(code, format, args...)
}

// event is Event for a caller already holding the lock.
func (t *taskReporter) event(name string, fields map[string]any) {
	m := map[string]any{"task": t.task}
	if t.shards > 0 {
		m["shard"], m["shards"] = t.shard, t.shards
	}
	for k, v := range fields {
		m[k] = v
	}
	t.Reporter.Event(name, m)
}
func (t *taskReporter) Status(string, ...any) {}
func (t *taskReporter) Clear()                {}
func (t *taskReporter) Done(string, bool)     {}

// depsDetail prints, under a failed dependency install's error, the end of
// the install's output and where its full log is. prefix starts every line
// (a parallel task's "[name] ").
func depsDetail(w io.Writer, prefix string, err error) {
	var de *client.DepsError
	if !errors.As(err, &de) {
		return
	}
	for _, l := range de.Detail() {
		fmt.Fprintf(w, "%s  %s\n", prefix, l)
	}
}

// waitLaunches lets a VM launch this process started record its instance
// ID before exiting (or the fleet could lose track of it for a while).
func waitLaunches() {
	client.WaitLaunchesSaying(90*time.Second, func() {
		fmt.Fprintln(os.Stderr, "sx: finishing a VM launch this run started (a few seconds; the command's result is above)")
	})
}
