# Toolchains, containers and caches
## Project containers

`[container]` runs every command, and the dependency install, inside a Docker image on the runner. The image carries the project's toolchain, so the runner itself only needs Docker; on AWS, spotexec installs Docker for you.

### Scaffold, edit, build

```sh
spotexec container init              # writes .spotexec/Dockerfile and adds [container] to .spotexec.toml (creating it if needed)
$EDITOR .spotexec/Dockerfile         # add the system packages and tools your commands need
spotexec container build -r aws      # build it on a runner now; on AWS, push it to ECR
sx pnpm test                         # every command now runs in the image
```

`container init` detects the stack and writes a commented Dockerfile with a pinned base image:

| Stack | Base image |
| --- | --- |
| Node.js (npm, pnpm, Yarn) | `node:<major>-bookworm`. The major version comes from `.nvmrc`, `.node-version` or `engines.node`; pnpm is pinned from `packageManager`. |
| Bun | `oven/bun:1` |
| Python with uv | `ghcr.io/astral-sh/uv:python<x.y>-bookworm`, with the version from `.python-version` |
| Python | `python:<x.y>-bookworm` |
| Go | `golang:<x.y>-bookworm`, with the version from `go.mod` |
| Rust | `rust:<version>-bookworm`, with a numeric version from `rust-toolchain(.toml)` (otherwise `1`), plus clippy and rustfmt |
| Maven / Gradle | `eclipse-temurin:21-jdk`, plus `maven` when there's no `./mvnw` (Gradle projects use `./gradlew`) |
| Anything else | `ubuntu:24.04` with build-essential |

`init` also stages the Dockerfile with `git add`. Untracked files under `.spotexec/` aren't synced, and the runner builds from the synced copy. Commit the file with the project; once Git tracks it, your edits sync like any other file. `--path` writes it elsewhere, and `--force` overwrites an existing one.

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

## Baked images

```sh
spotexec image bake aws      # build an AMI from the runner's bootstrap
spotexec image list aws
spotexec image clear aws
```

`image bake` boots an on-demand builder and runs `bootstrap_script`. It installs Docker and buildx, pre-pulls the project's public container image (`--pull IMAGE` adds more), and runs `user_bootstrap_script`. Then it sanitizes the instance (cloud-init state, SSH host keys, logs, the watchdog) and snapshots it.

From then on the fleet boots from the newest bake whose bootstrap hash matches, and skips the bootstrap, so a new VM is ready in roughly bare boot time. The per-instance setup (watchdog, SSH key, timers) still runs on every VM.

Bakes are shared across machines. A bake holds no keys or machine state, so every machine whose runner has the same name and bootstrap uses it, and with the config copied by `spotexec setup`, one `image bake` speeds up the whole stack. `image list` and `image clear` show and delete only this machine's own bakes.

Changing either bootstrap script, `arch` or `user` makes old bakes stop matching; run `image bake` again. Only the two newest bakes per runner are kept, and older images and their snapshots are deleted.
