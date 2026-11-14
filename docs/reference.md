# Command and file reference
## Command reference

```
sx [flags] <command...>                      run remotely (shorthand for `spotexec run --`)
sx <task> [args...]                          named task; parallel tasks fan out across slots
spotexec run [flags] -- <cmd>                sync, run, write back
sx --same-slot [flags] <cmd>                 run in this worktree's last slot (same VM)
spotexec exec [flags] -- <cmd>               same as sx --same-slot --sync none
spotexec tasks                               list tasks from .spotexec.toml
spotexec sync [flags]                        pre-sync a slot (and build the container image, install deps)
spotexec sync-plan [--json]                  dry-run the sync: files, bytes, largest files, exclusions
spotexec pull <glob>... [--dest DIR]         copy gitignored files from the last slot
spotexec status [runner...] [--json] [-A]    runners, instances, slot holders, idle timers (-A: every machine's VMs)
spotexec monitor [runner...] [--interval 2s] live load and burn-rate dashboard
spotexec up [runner] [--count N]             pre-warm fleet VMs (waits until ready)
spotexec down [runner...] [--all]            terminate fleet VMs (and leftover bake builders)
spotexec down -A [--machine NAME]            terminate VMs any machine launched (or one machine's)
spotexec daemon start|stop|run               optional pool manager
spotexec daemon status [--json]              what the pool manager sees and has done
spotexec container init [--path P] [--force] scaffold a Dockerfile and wire it into .spotexec.toml
spotexec container build [run flags]         build the project image on a runner now (ECR on AWS)
spotexec image bake [runner] [--pull IMG]    bake an AMI from the runner's bootstrap
spotexec image list|clear [runner]           list or delete baked AMIs
spotexec setup [host...] [--all]             set up this machine (config, AWS user and key, hooks), then others over SSH
spotexec setup --list | --revoke HOST        every machine's IAM user and VMs; revoke one
spotexec doctor [runner...]                  verify config, SSH, helper, credentials, network, quota
spotexec unlock [runner[/id]] (--slot N | --all)
spotexec prune [runner[/id]...] [--older-than 7d] [--dry-run]
spotexec init [--global] [--force]           generate configs
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
| `~/.local/state/spotexec/` | Pool ledgers, queue tickets, instance records, `known_hosts`, daemon state and log (`SPOTEXEC_STATE_DIR` or `XDG_STATE_HOME` override). |
| `.spotexec.toml`, `.spotexecignore` | Project config and extra sync excludes. |
| `<git-dir>/spotexec/conflicts/` | Remote versions of files that conflicted on write-back (`.spotexec/conflicts/` outside Git). |
| `.spotexec/Dockerfile` | Project image from `spotexec container init`. |
| `~/.cache/spotexec/` | Local SSH ControlMaster sockets (`cm/`) and helper install markers. |
| Runner: `~/.cache/spotexec/` | Helper binary, slots, dependency caches (`work_root`). |
| Runner: `/var/tmp/spotexec/`, `/var/log/spotexec-bootstrap.log` | Lifecycle state and bootstrap log on ephemeral VMs. |
