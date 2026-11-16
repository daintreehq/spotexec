# When to use spotexec

spotexec moves a command off your machine for as long as it runs, onto a few VMs that every command shares. That pays off for some commands and not for others. The numbers here come from [the Daintree case study](case-study-daintree.md), an Electron app with about 114,000 Vitest tests.

## Where it makes a difference

- **Whole-suite checks that are too heavy to run locally.** Daintree's unit suite takes 15 to 20 minutes on a laptop and locks the machine up. On spotexec it runs as one `exclusive` job on a warm 8-vCPU Spot VM, with the laptop free throughout. A full check before a pull request, instead of waiting for CI after the push, is where spotexec changes the most.
- **Several heavy checks at once.** Typecheck, lint and the project's guard scripts ran side by side, each on a whole VM, in 5m43s, the time of the slowest one. On one machine they compete for the same cores and memory.
- **Many worktrees on one machine.** With several coding agents each running tests and type checks, your laptop's CPU and memory become the bottleneck, and every check slows down. Each offloaded check gets its own slot on a small shared pool, and when the pool is full, checks wait their turn instead of slowing each other down. The pool shrinks to zero when the work stops.
- **Catching Linux-only problems before you push.** Commands run on Linux in a pinned [project container](environments.md#project-containers), as in CI. Case-sensitive paths, missing system libraries and locale problems show up before the push instead of after it. Suites that bind fixed ports run there too with [`network = "private"`](environments.md#settings), and suites that start containers of their own with [`docker = true`](environments.md#docker-inside-the-container).

## Where it doesn't help

- **Small, quick runs.** A warm run of one quick test file took about 5 seconds end to end: scan, sync, image and dependency checks, the test itself and write-back. Add any per-run setup your project needs (Daintree's workspace build adds about 14 seconds). A test that takes 2 seconds locally is faster locally, unless your machine is already saturated.
- **Cold starts, and many VMs at once.** A new VM takes a minute and a half or more before your command runs, about 3 minutes on Daintree once the image, dependency restore, sync and setup are counted, and the first run after your dependencies change installs them again (2 to 4 minutes for Daintree). After its last job a VM stays up, and bills, for `idle_timeout` (10 minutes by default). A few VMs that stay warm pay this once a session; spreading one job over many fresh VMs pays it on every VM, which is why [sharding a suite across VMs](scaling.md#when-sharding-pays) rarely beats one run on a warm one. [Baked images](environments.md#baked-images) shorten the boot.
- **Interactive work.** Dev servers, watch modes and anything that needs a terminal stay local.

## With coding agents

Agents can use the pool in two ways:

- **Automatically:** the [hook](agents.md#claude-code-hook) rewrites heavy commands to spotexec before they run.
- **On instruction:** a few lines in `CLAUDE.md` or `AGENTS.md` naming which commands go through `sx`, for teams that would rather not have commands rewritten.

A split that suits the timings above:

| Run | Where |
| :--- | :--- |
| One test file or a few, while iterating | Locally |
| The whole suite, before opening a pull request | `sx test-all` (one `exclusive` run on a whole VM) |
| Typecheck, lint, the full check | `sx typecheck`, `sx lint`, `sx check` |
| Anything that needs the machine the last run used | `sx -s ...` ([same slot](running.md#coming-back-to-the-same-machine)) |

An agent gets the command's own output and exit code back, plus one summary line, or NDJSON events with `--json`. Two exit codes come from spotexec itself, so an agent can tell "the tests failed" from "the tests couldn't run":

- `125` means spotexec failed.
- `137` with an `OOM_KILLED` warning means the command ran out of memory.

## What it costs

You pay for Spot VMs while they run, plus small amounts for disks, public IP addresses, ECR storage and the S3 dependency cache. In us-east-1 in November 2026, an 8-vCPU Spot VM cost between $0.13 and $0.33 an hour, depending on type and zone. spotexec launches each VM on the cheapest option available at the time. Most of the bill is VMs that aren't doing anything: each one idles for `idle_timeout` after its last job. Every VM you add adds that idle tail and a cold start, so how many VMs you start often matters more than how long your jobs run. On Daintree, the suite sharded across 9 VMs cost roughly 30 to 70 cents a run, almost all of it start-up and idling (see [the case study](case-study-daintree.md#one-vm-per-part-the-lesson)). Run on one warm VM that every command shares, the same suite adds only the minutes it runs. These are estimated from hourly prices rather than measured from a bill.

To keep the bill small, keep the pool small: a low `max_instances`, one VM type, `exclusive` for the jobs that need a whole VM, and runs that queue for a warm VM rather than launching more. [Scaling](scaling.md#a-few-warm-vms) covers the recommended shape, and [AWS setup](aws.md) covers cost guardrails.

## Known gaps

spotexec has been run on one real project so far: Node.js with npm workspaces, Electron and native addons, on AWS Spot in one region, from a Mac. Other stacks have scaffolds and presets but haven't been run against real repositories yet:

- **Python** (including virtualenv relocation), **Go**, **Rust**, **the JVM** and **Bun**.
- **Rust's `target/` directory** isn't cached, so each new slot starts with a cold build.
- **JVMs** size their default heap from the memory they can see, which in a shared slot is the whole VM. Set `-Xmx` (or Gradle's and Maven's equivalents) for jobs that share a VM.

Not yet exercised on a real project either:

- **Runners:** SSH runners, macOS runners and Graviton (arm64) fleets.
- **Concurrency:** several worktrees running heavy checks at the same time.
