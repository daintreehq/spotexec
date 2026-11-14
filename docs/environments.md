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
# user = "root"                       # "runner" (default: files keep the right owner) or "root"
# timeout = "20m"                     # pull/build limit
```

- **Paths and ports.** Write-back, path rewriting and snapshots work unchanged. Containers use host networking, so `SPOTEXEC_PORT_BASE` still applies.
- **Control.** Docker always runs as the runner user, so Ctrl-C and timeouts can always stop a container. `container.options` may not set `--name`, `--rm`, `--detach`, `--entrypoint`, `--user` or `--workdir`, because spotexec relies on those to find, stop and clean up containers.
- **Caching.** The dependency cache is keyed by the image's immutable ID, so a moving tag like `node:22` never restores modules built for an older image.

## Dependency cache

With `[deps]` configured, each slot's `cache_dirs` are restored from a cache keyed by:
- the lockfile contents and the install command;
- the cache dirs;
- the runner's OS and architecture;
- in a [project container](#project-containers), the image's immutable ID;
- otherwise, the runner's configured AMI, bootstrap scripts and `remote_path_setup`.

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
