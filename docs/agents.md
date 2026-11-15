# Using spotexec with coding agents

## What an agent sees

An agent reads everything on stderr, so when stderr isn't a terminal `sx` keeps quiet. The command's own stdout and stderr pass through untouched, and spotexec adds:
- a warning when something needs attention (`sx: warning: …`): a write-back conflict, a lost VM, a timeout;
- a progress line only when a phase takes more than 3 seconds, such as booting a VM, queueing or installing dependencies, and again every 30 seconds while it lasts;
- one summary line at the end, which says where the command ran and what came back:

```console
$ sx pnpm prettier --write src 2>&1 | cat
src/index.ts 41ms
sx: 2.3s on aws#2 · synced 3 files (12 KB) · pulled 1 modified file
```

Exit codes are the command's own; `125` means spotexec itself failed (see [Exit codes](running.md#exit-codes)).

The parts of a [parallel or sharded task](configuration.md#tasks) run at once. With `SPOTEXEC_AGENT=1` and stdout not a terminal, each part's output is held and printed as one block when the part finishes, still prefixed `[test-all 3/8]`, so one shard's failure reads in one place instead of spread across the interleaved lines of every shard. `sx --group=false <task>` keeps the live output.

A parallel or sharded task prints a line as each part finishes, and ends with a summary of the whole run: every part with its outcome (`failed`, `interrupted`, `stopped (fail_fast)` or `passed`), failed parts first, then a last line with the overall result, the parts that failed, the wall time and the combined exit code:

```console
sx: unit 2/8 · failed · 5m10s on aws#2 · exit 1
sx: unit 6/8 · failed · 6m1s on aws#6 · exit 1
sx: unit 1/8 · passed · 11m41s on aws#4
…
sx: ci · 2 of 9 parts failed (unit 2/8, unit 6/8) · 11m52s · exit 1
```

## Checking for spotexec

`spotexec supported` prints `true` and exits 0 when a run from the current directory would reach a runner: the project has a valid `.spotexec.toml` (here or above, up to the repository root), it isn't in a directory spotexec never syncs (your home directory, `/` or a credential directory), a runner is configured for it, and the working set passes the checks a run makes on the local files (it is within the file-count limit, a `container.dockerfile` and its build inputs are synced, and `deps.lockfiles` name at least one lockfile, none outside the repository). Whether a runner is reachable, the transfer fits, and the image builds and dependencies install, only a run finds out. Otherwise it prints `false` and exits 1, so an agent or script can choose between `sx` and running locally:

```sh
if spotexec supported >/dev/null; then sx pnpm test; else pnpm test; fi
```

`-C DIR` checks another directory. When it says `false`, the reason goes to stderr as one `spotexec:` line, so a broken config doesn't silently send every command to a local run; `-q` (`--quiet`) leaves it out. `spotexec doctor`, run from that directory, checks the config and runners in full.

## Same machine, next command

Each `sx` may land on a different VM. When an agent needs the machine it just used (it installed a package there), it adds `-s` (`--same-slot`): `sx -s pnpm test`. To look at files a failed run left in the slot without syncing (a synced run removes files that weren't written back), use `sx -s --sync none 'cat out.log'` or `spotexec exec`. See [coming back to the same machine](running.md#coming-back-to-the-same-machine). The `AGENTS.md` snippet from `hook install --codex` tells agents this.

## Machine-readable events

With `--json`, `SPOTEXEC_JSON=1` or `SPOTEXEC_AGENT=1`, lifecycle events go to stderr as NDJSON instead; `--events-file PATH` appends them to a file in any mode:

```json
{"event":"slot_acquired","runner":"aws","slot":3,"connect_ms":13,"waited_ms":0}
{"event":"sync_in","mode":"fingerprint_hit","files":0,"bytes":0,"duration_ms":0}
{"event":"sync_out","mutated_files":["src/index.ts"],"conflicts":[],"duration_ms":31}
{"event":"conflict","path":"src/app.ts","local":"/repo/src/app.ts","remote":"/repo/.git/spotexec/conflicts/20260101-120000-ab12cd34ef56/src/app.ts","deleted":false,"diff":"git diff --no-index -- /repo/src/app.ts /repo/.git/spotexec/conflicts/20260101-120000-ab12cd34ef56/src/app.ts","run_id":"20260101-120000-ab12cd34ef56"}
{"event":"result","exit_code":0,"duration_ms":4120,"timed_out":false}
```

A parallel or sharded task ends with a `result` event for the whole run, which (unlike each part's own `result`) lists its `parts` in task order, each with its `task`, `label`, `status` (`passed`, `failed`, `stopped` or `interrupted`), `exit_code` and `duration_ms`, its `shard` and `shards` when sharded, and where it ran: `runner` and `slot`, or `"local": true`.

Other events include:
- `queued`, `queue_position`, `placed`;
- `scale_out`, `provision`, `instance_running`, `instance_ready`;
- `boot_progress` while a new VM runs its first-boot setup: `elapsed_ms` and the latest `line` of its bootstrap log, at most every 5s;
- `manifest`, `deps`, `image`, `exec`, `pull`, `cancel`;
- `deps_failed` when the dependency install fails: the error's `code` and `message`, the install's last 40 lines (`output_tail`, `output_truncated` when earlier lines were left out) and the full `log` on this machine. The install's output never reaches the command's stdout or stderr, in any mode; a successful `deps` event also has its `log` when the install ran;
- `spot_interruption`, `spot_rebalance`, `retry`;
- `conflict`, one per write-back conflict: your kept file (`local`), the stashed remote version (`remote`, a marker when `deleted` is true), and a `diff` command to see the remote changes;
- `warning` events with stable codes (`SYNC_CONFLICT`, `RUNNER_LOST`, `SPOT_INTERRUPTION`, `TIMEOUT`, `LOCAL_FALLBACK`, `RTK_FILTER`, `STDIN_IGNORED`, …). `UNKNOWN_TASK` follows an exit 127 of `sx <word>` (no `--`) when the word is a plain name, no task has it, and the shell found no command by that name: its message names the closest task when exactly one is near, and `spotexec tasks` lists them all;
- `error` with code `SPOTEXEC_ERROR` when spotexec itself fails (exit `125`). For example, a `BOOTSTRAP_FAILED` message.

## Claude Code hook

```sh
spotexec hook install --claude    # .claude/settings.local.json (--shared: settings.json, --global: ~/.claude)
spotexec hook check 'pnpm test'   # show what the hook would do with a command line
```

The hook rewrites heavy Bash commands to `spotexec run -- '<cmd>'` before they execute:
- **Offloaded:** tests, `tsc`, linters, formatters, builds, Maven and Gradle verify goals.
- **Output filters stay local.** Agents often trim output with a pipe, as in `pnpm test 2>&1 | tail -50` or `cargo test | grep -E 'FAIL|panicked'`. The heavy part runs remotely and the filters (`tail`, `head`, `grep`, `sed`, `awk`, `wc`, `sort`, `uniq`, `cut`, `tr`, `tee`, `jq`, …) read its streamed output on your machine: `spotexec run -- 'pnpm test 2>&1' | tail -50`. spotexec's own warnings and summary go to stderr, outside the pipe, so a filter never hides a write-back conflict or a timeout.
- **Same meaning as the original line.** A leading `cd` runs locally, so the agent's shell and any local filter end up where it asked (`cd api && pnpm test | tee test.log` becomes `cd api && spotexec run -- 'pnpm test' | tee test.log`). A pipe filters only the command right before it, so a line like `pnpm build && pnpm test | tail -50` stays local rather than being split into two runs that could land on different machines.
- **Kept local:** `git`, `ls`, `rg`, dev servers, watch modes, any other pipe, redirects into files, background jobs, anything that `cd`s out of the repo or after a heavy command, and anything prefixed with `SPOTEXEC_LOCAL=1`. Use that prefix, for example, for a test that needs `localhost:5432`.
- **Timeout:** an offloaded command gets a Bash timeout of 10 minutes (or `BASH_MAX_TIMEOUT_MS`, if that is higher) unless the agent set one, since booting a Spot VM and installing dependencies can outlast Claude Code's 2-minute default.
- **No runners yet:** when spotexec has no usable runner (say the hook was installed before `spotexec setup`), heavy commands run locally and the hook says so once per session, to you and to the agent.
- **Scope:** it acts only in projects with a `.spotexec.toml`, and approves nothing itself. Your permission rules see the rewritten `spotexec run -- <cmd>`, so allow `Bash(spotexec run:*)` if you pre-approve test commands.

The `[hook]` table tunes it:
- `when_load_above = 0.6` offloads only when the laptop is busy;
- `patterns` / `exclude` add your own regexes;
- `mode = "block"` denies the command with instructions, instead of rewriting it;
- `disabled = true` turns it off.

A hook that v0.2.0 installed as `sx hook claude` still works the same way; `spotexec setup` (unless `--no-hooks`) and `spotexec hook install` rewrite it in place to call `spotexec`.

### With RTK

[RTK](https://github.com/rtk-ai/rtk) compacts command output for agents with a PreToolUse hook of its own, `rtk hook claude`, which rewrites many of the same commands (`cargo test` to `rtk cargo test`). Claude Code runs every matching hook on the original command and doesn't say which rewrite wins. With both hooks installed side by side, a test run would either stay local or come back uncompacted, whichever hook won.

So only one hook decides:
- **Chained.** `spotexec hook install`, and `spotexec setup` when it installs or keeps up spotexec's hook (not with `--no-hooks`), find RTK's hook in the settings file they install into and chain it behind spotexec's, in place: `spotexec hook claude --then 'rtk hook claude'`. spotexec decides first. Every command it doesn't offload goes to RTK unchanged, and so does every command outside a project with a `.spotexec.toml`. `--uninstall` puts RTK's own hook back. RTK doesn't recognise the chained form, so it may say `No hook installed — run rtk init -g` even though `rtk verify` passes. If `rtk init -g` adds RTK's hook again, run `spotexec hook install --claude --global` to fold it back into the chain.
- **Not chained.** RTK's hook may be in a different file, say `~/.claude/settings.json` when spotexec's is in the project. Then a command RTK would also rewrite is denied, and the reason tells the agent to run the offloaded form, which RTK leaves alone. The first denial in a session tells you which `spotexec hook install` command chains them. `spotexec hook check` shows when this would happen.

Where RTK is in use, offloaded output is still compacted. `rtk pipe` runs on this machine once the command finishes, through `spotexec run --rtk-filter <filter>`. The exit code stays the command's own, and spotexec's warnings and summary still reach stderr. If the command fails, the output ends with `[full output: <path>]` (the last 20 are kept). If RTK fails, the output is printed unfiltered with an `RTK_FILTER` warning. The hook adds `--rtk-filter` only for the commands whose raw output RTK's filters are known to read correctly:

| Command | Runs as | Filter |
| --- | --- | --- |
| `cargo test` | unchanged | `cargo-test` |
| `go test` | with `-json` added | `go-test` |

Every other offloaded command comes back uncompacted, exactly as it ran. RTK's other pipe filters can't read raw output that `rtk <tool>` didn't produce itself. Given plain `go test` or pytest output, they report "no tests" and hide the failures. A line the agent filters itself (`cargo test | tail -50`) is still offloaded, with its own filter kept and no RTK compaction.

An agent can also type the RTK form itself. `rtk cargo test` (and `rtk go`, `rtk pytest`, `rtk mypy`, `rtk ruff`, `rtk pnpm`, `rtk npx`), `rtk test <cmd>` and `rtk err <cmd>` are offloaded as the plain command, compacted where the table allows. `rtk proxy <cmd>` is offloaded uncompacted. `rtk vitest` and `rtk tsc` stay local, because RTK picks their package runner and arguments itself. Past 256 MB, or if it can't be written to disk, the output is printed as it comes, unfiltered, with an `RTK_FILTER` warning.

Never add `sx` or `spotexec` to RTK's `[hooks] transparent_prefixes`. RTK handles `sudo cargo test` as `sudo rtk cargo test`, so an `sx` prefix would most likely put `rtk` inside the remote command, on a VM that doesn't have it installed.

`spotexec hook install --codex` appends an `AGENTS.md` section for Codex and other harnesses without hooks. `--uninstall` removes either one.
