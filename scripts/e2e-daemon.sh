#!/usr/bin/env bash
# Pool-manager (spotexec daemon) scenarios against the Docker fleet simulator.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SX="$ROOT/bin/spotexec"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/spotexec-daemon.XXXXXX")
PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); printf '\033[32m✓\033[0m %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '\033[31m✗\033[0m %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ -x "$SX" ] || { echo "build first: make build"; exit 2; }

export SPOTEXEC_CONFIG_DIR="$WORK/cfg" SPOTEXEC_STATE_DIR="$WORK/state" SPOTEXEC_DAEMON_FAST=1
mkdir -p "$SPOTEXEC_CONFIG_DIR"
RUNNER=dmn$$
cat > "$SPOTEXEC_CONFIG_DIR/config.toml" <<EOF
version = 1
default_pool = ["$RUNNER"]
[runners.$RUNNER]
type = "sim"
min_instances = 1
max_instances = 3
max_slots = 2
idle_timeout = "20s"
boot_timeout = "40s"
boot_delay = "2s"
priority = 10
[runners.${RUNNER}r]
type = "sim"
min_instances = 1
max_instances = 2
max_slots = 2
idle_timeout = "20s"
boot_timeout = "40s"
max_lifetime = "80s"
boot_delay = "2s"
priority = 30
EOF

ids() { docker ps -q --filter "label=spotexec-runner=$RUNNER" --filter status=running; }
rids() { docker ps -q --filter "label=spotexec-runner=${RUNNER}r" --filter status=running; }
count() { ids | wc -l | tr -d ' '; }
waitfor() { # desc, condition, seconds
  local end=$((SECONDS+$3))
  until eval "$2"; do [ $SECONDS -ge $end ] && return 1; sleep 2; done
}
cleanup() {
  "$SX" daemon stop >/dev/null 2>&1
  for r in "$RUNNER" "${RUNNER}r"; do
    docker ps -aq --filter "label=spotexec-runner=$r" | xargs -r docker rm -f >/dev/null 2>&1
  done
  return 0
}
trap cleanup EXIT

REPO="$WORK/repo"
mkdir -p "$REPO" && cd "$REPO" && git init -q && git config user.email t@t && git config user.name t
echo hello > a.txt && git add -A && git commit -qm init
"$SX" sim build "$RUNNER" >/dev/null || { echo "could not build simulator image"; exit 2; }

echo "== start"
check "daemon status before start says not running" "\"$SX\" daemon status | grep >/dev/null 'not running'"
"$SX" daemon start >"$WORK/start.out" 2>&1
check "daemon starts in the background" "grep -q 'daemon started' $WORK/start.out"
check "second start is a no-op" "\"$SX\" daemon start | grep >/dev/null 'already running'"
check "spotexec status mentions the pool manager" "\"$SX\" status 2>/dev/null | grep >/dev/null 'pool manager: running'"

echo "== warm pool"
check "launches min_instances with no job waiting" "waitfor x '[ \$(count) = 1 ]' 60"
W=$(ids)
check "warm instance reported healthy" "waitfor x '\"$SX\" daemon status | grep >/dev/null \"healthy warm\"' 40"
sleep 35
check "warm instance outlives idle_timeout (kept alive)" "[ \"\$(ids)\" = \"$W\" ]"

echo "== jobs use the warm instance"
"$SX" --json 'echo warm-run' >"$WORK/r.out" 2>"$WORK/r.err"
check "job runs on the warm instance without scaling out" "grep -q warm-run $WORK/r.out && ! grep -q scale_out $WORK/r.err"

echo "== concurrent demand scales beyond the warm pool"
for i in 1 2 3 4 5 6; do "$SX" --json "sleep 6; echo job$i" >"$WORK/c$i.out" 2>"$WORK/c$i.err" & done
wait
n=$(cat "$WORK"/c*.out | grep -c 'job[0-9]')
check "6 concurrent jobs all succeed" "[ $n = 6 ]"
check "fleet grew past the warm pool" "[ \$(count) -ge 2 ]"
check "extra instances idle out back to min_instances" "waitfor x '[ \$(count) = 1 ]' 90"

echo "== spot notice on an idle warm instance: replaced during the notice"
W=$(ids)
"$SX" sim interrupt "$RUNNER" "$W" --after 60s >/dev/null 2>&1
# The notice gives 60s; a replacement running well before then shows the
# daemon reacted to the notice, not to the instance disappearing. (The idle
# noticed VM powers itself off early: nothing is running on it.)
check "replacement warm instance running before the notice deadline" "waitfor x '[ -n \"\$(ids | grep -v $W)\" ]' 40"
check "notice logged" "\"$SX\" daemon status | grep >/dev/null 'spot interruption notice'"
check "noticed instance gone, one warm instance left" "waitfor x '[ \$(count) = 1 ] && [ \"\$(ids)\" != \"$W\" ]' 90"

echo "== warm instance retired ahead of max_lifetime (80s; retire 25s ahead)"
# The second fleet has been warm since the daemon started.
check "retirement logged" "waitfor x '\"$SX\" daemon status | grep >/dev/null \"retiring warm instance\"' 120"
check "retired instance replaced (fleet keeps one warm instance)" "waitfor x '[ \$(rids | wc -l | tr -d \" \") -ge 1 ]' 30"
first=$(grep -m1 "${RUNNER}r: .* running at" "$SPOTEXEC_STATE_DIR/daemon/daemon.log" | grep -o 'sx-[a-z0-9-]*')
check "the first warm instance stopped before its hard max_lifetime kill" "waitfor x '[ -z \"\$(docker ps -q --filter name=$first)\" ]' 60 && ! grep -q \"$first.*past max_lifetime\" $SPOTEXEC_STATE_DIR/daemon/daemon.log"

echo "== health: an unresponsive instance is replaced"
W=$(ids)
docker pause "$W" >/dev/null
check "paused instance terminated and a new warm one launched" "waitfor x '[ -z \"\$(docker ps -aq --filter id=$W)\" ] && [ \$(count) = 1 ] && [ \"\$(ids)\" != \"$W\" ]' 120"
check "replacement logged" "\"$SX\" daemon status | grep >/dev/null unresponsive"

echo "== down + relaunch"
"$SX" down "$RUNNER" >"$WORK/down.out" 2>&1
check "down warns that the daemon will relaunch" "grep -q 'will relaunch' $WORK/down.out"
check "daemon relaunches the warm instance" "waitfor x '[ \$(count) = 1 ]' 60"

echo "== stop"
"$SX" daemon stop >"$WORK/stop.out" 2>&1
check "daemon stops" "grep -q 'daemon stopped' $WORK/stop.out && \"$SX\" daemon status | grep >/dev/null 'not running'"
check "without the daemon the warm instance idles out" "waitfor x '[ \$(count) = 0 ]' 90"

echo
echo "passed: $PASS  failed: $FAIL"
[ $FAIL = 0 ]
