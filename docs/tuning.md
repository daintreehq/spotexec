# Tuning a real project

`spotexec init` gives you a working start: an install command, a dependency cache, tasks from your package scripts, and a [project container](environments.md#project-containers) pinned to your toolchain's version. A small project often needs nothing more. A larger one usually does before its checks pass remotely the way they pass locally and in CI. This guide covers what to look at, roughly in the order problems tend to show up. [The Daintree case study](case-study-daintree.md) shows each step on a real project.

## Setting up

### Start from your CI configuration

Your CI workflow is the most accurate record of what a clean Linux machine needs to run your checks. Copy three things from its install step into `[deps]`:

- **What it runs.** The install, plus any step after it that fetches or builds something your tests need (a browser or Electron binary, generated code from a schema).
- **What it caches.** Every path, not just `node_modules`: workspace packages' own `node_modules`, native addon build output, virtualenvs.
- **What the cache key hashes.** A GitHub Actions `hashFiles(...)` list translates directly into `lockfiles` globs. A key that leaves out a postinstall script or native sources restores stale builds after they change.

```toml
[deps]
lockfiles = ["package-lock.json", "packages/*/package.json", "scripts/postinstall.cjs", "native/*/src/**"]
install = "npm ci && node scripts/fetch-browser.mjs"
cache_dirs = ["node_modules", "packages/*/node_modules", "native/*/build"]
```

`spotexec sync-plan` lists the files the key expands to.

### Run what CI runs

A task that runs something close to CI's command, but not quite the same, is the commonest cause of "passes in CI, fails remotely". Use the same script, flags and environment: if CI runs the linter with a larger heap or a set concurrency, so should the task.

### Rebuild source-derived output in a setup step

Anything built from your sources rather than from the lockfile can't live in the dependency cache: workspace packages' `dist/`, generated code, compiled assets that tests import. CI rebuilds it after restoring the cache. Put that build in [`[setup]`](configuration.md#setup), which runs before every task and ad-hoc command, after the dependency restore, and shows its output only when it fails:

```toml
[setup]
run = "npm run build:packages"
```

Don't rely on output an earlier run left behind. A slot keeps ignored files between runs, so without this step a task can pass on a slot that a previous run happened to prepare, and fail on a fresh one. A task that doesn't need the build can skip it with `setup = false`.

### Find missing system packages by running the code

The generated Dockerfile starts from your toolchain's official image. Before trusting the whole suite, run one test file from each part of it and look for:

- **Loader errors** (`error while loading shared libraries`). Electron, Chromium and other prebuilt binaries need their libraries even when a test only runs them headless or as plain Node.
- **`command not found`** for tools your scripts call.
- **`setlocale` warnings.** A test configuration that sets `LANG` or `LC_ALL` needs that locale generated in the image.

Add what's missing to the Dockerfile with `apt-get install`, and commit it.

### Turn on `git_seed` if anything asks git about the checkout

That includes build stamping, version-from-git tools, `git ls-files`-based checks, and "only changed files" modes. Without [`git_seed`](running.md#git-on-the-runner) they fail only remotely, often with an error that doesn't mention git. The seeded repository holds one commit, so tools that need history (tags, `HEAD~1`, blame) still won't work.

## Sizing

### Know each heavy job's peak memory

By default a VM runs four jobs, and jobs in shared slots draw from one memory pool: the VM's memory less a reserve for the runner itself, about 14 GiB on a 16 GB VM. A job that needs a large share should take a whole VM:

```toml
[tasks.typecheck]
run = "npm run typecheck"
exclusive = true
```

A command that runs several heavy processes at once can outgrow even a whole VM. Run them one after another, or use larger VMs.

### Match memory per CPU to your project

Compute-optimized instance types such as c6a and c7a have 2 GB of memory per vCPU. General-purpose types such as m6a and m7a, and GitHub's standard Linux runners, have 4 GB. A test suite tuned on CI can be memory-tight on compute-optimized VMs. Rather than naming types, a runner can [describe the VM it needs](aws.md#describe-the-vm-instead-of-naming-types) (`vcpus = 8`, `min_memory_gib = 32`), and spotexec launches whichever qualifying type is cheapest.

### Leave worker counts to your tools

A container sees only its slot's CPUs, so tools that size themselves by core count start the right number of workers on their own. Vitest (through Node's `os.availableParallelism()`), Go's `GOMAXPROCS` and `make -j$(nproc)` all see the slot's share. Hard-coded counts (`-j8`, `workers: 8`) still oversubscribe a slot; use `$SPOTEXEC_CPUS` for those.

### Run the whole suite on one warm VM, not sharded like CI

CI shards a suite because every CI job starts on a fresh machine anyway. On spotexec, a warm VM is the cheap part: each extra VM costs a cold start (boot, image, dependency restore, sync and setup) and then bills its idle timeout. Run the whole suite as one `exclusive` task, and let the test runner spread across all of the VM's CPUs:

```toml
[tasks.test-all]
run = "npx vitest run {args}"
exclusive = true
timeout = "30m"
```

Shard only a suite that runs many times longer than a cold start, with the test runner's own shard option in a [sharded task](configuration.md#tasks) (`shards = 4` and `--shard={shard}/{shards}`). Parts beyond the free slots launch VMs, each paying the cold start and the per-run setup, so more shards can be slower as well as costlier. [When sharding pays](scaling.md#when-sharding-pays) has the trade-off, and [the Daintree case study](case-study-daintree.md#one-vm-per-part-the-lesson) shows it on a real suite. Jobs your CI runs separately can still run side by side as the parts of a `parallel` task, which ends with a [summary of every part](agents.md#what-an-agent-sees); `--group` prints each part's output as one block when it finishes.

## Checking it

- **Make every task say what it ran.** A loop over an empty list exits 0 without output, which looks like a pass. Print a line per unit of work, and fail when there's nothing to do.
- **Test on a fresh slot before trusting a pass.** `spotexec down` and a rerun proves a task doesn't depend on files an earlier run left in its slot.
- **Check for fixed ports and Docker.** Tests that bind fixed ports can collide with another slot's on the same VM; [`network = "private"`](environments.md#settings) in `[container]` gives each run a network of its own. Tests that start their own containers need [`docker = true`](environments.md#docker-inside-the-container), which gives the command the runner's Docker daemon, and with it root on the runner.

## Debugging a runner

- **`spotexec status`** shows each VM's load and free memory, what every slot is running, and idle timers. Load far above the vCPU count, or free memory near zero, explains slow or lost runs.
- **A probe run** shows what a command actually sees: `sx -- 'nproc; echo $SPOTEXEC_MEMORY; cat /etc/hosts; git rev-parse HEAD'`. Add `--exclusive` to compare with a whole VM.
- **`spotexec exec -- <command>`** runs in the slot your worktree used last, without syncing, to look at what a failed run left behind.
- **`--events-file run.ndjson`** records timings for every phase (manifest, sync, image, dependencies, run, write-back) when a run is slower than expected.
- **SSH into a fleet VM** with your machine's generated key: `ssh -i ~/.config/spotexec/keys/id_ed25519 ubuntu@<address from spotexec status>`. There:
  - `sudo journalctl -k | grep -iE 'oom|killed process'` lists out-of-memory kills, with each process's memory at the time;
  - `cat /sys/fs/cgroup/spotexec.slice/memory.max` shows the shared slots' memory pool.
