# Running commands

```sh
sx [flags] <command> [args...]        # run a command remotely
sx [flags] -- <command> [args...]     # same, when the command is named like a subcommand
sx <task> [args...]                   # named task from .spotexec.toml
sx --same-slot <command>              # run on the same VM and slot as this worktree's last run
```

`sx` stops reading its own flags at the first word that isn't one, so `sx pnpm vitest run --watch=false` passes everything after `pnpm` through unchanged. Several arguments are shell-quoted one by one. A single argument runs as a shell line, as in `sx 'pnpm lint && pnpm test'`. Absolute paths under your worktree are mapped to the remote workspace.

The synced project is the enclosing Git worktree, or the nearest directory with a `.spotexec.toml`. Anywhere else, spotexec refuses to sync: it won't upload, say, your home directory with `~/.aws` and `~/.ssh` in it. Run `spotexec init` to mark a non-Git directory as a project, or use `--sync none`.

## Coming back to the same machine

Plain `sx` takes the first free slot. It prefers the one that already holds this worktree's files, so reruns usually land on the same VM, but that isn't guaranteed: the slot may be busy with another worktree, or the VM may have shut down after `idle_timeout`.

When the machine's state matters, use `--same-slot`. It syncs, runs and writes back as usual, but only in the slot this worktree used last, waiting for it if it's busy. That's the slot `spotexec status` shows and `spotexec pull` reads from. A typical agent loop:

```sh
sx pnpm test                                   # fails: libvips is missing
sx --same-slot 'sudo apt-get install -y libvips-dev'
sx --same-slot pnpm test                       # same VM, so libvips is there
```

If that VM is gone, `--same-slot` fails (exit `125`) and says so, rather than running somewhere the package isn't installed. For anything you'd need on every VM, put it in the [project container](environments.md#project-containers) or the runner's `bootstrap_script`. Set `SPOTEXEC_SAME_SLOT=1` to make every `sx` in a shell behave this way.

`--same-slot --sync none` skips the sync and the write-back, and runs on whatever is in the slot, for poking around after a failure. `spotexec exec -- <command>` is the same thing.

## Run flags

| Flag | Meaning |
| --- | --- |
| `--timeout D` | Wall-clock limit (default `5m`, or the project `timeout`). |
| `--queue-timeout D` | How long to wait for a free slot (default `10m`). |
| `--sync auto\|in\|none` | `auto` syncs and writes back; `in` syncs and discards changes; `none` runs on whatever the slot has. |
| `--writeback always\|on-success\|never` | When changes come back. |
| `-r, --runner NAME` / `--pool A,B` | Restrict where the command may run. |
| `--same-slot` | Run in this worktree's last slot (same VM), waiting if it's busy. See [above](#coming-back-to-the-same-machine). |
| `--exclusive` | Take every slot on a machine (the job gets all its CPUs). |
| `--priority N` | Queue priority when every slot is busy (higher first). |
| `--no-boot` | Never launch VMs for this command; use only what is running. |
| `--pull GLOB` | Also copy these gitignored paths back (repeatable). |
| `--no-deps` | Skip dependency hydration. |
| `--no-retry` | Don't replay the run if its VM is lost. |
| `--fallback-local` | Run locally if no runner could be reached (never after the command started remotely). |
| `--force-large-sync` | Allow syncing more than 100k files or 5 GiB. |
| `--json` / `--pretty` / `-q, --quiet` | Event format: NDJSON, spinner, or nothing. |
| `--events-file PATH` | Also append NDJSON events to a file. |
| `-C, --dir DIR` | Run as if started in `DIR`. |

## Environment on the runner

Every command gets:

| Variable | Value |
| --- | --- |
| `SPOTEXEC=1`, `CI=true` | Marks a remote run. |
| `SPOTEXEC_RUNNER` | Runner name. |
| `SPOTEXEC_WORKSPACE` | Absolute path of the remote checkout. |
| `SPOTEXEC_SLOT` | Host-wide slot number, unique across repos. |
| `SPOTEXEC_PORT_BASE` | `20000 + slot × 1000`. Bind test servers here and parallel jobs never collide. |
| `SPOTEXEC_CPUS` | This job's share: vCPUs ÷ slots, or all of them with `--exclusive`. |
| `SPOTEXEC_SLOTS` | Slots per machine. |
| `SPOTEXEC_RUN_ID`, `SPOTEXEC_ATTEMPT` | Stable across a replay; `SPOTEXEC_ATTEMPT` is `2` on the replay. |
| `COMPOSE_PROJECT_NAME` | `sx_<repo>_sNN`, so Docker Compose stacks don't collide. |
| `FORCE_COLOR`, `CLICOLOR_FORCE` | Set when your stdout is a terminal. |

Plus anything in `env_allow` (copied from your environment) and `env` (fixed values). Outside a project container, before each command the runner sources the login profile (`/etc/profile`, then `~/.bash_profile`, `~/.bash_login` or `~/.profile`) and then `remote_path_setup`.

## Exit codes

| Code | Meaning |
| --- | --- |
| *command's own* | The remote command's exit status, passed through. |
| `124` | The command hit `--timeout`. |
| `125` | spotexec itself failed: no runner, SSH, sync, bootstrap. In a parallel task, a `125` outranks a test failure, so agents can tell "tests failed" from "couldn't run tests". |
| `128+N` | The command died from signal N. |
| `130` | Interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`). |
| `1`, `2` | Usage or configuration error. |

## Write-back and artifacts

The manifest records the Git blob ID of every synced file: the **baseline**. After the command, the helper reports what changed by content, not timestamps, and each path is decided against your worktree *now*:

| Your local file now | Outcome |
| --- | --- |
| Equals the remote result | Nothing to do. |
| Equals the baseline | The remote version is applied atomically (temp file, then rename). |
| Anything else | You edited it during the run. **Your file is kept**, the remote version is saved to `<git-dir>/spotexec/conflicts/<run-id>/<path>`, and a `SYNC_CONFLICT` warning is emitted. |

Deletions apply deepest-first, then writes. New files only come back if Git wouldn't ignore them, so `dist/`, `target/` and `node_modules` never travel back. A runner can never write into `.git`, under any spelling.

To get **ignored outputs** back (coverage reports, build output), use one of:
- `--pull coverage/` on a run (globs are single-level: `--pull 'packages/*/coverage'`, and `**` is not recursive);
- `pull_artifacts` in `.spotexec.toml`;
- `spotexec pull <glob>` afterwards, from the last-used slot (`--dest DIR` to land them elsewhere).

Pulled files never overwrite tracked or non-ignored files, and never land in another repository or a git directory.
