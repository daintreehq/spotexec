# How spotexec works
## How it works

```
laptop                                         runner (SSH box or Spot VM)
──────                                         ───────────────────────────
sx ─┬─ manifest: git ls-files + blob IDs       spotexec-helper (uploaded on first use)
    ├─ OpenSSH ControlMaster ──────── SSH ───▶  ├─ flock'd concurrency tokens + workspace slots
    └─ framed RPC over stdio ─────────────────▶ ├─ applies the sync, hydrates deps
                                                ├─ runs the command in its own process group
EC2 API (your credentials) ◀── sx               ├─ snapshots files before/after → mutation list
  RunInstances / Describe / Terminate           └─ dependency cache (reflink/copy, S3 restore)
  tags: spotexec-runner, spotexec-owner
                                                cloud-init idle watchdog (systemd timer) ─▶ poweroff
```

A single `sx` invocation:

1. **Builds a manifest** of the worktree: every file Git tracks or would track (respecting `.gitignore`, `.spotexecignore` and `sync.exclude`), with Git blob IDs. Clean tracked files reuse the index IDs and are never read.
2. **Acquires a slot.** It tries runners in pool order and takes a free slot without blocking. When every slot is busy it joins a fair local queue and, for Spot fleets, registers demand that may launch more VMs.
3. **Syncs.** The helper reports which blobs the slot is missing, and only those travel, as one gzip'd tar over the same SSH channel. An unchanged worktree is a fingerprint hit and sends nothing.
4. **Hydrates dependencies** from a cache keyed by your lockfiles, or runs your install command once and caches the result.
5. **Runs the command** in its own process group, with a wall-clock timeout and heartbeats in both directions. Output streams back live.
6. **Writes back.** The helper hashes the workspace after the run and reports changed, created and deleted files. The CLI applies each one only if your local copy still matches what was synced.

Managing Spot instances needs no service running anywhere. Each concern below is covered by your laptop, the VM itself, or EC2:

| Concern | Who handles it |
| --- | --- |
| Launching a VM | The CLI, on demand, calling EC2 with your own AWS credentials. |
| Knowing which VMs exist | EC2 itself. Instances carry `spotexec-runner` and `spotexec-owner` tags; a local record only caches addresses. |
| Tearing VMs down | **The VM itself.** cloud-init first installs a systemd timer that powers the VM off after `idle_timeout` without activity, and unconditionally after `max_lifetime`. Instances launch with `InstanceInitiatedShutdownBehavior=terminate`, so power-off means terminate and billing stops. Closing your laptop lid doesn't matter. |
| Not shutting down mid-job | While it owns a slot, the helper holds a shared `flock` on `/var/tmp/spotexec/lifecycle.lock`. The watchdog needs that lock exclusively before it shuts down. |
| Stuck sessions (laptop asleep, network blackhole) | The CLI and helper exchange heartbeats. After 45s of silence the helper stops the command and releases the slot, so the idle clock resumes. After 75s without a word from the helper, in any phase (queueing for a slot, syncing, installing, running), the CLI gives up on the runner as a lost connection. |
| A reused SSH connection that died while the laptop slept | Each runner's ControlMaster persists for 15 minutes. A reused master that can't open a session within 5s is retired (`ssh -O stop`, which leaves other sessions alone), and the run dials a fresh connection and carries on. |
| Spot interruptions | The helper polls IMDSv2 for the 2-minute notice and marks the VM draining. The CLI starts finding replacement capacity right away and replays the run if the VM dies first, or if its shutdown kills the command (nothing from that attempt is written back). |
| Concurrent agents booting the same fleet | A local `flock`-protected launch ledger, plus an idempotent `ClientToken` on `RunInstances`. |
| A VM that never finishes booting | Reaching `running` and each bootstrap script are bounded by `boot_timeout`. A VM still not ready after `2 × boot_timeout + 2m` is terminated by ID by the next `sx` or `spotexec daemon` that sees it; otherwise its own watchdog powers it off. |

## Security model

The trust boundaries, from the inside out:

- **The VMs hold no AWS credentials.** They have no instance profile, and they get presigned URLs for exactly the objects they need. Instance metadata requires IMDSv2.
- **A runner can't write outside the worktree, or into `.git`, and can't read your other files.** Every path a runner sends back goes through these checks:
  - it must be clean and relative, with no `.git` segment under any case spelling;
  - it must not resolve through a symlink;
  - for a changed or deleted file, it must be in the manifest of what was synced;
  - for a new file, it must not be ignored, excluded, or already present locally, so a local file is never overwritten;
  - for a sync request, it must be in the manifest too, so a runner can't ask for `~/.aws/credentials`;
  - for a pull, it must match a pattern you asked for.
- **A cloned repository can't change how you connect.** `.spotexec.toml` can't set hosts, SSH options, keys, AWS profile, region, network or bucket. Project runners must extend one of your own runners.
- **Project config is still code for the runner.** Bootstrap scripts, tasks and `container.options` (for example `--privileged`) run with the runner user's rights on the runner. On a VM of your own, that only matters to you. On a **shared `ssh` runner**, treat `.spotexec.toml` like a CI config: review it before running untrusted repos.
- **`git_seed` sends HEAD, not your repository.** With `[sync] git_seed`, the runner gets HEAD's commit object, its trees and the HEAD versions of files you changed (never those of excluded files), and nothing else from `.git`: no other history, refs, config, hooks or remotes. The CLI sends only objects of that set, whatever the runner asks for. The seeded `.git` is never scanned for changes or written back.
- **Secrets stay out of process listings.** Forwarded environment values go to the runner in a `0600` file, not on the command line.
- **Slots are private.** The work root is `0700`. Lifecycle state files are writable only by the runner user's group.
- **The runner is trusted with your worktree.** Write-back is how formatters and codegen deliver their results, so a compromised runner can still change synced files and create new non-ignored ones, such as a `Makefile` or editor task config. Review `git status` after runs on machines you don't control.

## Known limits

- **Process cleanup is not cgroup-based.** Commands run in their own process group. On Linux runners, processes that escaped it (for example via `setsid`) are killed after the run, and again before the slot's next run, if their working directory is inside the slot or they inherited the run's `SPOTEXEC_WORKSPACE`; that covers a helper killed outright (OOM, `SIGKILL`) too. One that also cleared its environment can survive, as can any escaped process on a macOS runner.
- **Write-back can't be atomic against editors that ignore locks.** Each file is checked before its new content is staged, and again just before the rename. A save landing in that last syscall window can still be replaced.
- **Case-only directory renames keep the old spelling locally.** On a case-insensitive filesystem (the macOS default), a remote `mv src Src` updates the files but leaves the directory named `src`. Case-only *file* renames are applied correctly.
- **Each machine gets its own instances.** Instances are tagged with this machine's owner ID, and a machine's runs never use or terminate another's. Only the account-wide commands look at other machines' VMs: `spotexec status -A` and `down -A`, `setup --revoke` and `teardown` (baked images, by contrast, are shared). To share VMs between two machines, copy both `owner-id` and the SSH key in `keys/`, since each VM only accepts the key of the machine that launched it. They then coordinate only through EC2's eventually consistent tag filters. Machines do share the account's Spot vCPU quota: each checks live usage before launching ([sharing the quota](scaling.md#several-machines-one-spot-quota)).
- **Scaling can't predict demand.** A VM launched for demand that clears before it boots sits idle until `idle_timeout`, then terminates itself.
- **Exclusive jobs across mismatched fleets.** Demand is counted per fleet, so an `--exclusive` job that could land on two fleets with different `max_slots` may briefly over-launch by one instance.
- **Shared SSO cache.** Without the AWS CLI, spotexec refreshes IAM Identity Center tokens itself. If an AWS CLI elsewhere (say, a dev container sharing `~/.aws`) refreshes the same token at the same moment, one side may need `aws sso login` again.
- **`git_seed` is a one-commit repository.** History, other branches and upstreams are not there, nor the contents of unchanged files, so `git blame`, `git stash`, `git commit` and comparisons against another branch fail on the runner. Nothing is staged there. Your local Git config doesn't travel, so files it changes the meaning of (Git LFS, `core.autocrlf`, `core.filemode = false`, `core.symlinks = false`) can show as modified there, as can a staged change to a file you then marked assume-unchanged or skip-worktree. Submodules always look clean. SHA-256 repositories aren't supported.
- **Providers are `ssh` and `aws-spot`.** The provider interface is ready for more.

## Design decisions

- **No backend.** Coordination lives in local `flock`ed state plus cloud tags, and every VM is responsible for its own death. Losing your laptop mid-run costs, at most, one `idle_timeout` of Spot time.
- **No rsync.** Sync streams a gzip'd tar of exactly the blobs the slot is missing, over the RPC channel. macOS ships openrsync, which differs from GNU rsync 3.x. Owning the bytes also makes the write-back baseline exact: what was uploaded is what the 3-way check compares against.
- **Git blob IDs (SHA-1), not a faster hash.** Clean tracked files reuse the index's IDs and are never read.
- **The watchdog is shell plus systemd,** installed by cloud-init before anything else runs. It exists before spotexec ever connects, and needs nothing from it.
- **Hardlink hydration is opt-in.** Shared inodes let one slot corrupt another's dependencies.
- **Not built yet:** PTY allocation, parsed test-failure summaries.
