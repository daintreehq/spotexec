# spotexec (`sx`)

Run tests, type checks, linters, formatters and builds from your local git
worktrees on remote hardware: a homelab box over SSH, or an AWS Spot instance
that boots on demand and terminates itself when idle. Files the command
changes (`prettier --write`, `eslint --fix`, `vitest -u`, codegen) are written
back to your worktree with a 3-way safety check, so concurrent local edits are
never clobbered.

Built for running many coding agents in parallel worktrees without melting the
laptop: several worktrees share one big runner through isolated slots and a
shared dependency cache.

```
$ sx pnpm vitest run src/auth.test.ts
◐ aws#3 · synced 4 files (18 KB) in 0.2s
 ✓ src/auth.test.ts (12 tests) 340ms
● 4.1s on aws#3 · pulled 2 modified files
```

## How it works (and why there is no backend)

```
laptop                                  runner (SSH box or Spot VM)
──────                                  ───────────────────────────
sx ── git ls-files manifest ──┐         spotexec-helper (uploaded on first use)
   ── OpenSSH ControlMaster ──┼──SSH──▶  ├ flock'd concurrency tokens + workspace slots
   ── framed RPC (stdio) ─────┘          ├ applies sync, runs command in a process group
                                         ├ snapshots files before/after → mutation list
AWS EC2 API (your credentials) ◀── sx    └ lockfile-keyed dependency cache (reflink/copy)
                                        cloud-init idle watchdog (systemd timer) ── poweroff
```

Managing Spot instances does not need a service running anywhere. The
responsibilities split like this:

| Concern | Who handles it |
| --- | --- |
| Launching a VM | The CLI, on demand, calling EC2 with your own AWS credentials. |
| Knowing which VMs exist | EC2 itself: instances carry `spotexec-runner` and `spotexec-owner` tags. A local record only caches the address. |
| Tearing VMs down when you stop using them | **The VM itself.** The first thing the cloud-init user-data does is install a systemd timer. It powers the VM off after `idle_timeout` without activity, and unconditionally after `max_lifetime`. Instances launch with `InstanceInitiatedShutdownBehavior=terminate`, so power-off means terminate and billing stops. No credentials live on the VM, and closing your laptop lid does not matter. |
| Not shutting down mid-job | The helper holds a shared `flock` on `/var/tmp/spotexec/lifecycle.lock` while it owns a slot and refreshes `/var/tmp/spotexec/activity`. The watchdog needs that lock exclusively before it shuts down. It marks the VM `draining` first, so new sessions are refused and the CLI replaces the VM. |
| Stuck sessions (laptop asleep, network blackhole) | The CLI and helper exchange heartbeats. The helper cancels the command after 45s of silence and releases the slot, and the idle timer resumes. |
| Spot interruptions | The helper polls IMDSv2 for the 2-minute notice, marks the runner draining, and the CLI retries the run once on a fresh instance. |
| Two agents booting the same runner | A local `flock` per runner covers discovery, launch and readiness, and RunInstances uses an idempotent `ClientToken`. |
| A VM that never finishes booting | If first-boot setup never reports ready within `boot_timeout`, the CLI terminates that instance by ID. Its watchdog may never have been armed. |
| A runner shutting down while others use it | A draining VM is never terminated by another client. New runs wait for it to go, then boot a replacement. |

Residual risk is a VM whose kernel hangs before the timer fires. The
`max_lifetime` cap is armed twice (the timer, plus an independent
`systemd-run` one-shot), but neither helps if cloud-init never runs.
`spotexec status` shows anything still up (including stopped instances,
whose disks still bill), and `spotexec down --all` ends it.
If you want an outside backstop too, add an AWS Budgets alert, or an EventBridge
rule that terminates `spotexec-runner`-tagged instances older than N hours.

## Install

```sh
make build            # cross-compiles and embeds the remote helper, writes bin/spotexec and bin/sx
make install          # into $(go env GOPATH)/bin
```

Requirements: `ssh` and `git` locally. Runners only need an SSH server, `bash`,
and `gzip`. There is no rsync and nothing to preinstall: the helper uploads
itself into `~/.cache/spotexec/bin/` on first contact (Linux and macOS, amd64
and arm64).

## Quick start

```sh
spotexec init --global     # example ~/.config/spotexec/config.toml; edit the runners
spotexec init              # .spotexec.toml for this repo (detects lockfiles and tasks)
spotexec doctor            # checks SSH, helper, AWS credentials, AMI, VPC
sx pnpm test               # sync → run remotely → write back changes
sx fmt                     # named task from .spotexec.toml
sx verify                  # parallel task fan-out across slots
```

## Configuration

`~/.config/spotexec/config.toml` holds your hardware and is never committed:

```toml
version = 1
default_pool = ["homelab", "aws"]   # tried by priority; aws boots when homelab is full or unreachable

[runners.homelab]
type = "ssh"
hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]  # dialed in parallel, LAN wins within 500ms
max_slots = 4                                              # default: CPUs / 4
priority = 10

[runners.aws]
type = "aws-spot"
region = "us-east-1"
profile = "dev"                       # optional; SSO / assume-role work via the aws CLI
instance_types = ["c7a.4xlarge", "c6a.4xlarge", "m7a.4xlarge"]  # tried in order on capacity errors
# max_price = "0.40"                  # default: capped at the on-demand price
# on_demand_fallback = true
max_slots = 8
priority = 20
idle_timeout = "20m"
max_lifetime = "12h"
volume_gb = 64
hourly_cost = 0.25                    # for `spotexec monitor`
# ssh_cidr = "203.0.113.4/32"         # default 0.0.0.0/0 (key-only auth); narrow it if your IP is stable
# subnet_id = "subnet-…"; security_group_ids = ["sg-…"]; ami = "ami-…"; arch = "arm64"
bootstrap_script = """                # runs as root on first boot, after the watchdog is armed
apt-get update -o DPkg::Lock::Timeout=300 && apt-get install -y build-essential
"""
user_bootstrap_script = """           # runs as the login user
curl -fsSL https://mise.run | sh
"""
remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'
```

`.spotexec.toml` lives in the repo and is committed. Everything is optional:

```toml
version = 1
timeout = "5m"                 # wall-clock cap per command (exit 124 when exceeded)
env_allow = ["NODE_OPTIONS"]   # local env vars forwarded (nothing else is)

[sync]
mode = "auto"                  # auto: write back changes · in: discard them · none: no sync
writeback = "always"           # always | on-success | never (eslint --fix exits 1 but still fixes)
exclude = [".env*", "*.pem"]   # on top of .gitignore and .spotexecignore
pull_artifacts = ["coverage/"] # gitignored outputs to copy back after every run

[deps]
lockfiles = ["pnpm-lock.yaml"]
install = "pnpm install --frozen-lockfile"
cache_dirs = ["node_modules", "packages/*/node_modules"]
hydrate = "auto"               # reflink, falling back to copy; "hardlink" is opt-in (shares inodes)

[tasks.test]
run = "pnpm vitest run {args}"
[tasks.fmt]
run = "pnpm prettier --write ."
[tasks.verify]
parallel = ["typecheck", "test"]
fail_fast = false
```

Precedence: CLI flags, then `SPOTEXEC_*` env (`SPOTEXEC_TIMEOUT`, `SPOTEXEC_SYNC`,
`SPOTEXEC_RUNNER`, `SPOTEXEC_POOL`, `SPOTEXEC_JSON`, `SPOTEXEC_AGENT`), then
`.spotexec.toml`, then the global config, then defaults.

## AWS setup

Credentials resolve in this order:
1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`, unless
   the runner sets `profile`
2. `aws configure export-credentials`, which handles SSO, assume-role and
   credential_process. When the AWS CLI is installed its answer is final: an
   expired SSO session is reported, never silently replaced by other keys.
3. `~/.aws/credentials`, only when the AWS CLI is not installed

There is no AWS SDK in the binary. Minimum IAM permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": [
        "ec2:RunInstances", "ec2:CreateTags", "ec2:DescribeInstances", "ec2:TerminateInstances",
        "ec2:DescribeImages", "ec2:DescribeVpcs", "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups", "ec2:CreateSecurityGroup", "ec2:AuthorizeSecurityGroupIngress"
      ], "Resource": "*" },
    { "Effect": "Allow", "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:*::parameter/aws/service/canonical/*" },
    { "Effect": "Allow", "Action": "iam:CreateServiceLinkedRole", "Resource": "*",
      "Condition": { "StringEquals": { "iam:AWSServiceName": "spot.amazonaws.com" } } }
  ]
}
```

On first launch spotexec:
- resolves the current Ubuntu 24.04 AMI through the public SSM parameter;
- creates a `spotexec-ssh` security group in the default VPC, one group per
  `ssh_cidr` value, so narrowing the CIDR never leaves an old wide-open rule
  attached;
- always requests a public IPv4 address, and sets IMDSv2 to required with hop
  limit 1;
- generates an SSH key at `~/.config/spotexec/keys/id_ed25519`, which is injected through user-data, so no EC2 key pair is needed.

Each instance gets its own `known_hosts` file. `spotexec cloud-init` prints the
exact user-data script, so you can read it before trusting it.

## Commands

```
sx [flags] <command...>          run remotely (shorthand for `spotexec run --`)
sx <task> [args...]              named task; parallel tasks fan out across slots
spotexec exec -- <cmd>           reuse the last slot without syncing
spotexec sync / sync-plan        pre-sync / dry-run (file count, bytes, largest files)
spotexec pull <glob>...          copy gitignored artifacts from the last slot
spotexec status / monitor        runners, holders, load, idle timers, burn rate
spotexec up / down [--all]       pre-warm or terminate ephemeral runners
spotexec doctor                  verify SSH, helper, config and AWS
spotexec unlock [runner] --slot N|--all     kill a wedged run and free its slot
spotexec prune [--older-than 7d]            remove stale dependency caches and slots
spotexec init [--global]         generate configs
spotexec hook install [--claude] [--codex]  agent command interception
spotexec cloud-init [runner]     print the first-boot script
```

Useful run flags: `--timeout`, `--sync auto|in|none`, `--writeback`,
`--runner`/`--pool`, `--exclusive`, `--pull <glob>`, `--json`/`--pretty`/`-q`,
`--events-file`, `--no-deps`, `--fallback-local`, `-C <dir>`.

Exit codes: the command's own exit code, `124` on timeout, and `125` when
spotexec itself fails (no runner, SSH failure, sync error).

Every job gets these environment variables:
- `SPOTEXEC=1`, `CI=true`
- `SPOTEXEC_SLOT`
- `SPOTEXEC_PORT_BASE` (20000 + slot × 1000; use it to keep test servers from colliding)
- `COMPOSE_PROJECT_NAME=sx_<repo>_sNN`
- `SPOTEXEC_RUNNER`, `SPOTEXEC_RUN_ID`, `SPOTEXEC_WORKSPACE`

Slot numbers are host-wide, so ports never collide even across different repos.

## Output for agents and humans

When stderr is not a TTY (agents), or with `--json`, lifecycle events go to
stderr as one JSON object per line. The command's own stdout and stderr pass
through unchanged:

```json
{"event":"slot_acquired","connect_ms":13,"runner":"aws","slot":3,"waited_ms":0}
{"event":"sync_in","mode":"fingerprint_hit","files":0,"bytes":0,"duration_ms":0}
{"event":"sync_out","mutated_files":["src/index.ts"],"conflicts":[],"duration_ms":31}
{"event":"result","exit_code":0,"duration_ms":4120,"timed_out":false}
```

Remote workspace paths in output are rewritten to local paths, so file links
in stack traces work. Absolute local paths in arguments are mapped to the
remote workspace.

## Agent hooks

`spotexec hook install --claude` adds a Claude Code `PreToolUse` hook to
`.claude/settings.local.json` (`--shared` for `settings.json`, `--global` for
`~/.claude/settings.json`). Heavy Bash commands are rewritten in place to
`spotexec run -- '<cmd>'`:
- **Offloaded:** tests, `tsc`, linters, formatters, builds.
- **Kept local:** `git`, `ls`, `rg`, dev servers, watch modes, pipes, redirects, background jobs.

The rewrite only happens in projects with a `.spotexec.toml`. It does not
auto-approve anything; your normal permission rules apply to the rewritten
command. Use `[hook] when_load_above = 0.6` to offload only when the laptop is
busy, add extra regexes with `[hook] patterns`, and set `mode = "block"` to
deny and instruct instead of rewriting. `spotexec hook check '<cmd>'` shows the
decision for a command line. `--codex` appends an `AGENTS.md` section for
harnesses without hooks.

## Write-back semantics

The local manifest records the Git blob ID of every synced file (the baseline).
After the command runs, the helper compares content IDs, not timestamps, and
reports changed, created and deleted files. For each path:

| Local now | Outcome |
| --- | --- |
| Equals the remote result | Nothing to do. |
| Equals the baseline | The remote version is applied atomically: temp file, then rename. |
| Anything else | Someone edited it during the run. Your file is kept and the remote version is saved to `<git-dir>/spotexec/conflicts/<run-id>/<path>`, with a `SYNC_CONFLICT` warning. |

New files only come back if Git would not ignore them, so `dist/`, `target/`
and `node_modules` never travel back. Use `--pull` or `pull_artifacts` for
ignored outputs you want.

## Differences from the original spec

- **No rsync.** Sync streams a gzip'd tar of exactly the files the helper is
  missing, over the same RPC channel. macOS ships openrsync (protocol 29),
  which has `-z` and flag incompatibilities with GNU rsync 3.x. Sending the
  bytes through spotexec also makes the write-back baseline exact: whatever
  was uploaded is what the 3-way check compares against.
- **SHA-1 Git blob IDs instead of BLAKE3** (clean tracked files reuse the
  index IDs, so they are never read), and gzip instead of zstd.
- **The idle watchdog is a shell script plus a systemd timer installed by
  cloud-init**, not a helper daemon. It exists before spotexec ever connects.
- **Providers are `ssh` and `aws-spot` for now.** The provider interface is
  ready for more.
- **Hardlink dependency hydration is opt-in**, because shared inodes let one
  slot corrupt another's `node_modules`.
- **Not built:** `git_seed`, PTY allocation, and parsed test-failure
  summaries.

## Known limits

- **Process cleanup is not cgroup-based.** Commands run in their own process
  group. After each run, processes that escaped it (for example via `setsid`)
  are killed if their working directory is inside the slot. One that also
  leaves the workspace directory can survive.
- **Write-back cannot be atomic against editors that ignore locks.** Each file
  is re-checked right after its new content is staged and immediately before
  the rename. A save landing in that last syscall window can still be
  replaced.
- **Case-only directory renames keep the old spelling locally.** On a
  case-insensitive filesystem (the macOS default), a remote `mv src Src`
  updates the files but leaves the directory named `src`. Case-only file
  renames are applied correctly.
- **Each machine gets its own instances.** Instances are tagged with this
  machine's owner ID (`~/.config/spotexec/owner-id`), and a machine only ever
  discovers or terminates instances carrying its own ID. Two laptops with the
  same runner name each get their own VM. To share one, copy `owner-id`; the
  two machines then race only through EC2's eventually consistent tag
  filters.

## Development

```sh
make test     # unit tests
make e2e      # builds, starts an Ubuntu SSH container, runs scripts/e2e.sh
```
