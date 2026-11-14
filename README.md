<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme/logo-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/readme/logo-light.svg">
    <img src="docs/assets/readme/logo-light.svg" alt="spotexec" width="280">
  </picture>
</p>

<h1 align="center">Run your tests and builds off your&nbsp;laptop</h1>

<p align="center">
  Coding agents in parallel worktrees run test suites, type checks and builds all at once, and your laptop pays for it. Put <code>sx</code> in front of those commands and they run on a homelab box over SSH, or on AWS Spot VMs that shut down when idle. Output and exit codes stream back, and changed files return through a 3-way check that keeps any edits you made meanwhile.
</p>

<p align="center">
  <a href="https://github.com/daintreehq/spotexec/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/daintreehq/spotexec?style=flat-square&label=release&labelColor=3d444d&color=57606a"></a>
  <img alt="Runs on macOS and Linux" src="https://img.shields.io/badge/runs%20on-macOS%20%C2%B7%20Linux-57606a?style=flat-square&labelColor=3d444d">
  <a href="LICENSE"><img alt="MIT license" src="https://img.shields.io/badge/license-MIT-57606a?style=flat-square&labelColor=3d444d"></a>
</p>

<p align="center">
  <a href="#install"><b>Install</b></a> &nbsp;·&nbsp;
  <a href="#documentation">Documentation</a> &nbsp;·&nbsp;
  <a href="https://spotexec.com">spotexec.com</a>
</p>

<p align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme/terminal-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/readme/terminal-light.svg">
  <img src="docs/assets/readme/terminal-light.svg" width="690" alt="A terminal running sx pnpm vitest run -u src/auth.test.ts. Vitest's output streams back as if it ran locally: 12 tests passed and 2 snapshots updated. spotexec ends with one summary line: 4.1s on aws#3, synced 4 files (18 KB), pulled 2 modified files. Labels under it read: ran on a Spot VM, sent only what changed, wrote the snapshots back.">
</picture>
</p>

<p align="center">
  <code>sx</code> is short for <code>spotexec run --</code>, and works with any command: <code>sx&nbsp;cargo&nbsp;build</code>.
</p>

## Why spotexec

When several coding agents work at once, one per git worktree, each checks its work with the test suite, the type checker, the linter and a build, all on the laptop you are typing on. Every check slows down, and the agents end up waiting on each other for CPU.

The usual ways out each cost something. **CI** is minutes away from the edit. A **cloud dev environment** moves your editor, checkout and agents off your machine. A **shared build box** needs someone to keep it running and in sync.

spotexec keeps everything local except the work. Only the heavy commands move, and only while they run.

- **Your command, unchanged.** Output streams back as it runs, the exit code is the command's own, and remote paths in stack traces are rewritten to local ones.
- **Only what changed travels.** Each run sends just the files the runner is missing. Dependencies restore from a cache keyed by your lockfiles.
- **Your edits come first.** Changed files come back through a 3-way check. If you edited a file during the run, your version stays and the runner's is [saved separately](docs/running.md#write-back-and-artifacts).
- **VMs that clean up after themselves.** Spot fleets grow from zero as work queues up, and every VM powers itself off when idle. A run that loses its VM to an interruption is replayed once on a replacement.
- **Nothing to host.** No control plane, no service in your account, and no AWS credentials on the VMs. Coordination lives on your machine and in EC2 tags.
- **Made for agents.** Worktrees share one fleet through isolated slots and a fair queue. Agents see one summary line, or NDJSON events with `--json`.

## Install

```sh
brew install daintreehq/tap/spotexec
```

Homebrew works on macOS and Linux, and installs `spotexec`, the `sx` alias and shell completions.

<details>
<summary><b>Linux packages</b> (.deb, .rpm, .apk)</summary>
<br>

Download the package for your architecture from the [latest release](https://github.com/daintreehq/spotexec/releases/latest), then:

```sh
sudo apt install ./spotexec_*_linux_amd64.deb      # Debian, Ubuntu
sudo dnf install ./spotexec_*_linux_amd64.rpm      # Fedora, RHEL
sudo apk add --allow-untrusted ./spotexec_*.apk    # Alpine
```

</details>

<details>
<summary><b>Other systems, or from source</b></summary>
<br>

The release also has `.tar.gz` archives for macOS and Linux on amd64 and arm64. Each holds a single `spotexec` binary: put it on your `PATH` and link `sx` to it.

To build from source you need Go 1.26 or later:

```sh
git clone https://github.com/daintreehq/spotexec.git && cd spotexec
mkdir -p "$(go env GOPATH)/bin"    # must exist, and be on your PATH
make install                       # builds spotexec and sx, installs both there
```

`go install` isn't supported. The build cross-compiles the remote helper for Linux and macOS (amd64 and arm64) and embeds it in the binary, which `make` and the release builds do.

</details>

**Requirements.** Your machine needs `git` and OpenSSH. A runner needs an SSH server, `bash` and `gzip`, on Linux or macOS (amd64 or arm64), plus Docker if you use [project containers](docs/environments.md#project-containers). Nothing else is installed by hand: the helper uploads itself on first contact, and AWS VMs get Docker for you.

## Quick start

### 1. Choose your runners

Use either, or both.

**AWS Spot**, which scales to zero. Sign in with the AWS CLI as someone who can manage IAM users, then let setup do the rest:

```sh
brew install awscli    # if you don't have the AWS CLI yet
aws login              # or: aws sso login, aws configure
spotexec setup
```

Setup writes `~/.config/spotexec/config.toml` with a Spot fleet, gives this machine its own IAM user and key limited to spotexec's permissions, and checks your region, network and Spot quota. Your own AWS session can expire afterwards without affecting spotexec. [Setting up machines](docs/setup.md) covers more machines, key rotation and revoking access.

**An SSH machine you already own**, such as a Linux box or a spare Mac. Key login must already work. Put it in `~/.config/spotexec/config.toml`:

```toml
version = 1
default_pool = ["homelab"]

[runners.homelab]
type = "ssh"
hosts = ["me@192.168.1.50"]
max_slots = 2
```

Then run `spotexec setup`.

To use both, list both in `default_pool` and give the homelab the lower `priority` number (say 10, and 20 for AWS): it fills first and AWS takes the overflow.

### 2. Set up a project

```sh
cd ~/code/my-app
spotexec init              # writes .spotexec.toml: lockfiles, tasks
spotexec container init    # the toolchain as a Dockerfile, for AWS VMs
spotexec doctor            # checks config, SSH, AWS, quota
```

The runner needs your toolchain (`node`, `pnpm`, `go` and so on). AWS VMs boot plain Ubuntu, so give them a [project container](docs/environments.md#project-containers) or a runner `bootstrap_script`. On an SSH machine you can skip `container init` if the toolchain is on `PATH` in bash's login profile (`~/.bash_profile` or `~/.profile`, not `~/.zprofile`), or set by [`remote_path_setup`](docs/configuration.md).

### 3. Run

```sh
sx pnpm test                   # sync, run remotely, write changes back
sx pnpm tsc --noEmit           # type checks
sx pnpm prettier --write .     # formatted files land in your worktree
sx test --reporter=dot         # a task from .spotexec.toml, extra args
sx verify                      # a parallel task fanned out over slots
spotexec status                # what's running where, and idle timers
```

Files the command changes come back. Ignored outputs such as coverage reports need `pull_artifacts` in `.spotexec.toml`, or a flag like `--pull coverage/`; see [write-back and artifacts](docs/running.md#write-back-and-artifacts).

The first AWS run boots a VM, which takes about a minute plus your bootstrap. Later runs reuse it until it has been idle for `idle_timeout`. [Bake an image](docs/environments.md#baked-images) to bring cold boots close to bare boot time.

## With coding agents

Away from a terminal, `sx` stays quiet. The command's own output passes through untouched, and spotexec adds one summary line saying where it ran and what came back:

```text
$ sx pnpm prettier --write src 2>&1 | cat
src/index.ts 41ms
sx: 2.3s on aws#2 · synced 3 files (12 KB) · pulled 1 modified file
```

To have agents use the fleet without being told, install the hook for your harness:

```sh
spotexec hook install --claude   # Claude Code: heavy Bash commands go to the fleet
spotexec hook install --codex    # Codex and others: adds a section to AGENTS.md
```

The Claude Code hook rewrites tests, type checks, linters, formatters and builds to `spotexec run --` before they execute, and keeps `git`, dev servers and watch modes local. When an agent needs the machine it just used, say after installing a package there, it adds `-s` (`--same-slot`): `sx -s pnpm test`. [Coding agents](docs/agents.md) covers the hook's rules and the NDJSON events.

## How it works

<p align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme/how-it-works-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/readme/how-it-works-light.svg">
  <img src="docs/assets/readme/how-it-works-light.svg" width="760" alt="Diagram. Left, your machine: three git worktrees (agent-1, agent-2, agent-3) running sx pnpm test, sx pnpm tsc --noEmit and sx pnpm prettier --write ., with a fair local queue and nothing hosted. Middle, over SSH with your own keys: 1 sync, only the files the slot is missing; 2 run, output and exit code stream back; 3 write back, 3-way checked. Right, your runners: homelab-01 over SSH with both slots busy, Spot VM aws#1 with one of two busy, and aws#2 idle and powering off. Spot VMs launch for queued work and power themselves off when idle.">
</picture>
</p>

1. **Sync.** `sx` lists your worktree's files with their Git blob IDs, takes a free slot on a runner, and sends only the blobs that slot doesn't have, as one compressed stream over SSH.
2. **Run.** The runner restores dependencies from a cache keyed by your lockfiles, then runs the command in its own process group, with heartbeats both ways.
3. **Write back.** The runner reports which files the command changed, by content. Each one is applied only if your copy still matches what was synced.
4. **Clean up.** When there is no work left, a Spot VM's own watchdog powers it off, so billing stops even if your laptop is closed.

[How spotexec works](docs/architecture.md) covers the security model, the known limits and the design decisions.

## Documentation

| Guide | What's in it |
| :--- | :--- |
| [Setting up machines](docs/setup.md) | `spotexec setup`, choosing runners, more machines over SSH, keys, rotation and revoking a machine. |
| [AWS setup](docs/aws.md) | Every AWS step by hand: sign-in, the IAM policy, regions, Spot quota, networking, cost guardrails. |
| [Running commands](docs/running.md) | Run flags, the environment on the runner, exit codes, write-back and artifacts. |
| [Coding agents](docs/agents.md) | What agents see, `--same-slot`, NDJSON events, the Claude Code hook. |
| [Toolchains&nbsp;and&nbsp;caches](docs/environments.md) | Project containers, the dependency cache, baked images. |
| [Scaling](docs/scaling.md) | Slots, queueing, CPU sizing and warm pools. The [pool design](docs/POOL.md) has the scheduling details. |
| [Configuration](docs/configuration.md) | Every key in `config.toml` and `.spotexec.toml`. |
| [Troubleshooting](docs/troubleshooting.md) | Error codes and what to do about them. |
| [Commands and files](docs/reference.md) | Every command, and every file spotexec reads or writes. |
| [How spotexec works](docs/architecture.md) | Architecture, the security model, known limits and design decisions. |
| [Development](docs/development.md) | Building, the end-to-end suites, the Spot simulator and the package layout. |

## Contributing and license

Branches, CI and releases are in [CONTRIBUTING.md](CONTRIBUTING.md). spotexec is released under the [MIT license](LICENSE).
