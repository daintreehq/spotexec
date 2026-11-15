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
IMAGE=spotexec-e2e:$(git hash-object "$ROOT/scripts/e2e/Dockerfile" | cut -c1-10)
docker build -q -t "$IMAGE" "$WORK/image" >/dev/null
docker run -d --name "$NAME" -p "$PORT:22" "$IMAGE" >/dev/null
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
for u in fishy cshy; do cat >> "$SPOTEXEC_CONFIG_DIR/config.toml" <<EOF
[runners.$u]
type = "ssh"
host = "$u@127.0.0.1"
identity_file = "$WORK/id_ed25519"
ssh_options = ["Port=$PORT", "StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null"]
EOF
done

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

echo "== non-POSIX login shells (fish, tcsh)"
for u in fishy cshy; do
  sh=$(docker exec "$NAME" getent passwd $u | cut -d: -f7)
  "$SX" -q -r $u 'echo "ok as $(id -un)"' >"$WORK/sh.out" 2>"$WORK/sh.err" || true
  check "a runner whose login shell is $sh installs the helper and runs" "grep -qx 'ok as $u' $WORK/sh.out"
done

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

echo "== created files that aren't written back"
run --writeback never 'echo x > extra.txt; mkdir -p dist; echo k > dist/keep.js' || true
check "--writeback never leaves the created file remote only" "test ! -e extra.txt"
"$SX" exec 'cat extra.txt' >"$WORK/x1.out" 2>&1 || true
check "exec (no sync) still sees it" "grep -qx x $WORK/x1.out"
"$SX" -s --sync none 'cat extra.txt' >"$WORK/x2.out" 2>&1 || true
check "--same-slot --sync none still sees it" "grep -qx x $WORK/x2.out"
run 'test -e extra.txt && echo leaked || echo clean; cat dist/keep.js' || true
check "the next synced run removes it, without the fingerprint shortcut" "grep -qx clean $WORK/last.out && ! ev fingerprint_hit | grep -q ."
check "ignored build outputs survive the cleanup" "grep -qx k $WORK/last.out"
run --writeback on-success 'echo y > extra2.txt; exit 3' || true
check "a failing on-success run writes nothing back" "test ! -e extra2.txt"
run 'test -e extra2.txt && echo leaked || echo clean' || true
check "the next synced run removes its created file" "grep -qx clean $WORK/last.out"
"$SX" -q -s --sync none 'echo n > nosync.txt' || true
"$SX" -q --sync in 'echo i > syncin.txt' || true
check "--sync in leaves the created file remote only" "test ! -e syncin.txt"
run 'for f in nosync.txt syncin.txt; do test -e $f && echo leaked-$f; done; echo checked' || true
check "files created by --sync none and --sync in runs are removed by the next synced run" "grep -qx checked $WORK/last.out && ! grep -q leaked $WORK/last.out"
"$SX" -q 'echo d > died.txt; echo created; sleep 30' >"$WORK/died.out" 2>/dev/null &
P=$!
for _ in $(seq 1 50); do grep -q created "$WORK/died.out" 2>/dev/null && break; sleep 0.2; done
docker exec "$NAME" sh -c 'find /home/dev/.cache/spotexec -name died.txt | grep -q .' && remote=1 || remote=0
docker exec "$NAME" pkill -KILL -f 'spotexec-helper.* rpc' && killed=1 || killed=0
wait $P || true
run 'test -e died.txt && echo leaked || echo clean' || true
check "a file created by a run whose helper died is removed by the next synced run" "[ $remote = 1 ] && [ $killed = 1 ] && grep -qx clean $WORK/last.out"
run 'mkdir -p only/.git; echo y > only/.git/config' || true
check "write-back names a .git directory a run made below the root" "ev 'WRITEBACK_REFUSED.*only/\.git' | grep -q ."
run 'mkdir -p sub/.git deep/x/.git dist/clone/.git; echo y > sub/.git/config; echo r > deep/x/.git/HEAD; echo n > deep/x/file; echo c > dist/clone/.git/HEAD; test -e only/.git && echo leaked-only' || true
check "the next synced run removes it" "! grep -q leaked-only $WORK/last.out"
check "nested .git directories never come back, their neighbours do" "test ! -e sub/.git && test ! -e deep/x/.git && test -f deep/x/file"
check "write-back names each one, but not one in an ignored directory" "ev 'WRITEBACK_REFUSED.*deep/x/\.git, sub/\.git' | grep -q . && ! ev 'WRITEBACK_REFUSED.*dist/clone' | grep -q ."
run 'for d in sub/.git deep/x/.git; do test -e $d && echo leaked-$d; done; test -e dist/clone/.git && echo kept; echo checked' || true
check "the next synced run removes them, and keeps the ignored one" "grep -qx checked $WORK/last.out && ! grep -q leaked $WORK/last.out && grep -qx kept $WORK/last.out"
rm -rf deep
run 'mkdir -p dist; echo o > dist/only.js' || true
run true
check "a run that only creates ignored outputs keeps the fingerprint shortcut" "ev fingerprint_hit | grep -q ."
# Another worktree of the same repository landing in that slot.
git worktree add -q "$WORK/wt2" 2>/dev/null
git worktree add -q "$WORK/wt3" 2>/dev/null
run --writeback never 'echo z > leak.txt' || true
S0=$(grep slot_acquired "$WORK/last.err" | grep -o '"workspace_slot":[0-9]*' | tail -1)
(cd "$WORK/wt3" && "$SX" -q 'sleep 6' & cd "$WORK/wt3" && "$SX" -q 'sleep 6' & wait) &
WP=$!
sleep 2
(cd "$WORK/wt2" && "$SX" --json 'test -e leak.txt && echo leaked || echo clean') >"$WORK/wt2.out" 2>"$WORK/wt2.err" || true
wait $WP
check "another worktree reusing the slot doesn't see the file" "[ -n '$S0' ] && grep slot_acquired $WORK/wt2.err | grep -q '$S0' && grep -qx clean $WORK/wt2.out"
git worktree remove --force "$WORK/wt2"; git worktree remove --force "$WORK/wt3"

echo "== exit codes, timeout, cancel"
"$SX" --json 'exit 7' >/dev/null 2>&1 && code=0 || code=$?
check "exit code passes through" "[ $code = 7 ]"
"$SX" --json --timeout 1s 'sleep 30' >/dev/null 2>"$WORK/t.err" && code=0 || code=$?
check "timeout exits 124" "[ $code = 124 ]"
"$SX" --json 'trap "echo got-term; exit 143" TERM; sleep 30 & wait' >"$WORK/c.out" 2>&1 &
P=$!; sleep 1.5; kill -INT $P; wait $P && code=0 || code=$?
check "ctrl-c terminates the remote process group and exits 130" "grep -q got-term $WORK/c.out && [ $code = 130 ]"
"$SX" --json --writeback on-success 'trap "echo handled > handled.txt; exit 0" TERM; sleep 30 & wait' >/dev/null 2>"$WORK/c2.err" &
P=$!; sleep 1.5; kill -TERM $P; wait $P && code=0 || code=$?
check "a command that handles SIGTERM and exits 0 is still interrupted (130, no on-success write-back)" "[ $code = 130 ] && test ! -e handled.txt && grep -q '\"canceled\":true' $WORK/c2.err"
docker exec "$NAME" bash -c 'mkdir -p /home/dev/.cache/spotexec/tmp && chmod 000 /home/dev/.cache/spotexec/tmp'
"$SX" --json 'echo ran > never.txt' >/dev/null 2>"$WORK/se.err" && code=0 || code=$?
docker exec "$NAME" chmod 700 /home/dev/.cache/spotexec/tmp
check "a command the runner can't start exits 125" "[ $code = 125 ] && test ! -e never.txt && grep -q 'could not be started' $WORK/se.err"

echo "== stale SSH master (laptop sleep)"
# Freeze the server end of the persisted master: the master process still
# answers -O check, but nothing gets through it, as after a laptop sleep.
# The second time the helper isn't known to be installed (a new CLI version),
# so the install's first command is what meets the dead master.
for case in session install; do
  "$SX" -q true
  [ "$case" = install ] && rm -rf "$HOME/.cache/spotexec/helpers"
  WEDGED=$(docker exec "$NAME" pgrep -f 'sshd: dev' | tr '\n' ' ')
  docker exec "$NAME" kill -STOP $WEDGED
  t0=$(date +%s)
  "$SX" --json 'echo alive' >"$WORK/w.out" 2>"$WORK/w.err" && code=0 || code=$?
  t1=$(date +%s)
  docker exec "$NAME" kill -CONT $WEDGED; docker exec "$NAME" kill $WEDGED 2>/dev/null || true
  check "a dead reused master is replaced ($case): the run reconnects and succeeds" "[ -n '$WEDGED' ] && [ $code = 0 ] && grep -qx alive $WORK/w.out && [ $((t1-t0)) -lt 20 ]"
done

echo "== conflicts"
("$SX" --json 'sleep 2; echo remote > src/a.txt' >/dev/null 2>"$WORK/conf.err" && c=0 || c=$?; echo $c >"$WORK/conf.code") &
sleep 1; echo "local edit" > src/a.txt; wait
check "a conflict alone keeps the command's exit 0" "[ \$(cat $WORK/conf.code) = 0 ]"
check "the result event counts the conflict" "grep '\"event\":\"result\"' $WORK/conf.err | grep -q '\"conflict_count\":1'"
check "concurrent local edit is kept" "grep -qx 'local edit' src/a.txt"
check "remote version stashed" "ls .git/spotexec/conflicts/*/src/a.txt >/dev/null 2>&1"
check "SYNC_CONFLICT reported" "grep -q SYNC_CONFLICT $WORK/conf.err"
D=$(grep '"event":"conflict"' "$WORK/conf.err" | sed 's/.*"diff":"\([^"]*\)".*/\1/' | head -1)
check "the conflict event's diff command shows the remote change" "[ -n \"\$D\" ] && { eval \"\$D\" || true; } | grep -qx '+remote'"
("$SX" --json --fail-on-conflict 'sleep 2; echo remote2 > src/a.txt' >/dev/null 2>"$WORK/conf2.err" && c=0 || c=$?; echo $c >"$WORK/conf2.code") &
sleep 1; echo "local edit 2" > src/a.txt; wait
check "--fail-on-conflict: a successful run whose write-back conflicted exits 122, and its result says so" "[ \$(cat $WORK/conf2.code) = 122 ] && grep -qx 'local edit 2' src/a.txt && grep '\"event\":\"result\"' $WORK/conf2.err | grep '\"conflict_count\":1' | grep -q '\"exit_code\":122'"
(SPOTEXEC_FAIL_ON_CONFLICT=1 "$SX" -q 'sleep 2; echo remote3 > src/a.txt' >/dev/null 2>&1 && c=0 || c=$?; echo $c >"$WORK/conf3.code") &
sleep 1; echo "local edit 3" > src/a.txt; wait
check "SPOTEXEC_FAIL_ON_CONFLICT=1 turns it on" "[ \$(cat $WORK/conf3.code) = 122 ] && grep -qx 'local edit 3' src/a.txt"
(SPOTEXEC_FAIL_ON_CONFLICT=1 "$SX" --json --writeback always 'sleep 2; echo remote4 > src/a.txt; exit 3' >/dev/null 2>"$WORK/conf4.err" && c=0 || c=$?; echo $c >"$WORK/conf4.code") &
sleep 1; echo "local edit 4" > src/a.txt; wait
check "a failed command that conflicted keeps its own exit code" "[ \$(cat $WORK/conf4.code) = 3 ] && grep -qx 'local edit 4' src/a.txt && grep '\"event\":\"result\"' $WORK/conf4.err | grep -q '\"conflict_count\":1'"

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
DEPS_LOG=$(ev '"event":"deps",' | sed -n 's/.*"log":"\([^"]*\)".*/\1/p' | head -1)
check "install output kept out of the command's streams, in a local log" "[ -n \"\$DEPS_LOG\" ] && grep -Eq 'added [0-9]+ package' \"\$DEPS_LOG\" && ! grep -Eq 'added [0-9]+ package' $WORK/last.out $WORK/last.err"
("$SX" -q 'sleep 3' &) ; sleep 0.7
run test || true
check "other slot hydrated from cache" "ev '\"hydrated\":true' | grep -q ."
wait
"$SX" verify >"$WORK/v.out" 2>&1 || true
check "parallel task fan-out" "grep -q '\\[test\\] ok=true' $WORK/v.out && grep -q '\\[lint\\]' $WORK/v.out"
check "parallel task results are plain off a terminal" "grep -Eq '^sx: (test|lint) · .* on box#[0-9]' $WORK/v.out && ! grep -q \$'\\x1b' $WORK/v.out"

sed -i.bak 's/^install = "npm ci"/install = "npm ci --no-audit"/' .spotexec.toml && rm -f .spotexec.toml.bak
"$SX" --json verify >"$WORK/v2.out" 2>"$WORK/v2.err" || true
check "two slots on a cold cache: one builds, the other hydrates" "grep -q '\"built\":true' $WORK/v2.err && grep -q '\"hydrated\":true' $WORK/v2.err && [ \$(grep -c '\"exit_code\":0' $WORK/v2.err) -ge 3 ]"

echo "== python venv cache, relocated into each slot"
PYREPO="$WORK/pyrepo"
mkdir -p "$PYREPO/src/proj" && cd "$PYREPO"
git init -q && git config user.email t@t && git config user.name t
echo 'WHERE = __file__' > src/proj/__init__.py
echo v1 > requirements.lock
cat > .spotexec.toml <<'EOF'
version = 1
[deps]
lockfiles = ["requirements.lock"]
install = '''python3 -m venv --without-pip .venv && echo "$PWD/src" > "$(echo .venv/lib/python3*/site-packages)/proj.pth" && printf '#!%s/.venv/bin/python\nimport proj; print(proj.WHERE)\n' "$PWD" > .venv/bin/where && chmod +x .venv/bin/where'''
cache_dirs = [".venv"]
EOF
git add -A && git commit -qm init
VENV_CHECK='test "$(.venv/bin/where)" = "$SPOTEXEC_WORKSPACE/src/proj/__init__.py" && echo venv-here'
run "$VENV_CHECK" || true
check "venv built in the first slot" "ev '\"built\":true' | grep -q . && grep -q venv-here $WORK/last.out"
VENV_SLOT=$(grep -o '"workspace_slot":[0-9]*' "$WORK/last.err" | head -1 | cut -d: -f2)
("$SX" -q 'sleep 3' &) ; sleep 0.7
run "$VENV_CHECK" || true
check "a .venv hydrated into another slot runs that slot's interpreter and source" "ev '\"hydrated\":true' | grep -q . && grep -q venv-here $WORK/last.out"
wait
# Prune the slot that built it: the hydrated copy must not need it.
docker exec "$NAME" sh -c "rm -rf /home/dev/.cache/spotexec/repos/pyrepo-*/slots/slot-0$VENV_SLOT"
"$SX" --json --same-slot "$VENV_CHECK" >"$WORK/last.out" 2>"$WORK/last.err" || true
check "...and keeps working once the building slot is gone" "[ -n '$VENV_SLOT' ] && grep -q venv-here $WORK/last.out && ! ev '\"built\":true' | grep -q ."
cd "$REPO"

echo "== git_seed"
GREPO="$WORK/gitrepo"
mkdir -p "$GREPO/src" && cd "$GREPO"
git init -q -b feature/seeded && git config user.email t@t && git config user.name t
printf 'a\n' > src/a.txt; printf 'b\n' > src/b.txt; printf 'gone\n' > gone.txt; printf 'TOKEN=1\n' > secret.env
printf 'version = 1\n[sync]\ngit_seed = true\nexclude = ["*.env"]\n' > .spotexec.toml
git add -A && git commit -qm "the seeded commit"
printf 'b2\n' > src/b.txt; rm gone.txt; printf 'n\n' > new.txt
GIT_PROBE='git rev-parse HEAD --abbrev-ref HEAD; git log -1 --format=%s; git status --porcelain; git diff --stat | tail -1'
{ git rev-parse HEAD --abbrev-ref HEAD; git log -1 --format=%s; git status --porcelain; git diff --stat | tail -1; } > "$WORK/git.local"
run "$GIT_PROBE" || true
check "git_seed: HEAD, branch, log, status and diff match the local worktree" "diff -u $WORK/git.local $WORK/last.out"
run 'git tag runner-tag; printf "[alias]\n x = status\n" >> .git/config; echo from-runner > src/a.txt' || true
check "git_seed: a command's changes to .git never come back" "! git rev-parse -q --verify runner-tag >/dev/null && ! grep -q alias .git/config && grep -qx from-runner src/a.txt"
git checkout -q src/a.txt
run "$GIT_PROBE" || true
check "git_seed: the next run restores the seed" "diff -u $WORK/git.local $WORK/last.out && ev '\"git_seed\".*\"reseeded\":true' | grep -q ."
run "$GIT_PROBE" || true
check "git_seed: an unchanged seed costs nothing" "diff -u $WORK/git.local $WORK/last.out && ev '\"git_seed\".*\"reseeded\":false' | grep -q ."
cd "$REPO"

("$SX" -q 'sleep 3' &) ; sleep 1
"$SX" --json --exclusive 'echo excl' >"$WORK/x.out" 2>"$WORK/x.err" || true
check "--exclusive waits for running jobs" "grep -q excl $WORK/x.out && grep -q '\"event\":\"queued\"' $WORK/x.err"

"$SX" 'echo plain > plain.txt' >"$WORK/p.out" 2>"$WORK/p.err" || true
check "agents get plain, sparse stderr (one summary line, no NDJSON)" "grep -Eq '^sx: .* on box#[0-9]' $WORK/p.err && ! grep -q '\"event\"' $WORK/p.err && [ \$(wc -l < $WORK/p.err) -le 2 ] && grep -qx plain plain.txt"
rm -f plain.txt

echo "== pull, exec, status, unlock"
"$SX" -q 'mkdir -p coverage && echo report > coverage/index.html'
"$SX" pull 'coverage/*' >/dev/null 2>&1 || true
check "pull fetches gitignored artifacts" "grep -qx report coverage/index.html"
"$SX" exec --json 'ls node_modules' >"$WORK/e.out" 2>&1 || true
check "exec reuses the last slot without syncing" "grep -q is-number $WORK/e.out && ! grep -q sync_in $WORK/e.out"
"$SX" --json 'true' >/dev/null 2>"$WORK/ss0.err" || true
S1=$(grep slot_acquired "$WORK/ss0.err" | grep -o '"workspace_slot":[0-9]*' | tail -1)
("$SX" -q --same-slot 'sleep 4' &) ; sleep 1.5
printf 'local edit\n' > src/same.txt
"$SX" --json --same-slot 'cat src/same.txt; echo pinned > pinned.txt' >"$WORK/ss.out" 2>"$WORK/ss.err" &
SSP=$!; sleep 1
"$SX" status >"$WORK/ssw.out" 2>&1 || true
wait $SSP || true
check "status shows a --same-slot run waiting for its slot, not holding one" "grep -q 'waiting for workspace slot' $WORK/ssw.out && grep -Eq '^box .* 1/3' $WORK/ssw.out"
check "--same-slot waits for its own slot, syncs and writes back" "[ -n '$S1' ] && grep slot_acquired $WORK/ss.err | grep -q '$S1' && grep -Eq '\"waited_ms\":[1-9][0-9]{3}' $WORK/ss.err && grep -q 'local edit' $WORK/ss.out && grep -qx pinned pinned.txt"
"$SX" --json --same-slot --sync none 'cat pinned.txt' >"$WORK/ss2.out" 2>&1 || true
check "--same-slot --sync none is exec" "grep -qx pinned $WORK/ss2.out && ! grep -q sync_in $WORK/ss2.out"
rm -f pinned.txt src/same.txt
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
"$SX" echoarg >"$WORK/unk.out" 2>&1 || true
check "a mistyped task name names the closest task" "grep -qF 'no task is named \"echoarg\", and the shell found no command by that name on the runner; did you mean \"echoargs\"?' $WORK/unk.out && grep -q 'exit 127' $WORK/unk.out"
cat >> .spotexec.toml <<'EOF'

[tasks.sharded]
run = 'echo "shard=$SPOTEXEC_SHARD/$SPOTEXEC_SHARDS arg={shard}/{shards}" {args}'
shards = 3
EOF
"$SX" sharded 'x y' >"$WORK/sh.out" 2>&1 || true
check "sharded task fans out with its number and the args" "grep -qx '\\[sharded 1/3\\] shard=1/3 arg=1/3 x y' $WORK/sh.out && grep -qx '\\[sharded 2/3\\] shard=2/3 arg=2/3 x y' $WORK/sh.out && grep -qx '\\[sharded 3/3\\] shard=3/3 arg=3/3 x y' $WORK/sh.out"
check "sharded task results name each shard" "(for n in 1 2 3; do [ \$(grep -Ec \"^sx: sharded \$n/3 · [0-9][^·]* on box#[0-9]\" $WORK/sh.out) = 1 ] || exit 1; done)"
check "sharded task summary lists each shard" "(for n in 1 2 3; do [ \$(grep -Ec \"^sx: sharded \$n/3 · passed · \" $WORK/sh.out) = 1 ] || exit 1; done)"
git add -A >/dev/null && git commit -qm wip
"$SX" -q 'mv src/gen.txt src/Gen.txt' || true
check "case-only rename keeps the file" "ls src | grep -qx Gen.txt"
"$SX" -q --sync in 'echo remote-copy > src/a.txt' || true
"$SX" --pull 'src/*' --sync in true >/dev/null 2>"$WORK/pull.err" || true
check "pull never overwrites tracked source files" "! grep -q remote-copy src/a.txt && grep -q 'pull skipped' $WORK/pull.err"
"$SX" -q 'setsid sh -c "sleep 60" </dev/null >/dev/null 2>&1 & sleep 0.3; echo started' || true
check "processes escaping the group are reaped" "! docker exec $NAME pgrep -u dev -f 'sleep 60' >/dev/null"

echo "== a killed helper's leftovers"
# A background writer (a forked subshell) that left the workspace
# directory, then the helper killed outright (OOM, SIGKILL): only its direct
# child dies with it, and nothing left in the group is in the workspace.
"$SX" -q --no-retry 'cd /tmp; (while :; do date >> "$SPOTEXEC_WORKSPACE/bg.txt"; sleep 0.2; done) </dev/null >/dev/null 2>&1 & sleep 30' >/dev/null 2>&1 &
KP=$!; sleep 3
docker exec "$NAME" pkill -KILL -u dev -f 'spotexec-helper-[0-9a-f]* rpc' || true
wait $KP || true
# (bg[.]txt: a pattern that doesn't match the shell running the check)
check "the background writer outlives its killed helper" "docker exec $NAME pgrep -u dev -f 'bg[.]txt' >/dev/null"
"$SX" -q --sync none 'pgrep -u dev -f "bg[.]txt" >/dev/null && echo writer-alive || echo slot-clean' >"$WORK/kr.out" 2>&1 || true
check "the slot's next holder stops it before running" "grep -qx slot-clean $WORK/kr.out && ! docker exec $NAME pgrep -u dev -f 'bg[.]txt' >/dev/null"

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
# An image drop-in that sorts first by default and allows passwords and root.
docker exec "$NAME" bash -c 'printf "PasswordAuthentication yes\nPermitRootLogin yes\nKbdInteractiveAuthentication yes\n" > /etc/ssh/sshd_config.d/50-cloud-init.conf'
docker exec "$NAME" bash -c 'mkdir -p /tmp/fakebin && for b in systemctl systemd-run; do printf "#!/bin/sh\nexit 0\n" > /tmp/fakebin/$b; chmod +x /tmp/fakebin/$b; done; PATH=/tmp/fakebin:$PATH bash /tmp/userdata.sh >/dev/null 2>&1; cat /var/tmp/spotexec/ready'
sshd_eff() { docker exec "$NAME" /usr/sbin/sshd -T -C user=dev,host=localhost,addr=127.0.0.1 2>&1; }
key_only() { sshd_eff | grep -cxE 'authenticationmethods publickey|passwordauthentication no|kbdinteractiveauthentication no|permitrootlogin no|allowusers dev' | grep -qx 5; }
check "sshd takes key logins as the runner user only, over the image's drop-in" "key_only && docker exec $NAME test -f /etc/ssh/sshd_config.d/00-spotexec.conf"
# An explicit "AuthenticationMethods any" makes sshd reject ours: the rest still applies.
docker exec "$NAME" bash -c 'echo "AuthenticationMethods any" >> /etc/ssh/sshd_config.d/50-cloud-init.conf; PATH=/tmp/fakebin:$PATH bash /tmp/userdata.sh >/dev/null 2>&1'
check "...and without AuthenticationMethods when sshd rejects it" "docker exec $NAME /usr/sbin/sshd -t && [ \$(sshd_eff | grep -cxE 'passwordauthentication no|kbdinteractiveauthentication no|permitrootlogin no|allowusers dev|hostbasedauthentication no') = 5 ]"
docker exec "$NAME" sed -i '/AuthenticationMethods any/d' /etc/ssh/sshd_config.d/50-cloud-init.conf
# An image whose sshd_config sets passwords before (or without) the Include.
docker exec "$NAME" bash -c 'sed -i "/^Include/d" /etc/ssh/sshd_config; sed -i "1i PasswordAuthentication yes" /etc/ssh/sshd_config; PATH=/tmp/fakebin:$PATH bash /tmp/userdata.sh >/dev/null 2>&1'
check "...and over settings at the top of sshd_config" "key_only && docker exec $NAME head -1 /etc/ssh/sshd_config | grep -qx '# spotexec begin' && docker exec $NAME /usr/sbin/sshd -t"
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
