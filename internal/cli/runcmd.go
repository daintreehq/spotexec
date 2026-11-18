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
	"github.com/daintreehq/spotexec/internal/protocol"
	"github.com/daintreehq/spotexec/internal/transport"
	"github.com/daintreehq/spotexec/internal/ui"
)

// runFlags are shared by the shorthand form, run, exec, and tasks.
type runFlags struct {
	timeout       time.Duration
	timeoutFrom   string // "--timeout" or "SPOTEXEC_TIMEOUT" when either set it
	queueTimeout  time.Duration
	sync          string
	writeback     string
	writebackOver string
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
	failConflict  bool
	envErr        error          // a SPOTEXEC_* value applyEnv couldn't parse
	fs            *pflag.FlagSet // to tell explicit flags from defaults
}

func (f *runFlags) register(fs *pflag.FlagSet) {
	f.fs = fs
	fs.DurationVar(&f.timeout, "timeout", 0, "wall-clock limit for the command (default 5m or .spotexec.toml timeout)")
	fs.DurationVar(&f.queueTimeout, "queue-timeout", 0, "how long to wait for a free slot (default 10m or .spotexec.toml queue_timeout)")
	fs.StringVar(&f.sync, "sync", "", "sync mode: auto (bidirectional), in (no write-back), none")
	fs.StringVar(&f.writeback, "writeback", "", "write-back policy: always, on-success, never")
	fs.StringVar(&f.writebackOver, "writeback-over", "", "over the write-back limit: warn (bring everything back) or hold (leave the new files in the slot)")
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
	fs.StringVar(&f.rtkFilter, "rtk-filter", "", "compact stdout with RTK once the command ends (rtk pipe -f `NAME`): cargo-test for cargo test, go-test for go test -json; the full stdout is kept when it fails")
	fs.BoolVar(&f.failConflict, "fail-on-conflict", false, fmt.Sprintf("exit %d when the command succeeded but write-back kept a local edit over one of its changes (SYNC_CONFLICT)", client.ExitWritebackConflict))
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
// A value that doesn't parse is kept for validate to report, never dropped
// for the default.
func (f *runFlags) applyEnv() {
	f.envErr = nil
	if f.changed("timeout") {
		f.timeoutFrom = "--timeout"
	} else if v := os.Getenv("SPOTEXEC_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			f.timeout, f.timeoutFrom = d, "SPOTEXEC_TIMEOUT"
		} else {
			f.envErr = fmt.Errorf("SPOTEXEC_TIMEOUT must be a duration such as 15m (got %q)", v)
		}
	}
	if !f.changed("sync") {
		if v := os.Getenv("SPOTEXEC_SYNC"); v != "" {
			f.sync = v
		}
	}
	if !f.changed("writeback-over") {
		if v := os.Getenv("SPOTEXEC_WRITEBACK_OVER"); v != "" {
			f.writebackOver = v
		}
	}
	// A runner named in the environment narrows the default pool, never one
	// given with --pool.
	if !f.changed("runner") && !f.changed("pool") {
		if v := os.Getenv("SPOTEXEC_RUNNER"); v != "" {
			f.runner = v
		}
	}
	if !f.changed("json") && !f.changed("pretty") && !f.changed("quiet") {
		f.envSwitch("SPOTEXEC_JSON", &f.json)
	}
	if !f.changed("same-slot") {
		f.envSwitch("SPOTEXEC_SAME_SLOT", &f.sameSlot)
	}
	if !f.changed("fail-on-conflict") {
		f.envSwitch("SPOTEXEC_FAIL_ON_CONFLICT", &f.failConflict)
	}
	if f.changed("priority") {
		f.prioritySet = true
	} else if v := os.Getenv("SPOTEXEC_PRIORITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.priority, f.prioritySet = n, true
		} else if f.envErr == nil {
			f.envErr = fmt.Errorf("SPOTEXEC_PRIORITY must be a whole number (got %q)", v)
		}
	}
}

// envSwitch turns on a flag not given on the command line when its boolean
// variable says so; a value that isn't one is kept for validate to report.
func (f *runFlags) envSwitch(name string, flag *bool) {
	on, err := envBool(name)
	if err != nil {
		if f.envErr == nil {
			f.envErr = err
		}
		return
	}
	if on {
		*flag = true
	}
}

// envBool reads a boolean SPOTEXEC_* variable: 1, true or yes, or 0, false
// or no, in any case; unset or empty is false. Anything else is an error
// naming the variable, never silently false.
func envBool(name string) (bool, error) {
	switch v := os.Getenv(name); strings.ToLower(v) {
	case "", "0", "false", "no":
		return false, nil
	case "1", "true", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be 1, true or yes, or 0, false or no (got %q)", name, v)
	}
}

// validate rejects run settings no run could honour, before any runner is
// acquired: an unknown --writeback must never fall through to writing back.
func (f *runFlags) validate() error {
	if f.envErr != nil {
		return f.envErr
	}
	// An explicitly empty value would silently mean "the default".
	for name, empty := range map[string]bool{"sync": f.sync == "", "writeback": f.writeback == "", "writeback-over": f.writebackOver == "", "runner": f.runner == "", "pool": len(f.pool) == 0} {
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
	if f.writebackOver != "" && f.writebackOver != "warn" && f.writebackOver != "hold" {
		return fmt.Errorf("--writeback-over must be warn or hold (got %q)", f.writebackOver)
	}
	// Zero would otherwise mean "the default", and a negative limit nothing.
	if f.timeoutFrom != "" && f.timeout <= 0 {
		return fmt.Errorf("%s must be positive (got %s)", f.timeoutFrom, f.timeout)
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
// mapped to the remote workspace's absolute path, "$SPOTEXEC_WORKSPACE", so
// they still name the same file after the command changes directory
// (`make -C sub OUT=/repo/build`) and stay absolute for tools that check.
func buildCommand(ws *client.Workspace, args []string) string {
	if len(args) == 1 {
		return rewriteRoot(args[0], ws.Root)
	}
	q := make([]string, len(args))
	assigning := true
	for i, a := range args {
		// Leading NAME=value words stay environment assignments: quoting
		// them whole would make bash run "NAME=value" as a command. The
		// value is quoted on its own.
		if name, value, ok := envAssignment(a); ok && assigning {
			q[i] = name + "=" + quoteArg(value, ws.Root)
			continue
		}
		q[i] = quoteArg(a, ws.Root)
		// The command word must not read as an assignment once spliced
		// (A+=/repo/x -> A+="$W"/x would be one): start it quoted.
		if assigning && looksAssigning(q[i]) {
			q[i] = "''" + q[i]
		}
		assigning = false
	}
	return strings.Join(q, " ")
}

// quoteArg shell-quotes one word, with each worktree path in it (matched
// before quoting) turned into the remote workspace's, double-quoted so a
// workspace path with spaces stays one word.
func quoteArg(v, root string) string {
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

// looksAssigning reports whether bash could read the shell word w as an
// assignment: NAME=, NAME+= or NAME[...] at its start, unquoted.
func looksAssigning(w string) bool {
	n := 0
	for n < len(w) && (w[n] == '_' || w[n] >= 'A' && w[n] <= 'Z' || w[n] >= 'a' && w[n] <= 'z' || n > 0 && w[n] >= '0' && w[n] <= '9') {
		n++
	}
	return n > 0 && (strings.HasPrefix(w[n:], "=") || strings.HasPrefix(w[n:], "+=") || strings.HasPrefix(w[n:], "["))
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

// rewriteRoot replaces root in a shell snippet with the remote workspace
// where it appears as a whole path (followed by '/', the end, or a shell
// delimiter, and not as part of a longer name such as /repo-other). The
// variable is spliced so it expands to one word whatever the quoting
// around it, and ${...:?} aborts rather than expanding to "" if unset.
func rewriteRoot(s, root string) string {
	var b strings.Builder
	// The quoting context: 0 at the top, '"' in double quotes, '(' in
	// $(...) or a subshell and '`' in `...`, where quoting starts afresh.
	// Single quotes nest in neither. saved holds each $(...)'s outer braces.
	ctx, saved := []byte{0}, []int{}
	inSingle, heredoc, braces := false, false, 0 // braces: open ${...}
	atBoundary := true                           // where a path may start: not mid-word, as after My\ x
	for i := 0; i < len(s); {
		c := s[i]
		inDouble := ctx[len(ctx)-1] == '"'
		if end := matchRoot(s, i, root, !inSingle && !inDouble); end >= 0 && atBoundary && boundaryAfter(s, end) {
			switch {
			case inSingle:
				// Close the quotes, splice the variable in double quotes,
				// reopen ('a/x' -> ''"$W"'/x').
				b.WriteString(`'"${SPOTEXEC_WORKSPACE:?}"'`)
			case inDouble, heredoc:
				// Quotes in a here-document's body would be literal; the
				// tracker can't tell its body from the rest, so after <<
				// unquoted paths keep the bare form.
				b.WriteString("${SPOTEXEC_WORKSPACE:?}")
			default:
				b.WriteString(`"${SPOTEXEC_WORKSPACE:?}"`)
			}
			i = end
			atBoundary = false
			continue
		}
		switch {
		case inSingle:
			inSingle = c != '\''
		case c == '\\' && i+1 < len(s):
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i += 2
			// A line continuation vanishes, leaving the boundary as it was.
			if s[i-1] != '\n' {
				atBoundary = false
			}
			continue
		case c == '"' && inDouble:
			ctx = ctx[:len(ctx)-1]
		case c == '"':
			ctx = append(ctx, '"')
		case c == '`' && ctx[len(ctx)-1] == '`':
			ctx = ctx[:len(ctx)-1]
		case c == '`':
			ctx = append(ctx, '`')
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			ctx, saved, braces = append(ctx, '$'), append(saved, braces), 0
			b.WriteString("$(")
			i += 2
			atBoundary = true
			continue
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			braces++
		case c == '}' && braces > 0:
			braces--
		case inDouble:
		case c == '\'':
			inSingle = true
		case braces > 0: // ${x//(/} is a pattern, not a subshell
		case c == '(':
			ctx = append(ctx, '(')
		case c == ')' && ctx[len(ctx)-1] == '$':
			ctx, braces, saved = ctx[:len(ctx)-1], saved[len(saved)-1], saved[:len(saved)-1]
		case c == ')' && ctx[len(ctx)-1] == '(':
			ctx = ctx[:len(ctx)-1]
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<") && (i == 0 || s[i-1] != '<'):
			heredoc = true
		}
		b.WriteByte(c)
		i++
		atBoundary = strings.IndexByte(pathStarts, c) >= 0
	}
	return b.String()
}

// matchRoot returns where root ends if it is written at s[i:], or -1.
// Outside quotes (unquoted) each of its bytes may be backslash-escaped, as
// in /Users/me/My\ Projects/repo, and one the shell would read as syntax
// (a quote, '$', whitespace, an operator) must be.
func matchRoot(s string, i int, root string, unquoted bool) int {
	if !unquoted {
		if strings.HasPrefix(s[i:], root) {
			return i + len(root)
		}
		return -1
	}
	j := i
	for k := 0; k < len(root); k++ {
		switch {
		case j+1 < len(s) && s[j] == '\\' && s[j+1] == root[k] && root[k] != '\n': // \<newline> is a continuation
			j += 2
		case j < len(s) && s[j] == root[k] && !strings.ContainsRune(shellSyntax, rune(root[k])):
			j++
		default:
			return -1
		}
	}
	return j
}

// shellSyntax is the bytes that mean something to the shell outside quotes:
// quoting, expansion, word splitting, operators and globs.
const shellSyntax = " \t\n\\'\"$`;&|<>()*?["

// pathStarts is the bytes a whole path may follow.
const pathStarts = " \t\n'\"=:(;&|"

func boundaryBefore(s string, i int) bool {
	return i == 0 || strings.IndexByte(pathStarts, s[i-1]) >= 0
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
	taskLike   string            // the first word, when it may be a mistyped task name
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
		Env: s.env, FailOnConflict: f.failConflict,
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
	o.WritebackLimits = client.WritebackLimits{MaxFiles: p.Sync.WritebackMaxFiles, MaxBytes: int64(p.Sync.WritebackMaxBytes), Hold: p.Sync.WritebackOver == "hold"}
	if f.writebackOver != "" {
		o.WritebackLimits.Hold = f.writebackOver == "hold"
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
func (f *runFlags) execute(l *loaded, s runSpec) (code int) {
	var rec *recorder
	// A scale-out started by this process must record its instance ID before
	// we exit, or the pool could lose track of it for a while. The run's
	// record is saved before that wait, and again after it with how the
	// launches went.
	defer func() {
		if rec != nil {
			rec.finish(code)
		}
		waitLaunches()
		if rec != nil {
			rec.settle()
		}
	}()
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
	rec = newRecorder(l.ws, "")
	part, rep := rec.part(rep, "", s.display)
	var res client.Result
	local, localStart := false, time.Time{}
	interrupted := false // by the user: not a command that exited 130
	defer func() {
		if local {
			rec.localPhase(part, time.Since(localStart))
		}
		status := partFailed
		switch {
		case interrupted || res.Canceled:
			status = partInterrupted
		case code == 0:
			status = partPassed
		}
		var failed error
		if !local && status != partInterrupted {
			failed = err
		}
		rec.end(part, res, failed, code, status, local)
	}()
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
	var stderr io.Writer = os.Stderr
	var watch *notFoundWatch
	clientRep := rep
	if s.taskLike != "" {
		watch = newNotFoundWatch(os.Stderr, s.taskLike)
		stderr, clientRep = watch, watchReporter{rep, watch}
	}
	stdin := newStdinNotice(s.command)
	stdin.check(rep)
	o := f.options(l, s, stdin.reporter(clientRep), intr, stdout, stderr)
	res, err = client.Run(ctx, l.cfg, l.ws, o)
	if ctx.Err() == nil {
		// A run that failed reports no result to look before.
		stdin.check(rep)
	}
	if err != nil {
		if ctx.Err() != nil {
			interrupted = true
			rep.Clear()
			return finish(client.ExitInterrupted)
		}
		if f.fallsBack(l, o, res, err) {
			rep.Warn("LOCAL_FALLBACK", "no runner available (%v); running locally", err)
			// Unwatched: a terminal stays one for the local command.
			o.Stderr, o.Rep = os.Stderr, rep
			local, localStart = true, time.Now()
			code := runLocal(ctx, intr, l, o, rep, 0, 1)
			interrupted = ctx.Err() != nil
			if ctx.Err() == nil {
				stdin.check(rep)
			}
			code = finish(code)
			warnUnknownTask(rep, l.cfg.Project, s.taskLike, code, s.taskLike != "" && code == 127 && !localCommand(l, o, s.taskLike), true)
			return code
		}
		code := failExit(err)
		finish(code)
		rep.Clear()
		rep.Event("error", map[string]any{"code": errorCode(err), "message": err.Error()})
		if !rep.IsJSON() {
			fmt.Fprintf(os.Stderr, "spotexec: %v\n", err)
			errDetail(os.Stderr, "", err)
		}
		return code
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
	summary += writebackNote(len(res.Mutated), len(res.Deleted))
	summary += conflictNote(len(res.Conflicts))
	if res.HeldFiles > 0 {
		summary += fmt.Sprintf(" · held %d new file%s (%s) in the slot", res.HeldFiles, plural(res.HeldFiles), ui.Bytes(res.HeldBytes))
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
	if !res.Canceled {
		warnUnknownTask(rep, l.cfg.Project, s.taskLike, res.ExitCode, watch != nil && watch.Seen(), false)
	}
	rep.Done(summary, res.ExitCode == 0)
	return res.ExitCode
}

// executeLast runs in the last-used runner and slot (exec).
func (f *runFlags) executeLast(l *loaded, s runSpec) int {
	s.preferLast = true
	return f.execute(l, s)
}

// writebackNote is the summary's account of the write-back, as sync_out
// has it: the files written here (new or changed, mutated_files) and the
// files deleted here (deleted_files).
func writebackNote(written, deleted int) string {
	s := ""
	if written > 0 {
		s += fmt.Sprintf(" · pulled %d file%s", written, plural(written))
	}
	if deleted > 0 {
		s += fmt.Sprintf(" · deleted %d file%s", deleted, plural(deleted))
	}
	return s
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
		// Before any trailing comment, which would swallow them.
		end := endOfCode(run)
		body = run[:end] + ` "$@"` + run[end:]
	}
	return "set -- " + strings.Join(q, " ") + "\n" + body
}

// endOfCode returns where run's last command ends: after its last byte that
// is not whitespace, a closing ';' or '&', or part of a comment (a '#' that
// begins a word outside quotes, to the end of its line). Quoted strings,
// $(...), ${...} and `...` are skipped whole. A run that is only comments, or has a
// here-document (whose lines are data), ends at its end: arguments are
// appended as they always were.
func endOfCode(run string) int {
	end, beforeTerm := 0, -1 // beforeTerm: end before a closing ; or &
	wordStart, prevOp, prevDollar := true, false, false
	for i := 0; i < len(run); i++ {
		c := run[i]
		op, dollar := false, false
		switch {
		case c == '\\' && i+1 < len(run):
			i++
			if run[i] == '\n' {
				continue // a line continuation vanishes
			}
			wordStart = false
		case c == '#' && wordStart:
			nl := strings.IndexByte(run[i:], '\n')
			if nl < 0 {
				i = len(run)
			} else {
				i += nl // the loop steps past the newline
			}
			prevOp, prevDollar = false, false
			continue
		case c == ' ' || c == '\t' || c == '\n':
			wordStart, prevOp, prevDollar = true, false, false
			continue
		case c == '\'':
			i, wordStart = closeSingle(run, i, prevDollar), false
		case c == '"':
			i, wordStart = closeDouble(run, i), false
		case c == '`':
			i, wordStart = closeBacktick(run, i), false
		case c == '$' && i+1 < len(run) && run[i+1] == '(':
			i, wordStart = closeParen(run, i+1), false
		case c == '$' && i+1 < len(run) && run[i+1] == '{':
			i, wordStart = closeBrace(run, i+1), false
		case c == '<' && strings.HasPrefix(run[i:], "<<<"):
			i, wordStart, op = i+2, true, true
		case c == '<' && strings.HasPrefix(run[i:], "<<"):
			return len(run)
		case (c == ';' || c == '&') && !prevOp && strings.IndexByte(";&|>", nextByte(run, i)) < 0:
			// A list terminator: arguments go before it ("make &" ->
			// make "$@" &). Part of an operator (&&, 2>&1, ;;) it is code.
			beforeTerm, end = end, i+1
			wordStart, prevOp, prevDollar = true, true, false
			continue
		default:
			wordStart = strings.IndexByte(";&|()", c) >= 0
			op = strings.IndexByte(";&|<>", c) >= 0
			dollar = c == '$'
		}
		prevOp, prevDollar = op, dollar
		end, beforeTerm = i+1, -1
	}
	switch {
	case end == 0:
		return len(run)
	case beforeTerm > 0:
		return beforeTerm
	}
	return end
}

// closeSingle returns the index of the quote closing the '...' that opens at
// run[i], or the last index; in $'...' a backslash escapes.
func closeSingle(run string, i int, ansi bool) int {
	for j := i + 1; j < len(run); j++ {
		switch {
		case ansi && run[j] == '\\':
			j++
		case run[j] == '\'':
			return j
		}
	}
	return len(run) - 1
}

// closeDouble returns the index of the quote closing the "..." that opens at
// run[i], or the last index.
func closeDouble(run string, i int) int {
	for j := i + 1; j < len(run); j++ {
		switch {
		case run[j] == '\\':
			j++
		case run[j] == '"':
			return j
		case run[j] == '`':
			j = closeBacktick(run, j)
		case run[j] == '$' && j+1 < len(run) && run[j+1] == '(':
			j = closeParen(run, j+1)
		case run[j] == '$' && j+1 < len(run) && run[j+1] == '{':
			j = closeBrace(run, j+1)
		}
	}
	return len(run) - 1
}

// closeParen returns the index of the ')' matching the '(' at run[i], or the
// last index. Inside case ... esac a pattern's ')' doesn't close it.
func closeParen(run string, i int) int {
	depth, cases := 0, 0
	for j := i; j < len(run); j++ {
		c := run[j]
		if j > i && strings.IndexByte(" \t\n;&|(", run[j-1]) >= 0 && commandPos(run, i, j) {
			switch {
			case keywordAt(run, j, "case"):
				cases, j = cases+1, j+3
				continue
			case keywordAt(run, j, "esac") && cases > 0:
				cases, j = cases-1, j+3
				continue
			}
		}
		switch {
		case c == '\\':
			j++
		case c == '\'':
			j = closeSingle(run, j, run[j-1] == '$')
		case c == '"':
			j = closeDouble(run, j)
		case c == '`':
			j = closeBacktick(run, j)
		case c == '$' && j+1 < len(run) && run[j+1] == '{':
			j = closeBrace(run, j+1)
		case c == '$' && j+1 < len(run) && run[j+1] == '(':
			j = closeParen(run, j+1)
		case cases > 0:
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth <= 0 {
				return j
			}
		}
	}
	return len(run) - 1
}

// commandPos reports whether a word at run[j] (inside the parenthesis at
// run[i]) is where a command starts: first, or after ( ; & | or a newline.
func commandPos(run string, i, j int) bool {
	k := j - 1
	for k > i && (run[k] == ' ' || run[k] == '\t') {
		k--
	}
	return k == i || strings.IndexByte("(;&|\n", run[k]) >= 0
}

// keywordAt reports whether the word at run[j:] is kw.
func keywordAt(run string, j int, kw string) bool {
	k := j + len(kw)
	return strings.HasPrefix(run[j:], kw) && (k == len(run) || strings.IndexByte(" \t\n;&|)", run[k]) >= 0)
}

// closeBrace returns the index of the '}' closing the ${ whose '{' is at
// run[i], or the last index. A bare '{' inside (${x//{/}) is text.
func closeBrace(run string, i int) int {
	for j := i + 1; j < len(run); j++ {
		switch c := run[j]; {
		case c == '\\':
			j++
		case c == '\'':
			j = closeSingle(run, j, run[j-1] == '$')
		case c == '"':
			j = closeDouble(run, j)
		case c == '`':
			j = closeBacktick(run, j)
		case c == '$' && j+1 < len(run) && run[j+1] == '(':
			j = closeParen(run, j+1)
		case c == '$' && j+1 < len(run) && run[j+1] == '{':
			j = closeBrace(run, j+1)
		case c == '}':
			return j
		}
	}
	return len(run) - 1
}

// closeBacktick returns the index of the '`' closing the one at run[i], or
// the last index.
func closeBacktick(run string, i int) int {
	for j := i + 1; j < len(run); j++ {
		switch run[j] {
		case '\\':
			j++
		case '`':
			return j
		}
	}
	return len(run) - 1
}

func nextByte(s string, i int) byte {
	if i+1 >= len(s) {
		return 0
	}
	return s[i+1]
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
	parts := taskParts(l.cfg.Project, name, args)
	// A blank run, or run = "{args}" given none, would run nothing and pass;
	// checked for every part before any of them syncs.
	for _, pt := range parts {
		if strings.TrimSpace(pt.spec(l).command) == "" {
			fmt.Fprintf(os.Stderr, "spotexec: task %s: empty command (run = %q, with no arguments)\n", pt.name, pt.t.Run)
			return client.ExitInfra
		}
	}
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
	base, err := f.reporter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotexec:", err)
		return client.ExitInfra
	}
	// Each part's warnings and launches go in its part of the record; rep
	// records the task's own.
	rec := newRecorder(l.ws, name)
	rep := rec.reporter(base)
	exit := client.ExitInfra
	defer func() {
		rec.finish(exit)
		waitLaunches()
		rec.settle()
	}()
	group := f.grouped(t, os.Getenv("SPOTEXEC_AGENT") == "1" && !ui.IsTerminal(os.Stdout))
	ctx, intr, stop := signals()
	defer stop()
	// The parts that sync start from one scan of the worktree, taken by the
	// first of them to need it (#275).
	scan := client.NewSharedScan(ctx, l.ws, l.cfg.Project)
	defer scan.Close()
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
	// Every part has a command (whatever the task is called); before and
	// after they run, nothing else prints.
	stdin := &stdinNotice{in: os.Stdin}
	stdin.check(rep)
	start := time.Now()
	for i, part := range parts {
		wg.Add(1)
		go func(i int, part taskPart) {
			defer wg.Done()
			rp, prep := rec.part(base, part.label, part.spec(l).display)
			sr := &taskReporter{Reporter: prep, task: part.name, shard: part.shard, shards: part.shards, mu: &mu}
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
			o.Scan = scan
			// Parts race to sync: which one saves the last-run record is
			// decided below, once they are all done.
			o.DeferLastRun = true
			subStart := time.Now()
			res, err := client.Run(subCtx[i], l.cfg, l.ws, o)
			interrupted := ctx.Err() != nil
			out := partOutcome{part: part, code: res.ExitCode, runner: res.Runner, slot: res.Slot, conflicts: len(res.Conflicts)}
			if err != nil && subCtx[i].Err() == nil && f.fallsBack(l, o, res, err) {
				sr.Warn("LOCAL_FALLBACK", "no runner available (%v); running %s locally", err, part.label)
				localStart := time.Now()
				out.code, err = runLocal(subCtx[i], intrs[i], l, o, sr, i, len(parts)), nil
				out.runner, out.slot, out.local = "", 0, true
				interrupted = ctx.Err() != nil
				rec.localPhase(rp, time.Since(localStart))
			}
			if err != nil {
				out.code = failExit(err)
				if interrupted {
					// Ended by the interrupt: 130, as a single run (#289).
					out.code = client.ExitInterrupted
				}
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
			failed := err
			if out.status == partStopped || out.status == partInterrupted {
				failed = nil
			}
			rec.end(rp, res, failed, code, out.status, out.local)
			// report runs with mu held.
			report := func() {
				if wasStopped {
					return
				}
				// An interrupted part reports the interrupt, not the error
				// it ended with (such as the session the interrupt ended),
				// as a single run does.
				if err != nil && out.status != partInterrupted {
					sr.event("error", map[string]any{"code": errorCode(err), "message": err.Error()})
					if !rep.IsJSON() {
						fmt.Fprintf(os.Stderr, "%sspotexec: %v\n", prefix, err)
						errDetail(os.Stderr, prefix, err)
					}
				}
				sr.Reporter.PartDone(out.doneLine(res.DepsInstalled), code == 0)
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
	if ctx.Err() == nil {
		stdin.check(rep)
	}
	saveFirstLastRun(l.ws, records)
	interrupted := ctx.Err() != nil
	exit = taskExit(outs, interrupted)
	took := time.Since(start)
	result := map[string]any{"task": name, "exit_code": exit, "duration_ms": took.Milliseconds(), "parts": resultParts(outs), "conflict_count": totalConflicts(outs)}
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

// errorCode is the code of a --json error event: the reason the helper
// reported (BOOTSTRAP_FAILED, DEPS_FAILED, …), or the one the message starts
// with (QUEUE_TIMEOUT: …), else SPOTEXEC_ERROR.
func errorCode(err error) string {
	var re *transport.RemoteError
	if errors.As(err, &re) && re.Code != "" {
		return re.Code
	}
	// Only a known code counts: "BUILD_POOL: …" names a runner.
	if code, _, ok := strings.Cut(err.Error(), ": "); ok && errorCodes[code] {
		return code
	}
	return "SPOTEXEC_ERROR"
}

// errorCodes are the reasons a failed run's error can start with.
var errorCodes = map[string]bool{
	protocol.CodeDraining: true, protocol.CodeBooting: true, protocol.CodeBootFailed: true,
	protocol.CodeQueueTimeout: true, protocol.CodeBadRequest: true, protocol.CodeInternal: true,
	protocol.CodeSyncFailed: true, protocol.CodeDepsFailed: true, protocol.CodeImageFailed: true,
	protocol.CodeImageBuildFailed: true, protocol.CodeShutdown: true,
}

// failExit is the exit code of a run that failed with err: 123 when the
// project's Dockerfile failed to build, else 125 (spotexec failed).
func failExit(err error) int {
	var be *client.ImageBuildError
	if errors.As(err, &be) && be.Project() {
		return client.ExitImageBuild
	}
	return client.ExitInfra
}

// errDetail prints, under a failed dependency install's or image build's
// error (client.DepsError, client.ImageBuildError), the end of its output,
// where its full log is and what to do about it. prefix starts every line
// (a parallel task's "[name] ").
func errDetail(w io.Writer, prefix string, err error) {
	var de interface{ Detail() []string }
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
