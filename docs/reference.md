# Command and file reference
## Command reference

```
sx [flags] <command...>                      sync, run remotely, write back
sx [flags] <task> [args...]                  named task; parallel and sharded tasks fan out across slots
sx [flags] -- <command...>                   the command, even if a task has its name
spotexec run [flags] -- <cmd>                the same as sx --: <cmd> is never a task
sx --same-slot [flags] <cmd>                 run in this worktree's last slot (same VM)
spotexec exec [flags] -- <cmd>               same as sx --same-slot --sync none -- <cmd>
spotexec tasks                               list tasks from .spotexec.toml
spotexec sync [flags]                        pre-sync a slot (and build the container image, install deps)
spotexec sync-plan [--json]                  dry-run the sync: files, bytes, largest files, exclusions, deps key files
spotexec pull <glob>... [--dest DIR] [--json] copy gitignored files from the last slot
spotexec pull --held [<glob>...]             copy the new files a write-back held in the last slot
spotexec backups [--json]                    list kept write-back backups and conflict copies, newest first
spotexec backups clear ([<run-id>...] [--older-than AGE] | --all) [--dry-run]  delete them
spotexec status [runner...] [--json] [-A]    runners, instances, slot holders, idle timers (-A: every machine's VMs and builders, Spot quota use; with --json, {"instances", "searched", "failed", "spot_quotas"})
spotexec monitor [runner...] [--interval 2s] live load and burn-rate dashboard
spotexec up [runner] [--count N]             pre-warm fleet VMs (waits until ready)
spotexec down [runner...] [--all]            terminate fleet VMs (and leftover bake builders)
spotexec down -A [--machine NAME]            terminate VMs any machine launched (or one machine's)
spotexec daemon start|stop|run               optional pool manager
spotexec daemon status [--json]              what the pool manager sees and has done
spotexec container init [--path P] [--force] [--yes] scaffold a Dockerfile and wire it into .spotexec.toml
spotexec container build [run flags]         build the project image on a runner now (ECR on AWS)
spotexec image bake [runner] [--pull IMG]    bake an AMI from the runner's bootstrap (--region R for multi-region runners)
spotexec image list [runner]                 baked AMIs from every machine: base image, age, maker, which is in use (every region a runner uses, or --region R)
spotexec image clear [runner] [--all]        delete this machine's and this bootstrap's bakes (--all: every machine's; every region a runner uses, or --region R)
spotexec prices [runner] [--refresh] [--json] current Spot prices of the runner's instance types in every zone of its region(s), cheapest first; marks where the next VM launches
spotexec setup [host...] [--all] [--bake] [--hooks]  set up this machine (config, AWS user and key; the hook only if asked), then others over SSH; offer a bake
spotexec setup --list | --revoke HOST        every machine's IAM user and VMs; revoke one
spotexec teardown [--dry-run] [--region R]   remove everything spotexec created in the AWS account (admin)
spotexec supported [-C DIR] [-q]             print true (exit 0) if sx can run from here, else false (exit 1) and why on stderr
spotexec doctor [runner...]                  verify config, SSH, helper, credentials, network, quota
spotexec unlock [runner[/id]] (--slot N | --all)
spotexec prune [runner[/id]...] [--older-than 7d] [--dry-run]
spotexec init [--global] [--force] [--yes] [--no-container]  .spotexec.toml and the project container (or the runner config)
spotexec hook install [--claude] [--codex] [--shared|--global] [--uninstall]
spotexec hook check '<command>'              show whether the hook offloads a command
spotexec cloud-init [runner]                 print the first-boot user-data
spotexec completion bash|zsh|fish|powershell shell completion script
spotexec completion sx bash|zsh|fish|powershell   the same for sx (flags and task names)
spotexec version
```

## Files and state

| Path | Contents |
| --- | --- |
| `~/.config/spotexec/config.toml` | Global config (`SPOTEXEC_CONFIG`, `SPOTEXEC_CONFIG_DIR` or `XDG_CONFIG_HOME` override). |
| `~/.config/spotexec/keys/` | Generated SSH key for ephemeral runners. |
| `~/.aws/credentials` `[spotexec]`, `~/.aws/config` `[profile spotexec]` | The access key and region written by `spotexec setup`. |
| `~/.config/spotexec/owner-id` | This machine's permanent ID: it tags the instances it launches and its IAM user. A machine's runs only use its own instances. |
| `~/.config/spotexec/machines.toml` | The machines `spotexec setup` has set up from here (for `setup --all`). |
| `~/.local/state/spotexec/` | Pool ledgers, queue tickets, instance records, `known_hosts`, daemon state and log, dependency install logs (`logs/deps/`), the full output of failed `--rtk-filter` runs (`rtk/`) (`SPOTEXEC_STATE_DIR` or `XDG_STATE_HOME` override). |
| `.spotexec.toml`, `.spotexecignore` | Project config and extra sync excludes. |
| `.git/spotexec/conflicts/`, `.git/spotexec/backup/` | Remote versions of files that conflicted on write-back, and local versions write-back replaced or deleted that Git had no copy of (`.spotexec/` outside Git). Linked worktrees use the shared git directory (`git rev-parse --git-common-dir`), so removing a worktree keeps them. Pruned automatically ([policy](running.md#write-back-and-artifacts)); `spotexec backups` lists them. |
| `.spotexec/Dockerfile` | Project image from `spotexec init` or `spotexec container init`. |
| `~/.cache/spotexec/` | Local SSH ControlMaster sockets (`cm/`) and helper install markers. |
| Runner: `~/.cache/spotexec/` | Helper binary, slots, dependency caches (`work_root`). |
| Runner: `/var/tmp/spotexec/`, `/var/log/spotexec-bootstrap.log` | Lifecycle state and bootstrap log on ephemeral VMs. |
