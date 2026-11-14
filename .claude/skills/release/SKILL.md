---
name: release
description: Cut a spotexec release end to end with GitFlow. Picks the version, merges develop into main, waits for CI, tags, watches the Release workflow, checks the GitHub release and Homebrew tap, fixes failures and re-tags, then syncs develop. Use when the user asks to release, ship, publish, cut or tag a version, or to finish or retry a broken release.
---

# release

Ship a spotexec version from `develop` to users (GitHub release plus the Homebrew tap) and don't stop until it has landed or a human decision is needed.

## Input

An optional version (`v0.3.0`) or bump level (`patch`, `minor`, `major`). Without one, pick it from the commits since the last tag (see [Version](#1-version)). `--dry-run` runs the preflight and prints the plan without pushing anything.

## How releases work here

- **GitFlow.** `develop` is the integration branch. `main` holds released code only. Releases are tags on `main`. See `CONTRIBUTING.md`.
- **Workflows.**
  - CI (`.github/workflows/ci.yml`) runs build, vet and race tests on Linux and macOS for pushes to `main` and `develop`.
  - Release (`.github/workflows/release.yml`) runs on `v*` tags. It refuses a tag not on `main`, runs the tests, then GoReleaser (`.goreleaser.yaml`), which:
    - publishes the GitHub release (tarballs, deb/rpm/apk, checksums);
    - pushes `Formula/spotexec.rb` to `daintreehq/homebrew-tap`, using the `HOMEBREW_TAP_DEPLOY_KEY` deploy key secret.
- **Repo:** `daintreehq/spotexec`. **Tap:** `daintreehq/homebrew-tap`.

## Ground rules

- Commit messages and tags carry no AI attribution: no `Co-Authored-By`, no "generated with" lines.
- Never force-push `main` or `develop`. Never rewrite published history.
- **A version is burned once its formula reaches the tap.** Users may have installed it, and the formula pins checksums. After that point, never move or reuse the tag. Fix forward with the next patch version.
- Before the tap is updated, a failed release may be retried **at the same version** (see [Recovery](#recovery)).
- Wait on long operations without ending the turn. Use `gh run watch <id> --exit-status` under `timeout 590`, repeated until it finishes. Use `run_in_background` only when there is other work to do meanwhile.
- Use `rtk proxy` for any output you make claims about.

## Steps

### 0. Preflight

```sh
git fetch origin --tags --prune
git status --porcelain            # must be empty
git rev-parse --abbrev-ref HEAD   # note it; return here at the end
git log --oneline origin/main..origin/develop   # what will ship; empty means nothing to release
gh run list -R daintreehq/spotexec --branch develop --limit 1 --json conclusion,headSha
```

- Make sure local `develop` and `main` match `origin`: `git checkout develop && git pull --ff-only`, then the same for `main`. If either has diverged, stop and report.
- The latest CI run on `develop` must be `success` for the current `origin/develop` head. If it's red, go to [CI is red](#ci-is-red) before releasing.
- Run `go vet ./... && go test -race ./...` locally. When Docker is available and the release touches `internal/helper`, `internal/client`, `internal/transport`, `internal/provider` or `internal/daemon`, also run `make e2e`. All three suites must pass. Report their counts.

### 1. Version

`git describe --tags --abbrev=0 origin/main` is the last release. Read the commit subjects since then:
- any `feat!:` or `BREAKING CHANGE` means **major**. Before 1.0.0, use **minor** instead;
- any `feat:` means **minor**;
- otherwise, **patch**.

Check that the tag doesn't exist yet (`git ls-remote --tags origin vX.Y.Z`). State the version and the commits shipping before continuing.

### 2. Merge develop into main

```sh
git checkout main
git merge --no-ff develop -m "release: vX.Y.Z"
git push origin main
```

If the merge conflicts (a hotfix on `main` that never reached `develop`):
1. Abort.
2. Merge `main` into `develop`, resolve the conflicts there, and push `develop`.
3. Wait for CI on `develop`.
4. Retry this step.

### 3. Wait for CI on main

```sh
id=$(gh run list -R daintreehq/spotexec --branch main --workflow ci.yml --limit 1 --json databaseId,headSha --jq '.[0] | select(.headSha=="'"$(git rev-parse main)"'") | .databaseId')
timeout 590 gh run watch "$id" -R daintreehq/spotexec --exit-status --interval 20
```

The run can take a few seconds to appear; poll `gh run list` until one matches the `main` head SHA. Don't tag until it's green. If it fails, go to [CI is red](#ci-is-red).

### 4. Tag and release

```sh
git tag -a vX.Y.Z -m "spotexec vX.Y.Z"
git push origin vX.Y.Z
```

Find the Release run for the tag (`--workflow release.yml`, `headBranch` equal to the tag) and watch it to completion as in step 3. If it fails, go to [Recovery](#recovery).

### 5. Verify what users get

All of these must hold:

```sh
gh release view vX.Y.Z -R daintreehq/spotexec --json assets --jq '.assets[].name'
#  4 tar.gz (darwin/linux × amd64/arm64), 6 packages (deb/rpm/apk × amd64/arm64), checksums.txt
gh api repos/daintreehq/homebrew-tap/contents/Formula/spotexec.rb --jq .content | base64 -d | grep -m1 'version "'
#  version "X.Y.Z"
HOMEBREW_NO_AUTO_UPDATE=1 brew update && brew upgrade daintreehq/tap/spotexec || brew install daintreehq/tap/spotexec
"$(brew --prefix)/bin/spotexec" version     # spotexec X.Y.Z
brew test daintreehq/tap/spotexec
```

When Docker is available, also check Linux:

```sh
docker run --rm --platform linux/amd64 homebrew/brew bash -c \
  'HOMEBREW_NO_AUTO_UPDATE=1 brew install daintreehq/tap/spotexec >/dev/null && spotexec version'
```

### 6. Sync develop

```sh
git checkout develop
git merge --ff-only main || git merge main -m "Merge main (vX.Y.Z) into develop"
git push origin develop
```

Wait for CI on `develop` (as in step 3). Then check out the branch you started on.

### 7. Report

```
🚀 spotexec vX.Y.Z released
- Shipped: <commit subjects, one line each>
- CI: main ✓, develop ✓ · Release workflow ✓ (<duration>)
- Release: https://github.com/daintreehq/spotexec/releases/tag/vX.Y.Z (<n> assets)
- Homebrew: tap formula X.Y.Z; brew upgrade → spotexec X.Y.Z; Linuxbrew ✓|skipped
- Fixes made: <none | what broke and how it was fixed, with any burned versions>
```

## CI is red

1. Read the failing job's log: `gh run view <id> -R daintreehq/spotexec --log-failed | tail -80`.
2. Reproduce it locally (same Go version as `go.mod`; the failing OS if you can).
3. Fix it where it belongs:
   - **Before step 2** (the failure is on `develop`): fix it on `develop` (or a `feature/` branch merged into it), push, and wait for green.
   - **After step 2** (the failure is on `main`, untagged): fix it on a `hotfix/vX.Y.Z-ci` branch from `main`, merge it into `main` (`--no-ff`), push, and wait for green. It reaches `develop` in step 6.
4. Flaky test (it passes on a re-run with no change): `gh run rerun <id> --failed` once. If it flakes again, treat it as a real failure.

## Recovery

Work out how far the failed Release run got:

```sh
gh release view vX.Y.Z -R daintreehq/spotexec        # did the GitHub release get published?
gh api repos/daintreehq/homebrew-tap/contents/Formula/spotexec.rb --jq .content | base64 -d | grep 'version "'
```

**A. The tap formula is not at X.Y.Z** (failed in the guard, tests, build or packaging, or the tap push failed). The version isn't burned yet, so retry at the same version:
1. Fix the cause on `main` through a `hotfix/` branch. If the cause is the deploy key or secret, fix it in repo settings; there's no code change.
2. Delete the GitHub release if one was created, plus both tags:
   ```sh
   gh release delete vX.Y.Z -R daintreehq/spotexec --yes --cleanup-tag   # if it exists
   git push origin :refs/tags/vX.Y.Z; git tag -d vX.Y.Z
   ```
3. Wait for CI on `main`, then re-tag the new `main` head (step 4) and continue.

Common causes:
- **"not on main":** the tag was made on the wrong commit; re-tag on `main`.
- **Deploy key rejected:** the key was removed, or `deploy_keys_enabled_for_repositories` was turned off for the `daintreehq` org.
- **GoReleaser config errors:** run `HOMEBREW_TAP_DEPLOY_KEY=x goreleaser release --snapshot --clean --skip=publish` locally to reproduce. A `brews` deprecation warning alone is expected.

**B. The tap formula is at X.Y.Z, but something is wrong** (a broken binary, a failed `brew test`, missing assets). The version is burned:
1. Fix the cause on a `hotfix/` branch from `main`, and merge it into `main`.
2. Release **X.Y.(Z+1)** from step 3.
3. Edit the bad release's notes to say "superseded by vX.Y.(Z+1)". Leave its tag and assets alone.

After three failed attempts at the same problem, stop and report what you tried. Also stop for anything that needs a human: a credential, an org setting you can't change, or a decision to burn a major version.
