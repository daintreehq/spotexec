# Contributing

## Branches (GitFlow)

| Branch | Purpose |
| --- | --- |
| `develop` | Integration branch and the default. Feature work lands here. |
| `main` | Released code only. Every commit on `main` is, or is about to be, a tagged release. |
| `feature/<name>` | New work, branched from `develop` and merged back by pull request. |
| `release/<version>` | Optional release stabilisation, branched from `develop` and merged into `main`, then back into `develop`. |
| `hotfix/<name>` | Urgent fixes, branched from `main` and merged into `main`, then back into `develop`. |

CI (build, vet, race tests on Linux and macOS, plus the Docker end-to-end suites on Linux) runs on every push to `main` and `develop`, and on every pull request into `main`, `develop` or a `release/*` branch.

## Development

```sh
make build          # embedded helpers for all platforms, then bin/spotexec + bin/sx
make test           # unit tests
go test -race ./... # what CI runs
make e2e            # end-to-end suites (SSH runner, Spot fleet simulator, pool manager); needs Docker
```

See [Development](docs/development.md) for the simulator and the package layout.

## Releasing

Releases are cut from `main` by pushing a version tag. Everything after that is automatic.

```sh
git checkout main && git pull
git merge --no-ff develop            # or a release/x.y.z or hotfix/* branch
git push origin main
git tag -a v0.2.0 -m "spotexec v0.2.0"
git push origin v0.2.0
git checkout develop && git merge main && git push origin develop   # keep develop in step
```

CI also builds a snapshot of the release on every push and pull request and smoke-tests what users install (the archive, `sx`, completions and the Linux packages). To do the same locally: `export GOTOOLCHAIN=go$(go list -m -f '{{.GoVersion}}') && goreleaser release --snapshot --clean --skip=publish && ./scripts/smoke-release.sh`. The `GOTOOLCHAIN` pin matters when your default Go is newer: every binary must be built by exactly the Go release on go.mod's `go` line ([scripts/check-go-version.sh](scripts/check-go-version.sh)), the patch CI scans with govulncheck, so moving to a new Go patch means raising that line. CI and the Release workflow pin GoReleaser to one version (in both workflow files) so an upgrade can't change a release unannounced; bump it in both places together.

The tag starts the [Release workflow](.github/workflows/release.yml). It refuses a tag that isn't on `main`, builds the embedded helpers, runs vet and the race tests, and then runs [GoReleaser](.goreleaser.yaml), which:
- builds `spotexec` for macOS and Linux (amd64, arm64) with the remote helpers embedded;
- publishes a GitHub release with tarballs, `.deb`/`.rpm`/`.apk` packages, checksums and a changelog;
- updates the formula in [daintreehq/homebrew-tap](https://github.com/daintreehq/homebrew-tap), so `brew upgrade spotexec` picks the release up.

The tap is written with a deploy key that can only write to that repository. Its private half is the `HOMEBREW_TAP_DEPLOY_KEY` secret on this repository. To rotate it:
1. Generate a new key pair.
2. Add the public key as a write-enabled deploy key on `homebrew-tap`.
3. Replace the secret, then delete the old key.

Versions follow [semantic versioning](https://semver.org). Commit subjects use conventional prefixes (`feat:`, `fix:`, `docs:`, `build:`, `refactor:`, `test:`). Release notes leave out `docs`, `test`, `chore`, `style`, `ci`, `build` and `release` commits, scoped or not (`test(transport): ...`), and merge commits.
