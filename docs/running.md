# Running commands

```sh
sx [flags] <command> [args...]        # run a command remotely
sx [flags] <task> [args...]           # named task from .spotexec.toml
sx [flags] -- <command> [args...]     # the command, even if a task has its name
sx --same-slot <command>              # run on the same VM and slot as this worktree's last run
```

`sx` only runs things: every word after its flags is a command or one of this project's tasks, never a spotexec subcommand, so `sx status` runs a program called `status`. A task wins over a program of the same name: `sx build` runs your `build` task, and `sx -- build` runs the program called `build`. `spotexec run -- <command>` is the same as `sx -- <command>`: it never runs a task, which is why the [agent hook](agents.md#claude-code-hook) rewrites commands to it. Manage runners with `spotexec` (`spotexec status`, `spotexec doctor`, …), and use `sx -h` for the run flags and this project's tasks. `spotexec <command>` works like `sx <command>` as long as the command isn't named like a spotexec subcommand; `spotexec -- <command>` runs it whatever its name.

`sx` stops reading its own flags at the first word that isn't one, so `sx pnpm vitest run --watch=false` passes everything after `pnpm` through unchanged. Several arguments are shell-quoted one by one, except leading `NAME=value` words, which stay environment assignments whatever the value holds: `sx NODE_OPTIONS="--max-old-space-size=8192 --no-warnings" pnpm test`. A single argument runs as a shell line, as in `sx 'pnpm lint && pnpm test'`. Absolute paths under your worktree are mapped to the remote workspace.

The command's stdin is `/dev/null`, on a runner and in a local fallback alike: input piped or redirected into `sx` is not forwarded. `echo "x y" | sx -- cat` prints nothing, and `git diff | sx -- some-linter --stdin` checks nothing. When spotexec's own stdin holds unread input (a file, or a pipe with data in it) before or after the run, it warns `STDIN_IGNORED`, once, and the exit code stays the command's own. An empty pipe, as some agent harnesses pass, gets no warning. The check is best effort: a writer that only starts writing after the command finishes isn't seen. To give the command input, write it to a file in the project first and pass the path: `git diff > changes.diff && sx -- some-linter changes.diff`.

The synced project is the enclosing Git worktree, or the nearest directory with a `.spotexec.toml`. Anywhere else, spotexec refuses to sync: it won't upload, say, your home directory with `~/.aws` and `~/.ssh` in it. Run `spotexec init` to mark a non-Git directory as a project (it asks first, or takes `--yes`), or use `--sync none`. The home directory, `/` and anything inside `.aws`, `.ssh` or `.gnupg` are never projects, Git or not: `init` refuses them, a `.spotexec.toml` there is ignored, and `sx` won't sync them. Credential directories (`.aws`, `.ssh`, `.gnupg`) never sync unless Git tracks them, whatever `.spotexec.toml` says. Files in `.spotexec/`, such as a `[setup]` script or the container's Dockerfile, sync like any other, Git or not; outside Git, spotexec's own state there (`backup/`, `conflicts/`, `writeback.lock`) never does.

## Coming back to the same machine

Plain `sx` takes the first free slot. It prefers the one that already holds this worktree's files, so reruns usually land on the same VM, but that isn't guaranteed: the slot may be busy with another worktree, or the VM may have shut down after `idle_timeout`.

When the machine's state matters, use `--same-slot`. It syncs, runs and writes back as usual, but only in the slot this worktree used last, waiting for it if it's busy. While it waits it takes none of the machine's capacity, so other runs keep using the free slots; `spotexec status` lists it as waiting. That's the slot `spotexec status` shows and `spotexec pull` reads from. A typical agent loop, on a runner without a project container:

```sh
sx pnpm test                                   # fails: libvips is missing
sx --same-slot 'sudo apt-get install -y libvips-dev'
sx --same-slot pnpm test                       # same VM, so libvips is there
```

If that VM is gone (or `max_slots` has since dropped below that slot's number), `--same-slot` fails (exit `125`) and says so, rather than running somewhere the package isn't installed. So does `--same-slot` with a `--runner` or `--pool` that leaves out the runner it would return to. For anything you'd need on every VM, put it in the [project container](environments.md#project-containers) or the runner's `bootstrap_script`. In a project container, every run starts a fresh container: files in the workspace and `$HOME` carry over to the next `--same-slot` run, but system packages don't, so add them to the Dockerfile. spotexec warns `CONTAINER_FRESH` on a command that installs system packages (`apt-get install`, `apk add` and the like), and on a worktree's first `--same-slot` run or `exec`, rather than on every one. Set `SPOTEXEC_SAME_SLOT=1` to make every `sx` in a shell behave this way.

A parallel or sharded task's parts run in several slots at once, so `--same-slot` can't apply to the task itself. Afterwards, `--same-slot` returns to the slot of its first part (in `parallel` order, shard 1 first) that got its files synced and its container image, if any, ready; that's the same point at which a single run becomes the one `--same-slot` returns to.

`--same-slot --sync none` skips the sync and the write-back, and runs on whatever is in the slot, for poking around after a failure. `spotexec exec -- <command>` is the same as `sx --same-slot --sync none -- <command>`.

## Run flags

| Flag | Meaning |
| --- | --- |
| `--timeout D` | Wall-clock limit (default `5m`, or the project `timeout`). |
| `--queue-timeout D` | How long to wait for a free slot (default `10m`, or the project `queue_timeout`). |
| `--sync auto\|in\|none` | `auto` syncs and writes back; `in` syncs and discards changes; `none` runs on whatever the slot has. |
| `--writeback always\|on-success\|never` | When changes come back. |
| `--fail-on-conflict` | Exit `122` when the command succeeded but write-back kept a local edit over one of its changes (`SYNC_CONFLICT`), so a caller that only reads the exit status knows the changes didn't all land. Also `SPOTEXEC_FAIL_ON_CONFLICT=1`. See [write-back](#write-back-and-artifacts). |
| `--writeback-over warn\|hold` | Over the [write-back limit](#write-back-limits): bring everything back with a warning, or leave the new files in the slot. |
| `-r, --runner NAME` / `--pool A,B` | Restrict where the command may run. |
| `--same-slot` | Run in this worktree's last slot (same VM), waiting if it's busy. See [above](#coming-back-to-the-same-machine). |
| `--exclusive` | Take every slot on a machine (the job gets all its CPUs). |
| `--priority N` | Queue priority when every slot is busy (higher first). |
| `--no-boot` | Never launch VMs for this command; use only what is running. |
| `--pull GLOB` | Also copy these gitignored paths back (repeatable). |
| `--no-deps` | Skip dependency hydration. |
| `--no-setup` | Skip the project's [`[setup]`](configuration.md#setup) step. |
| `--no-retry` | Don't replay the run if its VM is lost. A run whose VM was out of memory when it was lost is never replayed (see [exit codes](#exit-codes)). |
| `--fallback-local` | Run locally if no runner could be reached (never after the command started remotely). It runs as on a runner: under `bash` in its own process group, with stdin from `/dev/null`, the same `--timeout` (exit `124`) and grace period, and Ctrl-C, `SIGTERM` or `SIGHUP` stopping it (exit `130`). Each part of a parallel or sharded task falls back on its own, with its own `SPOTEXEC_SLOT` and port range. |
| `--group` | In a parallel or sharded task, hold each part's output and print it as one block, still prefixed, when the part finishes; with the spinner or plain progress, its summary line follows the block. Other parts' warnings and summaries (or, with `--json`, their events) still show while they run, never inside a block. Past 8 MiB a part's held output moves to a temporary file. On by default with `SPOTEXEC_AGENT=1` when stdout isn't a terminal; `--group=false` keeps the live, interleaved output, and beats the task's [`group`](configuration.md#tasks) setting, which beats the agent default. Give it before the task name: `sx --group test-all`. |
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
| `SPOTEXEC_PORT_BASE` | `20000 + slot × 1000`. Bind test servers here and parallel jobs never collide. For tests that bind fixed ports, see [private networking](environments.md#settings). |
| `SPOTEXEC_CPUS` | This job's share: vCPUs ÷ slots, rounded down (at least 1), or all of them with `--exclusive`. In a [project container](environments.md#project-containers) on a Linux runner, the job is normally also [pinned](environments.md#settings) to that many CPUs, so `nproc` and other tools that count the CPUs they may use see the same number. |
| `SPOTEXEC_MEMORY` | This job's fair share of memory in MiB: the machine's memory, less a reserve for the runner itself (1/16 of it, between 512 MiB and 2 GiB), ÷ slots, or all of that with `--exclusive`; at least 256. Advice for sizing heaps and worker counts (`NODE_OPTIONS=--max-old-space-size=$((SPOTEXEC_MEMORY * 3 / 4))`); nothing enforces it. In a [project container](environments.md#settings), jobs sharing a VM are capped together, not one by one. |
| `SPOTEXEC_SLOTS` | Slots per machine. |
| `SPOTEXEC_SHARD`, `SPOTEXEC_SHARDS` | Only in a [sharded task](configuration.md#tasks): this part's number (from 1) and the number of parts, the same as `{shard}` and `{shards}`. |
| `SPOTEXEC_RUN_ID`, `SPOTEXEC_ATTEMPT` | Stable across a replay; `SPOTEXEC_ATTEMPT` is `2` on the replay. |
| `COMPOSE_PROJECT_NAME` | `sx_<repo>_sNN`, so Docker Compose stacks don't collide. In a project container, Compose needs [`container.docker`](environments.md#docker-inside-the-container). |
| `FORCE_COLOR`, `CLICOLOR_FORCE` | Set when your stdout is a terminal. |

Plus anything in `env_allow` (copied from your environment) and `env` (fixed values). Outside a project container, before each command the runner sources the login profile (`/etc/profile`, then `~/.bash_profile`, `~/.bash_login` or `~/.profile`) and then `remote_path_setup`.

On `aws-spot` VMs, `localhost` resolves to both `127.0.0.1` and `::1`, as on macOS and GitHub's hosted runners. Stock Ubuntu maps it to `127.0.0.1` only, which breaks a test that binds a server to `::1` and dials `localhost`. Project containers share the VM's `/etc/hosts`. `ssh` runners are your own machines, and spotexec leaves their `/etc/hosts` alone.

## Git on the runner

The workspace on the runner has no `.git`, so by default anything that asks Git about the checkout fails there while it works locally and in CI: a build stamping the commit SHA, a tool finding the repository root with `git rev-parse --show-toplevel`, a test recording `git status`. Set `git_seed = true` under `[sync]` and, before dependencies install and the command runs, the slot gets a small real repository:

- HEAD is your local HEAD commit, on your branch (or detached, or an unborn branch, as locally). `git rev-parse HEAD`, `git rev-parse --abbrev-ref HEAD`, `git log -1` and `git describe --always` answer as they do locally, and `git rev-parse --show-toplevel` names the workspace (printed as your local path).
- `git status` and `git diff --name-only HEAD` list the same modified, deleted, added and untracked files as locally, relative to HEAD. The runner's index is built from HEAD, so nothing is staged there: a change you staged shows as unstaged, a new file you staged as intent-to-add (` A`), and a file you only untracked (`git rm --cached`) as unchanged. Tracked files that aren't synced (excluded, or submodules) don't show up as deleted.
- `git diff` works for the files you changed: their HEAD versions travel along (up to 16 MiB each).

Only HEAD's commit and trees leave your machine, plus the HEAD versions of files you changed, never of excluded files: no other history, branches, tags, config, hooks, remotes or credentials. So it's a one-commit shallow repository, and anything past HEAD doesn't work: `HEAD~1`, `git blame`, `git describe` without `--always`, `@{upstream}`, or `--since main`-style modes that compare against another branch. Objects for unchanged files aren't there either, so `git stash`, `git commit`, `git archive` and `git show HEAD:<file>` for an unchanged file fail.

Nothing a command does in `.git` comes back: the post-run scan skips it and write-back never touches your `.git`. The next run puts the seed back, dropping any commits, refs, config or hooks a command added. When HEAD and your changes are the same as last run, that's one round trip; a new HEAD sends only the trees and blobs the slot doesn't have. Turning `git_seed` off removes the seeded `.git` on the next synced run.

The runner itself needs `git` installed, even when commands run in a project container. Without it, or for a project synced as a plain directory (`sync.source = "directory"`), spotexec warns (`GIT_SEED_UNAVAILABLE` or `GIT_SEED_FAILED`) and runs the command without a `.git`. A [project container](environments.md#project-containers) sees the same `.git` in the bind-mounted workspace; with `container.user = "root"`, spotexec passes `safe.directory` for the workspace through `GIT_CONFIG_*` (unless the project sets those itself), since Git otherwise refuses a repository another user owns. Older Git releases only read `safe.directory` from system or global config; if Git in your image still reports "dubious ownership", add `RUN git config --system --add safe.directory '*'` to the Dockerfile.

## Exit codes

| Code | Meaning |
| --- | --- |
| *command's own* | The remote command's exit status, passed through. |
| `1` | Also: the project's [`[setup]`](configuration.md#setup) failed (`SETUP_FAILED`), so the command never ran. |
| `122` | With `--fail-on-conflict` (or `SPOTEXEC_FAIL_ON_CONFLICT=1`) only: the command exited `0`, but write-back kept your version of at least one file it changed (`SYNC_CONFLICT`). A command that failed, timed out or was interrupted keeps its own code. In a parallel or sharded task the part counts as failed, so `fail_fast` stops the rest. |
| `124` | The command hit `--timeout`, or `[setup]` hit its timeout. |
| `125` | spotexec itself failed: no runner, SSH, sync, bootstrap, or the runner couldn't start the command at all. In a parallel or sharded task, a `125` outranks a test failure, so agents can tell "tests failed" from "couldn't run tests". A Spot VM lost mid-run is replayed once on another slot (unless `--no-retry`), but not when its heartbeats showed it out of memory (`MEMORY_PRESSURE`), or when EC2 still reports it running and it gave no Spot notice (`RUNNER_UNRESPONSIVE`): the run may be what overloaded it, and replayed it could take another VM's jobs down with it. |
| `128+N` | The command died from signal N. An `OOM_KILLED` warning (with `137`, SIGKILL, when the killed process was the command itself) means the kernel killed a process in the command's container because the VM's jobs together reached their [memory pool](environments.md#settings)'s cap and it was the largest (or, outside the pool, because the container reached a memory limit set in `container.options`, or the whole VM ran out): the command ran out of memory, and the run isn't replayed. `134` (SIGABRT) with `JavaScript heap out of memory` or `Ineffective mark-compacts near heap limit` in the output is Node's own heap limit, not the machine's: raise `--max-old-space-size`. |
| `130` | Interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`), even if the command caught the signal and exited `0`. An interrupted run never counts as a success for `writeback = "on-success"`. |

## Write-back and artifacts

The manifest records the Git blob ID of every synced file: the **baseline**. After the command, the helper reports what changed by content, not timestamps, and each path is decided against your worktree *now*:

| Your local file now | Outcome |
| --- | --- |
| Equals the remote result | Nothing to do. |
| Equals the baseline | The remote version is applied atomically (temp file, then rename). Your file keeps its permissions; only an executable-bit change made remotely comes back. |
| Anything else | You edited it during the run. **Your file is kept**, the remote version is saved to `<git-dir>/spotexec/conflicts/<run-id>/<path>`, and a `SYNC_CONFLICT` warning gives a ready-to-run `git diff --no-index` command comparing your file with the remote version. With `--json`, a `conflict` event carries both paths and that command. |
| Another path of the run's file | Two remote names are one file here: the filesystem ignores case or Unicode normalisation (macOS does by default), so `src/Dup.txt` opens `src/DUP.txt`. The file under the local name is kept, the other remote version is saved to the same conflicts directory, and a `SYNC_COLLISION` warning names both paths, says which version was kept, and gives the same `git diff --no-index` command. Its `conflict` event carries the other path as `with`. |

The summary line counts the conflicts (`· 1 conflict`), and every `result` event carries `conflict_count` (a parallel or sharded task's also has each part's in `parts`). The exit status is still the command's own, since its other changes did land; with `--fail-on-conflict` a run that would have exited `0` exits `122` instead. A conflict never rolls back what was applied.

Before write-back deletes or replaces a local file, it checks whether Git could give that version back: HEAD holds the same path with the same content and mode. If not (an untracked file, an uncommitted or staged edit, or any file outside Git), your version is first copied to `<git-dir>/spotexec/backup/<run-id>/<path>` (`.spotexec/backup/<run-id>/` outside Git), and a `WRITEBACK_BACKUP` warning says where. So a remote `git checkout -- .`, `git clean` or `rm -rf` never takes work that exists only on this machine. With `--json`, a `backup` event lists the files and the directory. Backups, like conflicts, stay until you remove them.

Some deletions are held back: the local file is kept, and a `WRITEBACK_HELD` warning names it (with `--json`, a `held_deletions` event). That happens to the project's own root `.spotexec.toml` and `.gitignore`, and to the deletions of work that would otherwise be lost when a run deleted at least 5 such files and at least half of the synced files. A deletion Git can undo (HEAD has the file as it was) or whose content the run wrote back under another path doesn't count and always applies, so moving files into a new directory isn't held. If the deletion was intended, delete the files yourself.

Deletions apply deepest-first, then writes. New files only come back if Git wouldn't ignore them (created no more permissively than your umask allows), so `dist/`, `target/` and `node_modules` never travel back. A runner can never write into `.git`, under any spelling. Symlinks come back as symlinks; one the run created or retargeted to point outside the worktree (an absolute target, one that climbs above the root, or one that leads out through another link) is applied with a `WRITEBACK_SYMLINK` warning naming each link and its target, and with `--json` an `outside_symlink` event per link.

A new file the run created that doesn't come back (`--writeback never`, `--sync in`, a failed `on-success` run, a `--sync none` run, a dropped connection) stays in the slot until the next run that syncs into it, which removes it. Until then `spotexec exec` and `sx -s --sync none` can still look at it. Ignored outputs and `cache_dirs` are kept, like any build cache.

To get **ignored outputs** back (coverage reports, build output), use one of:
- `--pull coverage/` on a run (globs are single-level: `--pull 'packages/*/coverage'`, and `**` is not recursive);
- `pull_artifacts` in `.spotexec.toml`;
- `spotexec pull <glob>` afterwards, from the last-used slot (`--dest DIR` to land them elsewhere).

Pulled files never overwrite tracked or non-ignored files, and never land in another repository or a git directory. The one exception is files a write-back [held](#write-back-limits). A pulled symlink that points outside the worktree (or, with `--dest` elsewhere, outside that directory) gets the same `WRITEBACK_SYMLINK` warning and `outside_symlink` event as write-back gives one, the link named where it landed.

### Write-back limits

A command that dumps a heap snapshot, a coverage tree or generated fixtures where Git doesn't ignore them would otherwise flood the worktree, and every later run would upload them again. So a write-back bringing back more than `writeback_max_files` files (default 1000) or `writeback_max_bytes` bytes (default 100 MB) warns before it transfers anything, with `LARGE_WRITEBACK` naming the largest paths: single files, and directories with how many files they hold. Set either limit to `0` to turn it off.

With `writeback_over = "hold"` in `[sync]` (or `--writeback-over hold`, or `SPOTEXEC_WRITEBACK_OVER=hold`), new files over the limit stay in the slot instead: none of them comes back, and a `WRITEBACK_HELD` warning says how many, how big and where. Changes to and deletions of files you synced still come back as usual, so a formatter rewriting 5,000 files only warns. Only the new files count toward holding, so a command that moves a directory gets its deletions applied while the moved files wait in the slot.

`spotexec pull --held` brings the held files back (`spotexec pull --held gen/` only some of them), and `spotexec pull <glob>` takes held files along with ignored outputs. A held file only lands where nothing is now, and only as the run left it; Git still mustn't track it. Get them before the next run that syncs into the slot, which removes them. They come from the slot `spotexec pull` reads, the last one this worktree used, so held files of the other parts of a parallel task can't be pulled.
