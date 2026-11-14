# Configuration reference

There are two files, and the split is deliberate.

| File | Holds | Committed? |
| --- | --- | --- |
| `~/.config/spotexec/config.toml` | Your hardware, accounts, credentials, network | Never |
| `.spotexec.toml` at the repository root | How this project syncs, installs and runs | Yes |

Settings resolve in this order, highest first:
1. CLI flags. A flag given on the command line beats its environment variable whatever its value, so `--json=false`, `--priority 0` and `--pool` all override the environment.
2. `SPOTEXEC_*` environment variables: `SPOTEXEC_TIMEOUT`, `SPOTEXEC_SYNC`, `SPOTEXEC_RUNNER`, `SPOTEXEC_POOL`, `SPOTEXEC_PRIORITY`, `SPOTEXEC_SAME_SLOT=1`, `SPOTEXEC_JSON=1`, `SPOTEXEC_AGENT=1`.
3. `.spotexec.toml`.
4. The global config.
5. Built-in defaults.

Exceptions to that order:
- `-r` beats `--pool`, and `SPOTEXEC_RUNNER` beats `SPOTEXEC_POOL` and any configured pool (but not `--pool`);
- `--json` beats `--pretty`;
- a task's own `pool` beats `SPOTEXEC_POOL`;
- `spotexec sync` always syncs and `spotexec exec` never does, whatever `SPOTEXEC_SYNC` says.

Unknown `--sync` and `--writeback` values (from flags or the environment) are rejected with exit `125` before any runner is used.

## Global config: `~/.config/spotexec/config.toml`

```toml
version = 1
default_pool = ["homelab", "aws"]   # runners tried for every command, by priority
# local_fallback = false            # run locally when no runner is reachable (see --fallback-local)

[runners.homelab]
type = "ssh"
hosts = ["me@192.168.1.50", "me@homelab.tailnet.ts.net"]
max_slots = 4
priority = 10

[runners.aws]
type = "aws-spot"
region = "us-east-2"
profile = "dev"
instance_types = ["c7a.4xlarge", "c6a.4xlarge", "m7a.4xlarge"]
max_slots = 8
max_instances = 4
min_instances = 0                 # kept warm only while `spotexec daemon` runs
priority = 20
idle_timeout = "10m"
max_lifetime = "12h"
volume_gb = 64
hourly_cost = 0.25                # used by `spotexec monitor` for burn rate
bootstrap_script = """
apt-get update -o DPkg::Lock::Timeout=300 && apt-get install -y build-essential
"""
user_bootstrap_script = """
curl -fsSL https://mise.run | sh
"""
remote_path_setup = 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"'
```

### Keys for every runner

| Key | Default | Meaning |
| --- | --- | --- |
| `type` | (required) | `ssh` or `aws-spot` (`sim` is the local simulator; see [Development](development.md)). |
| `max_slots` | vCPUs ÷ 4, at least 1 | Concurrent jobs per machine (1–44). |
| `priority` | `0` | Lower runs first. Later runners in the pool only take overflow. |
| `work_root` | `~/.cache/spotexec` | Where slots, caches and the helper live on the runner. |
| `remote_path_setup` | (none) | Shell run before each command, after the login profile (for example `PATH` for mise or nvm). Not used inside a [project container](environments.md#project-containers). |
| `ssh_options` | (none) | Extra `-o` options for `ssh`, for example `["Port=2222", "ProxyJump=bastion"]`. They override spotexec's defaults (`ConnectTimeout=10`, `ServerAliveInterval=10`, `ServerAliveCountMax=3`, `LogLevel=ERROR`), and runners whose options differ never share a connection. |
| `hourly_cost` | (none) | Price per machine-hour of a fleet instance, for `spotexec monitor`. |

### `ssh` runners

| Key | Meaning |
| --- | --- |
| `host` / `hosts` | Routes to the same machine: `user@host`, `host`, or a `~/.ssh/config` alias. They are dialed in parallel, and an earlier entry wins if it connects within 500ms of the first answer. |
| `user` | Login user when the host omits `user@`. |
| `identity_file` | SSH private key (default: your ssh config). Fleets always use the generated key. |

### `aws-spot` runners

| Key | Default | Meaning |
| --- | --- | --- |
| `region` | `AWS_REGION`, `AWS_DEFAULT_REGION`, else `us-east-1` | EC2 region. |
| `profile` | (env / default) | AWS profile to use. |
| `instance_types` | `c7a.4xlarge`, `c6a.4xlarge`, `m7a.4xlarge` | Tried in order on capacity errors. All must share one architecture. |
| `arch` | from the first instance type | `amd64` or `arm64` (Graviton). |
| `max_instances` | `4` | Fleet ceiling. |
| `min_instances` | `0` | Warm floor, kept only while the [pool manager](scaling.md#pool-manager-optional) runs. |
| `idle_timeout` | `10m` | The VM powers off after this long with no work. |
| `max_lifetime` | `12h` | Hard cap, regardless of activity. Must exceed `boot_timeout`. |
| `boot_timeout` | `15m` | Longest wait for first-boot setup; a VM that takes longer is terminated. |
| `bootstrap_script` | (none) | Runs as root on first boot, after the watchdog is armed. Bounded by `boot_timeout`. |
| `user_bootstrap_script` | (none) | Then runs as the login user, in a login shell. |
| `max_price` | the on-demand price | Spot bid cap in USD per hour, as a string (`"0.40"`). |
| `on_demand_fallback` | `false` | Launch on-demand when no Spot capacity is available. |
| `volume_gb` | `64` | Root gp3 volume (at least 8). |
| `ami` | `auto` | `auto` uses the newest matching bake, else Ubuntu 24.04. `base` ignores bakes. An `ami-…` ID pins one image. |
| `user` | `ubuntu` | Login user of the AMI. |
| `subnet_id`, `security_group_ids` | default VPC, a managed `spotexec-ssh` group | Network placement. |
| `ssh_cidr` | `0.0.0.0/0` | Source range for the managed group's SSH rule. Auth is key-only; narrow it if your IP is stable. |
| `cache_bucket` | `auto` | S3 [dependency cache](environments.md#dependency-cache): `auto`, a bucket name, or `off`. |

## Project config: `.spotexec.toml`

Every key is optional. `spotexec init` writes a starting point for npm, pnpm, yarn, bun, Go, Rust and Python projects.

```toml
version = 1
timeout = "5m"                 # wall-clock cap per command (exit 124 when exceeded)
grace_period = "5s"            # SIGTERM → SIGKILL delay on timeout or Ctrl-C
queue_timeout = "10m"          # how long to wait for a free slot
env_allow = ["NODE_OPTIONS"]   # local env vars forwarded (nothing else is)
env = { TZ = "UTC" }           # set on the runner for every command
pool = ["aws"]                 # overrides default_pool for this repo

[sync]
mode = "auto"                  # auto: write back · in: discard changes · none: don't sync
writeback = "always"           # always | on-success | never (eslint --fix exits 1 but still fixes)
exclude = [".env*", "*.pem"]   # on top of .gitignore and .spotexecignore
pull_artifacts = ["coverage/"] # gitignored outputs copied back after every run
# source = "git"               # git | directory (default: git when in a worktree)

[deps]
lockfiles = ["pnpm-lock.yaml"]
install = "pnpm install --frozen-lockfile"
cache_dirs = ["node_modules", "packages/*/node_modules"]
hydrate = "auto"               # auto (reflink, else copy) | reflink | copy | hardlink
# timeout = "20m"

[container]                    # optional; see "Project containers"
dockerfile = ".spotexec/Dockerfile"   # written by `spotexec init` or `spotexec container init`

[tasks.test]
run = "pnpm vitest run --minWorkers=1 --maxWorkers=$SPOTEXEC_CPUS {args}"

[tasks.typecheck]
run = "pnpm tsc --noEmit"

[tasks.verify]
parallel = ["typecheck", "test"]
fail_fast = false

[hook]
when_load_above = 0.6          # agents offload only when the laptop is busy
```

### Tasks

| Key | Meaning |
| --- | --- |
| `run` | Shell command. `{args}` (as its own word, or `"{args}"`) becomes the arguments after the task name, passed as `"$@"`; without it they are appended. |
| `parallel` | Names of other (non-parallel) tasks to run at once, each in its own slot. Extra arguments are not forwarded. |
| `fail_fast` | Stop the remaining parallel tasks when one fails. |
| `timeout`, `sync`, `exclusive` | Per-task overrides of the run flags. |
| `pool`, `priority` | Send this task to a different fleet, or move it ahead in the queue. |

`sx <task>` runs any task, even one named like a spotexec subcommand: `sx sync` runs a task called `sync`. Only `spotexec <task>` gives subcommands precedence, so `spotexec sync` still means the subcommand. `spotexec tasks` lists them.

## Project-defined fleets

A repository can declare its own fleet, built on one of your runners:

```toml
pool = ["ci-big"]

[runners.ci-big]
extends = "aws"                # one of YOUR runners in ~/.config/spotexec/config.toml
instance_types = ["c7a.2xlarge", "m7a.2xlarge"]
max_slots = 2                  # 4 vCPUs per job
max_instances = 3
```

A project runner **must** build on one of your global runners: name it with `extends`, or reuse its name to override it. It may only change sizing, scheduling and the VM's own setup: instance types, slots, instance counts, timeouts, price, volume, bootstrap scripts.

How you connect and which account and network are used stay in your global config. A project setting any of these is an error: `type`, `host`, `hosts`, `ssh_options`, `identity_file`, `work_root`, `profile`, `region`, `subnet_id`, `security_group_ids`, `ssh_cidr`, `user`, `cache_bucket`. A cloned repository therefore can't redirect your SSH, pick a different AWS account, or open a port. See [Security model](architecture.md#security-model).
