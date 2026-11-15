# Where it fits

## Where it makes a difference

- **Whole-suite checks that are too heavy to run locally.** Daintree's unit suite takes 15 to 20 minutes locally and locks the machine up. Through spotexec it ran in 10m37s on 4 Spot VMs, with the laptop free the whole time. Daintree's rules had banned agents from running it locally, so before spotexec the only full run was CI's, after a push. A full check before opening a pull request is the change that matters most here.
- **Several heavy checks at once.** Typecheck, lint and the guards ran side by side on three VMs in 5m43s, as long as the slowest of them. On one machine they compete for the same cores and memory.
- **Many worktrees, one machine.** With five or ten agents each running tests and type checks, the laptop's CPU and memory are the bottleneck and every check slows down. Offloaded, each check gets its own slot, and the fleet grows with demand and shrinks to zero afterwards. This is the main case for spotexec, and it hasn't been measured yet (see below).
- **Matching CI's environment before pushing.** The runner is Linux with a pinned toolchain in a container, like CI. Problems that only show on Linux (case-sensitive paths, locale, `localhost` resolution, missing system libraries) appear before the push rather than after it.

## Where it doesn't help

- **Small, quick runs.** A warm run of one quick test file took 5.2 seconds end to end (scan, sync, image and dependency checks, the run itself, write-back), plus any per-run setup the project needs, about 14 seconds for Daintree's workspace build. A test file that takes 2 seconds locally takes about 20 seconds remotely. The edit, run, edit loop on one test is faster locally, unless the machine is already saturated.
- **Cold starts.** A new VM takes about a minute and a half before a command runs, and a first run after the dependencies change has to reinstall them (2 to 4 minutes for Daintree). An idle VM stays up for `idle_timeout` (10 minutes by default), so a working session pays this once, but an agent waiting on one test after a quiet spell pays it again.
- **Interactive work.** Dev servers, watch modes and anything needing a terminal stay local.
- **Suites that need fixed ports or Docker.** Tests binding fixed ports collide across slots on one VM (#79), and tests that start their own containers can't run in a project container (#80). Both are open.

## With coding agents

Two ways to get agents to use the fleet:

- **The hook** (`spotexec hook install --claude`) rewrites heavy Bash commands to spotexec automatically.
- **Explicit instructions.** Daintree chose not to use the hook, and tells agents which commands go through `sx` instead.

A split that suits the timings above:

| Run | Where |
| :--- | :--- |
| One test file or a few, while iterating | Locally |
| The whole suite, before a pull request | `sx test-all` |
| Typecheck, lint, the full check | `sx typecheck`, `sx lint`, `sx check` |
| Anything after installing a system package on the runner | `sx -s ...` (same slot) |

What agents get back is the command's own output and exit code, plus one summary line (or NDJSON with `--json`). Exit code 125 means spotexec itself failed, and 137 with an `OOM_KILLED` warning means the command ran out of memory, so an agent can tell "the tests failed" from "the tests couldn't run". Open issues that affect agents most: on a cache miss, install output mixes into the command output (#68); parallel runs end without an overall summary (#76); and their output interleaves, with no option to group it per part (#77).

## Costs

The Spot price of an 8-vCPU VM in us-east-1 ranged from $0.129 to $0.331 an hour across types and zones during the trial, and VMs launch on the cheapest. A full Daintree suite run kept 4 VMs busy for about 11 minutes, and each then idled for up to 10 minutes before powering off. That puts a full-suite run at roughly 15 to 25 cents of Spot time, plus small charges for disks, public IP addresses, ECR storage and the S3 dependency cache. This is an estimate from hourly prices, not a measured bill.

## Not tested yet

The trial covered one project (Node, npm workspaces, Electron, native addons) on AWS Spot in one region, driven from one Mac. Not yet exercised on a real project:

- **Other stacks:** Python (including virtualenv relocation between slots), Go, Rust, the JVM, Bun, and pnpm or Yarn workspaces. The Rust `target/` directory isn't cached, so each new slot starts with a cold build. JVMs size their heaps from the memory they can see, which in a shared slot is the whole VM's, so four JVM jobs on one VM could outgrow the pool.
- **Runner types:** SSH runners (homelab machines), macOS runners, Graviton (arm64) fleets, and multi-region fleets.
- **Clients:** a Linux laptop as the client, and a second machine sharing the account.
- **Setup:** setup from scratch on a new AWS account, including the small Spot quota path.
- **Features:** the agent hooks, and the `vcpus` / `min_memory_gib` option on live AWS.
- **Concurrency:** the multi-agent case itself. Today's numbers are for single runs. The deciding measurement is several worktrees running `check` and the full suite at the same time, locally and through spotexec, comparing wall time and how usable the laptop stays.

Good candidates for a second trial are public repositories with real suites and CI to compare against:

| Repository | Stack | What it would exercise |
| :--- | :--- | :--- |
| `django/django` | Python | A large suite and parallel test workers |
| `BurntSushi/ripgrep` | Rust | Cold `target/` builds per slot |
| `cli/cli` | Go | Go module cache and build stamping |
| `vitejs/vite` | pnpm monorepo | pnpm workspaces |
| `testcontainers/testcontainers-go` | Go + Docker | Tests that start Docker containers (#80) |
| `spring-projects/spring-petclinic` | Java | JVM heap sizing in shared slots |
