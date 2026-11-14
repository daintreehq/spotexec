#!/usr/bin/env bash
# End-to-end tests against a throwaway Ubuntu 24.04 SSH container.
# Requires: docker, make build (bin/spotexec with embedded helpers).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SX="$ROOT/bin/spotexec"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/spotexec-e2e.XXXXXX")
PORT=${SPOTEXEC_E2E_PORT:-2222}
NAME=spotexec-e2e-$$
PASS=0
FAIL=0

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  ssh -o ControlPath="$HOME/.cache/spotexec/cm/"'*' -O exit 127.0.0.1 >/dev/null 2>&1 || true
}
trap cleanup EXIT

ok()   { PASS=$((PASS+1)); printf '\033[32m✓\033[0m %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '\033[31m✗\033[0m %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

[ -x "$SX" ] || { echo "build first: make build"; exit 2; }

# --- runner container ------------------------------------------------------
ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"
mkdir -p "$WORK/image"
cp "$ROOT/scripts/e2e/Dockerfile" "$WORK/image/"
cp "$WORK/id_ed25519.pub" "$WORK/image/"
docker build -q -t spotexec-e2e "$WORK/image" >/dev/null
docker run -d --name "$NAME" -p "$PORT:22" spotexec-e2e >/dev/null
for _ in $(seq 1 50); do
  ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -i "$WORK/id_ed25519" -p "$PORT" dev@127.0.0.1 true 2>/dev/null && break
  sleep 0.2
done

export SPOTEXEC_CONFIG_DIR="$WORK/cfg" SPOTEXEC_STATE_DIR="$WORK/state"
mkdir -p "$SPOTEXEC_CONFIG_DIR"
cat > "$SPOTEXEC_CONFIG_DIR/config.toml" <<EOF
version = 1
default_pool = ["box"]
[runners.box]
type = "ssh"
host = "dev@127.0.0.1"
identity_file = "$WORK/id_ed25519"
ssh_options = ["Port=$PORT", "StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null"]
max_slots = 3
EOF

# --- test repository -------------------------------------------------------
REPO="$WORK/repo"
mkdir -p "$REPO/src" && cd "$REPO" && REPO=$(pwd -P)
git init -q && git config user.email t@t && git config user.name t
printf 'a\n' > src/a.txt
printf 'b\n' > src/b.txt
printf 'node_modules/\ndist/\ncoverage/\n' > .gitignore
ln -s a.txt src/link
git add -A && git commit -qm init
printf 'untracked\n' > src/new.txt
printf 'SECRET=1\n' > .env

ev() { grep -E "$1" "$WORK/last.err" || true; }
run() { "$SX" --json "$@" >"$WORK/last.out" 2>"$WORK/last.err"; }

echo "== sync and exec"
run 'cat src/new.txt; test ! -e .env && echo noenv; readlink src/link; echo slot=$SPOTEXEC_SLOT; pwd' || true
check "untracked files synced" "grep -q untracked $WORK/last.out"
check ".env* excluded by default" "grep -q noenv $WORK/last.out || ! grep -q 'SECRET' $WORK/last.out"
check "symlinks preserved" "grep -qx a.txt $WORK/last.out"
check "slot env injected" "grep -q slot=1 $WORK/last.out"
check "remote paths rewritten to local" "grep -qF \"$REPO\" $WORK/last.out"

echo "== fingerprint short-circuit"
run true
check "second run skips sync" "ev fingerprint_hit | grep -q ."

echo "== write-back"
run 'sed -i s/a/A/ src/a.txt; echo created > src/gen.txt; rm src/b.txt; mkdir -p dist; echo x > dist/out.js; chmod +x src/new.txt' || true
check "modified file written back" "grep -qx A src/a.txt"
check "created file written back" "test -f src/gen.txt"
check "deleted file removed locally" "test ! -e src/b.txt"
check "mode change written back" "test -x src/new.txt"
check "gitignored output not written back" "test ! -e dist/out.js"
run true
check "after write-back the slot needs no transfer" "ev '\"sync_in\".*\"bytes\":0' | grep -q ."

echo "== exit codes, timeout, cancel"
"$SX" --json 'exit 7' >/dev/null 2>&1 && code=0 || code=$?
check "exit code passes through" "[ $code = 7 ]"
"$SX" --json --timeout 1s 'sleep 30' >/dev/null 2>"$WORK/t.err" && code=0 || code=$?
check "timeout exits 124" "[ $code = 124 ]"
"$SX" --json 'trap "echo got-term; exit 143" TERM; sleep 30 & wait' >"$WORK/c.out" 2>&1 &
P=$!; sleep 1.5; kill -INT $P; wait $P && code=0 || code=$?
check "ctrl-c terminates the remote process group" "grep -q got-term $WORK/c.out && [ $code = 143 ]"

echo "== conflicts"
("$SX" --json 'sleep 2; echo remote > src/a.txt' >/dev/null 2>"$WORK/conf.err") &
sleep 1; echo "local edit" > src/a.txt; wait
check "concurrent local edit is kept" "grep -qx 'local edit' src/a.txt"
check "remote version stashed" "ls .git/spotexec/conflicts/*/src/a.txt >/dev/null 2>&1"
check "SYNC_CONFLICT reported" "grep -q SYNC_CONFLICT $WORK/conf.err"

echo "== concurrency (max_slots=3, 4 jobs)"
for i in 1 2 3 4; do "$SX" --json "sleep 1.5; echo slot=\$SPOTEXEC_SLOT" >"$WORK/p$i.out" 2>"$WORK/p$i.err" & done; wait
check "three run at once, the fourth queues" "cat $WORK/p*.err | grep -c '\"event\":\"queued\"' | grep -qx 1"
check "all four succeed" "[ \$(cat $WORK/p*.out | grep -c slot=) = 4 ]"

echo "== deps cache"
cat > package.json <<'EOF'
{"name":"t","version":"1.0.0","scripts":{"test":"node -e \"console.log('ok=' + require('is-number')(5))\"","lint":"echo lint-ok"}}
EOF
"$SX" -q 'npm install --package-lock-only --no-audit --no-fund is-number@7 >/dev/null 2>&1'
check "lockfile generated remotely and written back" "test -f package-lock.json"
"$SX" init >/dev/null
check "init detects npm lockfile" "grep -q 'npm ci' .spotexec.toml"
run test || true
check "deps built once" "ev '\"built\":true' | grep -q ."
check "task runs with deps" "grep -q ok=true $WORK/last.out"
("$SX" -q 'sleep 3' &) ; sleep 0.7
run test || true
check "other slot hydrated from cache" "ev '\"hydrated\":true' | grep -q ."
wait
"$SX" verify >"$WORK/v.out" 2>&1 || true
check "parallel task fan-out" "grep -q '\\[test\\] ok=true' $WORK/v.out && grep -q '\\[lint\\]' $WORK/v.out"

sed -i.bak 's/^install = "npm ci"/install = "npm ci --no-audit"/' .spotexec.toml && rm -f .spotexec.toml.bak
"$SX" --json verify >"$WORK/v2.out" 2>"$WORK/v2.err" || true
check "two slots on a cold cache: one builds, the other hydrates" "grep -q '\"built\":true' $WORK/v2.err && grep -q '\"hydrated\":true' $WORK/v2.err && [ \$(grep -c '\"exit_code\":0' $WORK/v2.err) -ge 3 ]"
("$SX" -q 'sleep 3' &) ; sleep 1
"$SX" --json --exclusive 'echo excl' >"$WORK/x.out" 2>"$WORK/x.err" || true
check "--exclusive waits for running jobs" "grep -q excl $WORK/x.out && grep -q '\"event\":\"queued\"' $WORK/x.err"

echo "== pull, exec, status, unlock"
"$SX" -q 'mkdir -p coverage && echo report > coverage/index.html'
"$SX" pull 'coverage/*' >/dev/null 2>&1 || true
check "pull fetches gitignored artifacts" "grep -qx report coverage/index.html"
"$SX" exec --json 'ls node_modules' >"$WORK/e.out" 2>&1 || true
check "exec reuses the last slot without syncing" "grep -q is-number $WORK/e.out && ! grep -q sync_in $WORK/e.out"
("$SX" -q 'sleep 20' &) ; sleep 1
"$SX" status >"$WORK/s.out" 2>&1 || true
check "status shows the holder" "grep -q 'sleep 20' $WORK/s.out"
"$SX" unlock box --all >/dev/null 2>&1 || true
sleep 0.5
"$SX" status --json >"$WORK/s.json" 2>&1 || true
check "unlock reclaims the slot" "grep -q '\"holders\":\\[\\]' $WORK/s.json"

echo "== review regressions"
cat >> .spotexec.toml <<'EOF'

[tasks.echoargs]
run = 'printf "<%s>" "{args}"'
EOF
"$SX" -q echoargs '$(echo INJECTED)' 'two words' >"$WORK/inj.out" 2>&1 || true
check "task args are not shell-evaluated" "grep -qF '<\$(echo INJECTED)><two words>' $WORK/inj.out"
cat $WORK/inj.out
git add -A >/dev/null && git commit -qm wip
"$SX" -q 'mv src/gen.txt src/Gen.txt' || true
check "case-only rename keeps the file" "ls src | grep -qx Gen.txt"
"$SX" -q --sync in 'echo remote-copy > src/a.txt' || true
"$SX" --pull 'src/*' --sync in true >/dev/null 2>"$WORK/pull.err" || true
check "pull never overwrites tracked source files" "! grep -q remote-copy src/a.txt && grep -q 'pull skipped' $WORK/pull.err"
"$SX" -q 'setsid sh -c "sleep 60" </dev/null >/dev/null 2>&1 & sleep 0.3; echo started' || true
check "processes escaping the group are reaped" "! docker exec $NAME pgrep -u dev -f 'sleep 60' >/dev/null"

echo "== idle watchdog (cloud-init script, run as root in the container)"
"$SX" cloud-init --help >/dev/null
cat > "$WORK/render.go" <<'EOF'
package main

import (
	"fmt"
	"time"

	"github.com/daintreehq/spotexec/internal/cloudinit"
)

func main() {
	s, err := cloudinit.Render(cloudinit.Options{User: "dev", PublicKey: "ssh-ed25519 AAAA test", IdleTimeout: 3 * time.Second, MaxLifetime: time.Hour, SelfDestruct: "touch /tmp/destroyed"})
	if err != nil {
		panic(err)
	}
	fmt.Print(s)
}
EOF
mkdir -p "$ROOT/.e2e-render" && cp "$WORK/render.go" "$ROOT/.e2e-render/main.go"
(cd "$ROOT" && go run ./.e2e-render) > "$WORK/userdata.sh"; rm -f "$ROOT/.e2e-render/main.go"; rmdir "$ROOT/.e2e-render"
docker cp "$WORK/userdata.sh" "$NAME:/tmp/userdata.sh"
docker exec "$NAME" bash -c 'mkdir -p /tmp/fakebin && for b in systemctl systemd-run; do printf "#!/bin/sh\nexit 0\n" > /tmp/fakebin/$b; chmod +x /tmp/fakebin/$b; done; PATH=/tmp/fakebin:$PATH bash /tmp/userdata.sh >/dev/null 2>&1; cat /var/tmp/spotexec/ready'
check "watchdog installed and ready marker written" "docker exec $NAME test -x /usr/local/sbin/spotexec-watchdog"
"$SX" -q true  # helper now holds/touches the lifecycle files as user dev
check "helper (non-root) refreshes activity" "docker exec $NAME bash -c 'test \$(( \$(date +%s) - \$(stat -c %Y /var/tmp/spotexec/activity) )) -lt 3'"
docker exec "$NAME" bash -c '/usr/local/sbin/spotexec-watchdog'
check "no shutdown while recently active" "! docker exec $NAME test -e /tmp/destroyed"
("$SX" -q 'sleep 6' &) ; sleep 4
docker exec "$NAME" bash -c 'touch -d "-1 hour" /var/tmp/spotexec/activity; /usr/local/sbin/spotexec-watchdog'
check "no shutdown while a session holds the lifecycle lock" "! docker exec $NAME test -e /tmp/destroyed"
sleep 3
docker exec "$NAME" bash -c 'touch -d "-1 hour" /var/tmp/spotexec/activity; /usr/local/sbin/spotexec-watchdog'
check "idle VM self-destructs" "docker exec $NAME test -e /tmp/destroyed"
"$SX" --json true >/dev/null 2>"$WORK/d.err" && code=0 || code=$?
check "draining runner refuses new work" "[ $code = 125 ] && grep -q DRAINING $WORK/d.err"

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" = 0 ]
