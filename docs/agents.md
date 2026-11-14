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

## Same machine, next command

Each `sx` may land on a different VM. When an agent needs the machine it just used (it installed a package there, or wants to look at files a failed run left behind), it adds `-s` (`--same-slot`): `sx -s pnpm test`. See [coming back to the same machine](running.md#coming-back-to-the-same-machine). The `AGENTS.md` snippet from `hook install --codex` tells agents this.

## Machine-readable events

With `--json`, `SPOTEXEC_JSON=1` or `SPOTEXEC_AGENT=1`, lifecycle events go to stderr as NDJSON instead; `--events-file PATH` appends them to a file in any mode:

```json
{"event":"slot_acquired","runner":"aws","slot":3,"connect_ms":13,"waited_ms":0}
{"event":"sync_in","mode":"fingerprint_hit","files":0,"bytes":0,"duration_ms":0}
{"event":"sync_out","mutated_files":["src/index.ts"],"conflicts":[],"duration_ms":31}
{"event":"result","exit_code":0,"duration_ms":4120,"timed_out":false}
```

Other events include:
- `queued`, `queue_position`, `placed`;
- `scale_out`, `provision`, `instance_running`, `instance_ready`;
- `manifest`, `deps`, `image`, `exec`, `pull`, `cancel`;
- `spot_interruption`, `spot_rebalance`, `retry`;
- `warning` events with stable codes (`SYNC_CONFLICT`, `RUNNER_LOST`, `SPOT_INTERRUPTION`, `TIMEOUT`, `LOCAL_FALLBACK`, …);
- `error` with code `SPOTEXEC_ERROR` when spotexec itself fails (exit `125`). For example, a `BOOTSTRAP_FAILED` message.

## Claude Code hook

```sh
spotexec hook install --claude    # .claude/settings.local.json (--shared: settings.json, --global: ~/.claude)
spotexec hook check 'pnpm test'   # show what the hook would do with a command line
```

The hook rewrites heavy Bash commands to `spotexec run -- '<cmd>'` before they execute:
- **Offloaded:** tests, `tsc`, linters, formatters, builds, Maven and Gradle verify goals.
- **Output filters stay local.** Agents often trim output with a pipe, as in `pnpm test 2>&1 | tail -50` or `cargo test | grep -E 'FAIL|panicked'`. The heavy part runs remotely and the filters (`tail`, `head`, `grep`, `sed`, `awk`, `wc`, `sort`, `uniq`, `cut`, `tr`, `tee`, `jq`, …) read its streamed output on your machine: `spotexec run -q -- 'pnpm test 2>&1' | tail -50`.
- **Kept local:** `git`, `ls`, `rg`, dev servers, watch modes, any other pipe, redirects into files, background jobs, anything that `cd`s out of the repo, and anything prefixed with `SPOTEXEC_LOCAL=1`. Use that prefix, for example, for a test that needs `localhost:5432`.
- **Scope:** it acts only in projects with a `.spotexec.toml`, and approves nothing itself. Your permission rules see the rewritten `spotexec run -- <cmd>`, so allow `Bash(spotexec run:*)` if you pre-approve test commands.

The `[hook]` table tunes it:
- `when_load_above = 0.6` offloads only when the laptop is busy;
- `patterns` / `exclude` add your own regexes;
- `mode = "block"` denies the command with instructions, instead of rewriting it;
- `disabled = true` turns it off.

`spotexec hook install --codex` appends an `AGENTS.md` section for Codex and other harnesses without hooks. `--uninstall` removes either one.
