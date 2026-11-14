#!/usr/bin/env bash
# Smoke-tests GoReleaser's output (goreleaser release --snapshot --clean
# --skip=publish): the archive for this machine, the sx entry point and its
# completions and, with Docker, the Linux packages for this architecture.
#
#   scripts/smoke-release.sh [dist-dir]       (default: dist)
#   SMOKE_PACKAGES=0 skips the Docker package installs.
set -euo pipefail

DIST=$(cd "${1:-dist}" && pwd)
VERSION=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$DIST/metadata.json")
[ -n "$VERSION" ] || { echo "no version in $DIST/metadata.json" >&2; exit 1; }
OS=$(go env GOOS)
ARCH=$(go env GOARCH)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
check() { # name, command
  if eval "$2" >"$WORK/check.out" 2>&1; then
    printf '\033[32m✓\033[0m %s\n' "$1"; pass=$((pass + 1))
  else
    printf '\033[31m✗\033[0m %s\n' "$1"; sed 's/^/    /' "$WORK/check.out"; fail=$((fail + 1))
  fi
}
want_version() { # binary...
  local out
  out=$("$@")
  [ "$out" = "spotexec $VERSION" ] || { echo "$* printed '$out', want 'spotexec $VERSION'"; return 1; }
}

echo "== archive ($OS/$ARCH, $VERSION)"
mkdir -p "$WORK/pkg"
tar -xzf "$DIST/spotexec_${VERSION}_${OS}_${ARCH}.tar.gz" -C "$WORK/pkg"
ln -s spotexec "$WORK/pkg/sx" # as Homebrew and the Linux packages link it
B="$WORK/pkg"
check "spotexec --version" "want_version '$B/spotexec' --version"
check "spotexec version" "want_version '$B/spotexec' version"
check "sx --version" "want_version '$B/sx' --version"
for f in spotexec.bash spotexec.zsh spotexec.fish sx.bash sx.zsh sx.fish; do
  check "archive has completions/$f" "test -s '$B/completions/$f'"
done
check "bash loads the sx completion" "bash -c 'source \"$B/completions/sx.bash\" && complete -p sx'"
if command -v zsh >/dev/null; then
  check "zsh parses the sx completion" "zsh -n '$B/completions/sx.zsh'"
fi

echo "== sx is run-only"
# No runners, so anything sx treats as a command fails with 125.
export SPOTEXEC_CONFIG="$WORK/config.toml" SPOTEXEC_STATE_DIR="$WORK/state" SPOTEXEC_POOL= SPOTEXEC_RUNNER=
P="$WORK/proj"
mkdir -p "$P" && git -C "$P" init -q
printf 'version = 1\n[tasks.unit]\nrun = "echo unit"\n' >"$P/.spotexec.toml"
code() { local c=0; (cd "$P" && "$@") >"$WORK/code.out" 2>&1 || c=$?; echo "$c"; }
check "sx status runs a command called status (125: no runners)" \
  "[ \"\$(code '$B/sx' status)\" = 125 ] && grep -q 'no runners configured' '$WORK/code.out'"
check "sx rejects an unknown flag with 125" "[ \"\$(code '$B/sx' --no-such-flag)\" = 125 ]"
check "sx -h shows sx's help" "(cd '$P' && '$B/sx' -h) | grep -q 'Tasks in this project'"
check "a legacy sx hook claude handles the event locally" \
  "out=\$(echo '{}' | '$B/sx' hook claude) && [ -z \"\$out\" ]"
check "sx completes task names" "(cd '$P' && '$B/sx' __complete '') | grep -q '^unit'"
check "sx completes its flags" "(cd '$P' && '$B/sx' __complete --same) | grep -q -- '--same-slot'"

if [ "${SMOKE_PACKAGES:-1}" != 0 ] && command -v docker >/dev/null && docker info >/dev/null 2>&1; then
  echo "== Linux packages (linux/$ARCH)"
  # shellcheck disable=SC2016
  verify='set -e
spotexec --version | grep -qx "spotexec $V"
sx --version | grep -qx "spotexec $V"
test -s /usr/share/bash-completion/completions/sx
test -s /usr/share/fish/vendor_completions.d/sx.fish
test -s "$ZSH_DIR/_sx" && test -s "$ZSH_DIR/_spotexec"'
  export VERIFY="$verify"
  pkg="/dist/spotexec_${VERSION}_linux_${ARCH}"
  check ".deb installs (debian)" "docker run --rm --platform linux/$ARCH -v '$DIST:/dist:ro' -e V='$VERSION' -e ZSH_DIR=/usr/share/zsh/vendor-completions -e VERIFY debian:stable-slim sh -c 'apt-get update -qq >/dev/null && apt-get install -y -qq $pkg.deb >/dev/null && eval \"\$VERIFY\"'"
  check ".rpm installs (fedora)" "docker run --rm --platform linux/$ARCH -v '$DIST:/dist:ro' -e V='$VERSION' -e ZSH_DIR=/usr/share/zsh/site-functions -e VERIFY fedora:latest sh -c 'dnf install -y -q $pkg.rpm >/dev/null && eval \"\$VERIFY\"'"
  check ".apk installs (alpine)" "docker run --rm --platform linux/$ARCH -v '$DIST:/dist:ro' -e V='$VERSION' -e ZSH_DIR=/usr/share/zsh/site-functions -e VERIFY alpine:latest sh -c 'apk add -q --allow-untrusted $pkg.apk && eval \"\$VERIFY\"'"
fi

echo
echo "passed: $pass  failed: $fail"
[ "$fail" = 0 ]
