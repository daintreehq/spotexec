# The Daintree trial

## The project

Daintree is an Electron IDE for running coding agents in parallel git worktrees. Its checks are heavy, and agents run them from many worktrees at once:

- **Unit suite.** About 114,000 Vitest tests in about 5,000 files. Run locally, the whole suite takes 15 to 20 minutes and locks the machine up, so Daintree's own rules forbid agents from running it locally. Its CI splits it into 6 shards on GitHub's Ubuntu runners.
- **Typecheck.** Twelve TypeScript projects on the TypeScript 7 native compiler. One of them peaks above 6 GB of memory.
- **Lint.** An ESLint ratchet with an 8 GB Node heap.
- **Guards.** Around twenty codegen and drift checks, several of which call `git ls-files`.
- **Native code.** A postinstall that patches and compiles node-pty and three native addons against Electron's headers, and an Electron binary that one unit test runs.

The fleet was the default `spotexec setup` fleet in us-east-1: 8-vCPU VMs (c7a.2xlarge first, 16 GB), 4 slots each, at most 4 VMs.

## Setting it up

`spotexec init` gave a reasonable start: `npm ci` with `package-lock.json`, `node_modules` cached, tasks from the package scripts, and a Dockerfile pinned to `node:22.23.2-bookworm` from `.nvmrc`. It needed these changes to match how the project really builds:

- **Cache key and cache dirs copied from CI.** Daintree's GitHub Actions install step caches `node_modules`, `packages/*/node_modules`, `electron/native/*/build` and `electron/native/*/bin`, keyed on the lockfile plus every workspace manifest, the postinstall, its patch scripts and the addon sources. `[deps]` was set to match. `init` had missed the workspace `node_modules` (fixed in #62), and the key needed 16 literal paths until lockfile globs arrived (#64).
- **The install provisions Electron.** Electron's npm package downloads its binary on first use, so the install runs CI's own `verify-electron-install.mjs` after `npm ci`, which puts the binary inside the cached `node_modules`.
- **Dockerfile additions.** Electron's binary links GTK, NSS and a dozen other libraries even when it runs as plain Node, and Daintree's Vitest config forces `LC_ALL=en_US.UTF-8`, so the image needed those libraries and a generated locale. The base image already had git, python3, make and g++ for the native rebuilds.
- **Workspace builds before every task.** The workspace packages' `dist/` is built from source, not from the lockfile, so `[deps]` can't cache it. Tests import it and typecheck reads its declarations. CI rebuilds it after every cache restore, and so must every task (#78 would make this one setting).
- **Heavy tasks on a whole VM.** Typecheck, lint and the guards each run `exclusive`, as each gets its own runner in CI.

The container built on a cold VM and was pushed to ECR in 1m41s. The first test run, which also ran `npm ci` (1,438 packages) and fetched Electron, took 1m49s.

## What broke, and what fixed it

| What happened | Cause | Fix |
| :--- | :--- | :--- |
| Two perf-provenance tests failed: `sourceSha` was null. Later, the guards failed with `Command failed: git ls-files -z`. | The slot has no `.git`, so anything asking git about the checkout fails remotely while it works locally and in CI. | `[sync] git_seed = true` (#69): the slot gets a one-commit `.git` whose HEAD, branch and `git status` match the local worktree. |
| Two IPv6 dev-preview proxy tests got `502` and `socket hang up`. | Ubuntu's cloud image maps `localhost` to `127.0.0.1` only; macOS and GitHub's runners also map it to `::1`. A server bound to `::1` and dialed as `localhost` was unreachable. | First boot adds `localhost` to the `::1` line (#61). |
| VM load hit 10.5 and 11.6 on 8 vCPUs, and a file-scanning test timed out at 15 s. | Containers saw the whole VM, so Vitest (and anything else sizing itself by core count) started workers for 8 CPUs in each of 4 slots. | Each container is pinned to its slot's CPUs with `--cpuset-cpus`, so `nproc` and `os.availableParallelism()` report the slot's share (#65). |
| Typecheck lost its VM twice and exited 125; two VMs went unresponsive, and every job on them was replayed elsewhere. | A global out-of-memory on a 16 GB VM: `tsc` at 5.3 GB beside three test shards, no swap, no per-container limit. The VM thrashed until the helper's heartbeats stopped, spotexec declared it lost and replayed all four jobs, and the replayed `tsc` took a second VM down the same way. | Shared slots run inside one memory pool per VM with a reserve kept back for the runner, the helper protects itself from the OOM killer, OOM kills are reported as `OOM_KILLED` with exit 137, and a run whose VM ran out of memory is never replayed (#70). |
| After the first memory fix, a guard died with `JavaScript heap out of memory` at about 2 GB. | Node sizes its default heap from the container's memory limit. Any container limit, even 14 GB, cut the default heap from 4,144 MB to about 2,000 MB. | The limit moved to a parent cgroup Node can't see, and `--exclusive` runs get no limit (#70, second commit). |
| Typecheck still ran out of memory with a whole VM to itself. | `npm run typecheck` runs three compiler processes at once, which together outgrew 15.6 GB. | Daintree's task runs the twelve projects one after another: 34 s in total. |
| Typecheck passed three times, then failed on a fresh VM with `TS2307: Cannot find module '@daintreehq/tour'`. | The task didn't build the workspace packages. Earlier test runs had left their `dist/` in the slot; a fresh slot had none. | Every task builds the workspaces first. |
| Lint aborted after 9.5 minutes with a V8 heap error. | The task ran plain `eslint .` with Node's default heap, while CI runs `lint:ratchet` with an 8 GB heap. | Run exactly what CI runs. |
| The sharded suite needed eight near-identical tasks. | No way to parameterize a task over shards. | `shards = 8` with `{shard}` and `{shards}` (#63). |
| VMs launched on c7a.2xlarge even when c6a.2xlarge was cheaper. | A single-region runner tried its types in listed order. | Every launch goes to the cheapest current type and zone, and a runner can ask for "8 vCPUs, at least 32 GB" instead of naming types (#71). |

Smaller findings, filed as issues: committed project fleets broke `sx` for collaborators whose runners are named differently (#66, fixed by #75), `CONTAINER_FRESH` warned on every same-slot run (#67, fixed by #72), and the dependency install's output is mixed into the command's output on a cache miss (#68, open).

## Results

Every run of the full suite fanned 9 parts (8 shards plus a perf-liveness job) over 4 VMs.

| Run | spotexec | Wall time | Result |
| :--- | :--- | :--- | :--- |
| First | v0.3.0 | 11m15s | 5 failures in 4 files, all environment gaps (no `.git`, IPv6 `localhost`, a missing workspace build) |
| With heavy tasks alongside | v0.3.0 | not timed | The same failures, plus a test timing out under CPU overload; typecheck's out-of-memory lost two VMs and killed the perf-liveness part |
| Integration build | develop + #61 to #65, #69, #70 (first version) | 11m41s | 2 failures: a Daintree test that is flaky under load, and a build killed by the strict per-slot memory cap |
| Final | develop + all of the above, #70 second version | 10m37s | All green: 113,933 tests passed, 79 skipped |

Other timings on the final build:

- **`sx check`** (typecheck, lint and guards, each on its own VM): 5m43s, all passing. Typecheck 1m8s, guards 4m45s, lint 5m42s.
- **One test file, warm:** about 20 s. The same file run without the workspace build took 5.2 s end to end (sync, image and dependency checks, the test, write-back); the rest is Daintree's workspace build on a 2-CPU slot.
- **Cold VM to first output:** 1m42s with the image in ECR and dependencies in the S3 cache. 2 to 4 minutes when the dependency key had changed and `npm ci` ran.
- **Incremental sync:** after the first run, a changed config synced as `1 files (5 KB)`, against `13551 files (170.2 MB)` the first time.

What a container saw, before and after:

| Check | v0.3.0 | v0.4 |
| :--- | :--- | :--- |
| `nproc` / `os.availableParallelism()` in a 4-slot VM | 8 | 2 (8 with `--exclusive`) |
| Node's default heap | 4,144 MB | 4,144 MB |
| Memory | No limit; one job could take the VM down | Shared slots in a pool capped at the VM's memory minus a reserve (14.3 GiB on a c7a.2xlarge) |
| `localhost` | `127.0.0.1` | `::1` and `127.0.0.1` |
| `git rev-parse HEAD`, branch, `git status` | No repository | Same as the local worktree |

Spot prices in us-east-1 during the trial, for the default fleet's three types, ranged from $0.129 an hour (c6a.2xlarge, us-east-1f) to $0.331 (m7a.2xlarge, us-east-1f). A launch on the final build landed on c6a.2xlarge in us-east-1f, the cheapest option.

## The final config

The Daintree configuration as it stood at the end of the trial, for reference. Comments explain the non-obvious parts.

```toml
version = 1
timeout = "15m"
env_allow = ["NODE_OPTIONS", "TZ"]

[sync]
mode = "auto"
writeback = "always"
exclude = [".env*", "*.pem", "*.key"]
git_seed = true                      # provenance tests, guards and scripts ask git about the checkout

[deps]                               # mirrors CI's install step: same cached paths, same key inputs
lockfiles = [
  "package-lock.json", "package.json", ".npmrc",
  "packages/*/package.json",
  "scripts/postinstall.cjs", "scripts/patchNodePty*.cjs",
  "electron/native/*/binding.gyp", "electron/native/*/package.json", "electron/native/*/src/**",
]
install = "npm ci && node scripts/ci/verify-electron-install.mjs"
cache_dirs = ["node_modules", "packages/*/node_modules", "electron/native/*/build", "electron/native/*/bin"]
timeout = "20m"

[container]
dockerfile = ".spotexec/Dockerfile"

# Every task starts with the workspace build (shortened here as PREP):
# npm run prepare --workspaces --if-present >/tmp/prepare.log 2>&1 || { cat /tmp/prepare.log; exit 1; }
[tasks.test]
run = "PREP; npx vitest run {args}"

[tasks.unit]
run = "PREP; npx vitest run --shard={shard}/{shards} --exclude scripts/perf/__tests__/scenarioLiveness.test.ts {args}"
shards = 8
timeout = "30m"

[tasks.perf-liveness]
run = "PREP; npx vitest run scripts/perf/__tests__/scenarioLiveness.test.ts"
timeout = "30m"

[tasks.test-all]
parallel = ["unit", "perf-liveness"]
fail_fast = false

[tasks.typecheck]                    # the twelve projects one at a time; three at once outgrew 16 GB
run = "PREP; for p in $(...project names...); do node scripts/typecheck.mjs --only \"$p\"; done"
exclusive = true

[tasks.lint]
run = "PREP; ESLINT_CONCURRENCY=2 npm run lint:ratchet"
exclusive = true

[tasks.guards]
run = "PREP; npm run check:guards && npm run format:check"
exclusive = true

[tasks.check]                        # npm run check, split over three VMs the way CI splits it
parallel = ["typecheck", "lint", "guards"]
fail_fast = false
```

The Dockerfile adds `libasound2 libatk-bridge2.0-0 libatk1.0-0 libcups2 libdrm2 libgbm1 libgtk-3-0 libnss3 libxcomposite1 libxdamage1 libxkbcommon0 libxrandr2 libxshmfence1 libxss1 locales` to `node:22.23.2-bookworm` and generates `en_US.UTF-8`.

## Lessons

- **The first real project finds what the test suites can't.** spotexec had end-to-end suites against a Spot simulator and an SSH container, and none of the problems above showed up in them (the missing `.git` was already a documented gap). Real runners (Ubuntu cloud images, 16 GB VMs, four heavy jobs at once) were what exposed them.
- **Isolation matters as much as speed.** A fleet that runs four jobs per VM has to keep one job's CPU and memory use from hurting the other three. Without that, one heavy job didn't just fail: it took everything on its VM down with it, twice.
- **Fixes need the real workload too.** The first memory limit worked in unit tests and broke Node's heap on the first real run, because Node reads cgroup limits. It took a live run to find, and another to confirm the second design.
- **Copy CI, don't reinvent it.** Every Daintree problem on the config side came from doing something other than what CI does: a different lint command, a missing workspace build, three compilers at once. CI's workflow files are the specification for what a remote run needs.
- **State left in a slot can hide a missing step.** Ignored build output stays in a slot between runs, which is what makes repeat runs fast. It also let a task that forgot a build step pass on slots that earlier runs had prepared, and fail on a fresh one.
