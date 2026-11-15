# Development

Work happens on `develop` (GitFlow); releases are tagged on `main` and published automatically. See [CONTRIBUTING.md](../CONTRIBUTING.md) for the branch model and release process.

```sh
make build          # helpers for all platforms, then bin/spotexec + bin/sx
make test           # unit tests (go test ./...; add -race locally)
make vet
make completions    # completion scripts for spotexec and sx into completions/
make e2e            # builds, then runs all three end-to-end suites:
                    #   scripts/e2e.sh         SSH runner: sync, write-back, conflicts, tasks, deps, pull, exec, watchdog
                    #   scripts/e2e-pool.sh    Spot fleet scenarios on the Docker simulator
                    #   scripts/e2e-daemon.sh  pool manager scenarios on the simulator
```

The **`sim` provider** runs a Spot fleet as Docker containers on your machine, so the full lifecycle can be tested without an AWS account.
- Each instance is a container running sshd and booting the same cloud-init user-data.
- `spotexec sim interrupt` delivers an interruption notice; `spotexec sim rebalance` sends a rebalance recommendation.
- `spotexec sim build` builds the simulator image.
- `SPOTEXEC_SIM_QUOTA=<vCPUs>` simulates the account's Spot vCPU quota: each simulated instance counts 4 vCPUs, across every runner and owner labelled with the same `SPOTEXEC_SIM_ACCOUNT` (default `local`).
- Setting `SPOTEXEC_DAEMON_FAST=1` shrinks the pool manager's timings for tests.
- With a plain `go build` (no embedded helpers), `SPOTEXEC_HELPER_DIR` points at a directory of helper binaries.

To try a build against real runners, `make build`, then run `bin/sx` by its absolute path from a project. The helper uploads itself, so running VMs pick up the new one on their next run. Changes to first-boot user-data (`internal/cloudinit`) only reach new VMs, so run `spotexec down` first.

The code is organized as:

| Package | Role |
| --- | --- |
| `cmd/spotexec`, `internal/cli` | Command surface. |
| `internal/client` | Run orchestration, pool planning, queueing, write-back, guards. |
| `internal/helper`, `cmd/spotexec-helper`, `internal/helperbin` | The remote helper (slots, sync, deps, exec, containers, snapshots) and its embedded builds. |
| `internal/transport`, `internal/protocol` | SSH ControlMaster transport and the framed stdio RPC. |
| `internal/provider` | `ssh`, `aws-spot` (hand-signed EC2/S3/ECR/SSM APIs), `sim`. |
| `internal/cloudinit` | First-boot user-data: watchdog, key, bootstrap. |
| `internal/daemon` | The optional pool manager. |
| `internal/manifest`, `internal/ignore`, `internal/dockerignore`, `internal/fsid`, `internal/depskey` | Worktree manifests, ignore semantics, safe path handling, dependency cache keys. |
| `internal/gitseed` | What `[sync] git_seed` sends: HEAD's commit and trees, and the HEAD versions of changed files. |
| `internal/config`, `internal/hooks`, `internal/ui` | Config, agent hooks, output. |
