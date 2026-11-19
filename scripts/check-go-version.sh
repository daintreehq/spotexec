#!/usr/bin/env bash
# Fails unless every binary was built by exactly the Go release go.mod pins,
# so a release never ships the standard library of an unpatched compiler.
# GoReleaser runs it on the embedded helpers and on every build, and the
# release smoke test on every archive.
#
#   scripts/check-go-version.sh BINARY...
#
# A BINARY ending in .gz (an embedded helper) is checked decompressed.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WANT=go$(sed -n 's/^go[[:space:]]\{1,\}\([0-9][^[:space:]]*\).*/\1/p' "$ROOT/go.mod")
[[ $WANT =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "go.mod's go line must name a patch release (go 1.N.P), got '${WANT#go}'" >&2
  exit 1
}
[ $# -gt 0 ] || { echo "usage: $0 BINARY..." >&2; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
for f in "$@"; do
  bin=$f
  if [[ $f == *.gz ]]; then
    bin="$tmp/bin"
    gzip -dc "$f" >"$bin" || { echo "$f: can't decompress" >&2; fail=1; continue; }
  elif [ ! -f "$f" ]; then
    echo "$f: no such file" >&2; fail=1; continue
  fi
  # `go version FILE` prints "FILE: goX.Y.Z" on its first stdout line (more
  # lines follow under GOFLAGS=-m) and nothing for a non-Go file. Diagnostics
  # go to stderr, so a path that ends in a version can't pass for one.
  out=$(go version "$bin" 2>/dev/null) || out=
  line=${out%%$'\n'*}
  if [ "$line" = "$bin: $WANT" ]; then
    echo "$f: $WANT"
  else
    got=${line#"$bin: "}
    echo "$f: built with ${got:-no Go version (not a Go binary)}, want $WANT (go.mod)" >&2
    fail=1
  fi
done
exit "$fail"
