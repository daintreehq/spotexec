# Case study: Daintree

[Daintree](https://github.com/daintreehq/daintree) is an Electron IDE for running coding agents in parallel git worktrees. It was the first real project to run its full checks on spotexec, in November 2026. That trial shaped spotexec 0.4.

## The project

- **Unit suite.** About 114,000 Vitest tests in about 5,000 files. Locally the whole suite takes 15 to 20 minutes and locks the machine up, so the project's rules forbid agents from running it locally. CI splits it into 6 shards.
- **Typecheck.** Twelve TypeScript projects, one of which peaks above 6 GB of memory.
- **Lint and guard scripts.** An ESLint ratchet with an 8 GB Node heap, and about twenty codegen and drift checks, several of which call `git ls-files`.
- **Native code.** A postinstall that patches and compiles node-pty and three native addons against Electron's headers, and an Electron binary that one unit test runs.

The fleet was the default from `spotexec setup`: 8-vCPU Spot VMs with 16 GB in us-east-1, four slots each, at most four VMs.

## What changed in spotexec

Each of these showed up on Daintree's first runs, and none had shown up in spotexec's end-to-end suites:

| Found | Changed in 0.4 |
| :--- | :--- |
| Containers saw every CPU on the VM, so four test runs each started workers for eight cores, and a VM's load reached 11.6 on 8 vCPUs. | Each container sees only its slot's CPUs (#65). |
| A typecheck using over 5 GB, next to three test shards, ran a 16 GB VM out of memory. The VM stopped answering, all four of its jobs were replayed elsewhere, and the replayed typecheck took a second VM down the same way. | Shared slots draw from one memory pool per VM, with a reserve kept back for the runner. An out-of-memory kill is reported as `OOM_KILLED` with exit 137, and a run whose VM ran out of memory isn't replayed (#70). |
| A per-container memory limit halved Node's default heap, to about 2 GB, even on a whole VM. | The limit sits on a parent cgroup that Node doesn't see, and `--exclusive` runs have none (#70). |
| Tests and scripts that ask git about the checkout failed: the slot had no `.git`. | [`git_seed`](running.md#git-on-the-runner) gives the slot a one-commit repository matching the local HEAD, branch and status (#69). |
| Two IPv6 proxy tests failed: Ubuntu's cloud image resolves `localhost` to `127.0.0.1` only, while macOS and CI runners also resolve it to `::1`. | VMs resolve `localhost` to both (#61). |
| Sharding the suite like CI took eight near-identical tasks. | [Sharded tasks](configuration.md#tasks): `shards = 8` with `{shard}` and `{shards}` (#63). |
| The cache key had to list 16 files by hand, and `init` missed the workspace `node_modules` directories. | Globs in `deps.lockfiles` (#64), and `init` reads workspace declarations (#62). |
| VMs launched on the first listed instance type, even when another was cheaper. | Each launch picks the cheapest type and zone, and a runner can ask for a VM's shape instead of naming types (#71). |

Other fixes from the trial: committed project fleets no longer break `sx` for collaborators whose runners are named differently (#75), and `CONTAINER_FRESH` warns only when it's likely to matter (#72).

The rest was project configuration, covered in [Tuning a real project](tuning.md):

- **CI's install step, copied.** The same install, cached paths and cache key inputs as CI.
- **Image additions.** Electron's runtime libraries and a locale in the image.
- **Workspace builds.** A workspace package build before every task.
- **Whole VMs for the heavy jobs.** Typecheck, lint and the guards each run `exclusive`, with typecheck's projects run one at a time.

## Results

On spotexec 0.4, with the configuration below:

| Check | Result |
| :--- | :--- |
| Full unit suite (`sx test-all`: 8 shards and a perf test, over 4 VMs) | 10m37s, 113,933 passed, 79 skipped |
| Typecheck, lint and guards in parallel (`sx check`, each on its own VM) | 5m43s, all passing |
| One test file, warm | About 20 s (5 s without the project's per-run workspace build) |
| A new VM to the first output | About 1m40s |
| First sync of the worktree | 13,551 files, 170 MB; later runs send only changed files |

What a job sees on a 4-slot, 8-vCPU VM:

| | 0.3 | 0.4 |
| :--- | :--- | :--- |
| `nproc`, `os.availableParallelism()` | 8 | 2 (8 with `--exclusive`) |
| Node's default heap | 4,144 MB | 4,144 MB |
| Memory | Unlimited; one job could take the VM down | A shared pool of about 14 GiB, with a reserve for the runner |
| `localhost` | `127.0.0.1` | `::1` and `127.0.0.1` |
| Git | No repository | HEAD, branch and status match the local worktree (with `git_seed`) |

## Configuration

Daintree's `.spotexec.toml`, shortened: every task starts with the workspace build, written here as `PREP`.

```toml
version = 1
timeout = "15m"

[sync]
git_seed = true

[deps]
lockfiles = [
  "package-lock.json", "package.json", ".npmrc", "packages/*/package.json",
  "scripts/postinstall.cjs", "scripts/patchNodePty*.cjs",
  "electron/native/*/binding.gyp", "electron/native/*/package.json", "electron/native/*/src/**",
]
install = "npm ci && node scripts/ci/verify-electron-install.mjs"
cache_dirs = ["node_modules", "packages/*/node_modules", "electron/native/*/build", "electron/native/*/bin"]
timeout = "20m"

[container]
dockerfile = ".spotexec/Dockerfile"

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

[tasks.typecheck]          # the projects one at a time: three at once outgrew 16 GB
run = "PREP; for p in $PROJECTS; do node scripts/typecheck.mjs --only \"$p\"; done"
exclusive = true

[tasks.lint]
run = "PREP; ESLINT_CONCURRENCY=2 npm run lint:ratchet"
exclusive = true

[tasks.guards]
run = "PREP; npm run check:guards && npm run format:check"
exclusive = true

[tasks.check]              # the project's `npm run check`, split as its CI splits it
parallel = ["typecheck", "lint", "guards"]
```

The Dockerfile starts from `node:22.23.2-bookworm` (matching `.nvmrc`), adds the libraries Electron's binary links (`libgtk-3-0`, `libnss3`, `libgbm1`, `libasound2` and ten more) and the `locales` package, and generates `en_US.UTF-8`.
