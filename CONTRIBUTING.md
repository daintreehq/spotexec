# Contributing

## Branches (GitFlow)

| Branch | Purpose |
| --- | --- |
| `develop` | Integration branch and the default. Feature work lands here. |
| `main` | Released code only. Every commit on `main` is, or is about to be, a tagged release. |
| `feature/<name>` | New work, branched from `develop` and merged back by pull request. |
| `release/<version>` | Optional release stabilisation, branched from `develop` and merged into `main`, then back into `develop`. |
| `hotfix/<name>` | Urgent fixes, branched from `main` and merged into `main`, then back into `develop`. |

CI runs on every push to `main` and `develop`, and on every pull request into `main`, `develop` or a `release/*` branch. Every job runs on Ubuntu: build, vet, a vulnerability scan for each release platform, race tests, the Docker end-to-end suites and a smoke test of the release artifacts. Nothing runs natively on macOS (macOS runners queue for minutes), so the macOS tests are run on a Mac before each release; see [Releasing](#releasing).

## Development

```sh
make build          # embedded helpers for all platforms, then bin/spotexec + bin/sx
make test           # unit tests
go test -race ./... # what CI runs (on Linux); run it on a Mac too when you touch platform-specific code
make e2e            # end-to-end suites (SSH runner, Spot fleet simulator, pool manager); needs Docker
```

See [Development](docs/development.md) for the simulator and the package layout.

## Releasing

Releases are cut from `main` by pushing a version tag. Everything after that is automatic.

```sh
git checkout main && git pull
git merge --no-ff develop            # or a release/x.y.z or hotfix/* branch
# on a Mac, before pushing main and tagging: the macOS checks below
git push origin main
git tag -a v0.2.0 -m "spotexec v0.2.0"
git push origin v0.2.0
git checkout develop && git merge main && git push origin develop   # keep develop in step
```

CI and the Release workflow run only on Linux. They cross-compile the macOS binaries and require each to be built by the exact Go patch on go.mod's `go` line, but never run them, and the race tests never run on macOS there. So before tagging, run these on a Mac, from the commit on `main` you are about to tag:

```sh
export GOTOOLCHAIN=go$(go list -m -f '{{.GoVersion}}')   # see below
make build && go vet ./... && go test -race ./...     # the race tests against the darwin code paths
goreleaser release --snapshot --clean --skip=publish && SMOKE_PACKAGES=0 ./scripts/smoke-release.sh   # runs the darwin archive for this Mac
```

Run them from the default, case-insensitive volume: the tests for names that differ only in case (`internal/client/collision_test.go`) skip on a case-sensitive filesystem, so they skip on Linux CI and run only here. The smoke script tests the archive for the Mac's own architecture (`darwin_arm64` on Apple silicon), so the other darwin archive is only cross-compiled and version-checked. `SMOKE_PACKAGES=0` skips the Linux package installs, which CI already covers.

CI also builds a snapshot of the release on every push and pull request and smoke-tests what users install on Linux (the Linux archive, `sx`, completions and the Linux packages). To do the same locally: `export GOTOOLCHAIN=go$(go list -m -f '{{.GoVersion}}') && goreleaser release --snapshot --clean --skip=publish && ./scripts/smoke-release.sh`. On a Mac that smoke-tests the darwin archive rather than the Linux one. The `GOTOOLCHAIN` pin matters when your default Go is newer: every binary must be built by exactly the Go release on go.mod's `go` line ([scripts/check-go-version.sh](scripts/check-go-version.sh)), the patch CI scans with govulncheck, so moving to a new Go patch means raising that line. CI and the Release workflow pin GoReleaser to one version (in both workflow files) so an upgrade can't change a release unannounced; bump it in both places together.

The tag starts the [Release workflow](.github/workflows/release.yml). It refuses a tag that isn't on `main`, builds the embedded helpers, runs vet and the race tests on Linux, and then runs [GoReleaser](.goreleaser.yaml), which:
- builds `spotexec` for macOS and Linux (amd64, arm64) with the remote helpers embedded;
- publishes a GitHub release with tarballs, `.deb`/`.rpm`/`.apk` packages, checksums and a changelog;
- updates the formula in [daintreehq/homebrew-tap](https://github.com/daintreehq/homebrew-tap), so `brew upgrade spotexec` picks the release up.

The tap is written with a deploy key that can only write to that repository. Its private half is the `HOMEBREW_TAP_DEPLOY_KEY` secret on this repository. To rotate it:
1. Generate a new key pair.
2. Add the public key as a write-enabled deploy key on `homebrew-tap`.
3. Replace the secret, then delete the old key.

Versions follow [semantic versioning](https://semver.org). Commit subjects use conventional prefixes (`feat:`, `fix:`, `docs:`, `build:`, `refactor:`, `test:`). Release notes leave out `docs`, `test`, `chore`, `style`, `ci`, `build` and `release` commits, scoped or not (`test(transport): ...`), and merge commits.
