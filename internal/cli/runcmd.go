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
	fs.BoolVar(&f.noRetry, "no-retry", false, "do not retry once after a spot interruption")
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
	return &loaded{cfg: &config.Config{Global: g, Project: p}, ws: ws}, nil
}

// buildCommand turns argv into a remote shell line. A single argument is
// taken as a shell snippet (so `sx 'pnpm build && pnpm test'` works); several
// are quoted individually. Absolute local paths inside the worktree become
// paths relative to the (mirrored) working directory.
func buildCommand(ws *client.Workspace, args []string) string {
	toRoot := "."
	if ws.Cwd != "." {
		toRoot = strings.TrimSuffix(strings.Repeat("../", strings.Count(ws.Cwd, "/")+1), "/")
	}
	rewrite := func(s string) string {
		s = strings.ReplaceAll(s, ws.Root+"/", toRoot+"/")
		if s == ws.Root {
			return toRoot
		}
		return s
	}
	if len(args) == 1 {
		return rewrite(args[0])
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = transport.ShellQuote(rewrite(a))
	}
	return strings.Join(q, " ")
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
		PreferLast: s.preferLast, SyncOnly: s.syncOnly,
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
		if errors.As(err, &ie) && (f.fallbackLocal || l.cfg.Global.LocalFallback) && res.Runner == "" {
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
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
		return strings.TrimSpace(strings.ReplaceAll(run, "{args}", ""))
	}
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
		return f.execute(l, runSpec{label: name, command: cmd, display: name + ": " + cmd, timeout: t.Timeout.Duration, sync: t.Sync, exclusive: t.Exclusive, pool: t.Pool})
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
			spec := runSpec{label: sub, command: cmd, display: sub + ": " + cmd, timeout: st.Timeout.Duration, sync: st.Sync, exclusive: st.Exclusive, pool: st.Pool}
			o := f.options(l, spec, sr, intrs[i], ui.NewPrefixWriter(os.Stdout, prefix, &mu), ui.NewPrefixWriter(os.Stderr, prefix, &mu))
			res, err := client.Run(ctx, l.cfg, l.ws, o)
			code := res.ExitCode
			if err != nil {
				code = client.ExitInfra
				mu.Lock()
				fmt.Fprintf(os.Stderr, "%sspotexec: %v\n", prefix, err)
				mu.Unlock()
			}
			codes[i] = code
			mu.Lock()
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
						if j != i {
							c <- struct{}{}
						}
					}
				})
			}
		}(i, sub, st)
	}
	wg.Wait()
	exit := 0
	for _, c := range codes {
		if c != 0 && exit == 0 {
			exit = c
		}
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
