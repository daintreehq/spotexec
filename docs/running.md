# Running commands

```sh
sx [flags] <command> [args...]        # run a command remotely
sx [flags] <task> [args...]           # named task from .spotexec.toml
sx [flags] -- <command> [args...]     # the command, even if a task has its name
sx --same-slot <command>              # run on the same VM and slot as this worktree's last run
```

`sx` only runs things: every word after its flags is a command or one of this project's tasks, never a spotexec subcommand, so `sx status` runs a program called `status`. A task wins over a program of the same name: `sx build` runs your `build` task, and `sx -- build` runs the program called `build`. `spotexec run -- <command>` is the same as `sx -- <command>`: it never runs a task, which is why the [agent hook](agents.md#claude-code-hook) rewrites commands to it. Manage runners with `spotexec` (`spotexec status`, `spotexec doctor`, …), and use `sx -h` for the run flags and this project's tasks. `spotexec <command>` works like `sx <command>` as long as the command isn't named like a spotexec subcommand; `spotexec -- <command>` runs it whatever its name.

`sx` stops reading its own flags at the first word that isn't one, so `sx pnpm vitest run --watch=false` passes everything after `pnpm` through unchanged. Several arguments are shell-quoted one by one, except leading `NAME=value` words, which stay environment assignments whatever the value holds: `sx NODE_OPTIONS="--max-old-space-size=8192 --no-warnings" pnpm test`. A single argument runs as a shell line, as in `sx 'pnpm lint && pnpm test'`. Absolute paths under your worktree are mapped to the remote workspace.

The synced project is the enclosing Git worktree, or the nearest directory with a `.spotexec.toml`. Anywhere else, spotexec refuses to sync: it won't upload, say, your home directory with `~/.aws` and `~/.ssh` in it. Run `spotexec init` to mark a non-Git directory as a project (it asks first, or takes `--yes`), or use `--sync none`. The home directory, `/` and anything inside `.aws`, `.ssh` or `.gnupg` are never projects, Git or not: `init` refuses them, a `.spotexec.toml` there is ignored, and `sx` won't sync them. Credential directories (`.aws`, `.ssh`, `.gnupg`) never sync unless Git tracks them, whatever `.spotexec.toml` says.

## Coming back to the same machine

Plain `sx` takes the first free slot. It prefers the one that already holds this worktree's files, so reruns usually land on the same VM, but that isn't guaranteed: the slot may be busy with another worktree, or the VM may have shut down after `idle_timeout`.

When the machine's state matters, use `--same-slot`. It syncs, runs and writes back as usual, but only in the slot this worktree used last, waiting for it if it's busy. While it waits it takes none of the machine's capacity, so other runs keep using the free slots; `spotexec status` lists it as waiting. That's the slot `spotexec status` shows and `spotexec pull` reads from. A typical agent loop, on a runner without a project container:

```sh
sx pnpm test                                   # fails: libvips is missing
sx --same-slot 'sudo apt-get install -y libvips-dev'
sx --same-slot pnpm test                       # same VM, so libvips is there
```

If that VM is gone (or `max_slots` has since dropped below that slot's number), `--same-slot` fails (exit `125`) and says so, rather than running somewhere the package isn't installed. So does `--same-slot` with a `--runner` or `--pool` that leaves out the runner it would return to. For anything you'd need on every VM, put it in the [project container](environments.md#project-containers) or the runner's `bootstrap_script`. In a project container, every run starts a fresh container: files in the workspace and `$HOME` carry over to the next `--same-slot` run, but system packages don't (spotexec warns `CONTAINER_FRESH`), so add them to the Dockerfile. Set `SPOTEXEC_SAME_SLOT=1` to make every `sx` in a shell behave this way.

A parallel task's parts run in several slots at once, so `--same-slot` can't apply to the task itself. Afterwards, `--same-slot` returns to the slot of its first part (in `parallel` order) that got its files synced and its container image, if any, ready; that's the same point at which a single run becomes the one `--same-slot` returns to.

`--same-slot --sync none` skips the sync and the write-back, and runs on whatever is in the slot, for poking around after a failure. `spotexec exec -- <command>` is the same as `sx --same-slot --sync none -- <command>`.

## Run flags

| Flag | Meaning |
| --- | --- |
| `--timeout D` | Wall-clock limit (default `5m`, or the project `timeout`). |
| `--queue-timeout D` | How long to wait for a free slot (default `10m`, or the project `queue_timeout`). |
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
| `--fallback-local` | Run locally if no runner could be reached (never after the command started remotely). It runs as on a runner: under `bash` in its own process group, with stdin from `/dev/null`, the same `--timeout` (exit `124`) and grace period, and Ctrl-C, `SIGTERM` or `SIGHUP` stopping it (exit `130`). Each part of a parallel task falls back on its own, with its own `SPOTEXEC_SLOT` and port range. |
| `--force-large-sync` | Allow syncing more than 100k files or 5 GiB. |
| `--json` / `--pretty` / `-q, --quiet` | Progress format: NDJSON events, the spinner (even without a terminal), or warnings only (no progress or summary). `-q` wins over `--json`, and `--json` over `--pretty`. Without them you get the spinner at a terminal and one plain summary line otherwise, or NDJSON with `SPOTEXEC_JSON=1` or `SPOTEXEC_AGENT=1` (`-q`, `--json`, `--pretty` and `--json=false` override those). |
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
| `SPOTEXEC_CPUS` | This job's share: vCPUs ÷ slots, rounded down (at least 1), or all of them with `--exclusive`. In a [project container](environments.md#project-containers) on a Linux runner, the job is normally also [pinned](environments.md#settings) to that many CPUs, so `nproc` and other tools that count the CPUs they may use see the same number. |
| `SPOTEXEC_SLOTS` | Slots per machine. |
| `SPOTEXEC_RUN_ID`, `SPOTEXEC_ATTEMPT` | Stable across a replay; `SPOTEXEC_ATTEMPT` is `2` on the replay. |
| `COMPOSE_PROJECT_NAME` | `sx_<repo>_sNN`, so Docker Compose stacks don't collide. |
| `FORCE_COLOR`, `CLICOLOR_FORCE` | Set when your stdout is a terminal. |

Plus anything in `env_allow` (copied from your environment) and `env` (fixed values). Outside a project container, before each command the runner sources the login profile (`/etc/profile`, then `~/.bash_profile`, `~/.bash_login` or `~/.profile`) and then `remote_path_setup`.

On `aws-spot` VMs, `localhost` resolves to both `127.0.0.1` and `::1`, as on macOS and GitHub's hosted runners. Stock Ubuntu maps it to `127.0.0.1` only, which breaks a test that binds a server to `::1` and dials `localhost`. Project containers share the VM's `/etc/hosts`. `ssh` runners are your own machines, and spotexec leaves their `/etc/hosts` alone.

## Exit codes

| Code | Meaning |
| --- | --- |
| *command's own* | The remote command's exit status, passed through. |
| `124` | The command hit `--timeout`. |
| `125` | spotexec itself failed: no runner, SSH, sync, bootstrap, or the runner couldn't start the command at all. In a parallel task, a `125` outranks a test failure, so agents can tell "tests failed" from "couldn't run tests". |
| `128+N` | The command died from signal N. |
| `130` | Interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`), even if the command caught the signal and exited `0`. An interrupted run never counts as a success for `writeback = "on-success"`. |

## Write-back and artifacts

The manifest records the Git blob ID of every synced file: the **baseline**. After the command, the helper reports what changed by content, not timestamps, and each path is decided against your worktree *now*:

| Your local file now | Outcome |
| --- | --- |
| Equals the remote result | Nothing to do. |
| Equals the baseline | The remote version is applied atomically (temp file, then rename). Your file keeps its permissions; only an executable-bit change made remotely comes back. |
| Anything else | You edited it during the run. **Your file is kept**, the remote version is saved to `<git-dir>/spotexec/conflicts/<run-id>/<path>`, and a `SYNC_CONFLICT` warning gives a ready-to-run `git diff --no-index` command comparing your file with the remote version. With `--json`, a `conflict` event carries both paths and that command. |

Deletions apply deepest-first, then writes. New files only come back if Git wouldn't ignore them (created no more permissively than your umask allows), so `dist/`, `target/` and `node_modules` never travel back. A runner can never write into `.git`, under any spelling.

A new file the run created that doesn't come back (`--writeback never`, `--sync in`, a failed `on-success` run, a `--sync none` run, a dropped connection) stays in the slot until the next run that syncs into it, which removes it. Until then `spotexec exec` and `sx -s --sync none` can still look at it. Ignored outputs and `cache_dirs` are kept, like any build cache.

To get **ignored outputs** back (coverage reports, build output), use one of:
- `--pull coverage/` on a run (globs are single-level: `--pull 'packages/*/coverage'`, and `**` is not recursive);
- `pull_artifacts` in `.spotexec.toml`;
- `spotexec pull <glob>` afterwards, from the last-used slot (`--dest DIR` to land them elsewhere).

Pulled files never overwrite tracked or non-ignored files, and never land in another repository or a git directory.
