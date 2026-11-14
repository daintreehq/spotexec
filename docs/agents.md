# Using spotexec with coding agents

## Machine-readable output

When stderr isn't a terminal (or with `--json`, or `SPOTEXEC_AGENT=1`), lifecycle events go to stderr as NDJSON. The command's own stdout and stderr pass through untouched:

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
- **Kept local:** `git`, `ls`, `rg`, dev servers, watch modes, pipes, redirects, background jobs, anything that `cd`s out of the repo, and anything prefixed with `SPOTEXEC_LOCAL=1`. Use that prefix, for example, for a test that needs `localhost:5432`.
- **Scope:** it acts only in projects with a `.spotexec.toml`, and approves nothing itself. Your permission rules see the rewritten `spotexec run -- <cmd>`, so allow `Bash(spotexec run:*)` if you pre-approve test commands.

The `[hook]` table tunes it:
- `when_load_above = 0.6` offloads only when the laptop is busy;
- `patterns` / `exclude` add your own regexes;
- `mode = "block"` denies the command with instructions, instead of rewriting it;
- `disabled = true` turns it off.

`spotexec hook install --codex` appends an `AGENTS.md` section for Codex and other harnesses without hooks. `--uninstall` removes either one.
