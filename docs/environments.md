# Toolchains, containers and caches
## Project containers

`[container]` runs every command, and the dependency install, inside a Docker image on the runner. The image carries the project's toolchain, so the runner itself only needs Docker; on AWS, spotexec installs Docker for you.

### Set up, edit, build

`spotexec init` sets up the container for you when it detects a language toolchain (Node.js or Bun, Python, Go, Rust, Java): it writes `.spotexec/Dockerfile` and points `[container]` at it. AWS VMs boot plain Ubuntu, so this is how they get your toolchain. It skips the container with `--no-container`, when every configured runner is an SSH machine (which uses its own toolchain), and when it finds no toolchain. `spotexec doctor` warns about a toolchain project with no container on an AWS pool.

```sh
spotexec init                        # .spotexec.toml, plus .spotexec/Dockerfile for a detected toolchain
spotexec container init              # or just the Dockerfile and [container] (creating .spotexec.toml if needed)
$EDITOR .spotexec/Dockerfile         # add the system packages and tools your commands need
spotexec container build -r aws      # build it on a runner now; on AWS, push it to ECR
sx pnpm test                         # every command runs in the image (the first one builds it if needed)
```

The Dockerfile is commented and pinned as precisely as the repository allows:

| Stack | Base image |
| --- | --- |
| Node.js (npm, pnpm, Yarn) | `node:<version>-bookworm`. The version comes from `.node-version`, `.nvmrc` (including `lts/<name>`), `.tool-versions` or `volta.node`, exact where they give one, else the major of `engines.node`, else 24. The package manager is pinned from `packageManager`: pnpm and npm with `npm install -g`, Yarn through Corepack. Without one, pnpm's major follows the lockfile format and Yarn is 1. |
| Bun | `oven/bun:<version>`, from `packageManager`, `.bun-version` or `.tool-versions`, else `1` |
| Python with uv | `python:<version>-bookworm` plus the `uv` binary. The version comes from `.python-version` or `.tool-versions` (exact where given), else the lower bound of `requires-python`, else 3.12. uv is pinned when `[tool.uv] required-version` names one version. |
| Python | `python:<version>-bookworm`, found the same way |
| Go | `golang:<version>-bookworm`, from go.mod's `toolchain` line, else its `go` line |
| Rust | `rust:<version>-bookworm`, with a numeric version from `rust-toolchain(.toml)` (otherwise `1`), plus clippy and rustfmt |
| Maven / Gradle | `eclipse-temurin:<major>-jdk`, from `.java-version`, `.sdkmanrc`, `.tool-versions`, or the build's release / toolchain setting, else 21; plus `maven` when there's no `./mvnw` (Gradle projects use `./gradlew`) |
| Anything else | `ubuntu:24.04` with build-essential (`container init` only) |

The image is built on the first toolchain in that order. When the repository also has a `go.mod` or a `uv.lock`, Go, or uv with its Python, is added to the image too; any other toolchain it uses is named in a comment and in the command's output, for you to add. The `[deps]` install that `init` writes uses the same package manager as the image (`packageManager` wins over a leftover lockfile).

The runner builds from the synced copy. The configured Dockerfile, its `<Dockerfile>.dockerignore` and the files in its directory and build context always sync, whether or not Git tracks or ignores them and even though `.spotexec/` is otherwise skipped, so this works in non-Git projects and with `sync.source = "directory"`. (A build context of `.` is the whole project, synced by the usual rules.) Inside them the usual exclusions still apply: `sync.exclude`, `.spotexecignore`, `deps.cache_dirs`, and built-in ones such as `node_modules/`. `spotexec sync-plan` shows what is sent. Commit the Dockerfile so everyone runs in the same image. `--path` writes it elsewhere, and `--force` overwrites an existing one.

`container build` syncs the worktree, then builds the image on the chosen runner, or pulls it for `image =`. On AWS it pushes the result to a private ECR repository, `spotexec/<repo-id>`, which keeps the newest 10 images, and waits for the push to finish. It skips the dependency install, and it reports build and push errors straight away. Without `container build`, the first `sx` command builds the image the same way, but pushes it in the background, holding the VM up until the push is done. Every later VM pulls the image from ECR instead of rebuilding it.

### Writing the Dockerfile

- **Don't copy your source in.** The worktree is mounted at the same path at run time, and dependencies (`node_modules`, `.venv`, …) come from the [dependency cache](#dependency-cache). Install tools, not code.
- **Commands run as the runner's non-root user,** with `HOME=/home/spotexec`, a per-slot directory mounted at run time that persists between runs. Install toolchains system-wide (for example under `/usr/local`), never under `/root` or `/home/spotexec`: anything built into `/home/spotexec` is hidden by the mount. Point tool caches at `$HOME` so they survive between runs; the Go and Rust scaffolds already do this.
- **Use the image's own `ENV`.** Inside the container, the runner's login profile and `remote_path_setup` don't apply. The image needs `/bin/sh`, and bash is used when present.
- **Rebuilds are content-addressed.** The image is tagged by the content of the Dockerfile and its build context (default: the Dockerfile's directory). It is rebuilt only when those change. `.dockerignore` applies exactly as in `docker build`: `<Dockerfile>.dockerignore` wins over `<context>/.dockerignore`, and ignored files are neither sent nor hashed. With `context = "."` and an ignore file that keeps only manifests, editing source never triggers a rebuild.

### Settings

```toml
[container]
dockerfile = ".spotexec/Dockerfile"   # built on the runner; or:
# image = "node:22-bookworm"          # pulled as-is
# context = "."                       # default: the Dockerfile's directory
# options = ["--shm-size=1g"]         # extra `docker run` arguments
# network = "private"                 # "host" (default: the runner's network) or "private" (one per run)
# user = "root"                       # "runner" (default: files keep the right owner) or "root"
# docker = true                       # let commands start containers of their own; see below
# timeout = "20m"                     # pull/build limit
```

- **Paths and ports.** Write-back, path rewriting and snapshots work unchanged. By default containers use host networking, so `SPOTEXEC_PORT_BASE` still applies, services running on the runner are reachable on `localhost`, and names resolve from the runner's `/etc/hosts` (on `aws-spot` VMs, `localhost` is both `127.0.0.1` and `::1`).
- **Fixed ports.** Every job on a machine shares host networking, so two jobs whose tests bind the same fixed port (a test server on `3000`, a suite's own Postgres on `5432`, an OAuth callback on a registered port) collide when they land on the same VM: one fails with `EADDRINUSE`, or worse, talks to the other job's server. Tests that bind `SPOTEXEC_PORT_BASE` or an ephemeral port (`0`) never do. When the tests can't be changed, set `network = "private"`: each command (and the dependency install) then runs in a network of its own on Docker's default bridge, with its own `localhost` and every port free, while outbound connections (package registries, APIs) work as before. What it gives up: services on the runner itself aren't reachable on `localhost`, and `localhost` inside the container comes from Docker's `/etc/hosts`, not the runner's. Each command is its own container, so a server started by one command isn't reachable from the next; start it in the same command as the tests that use it, as a test suite normally does. It separates ports, not traffic: containers on the bridge can still reach one another by IP, and a fixed port published with `-p` in `options` still collides. Names resolve through the runner's upstream DNS servers (Docker reads past a `systemd-resolved` stub); add `--dns` to `options` if the runner's resolver is reachable only on its own `localhost`. A `--network` or `--net` in `options` wins over `network`.
- **Ownership.** With `user = "root"`, files the container creates are given back to the runner user when it exits. If the run is aborted first (client gone, `spotexec unlock`), the slot's next run does it before starting.
- **CPUs.** On a Linux runner, commands and the dependency install are pinned (`--cpuset-cpus`) to the slot's share of the VM: `SPOTEXEC_CPUS` CPUs, a separate block for each concurrent job (the job with `SPOTEXEC_SLOT=2` on an 8-vCPU, 4-slot VM gets CPUs 2-3). So `nproc`, Node's `os.availableParallelism()`, Go's `GOMAXPROCS`, `make -j$(nproc)` and test runners that size their workers from these use the job's share instead of the whole VM; tools that count every CPU on the machine (Node's `os.cpus()`) still see them all. `--exclusive` runs and runners with one slot aren't pinned. When vCPUs don't divide evenly, the leftover CPUs go to no slot; with more slots than vCPUs, each job gets one CPU, and jobs may share one. A pinned job can't borrow the cores of idle slots, so give a job that needs more `--exclusive` (or use fewer `max_slots`). Setting `--cpuset-cpus`, `--cpus`, `--cpu-quota`, `--cpu-period` or `--cgroup-parent` in `options` turns the pinning off (`SPOTEXEC_CPUS` doesn't change). The pinning also stays off, and the run goes ahead unpinned, wherever spotexec can't tell that a cpuset is valid for the Docker daemon: macOS runners (Docker Desktop's VM has its own CPUs), a daemon on another machine (`DOCKER_HOST` or a Docker context), a daemon without cpuset support, or one whose CPU count differs from the runner's. A `container_cpus` event (with `--json` or `--events-file`) says why, once per run. Every config that runs on the same machine should give it the same `max_slots`, or their jobs' blocks can overlap.
- **Memory.** On spotexec's own Linux VMs, the containers of jobs that share a VM run in one memory pool (a cgroup, through `--cgroup-parent`) capped at the VM's memory less a reserve for the runner itself: 1/16 of it, at least 512 MiB and at most 2 GiB (about 14.6 GiB of a 15.6 GiB c7a.2xlarge). Jobs can burst past their fair share; if together they reach the cap, the kernel kills the largest process among them, usually the job that spiked, which ends with an `OOM_KILLED` warning (usually exit `137`), while the other jobs, the helper and Docker carry on. Without the pool, one job's spike could exhaust the VM until it stopped answering and every slot's run on it was lost. The container itself has no memory limit, on purpose: runtimes size themselves from a container's own limit, and Node caps its default heap near 2 GiB under any limit at all (half what it takes on the same VM without one), so a limit there breaks Node tools that fit fine without it. A job alone on the VM (`--exclusive`, or a runner with one slot) runs with no limit at all, as before. `SPOTEXEC_MEMORY` is the fair share (the pool ÷ `max_slots`), for sizing heaps and worker counts; nothing enforces it. The dependency install runs in the pool too, and one that's killed fails with `DEPS_FAILED` saying so. Setting `--memory`, `-m`, `--memory-swap`, `--memory-reservation`, `--kernel-memory`, `--oom-kill-disable`, `--oom-score-adj` or `--cgroup-parent` in `options` leaves the container out of the pool (and a container `--memory` limit shrinks Node's default heap as above). The pool is only set up (once per VM, with sudo) on spotexec's own VMs with cgroup v2 and Docker's `systemd` or `cgroupfs` cgroup driver; elsewhere (SSH runners, macOS, a daemon on another machine) jobs run without it and a `container_memory` event says why. Without a container, nothing limits a job's memory.
- **Control.** Docker always runs as the runner user, so Ctrl-C and timeouts can always stop a container. `container.options` may not set `--name`, `--rm`, `--detach`, `--entrypoint`, `--user` or `--workdir`, because spotexec relies on those to find, stop and clean up containers.
- **Caching.** The dependency cache is keyed by the image's immutable ID, so a moving tag like `node:22` never restores modules built for an older image.

### Docker inside the container

Test suites that start containers of their own (Testcontainers, `docker compose up` in a test setup, integration tests against a real Postgres) need a Docker daemon. `docker = true` gives the project container one: the runner's own daemon, through a socket at `/var/run/docker.sock` that belongs to the run.

```toml
[container]
dockerfile = ".spotexec/Dockerfile"
docker = true
```

- **What the command sees.** `DOCKER_HOST=unix:///var/run/docker.sock`, so the image's own `ENV` can't point Docker elsewhere, and `TESTCONTAINERS_HOST_OVERRIDE=localhost`, because with host networking the ports Testcontainers maps are on this machine, not at the gateway it would otherwise guess. Either can be overridden in `[env]`. The dependency install gets the same socket, and a `COMPOSE_PROJECT_NAME` of its slot's own. Docker's SDKs (Testcontainers for Java, Go, Python and Node) need nothing more; the `docker` CLI, and for Compose its plugin, have to be in the image (the `docker:cli` image has both, to copy from).
- **Paths.** The workspace is mounted at the same absolute path as on the runner, so `docker run -v "$PWD:/src"` and Compose's relative bind mounts work as written. Files those containers create in the workspace are given back to the runner user afterwards, as with `user = "root"`.
- **Scoped to the slot.** Every container, network and volume created through the socket is labelled with the slot and the run (`io.spotexec.slot`, `io.spotexec.run`). When the command ends (normally, on a timeout or Ctrl-C), the run's containers are removed with their anonymous volumes, then its networks and its named volumes; if the run is aborted before that (client gone, `spotexec unlock`, a helper that died), the slot's next run does it before starting. Results aren't collected while any of them is still there. A container removed through the socket (`docker rm`, Compose's `down`) takes its anonymous volumes with it, as `docker run --rm` does, since nothing could find them afterwards. Images the run pulls or builds stay, as a cache. Containers that mount `/var/run/docker.sock` themselves (Testcontainers' Ryuk, a Compose service driving Docker) get the run's socket too, so what they create is labelled as well.
- **Names.** `COMPOSE_PROJECT_NAME` differs per slot, so Compose's containers, networks and volumes don't collide between concurrent jobs, and Testcontainers names its containers at random. A fixed name (`docker run --name postgres`, `container_name:` in a Compose file, a fixed host port) is shared by every job on the runner, and concurrent jobs collide on it.
- **Limits.** Containers the command starts run beside the project container, not in it: they aren't pinned to the slot's CPUs or held to the memory pool. A volume a container creates implicitly by name (`docker run -v data:/data` without `docker volume create`) isn't labelled and outlives the run; Compose creates its volumes explicitly, so they are. Anything that reaches the daemon some other way (another socket path, a mount of `/var/run` itself, a `DOCKER_HOST` set in `[env]`) isn't labelled or cleaned up.
- **Security.** Access to the Docker daemon is root on the runner: the command can start a `--privileged` container, mount `/`, and see or stop other jobs' containers. The labels are for cleanup, not isolation. Turn this on only for repositories you'd trust with root on the runner; on a shared `ssh` runner, that includes everyone who can change `.spotexec.toml`.

## Dependency cache

With `[deps]` configured, each slot's `cache_dirs` are restored from a cache keyed by:
- the contents of the `lockfiles` and the install command;
- the cache dirs;
- the runner's OS and architecture;
- in a [project container](#project-containers), the image's immutable ID;
- otherwise, the runner's configured AMI, bootstrap scripts and `remote_path_setup`.

`lockfiles` may hold globs, so the key can cover everything an install depends on, not just the package manager's lockfile, much like `hashFiles()` in a GitHub Actions cache key: workspace manifests, postinstall scripts and native addon sources.

```toml
[deps]
lockfiles = [
  "package-lock.json",
  "packages/*/package.json",
  "scripts/patchNodePty*.cjs",
  "electron/native/*/binding.gyp",
  "electron/native/*/package.json",
  "electron/native/*/src/**",
]
```

Patterns are expanded on your machine against the synced files (so they never match ignored files, `cache_dirs` contents or symlinks), and the runner hashes exactly that list. `**` is recursive here, unlike in `--pull`. A pattern matching nothing only warns. See [`[deps]`](configuration.md#dependencies) for the full rules, and `spotexec sync-plan` for the list a run would key on.

`spotexec init` lists the root `node_modules` and each workspace's in `cache_dirs`, from package.json `workspaces` (npm, Yarn, Bun) or pnpm-workspace.yaml `packages`: the workspace glob `packages/*` becomes `packages/*/node_modules`. `cache_dirs` globs match one directory per `*`, so a workspace pattern they can't express (`**`, `{a,b}`) is left out, and init names it for you to add by hand. Update `cache_dirs` when the workspaces change.

With a container, native modules never cross toolchain versions. Without one, pin tool versions in your bootstrap: upgrading Node on an `ssh` host, or a newer Ubuntu image behind `ami = "auto"`, doesn't change the key.

- **On a runner,** caches are hydrated into the slot with `reflink` where the filesystem supports it (XFS, Btrfs, APFS), else `copy`. `hardlink` is opt-in, because shared inodes let one slot corrupt another's `node_modules`.
- **Across Spot VMs,** the first VM to install a dependency set uploads it as one compressed archive to a private S3 bucket in your account, `spotexec-cache-<account>-<region>`. The bucket is created on the first `[deps]` run, with S3's default Block Public Access and a 14-day expiry. Without the S3 permissions, set `cache_bucket = "off"`. Later VMs download it instead of installing.
- **VMs never hold AWS credentials.** The CLI hands each one presigned URLs, and only for a bucket your account owns.
- **Synced files are never touched.** A tracked file that lives inside a cache dir is left alone by hydration and cache clearing.
- **Python virtualenvs are relocated.** A virtualenv records the absolute path it was built at (console-script shebangs, `activate`, editable installs of your own packages). When a cached `.venv` (the `uv.lock` preset, or any `cache_dirs` entry holding a `pyvenv.cfg`) is hydrated into another slot or VM, those paths are rewritten to that slot's workspace, so `.venv/bin/pytest` runs the slot's own interpreter against its own source.

## Baked images

```sh
spotexec image bake aws      # build an AMI from the runner's bootstrap
spotexec image list aws
spotexec image clear aws
```

`image bake` boots an on-demand builder from the current Ubuntu image and runs `bootstrap_script`. It installs Docker and buildx, applies the Ubuntu security updates published since that image, pre-pulls the project's public container image (`--pull IMAGE` adds more), and runs `user_bootstrap_script`. Then it sanitizes the instance (cloud-init state, SSH host keys, logs, the watchdog) and snapshots it. It usually takes 10 to 15 minutes, plus your bootstrap.

From then on the fleet boots from the newest bake whose bootstrap hash matches, and skips the bootstrap, so a new VM is ready in roughly bare boot time. The per-instance setup (watchdog, SSH key, timers) still runs on every VM.

Bakes are shared across machines. A bake holds no keys or machine state, so every machine whose runner has the same name and bootstrap uses it, and with the config copied by `spotexec setup`, one `image bake` speeds up the whole stack. `image list` shows the runner's bakes from every machine in the account, which machine made each, and which one new VMs boot from.

Changing either bootstrap script, `arch` or `user` makes old bakes stop matching; run `image bake` again.

VMs don't run OS updates (they're short-lived, and apt would compete with your commands), so a bake keeps the packages of the day it was made. Each bake records the Ubuntu image it was built from, and a bake goes stale:
- **When Canonical publishes a newer base image**, the bake is still used but reported as stale, for a week. After that, new VMs stop using it. (A bake made by an older spotexec, which didn't record its base, gets the same week.)
- **After 30 days**, new VMs stop using it whatever the base.

A VM that skips a stale bake boots the current Ubuntu image and runs the full bootstrap, as if there were no bake, so it is up to date but slower to start. Run `image bake` again to get fast boots back. `spotexec doctor` and `image list` show each bake's base image and age, and say when it is stale. Without a usable bake (and unless the runner pins an `ami`), VMs boot the current Ubuntu image.

Each bake prunes the runner's bakes that nothing boots any more, whichever machine made them: all but the two newest of each bootstrap (once the bake that replaced them has been out an hour or more), and any past 31 days. So a wiped machine's bakes don't pile up. `image clear` removes the bakes this machine made or would boot, plus expired ones; other machines' bakes of a different bootstrap stay unless you pass `--all`. Neither ever deletes a bake that a VM is booting from at that moment, and a launch that picked a bake just as it was deleted boots the current Ubuntu image instead.
