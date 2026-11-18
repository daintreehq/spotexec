# Configuration reference

There are two files, and the split is deliberate.

| File | Holds | Committed? |
| --- | --- | --- |
| `~/.config/spotexec/config.toml` | Your hardware, accounts, credentials, network | Never |
| `.spotexec.toml` at the repository root | How this project syncs, installs and runs | Yes |

Both start with `version = 1`, the only config format this spotexec understands. A file without `version` is read as version 1. Any other version (say, one written for a newer spotexec) is rejected rather than read under today's rules, with an error naming the supported version: `sx` and `spotexec run` exit `125`, `spotexec supported` prints `false`, `spotexec doctor` fails, and `spotexec container init` leaves the file alone.

Settings resolve in this order, highest first:
1. CLI flags. A flag given on the command line beats its environment variable whatever its value, so `--json=false`, `--priority 0` and `--pool` all override the environment.
2. `SPOTEXEC_*` environment variables (below).
3. `.spotexec.toml`.
4. The global config.
5. Built-in defaults.

Exceptions to that order:
- `-r` beats `--pool`, and `SPOTEXEC_RUNNER` beats `SPOTEXEC_POOL` and any configured pool (but not `--pool`);
- for the output format, `-q` beats `--json`, which beats `--pretty`, and `-q`, `--json`, `--pretty` or `--json=false` beats `SPOTEXEC_JSON=1` and `SPOTEXEC_AGENT=1`;
- a task's own `pool` beats `SPOTEXEC_POOL`;
- `spotexec sync` always syncs and `spotexec exec` never does, whatever `SPOTEXEC_SYNC` says.

Unknown `--sync`, `--writeback` and `--writeback-over` values and a zero or negative `--timeout` (from flags or the environment), and a `SPOTEXEC_TIMEOUT` or `SPOTEXEC_PRIORITY` that doesn't parse (when its flag isn't given), are rejected with exit `125` before any runner is used: a malformed variable never falls back to the default. So is a boolean variable (`SPOTEXEC_SAME_SLOT`, `SPOTEXEC_FAIL_ON_CONFLICT`, `SPOTEXEC_JSON`) set to anything but `1`, `true` or `yes` (on) or `0`, `false` or `no` (off), in any case; unset or empty is off.

### Environment variables

| Variable | Same as |
| --- | --- |
| `SPOTEXEC_TIMEOUT` | `--timeout`, for example `SPOTEXEC_TIMEOUT=15m`. |
| `SPOTEXEC_SYNC` | `--sync`. |
| `SPOTEXEC_WRITEBACK_OVER` | `--writeback-over`. |
| `SPOTEXEC_RUNNER` | `-r`, `--runner`. |
| `SPOTEXEC_POOL` | `--pool` (comma-separated runner names). |
| `SPOTEXEC_PRIORITY` | `--priority`. |
| `SPOTEXEC_SAME_SLOT=1` | `--same-slot`: every `sx` in that shell returns to this worktree's last slot. |
| `SPOTEXEC_FAIL_ON_CONFLICT=1` | `--fail-on-conflict`: a run whose write-back conflicted exits `122` instead of `0`. |
| `SPOTEXEC_JSON=1` | `--json`. |
| `SPOTEXEC_AGENT=1` | NDJSON events by default, for agent harnesses. |
| `SPOTEXEC_KEEP_AWAKE=0` | On macOS, lets the Mac idle-sleep during a run (`0`, `false` or `no`, in any case; any other value leaves it on). By default spotexec holds an idle-sleep assertion (`caffeinate -i`) while a run, pull or bake is under way, since a runner ends a run whose client goes silent for 45 seconds. A closed lid still sleeps. |
| `SPOTEXEC_CONFIG`, `SPOTEXEC_CONFIG_DIR` | The global config file, or its directory (default `~/.config/spotexec`, or under `XDG_CONFIG_HOME`). |
| `SPOTEXEC_STATE_DIR` | Local state (default `~/.local/state/spotexec`, or under `XDG_STATE_HOME`). |

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
instance_types = ["c7a.2xlarge", "m7a.2xlarge", "c6a.2xlarge"]
max_slots = 2                     # shared jobs per VM; an exclusive task takes both
max_instances = 4                 # a ceiling: past it, runs queue for a free slot
min_instances = 0                 # kept warm only while `spotexec daemon` runs
priority = 20
# scheduling = { policy = "consolidate", spare_slots = 0 }   # see docs/POOL.md
idle_timeout = "10m"
max_lifetime = "12h"
volume_gb = 64
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
| `max_slots` | vCPUs ÷ 4, between 1 and 44 | Commands running at once per machine (1–44). A machine may hold up to twice as many jobs (at most 44), the others syncing, preparing their image and dependencies, or writing back while those commands run; a job that is ready waits there for a free slot. Each job gets its own range of 1000 ports. A machine running jobs from configs that disagree uses the count of the first job that found it idle until it is idle again ([CPUs](environments.md)). |
| `priority` | `0` | Lower runs first. Later runners in the pool only take overflow. |
| `work_root` | `~/.cache/spotexec` | Where slots, caches and the helper live on the runner. |
| `remote_path_setup` | (none) | Shell run before each command, after the login profile (for example `PATH` for mise or nvm). Not used inside a [project container](environments.md#project-containers). |
| `ssh_options` | (none) | Extra `-o` options for `ssh`, for example `["Port=2222", "ProxyJump=bastion"]`. They override spotexec's defaults (`ConnectTimeout=10`, `ServerAliveInterval=10`, `ServerAliveCountMax=3`, `LogLevel=ERROR`), and runners whose options differ never share a connection. |
| `hourly_cost` | (none) | Price per machine-hour, for the burn estimates in `spotexec status` and `spotexec monitor`. `aws-spot` VMs are priced at the current Spot price of their own instance type in their zone, so for them this is only the fallback when that price is unknown (an on-demand VM, or prices that can't be read). A non-negative number. |

### `ssh` runners

| Key | Meaning |
| --- | --- |
| `host` / `hosts` | Routes to the same machine: `user@host`, `host`, or a `~/.ssh/config` alias. They are dialed in parallel, and an earlier entry wins if it connects within 500ms of the first answer. |
| `user` | Login user when the host omits `user@`. |
| `identity_file` | SSH private key (default: your ssh config). Fleets always use the generated key. |

The remote user's login shell can be anything (bash, zsh, fish, tcsh, nushell): spotexec runs its own commands under `/bin/sh`, and your commands under bash (or `sh` without it).

### `aws-spot` runners

| Key | Default | Meaning |
| --- | --- | --- |
| `region` | `AWS_REGION`, `AWS_DEFAULT_REGION`, else `us-east-1` | EC2 region. |
| `regions` | (unset) | Launch in several regions instead: a list, or `["all"]` for every region enabled in the account. Each new VM goes where the run is expected to cost least: the Spot price, and the waiting a cold or far region adds (see `launch_order`). Not with `region`, `subnet_id`, `security_group_ids`, an `ami-…` ID or a named `cache_bucket`. A list of one region is the same as `region`. See [Let spotexec pick the region](aws.md#let-spotexec-pick-the-region). |
| `exclude_regions` | (none) | With `regions = ["all"]`: regions never to launch in, such as `["ap-south-2"]` (frequent interruptions, too far away, data residency). `spotexec prices` and `doctor` list them as not used. |
| `profile` | (env / default) | AWS profile to use. |
| `instance_types` | `c7a.2xlarge`, `m7a.2xlarge`, `c6a.2xlarge` (8 vCPUs, as in the starter config), none with `vcpus` | The types VMs may use. Each VM goes to the first option in launch order among them and every zone (with `regions`, every region), moving down the ranking on capacity errors: in one region the cheapest current Spot price, equal prices to the type listed first; with `regions`, the lowest expected run cost (see `launch_order`). All must share one architecture. With `vcpus`, entries may be patterns (`"m*"`, `"c7a.*"`) and only narrow the matching types. See [Launch on the cheapest instance](aws.md#launch-on-the-cheapest-instance). |
| `vcpus` | (unset) | Instead of naming types: every current instance type in the region with exactly this many vCPUs (and `min_memory_gib`, `arch`) is a candidate, and the one with the lowest expected run cost launches (the Spot price, counting newer generations as faster). Exact, so every VM gets the same slots. See [Describe the VM instead of naming types](aws.md#describe-the-vm-instead-of-naming-types). |
| `min_memory_gib` | `0` | With `vcpus`: the least memory a type may have. |
| `min_generation` | `6` | With `vcpus`: the oldest instance generation used (m6a: 6). `7` keeps to the newest. |
| `exclude_instance_types` | (none) | With `vcpus`: types or patterns never to use, such as `["m6i.*", "*n.*"]`. |
| `launch_order` | `run_cost` | How launches rank the options. `run_cost`: by the expected cost of an hour of work, the Spot price plus the waiting (at `wait_cost`) that a slower generation (`vcpus` runners), a cold region or a farther region (several regions) adds; a runner in one region with a fixed list ranks by price, as with `cheapest`. `cheapest`: the Spot price alone. `listed`: `instance_types` in the listed order, each in its cheapest zone first. See [Rank by the cost of the run](aws.md#rank-by-the-cost-of-the-run). |
| `wait_cost` | `1` | With `launch_order = "run_cost"`: what an hour of waiting for results is worth, in USD, against the Spot bill. `0` counts only the bill; higher values favor warm regions and faster types more. A number or a string (`"1.50"`). |
| `arch` | from the first instance type that isn't a pattern, else `amd64` | `amd64` or `arm64` (Graviton). |
| `max_instances` | none | Fleet ceiling, not a target: past it, runs queue for a free slot instead of launching more VMs. Unset (or `0`), the fleet grows with demand, limited only by the account's Spot vCPU quota. Every VM pays a cold start and then its `idle_timeout`, so to bound the bill, set it to the most VMs you're willing to pay for at once. See [A few warm VMs](scaling.md#a-few-warm-vms). |
| `min_instances` | `0` | Warm floor, kept only while the [pool manager](scaling.md#pool-manager-optional) runs. |
| `scheduling.policy` | `consolidate` | How jobs are placed on running VMs. `consolidate`: inside a retained set sized to the last minute's demand; surplus VMs go standby and idle out. `shadow`: as `legacy`, reporting what `consolidate` would do (`placement_shadow` events). `legacy`: the oldest VM with a free slot, or the worktree's last VM within 10 minutes. See [Placement](POOL.md#placement-the-retained-set). |
| `scheduling.mode` | `balanced` | `latency` lets a worktree return to its own standby VM before a retained VM without its files: fewer cold syncs, more VMs kept. |
| `scheduling.spare_slots` | `0` | Slots kept retained on top of recent demand, for bursts. |
| `scheduling.early_drain` | `true` | End a standby (surplus) VM once it has been idle for `result_retention`, instead of at `idle_timeout`; retained VMs keep `idle_timeout`. Needs no pool manager. The cost: `exec`, `pull` and `--same-slot` lose a drained VM's workspaces once it ends, and if demand comes back while the retained VMs are full, a new VM boots rather than the drained one taking work again. `false` keeps surplus VMs until `idle_timeout`. See [Early drain](POOL.md#early-drain). |
| `scheduling.result_retention` | `2m` | With `early_drain`: how long after its last job a standby VM stays up so `exec`, `pull` and `--same-slot` can still reach its workspaces. Never longer than `idle_timeout`. |
| `idle_timeout` | `10m` | The VM powers off after this long with no work, and bills until then. |
| `max_lifetime` | `12h` | Hard cap, regardless of activity. Must exceed `boot_timeout`. New runs stop going to a VM 15 minutes before it (a quarter of a shorter lifetime). |
| `boot_timeout` | `15m` | Longest wait for first-boot setup; a VM that takes longer is terminated. |
| `bootstrap_script` | (none) | Runs as root on first boot, after the watchdog is armed. Bounded by `boot_timeout`. |
| `user_bootstrap_script` | (none) | Then runs as the login user, in a login shell. |
| `max_price` | the on-demand price | Spot bid cap in USD per hour: a positive number or string (`0.40`, `"0.40"`, `"$0.40"`), at most 10000. Options priced above it are skipped, and so is their on-demand fallback. |
| `on_demand_fallback` | `false` | Launch on-demand when no Spot capacity is available, trying the types in their Spot-price order. With `max_price`, only the types whose Spot price is at or under it. |
| `volume_gb` | `64` | Root gp3 volume, from 8 to 16384. |
| `work_volume_gb` | `64`; none with an `ami-…` ID | A second gp3 volume, formatted XFS on first boot and mounted at `work_root`, which holds the slots and dependency caches. On XFS a cached dependency set reaches a slot by reflink: the slot shares the cache's data until it writes to a file, so restoring it takes seconds and adds next to nothing to disk use. On the ext4 root it is a full copy. `0` keeps the work root on the root volume. From 8 to 16384, billed like the root volume, and separate from `volume_gb`: the root keeps the OS, Docker and images, so a runner that raised `volume_gb` for its workspaces should raise this instead. No volume is attached when the image maps another EBS volume, when `work_root` has characters other than letters, digits and `_ . / ~ + -`, or when its setup would push user-data past 16 KiB. If the VM can't set it up (no `mkfs.xfs`, say), the work root stays on the root volume, the volume is still billed, and `/var/log/spotexec-bootstrap.log` says why. |
| `ami` | `auto` | `auto` uses the newest matching bake, else Ubuntu 24.04. `base` ignores bakes. An `ami-…` ID pins one image. |
| `user` | `ubuntu` | Login user of the AMI. |
| `subnet_id`, `security_group_ids` | default VPC, a managed `spotexec-ssh` group | Network placement. |
| `ssh_cidr` | `0.0.0.0/0` | Source range for the managed group's SSH rule. sshd on the VMs takes key logins as `user` only; narrow it if your IP is stable. See [SSH exposure](aws.md#ssh-exposure). |
| `cache_bucket` | `auto` | S3 [dependency cache](environments.md#dependency-cache): `auto`, a bucket name, or `off`. |

## Project config: `.spotexec.toml`

Every key is optional. `spotexec init` writes a starting point for npm, pnpm, yarn, bun, Go, Rust and Python projects. Durations (`timeout`, `grace_period`, `queue_timeout`, and the `timeout` of `[deps]`, `[setup]`, `[container]` and each task) must be positive: zero or a negative value is an error when the file loads.

```toml
version = 1
timeout = "5m"                 # wall-clock cap per command (exit 124 when exceeded)
grace_period = "5s"            # SIGTERM → SIGKILL delay on timeout or Ctrl-C
queue_timeout = "10m"          # how long to wait for a free slot; size it for the longest task a run may queue behind
env_allow = ["NODE_OPTIONS"]   # local env vars forwarded (nothing else is)
env = { TZ = "UTC" }           # set on the runner for every command
pool = ["aws"]                 # overrides default_pool for this repo

[sync]
mode = "auto"                  # auto: write back · in: discard changes · none: don't sync
writeback = "always"           # always | on-success | never (eslint --fix exits 1 but still fixes)
exclude = [".env*", "*.pem"]   # on top of .gitignore and .spotexecignore
pull_artifacts = ["coverage/"] # gitignored outputs copied back after every run
writeback_max_files = 1000     # a write-back bringing back more files than this warns (0: no limit)
writeback_max_bytes = "100MB"  # ...or more bytes than this (a number of bytes, or KB/MB/GB; 0: no limit)
writeback_over = "warn"        # warn: bring them back anyway · hold: leave the new files in the slot
# source = "git"               # git | directory (default: git when in a worktree)
# git_seed = true              # a one-commit .git on the runner: HEAD, branch, git status (see below)

[deps]
lockfiles = ["pnpm-lock.yaml", "packages/*/package.json"]   # the cache key; globs allowed
install = "pnpm install --frozen-lockfile"
cache_dirs = ["node_modules", "packages/*/node_modules"]
hydrate = "auto"               # auto (reflink, else copy) | reflink | copy | hardlink
# timeout = "20m"

[setup]                        # optional: runs before every command, after [deps]
run = "pnpm -r run build"      # e.g. build workspace packages the tests import
# timeout = "5m"               # default: the command's own timeout

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
transparent_prefixes = ["direnv exec ."]  # wrappers the hook looks past (they run on the runner too)
```

`git_seed` gives the slot a minimal `.git` before the command runs (off by default), so tools that ask Git about the checkout, such as a build stamping the commit SHA or a test recording `git status`, answer as they do locally. See [Git on the runner](running.md#git-on-the-runner) for what it holds and what it can't do.

### Dependencies

| Key | Meaning |
| --- | --- |
| `lockfiles` | Repo-relative files whose contents key the [dependency cache](environments.md#dependency-cache). Required with `install`. Entries may be globs (see below). |
| `install` | Command that installs dependencies into `cache_dirs` when the cache misses. |
| `cache_dirs` | Directories cached and restored per slot. Globs are single-level (`packages/*/node_modules`). |
| `hydrate` | How a cache is restored into a slot: `auto` (reflink, else copy), `reflink`, `copy` or `hardlink`. |
| `timeout` | Cap on the install command. |

A `lockfiles` entry containing `*`, `?` or `[` is a pattern, matched one path segment at a time: `*` and `?` never cross a `/`, `[a-z]` is a character class, and a segment that is exactly `**` matches any number of directories, including none (`electron/native/*/src/**`, `**/package.json`). Unlike `--pull` and `pull_artifacts`, `**` is recursive here, because lockfile patterns are matched against the list of files spotexec syncs rather than the runner's filesystem. Every pattern must stay inside the repository (no leading `/`, no `..`, nothing in `.git`).

Patterns are expanded once per run, on your machine, against the files synced to the slot: ignored, excluded and `cache_dirs` files never match, nor do symlinks. The key hashes the sorted, deduplicated list of matches plus any literal entries, and that exact list is what the runner reads, so a new workspace or native addon joins the key as soon as it is synced. A pattern that matches nothing is a warning (`DEPS_LOCKFILES`), since an empty directory of addons is legitimate; a literal path that is missing is still an error, as is a list that matches no file at all. When a run syncs and installs dependencies, a literal path missing from your machine fails it before it reaches a runner, and `spotexec supported` and `doctor` report it, unless the path is under a symlinked directory, in a `cache_dirs` directory or ignored (an earlier run may have left it in the slot), where the runner may still have it. `spotexec sync-plan` prints the expanded list. Configs without patterns keep their existing cache key.

### Setup

`[setup]` declares a step that runs before every task and ad-hoc command, in the slot (and in the [project container](environments.md#project-containers), when there is one), after the dependency restore. Use it for work that depends on source rather than lockfiles, so `[deps]` can't cache it: building the `dist/` of workspace packages that tests import, or generating code.

| Key | Meaning |
| --- | --- |
| `run` | Shell command, run from the repository root (whatever directory the command runs from) with the command's environment. Required when `[setup]` is present. |
| `timeout` | Cap on the setup alone. Without it, the setup gets the command's own timeout; the command then gets its full timeout after it. |

Its output is held back: a successful setup shows nothing, and a failed one shows (the last 64 KiB of) its output on stderr, then a `SETUP_FAILED` warning. The command doesn't run, and the run exits `1` (`124` when the setup timed out, `137` when it ran out of memory: `OOM_KILLED`), whatever code the setup exited with. Files the setup changes are treated like the command's: ignored outputs such as `dist/` stay in the slot, and anything else comes back under the usual [write-back](running.md#write-back-and-artifacts) rules.

The setup runs on every run: each part of a parallel or sharded task, `spotexec exec`, `--sync none` runs, and runs that [fall back](running.md#run-flags) to your machine (there, in your worktree). It runs as its own process (and, under a container, its own container), so variables it exports don't reach the command; put those in `[env]`. `spotexec sync` skips it. `--no-setup` skips it for one run, and `setup = false` on a task for every run of that task (on a parallel task, for all of its parts).

Ignored outputs from earlier runs stay in a slot, so a setup that builds into `dist/` should leave it consistent with the source on its own (most build tools do), or clear it first.

### Tasks

| Key | Meaning |
| --- | --- |
| `run` | Shell command. `{args}` (as its own word, or `"{args}"`) becomes the arguments after the task name, passed as `"$@"`; without it they are appended. |
| `parallel` | Names of other (non-parallel) tasks to run at once, each in its own slot. A sharded task named here fans out into its shards. Extra arguments are not forwarded. |
| `shards` | Run a `run` task as this many parts at once (1 to 64), each in its own slot, like a parallel task's parts. In the command, `{shard}` becomes the part's number (from 1) and `{shards}` the count; each part also gets `SPOTEXEC_SHARD` and `SPOTEXEC_SHARDS`. Extra arguments go to every shard. Parts beyond the free slots launch VMs, each with its own cold start; see [when sharding pays](scaling.md#when-sharding-pays). |
| `fail_fast` | Stop the remaining parts of a parallel or sharded task when one fails. In a parallel task, its own setting covers every part, shards included. |
| `group` | `true` holds each part's output and prints it as one block, still prefixed, when the part finishes, instead of interleaving every part's lines as they come; `false` keeps the live output. Unset, it follows `--group`, which is on by default for agents (`SPOTEXEC_AGENT=1` with stdout not a terminal). In a parallel task, its own setting covers every part. |
| `timeout`, `sync`, `exclusive` | Per-task overrides of the run flags. |
| `pool`, `priority` | Send this task to a different fleet, or move it ahead in the queue. |
| `setup` | `false` skips the project's [`[setup]`](#setup) for this task (on a parallel task, for each of its parts). |

A sharded task splits one command into parts, for test runners that take a shard option. Each part needs a slot, and parts beyond the free slots launch VMs, up to `max_instances` when set, each paying a cold start and then its idle timeout. For most suites, one `exclusive` run on a warm VM costs less; shard a suite when it is long enough to be worth it (see [when sharding pays](scaling.md#when-sharding-pays)):

```toml
[tasks.test-all]
run = "npx vitest run --maxWorkers=$SPOTEXEC_CPUS --shard={shard}/{shards} {args}"
shards = 8
timeout = "30m"
fail_fast = false

[tasks.ci]
parallel = ["test-all", "lint"]   # 8 shards and lint, 9 parts at once
```

`sx test-all` runs eight parts, prefixed `[test-all 1/8]` to `[test-all 8/8]`, and exits as a parallel task does; `sx test-all --bail=1` passes `--bail=1` to all eight. With `--json`, each part's events carry `"shard"` and `"shards"` beside `"task"`, and the final `result` event has `"shards"` and each part's outcome in `"parts"`. The run ends with a summary of every part, failed ones first, and a last line with the overall result (see [what an agent sees](agents.md#what-an-agent-sees)). Using `{shard}` or `{shards}` without `shards` is a config error, as is `shards` on a parallel task. With `shards = 1` the task is an ordinary run (so `--same-slot` applies), with `{shard}` and `{shards}` both `1`.

`sx <task>` runs any task, even one named like a spotexec subcommand: `sx sync` runs a task called `sync`. Only `spotexec <task>` gives subcommands precedence, so `spotexec sync` still means the subcommand. A task also wins over a program of the same name; `sx -- <name>` and `spotexec run -- <name>` skip the tasks and run the program. `spotexec tasks` lists them.

## Project-defined fleets

A repository can declare its own fleet, built on one of your runners:

```toml
[runners.ci-big]
extends = "aws"                # one of YOUR runners in ~/.config/spotexec/config.toml
extends_type = "aws-spot"      # or, if you have none by that name, your aws-spot runner
instance_types = ["c7a.2xlarge", "m7a.2xlarge"]
max_slots = 2                  # 4 vCPUs per job
max_instances = 3

[tasks.typecheck]
run = "npm run typecheck"
pool = ["ci-big"]              # only this task needs the big fleet
```

Most projects don't need one. An `exclusive` task gets a whole VM from your shared pool, while a project fleet starts its own VMs, with their own cold starts and idle timeouts, even when the pool has room. Keep project fleets for jobs that need a different machine, such as more memory per job.

A project runner builds on one of your global runners. It may only change sizing, scheduling and the VM's own setup: instance types, slots, instance counts, timeouts, price, volume, bootstrap scripts. Its `max_instances`, `idle_timeout`, `max_lifetime` and `max_price` replace your runner's, higher or lower, since the repository knows how big a fleet its work needs; your runner's values apply wherever the project leaves them unset. Instance types that are all patterns (`["m*", "r*"]`) keep your runner's `arch`; a concrete type or `arch` sets it. It picks its base this way:

- `extends = "name"` uses your runner of that name. With neither key, a project runner of the same name as one of yours overrides it.
- `extends_type = "ssh" | "aws-spot" | "sim"` uses your runner of that type when `extends` names none you have (or one of another type). With several of that type, the first one in your `default_pool` wins.

Runner names are personal while `.spotexec.toml` is shared, so a committed fleet should usually set `extends_type`. When the base can't be found on your machine (no such runner, or several candidates and none in `default_pool`), the fleet is unavailable to you, and that's all: commands that don't use it keep working, `spotexec doctor` lists it with the reason, and a command that selects it (through `--runner`, a task's `pool` or the project's `pool`) fails, saying what's missing. It never falls back to another runner, since a fleet sized for a job may be the only place that job fits; choose another runner with `-r` instead. An unavailable fleet that has the same name as one of your runners hides that runner in this repository (only `status -A` and `down -A` still reach it), so your `default_pool` fails here too if it names it. A project-level `pool` makes the fleet the default for every command in the repository, so for a fleet only some tasks need, set `pool` on those tasks.

A project can also size its VMs by requirements. A suite that needs more memory per job than your runner's types have, for example:

```toml
[runners.ci-mem]
extends = "aws"
extends_type = "aws-spot"
vcpus = 8
min_memory_gib = 32            # whichever 8-vCPU type with 32 GiB or more is cheapest now
max_slots = 2
```

Setting `vcpus` (or another requirement key) without `instance_types` drops the inherited `instance_types`, and setting `instance_types` alone drops inherited requirements: the project's way of choosing types replaces yours. Set both to narrow the requirements with patterns.

How you connect and which account and network are used stay in your global config. So do the AMI your AWS VMs boot and whether they may fall back to on-demand prices. A project setting any of these is an error: `type`, `host`, `hosts`, `ssh_options`, `identity_file`, `work_root`, `profile`, `region`, `regions`, `exclude_regions`, `subnet_id`, `security_group_ids`, `ssh_cidr`, `user`, `cache_bucket`, `ami`, `on_demand_fallback`. A cloned repository therefore can't redirect your SSH, pick a different AWS account, open a port, or boot an image without the watchdog that powers idle VMs off. See [Security model](architecture.md#security-model).
