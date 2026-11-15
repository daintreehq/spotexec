# Operations and troubleshooting

```sh
spotexec doctor                 # config, SSH, ControlMaster, helper, credentials, AMI, VPC, quota
spotexec status [--json]        # runners, instances, slot holders, idle timers
spotexec monitor                # live load and burn-rate dashboard
spotexec up aws --count 2       # pre-warm VMs now (respects max_instances)
spotexec down aws | --all       # terminate fleet VMs now (and leftover bake builders)
spotexec unlock aws/i-0abc --slot 3  # kill a wedged run and free its slot (--all for every slot; name the instance when several run)
spotexec prune --older-than 7d  # remove stale dependency caches and slot workspaces on runners
spotexec sync-plan              # dry run: file count, bytes, largest files, exclusions
spotexec cloud-init aws         # print the exact first-boot script
```

| Symptom | Likely cause and fix |
| --- | --- |
| `BOOTSTRAP_FAILED` | Your `bootstrap_script` exited non-zero (or ran past `boot_timeout`). The error shows the log tail. Fix the script, then run `spotexec down <runner>`, which also clears the one-hour launch hold. |
| Jobs queue but no VM launches | Check `max_instances`, `spotexec daemon status` for a paused fleet, and the vCPU quota (`spotexec doctor`). A failed launch backs off for 20s. |
| `vCPU quota … has no room for another VM` | The account's Spot vCPU quota in that region is used up. Runs queue for VMs already running; with none, `sx` exits 125 and prints the `aws service-quotas request-service-quota-increase` command to run as an administrator. See [the quota section](aws.md#5-check-your-spot-vcpu-quota). |
| `InsufficientInstanceCapacity` | Add more `instance_types` (or use `vcpus` to consider every type of that size), or try another region. spotexec already moves on to the next-cheapest zone and type. |
| Sync is slow or refused as too large | Run `spotexec sync-plan`. Exclude build outputs and large fixtures with `sync.exclude` or `.spotexecignore`. |
| `SYNC_CONFLICT` | You edited a file during the run. Compare it with the saved copy under `.git/spotexec/conflicts/<run-id>/`. |
| `WRITEBACK_BACKUP` | The run deleted or replaced files that had no committed copy (untracked files, uncommitted edits). Your versions are saved in the directory the warning names (`<git-dir>/spotexec/backup/<run-id>/`, or `.spotexec/backup/<run-id>/` outside Git): copy back what you need. |
| `WRITEBACK_HELD` | The run deleted the project's own `.spotexec.toml` or `.gitignore`, or most of the synced files with no committed or moved copy, so those deletions weren't applied here. If they were intended, delete the files yourself. With `writeback_over = "hold"`, it can instead mean the run's new files were over the [write-back limit](running.md#write-back-limits) and stayed in the slot: bring them back with `spotexec pull --held` before the next run syncs into it. |
| `SYNC_COLLISION` | The remote command wrote two names that are one file on your filesystem (they differ only in case or Unicode normalisation, which macOS ignores by default). The warning says which version was kept; the other is under `.git/spotexec/conflicts/<run-id>/`. Rename one of them in the project. |
| `WRITEBACK_SYMLINK` | The command left a symlink to an absolute path, above the worktree root, or out through another link, and write-back (or a pull) recreated it locally. Anything reading that path now reads the target. If the command shouldn't make such links, delete them and restore the paths with `git checkout -- <path>`. |
| `WRITEBACK_REFUSED` | The runner reported paths write-back may never write: inside `.git` or `.spotexec`, files that weren't synced, or a `.git` directory the command created below the root (a `git init` or `git clone` into a subdirectory). Such a `.git` directory stays in the slot until the next run that syncs into it removes it. If you need a repository the command made, have it write a bundle or archive instead. |
| `fatal: not a git repository` on the runner | The workspace has no `.git` unless you set `git_seed = true` under `[sync]`. See [Git on the runner](running.md#git-on-the-runner). |
| `GIT_SEED_FAILED` or `GIT_SEED_UNAVAILABLE` | The command ran without a `.git`: the runner has no `git` (install it on the runner itself, for example in `bootstrap_script`: a project container doesn't count), the project isn't synced from a Git worktree, or HEAD couldn't be read. The warning says which. |
| A test needs a local service | Prefix it with `SPOTEXEC_LOCAL=1` (agent hook), or run it with plain `pnpm …` instead of `sx`. |
| `OOM_KILLED` (usually exit 137) | The jobs sharing the VM together reached their memory pool's cap (the VM's memory less a reserve), and the kernel killed the largest process, which was in this command's container. Outside the pool (`--exclusive`, a single slot, or a memory option in `container.options`) it means the container reached its own `--memory` limit, or the whole VM ran out. Give it room with `--exclusive` (the whole VM, no cap), fewer `max_slots` or a larger instance type, or make it use less (fewer workers, a smaller heap). The run isn't replayed: it would run out again. |
| Exit 134 and `JavaScript heap out of memory` or `Ineffective mark-compacts near heap limit` | Node hit its own heap limit, not the machine's. Its default heap depends on the memory it sees: about a quarter of the VM's (4 GiB on a 16 GiB VM), but under 2 GiB inside a container with its own memory limit, such as a `--memory` in `container.options` (spotexec's own pool isn't visible to Node and doesn't shrink it). Raise `--max-old-space-size` in `NODE_OPTIONS`, or drop the container limit. |
| `MEMORY_PRESSURE` (exit 125) | The VM stopped answering while its heartbeats showed it out of memory, typically a job outside a container, or one whose `container.options` lifted the limit. The run isn't replayed on another VM, where it would likely do the same (unless the VM announced its reclamation or shutdown: then it is). Put the job in a project container, or give it more memory as for `OOM_KILLED`. |
| `RUNNER_UNRESPONSIVE` (exit 125) | spotexec lost the VM mid-run while EC2 still reports it running, and the VM gave no sign it was going away (no Spot notice, no shutdown reported by the helper). The run isn't replayed elsewhere, since it may be what overloaded the VM. Rerun it; if it needs a lot of memory or CPU, with `--exclusive`. |
| `RUNNER_SILENT` | The VM stopped answering heartbeats but EC2 says it is running, so spotexec waits up to 3 more minutes before giving up on it. Usually memory: see `MEMORY_PRESSURE`. |
| Expired SSO session | `aws sso login` (or `aws login`). spotexec reports this instead of falling back to other keys. |
