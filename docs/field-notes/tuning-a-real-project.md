# Tuning a real project

`spotexec init` writes a working start: an install, a cache, tasks from the package scripts, and a Dockerfile pinned to the project's toolchain version. A real project usually needs more before its checks pass remotely the way they pass locally and in CI. This is the checklist the Daintree trial produced, roughly in the order the problems showed up.

## Setting up

1. **Start from CI, not from the package scripts.** The CI workflow is the most accurate description of what a clean machine needs. Read its install step and copy three things into `[deps]`:
   - **What it runs:** `npm ci` plus anything after it. Daintree's CI provisions the Electron binary after installing, so the spotexec install does too.
   - **What it caches:** every path, not just `node_modules`. Daintree's included each workspace's `node_modules` and the native addons' build output.
   - **What the cache key hashes:** CI's `hashFiles(...)` patterns translate directly into `lockfiles` globs. A key that leaves out the postinstall script or the addon sources restores stale native builds.

2. **Run what CI runs, with the same settings.** A task that runs something close to CI's command but not the same is the commonest source of "works in CI, fails on spotexec". Daintree's first lint task ran `eslint .`, which ran out of heap, while CI runs `lint:ratchet` with `--max-old-space-size=8192` and `ESLINT_CONCURRENCY=2`.

3. **Repeat source-derived build steps in every task.** Anything built from source rather than from the lockfile (workspace packages' `dist/`, generated code) can't live in the dependency cache, so CI rebuilds it after restoring the cache, and every task has to as well. Until #78 adds a shared setup step, prefix each task. Keep the step's output out of the command's output unless it fails:

   ```sh
   npm run prepare --workspaces --if-present >/tmp/prepare.log 2>&1 || { cat /tmp/prepare.log; exit 1; }; <the command>
   ```

4. **Find the image's missing system packages by running the code, not by reading it.** Run one test file from each corner of the suite and look for loader errors, `command not found` and locale warnings. Daintree's image needed 14 GUI and NSS libraries because one test runs the Electron binary (as plain Node, but it still links them), plus a generated `en_US.UTF-8` locale because the test config forces it. A `bash: warning: setlocale` line in the output means a locale is missing.

5. **Turn on `git_seed` if anything asks git about the checkout.** That includes build stamping, provenance or version tools, `git ls-files`-based checks, and "changed files" modes. Without it, these fail only remotely. The seed holds one commit, so tools that need history (tags, `HEAD~1`, blame) still won't work.

## Sizing

6. **Know each heavy job's peak memory.** By default a VM runs 4 jobs, and shared slots draw from one pool of the VM's memory less a reserve, about 14 GiB on a 16 GB VM. A job that needs a big share should take a whole VM with `exclusive = true`. Daintree runs typecheck, lint and the guards that way, as its CI gives each a runner of its own. A tool that runs several heavy processes at once can still outgrow a whole VM: Daintree's typecheck had to run its projects one at a time.

7. **Match memory per CPU to what the project expects.** Compute-optimized types (c6a, c7a) have 2 GB per vCPU. GitHub's standard Linux runners and general-purpose types (m6a, m7a) have 4 GB. A test suite tuned on CI can be memory-tight on c-family VMs. A runner can ask for the shape instead of naming types (`vcpus = 8`, `min_memory_gib = 32`), and spotexec launches whatever qualifying type is cheapest. That option needs `ec2:DescribeInstanceTypes`, which `spotexec setup` adds when re-run.

8. **Leave worker counts to the tools.** A container sees only its slot's CPUs, so tools that size themselves by core count (Vitest, Jest, Go, make with `$(nproc)`) start the right number of workers. Remove `--maxWorkers=$SPOTEXEC_CPUS`-style workarounds. Hard-coded counts (`-j8`, `workers: 8`) still oversubscribe a slot and should use `$SPOTEXEC_CPUS`.

9. **Shard the suite the way CI does.** Use the test runner's own shard flag in a task with `shards = N`. Pick N for the fleet: at most `max_instances × max_slots` parts can run at once (16 with the default fleet), and each part pays the per-run setup, so more shards isn't always faster. Run jobs that CI runs separately (Daintree's perf-liveness test) as their own part, next to the shards in a `parallel` task.

## Checking it

10. **Make every task say what it ran.** A loop over an empty list exits 0 silently. The first sequential typecheck "passed" in 36 seconds with no output, which could have been success or nothing at all. Print one line per unit of work, and fail when the list is empty.

11. **Test on a fresh slot before trusting a pass.** Slots keep ignored output between runs, so a task that forgets a build step can pass where an earlier run did the build and fail on a new VM. `spotexec down` and a rerun is the cheap way to prove a task stands on its own.

12. **Check the suite for fixed ports and Docker.** Tests that bind fixed ports collide when two slots share a VM (#79), and tests that start their own containers can't run inside a project container (#80). Both are open issues: until they're resolved, such suites need `exclusive` or have to stay local.

## Debugging a runner

- **`spotexec status`:** each VM's load and free memory, what every slot is running, and idle timers. A load far above the vCPU count, or free memory near zero, explains slow or lost runs.
- **A probe run:** `sx -- 'nproc; free -m; cat /etc/hosts; git rev-parse HEAD; node -e "..."'` shows what a command actually sees: CPUs, memory share (`$SPOTEXEC_MEMORY`), name resolution, git. Add `--exclusive` to compare with a whole-VM run.
- **`spotexec exec -- <cmd>`:** runs in the slot this worktree used last, without syncing. Use it to look at what a failed run left behind.
- **`--events-file run.ndjson`:** NDJSON timings for every phase (manifest, sync, image, deps, exec, write-back), useful when a run is slower than expected.
- **SSH into a VM:** fleet VMs accept the machine's generated key: `ssh -i ~/.config/spotexec/keys/id_ed25519 ubuntu@<ip from spotexec status>`. Then:
  - `sudo journalctl -k | grep -iE 'oom|killed process'` shows out-of-memory kills, with each process's memory at the time.
  - `cat /sys/fs/cgroup/spotexec.slice/memory.max` shows the shared slots' memory pool.
  - `cat /proc/$(docker inspect -f '{{.State.Pid}}' <container>)/cgroup` shows whether a container is in the pool.
- **Testing a spotexec change live:** `make build`, then call `bin/sx` by its absolute path from the project. The helper uploads itself, so running VMs pick up the new helper. Changes to first-boot user-data, such as #61's `/etc/hosts` fix, only apply to new VMs, so `spotexec down` first.
