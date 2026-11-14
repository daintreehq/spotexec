# spotexec

**Run your tests, type checks, linters and builds on remote hardware, straight from a local git worktree.**
Use a homelab box over SSH, or AWS Spot instances that boot on demand and terminate themselves when idle.
Files the command changes (`prettier --write`, `eslint --fix`, `vitest -u`, codegen) come back to your worktree through a 3-way check, so edits you make during the run are never overwritten.

Prefix any command with `sx` (short for `spotexec run --`):

```console
$ sx pnpm vitest run -u src/auth.test.ts
 ✓ src/auth.test.ts (12 tests) 340ms
● 4.1s on aws#3 · synced 4 files (18 KB) · pulled 2 modified files
```

`spotexec` is built for running many coding agents in parallel worktrees without melting your laptop. Many worktrees share a fleet of runners through isolated slots, a fair queue and a shared dependency cache. Nothing runs in the cloud except the VMs themselves. There is no control plane, no agent on your account and no credentials on the VMs.

- **Drop-in.** `sx <command>` runs any command remotely. Output streams back unchanged, exit codes are preserved, and remote paths in stack traces are rewritten to local ones.
- **Elastic.** AWS Spot fleets scale from zero to `max_instances` as demand queues up, pack jobs onto the oldest VMs, and scale back to zero on their own.
- **Safe.** Write-back compares content IDs against the exact bytes that were synced. A file you edited during the run is never overwritten: you keep it, and the runner's version is saved aside.
- **Fast.** Only missing files are sent. Dependencies restore from a lockfile-keyed cache (local reflink or copy, plus S3 across VMs), and baked AMIs skip first-boot setup.
- **Spot-aware.** Interruption notices are handled. A run that loses its VM is replayed once on a replacement, and an optional pool manager keeps warm capacity and replaces failing VMs.
- **Agent-friendly.** It emits NDJSON lifecycle events when it isn't on a TTY, and a Claude Code hook can move heavy Bash commands to the fleet transparently.

---

## Install

**Homebrew** (macOS and Linux):

```sh
brew install daintreehq/tap/spotexec     # installs spotexec, the sx alias and shell completions
spotexec version
```

**Debian / Ubuntu, Fedora / RHEL, Alpine:** download the `.deb`, `.rpm` or `.apk` for your architecture from the [latest release](https://github.com/daintreehq/spotexec/releases/latest), then:

```sh
sudo apt install ./spotexec_*_linux_amd64.deb   # or: sudo dnf install ./spotexec_*.rpm · sudo apk add --allow-untrusted ./spotexec_*.apk
```

**Any other system:** the release also has plain `.tar.gz` archives for macOS and Linux on amd64 and arm64. Each one holds a single `spotexec` binary; put it on your `PATH` and link `sx` to it.

**From source** (Go 1.26+):

```sh
git clone https://github.com/daintreehq/spotexec.git && cd spotexec
mkdir -p "$(go env GOPATH)/bin"   # make sure it exists and is on your PATH
make install        # builds bin/spotexec and the bin/sx symlink, installs both into $(go env GOPATH)/bin
```

`go install` isn't supported: the build cross-compiles the remote helper for Linux and macOS (amd64 and arm64) and embeds it in the `spotexec` binary, which `make build` and the release builds do. Nothing needs to be preinstalled on a runner: on first contact the helper uploads itself to `~/.cache/spotexec/bin/` there.

Your machine needs **`git`** and **OpenSSH** (`ssh`, used with ControlMaster) on `PATH`.

A runner needs:
- an SSH server, `bash` and `gzip`;
- Linux or macOS, on amd64 or arm64;
- Docker only if you use [project containers](docs/environments.md#project-containers). On AWS, Docker is installed for you.

The AWS CLI v2 is **optional**: `spotexec` signs AWS requests itself (there is no AWS SDK in the binary). Install the CLI if you sign in with SSO, `aws login`, assume-role or `credential_process`. `spotexec setup` uses it to create spotexec's own key; see [Setting up machines](docs/setup.md).

`sx` is a symlink to `spotexec`. `sx <command>` is shorthand for `spotexec run -- <command>`.

## Quick start

**1. Set up your machines** (once per machine):

```sh
aws login                  # or aws sso login / aws configure, as someone who can manage IAM users
spotexec setup             # writes the runner config and gives this machine its own AWS key
```

`spotexec setup` writes `~/.config/spotexec/config.toml` with an `aws` Spot fleet, creates an IAM user and key for this machine that only spotexec can use, and checks your account: region, network and Spot quota. Your own AWS session can then expire without spotexec noticing. It then offers to do the same for the other machines it finds on your network, or names you give it:

```sh
spotexec setup studio-01.local studio-02.local   # install, copy the config and create a key on each, over SSH
```

Have a Linux box or a spare Mac instead of (or as well as) AWS? Add it as an [SSH runner](docs/setup.md#choosing-runners).

**2. Describe each project** (once per repository):

```sh
cd ~/code/my-app
spotexec init               # writes .spotexec.toml: lockfiles, package manager, common tasks
spotexec container init     # optional: a Dockerfile with this project's toolchain
spotexec doctor             # checks config, SSH, the helper, AWS credentials, AMI, VPC, quota
```

**3. Run things:**

```sh
sx pnpm test                     # sync → run remotely → write back changes
sx pnpm prettier --write .       # formatted files land back in your worktree
sx test --reporter=dot           # a named task from .spotexec.toml, with extra args
sx verify                        # a parallel task: typecheck and test fan out across slots
spotexec status                  # what's running where, and idle timers
```

The first AWS run boots a VM, which takes about a minute plus your bootstrap. Later runs reuse it until it has been idle for `idle_timeout`. [Bake an image](docs/environments.md#baked-images) to make cold boots close to bare boot time.

## Documentation

| Guide | What's in it |
| --- | --- |
| [Setting up machines](docs/setup.md) | `spotexec setup`, choosing runners, setting up more machines over SSH, keys, rotation and revoking a machine. |
| [AWS setup](docs/aws.md) | Every AWS step by hand: sign-in methods, the IAM policy, regions, Spot quota, networking, what gets created, cost guardrails, the life of a Spot VM. |
| [Running commands](docs/running.md) | Run flags, the environment on the runner, exit codes, write-back and artifacts. |
| [Coding agents](docs/agents.md) | NDJSON events and the Claude Code hook that moves heavy commands to the fleet. |
| [Toolchains, containers and caches](docs/environments.md) | Project containers (`spotexec container`), the dependency cache, baked images. |
| [Scaling and the pool manager](docs/scaling.md) | Slots, queueing, CPU sizing, warm pools. The [pool design](docs/POOL.md) has the scheduling details. |
| [Configuration reference](docs/configuration.md) | Every key in `config.toml` and `.spotexec.toml`. |
| [Operations and troubleshooting](docs/troubleshooting.md) | Error codes and what to do about them. |
| [Command and file reference](docs/reference.md) | Every command, and every file spotexec reads or writes. |
| [How spotexec works](docs/architecture.md) | Architecture, the security model, known limits and design decisions. |
| [Development](docs/development.md) | Building, the end-to-end suites, the Spot simulator, the package layout. [CONTRIBUTING.md](CONTRIBUTING.md) covers branches and releases. |
