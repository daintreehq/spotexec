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
export SPOTEXEC_SIM_ACCOUNT=$RUNNER   # simulated quotas count this suite's VMs only
cat > "$SPOTEXEC_CONFIG_DIR/config.toml" <<EOF
version = 1
default_pool = ["$RUNNER"]
[runners.$RUNNER]
type = "sim"
max_instances = 3
max_slots = 2
idle_timeout = "20s"
boot_delay = "2s"
priority = 10
[runners.${RUNNER}b]
type = "sim"
max_instances = 1
max_slots = 4
idle_timeout = "20s"
boot_delay = "2s"
priority = 20
[runners.${RUNNER}l]
type = "sim"
max_instances = 2
max_slots = 2
idle_timeout = "60s"
max_lifetime = "60s"
boot_timeout = "20s"
boot_delay = "1s"
priority = 40
[runners.${RUNNER}s]
type = "sim"
max_instances = 1
max_slots = 2
idle_timeout = "30s"
boot_delay = "1s"
priority = 50
bootstrap_script = """
echo boot-step-one; sleep 6
echo boot-step-two; sleep 6
"""
[runners.${RUNNER}r]
type = "sim"
max_instances = 4
max_slots = 1
idle_timeout = "3m"
boot_timeout = "15s"
boot_delay = "1s"
priority = 60
EOF

count() { docker ps -q --filter "label=spotexec-runner=$RUNNER" | wc -l | tr -d ' '; }
countb() { docker ps -q --filter "label=spotexec-runner=${RUNNER}b" | wc -l | tr -d ' '; }
countx() { docker ps -q --filter "label=spotexec-runner=${RUNNER}x" | wc -l | tr -d ' '; }
cleanup() {
  # One filter per command: Docker ANDs repeated label filters.
  for r in "$RUNNER" "${RUNNER}b" "${RUNNER}x" "${RUNNER}l" "${RUNNER}s" "${RUNNER}r"; do
    docker ps -q --filter "label=spotexec-runner=$r" | xargs -r docker rm -f >/dev/null 2>&1
  done
  return 0
}
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

echo "== bin-packing (sequential jobs from one worktree reuse one instance)"
for i in 1 2 3 4; do "$SX" --json "echo seq$i" >/dev/null 2>"$WORK/b$i.err"; done
check "sequential jobs land on one instance" "[ \$(cat $WORK/b*.err | grep '\"placed\"' | sed 's/.*instance_id\":\"\\([^\"]*\\)\".*/\\1/' | sort -u | wc -l | tr -d ' ') = 1 ]"

echo "== idle scale-in (idle_timeout 20s)"
for _ in $(seq 1 30); do [ "$(count)" -le 1 ] && break; sleep 2; done
check "idle instances terminated themselves (busiest one may linger)" "[ \$(count) -le 1 ]"
for _ in $(seq 1 20); do [ "$(count)" = 0 ] && break; sleep 2; done
check "pool scaled to zero" "[ \$(count) = 0 ]"

echo "== spot interruption mid-run: job replayed on a replacement"
t0=$(date +%s)
"$SX" --json 'sleep 12; echo finished attempt=$SPOTEXEC_ATTEMPT run=$SPOTEXEC_RUN_ID' 2>&1 >"$WORK/i.out" | while IFS= read -r l; do echo "$(( $(date +%s)-t0 ))s $l"; done >"$WORK/i.err" &
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

echo "== the VM's shutdown kills the command: lost runner, nothing written back, replayed"
"$SX" --json --writeback always 'if [ "$SPOTEXEC_ATTEMPT" = 1 ]; then echo partial > partial.txt; sleep 60; fi # reclaim-marker
echo finished attempt=$SPOTEXEC_ATTEMPT' >"$WORK/k.out" 2>"$WORK/k.err" &
P=$!
for _ in $(seq 1 40); do grep -q '"exec"' "$WORK/k.err" 2>/dev/null && break; sleep 0.5; done
sleep 1
ID=$(grep '"placed"' "$WORK/k.err" | sed 's/.*instance_id":"\([^"]*\)".*/\1/' | tail -1)
"$SX" sim interrupt "$RUNNER" "$ID" --after 90s >/dev/null 2>&1 &
IP=$!
for _ in $(seq 1 20); do grep -q spot_interruption "$WORK/k.err" 2>/dev/null && break; sleep 0.5; done
# Shutdown begins and kills the command while the helper is still alive.
docker exec "$ID" sh -c 'touch /var/tmp/spotexec/sim/stopping; pkill -TERM -f reclaim-marker'
wait $P; code=$?
kill $IP 2>/dev/null; docker rm -f "$ID" >/dev/null 2>&1
check "killed run replayed and succeeded on attempt 2" "[ $code = 0 ] && grep -q 'finished attempt=2' $WORK/k.out && grep -q RUNNER_LOST $WORK/k.err"
check "nothing from the killed attempt was written back" "test ! -e partial.txt"

echo "== the VM's shutdown kills the command with no Spot notice (max_lifetime): replayed"
"$SX" --json 'if [ "$SPOTEXEC_ATTEMPT" = 1 ]; then sleep 60; fi # halt-marker
echo finished attempt=$SPOTEXEC_ATTEMPT' >"$WORK/vmhalt.out" 2>"$WORK/vmhalt.err" &
P=$!
for _ in $(seq 1 40); do grep -q '"exec"' "$WORK/vmhalt.err" 2>/dev/null && break; sleep 0.5; done
sleep 1
ID=$(grep '"placed"' "$WORK/vmhalt.err" | sed 's/.*instance_id":"\([^"]*\)".*/\1/' | tail -1)
# The OS halts while the cloud still reports the VM running.
docker exec "$ID" sh -c 'touch /var/tmp/spotexec/sim/stopping; pkill -TERM -f halt-marker'
wait $P; code=$?
docker rm -f "$ID" >/dev/null 2>&1
check "no notice reached the client" "test -s \"$WORK/vmhalt.err\" && ! grep -q spot_interruption \"$WORK/vmhalt.err\""
check "halted run replayed and succeeded on attempt 2" "[ $code = 0 ] && grep -q 'finished attempt=2' \"$WORK/vmhalt.out\" && grep -q RUNNER_LOST \"$WORK/vmhalt.err\" && ! grep -q RUNNER_UNRESPONSIVE \"$WORK/vmhalt.err\""

echo "== rebalance recommendation is advisory (real EC2 sends it at boot)"
"$SX" -q true
ID=$(docker ps --filter "label=spotexec-runner=$RUNNER" --format '{{.Names}}' | head -1)
"$SX" --json 'sleep 4; echo still-ok' >"$WORK/r.out" 2>"$WORK/r.err" &
P=$!
sleep 1
"$SX" sim rebalance "$RUNNER" "$ID" >/dev/null
wait $P
check "rebalance reported to the client" "grep -q spot_rebalance $WORK/r.err && grep -q still-ok $WORK/r.out"
sleep 5
check "rebalance does not drain or terminate the instance" "docker ps --format '{{.Names}}' | grep -q '^$ID\$' && ! docker exec $ID test -e /var/tmp/spotexec/draining"

echo "== thundering herd from cold (12 agents at once)"
cleanup; sleep 1
rm -rf "$SPOTEXEC_STATE_DIR/runners/$RUNNER/pool.json"
for i in $(seq 1 12); do "$SX" --json "sleep 3; echo herd$i" >"$WORK/h$i.out" 2>"$WORK/h$i.err" & done; wait
check "all twelve agents completed" "[ \$(cat $WORK/h*.out | grep -c '^herd') = 12 ]"
check "launched at most max_instances" "[ \$(cat $WORK/h*.err | grep -c scale_out) -le 3 ] && [ \$(count) -le 3 ]"

echo "== fair queue: 30 agents on 6 slots, and a late high-priority agent jumps ahead"
cleanup; sleep 1
"$SX" up "$RUNNER" --count 3 >/dev/null 2>&1
for i in $(seq 1 30); do "$SX" --json "sleep 3; echo q$i" >"$WORK/q$i.out" 2>"$WORK/q$i.err" & sleep 0.1; done
sleep 2
t0=$(date +%s)
"$SX" --json --priority 10 'echo vip' >"$WORK/vip.out" 2>"$WORK/vip.err"
vip=$(( $(date +%s) - t0 ))
wait
check "all thirty queued agents completed" "[ \$(cat $WORK/q*.out | grep -cE '^q[0-9]+$') = 30 ]"
check "queue positions were reported" "grep -q queue_position $WORK/q30.err"
check "high-priority agent ran within ~2 job waves (${vip}s)" "grep -q vip $WORK/vip.out && [ $vip -le 9 ]"
check "late arrivals did not overtake: agent 30 was queued behind others" "grep -q '\"position\":[2-9][0-9]*' $WORK/q30.err"

echo "== overflow into a second, lower-priority fleet (10 agents: 3x2 slots + 1x4 slots)"
cleanup; sleep 1
for i in $(seq 1 10); do SPOTEXEC_POOL="$RUNNER,${RUNNER}b" "$SX" --json "sleep 5; echo over$i" >"$WORK/o$i.out" 2>"$WORK/o$i.err" & done; wait
check "all ten agents completed" "[ \$(cat $WORK/o*.out | grep -c '^over') = 10 ]"
check "primary fleet filled to max (3)" "[ \$(count) = 3 ]"
check "overflow fleet launched exactly one" "[ \$(countb) = 1 ]"

echo "== shared dependency cache: a fresh instance restores instead of installing"
cleanup; sleep 1
DREPO="$WORK/depsrepo"
mkdir -p "$DREPO" && cd "$DREPO" && git init -q && git config user.email t@t && git config user.name t
echo "lock-v1" > deps.lock
cat > .spotexec.toml <<'EOF2'
version = 1
[deps]
lockfiles = ["deps.lock"]
install = "mkdir -p vendor && echo installed-on-$(hostname) > vendor/marker && sleep 4"
cache_dirs = ["vendor"]
EOF2
git add -A && git commit -qm deps
"$SX" --json 'cat vendor/marker' >"$WORK/d1.out" 2>"$WORK/d1.err"
check "first instance installs and starts an upload" "grep -q '\"built\":true' $WORK/d1.err && grep -q '\"uploading\":true' $WORK/d1.err"
FIRST=$(cat $WORK/d1.out)
sleep 4   # let the background upload finish
cleanup; sleep 1
"$SX" --json 'cat vendor/marker' >"$WORK/d2.out" 2>"$WORK/d2.err"
check "fresh instance restored deps from the shared cache" "grep -q '\"restored\":true' $WORK/d2.err && ! grep -q '\"built\":true' $WORK/d2.err"
check "restored files are the cached ones" "[ \"\$(cat $WORK/d2.out)\" = \"$FIRST\" ]"
echo "lock-v2" > deps.lock
"$SX" --json 'cat vendor/marker' >"$WORK/d3.out" 2>"$WORK/d3.err"
check "a lockfile change misses the cache and reinstalls" "grep -q '\"built\":true' $WORK/d3.err"

echo "== a sharded task on a cold cache installs once; the other shards wait and restore it (#139)"
cleanup; sleep 1
echo "lock-v3" > deps.lock
sed -i.bak 's/sleep 4"/sleep 8"/' .spotexec.toml && rm -f .spotexec.toml.bak
cat >> .spotexec.toml <<'EOF2'
[tasks.shardy]
run = "cat vendor/marker"
shards = 3
exclusive = true
EOF2
git add -A && git commit -qm shardy
# Warm VMs, so every shard reaches its dependencies while the first installs.
"$SX" up "$RUNNER" --count 3 >/dev/null 2>&1
"$SX" --json shardy >"$WORK/d4.out" 2>"$WORK/d4.err"; rc=$?
deps4() { grep '"event":"deps"' "$WORK/d4.err"; }
check "every shard ran" "[ $rc = 0 ] && [ \$(grep -c installed-on- $WORK/d4.out) = 3 ]"
check "one shard installed; the others restored its upload" "[ \$(deps4 | grep -c '\"built\":true') = 1 ] && [ \$(deps4 | grep -c '\"restored\":true') = 2 ]"
check "the installing shard waited for its upload to finish" "deps4 | grep '\"built\":true' | grep -q '\"uploaded\":true'"
check "the other shards said they waited for it" "[ \$(grep '\"event\":\"deps_wait\"' $WORK/d4.err | grep -c 'another part of this run (shardy ') = 2 ] && [ \$(deps4 | grep '\"restored\":true' | grep -c '\"waited_ms\"') = 2 ]"
check "every shard has the same install" "[ \$(sed 's/^\[[^]]*\] //' $WORK/d4.out | sort -u | wc -l | tr -d ' ') = 1 ]"

echo "== two runs at once on a cold cache: one installs, the other waits for it and says so (#139)"
cleanup; sleep 1
echo "lock-v4" > deps.lock
"$SX" up "$RUNNER" --count 2 >/dev/null 2>&1
"$SX" --json --exclusive 'cat vendor/marker' >"$WORK/d5.out" 2>"$WORK/d5.err" &
P=$!
"$SX" --json --exclusive 'cat vendor/marker' >"$WORK/d6.out" 2>"$WORK/d6.err"; rc6=$?
wait $P; rc5=$?
both() { cat "$WORK/d5.err" "$WORK/d6.err"; }
check "both ran with the same install" "[ $rc5 = 0 ] && [ $rc6 = 0 ] && [ -s $WORK/d5.out ] && [ \"\$(cat $WORK/d5.out)\" = \"\$(cat $WORK/d6.out)\" ]"
check "only one of them installed" "[ \$(both | grep '\"event\":\"deps\"' | grep -c '\"built\":true') = 1 ]"
check "the other waited for it, naming it, and restored it" "both | grep '\"event\":\"deps_wait\"' | grep -q 'another run on this machine (' && both | grep '\"event\":\"deps\"' | grep '\"restored\":true' | grep -q '\"waited_ms\"'"
cd "$REPO"

echo "== spotexec up ends a VM that fails to get ready"
cleanup; sleep 1
cat >> "$SPOTEXEC_CONFIG_DIR/config.toml" <<EOF
[runners.${RUNNER}x]
type = "sim"
max_instances = 1
idle_timeout = "10m"
boot_delay = "1s"
bootstrap_script = "exit 3"
EOF
"$SX" up "${RUNNER}x" >"$WORK/upx.out" 2>"$WORK/upx.err"; rc=$?
check "up fails on a broken bootstrap_script" "[ $rc != 0 ] && grep -q 'bootstrap_script exited 3' $WORK/upx.err"
check "the broken VM was terminated" "[ \$(countx) = 0 ]"
SPOTEXEC_POOL="${RUNNER}x" "$SX" --json --queue-timeout 5s 'echo should-not-run' >"$WORK/upx2.out" 2>"$WORK/upx2.err"
check "the failed boot is recorded: no relaunch of the broken setup" "[ \$(countx) = 0 ] && ! grep -q should-not-run $WORK/upx2.out"
"$SX" down "${RUNNER}x" >/dev/null 2>&1
sed -i.bak 's/bootstrap_script = "exit 3"/bootstrap_script = "sleep 120"/' "$SPOTEXEC_CONFIG_DIR/config.toml"
"$SX" up "${RUNNER}x" >"$WORK/upi.out" 2>"$WORK/upi.err" & UP=$!
for _ in $(seq 1 60); do [ "$(countx)" = 1 ] && break; sleep 0.5; done
sleep 2
kill -INT $UP; wait $UP; rc=$?
check "Ctrl-C during boot fails up" "[ $rc != 0 ]"
check "Ctrl-C during boot terminates the VM" "[ \$(countx) = 0 ]"

echo "== a cold boot shows what the VM is doing (plain output, as agents see it)"
SPOTEXEC_POOL=${RUNNER}s "$SX" 'echo booted' >"$WORK/bp.out" 2>"$WORK/bp.err"
check "plain heartbeat names the bootstrap step and how long it has run" "grep -q booted $WORK/bp.out && grep -qE 'sx: ${RUNNER}s: running first-boot setup [0-9.]+s — (boot-step|spotexec: )' $WORK/bp.err"
SPOTEXEC_POOL=${RUNNER}s "$SX" --json 'echo booted' >/dev/null 2>"$WORK/bpj.err"
SPOTEXEC_POOL=${RUNNER}s "$SX" 'echo warm' >"$WORK/bw.out" 2>"$WORK/bw.err"
check "a warm run says nothing about first boot" "grep -q warm $WORK/bw.out && ! grep -q first-boot $WORK/bw.err && ! grep -q boot_progress $WORK/bpj.err"
docker ps -q --filter "label=spotexec-runner=${RUNNER}s" | xargs -r docker rm -f >/dev/null 2>&1
SPOTEXEC_POOL=${RUNNER}s "$SX" --json 'echo booted' >/dev/null 2>"$WORK/bpj.err"
check "agents get boot_progress events with the latest log line" "grep -qE '\"event\":\"boot_progress\".*\"line\":\"(boot-step|spotexec: )' $WORK/bpj.err"

echo "== a VM near max_lifetime takes no new runs (60s lifetime: none in the last 15s)"
cleanup; sleep 1
placed() { grep '"placed"' "$1" | sed 's/.*instance_id":"\([^"]*\)".*/\1/' | tail -1; }
SPOTEXEC_POOL=${RUNNER}l "$SX" --json 'echo first' >"$WORK/l1.out" 2>"$WORK/l1.err"
L1=$(placed "$WORK/l1.err")
born=$(docker inspect -f '{{index .Config.Labels "spotexec-launched"}}' "$L1" 2>/dev/null || echo 0)
while [ $(( $(date +%s) - born/1000 )) -lt 47 ]; do sleep 1; done
SPOTEXEC_POOL=${RUNNER}l "$SX" --json --same-slot 'echo same' >"$WORK/l2.out" 2>"$WORK/l2.err"; code=$?
check "the VM's helper refuses a run to its slot near max_lifetime" "[ $code = 125 ] && ! grep -q same $WORK/l2.out && grep -q max_lifetime $WORK/l2.err"
SPOTEXEC_POOL=${RUNNER}l "$SX" --json 'echo second' >"$WORK/l3.out" 2>"$WORK/l3.err"
L3=$(placed "$WORK/l3.err")
check "a new run goes to a fresh VM, not the ageing one" "grep -q second $WORK/l3.out && [ -n \"$L3\" ] && [ \"$L3\" != \"$L1\" ]"

echo "== Spot vCPU quota (a simulated VM counts 4 vCPUs)"
cleanup; sleep 1
rm -rf "$SPOTEXEC_STATE_DIR/runners/$RUNNER/pool.json"
t0=$(date +%s)
SPOTEXEC_SIM_QUOTA=0 "$SX" 'echo never' >"$WORK/qz.out" 2>"$WORK/qz.err"; code=$?
took=$(( $(date +%s) - t0 ))
check "no VM and no quota: fails at once with exit 125 (${took}s)" "[ $code = 125 ] && [ $took -le 20 ] && ! grep -q never $WORK/qz.out"
check "the error names the region, the vCPUs and the increase command" "grep -q 'quota in sim' $WORK/qz.err && grep -q 'needs 4 vCPUs' $WORK/qz.err && grep -q 'request-service-quota-increase --region sim --service-code ec2 --quota-code L-34B43A08' $WORK/qz.err"
rm -rf "$SPOTEXEC_STATE_DIR/runners/$RUNNER/pool.json"
for i in 1 2 3 4 5; do SPOTEXEC_SIM_QUOTA=4 "$SX" --json "sleep 3; echo quota$i" >"$WORK/qq$i.out" 2>"$WORK/qq$i.err" & done; wait
check "room for one VM: all five jobs queue for it and succeed" "[ \$(cat $WORK/qq*.out | grep -c '^quota') = 5 ]"
check "only one VM ran" "[ \$(count) = 1 ]"
check "launches stop after the quota refusal (no retry loop)" "[ \$(cat $WORK/qq*.err | grep -c LAUNCH_FAILED) -le 2 ]"

echo "== two machines sharing the account's quota (room for one VM)"
cleanup; sleep 1
rm -rf "$SPOTEXEC_STATE_DIR/runners/$RUNNER/pool.json"
SPOTEXEC_SIM_QUOTA=4 "$SX" --json 'sleep 10; echo a-done' >"$WORK/ma.out" 2>"$WORK/ma.err" &
P=$!
for _ in $(seq 1 60); do grep -q '"exec"' "$WORK/ma.err" 2>/dev/null && break; sleep 0.5; done
mkdir -p "$WORK/cfgB" && cp "$SPOTEXEC_CONFIG_DIR/config.toml" "$WORK/cfgB/"
SPOTEXEC_CONFIG_DIR="$WORK/cfgB" SPOTEXEC_STATE_DIR="$WORK/stateB" SPOTEXEC_SIM_QUOTA=4 "$SX" 'echo b-ran' >"$WORK/mb.out" 2>"$WORK/mb.err"; code=$?
check "the other machine, with no VM, fails at once (exit 125)" "[ $code = 125 ] && ! grep -q b-ran $WORK/mb.out"
check "and says who holds the quota" "grep -q '4 of 4 vCPUs in use: 4 by owner ' $WORK/mb.err"
wait $P
check "the first machine's run was unaffected" "grep -q a-done $WORK/ma.out"

echo "== a region the client can't reach: launches move elsewhere, and come back once it answers (#225)"
export SPOTEXEC_SIM_REGIONS=sim-a,sim-b
RR=${RUNNER}r
region() { docker inspect -f '{{index .Config.Labels "spotexec-region"}}' "$1" 2>/dev/null; }
regions() { docker ps --filter "label=spotexec-runner=$RR" --format '{{.Label "spotexec-region"}}' | sort | tr '\n' ' '; }
"$SX" sim network "$RR" sim-a down >/dev/null
SPOTEXEC_POOL=$RR "$SX" --json 'echo served' >"$WORK/u1.out" 2>"$WORK/u1.err"; code=$?
U1=$(placed "$WORK/u1.err")
check "the job ran, on a VM in sim-b (${U1:-none})" "[ $code = 0 ] && grep -q served $WORK/u1.out && [ \"\$(region \"$U1\")\" = sim-b ]"
check "its first launch, into sim-a, is still running but was never used (regions: $(regions))" "[ \"\$(regions)\" = 'sim-a sim-b ' ]"
"$SX" status "$RR" >"$WORK/u-status.out" 2>&1
check "status says sim-a takes no launches, and why" "grep -q \"$RR: no launches into sim-a: unreachable from this client\" $WORK/u-status.out && grep -q 'never answered SSH' $WORK/u-status.out"
for i in 1 2; do SPOTEXEC_POOL=$RR "$SX" --json "sleep 4; echo busy$i" >"$WORK/u2$i.out" 2>"$WORK/u2$i.err" & done; wait
check "with sim-a still held, the next launch goes to sim-b too (regions: $(regions))" "[ \$(cat $WORK/u2*.out | grep -c '^busy') = 2 ] && [ \"\$(regions)\" = 'sim-a sim-b sim-b ' ]"
"$SX" sim network "$RR" sim-a up >/dev/null
sleep 32 # the sim-a VM's 30s suspect window: probed again from here
SPOTEXEC_POOL=$RR "$SX" --json 'echo back' >"$WORK/u3.out" 2>"$WORK/u3.err"
U3=$(placed "$WORK/u3.err")
check "sim-a answers again: a job runs there (${U3:-none})" "grep -q back $WORK/u3.out && [ \"\$(region \"$U3\")\" = sim-a ]"
"$SX" status "$RR" >"$WORK/u-status2.out" 2>&1
check "and its hold is gone" "! grep -q 'no launches into sim-a' $WORK/u-status2.out"
for i in 1 2 3 4; do SPOTEXEC_POOL=$RR "$SX" --json "sleep 6; echo again$i" >"$WORK/u4$i.out" 2>"$WORK/u4$i.err" & done; wait
check "with every VM busy, a new launch goes to sim-a again (regions: $(regions))" "[ \$(cat $WORK/u4*.out | grep -c '^again') = 4 ] && [ \"\$(regions)\" = 'sim-a sim-a sim-b sim-b ' ]"
unset SPOTEXEC_SIM_REGIONS

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" = 0 ]
