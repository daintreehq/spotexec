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
max_slots = 8                         # concurrent jobs per instance
max_instances = 4                     # fleet ceiling (default 4)
priority = 20
idle_timeout = "10m"                  # default 10m; 15m suits long agent think-gaps
max_lifetime = "12h"
volume_gb = 64
# cache_bucket = "auto"               # shared dependency cache: auto | <bucket name> | off
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

[container]                    # optional: run everything in this image on the runner
image = "node:22-bookworm-slim" # pulled; or: dockerfile = ".spotexec/Dockerfile"
# options = ["--shm-size=1g"]   # extra `docker run` arguments
# user = "runner"               # default; "root" runs as root (files then belong to root)

[tasks.test]
run = "pnpm vitest run --maxWorkers=$SPOTEXEC_CPUS {args}"
# pool = ["aws-big"]           # per-task fleet; priority = 10 queues it ahead
[tasks.fmt]
run = "pnpm prettier --write ."
[tasks.verify]
parallel = ["typecheck", "test"]
fail_fast = false
```

Precedence: CLI flags, then `SPOTEXEC_*` env (`SPOTEXEC_TIMEOUT`, `SPOTEXEC_SYNC`,
`SPOTEXEC_RUNNER`, `SPOTEXEC_POOL`, `SPOTEXEC_PRIORITY`, `SPOTEXEC_JSON`,
`SPOTEXEC_AGENT`), then `.spotexec.toml`, then the global config, then defaults.

### Project-defined fleets

A project can declare its own runners in `.spotexec.toml`. `extends` starts
from one of your personal runners, so the account, region and credentials
stay yours and the project only changes what it cares about:

```toml
pool = ["ci-big"]

[runners.ci-big]
extends = "aws"                # your runner from ~/.config/spotexec/config.toml
instance_types = ["c7a.2xlarge", "m7a.2xlarge"]
max_slots = 2                  # 4 vCPUs per job
max_instances = 3
```

### Scaling, queueing and CPUs

Every `sx` process schedules for itself; there is no daemon:
- **Placement:** a job goes to a free slot on the oldest instance that has
  one, so newer instances drain and idle out first.
- **Scaling out:** when every slot is busy, the job takes a ticket in a fair
  local queue (priority, then arrival) and the fleet launches more instances,
  up to `max_instances`, while waiting demand exceeds the capacity already
  starting.
- **Overflow:** lower-priority fleets only launch for demand the
  higher-priority ones can't cover.
- **Visibility:** waiting jobs report their position (`queue_position`
  events, and `queued #3 of 9` in the spinner).
- **Priority:** use `--priority N` (or `priority` on a task) to jump the
  queue.

Each job gets `SPOTEXEC_CPUS`, its fair share of the instance (vCPUs ÷
`max_slots`, or every vCPU with `--exclusive`), so test runners can size
their worker pools, for example `--maxWorkers=$SPOTEXEC_CPUS`.

### Shared dependency cache

When `[deps]` is configured on an `aws-spot` runner:
- The first VM to install a given dependency set uploads it as one
  compressed archive to a private S3 bucket in your account. The bucket is
  named `spotexec-cache-<account>-<region>`, is created on first use with
  public access blocked, and expires objects after 14 days.
- The archive's key covers the lockfiles, the install command, the
  cache dirs, the container image and the runner toolchain, so native
  modules never cross Node or OS versions.
- Any later VM downloads and unpacks the archive instead of installing:
  `node_modules` restores in under a second.
- The VMs never hold AWS credentials. The CLI hands them presigned URLs that
  can only target a bucket your account owns.

### Baked images (fast cold boot)

`spotexec image bake <runner>` builds an AMI for a runner:
- **On a builder VM:** it runs the runner's `bootstrap_script` and
  `user_bootstrap_script` once on an on-demand VM, installs Docker and buildx,
  and pre-pulls the project's container image if there is one.
- **Sanitising:** it cleans out everything instance-specific (cloud-init
  state, SSH host keys, the watchdog, logs).
- **Imaging:** it stops the VM and creates the image.

From then on, the fleet launches from the newest bake whose bootstrap matches
and skips the bootstrap entirely, so a new VM is ready in roughly the bare
boot time. The per-instance setup (idle watchdog, SSH key, timeouts) still
runs on every VM.

Changing either bootstrap script makes old bakes stop matching, so re-run
`image bake`. Only the two newest bakes per runner are kept; older images and
their snapshots are deleted. `spotexec image list` shows them and
`spotexec image clear` removes them all. `ami = "base"` on a runner ignores
bakes.

### Project containers

`[container]` runs every command, and the dependency install, inside a
Docker image on the runner:
- The workspace is mounted at the same path, so write-back, path rewriting
  and snapshots work unchanged.
- Commands run as the runner user, so files stay correctly owned, with host
  networking, so `SPOTEXEC_PORT_BASE` still works.
- With `image =`, the image is pulled. With `dockerfile =`, it is built on
  the runner and tagged by the content of the Dockerfile and its build
  context.
- For `dockerfile =` on AWS, the built image is pushed to a private ECR
  repository (`spotexec/<repo-id>`, keeping the newest 10), so every other VM
  pulls it instead of rebuilding.
- Docker (with buildx) is installed on the runner automatically if missing,
  and the runner user is added to the `docker` group. To skip that step on
  each new VM, put `apt-get install -y docker.io docker-buildx && usermod -aG
  docker ubuntu` in `bootstrap_script`.
- Docker always runs as the runner user, never through sudo, so Ctrl-C and
  timeouts can always stop a container. Containers are removed before
  write-back snapshots and when a slot is reused.
- The dependency cache is keyed by the image's immutable ID, so a moving tag
  like `node:22` never restores modules built against an older image.
- **Trust:** `.spotexec.toml` is trusted code. `container.options` (for
  example `--privileged -v /:/host`) give a project root on the runner, which
  matters for shared `ssh` runners. Ephemeral VMs belong to you alone.

## AWS setup

Credentials resolve in this order:
1. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`, unless
   the runner sets `profile`
2. `aws configure export-credentials` (AWS CLI v2), which handles SSO,
   `aws login`, assume-role and credential_process. When the AWS CLI is
   installed its answer is final: an expired session is reported, never
   silently replaced by other keys.
3. Without the AWS CLI, spotexec reads `~/.aws` itself:
   - static keys;
   - `credential_process`;
   - IAM Identity Center (SSO) profiles, using the token cached by
     `aws sso login` and renewing it with its refresh token, so you only log
     in again when the session itself expires;
   - unexpired `aws login` sessions.

The AWS CLI is optional: requests are signed by spotexec itself (there is no
AWS SDK in the binary). Install the AWS CLI if you use SSO, `aws login` or
assume-role profiles. Plain access keys work without it.

Minimum IAM permissions. Three statements are optional: the S3 statement is
only for the dependency cache, ECR only for Dockerfile-built containers, and
the last one only for `spotexec image bake`.

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
      "Condition": { "StringEquals": { "iam:AWSServiceName": "spot.amazonaws.com" } } },
    { "Effect": "Allow", "Action": [
        "s3:CreateBucket", "s3:PutLifecycleConfiguration", "s3:ListBucket",
        "s3:GetObject", "s3:PutObject"
      ], "Resource": ["arn:aws:s3:::spotexec-cache-*", "arn:aws:s3:::spotexec-cache-*/*"] },
    { "Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*" },
    { "Effect": "Allow", "Action": [
        "ecr:DescribeRepositories", "ecr:CreateRepository", "ecr:TagResource", "ecr:PutLifecyclePolicy",
        "ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
        "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"
      ], "Resource": "arn:aws:ecr:*:*:repository/spotexec/*" },
    { "Effect": "Allow", "Action": "servicequotas:GetServiceQuota", "Resource": "*" },
    { "Effect": "Allow", "Action": [
        "ec2:StopInstances", "ec2:CreateImage", "ec2:DeregisterImage", "ec2:DeleteSnapshot"
      ], "Resource": "*" }
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
spotexec up [runner] [--count N]  pre-warm fleet instances (respects max_instances)
spotexec down [runner|--all]     terminate fleet instances (and leftover bake builders)
spotexec image bake|list|clear   baked runner AMIs for fast cold boot
spotexec doctor                  verify SSH, helper, config and AWS
spotexec unlock [runner] --slot N|--all     kill a wedged run and free its slot
spotexec prune [--older-than 7d]            remove stale dependency caches and slots
spotexec init [--global]         generate configs
spotexec hook install [--claude] [--codex]  agent command interception
spotexec cloud-init [runner]     print the first-boot script
```

Useful run flags:
- `--timeout`, `--sync auto|in|none`, `--writeback`
- `--runner`/`--pool`, `--exclusive`, `--priority N`, `--no-boot`
- `--pull <glob>`, `--no-deps`, `--fallback-local`
- `--json`/`--pretty`/`-q`, `--events-file`
- `-C <dir>`

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
- **Native SSO refresh and the AWS CLI share a cache file.** Without the AWS
  CLI installed, spotexec refreshes IAM Identity Center tokens itself, under
  its own lock. If an AWS CLI elsewhere refreshes the same `~/.aws/sso/cache`
  file at the same moment, for example a dev container sharing `~/.aws`, one
  side may need `aws sso login` again.
- **Exclusive jobs across mismatched fleets.** Demand is counted per fleet,
  so an `--exclusive` job that could land on two fleets with different
  `max_slots` may briefly over-launch by one instance.
- **Scaling has no crystal ball.** An instance launched for demand that clears
  before it boots sits idle until `idle_timeout`, then terminates itself.
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
