# When to use spotexec

spotexec moves a command off your machine for as long as it runs. That pays off for some commands and not for others. The numbers here come from [the Daintree case study](case-study-daintree.md), an Electron app with about 114,000 Vitest tests.

## Where it makes a difference

- **Whole-suite checks that are too heavy to run locally.** Daintree's unit suite takes 15 to 20 minutes on a laptop and locks the machine up. Sharded over 4 Spot VMs it ran in 10m37s, with the laptop free throughout. A full check before a pull request, instead of waiting for CI after the push, is where spotexec changes the most.
- **Several heavy checks at once.** Typecheck, lint and the project's guard scripts ran side by side on three VMs in 5m43s, the time of the slowest one. On one machine they compete for the same cores and memory.
- **Many worktrees on one machine.** With several coding agents each running tests and type checks, your laptop's CPU and memory become the bottleneck, and every check slows down. Each offloaded check gets its own slot, and the fleet grows with demand and shrinks to zero afterwards.
- **Catching Linux-only problems before you push.** Commands run on Linux in a pinned [project container](environments.md#project-containers), as in CI. Case-sensitive paths, missing system libraries and locale problems show up before the push instead of after it.

## Where it doesn't help

- **Small, quick runs.** A warm run of one quick test file took about 5 seconds end to end: scan, sync, image and dependency checks, the test itself and write-back. Add any per-run setup your project needs (Daintree's workspace build adds about 14 seconds). A test that takes 2 seconds locally is faster locally, unless your machine is already saturated.
- **Cold starts.** A new VM takes about a minute and a half before your command runs, and the first run after your dependencies change installs them again (2 to 4 minutes for Daintree). A VM stays up for `idle_timeout` after its last job (10 minutes by default), so a working session usually pays this once. [Baked images](environments.md#baked-images) shorten the boot.
- **Interactive work.** Dev servers, watch modes and anything that needs a terminal stay local.
- **Suites that need fixed ports or Docker.** Until #79 and #80 are resolved, tests that bind fixed ports can collide with another slot's on the same VM, and tests that start their own containers can't run inside a project container. Run such suites `exclusive`, or keep them local.

## With coding agents

Agents can use the fleet in two ways:

- **Automatically:** the [hook](agents.md#claude-code-hook) rewrites heavy commands to spotexec before they run.
- **On instruction:** a few lines in `CLAUDE.md` or `AGENTS.md` naming which commands go through `sx`, for teams that would rather not have commands rewritten.

A split that suits the timings above:

| Run | Where |
| :--- | :--- |
| One test file or a few, while iterating | Locally |
| The whole suite, before opening a pull request | `sx test-all` (a [sharded task](configuration.md#tasks)) |
| Typecheck, lint, the full check | `sx typecheck`, `sx lint`, `sx check` |
| Anything that needs the machine the last run used | `sx -s ...` ([same slot](running.md#coming-back-to-the-same-machine)) |

An agent gets the command's own output and exit code back, plus one summary line, or NDJSON events with `--json`. Two exit codes come from spotexec itself, so an agent can tell "the tests failed" from "the tests couldn't run":

- `125` means spotexec failed.
- `137` with an `OOM_KILLED` warning means the command ran out of memory.

## What it costs

You pay for Spot VMs while they run, plus small amounts for disks, public IP addresses, ECR storage and the S3 dependency cache. In us-east-1 in November 2026, an 8-vCPU Spot VM cost between $0.13 and $0.33 an hour, depending on type and zone. spotexec launches each VM on the cheapest option available at the time. A full Daintree suite run kept 4 VMs busy for about 11 minutes, and each then idled for up to 10 minutes before powering off. That's roughly 15 to 25 cents a run, estimated from hourly prices rather than measured from a bill. [AWS setup](aws.md) covers cost guardrails.

## Known gaps

spotexec has been run on one real project so far: Node.js with npm workspaces, Electron and native addons, on AWS Spot in one region, from a Mac. Other stacks have scaffolds and presets but haven't been run against real repositories yet:

- **Python** (including virtualenv relocation), **Go**, **Rust**, **the JVM** and **Bun**.
- **Rust's `target/` directory** isn't cached, so each new slot starts with a cold build.
- **JVMs** size their default heap from the memory they can see, which in a shared slot is the whole VM. Set `-Xmx` (or Gradle's and Maven's equivalents) for jobs that share a VM.

Not yet exercised on a real project either:

- **Runners:** SSH runners, macOS runners and Graviton (arm64) fleets.
- **Concurrency:** several worktrees running heavy checks at the same time.

Open issues worth knowing about:

- **#68:** dependency install output appears in the command's output on a cache miss.
- **#76:** parallel and sharded runs end without an overall summary.
- **#77:** no option to group a parallel run's output by part.
- **#78:** no shared setup step for tasks.
- **#79:** tests that bind fixed ports can collide across slots.
- **#80:** tests can't start Docker containers inside a project container.
