package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/spotexec/spotexec/internal/client"
	"github.com/spotexec/spotexec/internal/config"
	"github.com/spotexec/spotexec/internal/transport"
	"github.com/spotexec/spotexec/internal/ui"
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
	forceLarge    bool
	fallbackLocal bool
	noBoot        bool
	dir           string
	noRetry       bool
	priority      int
}

func (f *runFlags) register(fs *pflag.FlagSet) {
	fs.DurationVar(&f.timeout, "timeout", 0, "wall-clock limit for the command (default 5m or .spotexec.toml timeout)")
	fs.DurationVar(&f.queueTimeout, "queue-timeout", 0, "how long to wait for a free slot")
	fs.StringVar(&f.sync, "sync", "", "sync mode: auto (bidirectional), in (no write-back), none")
	fs.StringVar(&f.writeback, "writeback", "", "write-back policy: always, on-success, never")
	fs.StringSliceVar(&f.pool, "pool", nil, "runner names to use (comma-separated)")
	fs.StringVarP(&f.runner, "runner", "r", "", "use only this runner")
	fs.BoolVar(&f.exclusive, "exclusive", false, "take every slot on the runner")
	fs.StringArrayVar(&f.pull, "pull", nil, "also pull these gitignored paths/globs back after the run (repeatable)")
	fs.BoolVar(&f.json, "json", false, "NDJSON lifecycle events on stderr (default when stderr is not a TTY)")
	fs.BoolVar(&f.pretty, "pretty", false, "human spinner output even when not a TTY")
	fs.BoolVarP(&f.quiet, "quiet", "q", false, "no lifecycle output")
	fs.StringVar(&f.eventsFile, "events-file", "", "also append NDJSON events to this file")
	fs.BoolVar(&f.noDeps, "no-deps", false, "skip dependency hydration")
	fs.BoolVar(&f.forceLarge, "force-large-sync", false, "allow syncing more than 100k files / 5 GiB")
	fs.BoolVar(&f.fallbackLocal, "fallback-local", false, "run locally if no runner is available")
	fs.BoolVar(&f.noBoot, "no-boot", false, "never provision ephemeral runners for this command")
	fs.StringVarP(&f.dir, "dir", "C", "", "run as if started in this directory")
	fs.BoolVar(&f.noRetry, "no-retry", false, "do not replay the run if its runner is lost (spot reclaim)")
	fs.IntVar(&f.priority, "priority", 0, "queue priority when every slot is busy (higher runs first)")
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
	}
	return ui.New(mode, os.Stderr, f.eventsFile)
}

// env overrides (SPOTEXEC_*), lower precedence than flags.
func (f *runFlags) applyEnv() {
	if f.timeout == 0 {
		if d, err := time.ParseDuration(os.Getenv("SPOTEXEC_TIMEOUT")); err == nil {
			f.timeout = d
		}
	}
	if f.sync == "" {
		f.sync = os.Getenv("SPOTEXEC_SYNC")
	}
	if f.runner == "" {
		f.runner = os.Getenv("SPOTEXEC_RUNNER")
	}
	if os.Getenv("SPOTEXEC_JSON") == "1" {
		f.json = true
	}
	if f.priority == 0 {
		if n, err := strconv.Atoi(os.Getenv("SPOTEXEC_PRIORITY")); err == nil {
			f.priority = n
		}
	}
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
	for i, a := range args {
		q[i] = transport.ShellQuote(rewriteRoot(a, ws.Root, toRoot, false))
	}
	return strings.Join(q, " ")
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
}

func (f *runFlags) options(l *loaded, s runSpec, rep ui.Reporter, intr <-chan struct{}, stdout, stderr io.Writer) client.Options {
	p := l.cfg.Project
	o := client.Options{
		Command: s.command, Display: s.display, Timeout: p.Timeout.Duration, Grace: p.Grace.Duration,
		QueueTimeout: p.QueueTimeout.Duration, Sync: p.Sync.Mode, Writeback: p.Sync.Writeback,
		Pool: f.pool, Exclusive: f.exclusive || s.exclusive, Pull: f.pull, NoDeps: f.noDeps,
		ForceLargeSync: f.forceLarge, Stdout: stdout, Stderr: stderr, Rep: rep, Interrupt: intr,
		Label: s.label, RetryPreempt: !f.noRetry, NoBoot: f.noBoot,
		PreferLast: s.preferLast, SyncOnly: s.syncOnly, Priority: s.priority,
	}
	if f.priority != 0 {
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
	if f.sync != "" {
		o.Sync = f.sync
	}
	if f.writeback != "" {
		o.Writeback = f.writeback
	}
	if f.runner != "" {
		o.Pool = []string{f.runner}
	}
	return o
}

// execute runs one spec end to end and returns the process exit code.
func (f *runFlags) execute(l *loaded, s runSpec) int {
	// A scale-out started by this process must record its instance ID before
	// we exit, or the pool could lose track of it for a while.
	defer client.WaitLaunches(90 * time.Second)
	f.applyEnv()
	if f.sync != "" && f.sync != "auto" && f.sync != "in" && f.sync != "none" {
		fmt.Fprintf(os.Stderr, "spotexec: --sync must be auto, in or none\n")
		return 2
	}
	rep, err := f.reporter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return 2
	}
	ctx, intr, stop := signals()
	defer stop()
	o := f.options(l, s, rep, intr, os.Stdout, os.Stderr)
	res, err := client.Run(ctx, l.cfg, l.ws, o)
	if err != nil {
		var ie *client.InfraError
		if ctx.Err() != nil {
			rep.Clear()
			return 130
		}
		// Only when no runner was ever reached (so the command never ran
		// remotely), and only for real commands (not exec, sync or pull).
		if errors.As(err, &ie) && (f.fallbackLocal || l.cfg.Global.LocalFallback) && !res.Reached && !o.PreferLast && !o.SyncOnly && s.command != "" {
			rep.Warn("LOCAL_FALLBACK", "no runner available (%v); running locally", err)
			return runLocal(l, s.command)
		}
		rep.Clear()
		rep.Event("error", map[string]any{"code": "SPOTEXEC_ERROR", "message": err.Error()})
		if !rep.IsJSON() {
			fmt.Fprintf(os.Stderr, "spotexec: %v\n", err)
		}
		return client.ExitInfra
	}
	summary := fmt.Sprintf("%s on %s#%d", ui.Dur(res.Duration), res.Runner, res.Slot)
	if res.Synced > 0 {
		summary += fmt.Sprintf(" · synced %d files (%s)", res.Synced, ui.Bytes(res.SyncBytes))
	}
	if n := len(res.Mutated) + len(res.Deleted); n > 0 {
		summary += fmt.Sprintf(" · pulled %d modified file%s", n, plural(n))
	}
	if len(res.Conflicts) > 0 {
		summary += fmt.Sprintf(" · %d conflict%s", len(res.Conflicts), plural(len(res.Conflicts)))
	}
	if res.ExitCode != 0 {
		summary += fmt.Sprintf(" · exit %d", res.ExitCode)
	}
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

func runLocal(l *loaded, command string) int {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = filepath.Join(l.ws.Root, filepath.FromSlash(l.ws.Cwd))
	// The command was written for the runner: worktree paths in it were
	// rewritten to ${SPOTEXEC_WORKSPACE}, which here is the worktree itself
	// (unset, "rm -rf ${SPOTEXEC_WORKSPACE}/tmp" would mean /tmp).
	// Defaults, then the project's [env] (which may override them, as on a
	// runner), then what must hold here.
	cmd.Env = append(os.Environ(), "CI=true", "SPOTEXEC_CPUS="+strconv.Itoa(runtime.NumCPU()), "SPOTEXEC_SLOTS=1", "SPOTEXEC_SLOT=0", "SPOTEXEC_PORT_BASE=20000")
	for k, v := range l.cfg.Project.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "SPOTEXEC=1", "SPOTEXEC_WORKSPACE="+l.ws.Root, "SPOTEXEC_LOCAL=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ee.ExitCode()
		}
		return 127
	}
	return 0
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

// runTask executes a named task (possibly a parallel fan-out).
func (f *runFlags) runTask(l *loaded, name string, args []string) int {
	t := l.cfg.Project.Tasks[name]
	defer client.WaitLaunches(90 * time.Second)
	if len(t.Parallel) == 0 {
		cmd := buildTaskLine(l, t.Run, args)
		return f.execute(l, runSpec{label: name, command: cmd, display: name + ": " + cmd, timeout: t.Timeout.Duration, sync: t.Sync, exclusive: t.Exclusive, pool: t.Pool, priority: t.Priority})
	}
	f.applyEnv()
	rep, err := f.reporter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return 2
	}
	ctx, intr, stop := signals()
	defer stop()
	var mu sync.Mutex
	codes := make([]int, len(t.Parallel))
	// Each subtask gets its own context, so fail_fast can stop siblings that
	// are still queued, syncing or installing (not only running ones).
	subCtx := make([]context.Context, len(t.Parallel))
	subCancel := make([]context.CancelFunc, len(t.Parallel))
	for i := range t.Parallel {
		subCtx[i], subCancel[i] = context.WithCancel(ctx)
		defer subCancel[i]()
	}
	stopped := make([]bool, len(t.Parallel)) // cancelled by fail_fast
	done := make([]bool, len(t.Parallel))
	intrs := make([]chan struct{}, len(t.Parallel))
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
	for i, sub := range t.Parallel {
		st := l.cfg.Project.Tasks[sub]
		wg.Add(1)
		go func(i int, sub string, st *config.Task) {
			defer wg.Done()
			sr := &taskReporter{Reporter: rep, task: sub}
			prefix := fmt.Sprintf("[%s] ", sub)
			cmd := buildTaskLine(l, st.Run, nil)
			spec := runSpec{label: sub, command: cmd, display: sub + ": " + cmd, timeout: st.Timeout.Duration, sync: st.Sync, exclusive: st.Exclusive, pool: st.Pool, priority: st.Priority}
			o := f.options(l, spec, sr, intrs[i], ui.NewPrefixWriter(os.Stdout, prefix, &mu), ui.NewPrefixWriter(os.Stderr, prefix, &mu))
			res, err := client.Run(subCtx[i], l.cfg, l.ws, o)
			code := res.ExitCode
			if err != nil {
				code = client.ExitInfra
			}
			mu.Lock()
			codes[i], done[i] = code, true
			if stopped[i] {
				// Cancelled by fail_fast: not a failure of its own.
				mu.Unlock()
				return
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%sspotexec: %v\n", prefix, err)
			}
			if !rep.IsJSON() {
				status := "\x1b[32m✓\x1b[0m"
				if code != 0 {
					status = fmt.Sprintf("\x1b[31m✗ exit %d\x1b[0m", code)
				}
				fmt.Fprintf(os.Stderr, "%s %s \x1b[2m%s on %s#%d\x1b[0m\n", status, sub, ui.Dur(res.Duration), res.Runner, res.Slot)
			}
			mu.Unlock()
			if code != 0 && t.FailFast {
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
		}(i, sub, st)
	}
	wg.Wait()
	// Interrupted: 130. A spotexec failure (125) outranks a command's own
	// failure, so agents can tell "tests failed" from "couldn't run tests".
	exit := 0
	for i, c := range codes {
		if stopped[i] {
			continue // the failure that stopped it is what counts
		}
		if c != 0 && (exit == 0 || c == client.ExitInfra) {
			exit = c
		}
	}
	if ctx.Err() != nil {
		exit = 130
	}
	rep.Event("result", map[string]any{"task": name, "exit_code": exit, "duration_ms": time.Since(start).Milliseconds(), "parallel": t.Parallel})
	return exit
}

func buildTaskLine(l *loaded, run string, args []string) string {
	return buildCommand(l.ws, []string{taskCommand(run, args)})
}

// taskReporter tags events with the task name and suppresses the spinner
// (several tasks share one terminal).
type taskReporter struct {
	ui.Reporter
	task string
}

func (t *taskReporter) Event(name string, fields map[string]any) {
	m := map[string]any{"task": t.task}
	for k, v := range fields {
		m[k] = v
	}
	t.Reporter.Event(name, m)
}
func (t *taskReporter) Status(string, ...any) {}
func (t *taskReporter) Clear()                {}
func (t *taskReporter) Done(string, bool)     {}
