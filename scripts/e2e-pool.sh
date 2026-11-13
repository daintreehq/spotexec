#!/usr/bin/env bash
# Spot worker pool scenarios against the Docker fleet simulator (type = "sim").
# Each simulated instance boots the real cloud-init user-data and watchdog;
# interruptions/rebalances are injected with `spotexec sim`.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SX="$ROOT/bin/spotexec"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/spotexec-pool.XXXXXX")
PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); printf '\033[32m✓\033[0m %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '\033[31m✗\033[0m %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
[ -x "$SX" ] || { echo "build first: make build"; exit 2; }

export SPOTEXEC_CONFIG_DIR="$WORK/cfg" SPOTEXEC_STATE_DIR="$WORK/state"
mkdir -p "$SPOTEXEC_CONFIG_DIR"
RUNNER=pool$$
cat > "$SPOTEXEC_CONFIG_DIR/config.toml" <<EOF
version = 1
default_pool = ["$RUNNER"]
[runners.$RUNNER]
type = "sim"
max_instances = 3
max_slots = 2
idle_timeout = "20s"
boot_delay = "2s"
EOF

count() { docker ps -q --filter "label=spotexec-runner=$RUNNER" | wc -l | tr -d ' '; }
cleanup() { docker ps -q --filter "label=spotexec-runner=$RUNNER" | xargs -r docker rm -f >/dev/null 2>&1; }
trap cleanup EXIT

REPO="$WORK/repo"
mkdir -p "$REPO" && cd "$REPO" && git init -q && git config user.email t@t && git config user.name t
echo hello > a.txt && git add -A && git commit -qm init

"$SX" sim build "$RUNNER" || { echo "could not build simulator image"; exit 2; }

echo "== cold start"
"$SX" --json 'echo hi from $SPOTEXEC_RUNNER' >"$WORK/c.out" 2>"$WORK/c.err"
check "first command launches an instance and runs" "grep -q 'hi from' $WORK/c.out && grep -q scale_out $WORK/c.err"
check "exactly one instance" "[ \$(count) = 1 ]"

echo "== scale out (6 jobs, 2 slots per instance, max 3 instances)"
for i in 1 2 3 4 5 6; do "$SX" --json "sleep 6; echo job$i" >"$WORK/s$i.out" 2>"$WORK/s$i.err" & done; wait
check "all six jobs succeed" "[ \$(cat $WORK/s*.out | grep -c '^job') = 6 ]"
check "fleet grew to 3 instances" "[ \$(count) = 3 ]"

echo "== capacity cap (8 jobs on 6 slots)"
for i in 1 2 3 4 5 6 7 8; do "$SX" --json "sleep 4; echo cap$i" >"$WORK/k$i.out" 2>"$WORK/k$i.err" & done; wait
check "all eight jobs succeed (two queued)" "[ \$(cat $WORK/k*.out | grep -c '^cap') = 8 ]"
check "never more than max_instances" "[ \$(count) -le 3 ]"

echo "== bin-packing (sequential jobs reuse the most recently used instance)"
for i in 1 2 3 4; do "$SX" --json "echo seq$i" >/dev/null 2>"$WORK/b$i.err"; done
check "sequential jobs land on one instance" "[ \$(cat $WORK/b*.err | grep '\"placed\"' | sed 's/.*instance_id\":\"\\([^\"]*\\)\".*/\\1/' | sort -u | wc -l | tr -d ' ') = 1 ]"

echo "== idle scale-in (idle_timeout 20s)"
for _ in $(seq 1 30); do [ "$(count)" -le 1 ] && break; sleep 2; done
check "idle instances terminated themselves (busiest one may linger)" "[ \$(count) -le 1 ]"
for _ in $(seq 1 20); do [ "$(count)" = 0 ] && break; sleep 2; done
check "pool scaled to zero" "[ \$(count) = 0 ]"

echo "== spot interruption mid-run: job replayed on a replacement"
"$SX" --json 'sleep 12; echo finished attempt=$SPOTEXEC_ATTEMPT run=$SPOTEXEC_RUN_ID' >"$WORK/i.out" 2>"$WORK/i.err" &
P=$!
for _ in $(seq 1 40); do grep -q '"exec"' "$WORK/i.err" 2>/dev/null && break; sleep 0.5; done
sleep 1
"$SX" sim interrupt "$RUNNER" --after 4s >/dev/null
wait $P; code=$?
check "notice reached the client" "grep -q spot_interruption $WORK/i.err"
check "replacement capacity warmed before the VM died" "[ \$(grep -c scale_out $WORK/i.err) -ge 2 ]"
check "run replayed and succeeded on attempt 2" "[ $code = 0 ] && grep -q 'finished attempt=2' $WORK/i.out"
check "replay kept the same run id" "grep -q RUNNER_LOST $WORK/i.err"

echo "== interruption after the job finishes: no replay"
"$SX" --json 'sleep 2; echo quick attempt=$SPOTEXEC_ATTEMPT' >"$WORK/q.out" 2>"$WORK/q.err" &
P=$!
for _ in $(seq 1 40); do grep -q '"exec"' "$WORK/q.err" 2>/dev/null && break; sleep 0.5; done
ID=$(grep '"placed"' "$WORK/q.err" | sed 's/.*instance_id":"\([^"]*\)".*/\1/' | tail -1)
"$SX" sim interrupt "$RUNNER" "$ID" --after 8s >/dev/null &
wait $P; code=$?
check "finished on the doomed instance without replay" "[ $code = 0 ] && grep -q 'quick attempt=1' $WORK/q.out && ! grep -q RUNNER_LOST $WORK/q.err"
wait

echo "== rebalance recommendation: drain, then leave when empty"
"$SX" -q true
ID=$(docker ps --filter "label=spotexec-runner=$RUNNER" --format '{{.Names}}' | head -1)
"$SX" --json 'sleep 6; echo drained-ok' >"$WORK/r.out" 2>"$WORK/r.err" &
P=$!
sleep 2
"$SX" sim rebalance "$RUNNER" "$ID" >/dev/null
sleep 4
"$SX" --json 'echo elsewhere' >"$WORK/r2.out" 2>"$WORK/r2.err"
check "new work avoids the draining instance" "grep -q elsewhere $WORK/r2.out && ! grep -q \"$ID\" <(grep '\"placed\"' $WORK/r2.err)"
wait $P
check "running job on the draining instance finished" "grep -q drained-ok $WORK/r.out"
for _ in $(seq 1 15); do docker ps --format '{{.Names}}' | grep -q "^$ID\$" || break; sleep 1; done
check "draining instance terminated as soon as it was empty" "! docker ps --format '{{.Names}}' | grep -q '^$ID\$'"

echo "== thundering herd from cold (12 agents at once)"
cleanup; sleep 1
rm -rf "$SPOTEXEC_STATE_DIR/runners/$RUNNER/pool.json"
for i in $(seq 1 12); do "$SX" --json "sleep 3; echo herd$i" >"$WORK/h$i.out" 2>"$WORK/h$i.err" & done; wait
check "all twelve agents completed" "[ \$(cat $WORK/h*.out | grep -c '^herd') = 12 ]"
check "launched at most max_instances" "[ \$(cat $WORK/h*.err | grep -c scale_out) -le 3 ] && [ \$(count) -le 3 ]"

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" = 0 ]
